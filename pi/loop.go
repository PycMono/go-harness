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
	messages       schema.Messages
	contextHistory schema.Messages
	availableTools schema.ToolDefinitions
}

// TextObserver 接收模型边生成边吐出的文本增量。它在模型调用期间同步调用，
// 实现应尽快返回，不要在其中做阻塞操作。
type TextObserver func(delta string)

type Loop struct {
	provider  ai.Provider
	scheduler *tools.Scheduler
	onText    TextObserver
	onMessage MessageObserver
	maxTurns  int
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
	state := &runState{
		contextHistory: append([]schema.Message(nil), runContext.Messages...),
		availableTools: append(schema.ToolDefinitions(nil), runContext.Tools...),
	}
	state.messages = append(schema.Messages(nil), state.contextHistory...)

	for turn := 0; ; turn++ {
		if err := ctx.Err(); err != nil {
			return state.messages, pierrors.ErrCanceled.Wrap(fmt.Errorf("agent 运行已取消: %w", err))
		}
		if turn >= l.maxTurns {
			return state.messages, pierrors.ErrRunLimitExceeded.Wrap(
				fmt.Errorf("连续 %d 轮模型调用都要求执行工具，已终止运行", l.maxTurns))
		}

		message, err := l.complete(ctx, state)
		if err != nil {
			return state.messages, err
		}
		// 这一轮的模型消息必须是助手消息：交回 nil 接口或别的具体类型都说明流
		// 实现破坏了契约。这里必须报内部错误——按"没有工具调用"处理会让一次
		// 坏掉的响应看起来像正常收尾，循环静默结束。
		assistant, ok := message.(*schema.AssistantMessage)
		if !ok {
			return state.messages, pierrors.ErrInternal.Wrap(fmt.Errorf(
				"模型响应不是助手消息: %T", message))
		}
		// 模型消息先入列：工具结果必须紧跟在发起调用的那条助手消息之后，
		// 两家协议都按这个顺序还原上下文。
		state.messages = append(state.messages, message)
		l.observe(message)

		if len(assistant.ToolCalls) == 0 {
			return state.messages, nil
		}
		if l.scheduler == nil {
			return state.messages, pierrors.ErrInternal.Wrap(fmt.Errorf(
				"模型请求调用工具 %q，但本轮运行没有接入工具调度器", assistant.ToolCalls[0].Name))
		}

		results, err := l.scheduler.ExecuteBatch(ctx, assistant.ToolCalls)
		if err != nil {
			return state.messages, err
		}
		for index := range results {
			// 工具自身的失败同样作为一条 IsError 的工具消息回给模型，
			// 让模型自己决定重试还是换条路；只有调度层面的失败才中断。
			result, err := results[index].ResultMessage()
			if err != nil {
				// 结束事件缺身份是调度器坏了，不是工具失败：工具失败会以
				// IsError 事件表达，不会走到这里。
				return state.messages, pierrors.ErrInternal.Wrap(err)
			}
			// 追加的是消息值本身：取一条复用变量的地址会让序列里的每条工具结果
			// 都指向同一份内存。
			state.messages = append(state.messages, result)
			l.observe(result)
		}
	}
}

// complete 调用一次模型，边生成边把文本增量交给观察者，返回这一轮的完整消息。
//
// Stream 是拉取式的：Result() 要等流读到结束才有效（结果与错误都在那条路径
// 上产生），所以这里必须一直 Next() 到返回 false。
func (l *Loop) complete(ctx context.Context, state *runState) (schema.Message, error) {
	stream := l.provider.Stream(ctx, state.messages, state.availableTools)
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
