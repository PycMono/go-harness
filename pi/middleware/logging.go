package middleware

import (
	"github.com/PycMono/go-harness/pi/schema"
	"github.com/PycMono/go-harness/pi/tools"
	logsdk "github.com/PycMono/go-logger-sdk"
)

// Logging 记录 Tool 执行的开始与结束日志，结束日志带输出字节数与状态。
func Logging(e *tools.Execution) {
	fields := []logsdk.Fields{
		logsdk.Any("component", "tool_runtime"),
		logsdk.Any("tool", e.Definition.Name),
		logsdk.Any("tool_call_id", e.Call.ID),
	}
	logsdk.Info(e.Ctx, "tool execution", append(fields, logsdk.Any("phase", "start"))...)

	e.Next()

	status := "success"
	if e.Err != nil {
		status = "error"
	}
	logsdk.Info(e.Ctx, "tool execution", append(fields,
		logsdk.Any("phase", "end"),
		logsdk.Any("byte_count", contentByteCount(e.Output.Content)),
		logsdk.Any("status", status),
	)...)
}

// 输出文本的字节数
func contentByteCount(content []schema.ContentBlock) int {
	count := 0
	for _, block := range content {
		if block.Type == schema.ContentTypeText {
			count += len(block.Text)
		}
	}

	return count
}
