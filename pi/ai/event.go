package ai

type StreamEventType string

const (
	StreamEventStart     StreamEventType = "start"
	StreamEventTextDelta StreamEventType = "text_delta"
	StreamEventDone      StreamEventType = "done"
	StreamEventError     StreamEventType = "error"
)

// StreamEvent 是与具体模型 SDK 无关的模型响应事件。
type StreamEvent struct {
	Type      StreamEventType
	TextDelta string
}
