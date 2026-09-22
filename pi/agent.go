package pi

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/PycMono/go-harness/pi/ai"
	"github.com/PycMono/go-harness/pi/ai/providers"
	pierrors "github.com/PycMono/go-harness/pi/error"
	"github.com/PycMono/go-harness/pi/middleware"
	"github.com/PycMono/go-harness/pi/schema"
	"github.com/PycMono/go-harness/pi/session"
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
	// Session 是会话管理器，必填。history 只认这一个来源：本轮运行前的历史
	// 从会话重建，本轮产生的消息逐条写回会话。只跑单轮用 session.InMemory()。
	Session *session.Manager
}

type Agent struct {
	loop           *Loop
	contextBuilder *ContextBuilder
	session        *session.Manager
	// writeErr 是本轮会话写入失败的第一个错误。观察者在循环的控制流里同步调用，
	// 没有返回错误的通道，错误只能先攒在这里，等 loop.run 返回后由 Run 透出；
	// 循环在构造期建一次、观察者挂死在上面，所以这个缓冲只能落在 Agent 上，
	// 每轮 Run 开头清空。
	writeErr error
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

	return newAgent(provider, opts)
}

// newAgent 用给定 provider 完成装配：工具注册、工具执行链、循环与观察者。
// NewAgent 与测试共用这条装配路径。
func newAgent(provider ai.Provider, opts *Options) (*Agent, error) {
	if opts.Session == nil {
		return nil, pierrors.ErrInitialization.Wrap(errors.New("session must not be nil"))
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

	agent := &Agent{
		contextBuilder: NewContextBuilder(opts.WorkDir),
		session:        opts.Session,
	}
	// 会话写入接在循环的逐条消息观察者上：模型消息与工具结果产生的当口就落盘，
	// 而不是等 Run 结束后批量补写。
	agent.loop = NewLoop(
		provider,
		WithScheduler(tools.NewScheduler(registry, maxParallel, opts.Observer, middleware.Defaults()...)),
		WithTextObserver(opts.TextObserver),
		WithMaxTurns(opts.MaxTurns),
		WithMessageObserver(func(message schema.Message) {
			// 已经出过错就不再往下写：一次写入失败会滚成一串，真正有用的只有第一个。
			if agent.writeErr != nil {
				return
			}
			agent.writeErr = agent.session.Append(session.Entry{Type: session.EntryMessage, Message: message})
		}),
	)

	return agent, nil
}

func (a *Agent) Run(ctx context.Context, input *RunInput) (*RunOutput, error) {
	if input == nil {
		return nil, pierrors.ErrRequestInvalid.Wrap(errors.New("run input must not be nil"))
	}
	if err := input.Validate(); err != nil {
		return nil, err
	}

	// 一个 Run 一轮账：上一轮的写入错误不带进这一轮。
	a.writeErr = nil
	runContext, err := a.prepareRunContext(ctx, input)
	if err != nil {
		return nil, err
	}

	messages, err := a.loop.run(ctx, runContext)
	// 运行失败时同样返回已经产生的消息序列，调用方可以据此看到模型跑到哪
	// 一步才出的问题。
	if err != nil {
		return &RunOutput{message: messages}, err
	}
	// 会话没写下去比模型出错更隐蔽：消息可能已经发给模型了，但盘上没有，
	// 下一轮重建出来的历史就缺一段，必须让调用方知道。
	if a.writeErr != nil {
		return &RunOutput{message: messages}, a.writeErr
	}

	return &RunOutput{message: messages}, nil
}

// prepareRunContext 准备执行 loop 的上下文
func (a *Agent) prepareRunContext(ctx context.Context, input *RunInput) (*Context, error) {
	inputMessage, err := input.Input.Message2AI()
	if err != nil {
		return nil, err
	}
	if inputMessage.Role() != schema.RoleUser {
		return nil, pierrors.ErrRequestInvalid.Wrap(
			fmt.Errorf("input sender type must be customer"))
	}

	history, err := a.history(inputMessage)
	if err != nil {
		return nil, err
	}

	blocks := make([]*ContextBlock, len(input.Context))
	for index, block := range input.Context {
		blocks[index] = &ContextBlock{Name: block.Name, Content: block.Content, Priority: block.Priority}
	}

	// 工具定义交给上下文组装：上下文负责把它们和技能、系统提示词一起排好，
	// 循环只消费组装结果。
	return a.contextBuilder.Build(ctx, history, inputMessage, blocks, a.loop.definitions())
}

// history 组装本轮的历史消息：从会话重建，并把本轮输入也交给会话记账。
func (a *Agent) history(inputMessage schema.Message) (schema.Messages, error) {
	// 重建必须在追加本轮输入之前：先落盘再重建的话，这条输入会既在历史里、
	// 又被上下文组装再追加一次。
	history := a.session.BuildMessages()
	// 趁模型还没开始跑就落盘：中途崩了，恢复时从客户这句话之后接着聊。
	if err := a.session.Append(session.Entry{Type: session.EntryMessage, Message: inputMessage}); err != nil {
		return nil, err
	}

	return history, nil
}
