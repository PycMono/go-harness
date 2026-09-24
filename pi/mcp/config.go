package mcp

import (
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	pierrors "github.com/PycMono/go-harness/pi/error"
)

// ServerConfig 是一个 MCP server 的接入配置：装配方把要接的 server 收拾成这样一份
// 交给 NewExtension。
//
// 它不带 json 标签，也不认配置文件里那些字段名——文件长什么样、读哪个文件，由读配置
// 的那一侧定（各驱动自己的 config，见 cmd/internal/mcpconfig），读出来转成这份运行期
// 结构。分成两边的理由：文件格式会跟着别的客户端变，而这边只该认"接一个 server 需要
// 知道哪些值"。
//
// 字段的规矩与判定都在 NewExtension 的 validate 里：名字合不合规、transport 认不认得、
// header_env 缺没缺、http 与 stdio 的字段有没有串。
type ServerConfig struct {
	// Name 是本地标识，用作工具名前缀与 owner。必须匹配 ^[a-z0-9][a-z0-9_-]*$：
	// 它会进工具名，而工具名要经得起两家 Provider 的口径。
	Name string
	// Enabled 为假时这个 server 完全不接：不连、不注册。缺省视为启用。
	Enabled *bool
	// Transport 是传输类型，必填判别字段：http 或 stdio。
	Transport TransportType
	// URL 是 Streamable HTTP 的单一入口。仅 http 用。
	URL string
	// Headers 是每个请求附带的固定头。仅 http 用。
	Headers map[string]string
	// HeaderEnv 也是请求头，只是值写成环境变量的名字：{"X-Api-Key":
	// "EXA_API_KEY"} 表示这个头的值从环境里取。凭据不进配置文件，也就不进
	// 版本库。仅 http 用。
	HeaderEnv map[string]string
	// Command/Args/Env/Cwd 是 stdio 的启动参数。仅 stdio 用。
	Command string
	Args    []string
	Env     map[string]string
	Cwd     string
	// Required 为真时，这个 server 连接或工具发现失败会让 Agent 装配失败；
	// 为假（默认）时只记一条 Warn，其余 server 与 Agent 照常起。
	Required bool
	// AllowTools 非空时只接入列出的远端工具；空表示接入全部。名字按原样用，
	// 不 trim 也不去重：写错了会在装配时报"白名单里的工具远端没有"，点出那个
	// 名字（多出来的空格也跟着打出来）。
	AllowTools []string
	// ToolPrefix 换掉本地工具名的第一段（默认 mcp）：本地名形如
	// <tool_prefix>__<server>__<远端名>，server 那一段始终在，多个 server 的
	// 同名工具不会撞在一起。空表示用默认值。
	ToolPrefix string
	// Timeout 是单次远端操作的超时：一次连接、一次工具发现、一次工具调用各
	// 算一次。0 表示取默认值。
	Timeout time.Duration
}

// IsEnabled 报告这个 server 要不要接：缺省是接。装配时按它跳过停用的条目；装配方
// 想按同一条规则显示"哪些没接"，也用它，别在外面重写一遍。
func (c *ServerConfig) IsEnabled() bool {
	return c.Enabled == nil || *c.Enabled
}

// invalid 造一个配置类错误。包一层是为了让每条判定占一行——判定有十来条，各自铺一遍
// Wrap 与 fmt.Errorf 会把这页撑成两页。
func (c *ServerConfig) invalid(format string, args ...any) error {
	return pierrors.ErrMCPConfigInvalid.Wrap(fmt.Errorf(format, args...))
}

// blockedHeaders 是传输自己在管的头，用户覆盖会把协议搞坏。比对前两侧都过
// http.CanonicalHeaderKey，否则写 host 或 HOST 就绕过去了。
var blockedHeaders = map[string]struct{}{
	"Host": {}, "Content-Length": {}, "Mcp-Session-Id": {},
	"Content-Type": {}, "Accept": {},
}

var serverNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// checkToolName 校验会进工具名的那几个字段。它们宽松一点就会在远端以难查的方式
// 失败：工具名要经得起两家 Provider 的口径。
func (c *ServerConfig) checkToolName(what, value string) error {
	if serverNamePattern.MatchString(value) {
		return nil
	}

	return c.invalid("server %q 的 %s %q 必须匹配 ^[a-z0-9][a-z0-9_-]*$（它会进工具名）",
		c.Name, what, value)
}

// validate 校验并补齐配置：它会把缺省的 Timeout 填成默认值、把 header_env 换成
// 真的头值，所以收指针。配置类错误一律致命，与 Required 无关——Required 管的是
// 远端可达性与工具发现这类运行期的事，配置写错是本地的事，混在一个开关下会让
// "错在哪"变难查。
func (c *ServerConfig) validate() error {
	if err := c.checkToolName("名字", c.Name); err != nil {
		return err
	}
	if c.ToolPrefix != "" {
		if err := c.checkToolName("tool_prefix", c.ToolPrefix); err != nil {
			return err
		}
	}
	// 非正数一律当"没配"取默认值。显式写的非正数在读文件那侧已经被拒了，走到这里
	// 的多半是手写 ServerConfig 的调用方——0 表示没配，负数按同一条规则处理，不
	// 额外报一次，判定少一条。
	if c.Timeout <= 0 {
		c.Timeout = defaultTimeout
	}
	if err := c.resolveHeaderEnv(); err != nil {
		return err
	}

	switch c.Transport {
	case TransportHTTP:
		return c.validateHTTP()
	case TransportStdio:
		return c.validateStdio()
	default:
		return pierrors.ErrMCPTransportUnsupported.Wrap(fmt.Errorf(
			"server %q transport %q 不支持，可选值: %s, %s",
			c.Name, c.Transport, TransportHTTP, TransportStdio))
	}
}

// resolveHeaderEnv 把 header_env 里的环境变量名换成值并进 Headers。环境变量没
// 设置时报错并点出变量名：凭据没装上，连接与每次调用都会失败，早说比晚说省事。
// 值本身不进错误文案也不进日志，这里只说变量名。
func (c *ServerConfig) resolveHeaderEnv() error {
	// 按变量名排序：缺好几个时错误文案是稳定的，测试与人都照着同一句话看。
	for _, name := range slices.Sorted(maps.Keys(c.HeaderEnv)) {
		envName := c.HeaderEnv[name]
		value, exists := os.LookupEnv(envName)
		if !exists || value == "" {
			return c.invalid("server %q header %q 要的环境变量 %s 没有设置",
				c.Name, name, envName)
		}
		if c.Headers == nil {
			c.Headers = make(map[string]string, len(c.HeaderEnv))
		}
		c.Headers[name] = value
	}

	return nil
}

func (c *ServerConfig) validateHTTP() error {
	if c.Command != "" || len(c.Args) > 0 || len(c.Env) > 0 || c.Cwd != "" {
		return c.invalid("server %q transport 是 http，command/args/env/cwd 只给 stdio 用",
			c.Name)
	}

	parsed, err := url.Parse(c.URL)
	if err != nil {
		return c.invalid("server %q url %q: %w", c.Name, c.URL, err)
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return c.invalid("server %q url %q 必须是 http 或 https 且带 host", c.Name, c.URL)
	}
	// userinfo 与 query 里常放 token，而 endpoint 会进错误文案与日志（见方案
	// 「凭据」那条）：凭据一律走 headers，URL 保持干净。
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return c.invalid("server %q url 不能带 userinfo、query 或 fragment", c.Name)
	}
	for name := range c.Headers {
		if _, blocked := blockedHeaders[http.CanonicalHeaderKey(name)]; blocked {
			return c.invalid("server %q header %q 由传输自己管理，不能覆盖", c.Name, name)
		}
	}

	return nil
}

func (c *ServerConfig) validateStdio() error {
	if c.URL != "" || len(c.Headers) > 0 || len(c.HeaderEnv) > 0 {
		return c.invalid("server %q transport 是 stdio，url/headers/header_env 只给 http 用",
			c.Name)
	}
	if strings.TrimSpace(c.Command) == "" {
		return c.invalid("server %q transport 是 stdio，必须给 command", c.Name)
	}

	return nil
}
