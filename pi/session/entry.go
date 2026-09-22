package session

import (
	"encoding/json"
	"fmt"

	pierrors "github.com/PycMono/go-harness/pi/error"
	"github.com/PycMono/go-harness/pi/schema"
)

// Entry 是 session 文件里的一行。Type 决定哪个载荷非空：session 行填 Header，
// message 行填 Message。
type Entry struct {
	// Type 行类型（session / message），决定下面哪个载荷非空。
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

// Header 是会话首行。名字与字段对齐 pi.dev 的 SessionHeader（session-manager.ts:40）：
type Header struct {
	// ID 会话 id（pi.dev 同名 id），与首条 entry 自己的 ID 同值，也是 parent 链的根。
	ID string `json:"id"`
	// Version 文件格式版本（pi.dev 同名 version，v1 的文件没这个字段），当前 1。
	// load 不校验它：未知版本今天照样能打开（见 validate）。
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

// messagesOf 取出路径上的对话消息，跳过 header。判据是 entry 自己的类型与载荷在不在
// （Message 是接口，nil 就是没载荷），不看消息结构体上的任何字段：联合类型换过一轮
// 实现，这里读的东西没变，所以不需要跟着改。
func (entries Entries) messagesOf() schema.Messages {
	messages := make(schema.Messages, 0, len(entries))
	for _, entry := range entries {
		if entry.Type == EntryMessage && entry.Message != nil {
			messages = append(messages, entry.Message)
		}
	}

	return messages
}

// validate 校验一条 entry 自身是否自洽。读文件时用它区分好行与坏行：
// 类型未知、或者载荷与类型对不上的行都按坏行处理。
//
// 载荷除了"在不在"还要"能不能用"：message 载荷要过自己的 Validate（工具结果的身份
// 字段、内容块规则），消息联合已经把逐字段的规则收在每个具体类型上，这里只做委派。
// Header 仍然只看在不在——Header.Version 没校验，未知版本今天照样能打开。
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
	default:
		// 带数据的挂细节，跟 pi/tools/register.go 同款写法。
		return pierrors.ErrSessionEntryTypeUnsupported.Wrap(
			fmt.Errorf("entry 类型不受支持: %q", entry.Type))
	}

	return nil
}
