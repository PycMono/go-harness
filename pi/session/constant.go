package session

// EntryType 表示 session 文件里一行 entry 的类型。
type EntryType string

const (
	EntryHeader  EntryType = "session" // 首行，只出现一次
	EntryMessage EntryType = "message" // 对话消息（含工具调用与结果）
)

// sessionVersion 是会话文件格式版本，写在 header 里。
const sessionVersion = 2

// sessionIDPrefix 是 NewSessionID 生成的会话 id 的前缀。会话 id 直接当文件名
// （<会话 id>.jsonl），所以这个前缀也决定了自动生成的会话文件长什么样：
// chat-7Q2M….jsonl。调用方自带 id 时不强制这个前缀。
const sessionIDPrefix = "chat-"

// entryIDLength 是 entry id 的长度：会话内唯一即可，不需要全局唯一。
const entryIDLength = 8

// entryIDAttempts 是 id 撞上已有 entry 后的重试次数（对齐 pi.dev 的 100 次），
// 仍撞就退到整串随机。
const entryIDAttempts = 100
