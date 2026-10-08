package observability

// 本文件固定本包的 Span 名称与属性键：所有字符串字面量只在这里出现一次。
// gen_ai.* 是 OpenTelemetry 语义约定（gen-ai，development 状态）的名字，
// 统一经 go-context-sdk 的 preset 或这里的常量封装，业务代码不手写。

// AgentName 是本项目的代理名，进 gen_ai.agent.name。
const AgentName = "go-harness"

// Span 名称。
const (
	// SpanNameRun 是整次 Run 的 Span。
	SpanNameRun = "pi.run"
	// SpanNameTurn 是一轮的 Span：插话交付、压缩、模型调用、工具执行全在里面。
	SpanNameTurn = "pi.turn"
	// SpanNameCompaction 是一次上下文压缩的 Span。
	SpanNameCompaction = "pi.compact_context"
)

// ChatSpanName 返回一次物理模型请求的 Span 名：chat {model}。
func ChatSpanName(model string) string { return "chat " + model }

// ToolSpanName 返回一次工具执行的 Span 名：execute_tool {tool}。
func ToolSpanName(tool string) string { return "execute_tool " + tool }

// 属性键。
const (
	// AttrGenAIAgentName 是 OTel 语义约定里发起调用的代理名。
	AttrGenAIAgentName = "gen_ai.agent.name"
	// AttrErrorType 是 OTel 标准的错误分类键。
	AttrErrorType = "error.type"
	// AttrErrorCode 是本项目的稳定错误码（pi/error 的 CodeOf）。
	AttrErrorCode = "pi.error.code"

	// 轮次。
	AttrTurnIndex      = "pi.turn.index"
	AttrToolsAvailable = "pi.tools.available_count"
	AttrToolsRequested = "pi.tools.requested_count"

	// 用量：gen_ai.* 的 preset 覆盖不到的几项。
	AttrUsagePlatformID  = "pi.usage.platform_id"
	AttrUsageLatencyMS   = "pi.usage.latency_ms"
	AttrUsageTTFTMS      = "pi.usage.ttft_ms"
	AttrUsageCacheRead   = "pi.usage.cache_read_tokens"
	AttrUsageCacheWrite  = "pi.usage.cache_write_tokens"
	AttrUsageReasoning   = "pi.usage.reasoning_tokens"
	AttrStreamChunkCount = "pi.stream.chunk_count"

	// 工具执行。
	AttrToolParallelSafe  = "pi.tool.parallel_safe"
	AttrToolIsError       = "pi.tool.is_error"
	AttrToolArgumentsSize = "pi.tool.arguments_size"
	AttrToolOutputSize    = "pi.tool.output_size"

	// 压缩。只记压缩前的体量：摘要多长看子 chat Span 的
	// gen_ai.usage.output_tokens，不在父 Span 上再抄一遍；压缩成没成看 Span
	// 的状态，不加一个成功标志位。
	AttrCompactionBeforeTokens = "pi.compaction.before_tokens"
)
