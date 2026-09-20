package pi

import (
	"context"
	"sort"
	"strings"

	"github.com/PycMono/go-harness/pi/resources"
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
	input *schema.Message,
	contextBlocks []*ContextBlock,
	definitions schema.ToolDefinitions) (*Context, error) {
	// 加载 agent 和技能
	loader, err := resources.Load(ctx, c.workDir)
	if err != nil {
		return nil, err
	}
	sysPrompt := loader.SystemPrompt() // 加载 agent 和技能
	systemMessage := &schema.Message{
		Role:    schema.RoleSystem,
		Content: []schema.ContentBlock{schema.TextBlock(sysPrompt)},
	}

	// 处理消息
	messages := make([]*schema.Message, 0, 2+len(contextBlocks)+len(history))
	messages = append(messages, append([]*schema.Message(nil), history...)...)
	messages = append(messages, input)
	messages = append(messages, systemMessage)

	blocks := append([]*ContextBlock(nil), contextBlocks...)
	sort.SliceStable(contextBlocks, func(i, j int) bool {
		return contextBlocks[i].Priority > contextBlocks[j].Priority
	})
	for _, block := range blocks {
		messages = append(messages, &schema.Message{
			Role: schema.RoleSystem,
			Content: []schema.ContentBlock{schema.TextBlock(
				"# Context: " + strings.TrimSpace(block.Name) + "\n" + block.Content,
			)},
		})
	}

	return &Context{
		Messages:          messages,
		Tools:             append(schema.ToolDefinitions(nil), definitions...),
		CurrentInputIndex: len(messages) - 1,
	}, nil
}
