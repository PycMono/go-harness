package pi

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/PycMono/go-harness/pi/ai"
	"github.com/PycMono/go-harness/pi/tools"
)

type runState struct {
	messages       ai.Messages
	contextHistory ai.Messages
	availableTools tools.ToolDefinitions
}

type Loop struct {
	provider ai.Provider
}

type LoopOption func(*Loop)

func NewLoop(provider ai.Provider, options ...LoopOption) *Loop {
	loop := &Loop{
		provider: provider,
	}
	for _, option := range options {
		if option != nil {
			option(loop)
		}
	}

	return loop
}

func (l *Loop) run(ctx context.Context, runContext *Context) (ai.Messages, error) {
	state := &runState{
		contextHistory: append([]*ai.Message(nil), runContext.Messages...),
	}

	state.availableTools = append(tools.ToolDefinitions(nil), runContext.Tools...)
	slices.SortFunc(state.availableTools, func(a, b *tools.ToolDefinition) int {
		return cmp.Compare(a.Name, b.Name)
	})

	// for 循环
	for {
		if err := ctx.Err(); err != nil {
			return finish(fmt.Errorf("agent 运行已取消: %w", err))
		}
	}
}
