package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	pierrors "github.com/PycMono/go-harness/pi/error"
	"github.com/PycMono/go-harness/pi/schema"
	"github.com/PycMono/go-harness/pi/tools"
)

/*
	tools.Tool 的实现类，运行 Agent 时需要把 mcp 装配成 tools.Tool 然后注册到 tools.Registry 里面
	最后大模型识别需要调用 mcp 时，直接执行 remoteTool 的 Execute 方法
*/

const (
	// maxRemoteTextBytes 是单个远端结果里文本总量的兜底上限，成功与失败两条路
	// 共用。它不是预算而是兜底：MCP 的 tools/call 没有 offset 这类续读手段，
	// 截掉的部分补不回来，所以阈值要远高于正常结果——碰到了说明这个 server
	// 在成批吐数据，模型看到的截断标记就是提醒。量级取自
	// pi/prompt.go 的 maxAgentsFileBytes（1 MiB）。
	maxRemoteTextBytes = 1024 * 1024
)

// remoteNamePattern 是远端工具名的闸：名字会拼进本地工具名并原样进两家
// Provider 的请求，按它们的共同口径校验。
var remoteNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// remoteTool 是远端工具在本地的代理，实现 pi/tools 的 Tool。
type remoteTool struct {
	definition schema.ToolDefinition // 本地定义：名字带前缀，Label/Description/schema 从远端映射来
	remoteName string                // 远端原名，tools/call 时用它
	server     string                // 只进错误文案
	session    *sdkmcp.ClientSession // 复用扩展那条会话
	timeout    time.Duration         // 来自配置，Execute 里套一层 ctx 超时
}

// newRemoteTool 把远端 Tool 包成本地工具，远端名字闸开在这里。
func newRemoteTool(
	remote *sdkmcp.Tool,
	session *sdkmcp.ClientSession,
	server, prefix string,
	timeout time.Duration,
) (*remoteTool, error) {
	if !remoteNamePattern.MatchString(remote.Name) {
		return nil, pierrors.ErrToolDefinitionInvalid.Wrap(fmt.Errorf(
			"server %q 的工具名 %q 必须匹配 ^[a-zA-Z0-9_-]{1,64}$", server, remote.Name))
	}

	return &remoteTool{
		definition: schema.ToolDefinition{
			Name:         toolNamePrefix(prefix) + "__" + server + "__" + remote.Name,
			Label:        displayName(remote),
			Description:  remote.Description,
			InputSchema:  remote.InputSchema,
			ParallelSafe: false,
		},
		remoteName: remote.Name,
		server:     server,
		session:    session,
		timeout:    timeout,
	}, nil
}

// displayName 按 SDK 写下的优先级取展示名（mcp/protocol.go 的 Tool.Annotations
// 字段注释）：title > annotations.title > name。
func displayName(remote *sdkmcp.Tool) string {
	if remote.Title != "" {
		return remote.Title
	}
	if remote.Annotations != nil && remote.Annotations.Title != "" {
		return remote.Annotations.Title
	}

	return remote.Name
}

func (t *remoteTool) Definition() schema.ToolDefinition { return t.definition }

func (t *remoteTool) Execute(
	ctx context.Context, arguments json.RawMessage, _ *tools.UpdateEmitter,
) (*schema.ToolOutput, error) {
	// 超时套在传进来的 ctx 上：SDK 自己的重试也落在同一个窗口里。
	callCtx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()

	// 空的 RawMessage 不能直接进 CallToolParams：它会在 json.Marshal 那步报错。
	// 参数被省略时按空对象发。
	if len(arguments) == 0 {
		arguments = json.RawMessage("{}")
	}

	result, err := t.session.CallTool(callCtx, &sdkmcp.CallToolParams{
		Name:      t.remoteName,
		Arguments: arguments,
	})
	if err != nil {
		// 超时与取消原样返回，别套 81004：CodeOf 会先看到 MCP 码，调用方就分不出
		// "远端坏了"和"我这轮超时了"。
		if ctxErr := callCtx.Err(); ctxErr != nil {
			return nil, ctxErr
		}

		return nil, pierrors.ErrMCPToolCallFailed.Wrap(fmt.Errorf(
			"server %q 工具 %q: %w", t.server, t.remoteName, err))
	}
	if result.IsError {
		return nil, pierrors.ErrMCPToolCallFailed.Wrap(
			errors.New(errorText(result.Content)))
	}

	// 成功路径的长度兜底交给 pi/tools.LimitText：它截断时保证 UTF-8 完整、追加
	// 同一个标记，并在 Details 上记 truncated。它返回的是值，这里取地址再交出。
	return new(tools.LimitText(schema.ToolOutput{
		Content: contentBlocks(result.Content),
		Details: result.StructuredContent,
	}, maxRemoteTextBytes)), nil
}

// contentBlocks 把远端内容块映成本地内容块，顺序原样保持。
func contentBlocks(content []sdkmcp.Content) schema.ContentBlocks {
	blocks := make(schema.ContentBlocks, 0, len(content))
	for _, item := range content {
		switch typed := item.(type) {
		case *sdkmcp.TextContent:
			blocks = append(blocks, schema.TextBlock(typed.Text))
		case *sdkmcp.ImageContent:
			blocks = append(blocks, schema.ContentBlock{
				Type: schema.ContentTypeImage,
				Image: &schema.ImageContent{
					// SDK 的 Data 在反序列化时已经从 wire 上的 base64 还原成原始
					// 字节，本地协议要的是 base64 字符串，得重新编码。
					Data:     base64.StdEncoding.EncodeToString(typed.Data),
					MIMEType: typed.MIMEType,
				},
			})
		default:
			// audio / resource / resource_link 落到这里：写明类型与标识，不静默丢。
			blocks = append(blocks, schema.TextBlock(placeholderText(item)))
		}
	}

	return blocks
}

// placeholderText 给本地协议装不下的内容块生成一条文本占位。
func placeholderText(item sdkmcp.Content) string {
	switch typed := item.(type) {
	case *sdkmcp.AudioContent:
		return fmt.Sprintf("[音频: %s]", typed.MIMEType)
	case *sdkmcp.ResourceLink:
		return fmt.Sprintf("[资源链接: %s]", typed.URI)
	case *sdkmcp.EmbeddedResource:
		if typed.Resource != nil {
			return fmt.Sprintf("[内嵌资源: %s]", typed.Resource.URI)
		}

		return "[内嵌资源]"
	default:
		return fmt.Sprintf("[不支持的内容块: %T]", item)
	}
}

// errorText 拼 isError 的错误文本：text 按原序拼，其余类型只出占位——image
// 绝不能把 base64 带进来（一张图几百 KB，日志与模型上下文都撑不住，而截图
// 本身可能带敏感信息）。拼完过一道长度兜底。
func errorText(content []sdkmcp.Content) string {
	var builder strings.Builder
	for _, item := range content {
		switch typed := item.(type) {
		case *sdkmcp.TextContent:
			builder.WriteString(typed.Text)
		case *sdkmcp.ImageContent:
			builder.WriteString("[图片]")
		default:
			builder.WriteString(placeholderText(typed))
		}
	}
	if builder.Len() == 0 {
		return "远端返回了空错误"
	}

	return limitText(builder.String())
}

// limitText 兜住远端文本的量级，保证 UTF-8 完整。成功路径用 pi/tools.LimitText
// 做同一件事；错误路径返回的是 error 而不是 ToolOutput，走不到那个函数，所以
// 这里复用同一个上限与同一个标记。
func limitText(text string) string {
	if len(text) <= maxRemoteTextBytes {
		return text
	}
	cut := maxRemoteTextBytes
	for cut > 0 && !utf8.ValidString(text[:cut]) {
		cut--
	}

	return text[:cut] + tools.OutputTruncationMarker
}
