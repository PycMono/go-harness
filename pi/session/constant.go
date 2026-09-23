package session

// EntryType 表示 session 文件里一行 entry 的类型。
type EntryType string

const (
	EntryHeader     EntryType = "session"    // 首行，只出现一次
	EntryMessage    EntryType = "message"    // 对话消息（含工具调用与结果）
	EntryCompaction EntryType = "compaction" // 压缩边界，摘要替代它之前的历史
)

const (
	// sessionVersion 是写入的格式版本：3 起会话文件里可能出现压缩边界。
	sessionVersion = 3
	// minReadableSessionVersion 是能读的最旧版本：v2 文件里没有压缩边界，
	// 读出来就是不折叠，语义正确。
	minReadableSessionVersion = 2
)

// sessionIDPrefix 是 NewSessionID 生成的会话 id 的前缀。会话 id 直接当文件名
// （<会话 id>.jsonl），所以这个前缀也决定了自动生成的会话文件长什么样：
// chat-7Q2M….jsonl。调用方自带 id 时不强制这个前缀。
const sessionIDPrefix = "chat-"

// entryIDLength 是 entry id 的长度：会话内唯一即可，不需要全局唯一。
const entryIDLength = 8

// entryIDAttempts 是 id 撞上已有 entry 后的重试次数（对齐 pi.dev 的 100 次），
// 仍撞就退到整串随机。
const entryIDAttempts = 100

// 摘要请求的拼装层：额度、序列化、请求本身，以及提示词。三样放一起是因为它们
// 一起改——模板说"上面的对话"，序列化定义"上面"长什么样，请求把两者装进一条
// user 消息。Plan 的 Request / transcript 住这里，Plan 的字段定义在 compaction.go。

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

// 压缩的算法层：窗口预算、token 估算、切点、计划，以及读侧折叠。全是纯函数，
// 不碰文件、不调模型。对外的名字只有两个值类型（Window、Plan）和 Entries 上的
// 方法；估算与折叠留在包内，因为它们的口径只有会话自己知道（哪些 usage 作废）。

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

// compactionSummaryPrefix / Suffix 是摘要消息的包装。它是一条普通 user 消息，
// 模型只有靠这段文字才知道"这不是用户刚说的话，是更早历史的压缩结果"。
const (
	compactionSummaryPrefix = "此前的对话历史已压缩成以下摘要：\n\n<summary>\n"
	compactionSummarySuffix = "\n</summary>"
)

const (
	conversationOpen     = "<conversation>\n"
	conversationClose    = "\n</conversation>\n\n"
	previousSummaryOpen  = "<previous-summary>\n"
	previousSummaryClose = "\n</previous-summary>"
)
