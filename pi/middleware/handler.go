// Package middleware 提供 Tool 执行链的内置 Handler。
//
// 中间件契约（Handler / Execution / Block 语义）定义在 pi/tools，
// 术语对齐 pi.dev：链上的单元叫 Handler（pi 文档的 tool_call handlers），
// 被拦截的一次 Tool 执行叫 Execution（对应 tool_execution_start/end
// 事件），阻断叫 Block（对应 {block: true, reason}）。
//
// 本包只放实现，依赖方向 pi/middleware → pi/tools。Scheduler 是链的
// 驱动方，链由装配处（如 pi/agent.go）通过 Defaults() 注入。
package middleware

import (
	"context"

	"github.com/PycMono/go-harness/pi/schema"
)

// UpdateObserver 接收 Tool 执行过程中的流式更新。主包用它桥接 ToolEvent，
// 供后续的事件转发 Handler 使用。
type UpdateObserver func(context.Context, schema.ToolCall, schema.ToolUpdate)
