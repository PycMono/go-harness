package pi

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/PycMono/go-harness/pi/ai"
	"github.com/PycMono/go-harness/pi/ai/providers"
	pierrors "github.com/PycMono/go-harness/pi/error"
	"github.com/PycMono/go-harness/pi/extension"
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
	// LoopGuardTurns 是循环检测的阈值：连续这么多轮的工具调用与结果完全相同
	// 就终止运行。0 取默认值（3），负数关闭——关闭之后只剩 MaxTurns 兜底。
	LoopGuardTurns int
	// Session 是会话管理器，必填。history 只认这一个来源：本轮运行前的历史
	// 从会话重建，本轮产生的消息逐条写回会话。只跑单轮用 session.InMemory()。
	Session *session.Manager
	// CompactionObserver 接收上下文压缩的结果，可为 nil 表示丢弃。
	CompactionObserver func(CompactionEvent)
	// Extensions 是启动期接入的扩展。
	Extensions []extension.Extension
}

// CompactionEvent 报告一次压缩的结果：TokensBefore 是压缩前的上下文估算；
// Err 为 nil 表示这次压成了，否则表示这次没压、历史原样保留。
type CompactionEvent struct {
	TokensBefore int64
	Err          error
}

type Agent struct {
	loop           *Loop
	contextBuilder *ContextBuilder
	session        *session.Manager
	// provider 是本轮调模型的入口。循环用它跑主对话，agent 自己还要用它跑摘要
	// ——摘要是一次独立、不带工具的调用。
	provider ai.Provider
	// window 是模型的上下文预算，装配时由 ProviderOptions.ContextWindow 算出。
	window session.Window
	// headLeafID 是本轮开始时的叶子：压缩只切到它之前，本轮的输入与产生的消息
	// 都留着。每轮 Run 开头重取。
	headLeafID string
	// guard 是本轮的循环检测。判据在装配时建一次、挂在循环的轮末钩子上，
	// 计数每轮 Run 开头清空——它记的是"连续几轮"，跨 Run 留着就会把两次
	// 不相干的运行接成一段。
	guard *loopGuard
	// onCompaction 接收压缩结果，可为 nil。
	onCompaction func(CompactionEvent)
	// writeErr 是本轮会话写入失败的第一个错误。观察者在循环的控制流里同步调用，
	// 没有返回错误的通道，错误只能先攒在这里，等 loop.run 返回后由 Run 透出；
	// 循环在构造期建一次、观察者挂死在上面，所以这个缓冲只能落在 Agent 上，
	// 每轮 Run 开头清空。
	writeErr error
	// runtime 持有启动期接入的扩展，Close 时逆序关。存 *extension.Runtime 而不是
	// []extension.Closer：关闭顺序与"只关一次"都在 Runtime 里，Agent 只转发。
	runtime *extension.Runtime
	// closed 是关闭标记，Run 入口查一次。谁先谁后不影响结果，"只关一次"由
	// Runtime 的 closeOnce 保证，所以用 Store 就够，不需要 CAS。
	closed atomic.Bool
}

// NewAgent 初始化 agent。ctx 是启动期 ctx：扩展拿它做连接（SDK 的 Connect
// 必须收到 ctx），调用方也能用它给整个启动期设上限——一个连不上的 server
// 不该把启动拖成 N × Timeout。
func NewAgent(ctx context.Context, opts *Options) (*Agent, error) {
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

	return newAgent(ctx, provider, opts)
}

// newAgent 用给定 provider 完成装配：工具注册、扩展接入、工具执行链、循环与
// 观察者。NewAgent 与测试共用这条装配路径。
func newAgent(ctx context.Context, provider ai.Provider, opts *Options) (*Agent, error) {
	if opts.Session == nil {
		return nil, pierrors.ErrInitialization.Wrap(errors.New("session must not be nil"))
	}

	// 获取 tools 下面的 4 个默认的工具
	newTools := impl.NewDefaultTools(opts.WorkDir)
	if len(newTools) > 0 {
		opts.Tools = append(opts.Tools, newTools...) //支持外部 tools 传入，自定义一些工具
	}

	// 注册工具
	registry, err := tools.Register(opts.Tools)
	if err != nil {
		return nil, err
	}

	// 注入扩展
	runtime, err := extension.NewRuntime(opts.Extensions)
	if err != nil {
		return nil, err
	}
	if err := runtime.Register(ctx, registry); err != nil {
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
		provider:       provider,
		window:         session.NewWindow(int64(opts.ProviderOptions.ContextWindow)),
		onCompaction:   opts.CompactionObserver,
		guard:          &loopGuard{limit: loopGuardLimit(opts.LoopGuardTurns)},
		runtime:        runtime,
	}
	// 会话写入接在循环的逐条消息观察者上：模型消息与工具结果产生的当口就落盘，
	// 而不是等 Run 结束后批量补写。压缩挂在每轮的前置钩子上：它要调模型，而
	// agent 是唯一持有 provider 的地方。循环检测挂在轮末的后置钩子上：它要看
	// 的现场（这一轮的调用与结果）只有轮末才齐。
	//
	// 装配不做条件判断：关不关由 guard.limit 的值决定，observe 自己吞掉。
	agent.loop = NewLoop(
		provider,
		WithScheduler(tools.NewScheduler(registry, maxParallel, opts.Observer, middleware.Defaults()...)),
		WithTextObserver(opts.TextObserver),
		WithMaxTurns(opts.MaxTurns),
		WithBeforeTurn(agent.compactBeforeTurn),
		WithAfterTurn(func(_ context.Context, report TurnReport) error {
			return agent.guard.observe(report)
		}),
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

// Close 关闭全部扩展并标记 Agent 已关闭。幂等：真正关一次，重复调用返回第一次
// 的结果；多个扩展的关闭错误用 errors.Join 聚合。语义上不等正在跑的 Run、也不
// 取消它——Run 归调用方的 ctx 管，Close 只做标记与关扩展。已经发出去的那次 MCP
// 调用会照常跑完（SDK 的会话关闭要等在途请求返回），其后同一轮里新起的调用会
// 失败（事件 IsError），Run 本身照常返回。
func (a *Agent) Close(ctx context.Context) error {
	// 先标记、再关扩展：反过来的话，新 Run 会溜进一个正在关闭的会话，拿到的是
	// 莫名其妙的调用失败，而不是 ErrClosed。
	a.closed.Store(true)

	return a.runtime.CloseAll(ctx)
}

func (a *Agent) Run(ctx context.Context, input *RunInput) (*RunOutput, error) {
	if a.closed.Load() {
		return nil, pierrors.ErrClosed
	}
	if input == nil {
		return nil, pierrors.ErrRequestInvalid.Wrap(errors.New("run input must not be nil"))
	}
	if err := input.Validate(); err != nil {
		return nil, err
	}

	// 一个 Run 一轮账：上一轮的写入错误、本轮起点与循环检测的计数都不带进
	// 这一轮。检测计数必须在这里清——它记的是"连续几轮"，跨 Run 留着就会把
	// 两次不相干的运行接成一段。
	a.writeErr = nil
	a.headLeafID = ""
	a.guard.reset()
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
	inputMessage, err := input.promptMessage()
	if err != nil {
		return nil, err
	}
	if inputMessage.Role() != schema.RoleUser {
		return nil, pierrors.ErrRequestInvalid.Wrap(
			fmt.Errorf("input sender type must be customer"))
	}

	// 记下本轮开始时的叶子：压缩只切到它之前，本轮的输入与产生的消息都留着。
	// 必须在追加本轮输入之前取——追加之后叶子就是本轮那条输入了。
	a.headLeafID = a.session.LeafID()
	history, err := a.history(inputMessage)
	if err != nil {
		return nil, err
	}

	// 工具定义交给上下文组装：上下文负责把它们和技能、系统提示词一起排好，
	// 循环只消费组装结果。
	return a.contextBuilder.Build(ctx, history, inputMessage, nil, a.loop.definitions())
}

// compactBeforeTurn 是本轮的前置钩子：越过触发线就压一次，返回替换后的历史段。
//
// 判定用会话算出来的大小，不是自己把请求摊平了估：折叠后的历史里哪些 usage 还
// 作数只有会话知道（边界之前的账会把已经压掉的上下文重新算回来），本轮的 Tail 与
// Produced 作为 extra 一起递进去。
func (a *Agent) compactBeforeTurn(ctx context.Context, turn Turn) (schema.Messages, error) {
	if !a.window.OverLine(a.session.ContextTokens(a.headLeafID, turn.extra())) {
		return nil, nil
	}

	plan, ok := a.session.PlanCompaction(a.window, a.headLeafID)
	if !ok {
		// 越过触发线但计划不成立：路径太短、末尾已经是边界、或本轮起点指不着。
		// 不是错误。
		return nil, nil
	}

	summary, err := a.summarize(ctx, plan)
	if err != nil {
		// 压不动不算这一轮失败：历史还是完整的，只是继续贴着窗口跑。
		// 真溢出了让 provider 去报（20003），比在这里把客户的这一轮打断好。
		a.report(CompactionEvent{TokensBefore: plan.TokensBefore, Err: err})

		return nil, nil
	}

	if err = a.session.Compact(plan, summary); err != nil {
		// 落盘失败与会话写入失败同类：盘上的历史从此缺一块，必须透出。
		return nil, err
	}
	a.report(CompactionEvent{TokensBefore: plan.TokensBefore})

	// 新历史从盘上折回来，不由这里拼：折叠规则只此一份，也就不会出现"计划里的
	// 保留段与读侧折叠不一致"这种要靠人盯的分歧。上界卡在本轮开始的位置——本轮
	// 已经产生的消息在盘上位于新边界之前，不挡住就会既进历史段、又进本轮的部分。
	return a.session.MessagesAt(a.headLeafID), nil
}

// summarize 用当前模型生成一段摘要。这是一次独立、不带工具的调用：不进循环、
// 不发工具描述、不接文本观察者（摘要不该流到用户屏幕上）。
func (a *Agent) summarize(ctx context.Context, plan *session.Plan) (string, error) {
	request, err := plan.Request(a.window)
	if err != nil {
		return "", pierrors.ErrCompactionFailed.Wrap(err)
	}

	stream := a.provider.Stream(ctx, request, nil)
	defer stream.Close()
	for stream.Next() {
	}

	response, err := stream.Result()
	if err != nil {
		return "", pierrors.ErrCompactionFailed.Wrap(err)
	}
	if response == nil {
		return "", pierrors.ErrCompactionFailed.Wrap(errors.New("摘要请求没有产出消息"))
	}
	// 输出上限由 provider 写死（Anthropic 4096，anthropic.go:55），请求里指定不了。
	// 被截断的摘要只是半段文本，拿它当检查点等于用谎言换空间。
	if response.FinishReason == schema.FinishReasonLength {
		return "", pierrors.ErrCompactionFailed.Wrap(errors.New("摘要被输出上限截断"))
	}
	if len(response.ToolCalls) > 0 {
		return "", pierrors.ErrCompactionFailed.Wrap(errors.New("摘要请求返回了工具调用"))
	}
	text, err := response.Content.Text()
	if err != nil {
		return "", pierrors.ErrCompactionFailed.Wrap(err)
	}
	if strings.TrimSpace(text) == "" {
		return "", pierrors.ErrCompactionFailed.Wrap(errors.New("摘要为空"))
	}

	return strings.TrimSpace(text), nil
}

// report 把一次压缩结果交给观察者；没接观察者就丢弃。
func (a *Agent) report(event CompactionEvent) {
	if a.onCompaction == nil {
		return
	}
	a.onCompaction(event)
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
