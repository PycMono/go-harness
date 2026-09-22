package pi

import (
	"context"
	"sort"
	"strings"

	"github.com/PycMono/go-harness/pi/schema"
)

/*
	模型上下文管理
*/

// Context 模型上下文管理
type Context struct {
	Messages schema.Messages
	Tools    schema.ToolDefinitions
	Context  []*ContextBlock
	// 供压缩识别本次真实用户输入
	CurrentInputIndex int
}

type ContextBuilder struct {
	workDir string
}

func NewContextBuilder(workDir string) *ContextBuilder {
	return &ContextBuilder{workDir: workDir}
}

// Build 构建上下文
func (c *ContextBuilder) Build(
	ctx context.Context,
	history schema.Messages,
	input schema.Message,
	contextBlocks []*ContextBlock,
	definitions schema.ToolDefinitions) (*Context, error) {
	// 组装系统提示词（核心纪律 + 技能目录 + AGENTS.md，细节见 prompt.go）
	sysPrompt, err := SystemPrompt(ctx, c.workDir)
	if err != nil {
		return nil, err
	}
	systemMessage, err := schema.NewSystemMessage([]schema.ContentBlock{schema.TextBlock(sysPrompt)})
	if err != nil {
		return nil, err
	}

	// 处理消息
	messages := make([]schema.Message, 0, 2+len(contextBlocks)+len(history))
	messages = append(messages, history...)
	// 本轮输入落在历史之后，压缩要按这个下标认出"客户这次说了什么"。
	currentInputIndex := len(messages)
	messages = append(messages, input)
	messages = append(messages, systemMessage)

	// 排的是副本：Priority 决定注入顺序，但不该改写调用方传进来的切片。
	blocks := append([]*ContextBlock(nil), contextBlocks...)
	sort.SliceStable(blocks, func(i, j int) bool {
		return blocks[i].Priority > blocks[j].Priority
	})
	for _, block := range blocks {
		// 系统提示词与业务上下文块共用同一个位置：都在本轮输入之后，作为对话
		// 中间的系统消息插进去。
		contextMessage, err := schema.NewSystemMessage([]schema.ContentBlock{schema.TextBlock(
			"# Context: " + strings.TrimSpace(block.Name) + "\n" + block.Content,
		)})
		if err != nil {
			return nil, err
		}
		messages = append(messages, contextMessage)
	}

	return &Context{
		Messages:          messages,
		Tools:             append(schema.ToolDefinitions(nil), definitions...),
		CurrentInputIndex: currentInputIndex,
	}, nil
}
