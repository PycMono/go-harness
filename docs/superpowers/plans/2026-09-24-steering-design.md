# 插话方案

## 一句话

`pi/loop.go` 加一条运行期间的入口通道：外部往运行中的循环里写一条消息，循环在每轮调用模型之前取走（压缩之前一次、压缩之后再一次），作为一条普通消息注入本次请求，并走与模型消息相同的落盘路径。对应 pi.dev 的 `sendMessage(..., { deliverAs: "steer" })` 加它的轮询点，只做 steer，不做 followUp。

## 决策一览（先看这个）

| # | 决策 | 选择 | 备选与代价 |
|---|---|---|---|
| 1 | 通道形态 | 队列 + 每轮调用模型之前取走（压缩前后各一次） | 立刻注入：模型调用正在跑，没有插入的位置；在"每个工具调用之前"注入：一轮里有多个调用，位置不确定，而且会破坏"工具结果必须紧跟发起它的助手消息"这条协议顺序 |
| 2 | steer 与 followUp | 只做 steer | 两个都做：followUp 的语义是"等循环要停下来时才交付"（`agent-loop.ts:267`），是追加任务不是改方向，多一个队列、多一个取走点、多一组测试 |
| 3 | 入口挂在哪 | `Agent.Steer` 加一个单方法接口 `Steerer` | 加进 `Runner`（`pi/runner.go:16`）：能不能跑与能不能插话是两件事，混进去之后"实现 Runner"变成"既要能跑又要能插话"；只放 `Agent` 不导出接口：调用方手里是 `Runner`，看不见 `Agent` 的方法 |
| 4 | 并发 | `sync.Mutex` | 无锁：Go 里对同一个 slice 的并发 append 不是原子操作，会丢数据或越界 |
| 5 | 队列生命周期 | 跨 Run 存活：运行期间写入在下一轮模型调用之前交付，运行开始前写入在第一次模型调用之前交付 | 运行结束时清空：写晚一步的消息被静默吞掉，调用方无从知道；只在运行期间有效：`Run` 是同步阻塞的，调用方要在运行前排队就得先起 goroutine 再抢时序 |
| 6 | 注入位置 | 每轮调用模型之前两处：`beforeTurn` **之前**一处、`beforeTurn` 之后 `l.complete` 之前一处 | 只留压缩之后那一处（上一版的选择）：压缩的窗口判定看不见排队中的消息——`compactBeforeTurn` 用 `turn.extra()`（只含当时的 Tail 与 Produced）判断要不要压（`pi/agent.go:245`），而这批插话是判定之后才进的 Produced，于是它既不参与"要不要压"、也不参与"压完够不够"，一次写得多就直接把请求顶过窗口，运行被 provider 的 20003 打掉（`pi/ai/providers/anthropic.go:121`）；只留压缩之前那一处：`beforeTurn` 挂的压缩会调一次模型（`pi/agent.go:256`、`:285`），那段时间写进来的消息要等一整轮，pi.dev 在 `agent-loop.ts:203` 单独留一个"压缩完再捞一次"的点正是为它。两处的分工不同（一处管记账与压缩判定，一处管压缩窗口内的写入），所以两处都要 |
| 7 | 一次取几条 | 全取 | pi.dev 默认 `one-at-a-time`（`agent.ts:246`，`types.ts:53-55`）：它那么做是因为人在 TUI 里能看到每条插话的效果再决定下一条；本仓库的 `Run` 阻塞返回，调用方拿不到中途反馈，一条条交付只剩复杂度 |
| 8 | 消息角色 | 只收 user，其余拒绝 | 不限角色：本来是想对齐 pi.dev 在同一个位置插系统消息的做法（`agent-loop.ts:209` 的 `declareToolChanges`），但那个用法在两家 provider 上的落点不同——Anthropic 把系统消息收进单独的 `system` 参数（`pi/schema/message_convert.go:63-69`），OpenAI 留在消息序列里（`:20-25`），"插在工具结果之后"在 Anthropic 上根本不成立（它会跑到整条请求最前面）。收窄成 user：一条普通用户消息，位置与语义在两家上都是确定的 |
| 9 | 与循环检测的关系 | 无关，独立 | 合并成一份：检测器用插话改方向是"拿通道当喇叭"，通道本身不需要知道有检测器（见 `2026-09-24-loop-guard-design.md`） |
| 10 | 清空入口 | 不做 | pi.dev 有 `clearQueue`（`agent-session.ts:1754`）：要它得先有"队列可见"这个概念——UI 得能展示待交付的消息，用户才知道该清什么，那是交互层的事 |
| 11 | 内容生成 | 不管，通道只负责送 | 通道拼文案：它不知道为什么要插话，检测器要插话就自己拼（那是下一件事） |
| 12 | 运行出错同时又落盘失败 | 两个错都用 `errors.Join` 带上（改 `pi/agent.go:203-210`） | 只报一个：运行错误先发生，只报它会吞掉"盘上历史缺了一段"这个更隐蔽的失败（`pi/agent.go:208` 的注释就是为此写的）；只报写入错会吞掉"模型为什么停"。两个错同时出现本来就是可能且都有用的，`Run` 的返回只有一条错误通道，只能合 |
| 13 | 入队的是原件还是副本 | 副本 | 存原件：`schema.Message` 的具体类型都是指针（`schema.UserMessage` 的唯一字段是 `Content ContentBlocks`，`pi/schema/message.go:117-120`），调用方在 `Steer` 返回之后改内容，就会让"入队时校验过的那份"与"发出去、落盘的那份"不是同一份，并与正在读它的 `Run` 构成数据竞争；要求调用方"写完别改"是约定，不是约束 |
| 14 | 待交付批次要不要封顶 | 不封顶 | 封顶（按字节或条数，超出在 `Steer` 处拒绝）：上一版加过，删掉了。它挡的是"一次写很多把请求顶过窗口"，而这个洞已经由决策 6 的两处取走点堵上——这批消息参与本轮的窗口判定，压缩会看见它们。剩下那点余量不足的情况（压完仍不够）本仓库本来就交给 provider 报 20003（`pi/agent.go:258` 的注释就是这个取舍），不在这里另设一道闸 |

## 目标与非目标

目标：

- 运行期间从外部写入一条消息，它在下一次模型请求里出现，位置在这一轮的助手消息与工具结果之后。
- 写入的消息成为会话历史的一部分：写入之后重启，重建出来的上下文与当时发出的请求一致。
- 写入不阻塞 `Run`，不打断正在执行的工具。
- 运行开始前写入的消息也能送达（对齐 pi.dev `agent-loop.ts:174` 的开头轮询）。
- 已经排进队列的消息参与这一轮的窗口判定：压缩看得到它们的大小，所以插话不会在判定之后把请求顶过窗口（见决策 6）。
- 写进去的东西不会再被改：入队的是副本，之后调用方怎么动原件都不影响发出去与落盘的那一份（见决策 13）。

非目标（明确不做，附理由）：

- **followUp 语义**（等循环要停下来时再交付，`agent-loop.ts:267`）。它是"追加任务"，与"运行期间改方向"不同：前者不需要在轮次边界抢时间，后者需要。两个队列会让"这一条什么时候生效"变成一个要查文档的问题。
- **队列的可见与清空**。没有 `Pending` 查询、没有 `Clear`。理由见决策 10：这两件事的服务对象是 UI，不是通道本身。调用方是写入方，它知道自己在写什么。
- **插话的内容生成**。通道只送不编。循环检测将来要插话，文案由检测侧负责。
- **插话图片与附件**。`Steer` 收 `schema.Message`，要发图自己用 `schema.NewUserMessage` 构造（`pi/runner.go:53` 的 `promptMessage` 就是这么做的）。入队时会连 `Image` 指向的那份一起复制，所以入队之后改图也不影响已排队的那条。
- **空内容与图片张数的检查**。`Steer` 只查"是不是 nil、角色对不对、自身合不合法"。空白内容、图片超过 `MaxImagesPerMessage` 这些 `Run` 入口查的东西（`pi/runner.go:36-51`）这里不查：两条入口的使用者不同——`Run` 的输入来自外部请求，多查一道划算；插话的写入方是同一个进程里的代码，它写错东西该让 provider 报回来，不在通道里堆规则。
- **运行结束后的自动交付**。`Run` 返回之后写入的消息留给下一次 `Run`，不在本次运行结束前抢送。写入与结束的竞态结果就是"留到下一次"，这一点写进 `Steer` 的文档注释，不靠约定。
- **会话层的顺序校验**。`session.Manager.Append` 在 `ParentID` 为空时自动取当前叶子（`pi/session/manager.go:143-148`），所以它管的是"写入者是不是落在了过期的位置上"，不是"这条消息在语义上该不该排在这"。轮次边界的顺序由我们自己保证、由测试直接断言消息序列，不指望 session 报 80002（详见「落盘」第 1 条）。

## 归属：每个名字住在哪

| 名字 | 形态 | 位置 | 为什么在这 |
|---|---|---|---|
| `Loop.Steer` | 方法 | `pi/loop.go` | 队列是循环的输入通道，写入与取走都在循环这一层 |
| `drainSteering` | 方法（包内） | `pi/loop.go` | 取走并清空；只在 `run` 里调用 |
| `Agent.Steer` | 方法 | `pi/agent.go` | 对外入口，转发给循环 |
| `Steerer` | 接口 | `pi/runner.go` | 与 `Runner` 并列的运行契约：单方法，只回答"能不能插话" |

对外面只有两个新名字：`Steerer` 与 `Agent.Steer`。`Loop.Steer` 与 `drainSteering` 跟着 `Loop` 走——`Loop` 已经导出，但它的使用者是装配方（`pi` 包根自己），不是上层业务。

`Steerer` 与 `Runner` 分开而不是合并成一个大接口，理由是这两个能力不该被绑在一起：一个只读的驱动器（比如回放会话做测试）能实现 `Run` 但没有任何插话的语义；反过来，将来若有别的运行实现（比如 `pi/ai` 之外的另一条执行路径），它不必为了插话去实现一整套 `Run`。Go 的接口是隐式实现的，分开不增加任何实现负担。

## 文件清单（这轮动了哪些）

```
pi/
├── loop.go            改：steering 字段与锁、Steer（校验 + 复制）、drainSteering、deliver、run 里两处取走点
├── agent.go           改：Agent.Steer（转发）、Run 的双错误返回（errors.Join）
├── runner.go          改：Steerer 接口
└── steering_test.go   新增：两处取走点、落盘顺序、复制、并发、运行前写入
```

不新增文件夹，不动 `pi/session`（插话就是一条普通 message entry，`session.EntryMessage`，不需要新 entry 类型）、不动 `pi/tools`、不动 `pi/error`（拒绝理由走既有的 `ErrRequestInvalid`）。

测试文件里需要一个最小 provider 桩（实现 `ai.Provider` 与 `ai.Stream`）。原先约定它与 `2026-09-24-loop-guard-design.md` 里的那一份共用，实际落地时 `pi/loopguard_test.go` 已经从工作区删除，所以这一份由 `pi/steering_test.go` 自己引入（`stubProvider` / `steeringProvider` / `echoTool` / `newTestLoop` / `newTestAgent` / `testRunContext`）。将来若再写一份测试，直接用这批，不要再造一个。

`steeringProvider` 是桩上面薄薄一层：`Run` 是同步阻塞的，测试要在运行期间写入，只能在循环调用 provider 的当口动手，所以它多一个"每次请求 provider 之前"的回调。

## 通道：`Steer`

```go
type Loop struct {
	...
	// steering 是外部在运行期间写入、还没交付的消息副本。它在每轮调用模型
	// 之前被取走，作为一条普通消息注入本轮请求。写入方与 run 不在同一个
	// goroutine（Run 是同步阻塞的），所以要锁。
	steering   schema.Messages
	steeringMu sync.Mutex
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
// （Turn.Messages 是 Head → Tail → Produced，见 pi/loop.go:49-55）。注意这条
// 顺序不是 session 帮我们查的，见「落盘」第 1 条。
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
//  2. 角色必须是 user。理由见决策 8：系统消息在 Anthropic 上会被挪到整条
//     请求的最前面，"插在工具结果之后"在那边不成立。在这里拒绝，比在组装
//     请求时静默挪位好——调用方当场知道这条通道不干那件事。
//  3. 消息自己合法（Validate）。校验的是副本，也就是真正会发出去的那一份。
//     内容为空、图片张数这些 Run 入口会查的东西这里不查，理由见非目标。
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
```

## 两处取走点

pi.dev 有三个取走点（`agent-loop.ts:174` 运行开头、`:203` `prepareNextTurn` 之后、`:263` 每个 `turn_end` 之后），其中前两个落在同一轮的两端：`:174` 在轮次开头的判定处，`:203` 在压缩跑完之后（注释写着"压缩可能很慢"）。本仓库要两处，位置对应：每轮 `beforeTurn` 之前一处、`beforeTurn` 之后 `l.complete` 之前一处。

上一版设计只留一处、放在压缩之后，这一版改回来。原因是记账：只留压缩之后那一处时，排队中的消息赶不上这一轮的窗口判定，一次写得多就把请求顶过窗口（推导见决策 6）。两处不是重复劳动，分工不同——一处管"这一轮要发的东西先把账算清"，一处管"算账期间新来的别丢"。

### 取走点一：`beforeTurn` 之前

位置在 maxTurns 判定之后、`l.beforeTurn` 调用之前：

```go
		// 排队中的插话先交付，再让压缩做窗口判定。反过来（先压缩后交付）的话，
		// 这批消息不在 turn.extra() 里（pi/agent.go:245），压缩既不会因为它们
		// 而触发、也不会在压完之后把它们的体量算进去——写得多就直接把请求顶过
		// 窗口，运行被 provider 的 20003 打掉。
		//
		// 运行开始前写入的消息也在这里交付：循环第一次进来就走到这里。
		l.deliver(l.drainSteering(), state)
```

### 取走点二：`beforeTurn` 之后、`l.complete` 之前

```go
		if l.beforeTurn != nil {
			head, err := l.beforeTurn(ctx, state.turn())
			if err != nil {
				return state.messages(), err
			}
			if head != nil {
				state.head = head
			}
		}

		// 压缩期间写进来的消息在这里补捞：压缩要调一次模型（pi/agent.go:256、
		// :285），慢的话是几秒，那段时间写入的不该等到下一轮。pi.dev 在
		// agent-loop.ts:203 单独留了一个"压缩完再捞一次"的点，理由相同。
		l.deliver(l.drainSteering(), state)
```

与轮次结束钩子的相对顺序不变：`afterTurn`（在上一轮的末尾）先跑，取走点一（这一轮的开头）后跑——与 pi.dev `:258` 先于 `:263` 一致。钩子返回错误就退出，队列里剩下的插话不再交付：那是对的，运行已经结束了。

`deliver` 是两处共用的注入动作：

```go
// deliver 把取走的消息注入这一轮已经产生的序列，并逐条交给观察者。
// 必须走 observe：会话落盘挂在消息观察者上（pi/agent.go:156），绕过它
// 注入的消息只在内存里——盘上的历史与当时发出的请求不一致，下一轮重建
// 就缺一段。
//
// 顺序是我们自己保证的，不是 session 查出来的：Append 在 ParentID 为空时
// 自动取当前叶子（pi/session/manager.go:143-148），观察者走的正是这条路
// （pi/agent.go:161 不带 ParentID），所以插在工具结果之前不会报 80002，
// 只会静默把顺序写错。取走点必须落在"这一轮的助手消息与工具结果都写回
// 之后"，测试直接断言消息序列（见「落盘」第 1 条）。
//
// 记账：取走点一交付的那些进 turn.extra()，参与这一轮的窗口判定；取走点二
// 交付的那些是在压缩判定之后才进的 Produced，要到下一轮才算进上下文大小。
// 压缩本身留有余量，这不影响判定；写在这里免得将来有人对着 token 数觉得
// 少了一条。
func (l *Loop) deliver(messages schema.Messages, state *runState) {
	for _, message := range messages {
		state.produced = append(state.produced, message)
		l.observe(message)
	}
}
```

## 落盘

三条约束，都在 `pi/session` 与循环之间，不需要新增代码，但都需要写清楚，否则将来有人把取走点挪个位置就炸。后面第 4 条不是约束，是这条通道顺带要修的一处既有缺口。

1. **顺序**（这三条里最容易误读的一处）。`session.Manager.Append` 只在调用方**显式带了** `ParentID` 时才校验它是不是当前叶子（`pi/session/manager.go:143-148`）；`ParentID` 为空时它自动取当前叶子。而消息观察者（`pi/agent.go:161`）不带 `ParentID`——所以插在工具结果之前**不会**报 80002，它会静默地把顺序写错。80002 拦的是"另一个写入者拿着过期的叶子来写"（`pi/session/manager.go:112-113` 的注释），不是"这条消息在语义上排错了位"。`entry.validate()`（`:152`）只看载荷有没有，也不管顺序。

   所以这条约束是我们自己的，不是 session 替我们查的：工具结果必须紧跟在发起它的助手消息之后（两家 provider 都按这个顺序还原上下文），而 `Turn.Messages()` 的排布是 Head → Tail → Produced（`pi/loop.go:49-55`）。取走点落在"工具结果写回之后、下一次模型调用之前"就同时满足这两条——测试直接断言请求里的消息序列，不拿 80002 当守卫，那条断言是空的。
2. **必须走观察者**。落盘挂在 `WithMessageObserver` 上（`pi/agent.go:156-162`），它逐条 append。绕过它直接在 `state.produced` 上加一条，内存里的请求与盘上的历史就分叉了。反过来，写入失败会被 `writeErr` 缓存（`pi/agent.go:74`），由 `Run` 透出——插话的落盘失败与模型消息的落盘失败走同一条路，不需要额外处理（"运行也失败"那种情况见第 4 条）。
3. **不属于历史段**。插话落在 `a.headLeafID`（`pi/agent.go:228` 取的本轮起点叶子）之后，所以它进 `Turn.Produced`、不进 `Turn.Head`。压缩只换 Head（`pi/agent.go:244-275`），边界不受影响；下一轮 Run 重建历史时它自然成为历史的一部分。

4. **两个错同时发生**（本方案顺带修的一处既有缺口）。`Run` 现在是这样两条分支（`pi/agent.go:203-210`）：运行出错就返回运行错，运行没错才看 `writeErr`。所以"运行中途写入失败、之后运行又失败"时只报运行错，盘上缺了一段这件事被吞掉。插话让这条路径更容易被走到（写入方在运行期间随时可能写），值得一并修：

   ```go
   	if err != nil {
   		return &RunOutput{message: messages}, errors.Join(err, a.writeErr)
   	}
   	if a.writeErr != nil {
   		return &RunOutput{message: messages}, a.writeErr
   	}
   ```

   `CodeOf` 走的是 `errors.As`（`pi/error/errors.go:186-193` 的 `AsType[coded]`），join 之后的遍历顺序是先运行错后写入错，而运行错总是带码的（`pi/loop.go` 里每条返回路径都经过 `pierrors`，provider 侧的失败也是，比如 `pi/ai/providers/anthropic.go:121`），所以拿到的仍是运行码——调用方按码分流的行为不变；`errors.Is` 能同时找到写入失败的原因。不额外定义优先级规则，`errors.Join` 已经是标准库对这个问题的答案。

`pi/session` 侧一行不改：插话就是一条 `session.EntryMessage`，与模型消息、工具结果同一种 entry。

## 入口

```go
// Steerer 是运行期间往循环里写消息的入口。它与 Runner 分开：能不能跑与
// 能不能插话是两件事，实现 Runner 的人不必被迫实现它。Run 是同步阻塞的，
// 所以要在运行期间写入只能从另一个 goroutine 调；Run 返回之后写入的消息
// 留给下一次 Run。
type Steerer interface {
	Steer(message schema.Message) error
}
```

```go
// Steer 往正在运行的循环里写一条消息。没有运行在跑时消息入队，等下一次
// Run 开始前交付。
func (a *Agent) Steer(message schema.Message) error {
	return a.loop.Steer(message)
}
```

`Agent.Steer` 只转发，校验全在 `Loop.Steer` 里——一处校验，`Loop` 的直接使用者拿到同一套规则。`Agent` 不查 `closed`：`Run` 的入口检查（`pi/agent.go:182`、`:185-190`）管的是"这次运行能不能开"，与写入一条待交付的消息无关；入队不碰任何资源，所以关掉的 `Agent` 也能入队。真正的交付发生在那次 `Run` 里，而那时 `Run` 会先查 `closed`。

## 错误码

不新增。`Steer` 的三个拒绝理由——typed nil、角色不是 user、消息自身不合法——都走既有的 `ErrRequestInvalid`（`pi/error/errors.go:69`，10002，"请求无效"）：三种都是"调用方写了不该写的东西"，不是运行状态的问题，分三个码只会让调用方多写三条分支去查同一件事。写入之后的失败（落盘失败）在 `Run` 的返回里透出，用的是既有路径，不需要新码。

## 测试

`pi/steering_test.go` 一个文件，与 `pi` 包内其他测试同包（要调 `drainSteering`，也要走 `pi/agent.go:106` 的 `newAgent` 测试缝）。provider 桩记下每一次请求的消息序列——后面几条用例的断言都吃这一份记录——而不是在测试里自己从会话重建，避免断言路径与 `deliver` 的实际落盘路径不一致。

1. **注入位置**（端到端，需要 provider 桩）
   - 桩按脚本吐两轮工具调用；第二次请求正在等模型返回的当口调 `Steer`
   - 交付落在下一轮的取走点：那时第二轮的工具结果已经写回、第三次请求还没发出
   - 断言第三次请求的消息序列里，插话紧跟在工具结果之后（`texts[5]` 是那条工具结果、`texts[6]` 是插话）

2. **落盘**
   - 用 `session.InMemory()` 跑一次，断言 `BuildMessages()` 里有这条插话
   - 顺序断言走消息序列本身（`[开始, 助手消息, 工具结果, 插话, 收到]`），**不要**断言"没有 80002"：观察者不带 `ParentID`，Append 会自动取当前叶子（`pi/session/manager.go:143-148`），那条断言永远成立，等于没测

3. **运行前写入**
   - `Run` 之前调 `Steer`（走 `Steerer` 接口），断言第一次请求里就有它（这条同时覆盖 turn 0：第一个模型调用之前也会取走）
   - 写完立刻改原件，请求里出现的仍是入队时的那份（决策 13 的请求级断言，与第 6 条互为表里）

4. **两处取走点的分工**（不需要真的压缩）
   - 取走点一：`pi.NewLoop` 配一个 `WithBeforeTurn` 钩子，钩子在压缩前就被交付过——钩子里读到的 `turn.extra()` 里应当已经有那条插话（证明它参与了这一轮的窗口判定）
   - 取走点二：同一个钩子在返回前再 `l.Steer` 写一条，断言它出现在**本次**请求里，而不是下一次
   - 两条用例合起来证明了两处的存在必要性：只有取走点一时第二条会晚一轮，只有取走点二时第一条不会进 `extra()`

5. **三个拒绝理由**（表驱动，不依赖桩）
   - `Steer(nil)` → 10002
   - `Steer((*schema.UserMessage)(nil))` → 10002（typed nil，这条用例是 `isNilMessage` 的存在理由）
   - `Steer(&schema.SystemMessage{...})` → 10002（角色不是 user，决策 8）
   - 一条自身不合法（`Validate` 会报错）的消息 → 10002
   - 四种情况都断言队列没被写进去（写失败不能留半条）

6. **入队之后改原件不影响已排队的那份**（决策 13）
   - `Steer(msg)` 之后改 `msg.Content` 里那个块的 `Text`，断言取走的那条还是改之前的内容
   - 图片同理：改 `Image.Data`，断言取走的还是原件
   - 这两条断在 `drainSteering` 上（不依赖桩，隔离得最干净）；走到请求里的那条由第 3 条覆盖

7. **并发**（`-race`）
   - 运行期间从十个 goroutine 各写一条，断言十条都在某一次请求里出现，一条不丢

8. **跨 Run**
   - 第一次 `Run` 结束之后写入，第二次 `Run` 的第一次请求里有它

9. **双错误**（`pi/agent.go` 的 `errors.Join` 改动）
   - 造错方式是删掉会话文件：`checkUnchanged` 摸不到文件即 `ErrSessionAppendFailed`，删文件不像改权限那样会因"以 root 跑测试"而失效
   - 运行错用循环检测那条现成的路：桩每轮重复同一个调用，撞到阈值即以 50001 收尾——写入错先发生、运行错后发生，正好构成"两个错同时存在"
   - 断言 `CodeOf` 是运行错的码，且 `errors.Is(err, pierrors.ErrSessionAppendFailed)` 为真

## 实施顺序

1. `pi/loop.go`：`steering` 字段、锁、`isNilMessage`、`Steer`、`drainSteering`、`deliver`，先不加取走点（行为不变，既有测试不受影响）。
2. `pi/loop.go`：加两处取走点（`beforeTurn` 之前、`beforeTurn` 之后 `l.complete` 之前）。这是本轮唯一会改变既有行为的地方，单独一步便于出问题时定位。
3. `pi/runner.go` 的 `Steerer` 与 `pi/agent.go` 的 `Agent.Steer`。
4. `pi/agent.go` 的 `Run`：双错误返回改 `errors.Join`。与插话本身无关，是这条通道让那条路径更容易被走到，顺带修。
5. 测试：先 5（不依赖桩），再 3、6，再 1、2、4、7、8，最后 9（要造双故障）。

## 待验证项（动手前先钉）

- 两处取走点的位置：一处在 maxTurns 判定之后、`l.beforeTurn` 之前；一处在 `beforeTurn` 块之后、`l.complete` 之前。确认 `Turn.Messages()` 的排布是 Head → Tail → Produced（`pi/loop.go:49-62`），这样插话才排在"本轮输入与系统提示词之后"。
- `observe` 在 `run` 里是同步调用（`pi/loop.go:137-142`），所以 `deliver` 里调它不会引入并发问题；确认 `Agent` 侧的观察者（`pi/agent.go:156-162`）也不需要自己的锁。
- `Agent.Steer` 与 `Agent.Run` 的调用关系：`a.loop` 在 `NewAgent` 里建好之后不再变（`pi/agent.go:150`），所以 `Steer` 不需要在 `Run` 期间重新取一次循环引用。
- 测试 4 的写法：`WithBeforeTurn` 的钩子在 run goroutine 上执行，它调 `l.Steer` 只碰 `steeringMu` 与切片（两次 `drainSteering` 分别在钩子之前与之后调，钩子内部不会自锁）。动手时先写这条最简的用例钉住两处的分工，而不是搭一套真的压缩。
- `message.Blocks()` 返回的是活切片还是副本（`pi/schema/message.go:41-53` 的注释把它定为"只读展示"用途）：不管是哪种，`ContentBlocks.Clone` 都是深拷贝，决策 13 的复制成立；但确认一下，免得将来有人以为 `Blocks()` 已经隔离了。
- 错误文案里的 `%q` 打在 `schema.Role` 上（`Role` 是 string 类型，`pi/schema/message.go:20`）：确认输出是 `"tool"` 这种可读形式，不是数字。
- `errors.As` 在 `errors.Join` 之后的遍历顺序：标准库语义是先左后右，运行错在左，所以 `CodeOf` 拿到运行码。动手时用测试 9 的一条断言把这条钉死——它依赖的是标准库行为，不是我们定的规则。

## 附：外部参照（pi.dev）

以下都在 `badlogic/pi-mono` 提交 `c7cdb46` 上核过：

| 事实 | 位置 |
|---|---|
| 类型定义：`getSteeringMessages` / `getFollowUpMessages` | `agent/src/types.ts:252`、`:265` |
| 队列与默认模式 `one-at-a-time` | `agent/src/agent.ts:191-192`、`:246` |
| 写入入口 | `agent/src/agent.ts:299`（steer）、`:304`（followUp） |
| 循环给的两个钩子 | `agent/src/agent.ts:490`、`:497` |
| 取走点一：运行开头 | `agent-loop.ts:174`，注释 "user may have typed while waiting" |
| 取走点二：`prepareNextTurn` 之后 | `agent-loop.ts:203`（理由是压缩可能很慢，压完再捞一次；本仓库的取走点二对应这一处） |
| 取走点三：每个 `turn_end` 之后 | `agent-loop.ts:263` |
| 交付时机（文档注释） | `agent-session.ts:1565-1566`（steering）、`:1574-1576`（followUp） |
| 队列清空 | `agent-session.ts:1754`（`clearQueue`） |
| 扩展侧的写入面 | `extensions/types.ts:1390` 的 `sendMessage(..., { deliverAs: "steer" \| "followUp" \| "nextTurn" })` |
| 同一个位置插系统消息 | `agent-loop.ts:209`（`declareToolChanges`） |

结论与取舍：pi.dev 的插话是"人对着运行中的 agent 说话"这个场景的产物，所以它有三个取走点、两种交付语义、一个可查可清的队列，以及三档 `deliverAs`。本方案只取承载"运行期间改方向"的最小一半：一个队列、一个语义（steer）、两处取走点（每轮调用模型之前，压缩前后各一次），没有查询与清空。偏离都写在决策表里，主要三条是决策 5（队列跨 Run 存活）、决策 7（一次全取）、决策 8（角色收窄到 user）。

## 附：这一轮之后自然会长出来的东西

- **检测器用插话改方向**：循环检测（`2026-09-24-loop-guard-design.md`）的 `AfterTurn` 挂在轮末，本方案的取走点在下一轮的开头——两者相邻（钩子返回错误就退出，返回值继续就走到取走点），所以"检测器判到循环 → 插一句话改方向"是钩子里调一次 `Steer` 的事。两件事拼起来才完整，但各自能独立落地。
- **followUp 语义**：队列加一个标记位区分两种交付时机，再加一个取走点（循环要停下来之前的那一处，对应 `agent-loop.ts:267`）。有了它，`Steerer` 会变成两个方法或一个带参数的 `Steer`。
- **队列的可见与清空**：`Pending() schema.Messages` 与 `ClearSteering()`。要它们的前提是有个 UI 会展示待交付的消息。
- **插话的去重与上限**：现在写多少条就交付多少条。如果出现"检测器每轮都插一句"的用法，需要一条"同一个签名只插一次"的规则。
- **插话的消息类型**：现在收 `schema.Message`。若将来要区分"普通用户消息"与"harness 注入的提示"，那是一条新的 `schema` 消息种类，会牵扯 provider 两家的映射，是独立的一块。
