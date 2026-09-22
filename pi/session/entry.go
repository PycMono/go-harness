package session

import (
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
	Message *schema.Message `json:"message,omitempty"`
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

// messagesOf 取出路径上的对话消息，跳过 header。
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
// 只看"载荷在不在"，不看载荷里的值：Header.Version 没校验，未知版本今天照样能打开。
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
		if entry.Message == nil {
			return pierrors.ErrSessionMessagePayloadMissing
		}
	default:
		// 带数据的挂细节，跟 pi/tools/register.go 同款写法。
		return pierrors.ErrSessionEntryTypeUnsupported.Wrap(
			fmt.Errorf("entry 类型不受支持: %q", entry.Type))
	}

	return nil
}
