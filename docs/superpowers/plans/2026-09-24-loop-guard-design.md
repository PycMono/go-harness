# 循环检测方案

## 一句话

`pi/loop.go` 加一条轮次结束钩子（`AfterTurn`），判定不进循环：`pi` 包根一个文件实现一条判据——**连续若干轮的工具调用与结果完全相同**就判循环，以稳定码 50001 终止这一次 Run，已产生的消息照常返回。钩子是机制（对应 pi.dev 的 `shouldStopAfterTurn`），判据是策略，两者分开。

## 决策一览（先看这个）

| # | 决策 | 选择 | 备选与代价 |
|---|---|---|---|
| 1 | 检测放在哪一层 | 循环只加一条钩子，判定作为钩子的消费者 | 把判定写进 `Loop.run`：策略进核心，换判据要改循环；完全照 pi.dev 交给宿主：`Agent` 缺省就还是只有 100 轮的哑上限，等于默认没有防护 |
| 2 | 钩子返回什么 | `error` | `bool`（pi.dev 的选择，`types.ts:230`）：调用方只拿到"停了"，拿不到"为什么停"；`Run` 已经是 `(RunOutput, error)` 的形状，白丢一个能带码的信息 |
| 3 | 判据口径 | 整轮的工具调用**与结果**，连续 N 轮完全一致（顺序也一致） | 只看调用（上一版的选择）：轮询类工具会被误停——连着三轮问同一个状态，第三次刚返回"完成"，运行就终止了，模型看不到这个结果（见决策 11）；单个调用在窗口内重复 N 次：漏掉「[改文件, 跑测试] × 3」这种整轮重复的形态；忽略调用顺序：调度器按调用在批内的原顺序分波执行（`pi/tools/scheduler.go:50-63`），顺序携带语义，[read a, write b] 与 [write b, read a] 的结果可能不同，排序会把它们算成同一轮——误报方向是杀掉一次健康的运行 |
| 4 | 计数方式 | 连续计数，两个字段 | 滑动窗口：能抓 A→B→A→B 振荡，多一个数据结构与一个可调参数，误报也更高 |
| 5 | 参数比较 | JSON 规范化后比较，数字按字面量（`UseNumber`） | 比原始字节：模型对同一次调用两次可能给出键序不同的 JSON，会把一次调用看成两次；解析进 `any`（默认 `float64`）：`1` 与 `1.0` 归一成同一个值，看似更宽容，代价是超过 2^53 的整数会掉精度、两个不同的数被算成同一个——宽容方向是误报（杀掉一次健康运行），比漏报更难查 |
| 6 | 阈值 | 默认 3 轮 | 2 轮：正常流程里「先读一遍结构、改完再读一遍」就命中了；5 轮：烧掉的钱已经不少 |
| 7 | 与 `maxTurns` 的关系 | 并存，各报各的码 | 用检测取代 `maxTurns`：抓不到 A→B→A→B 和「每轮都不同但都没进展」；把 `maxTurns` 收进检测器：`maxTurns` 是 `Loop` 自己的兜底（`pi/loop.go:16` 的默认值在 `NewLoop` 里就生效，`:87`），检测器是 `Agent` 装配时才挂上的（`pi/agent.go:150`）——合并等于把「自己拼 `Loop` + `Scheduler` 的人」唯一的调用次数上限拿掉，而且两件事的触发条件不同，合并只会让两者都说不清 |
| 8 | 缺省是否开启 | 开，阈值 3；`Options.LoopGuardTurns` 负数关闭 | 缺省关闭：得先知道有这个选项才会去打开，等于默认没有 |
| 9 | 工具批内声明终止（pi.dev 的 `terminate`） | 不做 | 见非目标 |
| 10 | 新包 | 不建，检测器一个文件进 `pi` 包根 | 建 `pi/loopdetect`：判据只有一条、状态只有两个字段，一个文件够了；等长出第二种判据再拆 |
| 11 | 判据的判据：只看调用还是也看结果 | 两者都看（工具名 + 参数 + 结果） | 只看调用（上一版的选择）：把"调用相同"当成"没进展"，可调用相同不等于没进展——轮询类工具（问任务状态、等构建结束、查队列深度）就是靠反复问同一个东西来推进的，第三次问出"完成"的那一刻被判成循环、运行被终止，模型永远看不到那个结果；只看结果不看调用：结果相同而调用不同本来就不像卡住。加上结果是**收紧**判据（能停的形态变少），方向上只会减少误报 |

## 目标与非目标

目标：

- 整轮的工具调用（名称、顺序、参数）与它们的**结果**连续若干轮完全一致时，这一次 Run 以 50001 终止，`RunOutput.Messages()` 里保留已经产生的消息，调用方能看到模型跑到哪一步。
- 反复问同一个问题的工具不被误停：调用相同、结果在变（比如从"运行中"变成"完成"）不算循环。
- 判定不依赖工具语义：不看工具是读还是写、不解析参数的字段含义，只做字符串比较。
- 判定结果可解释：错误文案里带连续轮数与签名，日志里不用再拼现场。

非目标（明确不做，附理由）：

- **工具批内声明终止（pi.dev 的 `terminate`）**。语义是"整批工具结果都声明了才提前结束"（`agent-loop.ts:644` 的 `every`），在 pi.dev 里的主用途是策略拦截（`block`）的搭配，不是循环检测。要做的话是 `pi/tools` 的 `Execution` 与 `Event` 各加字段、`pi/loop.go` 做整批判定，与本轮的判据无关，独立成一块。
- **无进展检测**（连续若干轮没有任何写操作成功）。要先定义"什么算写操作"，这得给 `pi/tools` 的工具加读/写分类，是另一块。它与"结果也进签名"（决策 11）不是同一件事：后者只要求结果别在变，前者要求判出"这一轮有没有推进"，得读懂工具语义。
- **结果不可比的工具**。结果里带时间戳、耗时、随机 id 时，同一个调用每次的结果都不相等，"结果也相同"这个条件永远不成立，判据退化成只看调用（也就是上一版的行为）。本仓库的内建工具不掺这些（`pi/tools/impl/bash.go:62-65` 返回的就是命令原始输出），MCP 工具由对方决定。真遇到时再给工具加"结果不可比"的声明，现在不加。
- **振荡检测**（A→B→A→B）。连续计数抓不到它。真出现了再加窗口，现在加只是提前付复杂度。
- **"同一批调用、只换了顺序"**。判据要求顺序也一致（决策 3），所以模型每轮把同一批工具换个顺序就绕过了。这是刻意的取舍方向：排序会把顺序不同算成同一轮，而顺序在调度器里有语义，误判的代价是掐掉一次健康运行；漏判的代价只是这次没抓住。绕过的形态是"每轮都在真干活只是顺序在变"，那本来也不像卡住。
- **插话**。检测到循环之后的另一条出路是往运行里插一句话让模型换方向，那是一条独立的入口通道，单独一份设计（`2026-09-24-steering-design.md`）。
- **可插拔的检测器**。`AfterTurn` 就是那个口子，但 `Agent` 只装内建的一个，`Options` 上不开「传自定义检测器」的字段——还没出现第二种策略。真出现了加一个字段即可，`AfterTurn` 已经导出，不用改接口。
- **跨 Run 的累计**。一个 Run 一轮账。上一个 Run 的签名不带进这一个（见「计数与重置」）。

## 归属：每个名字住在哪

| 名字 | 形态 | 位置 | 为什么在这 |
|---|---|---|---|
| `TurnReport` | 结构 | `pi/loop.go` | 循环的轮次现场，只有循环能填出来 |
| `AfterTurn` | 类型 | `pi/loop.go` | 轮次结束钩子；与 `BeforeTurn`（`loop.go:73`）对称 |
| `WithAfterTurn` | 函数 | `pi/loop.go` | 与 `WithBeforeTurn`（`loop.go:131`）对称 |
| `loopGuard` / `newLoopGuard` | 结构（包内）/ 构造函数 | `pi/loopguard.go` | 内建判据。不导出——判据是策略，形状不该承诺；构造与配置解释放一起，装配点只传 `Options` 上的那个值 |
| `signatureOf` / `canonicalArguments` / `resultFingerprint` / `clipSignature` | 函数（包内） | `pi/loopguard.go` | 纯函数，无状态，可单独表驱动 |
| `Options.LoopGuardTurns` | 字段 | `pi/agent.go` | 装配点的旋钮 |

对外面只有三个新名字：`TurnReport`、`AfterTurn`、`WithAfterTurn`，加一个 `Options` 字段。检测器本身不导出。

`TurnReport.ToolResults` 用 `schema.Messages` 而不是 `[]tools.Event`：判定要看的是"进上下文的那份东西"，pi.dev 的 `ShouldStopAfterTurnContext.toolResults`（`types.ts:134`）也是消息而不是事件。代价是丢掉了 `Event.ErrorCode`（`pi/tools/event.go:16`）——签名只需要内容块，`Message.Blocks()` 就够，用不到码；`schema.ToolResultMessage` 还有 `IsError`（`pi/schema/message.go:200`），将来做"重复失败"检测时够用。用 `[]tools.Event` 反而把 `pi/tools` 的事件形状钉进了 `pi` 包根的公开 API。

结果用 `Message.Blocks()` 读内容而不是断言成 `*schema.ToolResultMessage` 再读 `Content`：两者的值一样，但前者不需要断言，也就没有"断言失败怎么办"这个分支要编。代价是拿不到 `IsError`——签名的注释里写了为什么不带它。

## 文件清单（这轮动了哪些）

```
pi/
├── loop.go              改：TurnReport、AfterTurn、WithAfterTurn、run 里加调用点
├── loopguard.go         新增：签名计算、连续计数、错误文案
├── loopguard_test.go    新增：签名的表驱动用例、连续计数、端到端（配一个最小 provider 桩）
└── agent.go             改：Options.LoopGuardTurns、装配钩子、Run 开头重置
```

不新增文件夹，不动 `pi/tools`、`pi/session`、`pi/middleware`、`pi/error`（50001 已经在 `pi/error/errors.go:106`）。现存的 `pi/loopdetect/` 与 `pi/governor/` 这一轮也不碰，理由见「这一轮之后自然会长出来的东西」最后两条。

## 轮次边界：`AfterTurn`

现在 `pi/loop.go` 里唯一给上层的开口是 `:190` 的 `beforeTurn`，它在模型调用**之前**、只能替换请求的历史段。检测器要的是它的对面：这一轮的工具结果已经写回、下一轮还没开始。

```go
// TurnReport 是一轮的现场：这一轮的助手消息与它带出的工具结果。
// 只有带工具调用的轮才会产生它——模型不再要求调用工具时运行就该结束了，
// 没有"还要不要开下一轮"这个问题要问。
type TurnReport struct {
	// Index 是轮次序号，从 0 起，与 run 里 for 的计数一致。
	Index int
	// Message 是这一轮的助手消息。
	Message *schema.AssistantMessage
	// ToolResults 是这一轮的工具结果消息，下标与 Message.ToolCalls 对齐——
	// 判据按下标把结果配到调用上，所以这个对齐是签名正确性的一部分。
	ToolResults schema.Messages
}

// AfterTurn 在每轮的工具结果写回之后调用，是本包里唯一一个"轮次结束"的开口
// （对应 pi.dev 的 shouldStopAfterTurn，agent-loop.ts:258）。返回 nil 表示继续；
// 返回错误则收尾——循环把已产生的消息连同这个错误一起交回，不掐 provider 流、
// 不取消正在跑的工具，只是不再开下一轮。
type AfterTurn func(ctx context.Context, report TurnReport) error

// WithAfterTurn 给循环接一个轮次结束钩子。不设置时行为不变。
func WithAfterTurn(hook AfterTurn) LoopOption {
	return func(loop *Loop) { loop.afterTurn = hook }
}
```

调用点在 `pi/loop.go:220` 的 `ExecuteBatch` 与结果回填之后，`for turn` 的下一次迭代之前：

```go
		results, err := l.scheduler.ExecuteBatch(ctx, assistant.ToolCalls)
		if err != nil {
			return state.messages(), err
		}
		toolResults := make(schema.Messages, 0, len(results))
		for index := range results {
			result, err := results[index].ResultMessage()
			if err != nil {
				return state.messages(), pierrors.ErrInternal.Wrap(err)
			}
			state.produced = append(state.produced, result)
			l.observe(result)
			toolResults = append(toolResults, result)
		}

		// 每轮的工具结果写回之后给上层一次机会判定这是不是循环。判定放在这里
		// 而不是每轮开头：只有"还要开下一轮"的那些轮才有这个问题，模型不再要
		// 工具时上面已经返回了。返回错误就是收尾，与 :184 的 maxTurns 同一种
		// 形状——带消息返回，不中断正在跑的东西。
		if l.afterTurn != nil {
			if err = l.afterTurn(ctx, TurnReport{
				Index:       turn,
				Message:     assistant,
				ToolResults: toolResults,
			}); err != nil {
				return state.messages(), err
			}
		}
```

与 pi.dev 的一处差异要写清楚：pi.dev 的 `shouldStopAfterTurn` 在 `agent-loop.ts:258`，位置在 `if (toolCalls.length > 0)` 那个块**之外**，所以模型每一轮都会被问一次，包括不带工具调用的那一轮。我们只在带工具调用的轮问。理由是钩子要回答的问题是"还要不要开下一轮"，模型不要工具时这个问题不存在——上面 `:212` 已经 `return state.messages(), nil` 了。少一次调用，语义更窄也更好推理。

`beforeTurn` 返回非 nil 时替换 Head（`pi/loop.go:196`），`afterTurn` 返回错误时终止整次运行。两个钩子名字对称、能力不对称，这是刻意的：轮次开始处能改的是请求，轮次结束处能改的只有"要不要继续"。

## 判据：一轮的签名

签名函数是这一轮判据的全部，三条要求：顺序照抄（顺序携带语义，见决策 3）、参数的写法无关、结果也要进来（决策 11）。

```go
// signatureOf 是一轮工具调用与结果的指纹：批内每个调用按原顺序取「工具名 +
// 规范化参数 + 结果」，再用换行拼起来。不做排序——调度器按调用在批内的原顺序
// 分波执行（scheduler.go:50-63），[read a, write b] 与 [write b, read a] 的
// 结果可能不同，排了序会把它们算成同一轮。
//
// 分隔符用换行：结果文本里本来就有换行，所以严格说两条不同的记录理论上能拼出
// 同一个串（要结果里写出一段像下一条调用前缀的文字，还得连着三轮一样）。这
// 不是安全边界，撞了的后果是多停一次运行，不为它换一个更怪的字符。
func signatureOf(calls schema.ToolCalls, results schema.Messages) string {
	parts := make([]string, 0, len(calls))
	for index, call := range calls {
		part := call.Name + ":" + canonicalArguments(call.Arguments)
		// 结果按下标对齐（ExecuteBatch 的返回与 calls 一一对齐）。长度不一致时
		// 退化成只看调用：判据弱一点，比越界 panic 好——钩子在运行路径上，
		// panic 会把整次运行连同已产生的消息一起丢掉。
		if index < len(results) {
			part += "=>" + resultFingerprint(results[index])
		}
		parts = append(parts, part)
	}

	return strings.Join(parts, "\n")
}

// resultFingerprint 把一条工具结果压成可比的一行：各内容块的文本。带上结果比
// 不带严——调用相同、结果也相同才叫"这一轮什么也没变"，于是轮询类工具（连着
// 追问同一个状态，第三次返回"完成"）不会被误停。那是误报方向，本方案一路都在
// 躲它。
//
// 图片块取「媒体类型 + 载荷长度」，不取内容摘要：同类型、同长度的两张图会撞成
// 同一条结果（又是误报方向，但概率低得多）。真出现图片类工具再加 sha256，现在
// 不为它引一个 crypto。
//
// 不单独带 IsError：内容一样就是没进展，成败那一栏由内容体现，多一栏不改变
// 判定，反而让"同一段报错、一次算失败一次算成功"变成两轮。
func resultFingerprint(message schema.Message) string {
	blocks := message.Blocks()
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if block.Image != nil {
			parts = append(parts, fmt.Sprintf("[image %s %d]",
				block.Image.MIMEType, len(block.Image.Data)))
			continue
		}
		parts = append(parts, block.Text)
	}

	return strings.Join(parts, "\n")
}

// canonicalArguments 把参数规范化成可比的字符串。模型对同一次调用两次可能
// 给出键序不同的 JSON（对象成员顺序在协议上没有意义），直接比原始字节会把
// 一次调用看成两次。解析再编回去就归了序——encoding/json 编 map 时按键排序，
// 这是标准库保证的行为。
//
// 解析用 UseNumber：默认解析进 float64 会把 1 与 1.0 归一（宽容），但同时
// 让超过 2^53 的整数掉精度，两个不同的数会被算成同一个。宽容的方向是误报，
// 误报会掐掉一次健康运行，比漏报更难查。UseNumber 保留字面量，代价是 1 与
// 1.0 算两个签名：那是漏报方向（同一件事因为数字写法不同被当成两轮），但模型
// 不会无缘无故在两轮之间改数字写法，撞上的概率远低于精度塌陷。
//
// 解析不了、或后面还跟着别的 token（模型给过截断或拼接错位的 JSON）时原样
// 返回：它是模型给的字符串，比不出错，也不该在这里报错。
func canonicalArguments(arguments json.RawMessage) string {
	decoder := json.NewDecoder(bytes.NewReader(arguments))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return string(arguments)
	}
	// 合法的 JSON 值之后除了 EOF 不该再有内容。有的话说明这串东西不是我们
	// 以为的一整个值，别编回去——编回去会丢掉后面那段，让两个不同的参数
	// 撞成同一个签名。
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return string(arguments)
	}
	encoded, err := json.Marshal(decoded)
	if err != nil {
		return string(arguments)
	}

	return string(encoded)
}
```

签名会进错误文案，所以要能截断：

```go
// clipSignature 把签名截到能进错误文案的长度，按 UTF-8 边界切，免得日志里
// 出现半个汉字。只用在错误文案上，不参与比较——比较用的是完整签名。
// 签名通常由 json.Marshal 产出、是合法 UTF-8；走 canonicalArguments 原样
// 返回那条路时可能不是（那是模型给的原始字节）。所以这里按 ValidString 往前
// 退到边界，最坏退到 0、文案只剩一个省略号——可接受的降级，不为它加分支。
func clipSignature(signature string) string {
	const max = 240
	if len(signature) <= max {
		return signature
	}
	cut := max
	for cut > 0 && !utf8.ValidString(signature[:cut]) {
		cut--
	}

	return signature[:cut] + "…"
}
```

## 计数与重置

```go
// loopGuard 是内建的循环检测：连续若干轮的工具调用与结果都完全相同就判循环。
// 判据只有一条，因为它只需要一个状态：上一轮的签名和它连续出现了几次。
// "连续"而不是"窗口内累计"——连续是更强的信号、误报更低，也让状态退化
// 成两个字段；代价是抓不到 A→B→A→B 这种振荡，真出现了再加窗口。
type loopGuard struct {
	limit   int
	last    string
	repeats int
}

// observe 记下这一轮的签名并判定。返回 nil 表示这一轮不是循环。
// limit <= 0 表示关闭，直接返回——装配不做条件判断，关不关由值决定。
func (g *loopGuard) observe(report TurnReport) error {
	if g.limit <= 0 {
		return nil
	}

	signature := signatureOf(report.Message.ToolCalls, report.ToolResults)
	if signature == g.last {
		g.repeats++
	} else {
		g.last, g.repeats = signature, 1
	}
	if g.repeats < g.limit {
		return nil
	}

	return pierrors.ErrRunLoopDetected.Wrap(fmt.Errorf(
		"连续 %d 轮的工具调用与结果完全相同：%s", g.repeats, clipSignature(signature)))
}

// reset 清空计数。一个 Run 一轮账：跨 Run 留着的话，两次毫不相干的运行
// 各调一次同一个工具就会被算成重复。与 a.writeErr、a.headLeafID 同处重置。
func (g *loopGuard) reset() { g.last, g.repeats = "", 0 }
```

`defaultLoopGuardTurns = 3` 取 3 的理由：读同一个文件两遍（`repeats` 到 2）在正常流程里常见——先看结构、改完再确认；三遍还一模一样，基本可以确定这一轮没有产生任何变化。阈值可调而不是可关：`Options.LoopGuardTurns` 是 `int`，0 取默认、负数关闭，与 `MaxTurns` / `MaxParallel` 的「`<= 0` 取默认值」不同——关闭需要一个表达方式，用 0 表示关闭就得另给默认值找一个表达方式，负数比哨兵值清楚。

## 与 `maxTurns` 的关系

并存。

- `pi/loop.go:16` 的 `defaultMaxTurns = 100` 抓的是**总量**：不管每轮在干什么，模型调用次数到 100 就停，报 50000（`pi/error/errors.go:105`）。它拦得住"一直在推进但战线过长"，也拦得住"每轮都不同但都没进展"。
- 检测器抓的是**形态**：同一个调用连续三轮不变就停，报 50001（`pi/error/errors.go:106`）。它拦得住"烧得不多但完全没动"，那种情况离 100 轮还远，`maxTurns` 看不见。
- 两个码分开，调用方按 `CodeOf` 分流：50000 是"这次活的量超了，可以调大上限重试或换个做法"，50001 是"这个做法本身卡住了，重试没用"。

不改 `maxTurns` 的默认值，也不动它的判定位置。检测器先命中就报 50001，`maxTurns` 先命中就报 50000，两者不互相压制。

不把 `maxTurns` 收进检测器还有一层结构上的原因：它俩的**生命周期不在同一个层级**。`defaultMaxTurns = 100` 在 `NewLoop` 里就生效（`pi/loop.go:16`、`:87`），任何自己拼 `Loop` + `Scheduler` 的调用方都白得一个上限；检测器是 `Agent` 装配时才挂上去的（`pi/agent.go:150`），`Agent` 之外的用法拿不到。合并等于让"不用 `Agent`、直接拿 `Loop`"的人失去唯一的调用次数上限——那是把兜底从一个必然存在的地方挪到一个可能不存在的地方。

## 装配

`pi/agent.go` 的 `Options` 加一个字段，`newAgent` 里装配，`Run` 里重置：

```go
	// LoopGuardTurns 是循环检测的阈值：连续这么多轮的工具调用完全相同就
	// 终止运行。0 取默认值（3），负数关闭——关闭之后只剩 MaxTurns 兜底。
	LoopGuardTurns int
```

```go
	guard := newLoopGuard(opts.LoopGuardTurns)
	agent := &Agent{
		...
		guard: guard,
	}
	agent.loop = NewLoop(
		provider,
		WithScheduler(...),
		WithTextObserver(opts.TextObserver),
		WithMaxTurns(opts.MaxTurns),
		WithBeforeTurn(agent.compactBeforeTurn),
		WithAfterTurn(func(_ context.Context, report TurnReport) error {
			return guard.observe(report)
		}),
		WithMessageObserver(...),
	)
```

```go
	// 一个 Run 一轮账：上一轮的写入错误、本轮起点与循环检测的计数都不带进
	// 这一轮。检测计数必须在这里清——它记的是"连续几轮"，跨 Run 留着就会把
	// 两次不相干的运行接成一段。
	a.writeErr = nil
	a.headLeafID = ""
	a.guard.reset()
```

装配不做条件分支：`limit <= 0` 的关闭语义由 `observe` 自己吞掉。少一个"装不装钩子"的判断，`Loop.run` 里那个 `l.afterTurn != nil` 也就永远是真——不影响正确性，钩子本身有值语义的开关。

## 错误码

不新增码。`pi/error/errors.go:106` 的 `ErrRunLoopDetected`（50001，"检测到循环调用"）已经在了，直接 `Wrap` 使用。`pi/error/README.md:6` 写过领域错误类型随领域包定义、不集中到 `pi/error` 的规矩——那说的是"要跨包被 `errors.As` 判定的领域类型"（比如设想中的 `loopdetect.Error`）。本轮的判定结果是纯错误值包装，没有额外的结构化字段要带（轮数与签名都在文案里），所以不进 `pi/error` 之外的包、也不定义新类型。哪天真要带结构化证据（供上层按签名做统计），再按那条规矩在领域包里加类型并实现 `Code()`。

## 测试

`pi/loopguard_test.go` 一个文件三组：

1. **签名的表驱动**（纯函数，无需 provider）
   - 同一个调用的参数键序不同 → 同一签名。`{"a":1,"b":2}` 与 `{"b":2,"a":1}`
   - 一轮里调用顺序不同 → **不同**签名（顺序照抄，决策 3）
   - 参数不同 → 不同签名（哪怕只是一个字段的值）
   - 调用相同、结果不同 → **不同**签名（这条就是轮询场景，决策 11 的存在理由）
   - 参数不是合法 JSON（模型给过截断的 JSON）→ 原样返回，不报错
   - JSON 值后面跟着多余内容（`{"a":1}{"b":2}`、`{"a":1} x`）→ 原样返回，不与任何规范化结果撞
   - 超过 2^53 的两个相邻整数（`9007199254740993` 与 `9007199254740992`）→ 不同签名（`UseNumber` 保住了精度，默认 `float64` 会塌成一个）
   - 数字写法 `1` 与 `1.0` → 不同签名（保留字面量；这条是取舍的代价，不是缺陷，写进用例免得将来被人当 bug 改掉）
   - 结果条数比调用条数少 → 不 panic，退化成只看调用（兜底分支要有一条用例，否则它是死代码）
   - 带图片的结果：同类型同长度 → 同一签名（已知的粗，用例名字写清楚，免得将来被当成 bug）

2. **连续计数**
   - 同一签名连续 3 轮 → 第 3 轮返回错误，`pierrors.CodeOf` 为 50001
   - 中途换签名 → 计数回到 1，不报错
   - 3、4、5 轮 → 每轮都报（不是只报一次就哑）
   - `limit <= 0` → 永不报错
   - `reset` 之后同一签名 → 重新从 1 开始（跨 Run 不串）

3. **端到端**（走 `pi/agent.go:106` 的 `newAgent(ctx, provider, opts)` 缝——那句注释写的就是"NewAgent 与测试共用这条装配路径"）
   - 一个最小 provider 桩，按脚本吐出 N 条带同一工具调用的助手消息，工具结果也固定
   - 断言 `Run` 返回的错误 `CodeOf` 是 50001，且 `RunOutput.Messages()` 非空（已产生的消息没丢）
   - 再加一条反向用例：同样的三轮调用，但每次结果不同（模拟轮询）→ **不**报错，运行正常结束。这条比上面那条重要，它守的是"不误停"
   - 桩需要实现 `ai.Provider`（`pi/ai/provider.go:13`，一个方法）与 `ai.Stream`（`provider.go:18`，四个方法），约 40 行

## 实施顺序

1. `pi/loop.go`：加 `TurnReport`、`AfterTurn`、`WithAfterTurn` 与调用点，先不接任何消费者（行为不变，既有测试不受影响）。
2. `pi/loopguard.go` 与签名的表驱动用例。纯函数先可验，不牵扯 provider。
3. `pi/agent.go`：加 `Options.LoopGuardTurns`、装配钩子、`Run` 开头重置。
4. 端到端用例（provider 桩 + 连续计数 + 反向的轮询用例）。

## 待验证项（动手前先钉）

- `TurnReport.ToolResults` 的下标承诺靠 `Scheduler.ExecuteBatch` 的"返回与 calls 下标一一对齐"（`pi/tools/scheduler.go:45-51`）。动手前读一遍 `executeWave`（`scheduler.go:123`）确认并发写回也是按下标，而不是按完成顺序。这一条本轮比上一版更要紧：签名按下标取结果，错位会把 B 的结果配到 A 的调用上。
- `pi/loop.go:220` 拿到的 `results` 在部分失败时只部分有效（`scheduler.go:49`）；确认这种情况下钩子不会被调用（`ExecuteBatch` 返回 error 时上面已经 `return`，所以不会）。
- `Options.LoopGuardTurns` 的名字与既有配置字段（`MaxTurns`、`MaxParallel`）的命名风格对齐。
- `Run` 里 `a.guard.reset()` 的位置在 `prepareRunContext`（`pi/agent.go:195`）之前——重置必须在任何工具调用可能发生之前。
- `canonicalArguments` 现在多三个 import（`bytes`、`io`、`errors`），动手时确认 `pi/loopguard.go` 里没有和既有名字（尤其是 `errors`）撞车。
- `json.Decoder.Token()` 读干净之后的返回：合法 JSON 值之后应当拿到 `io.EOF`，多余 token 时不该是 `io.EOF`。动手时按标准库语义先写一条小用例钉住这个前提，再写"多余内容原样返回"那条用例——后者依赖前者。

## 附：外部参照（pi.dev）

以下都在 `badlogic/pi-mono` 提交 `c7cdb46` 上核过：

| 事实 | 位置 |
|---|---|
| 主循环 `while (true)`，无迭代上限 | `packages/agent/src/agent-loop.ts:177` |
| 退出条件：stopReason 为 error/aborted | `agent-loop.ts:221` |
| 退出条件：这一轮没有工具调用 | `agent-loop.ts:231` |
| 退出条件：宿主钩子 `shouldStopAfterTurn` 返回 true | `agent-loop.ts:258` |
| 退出条件：没有 follow-up 消息 | `agent-loop.ts:268` |
| `shouldStopAfterTurn` 的类型与文档 | `agent/src/types.ts:230`、`agent/src/agent.ts:123`/`:462`、`agent/README.md:148` |
| 钩子拿到的现场（四字段） | `agent/src/types.ts:130-140` |
| 工具声明终止（整批才生效） | `extensions/types.ts:1137`、`agent-loop.ts:644` |
| 没有循环检测、没有迭代上限 | 全仓 grep `loop.?detect\|stuck\|oscillat` 与 `maxTurns\|maxIterations\|maxSteps` 均无命中 |
| 提示词里也没有"别重复"的指令 | `coding-agent/src/core/system-prompt.ts`（216 行）、`prompt-templates.ts`（285 行）零命中 |

结论与取舍：pi.dev 把"什么叫跑偏"整个判给宿主，自己只留机制。本方案照搬它的机制（轮次结束钩子），但在两处偏离，都写在上面的决策表里：

- 钩子返回 `error` 而不是 `bool`（决策 2）。pi.dev 返回 `true` 时循环正常结束、不发任何信号（`agent-loop.ts:259-260`）；本仓库 `Run` 已经是错误码分流的形状，白丢一个信息不划算。
- 缺省装一个判据（决策 8）。pi.dev 缺省什么都不装，因为它的主场景是交互式 TUI，卡住了有人按 Esc；本仓库的 `cmd/harness` 是无人值守的驱动器，缺省没有防护等于没有。

## 附：这一轮之后自然会长出来的东西

- **插话**（单独一份设计，`2026-09-24-steering-design.md`）：检测到循环之后不终止，而是插一句话让模型换方向。本方案的 `AfterTurn` 是它的天然挂点——钩子在轮末，插话的取走点在下一轮的开头调用模型之前，两者紧邻：钩子里调一次 `Steer`，那句话就在下一轮模型调用里出现。两条通道互不依赖，各自能单独落地。
- **振荡检测**：连续计数抓不到 A→B→A→B。加一个窗口（最近 N 轮的签名集合 + 出现次数）即可，`signatureOf` 不用改。
- **无进展检测**：要先给 `pi/tools` 的工具加读/写分类，之后判据变成"连续 N 轮没有任何写类调用成功"。`TurnReport.ToolResults` 里的 `IsError` 已经够用（`pi/schema/message.go:200`）。
- **`terminate`**：pi.dev 的批内终止（`agent-loop.ts:644`）。有了轮次钩子之后它不是必需品，但它是"工具自己喊停"的语义，与"宿主判断后喊停"不同，将来做工具权限或预算时会想要。
- **`maxTurns` 的分类化**：50000 现在只说"运行预算超限"，不说超的是哪个预算。等预算种类多起来（轮次、token、墙钟时长）再谈，那是一件需要新结构的事，不是加一个码。
- **预算挂在哪**：将来真要做预算（token 上限、墙钟时长），它和循环检测是**同一类东西**——都是"宿主对这一次运行的策略"，都该挂在轮次边界上。`AfterTurn` 就是那个挂点，装配处会是这个形状：

  ```go
  WithAfterTurn(func(ctx context.Context, report TurnReport) error {
  	if err := guard.observe(report); err != nil {
  		return err
  	}
  	return budget.check(report) // 将来有预算时
  }),
  ```

  这是"钩子只有一个、消费者可以多个"的理由之一；也是不把两种策略合并进一个类型的原因：判据不同、报的码不同、将来还要各长各的状态。
- **`pi/loopdetect/` 与 `pi/governor/`**（现存两个目录）：本轮不动它们。`loopdetect` 里设想的是一条独立判据 + 一个可被 `errors.As` 判定的领域错误类型，`governor` 里设想的是预算治理；本方案只做循环检测这一条判据，形状是一个包内类型加一个错误值包装（见「错误码」），不做跨包可判定的领域类型。等第二种判据（振荡、无进展）或预算真出现时，再回过头决定这两个目录装什么——那时的形状由当时的第二个消费者决定，现在定只会定错。这两个目录目前是空壳（只有 README），删留都不影响本轮实现。
