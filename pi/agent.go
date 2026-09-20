package pi

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/PycMono/go-harness/pi/ai/providers"
	pierrors "github.com/PycMono/go-harness/pi/error"
	"github.com/PycMono/go-harness/pi/schema"
	"github.com/PycMono/go-harness/pi/tools"
	"github.com/PycMono/go-harness/pi/tools/impl"
)

// defaultMaxParallel 是同一批工具调用的默认并发上限。
const defaultMaxParallel = 4

type Options struct {
	// WorkDir 是工具的工作目录，也是文件工具解析相对路径的根。
	WorkDir string
	// ProviderOptions 是模型平台配置，必填。
	ProviderOptions *providers.Options
	// Tools 是本轮可用的工具实现，通常来自 impl.NewDefaultTools；为空表示
	// 不带工具运行。
	Tools []tools.Tool
	// MaxParallel 是单批工具调用的并发上限，<= 0 时取默认值。
	MaxParallel int
	// Observer 接收工具执行的生命周期事件，可为 nil 表示丢弃。
	Observer tools.EventObserver
	// TextObserver 接收模型输出的增量文本，可为 nil 表示丢弃。
	TextObserver TextObserver
	// MaxTurns 是单次运行的模型调用次数上限，<= 0 时取默认值。
	MaxTurns int
}

type Agent struct {
	loop           *Loop
	contextBuilder *ContextBuilder
}

// NewAgent 初始化 agent
func NewAgent(opts *Options) (*Agent, error) {
	if opts == nil {
		return nil, pierrors.ErrInitialization.Wrap(errors.New("agent options must not be nil"))
	}
	if strings.TrimSpace(opts.WorkDir) == "" {
		return nil, pierrors.ErrAIWorkDirIsNil
	}
	if opts.ProviderOptions == nil {
		return nil, pierrors.ErrInitialization.Wrap(errors.New("provider options must not be nil"))
	}

	provider, err := providers.New(opts.ProviderOptions)
	if err != nil {
		return nil, err
	}

	// 获取 tools 下面的 4 个默认的工具
	newTools := impl.NewDefaultTools(opts.WorkDir)
	if len(newTools) > 0 {
		opts.Tools = append(opts.Tools, newTools...)
	}

	// 注册工具
	registry, err := tools.Register(opts.Tools)
	if err != nil {
		return nil, err
	}
	registry.Freeze()

	maxParallel := opts.MaxParallel
	if maxParallel <= 0 {
		maxParallel = defaultMaxParallel
	}

	loop := NewLoop(
		provider,
		WithScheduler(tools.NewScheduler(registry, maxParallel, opts.Observer)),
		WithTextObserver(opts.TextObserver),
		WithMaxTurns(opts.MaxTurns),
	)

	return &Agent{loop: loop, contextBuilder: NewContextBuilder(opts.WorkDir)}, nil
}

func (a *Agent) Run(ctx context.Context, input *RunInput) (*RunOutput, error) {
	if input == nil {
		return nil, pierrors.ErrRequestInvalid.Wrap(errors.New("run input must not be nil"))
	}
	if err := input.Validate(); err != nil {
		return nil, err
	}

	runContext, err := a.prepareRunContext(ctx, input)
	if err != nil {
		return nil, err
	}

	messages, err := a.loop.run(ctx, runContext)
	// 运行失败时同样返回已经产生的消息序列，调用方可以据此看到模型跑到哪
	// 一步才出的问题。
	return &RunOutput{message: messages}, err
}

// 准备执行 loop 的上下文
func (a *Agent) prepareRunContext(ctx context.Context, input *RunInput) (*Context, error) {
	// 组装历史消息
	history := make(schema.Messages, len(input.History))
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
	if inputMessage.Role != schema.RoleUser {
		return nil, pierrors.ErrRequestInvalid.Wrap(
			fmt.Errorf("input sender type must be customer"))
	}

	blocks := make([]*ContextBlock, len(input.Context))
	for index, block := range input.Context {
		blocks[index] = &ContextBlock{Name: block.Name, Content: block.Content, Priority: block.Priority}
	}

	// 工具定义交给上下文组装：上下文负责把它们和技能、系统提示词一起排好，
	// 循环只消费组装结果。
	return a.contextBuilder.Build(ctx, history, inputMessage, blocks, a.loop.definitions())
}
