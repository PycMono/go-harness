package pi

import (
	"context"
	"sort"
	"strings"

	"github.com/PycMono/go-harness/pi/ai"
	"github.com/PycMono/go-harness/pi/tools"
)

// Context 模型上下文信息
type Context struct {
	Messages ai.Messages
	Tools    tools.ToolDefinitions
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
	history ai.Messages,
	input *ai.Message,
	contextBlocks []*ContextBlock,
	definitions tools.ToolDefinitions) (*Context, error) {
	// todo 加载技能

	// 处理消息
	messages := make([]*ai.Message, 0, 2+len(contextBlocks)+len(history))
	messages = append(messages, append([]*ai.Message(nil), history...)...)
	messages = append(messages, input)

	blocks := append([]*ContextBlock(nil), contextBlocks...)
	sort.SliceStable(contextBlocks, func(i, j int) bool {
		return contextBlocks[i].Priority > contextBlocks[j].Priority
	})
	for _, block := range blocks {
		messages = append(messages, &ai.Message{
			Role: ai.RoleSystem,
			Content: []tools.ContentBlock{tools.TextBlock(
				"# Context: " + strings.TrimSpace(block.Name) + "\n" + block.Content,
			)},
		})
	}

	return &Context{
		Messages:          messages,
		Tools:             append(tools.ToolDefinitions(nil), definitions...),
		CurrentInputIndex: len(messages) - 1,
	}, nil
}
