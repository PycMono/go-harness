package pi

import (
	"context"
	"fmt"

	"github.com/PycMono/go-harness/pi/ai"
	pierrors "github.com/PycMono/go-harness/pi/error"
	"github.com/PycMono/go-harness/pi/schema"
	"github.com/PycMono/go-harness/pi/tools"
)

// defaultMaxTurns 是单次运行内允许的模型调用次数上限。每次模型消息只要带回
// 工具调用就消耗一轮，模型不再调用工具即正常结束；撞到上限说明模型陷入
// 自我循环，直接报错退出而不是无限烧 token。
const defaultMaxTurns = 100

type runState struct {
	head           schema.Messages
	tail           schema.Messages
	produced       schema.Messages
	availableTools schema.ToolDefinitions
}

// turn 把当前状态摊成一次请求。Head 是唯一可被 BeforeTurn 替换的一段。
func (s *runState) turn() Turn {
	return Turn{Head: s.head, Tail: s.tail, Produced: s.produced}
}

// messages 是摊平后的完整序列，也是 run 的返回值。
func (s *runState) messages() schema.Messages {
	return s.turn().Messages()
}

// Turn 是一次模型调用之前的三段请求。压缩只替换 Head：Tail 是本轮固定的输入与
// 系统提示词，Produced 是本轮已经产生的消息，两者都不属于会话历史。
type Turn struct {
	// Head 会话来源的历史段：从根到本轮开始，压缩时整体替换。
	Head schema.Messages
	// Tail 本轮固定段：本轮输入、系统提示词、上下文块。系统提示词与上下文块每轮
	// 重新组装、不落盘；本轮输入在盘上有（agent.go:179），但写在"本轮起点"之后，
	// 不属于历史段。整段都不参与压缩。
	Tail schema.Messages
	// Produced 本轮已经产生的模型消息与工具结果。它们已经在盘上了，但本轮还要
	// 原样带着（工具结果必须紧跟发起调用的那条助手消息）。
	Produced schema.Messages
}

// Messages 按模型看到的顺序拼成一条请求：历史 → 本轮固定段 → 已产生的消息。
func (t Turn) Messages() schema.Messages {
	messages := make(schema.Messages, 0, len(t.Head)+len(t.Tail)+len(t.Produced))
	messages = append(messages, t.Head...)

	return append(messages, t.extra()...)
}

// extra 是不属于历史段的那部分：固定段加已产生的消息。它们进请求，而会话那边折叠
// 只折到本轮起点为止，所以判定大小时要由调用方把 extra 单独递进去
// （Manager.ContextTokens）——两边各带一半，合起来才是这一轮真正的请求。
func (t Turn) extra() schema.Messages {
	extra := make(schema.Messages, 0, len(t.Tail)+len(t.Produced))
	extra = append(extra, t.Tail...)

	return append(extra, t.Produced...)
}

// TextObserver 接收模型边生成边吐出的文本增量。它在模型调用期间同步调用，
// 实现应尽快返回，不要在其中做阻塞操作。
type TextObserver func(delta string)

// BeforeTurn 在每轮模型调用之前调用，传入本轮请求；返回非 nil 时替换请求的
// 历史段（Head），返回 nil 表示不改写。
type BeforeTurn func(ctx context.Context, turn Turn) (schema.Messages, error)

type Loop struct {
	provider   ai.Provider
	scheduler  *tools.Scheduler
	onText     TextObserver
	onMessage  MessageObserver
	beforeTurn BeforeTurn
	maxTurns   int
}

func NewLoop(provider ai.Provider, options ...LoopOption) *Loop {
	loop := &Loop{
		provider: provider,
		maxTurns: defaultMaxTurns,
	}
	for _, option := range options {
		if option != nil {
			option(loop)
		}
	}

	return loop
}

type LoopOption func(*Loop)

// WithScheduler 给循环接上工具调度器。不设置时循环只调用模型，一旦模型请求
// 调用工具就报错——宁可明确失败，也不要静默丢掉模型的工具调用。
func WithScheduler(scheduler *tools.Scheduler) LoopOption {
	return func(loop *Loop) { loop.scheduler = scheduler }
}

// WithTextObserver 接收模型输出的增量文本，用于边生成边展示。不设置时增量
// 文本直接丢弃，只保留最终消息——观察者不是必需的。
func WithTextObserver(observer TextObserver) LoopOption {
	return func(loop *Loop) { loop.onText = observer }
}

// WithMaxTurns 覆盖单次运行的模型调用次数上限；n <= 0 时保持默认值。
func WithMaxTurns(n int) LoopOption {
	return func(loop *Loop) {
		if n > 0 {
			loop.maxTurns = n
		}
	}
}

// MessageObserver 接收循环逐条产生的模型消息与工具结果消息。
type MessageObserver func(message schema.Message)

// WithMessageObserver 逐条接收循环产生的消息，用于会话持久化；不设置时
// 行为不变，与 TextObserver 一样在单线程控制流中同步调用。
func WithMessageObserver(observer MessageObserver) LoopOption {
	return func(loop *Loop) { loop.onMessage = observer }
}

// WithBeforeTurn 给循环接一个前置钩子。不设置时行为不变。
func WithBeforeTurn(hook BeforeTurn) LoopOption {
	return func(loop *Loop) { loop.beforeTurn = hook }
}

// observe 把一条刚产生的消息交给观察者。传入的是消息序列里的同一份消息，
// 观察者只读。
func (l *Loop) observe(message schema.Message) {
	if l.onMessage == nil {
		return
	}
	l.onMessage(message)
}

// definitions 返回本轮可用工具的快照。注册表已按名称排好序，这里只是把值
// 换成上下文使用的指针形式。
func (l *Loop) definitions() schema.ToolDefinitions {
	if l.scheduler == nil {
		return nil
	}

	registered := l.scheduler.Definitions()
	definitions := make(schema.ToolDefinitions, len(registered))
	for index := range registered {
		definitions[index] = &registered[index]
	}

	return definitions
}

// run 执行一次完整的 agent 循环：调用模型 → 模型要求调用工具就执行并把结果
// 写回消息序列 → 带着工具结果再次调用模型，直到模型不再要求调用工具为止。
// 返回的是本轮运行产生的完整消息序列（上下文 + 模型消息 + 工具结果）；即使
// 中途出错，返回值也包含已经产生的部分，便于上层排查。
func (l *Loop) run(ctx context.Context, runContext *Context) (schema.Messages, error) {
	// 三段按 CurrentInputIndex 切：ContextBuilder.Build 的排布是"历史 → 本轮输入 →
	// 系统提示词 → 上下文块"，所以这个下标正好是"历史"与"本轮"的分界。校验越界不是
	// 形式主义：Context 是导出类型，调用方可以自己造一个塞进 run。
	split := runContext.CurrentInputIndex
	if split < 0 || split > len(runContext.Messages) {
		return nil, pierrors.ErrInternal.Wrap(fmt.Errorf(
			"上下文的本轮输入下标 %d 越界（共 %d 条消息）", split, len(runContext.Messages)))
	}
	state := &runState{
		head:           append(schema.Messages(nil), runContext.Messages[:split]...),
		tail:           append(schema.Messages(nil), runContext.Messages[split:]...),
		availableTools: append(schema.ToolDefinitions(nil), runContext.Tools...),
	}

	for turn := 0; ; turn++ {
		if err := ctx.Err(); err != nil {
			return state.messages(), pierrors.ErrCanceled.Wrap(fmt.Errorf("agent 运行已取消: %w", err))
		}
		if turn >= l.maxTurns {
			return state.messages(), pierrors.ErrRunLimitExceeded.Wrap(
				fmt.Errorf("连续 %d 轮模型调用都要求执行工具，已终止运行", l.maxTurns))
		}

		// 每轮调用模型之前给上层一次机会改写历史段（压缩就挂在这里）。返回 nil
		// 表示不改写；报错则带上已经产生的部分退出。
		if l.beforeTurn != nil {
			head, err := l.beforeTurn(ctx, state.turn())
			if err != nil {
				return state.messages(), err
			}
			if head != nil {
				state.head = head
			}
		}

		message, err := l.complete(ctx, state)
		if err != nil {
			return state.messages(), err
		}
		// Provider 的 Stream.Result 已经把返回类型收窄为助手消息，Loop 不再需要
		// 对通用 Message 做运行时类型断言。
		assistant := message
		// 模型消息先入列：工具结果必须紧跟在发起调用的那条助手消息之后，
		// 两家协议都按这个顺序还原上下文。
		state.produced = append(state.produced, message)
		l.observe(message)

		if len(assistant.ToolCalls) == 0 {
			return state.messages(), nil
		}
		if l.scheduler == nil {
			return state.messages(), pierrors.ErrInternal.Wrap(fmt.Errorf(
				"模型请求调用工具 %q，但本轮运行没有接入工具调度器", assistant.ToolCalls[0].Name))
		}

		results, err := l.scheduler.ExecuteBatch(ctx, assistant.ToolCalls)
		if err != nil {
			return state.messages(), err
		}
		for index := range results {
			// 工具自身的失败同样作为一条 IsError 的工具消息回给模型，
			// 让模型自己决定重试还是换条路；只有调度层面的失败才中断。
			result, err := results[index].ResultMessage()
			if err != nil {
				// 结束事件缺身份是调度器坏了，不是工具失败：工具失败会以
				// IsError 事件表达，不会走到这里。
				return state.messages(), pierrors.ErrInternal.Wrap(err)
			}
			// 追加的是消息值本身：取一条复用变量的地址会让序列里的每条工具结果
			// 都指向同一份内存。
			state.produced = append(state.produced, result)
			l.observe(result)
		}
	}
}

// complete 调用一次模型，边生成边把文本增量交给观察者，返回这一轮的完整消息。
//
// Stream 是拉取式的：Result() 要等流读到结束才有效（结果与错误都在那条路径
// 上产生），所以这里必须一直 Next() 到返回 false。
func (l *Loop) complete(ctx context.Context, state *runState) (*schema.AssistantMessage, error) {
	stream := l.provider.Stream(ctx, state.messages(), state.availableTools)
	defer stream.Close()

	for stream.Next() {
		event := stream.Current()
		if event.Type != schema.StreamEventTextDelta || l.onText == nil {
			continue
		}
		l.onText(event.TextDelta)
	}

	return stream.Result()
}
