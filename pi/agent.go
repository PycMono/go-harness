package pi

import (
	"context"
	"fmt"

	"github.com/PycMono/go-harness/pi/ai"
	"github.com/PycMono/go-harness/pi/ai/providers"
	errors "github.com/PycMono/go-harness/pi/error"
)

type Options struct {
	WorkDir      string
	providerOpts *providers.Options
}

type Agent struct {
	loop           *Loop
	contextBuilder *ContextBuilder
}

// NewAgent 初始化 agent
func NewAgent(opts *Options) (*Agent, error) {
	if opts.WorkDir == "" {
		return nil, errors.ErrAIWorkDirIsNil
	}

	provider, err := providers.New(opts.providerOpts)
	if err != nil {
		return nil, err
	}

	loop := NewLoop(provider)
	return &Agent{loop: loop, contextBuilder: NewContextBuilder(opts.WorkDir)}, nil
}

func (agent *Agent) Run(ctx context.Context, input *RunInput) (*RunOutput, error) {
	if err := input.Validate(); err != nil {
		return nil, err
	}

	//

	return nil, nil
}

// 准备执行 loop 的上下文
func (agent *Agent) prepareRunContext(ctx context.Context, input *RunInput) (*Context, error) {
	// 组装历史消息
	history := make(ai.Messages, len(input.History))
	for i, message := range input.History {
		converted, err := message.Message2AI()
		if err != nil {
			return nil, err
		}

		history[i] = converted
	}
	inputMessage, err := input.Input.Message2AI()
	if err != nil {
		return nil, err
	}
	if inputMessage.Role != ai.RoleUser {
		return nil, errors.ErrRequestInvalid.Wrap(
			fmt.Errorf("input sender type must be customer"))
	}

	blocks := make([]*ContextBlock, len(input.Context))
	for index, block := range input.Context {
		blocks[index] = &ContextBlock{Name: block.Name, Content: block.Content, Priority: block.Priority}
	}

	loopContext, err := agent.contextBuilder.Build(ctx, history, inputMessage, blocks, nil)
	if err != nil {
		return nil, err
	}

}
