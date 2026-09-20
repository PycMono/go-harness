package tools

const (
	toolOutputTruncationMarker = "\n[output truncated]"
	staticToolOwner            = "pi:static"
)

type EventPhase string

const (
	EventStart  EventPhase = "start"
	EventUpdate EventPhase = "update"
	EventEnd    EventPhase = "end"
)
