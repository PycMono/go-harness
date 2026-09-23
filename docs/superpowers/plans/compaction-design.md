# 压缩（Phase 2）设计方案

参考 pi.dev（`packages/coding-agent/src/core/compaction/compaction.ts` 1024 行、`session-manager.ts:442-481` 的读侧折叠），行号见文末"与 pi.dev 的对照"。

## 一句话

往会话文件里追加一条 `compaction` entry，它带一段摘要和一个 `first_kept_entry_id` 指针；`BuildMessages` 在追根之后按这条边界折叠——指针之前的历史换成摘要那一条消息，指针起的消息原样保留。文件里一条都不删。

## 目标与非目标

目标：

- 上下文接近窗口时自动压缩，长会话不再撞窗口；
- 压缩结果落盘，跨进程有效（下次 `OpenOrCreate` 打开就是折叠后的历史）；
- 算法是纯函数，能拿 entry 快照离线单测。

非目标（这轮明确不做）：

- **不拆轮次**。pi.dev 在一刀切在轮次中间时会额外生成一份"轮次前缀摘要"再合并（`compaction.ts:895-940`）。我们不做的理由是可证的：保留段以切点开头，而切点不可能是工具结果（`isCutPoint`），**紧随助手工具调用之后的消息必然是它自己的工具结果**（`pi/loop.go:136,162` 连续写入保证），所以"切点前一条是带工具调用的助手消息"蕴含"切点是工具结果"，与切点规则矛盾。保留段因此永远不会以一条无主的工具结果开头，摘要段也不会以一条等待工具结果的助手消息结尾。
  这条只证了消息序列合法，不等于与 pi.dev 等价：那边的轮次前缀摘要还用来留住"被切掉的那一轮里用户提了什么、已经做到哪一步"。我们不做，代价是——切点落在轮次中间时，那一轮的用户请求与前半程进展只留在摘要的压缩文本里，而同一轮后续的助手回复原样保留，轮次内部两侧的细节密度不对称。这个代价只在切点落在轮次中间时出现（触发条件：单轮就超过 `KeepRecent`，十几条大工具结果就够），切在轮次边界上（绝大多数情况）没有它。
- **不跟踪文件清单**。pi.dev 会从工具调用里抽 `<read-files>` / `<modified-files>`（`compaction.ts:55-92`、`utils.ts:62-84`）。它要求认识 `read` / `write` / `edit` 这些工具名与参数结构，等于把工具域的知识渗进 `pi/session`。真要做，落点应该在 `pi` 层的编排里（那里本来就知道工具），而不是这里。
- 不做扩展钩子（`session_before_compact` 等），不做摘要模型可配置，不做分叉摘要。

## 归属：每个名字住在哪

压缩涉及的所有值都挂在自己的数据上，对外没有游离的函数：

| 名字 | 接收者 / 归属 | 文件 | 职责 |
|---|---|---|---|
| `Window` / `NewWindow` | 值类型 | `pi/session/compaction.go` | 模型窗口与由它算出的两个预算，以及"该不该压"的判定 |
| `Window.Enabled` | `Window` | 同上 | 窗口没配、太小、或放不下摘要请求的开销 → 不压 |
| `Window.OverLine` | `Window` | 同上 | 拿一个算好的估算值跟触发线比 |
| `Plan` | 值类型 | 同上 | 一次压缩的计划：保留区从哪起、压哪些、压前多大 |
| `Plan.Entry` | `Plan` | 同上 | 用计划 + 一段摘要拼出要落盘的压缩边界 |
| `Plan.Request` | `Plan` | `pi/session/summary.go` | 拼装发给模型的摘要请求（含容量降级） |
| `Plan.transcript` | `Plan`（包内） | 同上 | 把要摘要的消息序列化成纯文本 |
| `Entries` 上的方法 | `Entries` | `pi/session/compaction.go` | 折叠、取区间、找切点、做计划 |
| `Entry` 的形状谓词（`isCutPoint` / `isToolMessage`） | `Entry`（包内） | `pi/session/entry.go` | 问的是"这条 entry 长什么样"，与字段同处一文件；压缩侧只调用它们 |
| `Compaction.summaryMessage` | `Compaction`（包内） | 同上 | 摘要投影成一条 user 消息 |
| `Manager.BuildMessages` / `Manager.MessagesAt` / `Manager.PlanCompaction` / `Manager.Compact` | `Manager` | `pi/session/manager.go` | 对外入口；`Compact` 是唯一写边界的地方 |
| `Manager.ContextTokens` | `Manager` | 同上 | 估算"折叠到本轮起点为止的历史 + 本轮的 Tail 与 Produced"。估算落在这儿而不是 `Window` 上：只有会话知道边界在哪，而边界决定哪些 usage 作废 |
| `Turn` / `Turn.Messages` | 值类型 | `pi/loop.go` | 一次模型调用前的三段请求 |
| `Agent.compactBeforeTurn` 等三个 | `Agent` | `pi/agent.go` | 编排 |
| 估算、截断、行格式 | 包内函数 | `pi/session/compaction.go` / `summary.go` | 接收者是 `schema.Messages` 这类外部类型，Go 不允许给外部类型加方法，只能是函数；不导出 |

两个例外说明一下：`NewWindow` 是构造函数，Go 里只能是包级函数；估算函数见上表的理由。其余对外只有方法。

## 数据模型（`pi/session/constant.go`、`entry.go`）

新增第三种 entry 类型。载荷只有三个字段，每个都必需：

```go
// constant.go
const (
	EntryHeader     EntryType = "session"     // 首行，只出现一次
	EntryMessage    EntryType = "message"     // 对话消息（含工具调用与结果）
	EntryCompaction EntryType = "compaction"  // 压缩边界，摘要替代它之前的历史
)
```

```go
// entry.go
// Compaction 是压缩边界 entry 的载荷。
type Compaction struct {
	// Summary 是这段历史的摘要正文，模型生成的七段文本。它替代
	// FirstKeptEntryID 之前的全部对话。
	Summary string `json:"summary"`
	// FirstKeptEntryID 是保留区第一条 entry 的 id。读侧只认这一个指针：
	// 它之前的消息不再进上下文，它起的（直到边界）原样保留。
	// 写入时由 Manager.Compact 校验它在本轮叶子的路径上、且不晚于边界自己。
	FirstKeptEntryID string `json:"first_kept_entry_id"`
	// TokensBefore 是压缩前整个上下文的估算 token 数。纯记录，给日志与
	// 审计看"这次压掉了多少"，不进模型。
	TokensBefore int64 `json:"tokens_before"`
}
```

`Entry` 加一个载荷字段（与 `Header` / `Message` 平级）：

```go
	// Compaction 压缩边界载荷，只有 Type 为 compaction 时非空。
	Compaction *Compaction `json:"compaction,omitempty"`
```

`Entry.validate` 加一个分支。它只看得到这一条 entry，所以只校验"字段在不在、摘要是不是空"；指针指不指得着属于路径级的事，由 `Manager.Compact` 在锁内校验（见"编排"一节）。理由码 **80014**（80010–80013 已各有其主）：

```go
	case EntryCompaction:
		if entry.Compaction == nil {
			return pierrors.ErrSessionCompactionPayloadMissing
		}
		// 摘要是这条 entry 的全部意义，空摘要等于凭空删掉一段历史。
		if strings.TrimSpace(entry.Compaction.Summary) == "" {
			return pierrors.ErrSessionCompactionPayloadMissing.Wrap(
				errors.New("compaction 摘要不能为空"))
		}
		if entry.Compaction.FirstKeptEntryID == "" {
			return pierrors.ErrSessionCompactionPayloadMissing.Wrap(
				errors.New("compaction first_kept_entry_id 不能为空"))
		}
```

```go
// pi/error/errors.go
ErrSessionCompactionPayloadMissing = New(80014, "compaction entry 缺少载荷")
ErrSessionCompactionPointerStale  = New(80015, "compaction 边界指针不在当前路径上")
ErrSessionCompactionVersionUnsupported = New(80016, "会话文件版本过旧，不支持压缩边界")
ErrCompactionFailed               = New(50002, "上下文压缩失败")
```

## 读侧折叠

`BuildMessages` 从"追根 + 取消息"变成"追根 + 折叠 + 取消息"：

```go
// manager.go
func (m *Manager) BuildMessages() schema.Messages {
	m.mu.Lock()
	defer m.mu.Unlock()

	path := m.entries.pathToRoot(m.leafID)
	messages, _ := path.foldedContext(len(path))

	return messages
}

// MessagesAt 取"折叠到 upto 为止"的历史段（upto 是本轮开始时的叶子 id）。压缩
// 当场换历史时用它，而不是自己拿计划里的字段拼：折叠规则（认哪条边界、摘要怎么
// 投影、哪些消息算数）只此一份，历史段因此不可能与读侧不一致。upto 指不着时返回
// nil——调用方据此不改写历史，而不是拿一份可能重复的历史去顶替。
func (m *Manager) MessagesAt(upto string) schema.Messages {
	m.mu.Lock()
	defer m.mu.Unlock()

	path := m.entries.pathToRoot(m.leafID)
	index := path.indexOf(upto)
	if index < 0 {
		return nil
	}
	messages, _ := path.foldedContext(index + 1)

	return messages
}

// ContextTokens 估算这次请求有多大：折叠到 upto 为止的历史，加上 extra。
//
// 上界必须是 upto（本轮开始时的叶子），不能是文件尾。本轮输入与本轮产生的消息在
// 盘上位于 upto 之后（只能往后追加，本轮开始时它们都还不存在），折到文件尾会把
// 同一段消息既算进历史段、又算进 extra，数两遍——而 extra 正是为了补上它们才存的。
//
// extra 是本轮还没进历史的那些（本轮输入、系统提示词、本轮已产生的消息）：在列表
// 里位于折叠段之后，所以它们的 usage 一律按新鲜算。正常路径上这条成立——压缩只
// 发生在某一轮的开头，紧接着这一轮就产出带 usage 的回复，下一次判定拿到的最新
// usage 必然写在最新边界之后。
//
// 唯一的例外是"边界之后一条 usage 都没有"（那一轮的回复没带用量）：扫描会退到
// 压缩前那条账上，估算偏大。代价有界——触发一次判定、多摘一次已经摘过的保留段，
// 落盘的边界仍然正确（`TokensBefore` 只记录、不进模型）。要堵住它得让调用方记下
// "压缩当场已产出的条数"、作为 extra 的第二个新鲜下标，这轮不做：多一个字段和一个
// 参数，换的是这个少见分支里省一次摘要调用。
func (m *Manager) ContextTokens(upto string, extra schema.Messages) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()

	path := m.entries.pathToRoot(m.leafID)
	index := path.indexOf(upto)
	if index < 0 {
		// 上界指不着（本轮起点不在当前路径上）：宁可按整条路径高估。触发只是多
		// 判一次，而 PlanCompaction 同样指不着 upto、不会落盘，估错不落地。
		index = len(path) - 1
	}
	messages, freshFrom := path.foldedContext(index + 1)
	if len(extra) > 0 {
		messages = append(messages[:len(messages):len(messages)], extra...)
	}

	return estimateContextTokens(messages, freshFrom)
}

// PlanCompaction 对当前路径做一次压缩计划。upto 是本轮开始时的叶子 id：
// 本轮输入与本轮产生的消息不在压缩范围内，必须原样留在请求里。upto 为空或
// 指不着时返回 nil 与 false。
func (m *Manager) PlanCompaction(window Window, upto string) (*Plan, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.entries.pathToRoot(m.leafID).plan(window, upto)
}
```

折叠本身在 `compaction.go`：

```go
// foldedContext 把一条路径折叠成模型上下文：路径上最后一条可用的压缩边界之后
// 的消息原样保留，它保留区起点到它之间的消息也保留，更早的由摘要那一条替代。
// 消息顺序是"摘要 → 保留段 → 边界之后"，摘要排在保留段之前。
//
// 返回值多一个 freshFrom：从这一条起，消息自带的 usage 才算数。边界之前的消息
// 记的是压缩前的账，而折叠把摘要塞在了它们前面——拿它当估算基数会把上下文估回
// 压缩前的大小（见 estimateContextTokens）。没有边界时 freshFrom 为 0，整条
// 路径的 usage 都算数，因为那条路径就是模型收到过的东西。
//
// upto 是算到哪条 entry 为止（不含，与 messagesIn 同口径）。压缩当场换历史段
// （Manager.MessagesAt）与判定大小（Manager.ContextTokens）都必须把上界卡在本轮
// 开始的地方：**本轮输入与本轮产生的消息在盘上位于边界之前**（只能往后追加，边界
// 是这一轮最后才写的），所以任何一次完整折叠都会把它们带进历史段，而它们同时还要
// 作为本轮的部分进请求——一处会重复替换、一处会重复计数。上界因此不是可选的
// 优化，是正确性的前提。
func (entries Entries) foldedContext(upto int) (schema.Messages, int) {
	if upto > len(entries) {
		upto = len(entries)
	}
	index := entries.lastBoundaryIndex()  // 边界可能落在 upto 之后，见下
	if index < 0 {
		return entries.messagesIn(0, upto), 0
	}

	messages := make(schema.Messages, 0, upto+1)
	if summary := entries[index].Compaction.summaryMessage(); summary != nil {
		messages = append(messages, summary)
	}
	// 保留段与边界之后的段都按 upto 截断：边界刚写下的那一刻它自己是最后一条，
	// 此时"边界之后"为空，而保留段里含着本轮已经产生的消息，全靠 upto 挡住。
	keptEnd := index
	if keptEnd > upto {
		keptEnd = upto
	}
	if kept := entries.indexOf(entries[index].Compaction.FirstKeptEntryID); kept >= 0 && kept < keptEnd {
		messages = append(messages, entries.messagesIn(kept, keptEnd)...)
	}

	freshFrom := len(messages)
	if index < upto {
		after := entries.messagesIn(index+1, upto)
		messages = append(messages, after...)
	}

	return messages, freshFrom
}

// lastBoundaryIndex 返回路径上最后一条可用的压缩边界下标，没有返回 -1。
//
// "可用"指 first_kept_entry_id 在路径上指得着、且不晚于边界自己。指不着的边界
// 当成不存在：折叠会把边界之后的全部消息原样保留，所以忽略一条边界只会让这一段
// 重新进上下文（可能撑爆窗口），绝不会丢历史。反过来，若照旧取最后一条边界、
// 再把指不着的保留段省掉，就会静默删掉一段历史。
func (entries Entries) lastBoundaryIndex() int {
	for index := len(entries) - 1; index >= 0; index-- {
		entry := entries[index]
		if entry.Type != EntryCompaction || entry.Compaction == nil {
			continue
		}
		if kept := entries.indexOf(entry.Compaction.FirstKeptEntryID); kept >= 0 && kept <= index {
			return index
		}
	}

	return -1
}

// compactionSummaryPrefix / Suffix 是摘要消息的包装。它是一条普通 user 消息，
// 模型只有靠这段文字才知道"这不是用户刚说的话，是更早历史的压缩结果"。
const (
	compactionSummaryPrefix = "此前的对话历史已压缩成以下摘要：\n\n<summary>\n"
	compactionSummarySuffix = "\n</summary>"
)

// summaryMessage 把摘要投影成一条 user 消息。BuildMessages 没有错误通道，
// 而这里的内容由 validate 保证非空、只有单个文本块，内容块规则不可能失败。
// 真失败（有人绕过 validate 塞了脏摘要）返回 nil，让调用方跳过摘要而不是
// 整条历史崩掉。
func (c Compaction) summaryMessage() schema.Message {
	message, err := schema.NewUserMessage(schema.ContentBlocks{
		schema.TextBlock(compactionSummaryPrefix + c.Summary + compactionSummarySuffix),
	})
	if err != nil {
		return nil
	}

	return message
}
```

一点与 pi.dev 的差别：它折叠时会跳过保留段里的 system message（`session-manager.ts:471`），因为它的系统提示词也存进会话。我们的会话只存 user / assistant / tool 三类（循环只在 `pi/loop.go:137,163` 两处通知观察者——模型消息与工具结果，观察者 `agent.go:110-115` 收到什么写什么，系统提示词每轮由 `ContextBuilder` 重新组装、从不落盘），所以没有这一步。

## 算法（新文件 `pi/session/compaction.go`）

纯函数，不碰文件、不调模型。对外的名字只有两个值类型（`Window`、`Plan`）和 `Entries` 上的方法。

### Window：窗口与预算

预算不散成参数，收进一个值类型：三个数字要一起算出来，判定又要同时用到它们。

```go
const (
	// defaultReserveTokens 是给模型输出预留的余量：触发线定在窗口减去它，
	// 而不是窗口本身。定在窗口本身等于"请求塞满了才压"，那一次没有空间生成回复。
	defaultReserveTokens = 16384
	// defaultKeepRecentTokens 是保留区预算：从最新往回数，数够这么多 token
	// 就停，更早的才摘要。最近的对话是当下任务的上下文，压掉就断片。
	defaultKeepRecentTokens = 20000
	// asciiCharsPerToken 是 ASCII 文本的折算比例：英文与代码大约 4 字符 1 token。
	asciiCharsPerToken = 4
	// estimatedImageChars 是图片块折算的字符数：图片没有文本可数，按经验值折算。
	estimatedImageChars = 4800
)

// Window 是一个模型的上下文预算：窗口大小，以及由它算出的两个余量。
type Window struct {
	// Tokens 是模型的上下文窗口，单位 token。它只能由调用方给
	// （providers.Options.ContextWindow，装配时经 NewWindow 换算）：这个包不知道
	// 任何一个具体模型的窗口有多大，也不去猜。<= 0 表示没配窗口，Enabled 报 false。
	Tokens int64
	// Reserve 是给模型输出预留的余量：触发线因此是 Tokens - Reserve 而不是
	// Tokens（这个值为什么这么定，见 defaultReserveTokens）。窗口太小时 NewWindow
	// 会把它减半，所以拿到手它不一定等于 defaultReserveTokens。
	Reserve int64
	// KeepRecent 是保留区预算：压缩时从最新往回数，数够这么多 token 就停，更早的
	// 才摘要（理由见 defaultKeepRecentTokens）。与 Reserve 一样，小窗口下 NewWindow
	// 会把它减半。
	KeepRecent int64
}

// NewWindow 按窗口大小算预算。小窗口模型按比例收缩：两个预算之和必须明显小于
// 窗口，否则保留区自己就贴着触发线，压完立刻又触发。窗口没配或小到分不出预算
// 时返回零值，Enabled 报 false。
func NewWindow(tokens int64) Window {
	if tokens <= 0 {
		return Window{}
	}
	window := Window{Tokens: tokens, Reserve: defaultReserveTokens, KeepRecent: defaultKeepRecentTokens}
	for window.KeepRecent > 0 && window.Reserve+window.KeepRecent >= tokens {
		window.KeepRecent /= 2
	}
	for window.Reserve > 0 && window.Reserve+window.KeepRecent >= tokens {
		window.Reserve /= 2
	}
	if window.Reserve == 0 || window.KeepRecent == 0 {
		return Window{}
	}

	return window
}

// Enabled 报告这个窗口能不能压缩。三条都不满足就不压：没配窗口、分不出预算、
// 或窗口减掉预留之后连摘要请求自己的开销都放不下——最后一种压了也没法生成摘要。
func (w Window) Enabled() bool {
	return w.Tokens > 0 && w.KeepRecent > 0 && w.Tokens-w.Reserve > requestHeadroomTokens
}

// OverLine 判定一个已算好的估算值是否越过触发线。估算本身不在这儿做：哪些 usage
// 作废只有会话知道（见 Manager.ContextTokens），这里只管那条不等式。
func (w Window) OverLine(contextTokens int64) bool {
	if !w.Enabled() {
		return false
	}

	return contextTokens > w.Tokens-w.Reserve
}
```

### 估算：包内辅助

接收者类型（`schema.Messages`、`schema.Message`）属于别的包，Go 不允许给外部类型加方法，所以这几个只能是函数；但不导出——对外只有 `Manager.ContextTokens`、`Entries.plan` 与 `Window.OverLine` 三个入口。

```go
// estimateContextTokens 估算当前上下文大小：最后一次真实 usage 当基数，加上它
// 之后的消息。usage 记的是"截至那条消息"的账，其后新增的只能估。
//
// freshFrom 之前的下标不作数：那些消息带着的是压缩前的 usage，而折叠在它们前面
// 塞了一条摘要——那次请求里含着已经被摘要掉的那段历史，拿它当基数会把压缩后的
// 上下文估回压缩前的大小，压完的下一轮立刻又触发。基数只在 freshFrom 之后找，
// 一条都没有就全量估算：折叠后的列表本来就短，全量估足够准。
//
// pi.dev 在触发判定上处理同一件事，用的是时间戳：助手消息的时间戳不晚于最新边界
// 就整个跳过这次判定（agent-session.ts:2327-2335，注释 "This prevents a stale
// pre-compaction usage/error from retriggering compaction on the first prompt
// after compaction"），用量路径下同样如此（:2391-2406）。我们改成"换掉基数、
// 接着判"：作废的 usage 不该当证据，但同一次判定里还有别的大小证据（全量估算），
// 该压还是要压。用 entry 顺序而不是时间戳，是因为会话文件里顺序就是时间。
func estimateContextTokens(messages schema.Messages, freshFrom int) int64 {
	if freshFrom < 0 {
		freshFrom = 0
	}
	for index := len(messages) - 1; index >= freshFrom; index-- {
		assistant, ok := messages[index].(*schema.AssistantMessage)
		if !ok {
			continue
		}
		base := assistant.Usage.TotalTokens()
		if base == 0 {
			continue
		}

		return base + estimateTokens(messages[index+1:])
	}

	return estimateTokens(messages)
}

// 总数的口径不在这里：它属于 Usage 自己，落在 schema.Usage.TotalTokens
// （只加输入 + 输出；缓存读写与推理各自是子集，四项相加会把缓存算三遍，
// 估算凭空高一截，触发线跟着失真）。

// estimateTokens 估算一段消息占用的 token 数。
func estimateTokens(messages schema.Messages) int64 {
	var total int64
	for _, message := range messages {
		total += estimateMessageTokens(message)
	}

	return total
}

func estimateMessageTokens(message schema.Message) int64 {
	if message == nil {
		return 0
	}

	chars := 0
	for _, block := range message.Blocks() {
		if block.Type == schema.ContentTypeImage {
			chars += estimatedImageChars
			continue
		}
		chars += estimateTextChars(block.Text)
	}
	// 工具调用的名字与参数也要算：它们原样进请求，不是附注。
	if assistant, ok := message.(*schema.AssistantMessage); ok {
		for _, call := range assistant.ToolCalls {
			chars += estimateTextChars(call.Name) + estimateTextChars(string(call.Arguments))
		}
	}

	return int64((chars + asciiCharsPerToken - 1) / asciiCharsPerToken)
}

// estimateTextChars 把一段文本折成"按 ASCII 比例计的字符数"。
//
// 不是直接数长度：ASCII 按既定比例折算，非 ASCII 一个字符算一个 ASCII 字符的
// asciiCharsPerToken 倍——中文、日文一个字大约一个 token，而一个汉字是 3 字节、
// 按 ASCII 的比例折只会算成四分之一个 token。整体除以 4 会把中文低估到四分之一，
// 方向与"宁可高估"正好相反。
//
// pi.dev 用的是裸 chars/4（compaction.ts:278-321，注释还自称 overestimate）——
// 那个说法只对英文成立，这里是中文优先的仓库，故意不跟。
func estimateTextChars(text string) int {
	ascii, wide := 0, 0
	for _, rune := range text {
		if rune < utf8.RuneSelf {
			ascii++
			continue
		}
		wide++
	}

	return ascii + wide*asciiCharsPerToken
}
```

### 切点

```go
// isCutPoint 报告这条 entry 能不能当压缩边界。user 与 assistant 可以；工具结果
// 不行——它必须紧跟发起它的那条助手消息，切在它前面会把两者拆散。
// header 与压缩边界本身不产生上下文消息，也不当切点。
func (entry Entry) isCutPoint() bool {
	if entry.Type != EntryMessage || entry.Message == nil {
		return false
	}
	switch entry.Message.Role() {
	case schema.RoleUser, schema.RoleAssistant:
		return true
	default:
		return false
	}
}

// cutPoints 列出 [start, end) 里所有合法切点的下标，升序。
func (entries Entries) cutPoints(start, end int) []int {
	points := make([]int, 0, end-start)
	for index := start; index < end; index++ {
		if entries[index].isCutPoint() {
			points = append(points, index)
		}
	}

	return points
}

// findCutPoint 从最新往回数，数够 keepRecent 就停，返回保留区第一条 entry 的
// 下标；没有合法切点或区间为空时返回 -1。
//
// 累加超过预算时取"不早于当前位置的最近合法切点"：尾部工具结果自己就超预算时，
// 宁可保留它前面那条发起调用的助手消息，也不回退到第一条。
func (entries Entries) findCutPoint(start, end int, keepRecent int64) int {
	points := entries.cutPoints(start, end)
	if len(points) == 0 {
		return -1
	}

	// 默认落在第一条合法切点上：整段都不够预算时，尽量多保留、只压更早的。
	cut := points[0]
	var accumulated int64
	for index := end - 1; index >= start; index-- {
		tokens := estimateMessageTokens(entries[index].Message)
		if tokens == 0 {
			continue
		}
		accumulated += tokens
		if accumulated < keepRecent {
			continue
		}
		cut = points[len(points)-1]
		for _, candidate := range points {
			if candidate >= index {
				cut = candidate
				break
			}
		}
		break
	}

	return cut
}
```

### 计划

```go
// Plan 是一次压缩的计划：保留区从哪条 entry 起、哪些消息该摘要、压缩前的上下文
// 有多大。它是纯数据，压不压、压完写不写盘由调用方决定。替换后的历史段不在计划里
// ——那是读侧折叠的产物，计划只描述"压哪一段"（见 Manager.MessagesAt）。
type Plan struct {
	// FirstKeptEntryID 是保留区第一条 entry 的 id，写进压缩边界。
	FirstKeptEntryID string
	// Summarize 是要摘要、之后不再进上下文的那段消息。
	Summarize schema.Messages
	// TokensBefore 是压缩前那份上下文的估算值（折叠之后、模型本来会收到的样子）。
	TokensBefore int64
	// PreviousSummary 是上一条压缩边界的摘要，非空表示这次是增量更新。
	PreviousSummary string
	// LeafID 是计划成立时的叶子。写入前要在同一个锁里跟当前叶子比对。
	LeafID string
}

// Entry 用计划与一段摘要拼出要落盘的压缩边界。调用方只需要给摘要文本——
// 边界指针与压缩前的尺寸是计划的产物，不该由调用方再抄一遍。
func (p Plan) Entry(summary string) Entry {
	return Entry{
		Type: EntryCompaction,
		Compaction: &Compaction{
			Summary:          summary,
			FirstKeptEntryID: p.FirstKeptEntryID,
			TokensBefore:     p.TokensBefore,
		},
	}
}

// 替换后的历史段不在计划里，也不该在：它由读侧的折叠给出（Manager.MessagesAt）。
// 计划里曾经有过 Head / Kept 两个字段，用保留段在进程内拼一份历史；那等于同一份
// 历史有两个出处，而"两处必须逐条一致"是条不能靠测试守住的约定。删掉它们之后，
// 压缩当场给模型的那段历史就是折叠的产物，只有一个出处。
// plan 在一条根到叶的路径上做压缩计划。upto 是本轮开始时的叶子 id：压缩只切到
// 它之前，本轮输入与本轮产生的消息一律不进摘要，也不进替换后的历史段（历史段的
// 上界由折叠负责，见 foldedContext 的 upto）。
//
// 返回 nil 与 false 表示不值得压：窗口没配、路径为空、末尾已经是压缩边界（没有新对话
// 可压）、upto 指不着、区间里没有合法切点、或边界之后没有可摘要的消息。
func (entries Entries) plan(window Window, upto string) (*Plan, bool) {
	if !window.Enabled() || len(entries) == 0 || upto == "" {
		return nil, false
	}
	// 末尾已经是压缩边界：上一次压完还没有新对话，再压只会得到同一份摘要。
	if entries[len(entries)-1].Type == EntryCompaction {
		return nil, false
	}
	// 本轮开始的位置。指不着就不压：切到哪里都会把本轮的对话切进去。
	uptoIndex := entries.indexOf(upto)
	if uptoIndex < 0 {
		return nil, false
	}

	// 边界在整条路径上找，而不是在 upto 之前找：一次 Run 里可能已经压过一次，
	// 那条边界落在 upto 之后，但它的保留段起点仍是要接着摘要的地方。
	previousSummary := ""
	start := 0
	if index := entries.lastBoundaryIndex(); index >= 0 {
		previousSummary = entries[index].Compaction.Summary
		// 二次压缩从上次的保留区起点接上，不是从边界之后接：上次保留的那段
		// 现在也旧了，跟新消息一起重新摘要——摘要因此是增量更新而不是重压。
		start = entries.indexOf(entries[index].Compaction.FirstKeptEntryID)
	}

	// 切点只在 upto 之前找：本轮的消息不进摘要。
	view := entries[:uptoIndex+1]
	if start > uptoIndex {
		start = uptoIndex
	}
	// 起点落在一条工具结果上时往前推：摘要请求不该以"没有对应调用的工具结果"
	// 开头。lastBoundaryIndex 已经保证指针落在一条可用边界上，这里兜住的是
	// 手工改过的文件。
	for start < uptoIndex && view[start].isToolMessage() {
		start++
	}

	cut := view.findCutPoint(start, len(view), window.KeepRecent)
	if cut < 0 {
		return nil, false
	}
	summarize := view.messagesIn(start, cut)
	if len(summarize) == 0 {
		return nil, false
	}

	// TokensBefore 量的是"不压的话这轮要发多大"，也就是折叠之后的那份上下文，
	// 不是整条路径：二次压缩时两者差着一整段已经被摘要掉的历史。估算的基数规则
	// 与判定时同一套（foldedContext 给出的 freshFrom）。这里算到 upto 为止：本轮
	// 已经产生的消息此刻也在盘上（折叠会把它们带进来），但它们不属于"压缩前那份
	// 上下文"——历史段的上界在 upto。
	folded, freshFrom := entries.foldedContext(uptoIndex + 1)

	return &Plan{
		FirstKeptEntryID: view[cut].ID,
		Summarize:        summarize,
		TokensBefore:     estimateContextTokens(folded, freshFrom),
		PreviousSummary:  previousSummary,
		LeafID:           entries[len(entries)-1].ID,
	}, true
}
```

### 区间与查找

```go
// messagesIn 取 [start, end) 区间里 entry 的对话消息。保留段与要摘要段都是这条
// 路径上的区间，取法集中在这里。
func (entries Entries) messagesIn(start, end int) schema.Messages {
	if start < 0 {
		start = 0
	}
	if end > len(entries) {
		end = len(entries)
	}
	if start >= end {
		return nil
	}
	messages := make(schema.Messages, 0, end-start)
	for _, entry := range entries[start:end] {
		if entry.Type == EntryMessage && entry.Message != nil {
			messages = append(messages, entry.Message)
		}
	}

	return messages
}

// isToolMessage 报告这条 entry 是不是一条工具结果消息。与 isCutPoint 同类
// （entry 的形状谓词），同样挂在 Entry 上。
func (entry Entry) isToolMessage() bool {
	return entry.Type == EntryMessage && entry.Message != nil &&
		entry.Message.Role() == schema.RoleTool
}

// messagesOf 取整条路径上的对话消息。
func (entries Entries) messagesOf() schema.Messages {
	return entries.messagesIn(0, len(entries))
}

// indexOf 返回 entry id 在路径上的下标，找不到返回 -1。
func (entries Entries) indexOf(id string) int {
	for index := range entries {
		if entries[index].ID == id {
			return index
		}
	}

	return -1
}
```

`lastBoundaryIndex` 见"读侧折叠"一节——折叠与计划共用它，所以定义在 `compaction.go`。

## 摘要请求（新文件 `pi/session/summary.go`）

提示词、序列化、请求拼装放一起，因为它们一起改：模板说"上面的对话"，序列化定义"上面"长什么样，请求把两者装进一条 user 消息。Plan 的 `Request` 与 `transcript` 同住这里；Plan 的字段定义在 `compaction.go`。

### 容量：序列化正文的额度

触发线保证的是"上下文不再变长"，不是"摘要请求一定发得出去"：要摘要的那段本身就是旧上下文的一部分，它可能正贴着窗口；序列化还要加标签。所以请求在拼装时自己算一遍容量，超了就按代价从低到高降级——先压单条工具结果，再压所有单条消息，最后丢最早的消息。降级一定要在正文里留痕，读到的人得知道这份摘要是残缺的。

这里说清楚额度管的是什么：**它管的是序列化正文**。模板、标签、降级说明与旧摘要都是固定的，先在正文之前算出来，从额度里扣掉（`transcriptBudget` 的第二个参数就是这笔），剩下的才归正文。所以额度为正时，"正文 + 模板 + 旧摘要 + 留白"确实一起被压在窗口减 `requestHeadroomTokens` 之内；额度不为正时（旧摘要自己就接近窗口）这条保证失效，序列化退到最小形态并留痕，请求可能超窗口——超了就是 provider 报错、这次压缩失败、不落盘（见"错误处理"），不会留下一半的摘要。这比 pi.dev 多做一层，代价写在明处。

```go
const (
	// toolResultMaxChars 是序列化时单条工具结果的字符上限（第一级）。
	toolResultMaxChars = 2000
	// squeezedMaxChars 是降级后所有单条消息的字符上限（第二级）。
	squeezedMaxChars = 500
	// requestHeadroomTokens 是摘要请求留给输出、模板与标签的 token 余量。
	// 输出上限由 provider 定死（Anthropic 4096，anthropic.go:55），本仓库的
	// Provider.Stream 没有单次调用参数（pi/ai/provider.go:16），写死一个常量
	// 比假装能按窗口比例算出输出预算更诚实。窗口减预留之后放不下它就不压
	// （Window.Enabled）——那种窗口压出来的摘要请求必然超窗口。
	requestHeadroomTokens = 8192
)
```

```go
// transcriptBudget 是这次摘要请求能分给序列化正文的字符额度：窗口减去余量，
// 再减去固定部分（模板、标签、旧摘要）已经占掉的字符，最后折成"按 ASCII 比例
// 计的字符数"。不是正数时返回 0，调用方据此走最小序列化。
//
// 固定部分必须算进来：不然"上限"只限住了正文，模板与旧摘要可以把它顶出去，
// 这个数就只是正文的限额，不是请求的限额。
func (p Plan) transcriptBudget(window Window, fixedChars int) int {
	if !window.Enabled() {
		return 0
	}
	chars := int(window.Tokens-requestHeadroomTokens)*asciiCharsPerToken - fixedChars
	if chars <= 0 {
		return 0
	}

	return chars
}

// Request 拼装这次压缩的摘要请求：一条 system 说明任务，一条 user 装序列化后的
// 对话与提示词。请求里不带工具描述，模型没有"顺手做点别的"的入口。
//
// 请求本身也要放得下：固定部分先拼出来、先计价，剩下的额度才归序列化正文。
// 降级说明按"一定会出现"计价——它只有一行，先留出位置比拼两遍省事。
func (p Plan) Request(window Window) (schema.Messages, error) {
	system, err := schema.NewSystemMessage(schema.ContentBlocks{schema.TextBlock(summarySystemPrompt)})
	if err != nil {
		return nil, err
	}

	fixed := estimateTextChars(conversationOpen) + estimateTextChars(conversationClose) +
		estimateTextChars(summaryDegradedNote) + estimateTextChars(p.instructions())
	transcript, degraded := p.transcript(p.transcriptBudget(window, fixed))

	var builder strings.Builder
	builder.WriteString(conversationOpen)
	builder.WriteString(transcript)
	builder.WriteString(conversationClose)
	if degraded {
		builder.WriteString(summaryDegradedNote)
		builder.WriteString("\n\n")
	}
	builder.WriteString(p.instructions())

	user, err := schema.NewUserMessage(schema.ContentBlocks{schema.TextBlock(builder.String())})
	if err != nil {
		return nil, err
	}

	return schema.Messages{system, user}, nil
}

// instructions 是请求里跟在对话后面的那段固定文字：首次压缩用摘要模板，二次
// 压缩用旧摘要加增量模板。它与序列化的顺序一致——模板说的是"上面的对话"。
func (p Plan) instructions() string {
	if p.PreviousSummary == "" {
		return summaryPrompt
	}

	return previousSummaryOpen + p.PreviousSummary + previousSummaryClose + "\n\n" + updateSummaryPrompt
}

const (
	conversationOpen     = "<conversation>\n"
	conversationClose    = "\n</conversation>\n\n"
	previousSummaryOpen  = "<previous-summary>\n"
	previousSummaryClose = "\n</previous-summary>"
)

// transcript 把要摘要的消息序列化成纯文本，行首带角色标记，并按额度降级。
// degraded 报告是否发生过降级。
//
// budget <= 0 表示额度分不出来（窗口太小，或固定部分自己就占满了）：这时请求
// 无论如何都放不下，所以不逐条丢——丢最早的消息救不回窗口，只会再少给摘要模型
// 一段内容。走最小形态并留痕，把"放不下"这个事实交给 provider 报错。
//
// 额度为正时三级，代价从低到高：单条工具结果截到 2000 → 所有单条消息截到 500 →
// 从最早的消息开始整条丢弃。最后一级丢的是最旧的内容，并且留一行标记说明丢了
// 多少条——摘要模型据此才知道自己看的不完整。至少留一条：一条不剩的摘要请求
// 没有意义。
func (p Plan) transcript(budget int) (string, bool) {
	lines := p.serializedLines(toolResultMaxChars)
	if len(lines) == 0 {
		return "", false
	}
	if budget <= 0 {
		return strings.Join(p.serializedLines(squeezedMaxChars), "\n"), true
	}

	degraded := false
	if sumChars(lines) > budget {
		lines = p.serializedLines(squeezedMaxChars)
		degraded = true
	}
	dropped := 0
	for len(lines) > 1 && sumChars(lines) > budget {
		lines = lines[1:]
		dropped++
	}
	if dropped > 0 {
		marker := fmt.Sprintf("[更早的 %d 条消息未纳入本次摘要]", dropped)
		lines = append([]string{marker}, lines...)
		degraded = true
	}

	return strings.Join(lines, "\n"), degraded
}

// serializedLines 按每条消息的字符上限把要摘要的消息摊成若干行。
func (p Plan) serializedLines(limit int) []string {
	lines := make([]string, 0, len(p.Summarize))
	for _, message := range p.Summarize {
		if message == nil {
			continue
		}
		lines = append(lines, transcriptLines(message, limit)...)
	}

	return lines
}

// transcriptLines 把一条消息投影成若干行（助手消息的工具调用各占一行）。摘要
// 模型看到的是"要被阅读的对话"，不是"要继续的对话"——所以用显式标记而不是消息
// 角色本身，免得模型接话。图片块换成脱敏占位（复用 schema 的占位投影），不让带图
// 的消息在摘要请求里变成空行。
func transcriptLines(message schema.Message, limit int) []string {
	text, err := message.Blocks().WithImagePlaceholders().Text()
	if err != nil {
		// 脏块投影不出文本：给一条带标记的降级文本，不静默丢消息。
		text = fmt.Sprintf("[内容无法投影: %v]", err)
	}

	switch message.Role() {
	case schema.RoleAssistant:
		lines := []string{transcriptLine("[Assistant]", text, limit)}
		if assistant, ok := message.(*schema.AssistantMessage); ok {
			for _, call := range assistant.ToolCalls {
				lines = append(lines, transcriptLine("[Assistant tool calls]",
					call.Name+" "+string(call.Arguments), limit))
			}
		}

		return lines
	case schema.RoleTool:
		// 工具结果是长文本的主要来源：一条几万字符的输出原样塞进去，请求还没发
		// 出去就先撞窗口。第二级降级对它是唯一默认就截的角色。
		return []string{transcriptLine("[Tool result]", text, limit)}
	default:
		return []string{transcriptLine("["+string(message.Role())+"]", text, limit)}
	}
}

// transcriptLine 写一行 "标记: 正文"；空正文也保留标记，占位行不会被静默吃掉。
func transcriptLine(label, text string, limit int) string {
	if strings.TrimSpace(text) == "" {
		return label + ":"
	}

	return label + ": " + truncateForSummary(text, limit)
}

// truncateForSummary 截断一段文本，并留下"截掉了多少"的标记：读摘要的模型需要
// 知道这里是不完整的，否则会把半句输出当成全部。
func truncateForSummary(text string, max int) string {
	runes := []rune(text)
	if max <= 0 || len(runes) <= max {
		return text
	}

	return string(runes[:max]) + fmt.Sprintf("\n[... 另有 %d 个字符被截断]", len(runes)-max)
}

// sumChars 按与估算同一套口径数一段文本的"折合字符数"。
func sumChars(lines []string) int {
	total := 0
	for _, line := range lines {
		total += estimateTextChars(line)
	}

	return total
}
```

提示词本体：

```go
// summarySystemPrompt 让模型明白这是压缩任务，不是继续对话。
const summarySystemPrompt = `你是一个上下文压缩助手。你的任务是阅读一段用户与 AI 助手的对话，然后按指定格式产出结构化摘要。

不要继续这段对话，不要回答对话里的任何问题，只输出结构化摘要。`

// summaryPrompt 是初次压缩的模板。
const summaryPrompt = `上面的对话需要压缩成一份"上下文检查点"摘要，供另一个模型接着做这件事。

严格按下面的格式：

## Goal
[用户想达成什么？一个会话覆盖多个任务时逐条列出。]

## Constraints & Preferences
- [用户提出的约束、偏好或要求]
- [没有就写 "(无)"]

## Progress
### Done
- [x] [已完成的工作]

### In Progress
- [ ] [正在进行的工作]

### Blocked
- [卡住的问题，没有就不写这一节]

## Key Decisions
- **[决定]**：[理由]

## Next Steps
1. [接下来该做什么，按顺序]

## Critical Context
- [接着做需要的数据、示例或引用]
- [没有就写 "(无)"]

每一节都要简短。文件路径、函数名与错误信息必须原样保留。`

// updateSummaryPrompt 是二次压缩的模板：在旧摘要上做增量，而不是重写。
const updateSummaryPrompt = `上面的消息是要并进已有摘要的新对话，旧摘要在 <previous-summary> 标签里。

规则：
- 保留旧摘要里的全部信息
- 把新消息里的进展、决定与上下文补进去
- Progress 一节：已完成的从 In Progress 挪到 Done
- Next Steps 按当前状态更新
- 文件路径、函数名与错误信息必须原样保留

` + summaryPrompt

// summaryDegradedNote 在降级过的请求里出现：读过它之后，摘要才不会假装自己
// 掌握了全部历史。
const summaryDegradedNote = `注意：上面的对话经过截断或省略，不是完整记录。请在摘要里如实反映"细节可能缺失"。`
```

## 编排

压缩要调模型，所以动作在 agent 层（唯一持有 provider 的地方）。

### 三段请求：循环把"可替换的历史"与"本轮的部分"分开（`pi/loop.go`）

现在循环把请求放成一条平铺的 `state.messages`（`pi/loop.go:18-22,112-116`）：历史、本轮输入、系统提示词、上下文块、本轮产生的消息混在一起。压缩要替换的是历史，而 `Context.CurrentInputIndex`（`pi/context.go:21,53`）已经标出了历史与本轮的分界——只是眼下没人读它（全仓库只有赋值，没有取值），`runState.contextHistory`（`pi/loop.go:20`）也是只写不读的死字段。

把请求拆成三段，压缩只换第一段：

```go
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

// BeforeTurn 在每轮模型调用之前调用，传入本轮请求；返回非 nil 时替换请求的
// 历史段（Head），返回 nil 表示不改写。
type BeforeTurn func(ctx context.Context, turn Turn) (schema.Messages, error)

// WithBeforeTurn 给循环接一个前置钩子。不设置时行为不变。
func WithBeforeTurn(hook BeforeTurn) LoopOption {
	return func(loop *Loop) { loop.beforeTurn = hook }
}
```

```go
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
```

`run` 的装配改成按 `CurrentInputIndex` 切分（`ContextBuilder.Build` 的排布是"历史 → 本轮输入 → 系统提示词 → 上下文块"，所以这个下标正好是切点）：

```go
func (l *Loop) run(ctx context.Context, runContext *Context) (schema.Messages, error) {
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
	...
}
```

循环体里，在 `l.complete` 之前接钩子，其余返回点把 `state.messages` 换成 `state.messages()`：

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

		message, err := l.complete(ctx, state)
		if err != nil {
			return state.messages(), err
		}
		...
		state.produced = append(state.produced, message)
		l.observe(message)
		...
			state.produced = append(state.produced, result)
			l.observe(result)
```

`complete` 取 `state.messages()`。下标越界校验不是形式主义：`Context` 是导出类型，调用方可以自己造一个塞进 `run`。

### agent 侧（`pi/agent.go`）

`Options` 加一个观察者，与 `TextObserver` 同款（nil 表示丢弃）：

```go
	// CompactionObserver 接收上下文压缩的结果，可为 nil 表示丢弃。
	CompactionObserver func(CompactionEvent)
```

```go
// CompactionEvent 报告一次压缩的结果：TokensBefore 是压缩前的上下文估算；
// Err 为 nil 表示这次压成了，否则表示这次没压、历史原样保留。
type CompactionEvent struct {
	TokensBefore int64
	Err          error
}
```

`Agent` 多三个字段：`provider`（原本只在构造期传给 Loop，现在 agent 自己也要调模型）、`window`（`session.Window`）、`headLeafID`（本轮开始时的叶子）。

```go
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
```

`headLeafID` 在 `prepareRunContext` 里取，取在**追加本轮输入之前**：

```go
	// 记下本轮开始时的叶子：压缩只切到它之前，本轮的输入与产生的消息都留着。
	a.headLeafID = a.session.LeafID()
	history, err := a.history(inputMessage)
	if err != nil {
		return nil, err
	}
```

```go
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

func (a *Agent) report(event CompactionEvent) {
	if a.onCompaction == nil {
		return
	}
	a.onCompaction(event)
}
```

`Run` 开头与 `writeErr` 一起清掉 `headLeafID`；`newAgent` 里存 provider 与窗口，并给 Loop 挂上钩子：

```go
	agent := &Agent{
		contextBuilder: NewContextBuilder(opts.WorkDir),
		session:        opts.Session,
		provider:       provider,
		window:         session.NewWindow(int64(opts.ProviderOptions.ContextWindow)),
		onCompaction:   opts.CompactionObserver,
	}
	agent.loop = NewLoop(
		provider,
		...,
		WithBeforeTurn(agent.compactBeforeTurn),
	)
```

### 写入：`Manager.Compact` 一次锁内校验并落盘

计划与追加是两个动作，中间放得进另一次写入。`Append` 会用当前叶子填 ParentID（`manager.go:127-132`），所以旧计划不会写坏链、也不会丢数据——但 `TokensBefore` 会失真、行为不确定。把校验与落盘收进一个入口：

```go
// Compact 落盘一次压缩：在同一个锁里核对计划仍然成立（计划成立时的叶子就是当前
// 叶子、边界指针在当前路径上且不晚于边界自己），再写边界。
//
// 指针校验必须在这儿做而不是在 validate 里：validate 只看得到一条 entry，
// 看不到路径，而"指得着"是路径级的事实。
//
// plan 为 nil 是调用方的错误（计划只有 PlanCompaction 一个来源，拿到非 nil 的
// 计划才该走到这里），返回 90000 而不是让它解引用时 panic。
func (m *Manager) Compact(plan *Plan, summary string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if plan == nil {
		return pierrors.ErrInternal.Wrap(errors.New("压缩计划为空"))
	}
	if plan.LeafID != m.leafID {
		return pierrors.ErrSessionCompactionPointerStale.Wrap(fmt.Errorf(
			"计划成立于叶子 %s，当前叶子是 %s", plan.LeafID, m.leafID))
	}
	path := m.entries.pathToRoot(m.leafID)
	if kept := path.indexOf(plan.FirstKeptEntryID); kept < 0 || kept > len(path)-1 {
		return pierrors.ErrSessionCompactionPointerStale.Wrap(fmt.Errorf(
			"first_kept_entry_id=%s 不在当前路径上", plan.FirstKeptEntryID))
	}

	return m.appendLocked(plan.Entry(summary))
}
```

`Append` 拆成"加锁 + `appendLocked`"，两条路径共用同一份校验与落盘：

```go
func (m *Manager) Append(entry Entry) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.appendLocked(entry)
}
```

## 文件格式版本：旧文件不能压，也不动它

版本策略是"新文件写 3、读侧接受 2..3"，升版的理由只有一个：**压缩边界是一种旧二进制读不懂的 entry 类型**。它插在链中间，旧二进制跳过它（`file.go:61-63` 只比首行，`:67-76` 把认不出的行按坏行跳过）之后，边界后面那条 entry 的 ParentID 就指不着了——`pathToRoot`（`entry.go:90-97`）追到那里就断，旧二进制看到的历史静默短一截。首行是旧二进制唯一的版本信号，所以新文件必须是 3。

已经存在的 v2 文件不升级、也不重写，直接拒绝压缩。两处拒绝，各管一件事：判定之前那次省掉一次白跑的摘要调用（上下文压不下去，每轮都会重新越线），写入之前那次是兜底——文件格式的事不该只守在一处。

```go
// manager.go
// PlanCompaction 对当前路径做一次压缩计划。upto 是本轮开始时的叶子 id：
// 本轮输入与本轮产生的消息不在压缩范围内，必须原样留在请求里。upto 为空或
// 指不着时返回 nil 与 false。
//
// 文件版本过旧的会话直接返回 nil 与 false，连计划都不做：v2 文件写不了边界（见
// appendLocked），做了计划也只是白调一次摘要模型，而上下文并不会因此变短——
// 于是每一轮都会重新越线、重新白跑。
func (m *Manager) PlanCompaction(window Window, upto string) (*Plan, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.canWriteBoundary() {
		return nil, false
	}

	return m.entries.pathToRoot(m.leafID).plan(window, upto)
}

// canWriteBoundary 报告这份会话能不能收压缩边界。内存会话能（没有首行要对谁诚实），
// 文件会话要求首行版本就是当前版本。
func (m *Manager) canWriteBoundary() bool {
	return m.file == nil || m.headerVersion >= sessionVersion
}
```

```go
// appendLocked 是 Append 与 Compact 的公共实现，调用方必须已经持有锁。
func (m *Manager) appendLocked(entry Entry) error {
	...
	// 压缩边界是 v3 才有的东西。v2 文件不升版、也不重写：首行永远写着 v2，塞进
	// 边界只会让旧二进制跳过它、把 parent 链断在那里。所以这里不是"升级之后接着
	// 写"，而是拒绝——写不进去比写进去更诚实。
	if entry.Type == EntryCompaction && !m.canWriteBoundary() {
		return pierrors.ErrSessionCompactionVersionUnsupported.Wrap(fmt.Errorf(
			"会话文件版本为 %d，不支持压缩边界（需要 %d）", m.headerVersion, sessionVersion))
	}
	...
}
```

这样还消掉一个隐含前提：**v2 文件里不可能有压缩边界**，所以读侧的折叠对 v2 文件必然退化成原样重建（`lastBoundaryIndex` 返回 -1）。这不是检查出来的，是构造上不可能。
`Manager` 多一个 `headerVersion` 字段（`newManager` 时从首行取），版本判断不必每轮读盘。读侧的接受范围：

```go
// constant.go
const (
	// sessionVersion 是写入的格式版本：3 起会话文件里可能出现压缩边界。
	sessionVersion = 3
	// minReadableSessionVersion 是能读的最旧版本：v2 文件里没有压缩边界，
	// 读出来就是不折叠，语义正确。
	minReadableSessionVersion = 2
)
```

```go
// file.go，替换现在的 `header.Header.Version != sessionVersion`
if header.Header.Version < minReadableSessionVersion || header.Header.Version > sessionVersion {
	return nil, f.invalid(fmt.Errorf("不支持的会话文件版本 %d，当前版本为 %d",
		header.Header.Version, sessionVersion))
}
```

代价写在明处：

- **这份二进制发布之前建立的会话都压不了**，直到新建一个（新文件是 v3，照常能压）。压缩是优化，不是既有会话的运行前提：拒绝只是让它们继续贴着窗口跑。
- **带压缩边界的 v3 文件，旧二进制打不开**（首行就报 80000）。这正是想要的，比静默截断强。
- 换来的是：整份设计里没有"读—改—写"。只有 `O_APPEND` 追加与建文件时的一次 `os.Link`，唯一可能丢数据的窗口（重写盖掉别人在这期间的追加）随之消失，也不再需要 `flock`。

v2 老文件在新二进制里照常打开、照常读、照常聊，只是不压。

## 配置：窗口大小

判定要有分母。`providers.Options` 加一个字段（`pi/ai/providers/options.go`）：

```go
	// ContextWindow 是该模型的上下文窗口，单位 token。上下文估算越过
	// "窗口 - 预留余量"时触发压缩；0 表示不压缩（没配分母就无法判断），
	// 负数是非法配置。
	ContextWindow int `json:"contextWindow,omitempty"`
```

`Validate` 里加一条：

```go
	if opts.ContextWindow < 0 {
		return pierrors.ErrAIInvalidRequest.Wrap(errors.New("contextWindow 不能为负"))
	}
```

选它而不是 `pi.Options` 的理由：窗口是模型的属性，跟 `Model` / `SupportsImageInput` 同一类（`options.go:19-31`），`config.json` 的 platforms 数组里每个平台配一次最自然。两个预算（reserve / keepRecent）仍是包内常量、不暴露。

## 错误处理

| 情况 | 行为 | 理由 |
|---|---|---|
| 摘要调用失败 / 被截断 / 摘要为空 | 不落盘、继续跑，事件里带 `Err`（50002） | 压缩是优化，失败不该打断客户这一轮；真溢出由 provider 报 20003 |
| 计划与落盘之间有人写过 | `Compact` 报 80015，不落盘 | 计划成立于另一个叶子，按它写边界是拿旧判断改新状态 |
| 边界指针不在当前路径上 | `Compact` 报 80015，不落盘 | 指不着的边界会让读侧丢掉一段历史 |
| 会话文件版本过旧（v2） | `PlanCompaction` 返回 nil，这次不压，文件不动 | 边界是旧二进制读不懂的类型，写进去会让它在链上断掉；v2 文件不重写、不升级，压不了就不压 |
| 写压缩 entry 失败 | `Run` 透出错误并返回已产生的消息 | 与会话写入失败同类：盘上历史缺一块，不能瞒 |
| 读侧遇到指不着的边界 | 当它不存在，回退到上一条可用边界 | 折叠保留边界之后的全部消息，所以忽略一条边界只多带一段历史，不丢历史 |
| 折叠时摘要投影失败 | 跳过摘要、保留段照旧 | 走不到（`validate` 保证非空）；真到了也只是降级 |

## 测试

算法与折叠全部离线可测，只有编排要假 provider。测试都在包内（`package session` / `package pi`），所以包内方法可以直接测。

`pi/session/compaction_test.go`：

- 切点落在预算边界上：造 5 轮对话（每轮 user + assistant），`KeepRecent` 设成恰好覆盖最后两轮，`plan` 给出的 `FirstKeptEntryID` 必须是第 3 轮的 user；
- 工具结果不是切点：预算只够最后一条工具结果时，切点回退到发起调用的那条助手消息；
- 整段都不够预算：切点落在第一条合法切点，压掉的只有更早的；
- **切点落在带工具调用的助手消息上时，保留段不以工具结果开头**（"不拆轮次"那条论证的回归）；
- `upto` 之后的 entry 不参与摘要：在本轮输入与产生的消息之后调 `plan`，`Summarize` 不含它们，`TokensBefore` 也只算到 `upto`；
- 末尾已是压缩边界 → `plan` 返回 nil；`upto` 指不着 → 返回 nil；
- 二次压缩的边界：路径上有旧边界时，`Summarize` 从旧 `FirstKeptEntryID` 起算，`PreviousSummary` 是旧摘要；
- 小窗口收缩：`NewWindow(40000)` 的两个预算之和必须小于窗口；`NewWindow(0)` / `NewWindow(100)` 的 `Enabled` 为假；
- 估算：`CacheReadTokens`/`CacheWriteTokens` 非零时总数不变（防把缓存加两遍的回归）；纯中文文本估出来的 token 数 ≥ 字符数（防把它按 ASCII 除以 4 的回归）；工具调用的参数也算（防"只数正文"的回归）；
- **压缩后的旧 usage 不作数**（本轮新增的回归）：造一段"usage 很大"的历史 + 一条边界，边界之后只有保留段、没有带 usage 的助手消息 → `ContextTokens` 必须退到全量估算，不得把压缩前的大小算回来；
- 同一个反面的守卫：没有边界时同一条 usage 必须**仍被当作基数**（别把 freshFrom 写成"永远不用 usage"）；
- 边界之后有一条带 usage 的助手消息时，基数用它而不是更早那条旧的；
- `Plan.Entry` 与计划字段一致：边界指针、`TokensBefore`；`Plan` 里没有历史段（历史段只从折叠来）；
- **折叠的上界**：一条路径上写边界之后，`foldedContext(len)` 含本轮已经产生的消息，`foldedContext(upto+1)`（`MessagesAt`）不含——这两条一起证明压缩当场换历史时挡得住重复；
- **估算与请求是同一份列表**（防"折算一份、发另一份"的回归）：不压缩的那一轮，`ContextTokens` 估的那串消息与 `Turn.Messages()`（真正发给 provider 的那串）逐条一致——顺序、条数、每条的内容都比；
- **同一 Run 里压完接着跑**：先跑出"助手消息 + 工具结果"，再在下一轮开头压缩，然后断言三件事——估算的消息列表里每条只出现一次（防本轮产出被算两遍）、`Turn.Messages()` 与它逐条一致、基数取的是边界之后那条 usage（压缩前那条很大的 usage 不再影响估算值）。

`pi/session/entry_test.go` 补：

- 折叠：三段历史 + 一条边界（`FirstKeptEntryID` 指第二段）→ 重建出来是"摘要 + 第二段 + 边界之后"；
- 没有边界时重建结果与折叠前完全一致（既有用例的守卫）；
- **指针指不着的边界被忽略**：重建结果等于完全没压过的历史（不丢消息）；
- **两条边界时只有最后一条可用**，且它的摘要进上下文；
- `validate` 拒收空载荷、空摘要、空指针，三条各一个原因码断言；
- 折叠出来的第一条消息角色是 user、内容里有 `<summary>`。

`pi/session/file_test.go` 补：

- **v2 文件压不了**：拿一份首行是 2 的文件，`PlanCompaction` 返回 nil、`Compact` 报 80016，文件一字节不变、没有边界（这条同时守住"设计里没有读—改—写"）；
- `Compact` 传一个 `LeafID` 不等于当前叶子的计划 → 80015，且文件里没多出边界。

`pi/session/summary_test.go`（经 `Plan.Request` 断言，不单独测 `transcript`）：

- 请求是两条消息，system 是摘要系统提示词，user 里有 `<conversation>`；
- 序列化的角色标记与顺序；
- 工具结果超 2000 字符被截断且留下截断标记；
- **超预算时降级到 500 字符，并出现降级说明**；
- **再超预算时最早的整条消息被丢掉，出现"更早的 N 条消息未纳入本次摘要"标记，且最后一条一定还在**；
- 模板与旧摘要计入额度：同一个 `Plan`，窗口调到"固定部分自己就占满额度"的大小，正文必须被压过（不得原样全文），且出现降级说明；
- 带图消息投影成脱敏占位而不是空行；
- `PreviousSummary` 非空时出现 `<previous-summary>` 且用增量模板。

`pi` 包：

- 用假 provider（`newAgent(provider, opts)` 这条测试路径已有）造一个超预算会话，断言第二次模型调用收到的消息里历史已被摘要替代，且会话文件里多了一条 compaction entry；
- **逐条比对压缩前后发给 provider 的消息**：压缩后序列里，本轮输入只出现一次、本轮已产生的助手消息与工具结果只出现一次、顺序与压缩前一致，且没有任何压缩前的历史消息凭空留下未摘要；
- **工具调用后的再次压缩**：一轮里先跑出工具调用，再触发压缩，同样逐条比对无重复、无遗漏；
- 假 provider 在摘要调用上返回 error → `Run` 不失败、`CompactionObserver` 收到带 `Err` 的事件、会话里没有 compaction entry；
- 假 provider 摘要返回 `FinishReasonLength` → 同上（不得落盘）；
- 压缩后 `ParentID` 链仍连续：`Append` 的下一条消息挂在压缩边界之后；
- **压完不重复触发**：压过一次之后，紧接着的下一轮不得再压——假 provider 统计摘要调用次数，第二次模型调用之前不能出现第二次摘要请求（压完那一轮的回复带 usage，判定该用它，不该用压缩前那条旧的）；

## 实施顺序

1. `constant.go` / `entry.go`：类型、载荷、`validate`、原因码 80014 / 80015；
2. `compaction.go`：`Window`、估算、切点、`Plan` 与 `plan`、折叠（纯函数，先带测试）；
3. `manager.go`：`BuildMessages` / `MessagesAt` 改走折叠（带上界），`Append` 拆出 `appendLocked`，新增 `ContextTokens` / `PlanCompaction` / `Compact` / `canWriteBoundary`；补折叠、上界、估算作废与指针校验测试；
4. `summary.go`：提示词、容量降级、`Plan.Request` / `Plan.transcript`（纯函数，先带测试）；
5. `constant.go` / `file.go` / `manager.go`：版本升 3、读侧接受 2..3、`headerVersion` 与 `canWriteBoundary`（v2 文件拒绝压缩，报 80016），改 `version_test.go`；
6. `providers.Options.ContextWindow` 与校验；
7. `loop.go` 的三段请求与 `WithBeforeTurn`、`agent.go` 的编排与观察者、50002；
8. `cmd/sessiontest` 加一个 `-compact` 端子：打印压缩前后的 entry 条数与重建消息条数——"压完到底少没少"从此是屏幕上的一行数字。

前 4 步做完，压缩的全部判断逻辑就已经可测；第 7 步才需要真模型。

## 与 pi.dev 的对照

| 项 | pi.dev | 本方案 | 理由 |
|---|---|---|---|
| 触发 | `contextTokens > contextWindow - reserveTokens`（`compaction.ts:250-253`） | 同（`Window.OverLine`） | 一条不等式，没有别的机关 |
| 旧 usage 作废 | 触发所依据的助手消息时间戳不晚于最新边界就**跳过整个判定**（`agent-session.ts:2327-2335`，注释写明"防止压缩前的账在压完的第一个 prompt 上重新触发"），用量路径下同样（`:2391-2406`） | 边界之前的 usage 不当估算基数，退回全量估算，判定照做 | 语义一致（压缩前的账不许把上下文估回原样）；差别在那一句"跳过"：作废的 usage 不该当证据，但同一次判定还有别的大小证据，该压还是要压。它按时间戳判，我们按 entry 顺序——会话文件里顺序就是时间 |
| 触发时机 | 每次助手响应之前（`agent-session.ts:568-591`）与响应之后（`:2410`） | 每次模型调用之前（`BeforeTurn`） | 覆盖 pi.dev 的主要时机；"响应之后"那条是为了处理溢出失败重试（Case 1/2），这轮不做重试 |
| token 估算 | 最后一条 usage + 其后 chars/4（`compaction.ts:217-248`）；估算器本身是裸 `chars/4`，注释自称保守（`:278-321`） | 最后一条 usage + 其后"ASCII 按 4:1、非 ASCII 按 1:1" | 那个"保守"只对英文成立；本仓库中文优先，整体除以 4 会把中文低估到四分之一 |
| usage 基数 | `input + output + cacheRead + cacheWrite`（`compaction.ts:161-163`） | `InputTokens + OutputTokens` | 本仓库的缓存读写是输入的子集（`usage.go:52`、`anthropic.go:141`、`openai.go:377-381`），那边是分项不重叠的口径，抄过来会重复计算 |
| 切点 | user / assistant / bashExecution / custom / branchSummary；工具结果不行（`compaction.ts:323-336`） | user / assistant；工具结果、header、边界不行 | 本仓库只有这几类 entry（`constant.go:6-9`） |
| 摘要模板 | 七段固定格式（`compaction.ts:507-545`） | 同，中文 | 与仓库其他提示词同语言 |
| 二次压缩 | `<previous-summary>` + 增量模板（`compaction.ts:545-551`） | 同 | 重压会让摘要越压越短 |
| 摘要容量 | 不控请求总量，只截单条工具结果（`utils.ts:89-98`）；输出按 `0.8 * reserve` 要额度（`compaction.ts:685`） | 序列化正文三级降级（2000 → 500 → 丢最早），模板与旧摘要计入额度，输出不指定 | 本仓库 `Stream` 没有单次调用参数（`pi/ai/provider.go:16`），输出上限写死在 provider（`anthropic.go:55`），"按 reserve 算输出预算"表达不出来。额度算的是正文，但固定部分先扣掉，所以额度为正时整条请求确实压在窗口减余量之内——这比上游多做一层，别把它说成"请求总量上限" |
| 拆轮次 | 额外生成轮次前缀摘要再合并（`compaction.ts:895-940`） | 不做 | 非目标一节的论证只覆盖消息序列的合法性（切点不可能是工具结果，保留段不会以无主的工具结果开头）；语义上不等价——切在轮次中间时，那一轮的用户请求与前半程进展只留在压缩后的摘要文本里，代价写在非目标一节 |
| 请求分段 | 消息序列里压缩摘要替换一段区间（`agent-session.ts` 持有可变消息表） | 三段请求（Head / Tail / Produced），只换 Head | 本仓库本轮输入与产生的消息当口就落盘（输入在 `agent.go:179`，模型的产出在 `agent.go:110-115` 的观察者里），平铺拼接会让它们重复出现 |
| 文件清单 | `<read-files>` / `<modified-files>`（`utils.ts:62-84`） | 不做 | 会让 `pi/session` 认识工具名，越界 |
| 摘要请求 | 独立系统提示词 + 一条 user，不带工具（`compaction.ts:654-666`、`utils.ts:156`） | 同 | 摘要不该有"顺手做点别的"的入口 |
| 摘要失败 | 报错、不落盘、发事件（`compaction.ts:557-565`） | 同，且继续跑当前轮 | 压缩失败不该打断客户 |
| 落盘 | 追加 `CompactionEntry`，叶子前移（`session-manager.ts:1170-1190`） | 同（`Manager.Compact` + `Plan.Entry`） | 校验与落盘一次锁内完成 |
| 读侧折叠 | `firstKeptEntryId` 之前丢弃（`session-manager.ts:442-481`） | 同，另加"指针指不着就忽略这条边界" | 静默省掉保留段等于删历史 |
| 摘要进模型 | user 消息 + `The conversation history before this point was compacted…` 包装（`messages.ts:176-183`、`:11-17`） | 同，中文包装 | 模型只靠这层文字区分它和用户真说的话 |
| 窗口从哪来 | 模型记录上的必填字段 `contextWindow`（`ai/src/types.ts:1000`）。内置模型的值由生成的目录给出（`ai/scripts/generate-models.ts:1299,1395` 等，上游查不到就落 4096），provider 工厂把它一起挂上（`ai/src/providers/anthropic.ts:56`），`builtinModels()` 一次注册全部（`ai/src/providers/all.ts:137-143`）——用内置 provider 的业务一个数都不用给；自建 provider 与 `models.json` 得自己声明（`ai/src/models.ts:770` 的 `models: readonly Model[]`、`docs/models.md:209` 缺省 128000）。压缩侧永远只收入参：`this.model?.contextWindow ?? 0`（`agent-session.ts:2317`）→ `shouldCompact(contextTokens, contextWindow, settings)`（`compaction.ts:250-253`） | `providers.Options.ContextWindow`，配置给，0 表示不压 | 差别不在形状（两边都是"模型对象上的字段 + 调用点传参"），在于我们少了那一层目录：`providers.Options` 是 `config.json` 里手写的一条（`options.go:19-31`），没有按模型名查表的地方，所以数只能人来给。要补就补在 providers 包（目录或从平台拉），压缩侧不动。两边对"没配分母"的处理一致：不压（`agent-session.ts:572-573`） |
| 配置 | `enabled` / `reserveTokens` / `keepRecentTokens`，带模型级覆盖（`settings-manager.ts:887-906`） | 两个预算为包内常量，只暴露窗口 | 业务方要调的是窗口；预算调整等真有需求再加 |
| 格式版本 | `CURRENT_SESSION_VERSION = 3`，打开时迁移（`session-manager.ts:38,303-313`）；那次升版的内容是 `hookMessage` 角色改名成 `custom`（`:282-297`） | 2 → 3，读侧接受 2..3；旧文件不重写、不升级，只是压不了 | 两条升版理由不一样，分开说：它改的是一个角色的名字，不升版也能读旧数据；我们新增的是一种 entry 类型，旧二进制读不懂它——必须明确拒绝，而不是静默截断。旧文件那边我们选择"不动它、也不压它"：整文件重写是方案里唯一的读—改—写，换来的只是"发布前的会话也能压" |
| 读侧折叠的上界 | 没有这个概念（消息表在内存里，压缩只重写那张表） | 折叠带 `upto` 上界，`MessagesAt` 与 `ContextTokens` 都用它：一个取"压缩当场该换上的历史段"，一个估算请求有多大 | 我们这边消息一律先落盘再进请求（输入在 `agent.go:179`，模型的产出在 `agent.go:110-115` 的观察者里），本轮的产出在盘上位于新边界之前，不上界就会既进历史段又进本轮的部分——换历史与估算两处都会，所以上界长在折叠上，不是长在调用点上 |
