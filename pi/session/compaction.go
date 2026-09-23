package session

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/PycMono/go-harness/pi/schema"
)

// Window 是一个模型的上下文预算：窗口大小，以及由它算出的两个余量。
type Window struct {
	// Tokens 是模型的上下文窗口，单位 token。模型的上下文窗口是一次调用的 input + output 加起来的硬上限。它只能由调用方给。
	// 任何一个具体模型的窗口有多大，也不去猜。<= 0 表示没配窗口，Enabled 报 false。
	Tokens int64
	// Reserve 是给模型输出预留的余量：触发线因此是 Tokens - Reserve
	Reserve int64
	// KeepRecent 是保留区预算：压缩时从最新往回数，数够这么多 token 就停，更早的
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
func (w Window) OverLine(contextTokens int64) bool {
	if !w.Enabled() {
		return false
	}

	return contextTokens > w.Tokens-w.Reserve
}

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

// estimateContextTokens 估算当前上下文大小：最后一次真实 usage 当基数，加上它
// 之后的消息。usage 记的是"截至那条消息"的账，其后新增的只能估。基数取的是
// Usage.TotalTokens（只加输入 + 输出，缓存读写与推理各自是子集），这一层不重算
// ——"总数是哪两项"属于 Usage 的语义，写在它自己的文件里。
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

// estimateTokens 估算一段消息占用的 token 数。
func estimateTokens(messages schema.Messages) int64 {
	var total int64
	for _, message := range messages {
		total += estimateMessageTokens(message)
	}

	return total
}

// estimateMessageTokens 估算一条消息占用的 token 数：文本按比例折，图片按经验值，
// 工具调用的名字与参数算进去。
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

// 替换后的历史段不在计划里，也不该在：它由读侧的折叠给出（Manager.MessagesAt）。
// 计划里曾经有过 Head / Kept 两个字段，用保留段在进程内拼一份历史；那等于同一份
// 历史有两个出处，而"两处必须逐条一致"是条不能靠测试守住的约定。删掉它们之后，
// 压缩当场给模型的那段历史就是折叠的产物，只有一个出处。
//
// plan 在一条根到叶的路径上做压缩计划。upto 是本轮开始时的叶子 id：压缩只切到
// 它之前，本轮输入与本轮产生的消息一律不进摘要，也不进替换后的历史段（历史段的
// 上界由折叠负责，见 foldedContext 的 upto）。
//
// 返回 nil 与 false 表示不值得压：窗口没配、路径为空、末尾已经是压缩边界（没有
// 新对话可压）、upto 指不着、区间里没有合法切点、或边界之后没有可摘要的消息。
// 计划本身是指针：它的"有没有"就由 nil 表达，false 只回答同一个问题的另一半。
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
