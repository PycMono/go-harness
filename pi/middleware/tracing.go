package middleware

import (
	"context"

	contexttracing "github.com/PycMono/go-context-sdk/tracing"
	"github.com/PycMono/go-harness/pi/observability"
	"github.com/PycMono/go-harness/pi/schema"
	"github.com/PycMono/go-harness/pi/tools"
)

// Tracing 为每次实际工具执行开一个 execute_tool {name} Span。只记元数据与
// 长度，不采参数与输出正文——正文可能很长，也可能带用户数据。
//
// 它替换 e.Ctx 为 Span 的 ctx：链上更靠内的 handler 与真实工具调用都拿它做
// 父上下文，工具的日志因而也落在同一个 trace 上。
func Tracing(e *tools.Execution) {
	err := contexttracing.WithSpan(e.Ctx, observability.ToolSpanName(e.Definition.Name),
		func(ctx context.Context) error {
			e.Ctx = ctx
			contexttracing.WithKV(ctx,
				contexttracing.OperationName("execute_tool"),
				contexttracing.ToolName(e.Definition.Name),
				contexttracing.ToolCallID(e.Call.ID),
				contexttracing.KV(observability.AttrToolParallelSafe, e.Definition.ParallelSafe),
				contexttracing.KV(observability.AttrToolArgumentsSize, len(e.Call.Arguments)),
			)

			e.Next()

			// 工具的失败（e.Err 非 nil）是业务性的：它作为一条 IsError 的
			// 工具消息回给模型，不中断调度。Span 上照样标 Error，但不改变
			// 链的走向——返回值就是 e.Err 本身。
			fields := []contexttracing.Field{
				contexttracing.KV(observability.AttrToolIsError, e.Err != nil),
				contexttracing.KV(observability.AttrToolOutputSize, toolOutputSize(e.Output)),
			}
			fields = append(fields, observability.ErrorFields(e.Err)...)
			contexttracing.WithKV(ctx, fields...)

			return e.Err
		},
		contexttracing.WithErrorClassifier(observability.ClassifyError),
	)
	e.Err = err
}

// toolOutputSize 统计工具输出的文本体量：只算文本块，图片按 0 计——图片的
// 体量已经在 ToolOutput 里由 provider 自己决定怎么送，这里量的是"回给模型的
// 文本有多长"。
func toolOutputSize(output schema.ToolOutput) int {
	size := 0
	for _, block := range output.Content {
		size += len(block.Text)
	}

	return size
}
