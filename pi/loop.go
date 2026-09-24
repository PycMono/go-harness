package pi

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"

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

// TurnReport 是一轮的现场：这一轮的助手消息与它带出的工具结果。只有带工具
// 调用的轮才会产生它——模型不再要求调用工具时运行就该结束了，没有"还要不要
// 开下一轮"这个问题要问。
type TurnReport struct {
	// Index 是轮次序号，从 0 起，与 run 里 for 的计数一致。
	Index int
	// Message 是这一轮的助手消息。
	Message *schema.AssistantMessage
	// ToolResults 是这一轮的工具结果消息，下标与 Message.ToolCalls 对齐——
	// 判据按下标把结果配到调用上，所以这个对齐是签名正确性的一部分。
	ToolResults schema.Messages
}

// AfterTurn 在每轮的工具结果写回之后调用，是本包里唯一一个"轮次结束"的开口。
// 返回 nil 表示继续；返回错误则收尾——循环把已产生的消息连同这个错误一起交回，
// 不掐 provider 流、不取消正在跑的工具，只是不再开下一轮。
type AfterTurn func(ctx context.Context, report TurnReport) error

type Loop struct {
	provider   ai.Provider
	scheduler  *tools.Scheduler
	onText     TextObserver
	onMessage  MessageObserver
	beforeTurn BeforeTurn
	afterTurn  AfterTurn
	maxTurns   int
	// steering 是外部在运行期间写入、还没交付的消息副本。它在每轮调用模型
	// 之前被取走，作为一条普通消息注入本轮请求。写入方与 run 不在同一个
	// goroutine（Run 是同步阻塞的），所以要锁。
	steering   schema.Messages
	steeringMu sync.Mutex
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

// WithAfterTurn 给循环接一个轮次结束钩子。不设置时行为不变。
func WithAfterTurn(hook AfterTurn) LoopOption {
	return func(loop *Loop) { loop.afterTurn = hook }
}

// observe 把一条刚产生的消息交给观察者。传入的是消息序列里的同一份消息，
// 观察者只读。
func (l *Loop) observe(message schema.Message) {
	if l.onMessage == nil {
		return
	}
	l.onMessage(message)
}

// isNilMessage 判断一条消息是不是"接口非 nil、装着一个类型化 nil"的形态。
// 与 pi/tools 的 isNilTool（pi/tools/interface.go:16-28）、pi/extension 的
// isNilExtension（pi/extension/runtime.go:116-130）同一个写法：那两处是同类
// 问题的既有解法，这里是第三处，照抄而不是各写一套。
func isNilMessage(message schema.Message) bool {
	if message == nil {
		return true
	}
	value := reflect.ValueOf(message)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

// Steer 在运行期间从外部写入一条消息，返回 nil 表示已入队。它在下一次模型
// 调用之前被取走，注入位置在这一轮的助手消息与工具结果之后——既是协议要求的
// 顺序（工具结果必须紧跟发起它的助手消息），也是本仓库自己的排布约定
// （Turn.Messages 拼出来是历史 → 本轮固定段 → 已产生的消息）。
//
// 队列跨 Run 存活：运行开始前写入的消息在第一次模型调用之前交付；运行已经
// 结束时写入的消息留给下一次 Run。Run 是同步阻塞的，所以调用方要在运行期间
// 写入，只能从另一个 goroutine 调这个方法。
//
// 入队的是副本，不是调用方那一条：具体类型都是指针，存原件的话调用方在返回
// 之后改内容，就会让校验过的那份与发出去的那份不是同一份，并与正在读它的 run
// 构成数据竞争。副本用 ContentBlocks.Clone（pi/schema/message_content.go:117，
// 连 Image 指向的那份一起复制），所以图片也是安全的。
//
// 三道拒绝，都在写入口一次做完：
//
//  1. typed nil。message == nil 抓不到 (*schema.UserMessage)(nil) 这种值，
//     它进队之后要到组装请求时才炸，那时离调用点已经很远。
//  2. 角色必须是 user。系统消息在 Anthropic 上会被收进单独的 system 参数，
//     "插在工具结果之后"在那边不成立；在这里拒绝，比在组装请求时静默挪位好
//     ——调用方当场知道这条通道不干那件事。
//  3. 消息自己合法（Validate）。校验的是副本，也就是真正会发出去的那一份。
//     内容为空、图片张数这些 Run 入口会查的东西这里不查：那是请求边界的事，
//     写入方是同一个进程里的代码，不在通道里堆规则。
//
// 三道都用 ErrRequestInvalid（10002）：它们都是调用方写了不该写的东西，不是
// 运行状态的问题。
func (l *Loop) Steer(message schema.Message) error {
	if isNilMessage(message) {
		return pierrors.ErrRequestInvalid.Wrap(errors.New("steering message must not be nil"))
	}
	if role := message.Role(); role != schema.RoleUser {
		return pierrors.ErrRequestInvalid.Wrap(fmt.Errorf(
			"steering message must be a user message, got %q", role))
	}

	// 复制放在校验之前：校验的与入队的是同一份（那条副本）。
	queued := &schema.UserMessage{Content: message.Blocks().Clone()}
	if err := queued.Validate(); err != nil {
		return pierrors.ErrRequestInvalid.Wrap(err)
	}

	l.steeringMu.Lock()
	defer l.steeringMu.Unlock()
	l.steering = append(l.steering, queued)

	return nil
}

// deliver 把取走的消息注入这一轮已经产生的序列，并逐条交给观察者。
// 必须走 observe：会话落盘挂在消息观察者上，绕过它注入的消息只在内存里——
// 盘上的历史与当时发出的请求不一致，下一轮重建就缺一段。
//
// 顺序是我们自己保证的，不是 session 查出来的：Append 在 ParentID 为空时
// 自动取当前叶子（pi/session/manager.go:143-148），观察者走的正是这条路
// （不带 ParentID），所以插在工具结果之前不会报 80002，只会静默把顺序写错。
// 取走点必须落在"这一轮的助手消息与工具结果都写回之后"，测试直接断言消息
// 序列。
//
// 记账：取走点一交付的那些在窗口判定之前进 Produced，参与这一轮的上下文大小
// 估算；取走点二交付的那些是在判定之后才进的，要到下一轮才算进大小。压缩本身
// 留有余量，这不影响判定；写在这里免得将来有人对着 token 数觉得少了一条。
func (l *Loop) deliver(messages schema.Messages, state *runState) {
	for _, message := range messages {
		state.produced = append(state.produced, message)
		l.observe(message)
	}
}

// drainSteering 取走并清空排队中的消息。没有时返回 nil。
func (l *Loop) drainSteering() schema.Messages {
	l.steeringMu.Lock()
	defer l.steeringMu.Unlock()
	if len(l.steering) == 0 {
		return nil
	}
	pending := l.steering
	l.steering = nil

	return pending
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

		// 排队中的插话先交付，再让压缩做窗口判定。反过来（先压缩后交付）的话，
		// 这批消息不在 turn.extra() 里（pi/agent.go 的 compactBeforeTurn 用它
		// 判要不要压），压缩既不会因为它们而触发、也不会在压完之后把它们的
		// 体量算进去——写得多就直接把请求顶过窗口，运行被 provider 的 20003
		// 打掉。
		//
		// 运行开始前写入的消息也在这里交付：循环第一次进来就走到这里。
		l.deliver(l.drainSteering(), state)

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

		// 压缩期间写进来的消息在这里补捞：压缩要调一次模型，慢的话是几秒，
		// 那段时间写入的不该等到下一轮。pi.dev 在 agent-loop.ts:203 单独留了
		// 一个"压缩完再捞一次"的点，理由相同。
		l.deliver(l.drainSteering(), state)

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
		toolResults := make(schema.Messages, 0, len(results))
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
			toolResults = append(toolResults, result)
		}

		// 每轮的工具结果写回之后给上层一次机会判定这是不是循环。判定放在这里
		// 而不是每轮开头：只有"还要开下一轮"的那些轮才有这个问题，模型不再要
		// 工具时上面已经返回了。返回错误就是收尾，与上面 maxTurns 同一种形状
		// ——带消息返回，不中断正在跑的东西。
		if l.afterTurn != nil {
			if err = l.afterTurn(ctx, TurnReport{
				Index:       turn,
				Message:     assistant,
				ToolResults: toolResults,
			}); err != nil {
				return state.messages(), err
			}
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
