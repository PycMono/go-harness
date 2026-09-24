package tools

const (
	// OutputTruncationMarker 是文本被截断时追加的标记。LimitText 用它，需要
	// 自己截文本的调用方（比如 MCP 的错误路径，它返回 error、过不了 LimitText）
	// 也用它——同一个标记只留一个来源。
	OutputTruncationMarker = "\n[output truncated]"
	staticToolOwner        = "pi:static"
)

type EventPhase string

const (
	EventStart  EventPhase = "start"
	EventUpdate EventPhase = "update"
	EventEnd    EventPhase = "end"
)
