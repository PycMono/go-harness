package ai

import (
	"context"

	"github.com/PycMono/go-harness/pi/tools"
)

type Provider interface {
	Stream(context.Context, Messages, tools.ToolDefinitions) Stream
}

// Stream 表示一次按顺序消费的模型响应流。
type Stream interface {
	Next() bool
	Current() StreamEvent
	Result() (*Message, error)
	Close() error
}
