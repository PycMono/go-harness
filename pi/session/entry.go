package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	pierrors "github.com/PycMono/go-harness/pi/error"
	"github.com/PycMono/go-harness/pi/schema"
)

// Entry 是 session 文件里的一行。Type 决定哪个载荷非空：session 行填 Header，
// message 行填 Message，compaction 行填 Compaction。
type Entry struct {
	// Type 行类型（session / message / compaction），决定下面哪个载荷非空。
	Type EntryType `json:"type"`
	// ID 这一行的 id，会话内唯一：8 位随机，且与已有 id 查过重（newEntryID）。
	ID string `json:"id"`
	// ParentID 上一条 entry 的 id；首行（header）为空，链从它往下串。
	ParentID string `json:"parent_id,omitempty"`
	// Timestamp 这一行写入的时间，RFC3339 UTC（Append 补空值）。
	Timestamp string `json:"timestamp"`
	// Header 会话元信息，只有 Type 为 session 时非空。
	Header *Header `json:"header,omitempty"`
	// Message 对话消息（含工具调用与结果），只有 Type 为 message 时非空。
	// 联合类型的接口字段，解码走下面的 UnmarshalJSON。
	Message schema.Message `json:"message,omitempty"`
	// Compaction 压缩边界载荷，只有 Type 为 compaction 时非空。
	Compaction *Compaction `json:"compaction,omitempty"`
}

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

// isToolMessage 报告这条 entry 是不是一条工具结果消息。它与 isCutPoint 是同一类
// 判断：问的都是"这条 entry 长什么样"，不碰压缩的算法，所以两条都跟 Entry 的字段
// 同处本文件；压缩侧（切点搜索、摘要起点）只是调用它们。
func (entry Entry) isToolMessage() bool {
	return entry.Type == EntryMessage && entry.Message != nil &&
		entry.Message.Role() == schema.RoleTool
}

// UnmarshalJSON 解码一行 entry。需要自己实现是因为 encoding/json 填不了非空的接口
// 字段：Message 是联合类型，具体是哪个类型只能由 JSON 里的 role 决定。
//
// 别名 Alias 只是把 Entry 的字段（连同 tag）借过来，自身不带方法集，所以不会绕回
// 本方法；同名字段 Message 在这一层被 json.RawMessage 抢走（浅层字段优先），剩下的
// 字段照旧按 tag 解。header 行没有 message 键，载荷保持 nil。
func (entry *Entry) UnmarshalJSON(data []byte) error {
	type Alias Entry
	fields := struct {
		*Alias
		Message json.RawMessage `json:"message"`
	}{Alias: (*Alias)(entry)}

	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if len(fields.Message) == 0 {
		entry.Message = nil

		return nil
	}

	message, err := schema.DecodeMessage(fields.Message)
	if err != nil {
		return err
	}
	entry.Message = message

	return nil
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

// Header 是会话首行。名字与字段对齐 pi.dev 的 SessionHeader（session-manager.ts:40）：
type Header struct {
	// ID 会话 id（pi.dev 同名 id），与首条 entry 自己的 ID 同值，也是 parent 链的根。
	ID string `json:"id"`
	// Version 文件格式版本。写入的是 sessionVersion（3），读取时接受
	// minReadableSessionVersion..sessionVersion：v2 文件里没有压缩边界，
	// 读出来就是不折叠，语义正确；写边界时才要求版本够新（canWriteBoundary）。
	Version int `json:"version"`
	// CreatedAt 会话创建时间（对应 pi.dev 首行的 timestamp），RFC3339 UTC。
	CreatedAt string `json:"created_at"`
	// WorkDir 原始工作区路径（对应 pi.dev 的 cwd）。目录名那层是 encodeWorkDir 的
	// 有损编码，解不回来，所以这里是唯一的原始值留存处。
	WorkDir string `json:"work_dir"`
	// ParentSession 父会话文件路径（pi.dev 同名 parentSession，那边真在写）：fork 出的
	// 会话指回父文件。这边还没写过。
	ParentSession string `json:"parent_session,omitempty"`
}

// Entries 是一次会话的 entry 快照。重建上下文与它用到的查找（追根）都落在这个
// 集合上：接收者不变、方法也不修改自身，仍是纯函数，可以拿快照离线单测。
type Entries []Entry

// pathToRoot 从叶子沿 ParentID 追到根，再反转成从根到叶的顺序。链断在中间
// （中间某行坏掉）时保留从叶子走得通的一段，而不是丢掉整条路径。
func (entries Entries) pathToRoot(leafID string) Entries {
	indexByID := make(map[string]int, len(entries))
	for index, entry := range entries {
		indexByID[entry.ID] = index
	}

	reversed := make(Entries, 0, len(entries))
	// 计数上限兜住 id 互相指向的死循环：坏文件不该把重建卡死。
	for id, steps := leafID, 0; id != "" && steps < len(entries); steps++ {
		index, ok := indexByID[id]
		if !ok {
			break
		}
		reversed = append(reversed, entries[index])
		id = entries[index].ParentID
	}

	path := make(Entries, len(reversed))
	for index := range reversed {
		path[len(reversed)-1-index] = reversed[index]
	}

	return path
}

// hasID 报告 id 是否已经在快照里：生成新 entry id 时用它查重。
func (entries Entries) hasID(id string) bool {
	for _, entry := range entries {
		if entry.ID == id {
			return true
		}
	}

	return false
}

// validate 校验一条 entry 自身是否自洽。读文件时用它区分好行与坏行：
// 类型未知、或者载荷与类型对不上的行都按坏行处理。
//
// 载荷除了"在不在"还要"能不能用"：message 载荷要过自己的 Validate（工具结果的身份
// 字段、内容块规则），消息联合已经把逐字段的规则收在每个具体类型上，这里只做委派；
// compaction 载荷要摘要非空、指针非空——它们不是格式细节，是"这条边界到底替掉了
// 哪段历史"的语义，缺了就无从折叠。Header 只看载荷在不在，它里面的版本号由 load
// 在首行单独比对（见 sessionFile.load）。
//
// 返回的是 pi/error 里的原因码，不是临时字符串：坏行为什么被丢，日志和断言
// 都能问出来。环境码（80000 / 80001）由调用方挂，原因码只答"为什么"。
func (entry Entry) validate() error {
	if entry.ID == "" {
		return pierrors.ErrSessionEntryIDMissing
	}
	switch entry.Type {
	case EntryHeader:
		if entry.Header == nil {
			return pierrors.ErrSessionHeaderPayloadMissing
		}
	case EntryMessage:
		// 接口的 nil 判断就是"载荷在不在"：具体类型由解码或调用方给，空接口就是空载荷。
		if entry.Message == nil {
			return pierrors.ErrSessionMessagePayloadMissing
		}
		// 载荷在还要载荷可用。这条比旧的"只看在不在"严：旧代码放行的字段组合（比如
		// 工具结果缺 tool_name）现在会被判成坏行，与 Ruling 10 的坏行策略一致。原因码
		// 沿用 message 载荷那一个——对这一行来说，载荷同样是用不了的。
		if err := entry.Message.Validate(); err != nil {
			return pierrors.ErrSessionMessagePayloadMissing.Wrap(err)
		}
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
	default:
		// 带数据的挂细节，跟 pi/tools/register.go 同款写法。
		return pierrors.ErrSessionEntryTypeUnsupported.Wrap(
			fmt.Errorf("entry 类型不受支持: %q", entry.Type))
	}

	return nil
}

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
	index := entries.lastBoundaryIndex() // 边界可能落在 upto 之后，见下
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

// messagesOf 取整条路径上的对话消息，跳过 header 与压缩边界。判据是 entry 自己的
// 类型与载荷在不在（Message 是接口，nil 就是没载荷），不看消息结构体上的任何字段：
// 联合类型换过一轮实现，这里读的东西没变，所以不需要跟着改。
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
