// Package ai 定义模型 Provider 的抽象接口：屏蔽各平台 SDK 的差异，向上层暴露
// 统一的流式调用契约。本包不依赖任何 SDK 内部结构，消息与工具词汇由 pi/schema
// 承载。
package ai

import (
	"context"

	"github.com/PycMono/go-harness/pi/schema"
)

// Provider 是模型平台适配器的统一入口。
type Provider interface {
	Stream(context.Context, schema.Messages, schema.ToolDefinitions) Stream
}

// Stream 表示一次按顺序消费的模型响应流。
type Stream interface {
	Next() bool
	Current() schema.StreamEvent
	Result() (*schema.Message, error)
	Close() error
}
