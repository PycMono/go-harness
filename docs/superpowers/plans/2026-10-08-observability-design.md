# 可观测性方案（pi/observability）

## 一句话

`pi/observability` 落两件事：**追踪**（每次 Run／每一轮／每次模型请求／每次工具执行各开一个 Span，组成一棵 trace 树）与**计量**（给每次成功的模型响应补上延迟、TTFT、归属）。实现方式是给 `ai.Provider` 套两层装饰器 + 给工具链加一个 handler，底层复用 `go-context-sdk/tracing`（它底下是 OpenTelemetry），**业务代码不直接 import otel**。金额换算不进这个包：本包只吐原始数字。

## 改了哪些目录、加了哪些目录（一眼看明白）

标记：`+` 新增文件，`~` 修改文件，`.` 本方案不动（但被读写，列出来是为了看清依赖关系）。

```text
go-harness/
├── go.mod                                  ~  加 3 条依赖（go-context-sdk、两条 otel 转直接依赖）
├── cmd/
│   └── harness/main.go                     .  不动。接后端（OTLP exporter）是后续单独一轮
└── pi/
    ├── agent.go                            ~  装配套装饰器 + Run 开 run Span + compactBeforeTurn 开压缩 Span（summarize 不动）
    ├── agent_observability_test.go         +  agent 层：压缩 Span 开不开、红不红（包内测试，要用 newAgent）
    ├── loop.go                             ~  run 的循环体抽成 step，整段括进 turn Span
    ├── observability/                      ~  目录已存在（原先只有一个 README.md），Go 包是新增的
    │   ├── README.md                       ~  按实现改三行（原文写的 hint 不做）
    │   ├── semantics.go                    +  Span 名、属性键、AgentName
    │   ├── classify.go                     +  错误码分类器、错误属性
    │   ├── provider.go                     +  TracingProvider、Wrap（装饰顺序唯一的一处）
    │   ├── meter.go                        +  UsageMeter（延迟、TTFT、归属）
    │   ├── provider_test.go                +  新增
    │   └── meter_test.go                   +  新增
    └── middleware/
        ├── defaults.go                     ~  链首加 Tracing（顺序：Tracing → Logging → Retry → PanicRecovery）
        ├── tracing.go                      +  工具 Span 的 handler
        └── tracing_test.go                 +  新增

被本方案读写、但一个字都不改的文件：
    pi/ai/provider.go         被装饰的接口（Stream / Stream.Result / Stream.Close）
    pi/schema/usage.go        被计量层补字段（LatencyMS / TTFTMS / PlatformID / Model）
    pi/schema/message.go      被 chat Span 读（Usage、FinishReason）
    pi/schema/tools.go        被工具 Span 读（ToolCall.ID、ToolDefinition.ParallelSafe）
    pi/error/errors.go        被 classify.go 引用（CodeOf、ErrAIGeneration）
    pi/middleware/{handler,logging,recovery,retry}.go   不动，只在 defaults.go 里调整了相对位置
```

数一数：

| | 新增 | 修改 | 合计 |
|---|---|---|---|
| 目录（Go 包） | 1 个（`pi/observability`） | 0 | 1 |
| 文件 | 9 个 | 5 个 | 14 |
| 代码行（估） | 约 480 行（含测试约 800 行） | 约 85 行 | — |

**没有新增任何子目录。** `pi/observability` 这个目录本来就存在（里面只有一个 8 行的 README.md），本方案是往它里面放文件、顺便把那个 README 改对。工具那一层没建新包，`Tracing` 直接进了已有的 `pi/middleware`。

## 决策一览（先看这个）

| # | 决策 | 选择 | 备选与代价 |
|---|---|---|---|
| 1 | 底座 | 自建契约 façade 套在 OpenTelemetry 之上，复用 `go-context-sdk/tracing` | **纯 OTel**：每个包 import `otel/trace`、手工 `End()`、属性键散落各处；换后端确实只要 `otel.SetTracerProvider`，不动核心，但**词汇表的归属**在别人手里，`gen_ai.*` 是 development 状态的约定，随时可能改。**完全自建**（pi.dev 那套回调解耦）：要自己写 NOOP／内存实现两套，`pi` 的每个包都得改签名，收益在本仓库没有兑现场景 |
| 2 | 追踪的切入点 | Provider 装饰器（`TracingProvider`），循环不碰 chat Span | 把 chat Span 塞进 `Loop.complete`：循环要知道 provider 内部的事，换协议/换装饰顺序就得改循环 |
| 3 | 记哪几层 | 四层全记：`pi.run` → `pi.turn` → `chat {model}` / `execute_tool {name}`，压缩另开 `pi.compact_context` | 只记 `pi.run` + `chat`：工具执行看不见，一次运行里"哪一步慢"答不上来。四层的代价是 `pi/loop.go` 要开一个 turn Span（见决策 4）。压缩 Span 开在哪、记什么、失败时什么样，见决策 13-15；被拒绝的工具调用不算，见决策 16 |
| 4 | turn Span 怎么开 | 把 `run` 的循环体抽成 `Loop.step`，整段括在 `WithSpan` 里 | 在 `run` 里手工 `StartSpan`／每处 `return` 前 `End()`：`run` 里有 6 个返回点，漏一个就漏一个没结束的 Span，而且这种漏只在生产环境的后端上看得见。抽 `step` 是纯搬迁，行为不变 |
| 5 | 计量层输出什么 | 只吐原始数字：`LatencyMS`／`TTFTMS`／`PlatformID`／`Model`，并校验 Usage 存在、全部分项非负（见决策 18） | 顺手算成本：`Usage` 里已经有 `CostUSD` 与价格字段（`pi/schema/usage.go:36-40`），填进去很自然。不进这个包的理由是价格表归业务：同一份 token 账，不同客户、不同折扣期算出来的钱不一样，塞进 harness 就得为每个客户改 harness |
| 6 | Usage 缺失或出现脏值时 | 严格：返回 `ErrAIGeneration`（20000），这一次调用失败。"脏"有两种——`Usage` 是 nil，或任一分项为负（见决策 18） | 只告警、继续跑：账丢了静默变成一行 0，负的账静默变成一行负数，计费与预算都建立在这些数字上，比明确失败危险得多。**代价**：自定义 Provider 忘了填 `Usage` 会让整个 Run 失败；测试里的假 Provider 必须填 `Usage` 且各分项非负 |
| 7 | TTFT 写在哪 | 只写 `Usage.TTFTMS` | 同时写 Span 属性（go-reagent 的选择，`tracing_provider.go:98-105`）：要额外定义一个包内私有接口让 tracer 从 meter 里把快照抠出来，两套读法读同一份数，多一层同步。本包 Span 上的 `pi.usage.ttft_ms` 从 `Usage.TTFTMS` 读，同一个来源 |
| 8 | 错误分类 | 用 `pi/error` 的稳定码（`CodeOf`）做 Status 描述与 `error.type` | 用 `err.Error()`：正文里可能有用户数据，而且基数不受控，前端按它分组会炸 |
| 9 | 开关 | 不设开关，靠 OTel 全局 Provider 的 noop 兜底 | `Options.Tracing bool`：得为它写装配分支、为"关掉时 Span 是 nil"写判空，而 OTel 已经保证了"没装 Provider 就什么都不做" |
| 10 | 提示语义（hint） | 不做 | go-reagent 有一个 `hint.go`（`GenerationHint{Phase, Attempt, RequestIndex}` 走 ctx），服务的是"重试的哪一次尝试""压缩触发的还是正常生成的"这类区分。本仓库的重试在工具链上（`pi/middleware/retry.go`）、模型层没有重试；压缩是独立 Span，不靠 hint 也认得出。**没有消费者就不建通道** |
| 11 | Retry 事件（`reagent.retry.*`） | 不做 | 本仓库的 Retry 在工具链上（`pi/middleware/retry.go:42-47`），已经有 `logsdk.Warn` 记每次重试。事件要写在 Span 上，等于给工具 Span 加一串 `AddEvent`——日志已经在说这件事，先不抄第二份 |
| 12 | 新包 | 建 `pi/observability`（4 个文件），工具 handler 进 `pi/middleware` | 把工具 handler 也塞进 `pi/observability`：依赖方向会变成 `observability → pi/tools`，而 `pi/middleware` 已经在依赖 `pi/tools`，两边都能碰 `Execution`。放进 `pi/middleware` 与 `Logging`／`Retry`／`PanicRecovery` 排一排，链的顺序只在一处读得懂 |
| 13 | 压缩 Span 开在哪 | `compactBeforeTurn` 里，尾段套一个局部闭包；`summarize` **一个字不改** | **开在 `summarize` 里**：花同样一行 `WithSpan`，覆盖面却窄一截——`Compact` 落盘失败（`pi/agent.go:299`）落在 Span 之外；`summarize` 也被弄脏，它的职责是"摘要合不合格"，"这次压缩值不值"不是它的事。**完全不开**：摘要请求的 `chat {model}` Span 是装饰器白送的（`summarize` 走 `a.provider`，装配后就是最外层 `TracingProvider`，见决策 2），但那样 trace 上这次 chat 与普通生成长得一样，看不出是压缩，而压缩失败是**静默继续**的（`pi/agent.go:292-297`），trace 是唯一能看见它的地方。**拆成两个方法**：多一个方法，而且拿不到决策 15 那个修法 |
| 14 | 压缩 Span 记什么 | 只记 `pi.compaction.before_tokens` | 再记"摘要多长"：子 chat Span 的 `gen_ai.usage.output_tokens` 已经在说这件事，父 Span 上再抄一遍没有新信息。记 `after_tokens`：`session.Plan` 上**没有**这个字段（`pi/session/compaction.go:186-197` 只有 `TokensBefore`），要拿得改 `pi/session` 或折回后重估一次，出本方案范围 |
| 15 | 压缩失败的轮次，Span 是红的吗 | 是。闭包把错误返回出去让 Span 标红，调用方再把它吞掉（`failure` 记一笔，`return nil, nil`） | 让闭包 `return nil` 静默通过：`summarize` 失败那条路本来就是 `return nil, nil` 吞掉的（设计如此，压不动不算这一轮失败），照抄的话 Span 永远是绿的——**而这个 Span 存在的全部意义就是让人看见"压缩没生效"**，绿的等于白开。**代价**：错误走了两条路（Span 标红 + 局部变量），读代码时要知道 `failure` 是干什么的 |
| 16 | 被拒绝的工具调用有没有 Span | 没有。目标里写的是"每次**进入执行链**的工具调用"，拒绝路径不算（见非目标最后一条） | 把 Span 开在 `Scheduler.Execute` 的查表之前：能看见"模型老在调一个不存在的工具"，但那要 `pi/tools` import `pi/observability`（破坏决策 12），而且用 `execute_tool` 这个名是假的。要做就用另一个 Span 名，单独一轮 |
| 17 | run Span 从哪儿盖到哪儿 | 从 `prepareRunContext` 到会话写入错误的判定，闭包返回 `errors.Join(runErr, a.writeErr)` | **只盖 `loop.run`**（初版写法）：两个漏。①`prepareRunContext` 失败时 trace 上什么都没有——`Run` 报了错却没有 run Span；②`a.writeErr` 在 Span 结束后才查，会话写入失败是红的错误、绿 Span，正好是"部分失败记成成功"。**代价**：多一个 `started` 局部量用来保持"准备失败返回 `nil` 输出、运行中途失败返回非 nil 输出"这个既有形状 |
| 18 | 用量校验查到什么粒度 | 分项全查非负（输入、输出、缓存读写、推理）；`CacheRead+CacheWrite ≤ InputTokens` 与 `Reasoning ≤ OutputTokens` 这两条子集约束**不强制**，只在文档里声明 | **只查输入输出**（初版写法）：负的缓存读/写/推理分项会当成可信用量写进 Span，而 `TotalTokens()` 的正确性（`pi/schema/usage.go:75-85`）正建立在子集约束上。**强制子集约束**：两个内置 provider 确实满足（`anthropic.go:140-145` 的 `InputTokens` 已经把 cache 读写加进去了；`openai.go:376-388` 取的是 `PromptTokens`／`CachedTokens`／`ReasoningTokens`，按 OpenAI 定义都是子集），但第三方 provider 或代理把缓存报在输入之外时会硬失败——那是一个**能跑通的请求被计量层打掉**，代价比记一个可疑数字大 |

## 目标与非目标

目标：

- 一次 `Agent.Run` 在追踪后端上是一棵树：`pi.run` 底下每轮一个 `pi.turn`，`pi.turn` 底下每次模型请求一个 `chat {model}`、每次**进入执行链**的工具调用一个 `execute_tool {name}`；压缩发生时多一个 `pi.compact_context`。
- **`Agent.Run` 返回错误 ⇒ `pi.run` 是红的**（三个入口参数校验除外，见决策 17）。
- 每次成功的模型响应都带得走一份可信的账：输入/输出 token、缓存读写、推理 token、延迟、TTFT、平台、模型、结束原因。
- 失败的 Span 只带稳定错误码，不带错误正文。
- **不接追踪后端也能跑**：OTel 全局 provider 是 noop 时不开 Span、不联网、不建 goroutine。**这不等于零行为变化**——计量与它的严格校验是无条件跑的（决策 6），一个缺 `Usage` 的成功响应会因此变成失败，这一条与有没有追踪后端无关。
- 业务代码不 import `go.opentelemetry.io/otel/*`，只用 `pi/observability` 与 `go-context-sdk/tracing` 暴露的词汇。

非目标（明确不做，附理由）：

- **成本与金额换算**（决策 5）。`Usage.CostUSD`、`InputPriceUSDPerMillionTokens` 等字段留在 `pi/schema` 里由业务层填。本包读写 `Usage` 时**不碰**这几个字段。
- **把 trace 接入某个具体后端**（Jaeger／Tempo／OTLP）。那是一个 main 函数里 `otel.SetTracerProvider` 的事，属于 `cmd/harness` 的装配，不属于库。本包只保证"有 Provider 就上，没有就 noop"。
- **指标（metrics）与日志**。本层的数字以 Span 属性承载；要做直方图（比如 TTFT 分布）是另一个包的事，现在没有消费者。
- **在 Span 上记录提示词、模型输出、工具参数与输出的正文**。只记长度（`pi.tool.arguments_size`、`pi.tool.output_size`）与元数据。正文可能很长，可能带用户数据，落进追踪后端就是另一份需要合规处理的副本。
- **被拒绝的工具调用**（未注册的工具名、参数没过 JSON Schema 校验）。它们在 `Scheduler.Execute` 查表与校验时就返回拒绝事件了（`pi/tools/scheduler.go:74-84`），**不经过中间件链**，所以没有 `execute_tool` Span。它们仍然是一条 `IsError` 的工具结果消息、照常回给模型，只是不在 trace 上。要覆盖它们，Span 得开在 `Scheduler.Execute` 的查表之前——那意味着 `pi/tools` 这个最底层的包要 import `pi/observability`，破坏决策 12 的依赖安排；而且那次调用什么都没执行，挂在 `execute_tool {name}` 名下是假的（名字是有的，`Definition` 是空值，`parallel_safe`／`args_size` 这些属性也填不出来）。真要做，用另一个 Span 名（比如 `pi.tool.rejected`）开在 Scheduler 里，单独一轮决定——本方案先不做。
- **GenerationHint**（决策 10）与 **Retry Event**（决策 11）。
- **子代理、多 Agent 的 Span**。本仓库还没有子代理。
- **采样策略**。归 OTel SDK 的配置。

## 归属：每个名字住在哪

| 名字 | 形态 | 位置 | 为什么在这 |
|---|---|---|---|
| `SpanNameRun` / `SpanNameTurn` / `SpanNameCompaction` | 常量 | `pi/observability/semantics.go` | 跨包使用（`pi` 与 `pi/middleware` 都要拼），字符串字面量只出现一次 |
| `ChatSpanName` / `ToolSpanName` | 函数 | `pi/observability/semantics.go` | 名字带变量（模型名、工具名），常量表达不了 |
| `Attr*` | 常量 | `pi/observability/semantics.go` | 同上，属性键不许散落 |
| `AgentName` | 常量 | `pi/observability/semantics.go` | 进 `gen_ai.agent.name`，本项目只有一个 agent |
| `ClassifyError` / `ErrorFields` | 函数 | `pi/observability/classify.go` | 错误码是 **go-harness 的概念**（`pi/error`），SDK 不认识它，所以这一层在本仓库 |
| `spanError` | 函数（包内） | `pi/observability/classify.go` | 只给 chat Span 那种"包不进函数"的 Span 收尾，唯一调用者在同包内 |
| `TracingProvider` / `NewTracingProvider` / `Wrap` | 结构／函数 | `pi/observability/provider.go` | 装饰 `ai.Provider`；`Wrap` 是唯一一处固定装饰顺序的地方 |
| `UsageMeter` / `NewUsageMeter` | 结构／函数 | `pi/observability/meter.go` | 同上 |
| `Tracing`（工具 handler） | 函数 | `pi/middleware/tracing.go` | 与 `Logging`／`Retry`／`PanicRecovery` 同排，链的顺序在一处读得懂 |

`pi/observability` 的导出面就这些：5 个 Span 名（3 常量 2 函数）、18 个属性键、1 个 `AgentName`、2 个错误辅助（`ClassifyError`／`ErrorFields`）、2 个装饰器 + `Wrap`。`tracingStream`、`meterStream`、`spanError` 不导出——形状与收尾动作都是内部实现。

依赖方向：`pi/observability` → `pi/ai`、`pi/schema`、`pi/error`、`go-context-sdk/tracing`。它**不 import `pi`**（父包），所以 `pi → pi/observability`、`pi/middleware → pi/observability` 都不成环。

## 代码实战

### 1. `pi/observability/semantics.go`（新增）

```go
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
```

### 2. `pi/observability/classify.go`（新增）

```go
package observability

import (
	"strconv"

	contexttracing "github.com/PycMono/go-context-sdk/tracing"
	pierrors "github.com/PycMono/go-harness/pi/error"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// ClassifyError 是 contexttracing.WithSpan 的错误分类器：把错误映射成
// pi/error 的稳定错误码。绝不返回错误正文——正文里可能有用户数据，
// 而且基数不受控，前端按它分组会炸。
func ClassifyError(err error) string {
	if err == nil {
		return ""
	}

	return strconv.Itoa(pierrors.CodeOf(err))
}

// ErrorFields 返回失败操作的 error.type / pi.error.code 属性。未分类的错误
// （CodeOf 返回 0，比如 stdlib 的错误）只留 OTel 标准那一项，值为 "unknown"
// ——写 0 会与"没分类"混在同一个桶里。
func ErrorFields(err error) []contexttracing.Field {
	if err == nil {
		return nil
	}
	code := pierrors.CodeOf(err)
	if code == 0 {
		return []contexttracing.Field{contexttracing.KV(AttrErrorType, "unknown")}
	}

	return []contexttracing.Field{
		contexttracing.KV(AttrErrorType, strconv.Itoa(code)),
		contexttracing.KV(AttrErrorCode, strconv.Itoa(code)),
	}
}

// spanError 收尾一个失败的手工生命周期 Span。目前只有 chat Span 走这条路：
// 它的生命周期跨 Stream 的多次调用，包不进一个函数。函数作用域的 Span 一律
// 走 contexttracing.WithSpan + ClassifyError，不用这个。
//
// 不导出：它是本包内部给"包不进函数的那种 Span"用的收尾动作，唯一的调用者在
// provider.go。导出的代价是 `trace.Span` 这个 otel 类型进入本包的公开 API；
// 真要给外部用，包一层只收 error 的形状而不是把这个签名递出去。
func spanError(span trace.Span, err error) {
	if err == nil {
		return
	}
	span.SetStatus(codes.Error, ClassifyError(err))
	span.SetAttributes(ErrorFields(err)...)
}
```

### 3. `pi/observability/meter.go`（新增）

```go
package observability

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/PycMono/go-harness/pi/ai"
	pierrors "github.com/PycMono/go-harness/pi/error"
	"github.com/PycMono/go-harness/pi/schema"
)

// UsageMeter 给每次成功的模型响应补上计量：延迟、TTFT 与归属（平台、模型）。
// 它不做金额换算——token → 美元要按业务自己的价格表算，本包不接触任何金额，
// 也不读写 Usage 里的价格与成本字段。
//
// 严格模式：响应缺 Usage、任一用量分项为负、或延迟为负，都返回 ErrAIGeneration。
// 宁可明确失败，也不要让"账丢了"静默变成一行 0、"账是脏的"静默变成一行负数
// ——计费与预算都建立在这些数字上。分项查全部五个（输入、输出、缓存读写、
// 推理），子集约束不查，理由见 negativeTokens。
type UsageMeter struct {
	next       ai.Provider
	platformID string
	model      string
	now        func() time.Time
}

// NewUsageMeter 构造计量装饰器。platformID 与 model 都会写进 Usage 的归属
// 字段，两者都不能为空——它们的用途正是"按平台、按模型对账"。
func NewUsageMeter(next ai.Provider, platformID, model string) (*UsageMeter, error) {
	platformID = strings.TrimSpace(platformID)
	model = strings.TrimSpace(model)
	switch {
	case platformID == "":
		return nil, errors.New("usage meter: platform id is required")
	case model == "":
		return nil, errors.New("usage meter: model is required")
	}

	return &UsageMeter{next: next, platformID: platformID, model: model, now: time.Now}, nil
}

// Stream 包一层流。
//
// 计时口径：startedAt 取在**调用下层 Stream 之前**。下层的 Stream 不是纯粹建对象
// ——两个 provider 都在里面就把请求发出去了（`pi/ai/providers/anthropic.go` 的
// `p.client.Messages.NewStreaming(ctx, params)` 就在 Stream 里），先调下层再取
// 时刻会把建连与首包前的时间整段漏掉。反过来写成先取时刻，量到的是"从进入装饰器
// 到 Result 返回"，包含请求构造、网络、以及调用方读完整个流的时间，也就是这一轮
// 模型调用的全部墙钟；TTFT 是其中的一段（到首个非空文本增量为止），所以恒有
// TTFT ≤ Latency。
func (m *UsageMeter) Stream(
	ctx context.Context,
	messages schema.Messages,
	tools schema.ToolDefinitions,
) ai.Stream {
	startedAt := m.now()

	return &meterStream{
		next:      m.next.Stream(ctx, messages, tools),
		meter:     m,
		startedAt: startedAt,
	}
}

type meterStream struct {
	next      ai.Stream
	meter     *UsageMeter
	startedAt time.Time

	current schema.StreamEvent
	// ttft 是首个非空文本增量的延迟快照；hasTTFT 为 false 表示这一轮没有
	// 文本增量（纯工具调用响应），TTFTMS 保持 nil。
	ttft    time.Duration
	hasTTFT bool
	// resolved 保证 measure 只跑一次：Result 可能被调用多次（下游的装饰器
	// 与循环都可能读它）。
	resolved bool
	response *schema.AssistantMessage
	err      error
}

func (s *meterStream) Next() bool {
	if !s.next.Next() {
		return false
	}
	s.current = s.next.Current()
	if !s.hasTTFT && s.current.Type == schema.StreamEventTextDelta && s.current.TextDelta != "" {
		s.ttft = s.meter.now().Sub(s.startedAt)
		s.hasTTFT = true
	}

	return true
}

func (s *meterStream) Current() schema.StreamEvent { return s.current }

func (s *meterStream) Result() (*schema.AssistantMessage, error) {
	if s.resolved {
		return s.response, s.err
	}
	s.resolved = true
	response, err := s.next.Result()
	s.response, s.err = s.meter.measure(s.startedAt, response, err)
	// TTFT 与延迟是两笔账：延迟在 measure 里补（它只看起止时刻），TTFT 要
	// 等流读到第一个文本增量才知道，所以在这里补。不足 1ms 记 0，不是 nil
	// ——nil 的含义是"没有文本增量"。
	if s.err == nil && s.hasTTFT {
		ttftMS := s.ttft.Milliseconds()
		s.response.Usage.TTFTMS = &ttftMS
	}

	return s.response, s.err
}

func (s *meterStream) Close() error { return s.next.Close() }

// measure 校验并补上延迟与归属。失败的请求原样返回——它的账不可信，不该
// 被当成一次计量。
func (m *UsageMeter) measure(
	startedAt time.Time,
	response *schema.AssistantMessage,
	err error,
) (*schema.AssistantMessage, error) {
	latencyMS := m.now().Sub(startedAt).Milliseconds()
	if err != nil {
		return response, err
	}
	if response == nil || response.Usage == nil {
		return nil, pierrors.ErrAIGeneration.Wrap(fmt.Errorf(
			"provider %s/%s 的响应缺少用量", m.platformID, m.model))
	}
	if negativeTokens(response.Usage) || latencyMS < 0 {
		return nil, pierrors.ErrAIGeneration.Wrap(fmt.Errorf(
			"provider %s/%s 的用量为负", m.platformID, m.model))
	}

	// 复制一份再改：下层 Provider 可能把 Usage 指针复用在下一条消息上，
	// 就地改会连带改到别人手里的那一份。
	measured := *response
	usage := *response.Usage
	usage.LatencyMS = latencyMS
	usage.PlatformID = m.platformID
	usage.Model = m.model
	measured.Usage = &usage

	return &measured, nil
}

// negativeTokens 检查全部用量分项非负。只查输入输出是不够的：缓存读写与推理是
// 各自父项的**子集**（`pi/schema/usage.go:53-58`），负的子集同样是脏数据，放过去
// 会当成可信账写进 Span。
//
// 子集约束本身（CacheRead+CacheWrite ≤ InputTokens、Reasoning ≤ OutputTokens）不
// 在这里强制。两个内置 provider 都满足它，但那是**映射约定**，不是协议保证：第三方
// provider 或代理把缓存报在输入之外时，强制会把一个本来跑得通的请求打掉，代价比
// 记一个可疑数字大。约定写在 pi/schema 的字段注释里，不在这里收税。
func negativeTokens(usage *schema.Usage) bool {
	return usage.InputTokens < 0 ||
		usage.OutputTokens < 0 ||
		usage.CacheReadTokens < 0 ||
		usage.CacheWriteTokens < 0 ||
		usage.ReasoningTokens < 0
}
```

### 4. `pi/observability/provider.go`（新增）

```go
package observability

import (
	"context"
	"errors"
	"time"

	contexttracing "github.com/PycMono/go-context-sdk/tracing"
	"github.com/PycMono/go-harness/pi/ai"
	"github.com/PycMono/go-harness/pi/schema"
	"go.opentelemetry.io/otel/trace"
)

// errStreamAbandoned 表示流在 Result 之前被放弃：调用方读了几个事件就 Close，
// 或者上层 ctx 先取消了。只用于把 Span 标成失败，正文不进 Span。
var errStreamAbandoned = errors.New("observability: provider stream closed before result")

// TracingProvider 给每次物理模型请求开一个 chat {model} Span。它套在
// UsageMeter 外面，装饰顺序固定为 Loop → TracingProvider → UsageMeter →
// 原始 Provider：Span 上的 token 属性来自内层计量层补好的 Usage，顺序反了
// 就读不到。
//
// provider 是 gen_ai.provider.name 的值，取协议名（openai／anthropic）。
type TracingProvider struct {
	next     ai.Provider
	provider string
	model    string
}

// NewTracingProvider 包装 next。
func NewTracingProvider(next ai.Provider, provider, model string) *TracingProvider {
	return &TracingProvider{next: next, provider: provider, model: model}
}

// Wrap 给原始 Provider 套上本项目的两层观测装饰，返回最外层。装饰顺序只写在
// 这里：调用方拿到的是可以直接交给 NewLoop 与 Agent 的那一个。
func Wrap(provider ai.Provider, platformID, model, protocol string) (ai.Provider, error) {
	meter, err := NewUsageMeter(provider, platformID, model)
	if err != nil {
		return nil, err
	}

	return NewTracingProvider(meter, protocol, model), nil
}

// Stream 开 Span 并把下层流套起来。Span 一直活到 Result 或 Close——不能在
// Stream 返回时就结束，否则量到的是"连接建立"而不是"这一轮生成"。
func (p *TracingProvider) Stream(
	ctx context.Context,
	messages schema.Messages,
	tools schema.ToolDefinitions,
) ai.Stream {
	spanCtx, span := contexttracing.StartSpan(ctx, ChatSpanName(p.model),
		trace.WithSpanKind(trace.SpanKindClient))
	contexttracing.WithKV(spanCtx,
		contexttracing.OperationName("chat"),
		contexttracing.ProviderName(p.provider),
		contexttracing.RequestModel(p.model),
	)

	return &tracingStream{
		ctx:  spanCtx,
		span: span,
		next: p.next.Stream(spanCtx, messages, tools),
	}
}

type tracingStream struct {
	ctx  context.Context
	span trace.Span
	next ai.Stream

	current schema.StreamEvent
	// chunks 是事件条数。它是唯一能区分"一次吐一大段"与"挤了几百次"的信号，
	// 也是判断 TTFT 有没有意义的旁证（一个块都没有就没有 TTFT）。
	chunks   int
	resolved bool
}

func (s *tracingStream) Next() bool {
	if !s.next.Next() {
		return false
	}
	s.current = s.next.Current()
	s.chunks++

	return true
}

func (s *tracingStream) Current() schema.StreamEvent { return s.current }

func (s *tracingStream) Result() (*schema.AssistantMessage, error) {
	message, err := s.next.Result()
	if !s.resolved {
		s.resolved = true
		s.finish(message, err)
	}

	return message, err
}

// Close 关下层流；没走过 Result 的话（调用方提前放弃，或 ctx 取消）在这里把
// Span 标成失败并结束。resolved 保证 Span 只结束一次——normal、错误、取消、
// 提前 Close、重复 Result 五条路都走同一个出口。
func (s *tracingStream) Close() error {
	err := s.next.Close()
	if !s.resolved {
		s.resolved = true
		abandoned := s.ctx.Err()
		if abandoned == nil {
			abandoned = errStreamAbandoned
		}
		s.finish(nil, abandoned)
	}

	return err
}

// finish 写这一轮的属性并结束 Span。
func (s *tracingStream) finish(message *schema.AssistantMessage, err error) {
	defer s.span.End()

	contexttracing.WithKV(s.ctx, contexttracing.KV(AttrStreamChunkCount, s.chunks))
	if err != nil {
		// 失败的请求只留 Span、耗时与错误分类，不写 token：这一轮的账不可信。
		spanError(s.span, err)

		return
	}
	if message == nil || message.Usage == nil {
		// 计量层在链上时不会让 nil Usage 走到这里；自定义 Provider 绕过计量层
		// 时可能。没有账就不写账，这不是错误。
		return
	}

	usage := message.Usage
	fields := []contexttracing.Field{
		contexttracing.InputTokens(int(usage.InputTokens)),
		contexttracing.OutputTokens(int(usage.OutputTokens)),
		contexttracing.KV(AttrUsageCacheRead, usage.CacheReadTokens),
		contexttracing.KV(AttrUsageCacheWrite, usage.CacheWriteTokens),
		contexttracing.KV(AttrUsageReasoning, usage.ReasoningTokens),
		contexttracing.KV(AttrUsagePlatformID, usage.PlatformID),
		contexttracing.KV(AttrUsageLatencyMS, usage.LatencyMS),
	}
	if usage.Model != "" {
		fields = append(fields, contexttracing.ResponseModel(usage.Model))
	}
	if usage.TTFTMS != nil {
		fields = append(fields, contexttracing.KV(AttrUsageTTFTMS, *usage.TTFTMS))
	}
	if message.FinishReason != "" {
		fields = append(fields, contexttracing.FinishReasons(string(message.FinishReason)))
	}
	contexttracing.WithKV(s.ctx, fields...)
}
```

### 5. `pi/middleware/tracing.go`（新增）

```go
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
```

### 6. `pi/middleware/defaults.go`（改）

只改顺序与注释：

```go
package middleware

import "github.com/PycMono/go-harness/pi/tools"

// Defaults 返回默认中间件集合，切片顺序即执行顺序：
// Tracing 最外层包住整条链与真实工具调用，一个工具调用一个 Span（重试的
// 每一次尝试在同一个 Span 里，次数看 Logging 与 Retry 的日志）；Logging
// 记录开始与结束；RetryTransient 居中，重试只重跑链上更靠内的部分
// （PanicRecovery 与真实工具调用），结束日志记录的是最终状态；PanicRecovery
// 最靠内，兜住每次尝试中真实工具执行的 panic，恢复后 Retry 拿到 ErrToolPanic，
// 不命中 ErrToolTimeout，不会重试。
// 未注册的 Tool 在 pi/tools 的 Runtime 入口即返回，不经过本链。
func Defaults() []tools.Handler {
	return []tools.Handler{
		Tracing,
		Logging,
		RetryTransient(),
		PanicRecovery,
	}
}
```

### 7. `pi/loop.go`（改）

把 `run` 的循环体抽成 `step`，整段括进 turn Span。**这是本方案唯一的结构改动**，其余是纯搬迁。

```go
// run 执行一次完整的 agent 循环：调用模型 → 模型要求调用工具就执行并把结果
// 写回消息序列 → 带着工具结果再次调用模型，直到模型不再要求调用工具为止。
// 返回的是本轮运行产生的完整消息序列（上下文 + 模型消息 + 工具结果）；即使
// 中途出错，返回值也包含已经产生的部分，便于上层排查。
func (l *Loop) run(ctx context.Context, runContext *Context) (schema.Messages, error) {
	// 三段按 CurrentInputIndex 切：ContextBuilder.Build 的排布是"历史 → 本轮输入 →
	// 系统提示词 → 上下文块"，所以这个下标正好是"历史"与"本轮"的分界。校验越界不是
	// 形式主义：Context 是导出类型，调用方可以自己造一个塞进 run。
	split := runContext.CurrentInputIndex
	if split < 0 || split > len(runContext.Messages) {
		return nil, pierrors.ErrInternal.Wrap(fmt.Errorf(
			"上下文的本轮输入下标 %d 越界（共 %d 条消息）", split, len(runContext.Messages)))
	}
	state := &runState{
		head:           append(schema.Messages(nil), runContext.Messages[:split]...),
		tail:           append(schema.Messages(nil), runContext.Messages[split:]...),
		availableTools: append(schema.ToolDefinitions(nil), runContext.Tools...),
	}

	for turn := 0; ; turn++ {
		// 轮首的两处退出判定落在轮与轮之间、不属于任何一轮：取消与超预算都
		// 在 turn Span 之外，Span 里只有真正跑起来的那一轮的工作。
		if err := ctx.Err(); err != nil {
			return state.messages(), pierrors.ErrCanceled.Wrap(fmt.Errorf("agent 运行已取消: %w", err))
		}
		if turn >= l.maxTurns {
			return state.messages(), pierrors.ErrRunLimitExceeded.Wrap(
				fmt.Errorf("连续 %d 轮模型调用都要求执行工具，已终止运行", l.maxTurns))
		}

		stop, err := l.step(ctx, state, turn)
		if err != nil {
			return state.messages(), err
		}
		if stop {
			// 模型不再要求调用工具，运行正常结束，消息序列照常返回。
			return state.messages(), nil
		}
	}
}

// step 跑一轮：交付插话 → 压缩 → 调模型 → 写回消息 → 执行工具 → 循环检测。
// 返回 stop 表示模型这一轮不再要求调用工具，运行到此结束。
//
// 整段括在一个 turn Span 里。抽成函数是为了让 Span 有一个包得住整轮的进出点：
// run 里有六个返回点，手工 End 会漏；这里只有一个出口，Span 一定只结束一次。
// 搬迁之外没有别处改动，消息序列的推进顺序与原来一字不差。
func (l *Loop) step(ctx context.Context, state *runState, turn int) (stop bool, err error) {
	err = contexttracing.WithSpan(ctx, observability.SpanNameTurn,
		func(ctx context.Context) error {
			contexttracing.WithKV(ctx,
				contexttracing.KV(observability.AttrTurnIndex, turn),
				contexttracing.KV(observability.AttrToolsAvailable, len(state.availableTools)),
			)

			// 排队中的插话先交付，再让压缩做窗口判定。反过来（先压缩后交付）的话，
			// 这批消息不在 turn.extra() 里（pi/agent.go 的 compactBeforeTurn 用它
			// 判要不要压），压缩既不会因为它们而触发、也不会在压完之后把它们的
			// 体量算进去——写得多就直接把请求顶过窗口，运行被 provider 的 20003
			// 打掉。
			//
			// 运行开始前写入的消息也在这里交付：循环第一次进来就走到这里。
			l.deliver(l.drainSteering(), state)

			// 每轮调用模型之前给上层一次机会改写历史段（压缩就挂在这里）。返回 nil
			// 表示不改写；报错则带上已经产生的部分退出。
			if l.beforeTurn != nil {
				head, err := l.beforeTurn(ctx, state.turn())
				if err != nil {
					return err
				}
				if head != nil {
					state.head = head
				}
			}

			// 压缩期间写进来的消息在这里补捞：压缩要调一次模型，慢的话是几秒，
			// 那段时间写入的不该等到下一轮。pi.dev 在 agent-loop.ts:203 单独留了
			// 一个"压缩完再捞一次"的点，理由相同。
			l.deliver(l.drainSteering(), state)

			message, err := l.complete(ctx, state)
			if err != nil {
				return err
			}
			// Provider 的 Stream.Result 已经把返回类型收窄为助手消息，Loop 不再需要
			// 对通用 Message 做运行时类型断言。
			assistant := message
			// 模型消息先入列：工具结果必须紧跟在发起调用的那条助手消息之后，
			// 两家协议都按这个顺序还原上下文。
			state.produced = append(state.produced, message)
			l.observe(message)

			if len(assistant.ToolCalls) == 0 {
				stop = true

				return nil
			}
			if l.scheduler == nil {
				return pierrors.ErrInternal.Wrap(fmt.Errorf(
					"模型请求调用工具 %q，但本轮运行没有接入工具调度器", assistant.ToolCalls[0].Name))
			}

			contexttracing.WithKV(ctx,
				contexttracing.KV(observability.AttrToolsRequested, len(assistant.ToolCalls)))

			results, err := l.scheduler.ExecuteBatch(ctx, assistant.ToolCalls)
			if err != nil {
				return err
			}
			toolResults := make(schema.Messages, 0, len(results))
			for index := range results {
				// 工具自身的失败同样作为一条 IsError 的工具消息回给模型，
				// 让模型自己决定重试还是换条路；只有调度层面的失败才中断。
				result, err := results[index].ResultMessage()
				if err != nil {
					// 结束事件缺身份是调度器坏了，不是工具失败：工具失败会以
					// IsError 事件表达，不会走到这里。
					return pierrors.ErrInternal.Wrap(err)
				}
				// 追加的是消息值本身：取一条复用变量的地址会让序列里的每条工具结果
				// 都指向同一份内存。
				state.produced = append(state.produced, result)
				l.observe(result)
				toolResults = append(toolResults, result)
			}

			// 每轮的工具结果写回之后给上层一次机会判定这是不是循环。判定放在这里
			// 而不是每轮开头：只有"还要开下一轮"的那些轮才有这个问题，模型不再要
			// 工具时上面已经返回了。返回错误就是收尾，与上面 maxTurns 同一种形状
			// ——带消息返回，不中断正在跑的东西。
			if l.afterTurn != nil {
				if err = l.afterTurn(ctx, TurnReport{
					Index:       turn,
					Message:     assistant,
					ToolResults: toolResults,
				}); err != nil {
					return err
				}
			}

			return nil
		},
		contexttracing.WithErrorClassifier(observability.ClassifyError),
	)

	return stop, err
}
```

`pi/loop.go` 的 import 增加两行：

```go
	contexttracing "github.com/PycMono/go-context-sdk/tracing"
	"github.com/PycMono/go-harness/pi/observability"
```

### 8. `pi/agent.go`（改）

三处：装配处套装饰器、`Run` 开 run Span、`summarize` 开压缩 Span。

**(a) 装配**（`newAgent`）——先加一个 `ProviderOptions` 判空（`newAgent` 也被测试直接调，判空原本只在 `NewAgent` 里），再套装饰器，然后把原来的 `provider` 全部换成 `traced`：

```go
func newAgent(ctx context.Context, provider ai.Provider, opts *Options) (*Agent, error) {
	if opts.Session == nil {
		return nil, pierrors.ErrInitialization.Wrap(errors.New("session must not be nil"))
	}
	// 观测装配要用 ProviderOptions 里的归属信息（平台、模型、协议），所以
	// 这里也判一次空。NewAgent 已经判过；newAgent 被测试直接调用，判空不能
	// 只留在上层。
	if opts.ProviderOptions == nil {
		return nil, pierrors.ErrInitialization.Wrap(errors.New("provider options must not be nil"))
	}

	// 观测装配：装饰顺序固定在 observability.Wrap 里，拿到的是最外层。
	// 没装 OTel Provider 时两层都是透传，代价是两次函数调用。
	traced, err := observability.Wrap(
		provider,
		opts.ProviderOptions.ID,
		opts.ProviderOptions.Model,
		string(opts.ProviderOptions.Protocol),
	)
	if err != nil {
		return nil, pierrors.ErrInitialization.Wrap(err)
	}

	// 获取 tools 下面的 4 个默认的工具
	newTools := impl.NewDefaultTools(opts.WorkDir)
	if len(newTools) > 0 {
		opts.Tools = append(opts.Tools, newTools...) //支持外部 tools 传入，自定义一些工具
	}

	// 注册工具
	registry, err := tools.Register(opts.Tools)
	if err != nil {
		return nil, err
	}

	// 注入扩展
	runtime, err := extension.NewRuntime(opts.Extensions)
	if err != nil {
		return nil, err
	}
	if err := runtime.Register(ctx, registry); err != nil {
		return nil, err
	}
	registry.Freeze()

	maxParallel := opts.MaxParallel
	if maxParallel <= 0 {
		maxParallel = defaultMaxParallel
	}

	agent := &Agent{
		contextBuilder: NewContextBuilder(opts.WorkDir),
		session:        opts.Session,
		provider:       traced,
		window:         session.NewWindow(int64(opts.ProviderOptions.ContextWindow)),
		onCompaction:   opts.CompactionObserver,
		guard:          newLoopGuard(opts.LoopGuardTurns),
		runtime:        runtime,
	}
	// 会话写入接在循环的逐条消息观察者上：模型消息与工具结果产生的当口就落盘，
	// 而不是等 Run 结束后批量补写。压缩挂在每轮的前置钩子上：它要调模型，而
	// agent 是唯一持有 provider 的地方。循环检测挂在轮末的后置钩子上：它要看
	// 的现场（这一轮的调用与结果）只有轮末才齐。
	//
	// 装配不做条件判断：关不关由 guard.limit 的值决定，observe 自己吞掉。
	agent.loop = NewLoop(
		traced,
		WithScheduler(tools.NewScheduler(registry, maxParallel, opts.Observer, middleware.Defaults()...)),
		WithTextObserver(opts.TextObserver),
		WithMaxTurns(opts.MaxTurns),
		WithBeforeTurn(agent.compactBeforeTurn),
		WithAfterTurn(func(_ context.Context, report TurnReport) error {
			return agent.guard.observe(report)
		}),
		WithMessageObserver(agent.recordMessage),
	)

	return agent, nil
}
```

注意 `provider` 字段也要跟着换成 `traced`：`summarize` 走 `a.provider`，用最外层才能让压缩里的模型调用也留下 chat Span 与用量。

**(b) `Run` 开 run Span**：

```go
func (a *Agent) Run(ctx context.Context, input *RunInput) (*RunOutput, error) {
	if a.closed.Load() {
		return nil, pierrors.ErrClosed
	}
	if input == nil {
		return nil, pierrors.ErrRequestInvalid.Wrap(errors.New("run input must not be nil"))
	}
	if err := input.Validate(); err != nil {
		return nil, err
	}

	// 一个 Run 一轮账：上一轮的写入错误、本轮起点与循环检测的计数都不带进
	// 这一轮。检测计数必须在这里清——它记的是"连续几轮"，跨 Run 留着就会把
	// 两次不相干的运行接成一段。这三行是账本不是运行，留在 Span 之外。
	a.writeErr = nil
	a.headLeafID = ""
	a.guard.reset()

	// 整次运行括一个 Span：准备上下文、模型调用、工具执行、压缩都是它的子
	// Span，一次运行在追踪后端上是一棵树。属性在闭包里写而不是起始选项里写，
	// 是为了让 agent.go 不 import otel 的 trace 包。
	//
	// 闭包的返回值与 Run 的返回值是同一件事：Run 返回错误 ⇔ 这个 Span 是红的。
	// 唯一在 Span 之外的是上面那三个入口校验——那是调用方写错了参数，这一次
	// 运行根本没开始，不该在 trace 上留一个 Run。
	var (
		messages schema.Messages
		started  bool
	)
	err := contexttracing.WithSpan(ctx, observability.SpanNameRun, func(ctx context.Context) error {
		contexttracing.WithKV(ctx,
			contexttracing.OperationName("invoke_agent"),
			contexttracing.KV(observability.AttrGenAIAgentName, observability.AgentName),
		)

		// 准备上下文也会失败（本轮输入拼不出来、历史读不出来），所以它进 Span：
		// 不进的话 Run 报了错、trace 上却什么都没有。
		runContext, err := a.prepareRunContext(ctx, input)
		if err != nil {
			return err
		}
		started = true

		produced, runErr := a.loop.run(ctx, runContext)
		messages = produced

		// 会话写入失败比模型出错更隐蔽：消息可能已经发给模型了，但盘上没有，
		// 下一轮重建出来的历史就缺一段。它不算"运行成功"，Span 也要跟着红。
		//
		// 两个错可能同时发生：运行错在前、写入错在后，两个都带上——只报运行错会
		// 吞掉"盘上历史缺了一段"，只报写入错会吞掉"模型为什么停"。CodeOf 走
		// errors.As、先左后右，所以拿到的仍是运行错的码；errors.Is 能同时找到
		// 写入失败的原因。
		return errors.Join(runErr, a.writeErr)
	},
		contexttracing.WithErrorClassifier(observability.ClassifyError),
	)
	// started 是准备阶段的成败：准备失败时没有消息序列可返回，与原来一样返回
	// nil 输出；运行中途失败时返回已经产生的部分，调用方可以据此看到模型跑到
	// 哪一步才出的问题。
	if !started {
		return nil, err
	}

	return &RunOutput{message: messages}, err
}
```

**(c) `compactBeforeTurn` 开压缩 Span——`summarize` 一个字不改**

先说清一件事：**摘要请求的 `chat {model}` Span 不需要在这里写任何代码**。`summarize` 走的是 `a.provider`，而 `a.provider` 在装配处已经换成了最外层的 `TracingProvider`（(a) 那条），装饰器自己会开 Span。所以"压缩时的模型调用有 trace"这一点是装饰器白送的（决策 13）。

压缩 Span 要补的是装饰器看不见的那两件事：**这次是压缩、压之前上下文多大**，以及 `Compact` 落盘失败时那个错误该落在哪个 Span 上——它现在落在任何 Span 之外。

所以 Span 不开在 `summarize` 里，开在**调用它的人**那里。但不能开在函数顶上：`compactBeforeTurn` 每轮都被调，第一个检查 `OverLine` 在绝大多数轮次上直接返回，Span 开在顶上等于每轮留一个空的 `pi.compact_context`。**开在两个提前返回之后**：

```go
// compactBeforeTurn 是本轮的前置钩子：越过触发线就压一次，返回替换后的历史段。
//
// 判定用会话算出来的大小，不是自己把请求摊平了估：折叠后的历史里哪些 usage 还
// 作数只有会话知道（边界之前的账会把已经压掉的上下文重新算回来），本轮的 Tail 与
// Produced 作为 extra 一起递进去。
func (a *Agent) compactBeforeTurn(ctx context.Context, turn Turn) (schema.Messages, error) {
	if !a.window.OverLine(a.session.ContextTokens(a.headLeafID, turn.extra())) {
		return nil, nil
	}

	plan, ok := a.session.PlanCompaction(a.window, a.headLeafID)
	if !ok {
		// 越过触发线但计划不成立：路径太短、末尾已经是边界、或本轮起点指不着。
		// 不是错误。
		return nil, nil
	}

	// 两个提前返回都在压缩 Span 之外：那两条路上什么都没压，不该在 trace 上留
	// 一个空 Span。
	//
	// failure 是"这次压不动"那一条路的错误。它既走闭包的返回值（让 Span 标红）
	// 又走这个局部变量（给下面吞掉）——压不动不算这一轮失败，但 Span 必须红，
	// 否则这个 Span 就永远是绿的，而它存在的全部意义正是让人看见"压缩没生效"。
	var failure error
	err := contexttracing.WithSpan(ctx, observability.SpanNameCompaction, func(ctx context.Context) error {
		contexttracing.WithKV(ctx,
			contexttracing.KV(observability.AttrCompactionBeforeTokens, plan.TokensBefore))

		summary, err := a.summarize(ctx, plan)
		if err != nil {
			failure = err

			return err
		}

		// 落盘失败不吞：盘上的历史从此缺一块，必须透出。
		return a.session.Compact(plan, summary)
	},
		contexttracing.WithErrorClassifier(observability.ClassifyError),
	)

	if failure != nil {
		// 压不动不算这一轮失败：历史还是完整的，只是继续贴着窗口跑。真溢出了
		// 让 provider 去报（20003），比在这里把客户的这一轮打断好。
		a.report(CompactionEvent{TokensBefore: plan.TokensBefore, Err: failure})

		return nil, nil
	}
	if err != nil {
		// 落盘失败与会话写入失败同类：盘上的历史从此缺一块，必须透出。
		return nil, err
	}
	a.report(CompactionEvent{TokensBefore: plan.TokensBefore})

	// 新历史从盘上折回来，不由这里拼：折叠规则只此一份，也就不会出现"计划里的
	// 保留段与读侧折叠不一致"这种要靠人盯的分歧。上界卡在本轮开始的位置——本轮
	// 已经产生的消息在盘上位于新边界之前，不挡住就会既进历史段、又进本轮的部分。
	return a.session.MessagesAt(a.headLeafID), nil
}
```

`summarize` 保持原样，**一行都不动**——它现在的样子就已经是对的。只在注释里补一句它为什么不碰观测：

```go
// summarize 用当前模型生成一段摘要。这是一次独立、不带工具的调用：不进循环、
// 不发工具描述、不接文本观察者（摘要不该流到用户屏幕上）。
//
// 它不碰观测：这次模型调用的 chat Span 由装配处的 TracingProvider 开（装饰器
// 白送的），它自己只负责"摘要合不合格"。压缩的账（压前多大、成没成）由调用者
// compactBeforeTurn 记。
func (a *Agent) summarize(ctx context.Context, plan *session.Plan) (string, error) {
	// ……函数体一字不改……
}
```

`pi/agent.go` 的 import 增加两行：

```go
	contexttracing "github.com/PycMono/go-context-sdk/tracing"
	"github.com/PycMono/go-harness/pi/observability"
```

### 9. `go.mod`（改）

```go
require (
	github.com/PycMono/go-context-sdk v1.0.4
	...
	go.opentelemetry.io/otel v1.45.0
	go.opentelemetry.io/otel/trace v1.45.0
)
```

两个 otel 包现在从 `// indirect` 转成直接依赖（`go mod tidy` 会自动搬）。

`go-context-sdk` 的**最低**版本要求，是包含这两个文件的第一个**已发布** tag：

- `tracing/scope.go` —— `WithSpan`（:55）与三个选项构造函数 `WithErrorClassifier`（:27）、`WithStartOptions`（:33）、`WithFinalSpanName`（:39）
- `tracing/preset.go` —— `Field`（:17）、`KV`（:21）、`WithKV`（:62）、`OperationName`（:81）、`ToolName`（:116）

**落到 v1.0.4**：`sdk/go-context-sdk` 的本地 HEAD（`15d7db8`，提交信息 "feat: add function-scoped span lifecycle helper"）就是远端 tag `v1.0.4` 指向的那个提交，两个文件都在里面。也就是**这个版本已经在远端发布了，不需要 `replace`，也不需要等新 tag**。

落地时的动作只有一步：

```bash
go get github.com/PycMono/go-context-sdk@v1.0.4
go mod tidy
```

**不许把本地 `replace` 提交进主干**。本机之所以能编译，靠的是 `sdk/go-context-sdk` 这个同仓目录；一旦 `go mod edit -replace ...=/Users/allen/...` 进了 `go.mod`，CI 与其他开发者的机器上都指不到那个绝对路径，构建当场断。要让依赖可复现，只有一条：用远端 tag（现在就是 `v1.0.4`），或者先把本地那两个文件推上去打一个新 tag 再用它。落地前先跑一次 `go list -m -versions github.com/PycMono/go-context-sdk` 确认 `v1.0.4` 在远端可见，再动 `go.mod`。

### 10. Span 树长什么样

一次 Run、两轮、第二轮的工具有重试：

```text
pi.run                                   （agent.go，Run）
├── pi.turn  index=0  tools_available=5
│   └── chat claude-sonnet-4-...         （provider.go，SpanKind=Client）
│         in=1200 out=80 latency_ms=1800 ttft_ms=420 finish=stop
├── pi.turn  index=1  tools_available=5  tools_requested=2
│   ├── chat claude-sonnet-4-...
│   │     in=2400 out=150 latency_ms=2100 ttft_ms=380 finish=tool_use
│   ├── execute_tool read_file   is_error=false parallel_safe=true args=48 output=812
│   └── execute_tool run_tests   is_error=false parallel_safe=false args=22 output=1904
└── pi.turn  index=2  tools_available=5
    ├── pi.compact_context  before_tokens=180000
    │   └── chat claude-sonnet-4-...        （摘要请求，不带工具）
    │         in=180500 out=1200 latency_ms=9600 ttft_ms=1400 finish=stop
    └── chat claude-sonnet-4-...            （压缩后的正式请求）
```

三处值得对照：

- 压缩 Span 挂在**它发生的那一轮**下面，且在那一轮的 chat 之前：`compactBeforeTurn` 由 `step` 通过 `beforeTurn` 钩子调用，它的 ctx 就是那一轮的 turn Span ctx。
- 摘要请求自己又是一个 chat Span，**靠父 Span 与正式请求区分**——这正是决策 13 里"完全不开压缩 Span 不行"的根据：没有这层父 Span，两个 `chat {model}` 就是同一轮下的兄弟，看不出哪个是压缩。
- 摘要有多长不用压缩 Span 记：子 chat Span 的 `out=1200` 就是（决策 14）。

压缩**失败**那一轮的形状（决策 15）：`pi.compact_context` 是红的，状态描述是稳定码 50002（`ErrCompactionFailed`），下面那个 chat Span 也是红的，而**这一轮的正式 chat 照常绿着跑**——因为运行没有被打断。这就是"压缩一直在失败、每轮都贴着窗口跑"在 trace 上的样子。

## 测试

前三个是包内单元测试（各一个文件），第四个是 agent 层的装配测试。

### `pi/observability/meter_test.go`

用假 Provider 与 OTel 的 **内存 Span 导出器**（`tracetest`／`sdktrace.NewTracerProvider` + `tracetest.NewSpanRecorder`）验证：

1. 成功响应被补上 `LatencyMS`、`PlatformID`、`Model`，且**不改原消息**（原 `Usage` 指针上的字段不变）——这是"复制一份再改"那条的回归测试。
2. 首个非空文本增量后 `TTFTMS` 非 nil 且 `>= 0`；纯工具调用响应（没有文本增量）时 `TTFTMS` 为 nil。
3. 响应缺 `Usage` → `pierrors.CodeOf(err) == 20000`；**每一个**分项为负（输入、输出、缓存读、缓存写、推理）都单独一条 → 同样 20000。这一组是决策 18 的回归测试：初版只查输入输出的写法，在缓存读/写与推理那三条上会漏过去（表驱动，一行一个分项）。
4. 子集约束**不**触发失败：一个 `CacheReadTokens` 大于输入 token 的响应照常成功（这是决策 18 有意放行的形态，得有一条测试钉住"我们确实没查它"，否则将来有人顺手补上校验不会有测试拦）。

```go
// 假 now：每次调用 +10ms，让延迟可预测。
func fakeClock() func() time.Time {
	base := time.Unix(0, 0)
	var calls int
	return func() time.Time {
		calls++
		return base.Add(time.Duration(calls) * 10 * time.Millisecond)
	}
}
```

第 3 条用表驱动最省事——`negativeTokens` 的五个条件各喂一个负值：

```go
func TestMeterRejectsNegativeSubfields(t *testing.T) {
	cases := map[string]schema.Usage{
		"input":         {InputTokens: -1},
		"output":        {OutputTokens: -1},
		"cache_read":    {CacheReadTokens: -1},
		"cache_write":   {CacheWriteTokens: -1},
		"reasoning":     {ReasoningTokens: -1},
	}
	for name, usage := range cases {
		t.Run(name, func(t *testing.T) {
			// 成功响应带着这份 usage 走一遍 UsageMeter.Stream，
			// 断言 Result() 返回的 err 的 CodeOf 是 20000。
		})
	}
}
```

第 4 条与之配对，是同一个函数的反面：

```go
func TestMeterAllowsSubsetViolation(t *testing.T) {
	// CacheReadTokens(500) 大于 InputTokens(400)：违反子集约束，但
	// negativeTokens 不看它，所以这次调用必须成功。改了校验的人会挂在这里。
}
```

### `pi/observability/provider_test.go`

同样用内存 Span 导出器：

1. 成功一轮：`chat {model}` Span 上有 `gen_ai.operation.name=chat`、`gen_ai.provider.name`、`gen_ai.request.model`、输入/输出 token、`pi.usage.latency_ms`、`pi.usage.ttft_ms`、`gen_ai.response.finish_reasons`；SpanKind 是 Client。
2. 下层 `Result()` 返回错误 → Span 状态是 Error，描述是稳定码，`error.type` 有值，且**没有** token 属性。
3. 读了两个事件就 `Close()`、没走 `Result()` → Span 仍被结束，状态是 Error。
4. 嵌套：在 `WithSpan("outer")` 里调 `Stream`，chat Span 的父 Span 是 `outer`（父传播这条最容易写错，必须有测试）。

### `pi/middleware/tracing_test.go`

在现有中间件测试的驱动方式上加一条：

1. 一次成功的工具执行 → `execute_tool {name}` Span 上 `pi.tool.is_error=false`、`pi.tool.output_size` 等于输出文本长度、`pi.tool.arguments_size` 等于参数字节数；**Span 上没有任何参数或输出的正文**。
2. 工具返回错误 → 状态 Error，`pi.tool.is_error=true`，链的行为不变（`e.Err` 原样传下去）。
3. `Tracing` 在 `Defaults()` 里排第一：一次被重试两次的调用只留**一个** `execute_tool` Span。

### `pi/agent_observability_test.go`（agent 层，唯一一个跨包的）

前三个都是包内单元测试。压缩 Span 的逻辑在 `compactBeforeTurn` 里，要验它得装配一个真的 `Agent`（`newAgent` 就是为此留的入口：测试直接传自己的假 provider，绕过 `providers.New`）。假 provider 按调用序返回预设的响应，用内存 Span 导出器断言：

1. **不越过触发线 → 一个 `pi.compact_context` 都不开**。这是决策 13 的回归测试：Span 开在函数顶上时，这个断言会挂，而且挂的方式是"每轮多一个空 Span"——不会有人注意到。
2. **越过触发线但 `PlanCompaction` 不成立 → 同样一个都不开**。
3. **压缩成功 → 恰好一个 `pi.compact_context`**，带 `pi.compaction.before_tokens`，状态是 Unset（绿），**且它下面只挂一个 chat Span（摘要请求）**——本轮的正式请求那个 chat Span 是它的**兄弟**，不是子节点。断言要按这个写：子节点数写成 2 就是错的（与第 10 节那棵树对照）。
4. **压缩失败（假 provider 让摘要返回空文本）→ `pi.compact_context` 状态是 Error、描述是 `50002`，而 `Agent.Run` 的返回值是 `(output, nil)`**——运行没被打断。这一条与决策 15 一一对应，也是最值得写的一条：它同时锁住了"Span 标红"和"错误被吞"这两个看起来矛盾的行为。

## 与 pi.dev、go-reagent 差在哪

参考实现是 pi.dev（[badlogic/pi-mono](https://github.com/badlogic/pi-mono)）与 go-reagent。下面的事实都在本机的克隆上核过。

| 事实 | 位置 |
|---|---|
| pi.dev 的遥测是**回调式契约**：`TelemetryContext.startSpan(name, attrs, cb)`，NOOP 与内存两套参考实现，父上下文显式传递（不用 AsyncLocalStorage） | `pi-mono-clone/packages/telemetry` |
| pi.dev 的 schema（`pi.ai.*`／`pi.harness.*`）由 agent-core 自己拥有，telemetry 包不认识业务词汇 | 同上 |
| go-reagent 的追踪**套在 OTel 之上**，业务代码用 `contexttracing.WithSpan`，不碰 `otel/trace` 类型 | `go-reagent/pi/harness/observability/tracing_provider.go:47-69`、`go-reagent/pi/middleware/tracing.go:14-38` |
| go-reagent 的装饰顺序 `Loop → TracingProvider → UsageMeter → Raw` | `go-reagent/pi/agent.go:47-59` |
| go-reagent 计量层不碰金额 | `go-reagent/pi/harness/observability/usage_meter.go:14-16`（"价格与成本不属于 SDK"） |
| go-reagent 的 Tracing handler 排在最外层 | `go-reagent/pi/middleware/defaults.go:9` |

本方案与 go-reagent 的偏离，逐条：

1. **Span 名**：本仓库用 `pi.run`／`pi.turn`，go-reagent 用 `conversation.run`／`reagent.turn`；两者都没跟 OTel 语义约定的 `invoke_agent {agent}`。本方案的 `pi.run` 上额外写了 `gen_ai.operation.name=invoke_agent` 与 `gen_ai.agent.name`，**属性跟约定、名字跟仓库**——后端能按属性认出来，人看 trace 时认得出是本仓库。这是一处刻意的偏离，不是遗漏。
2. **少了 hint（`GenerationHint`）**。go-reagent 用它区分"重试的哪一次尝试""压缩触发的还是正常生成的"（`go-reagent/pi/harness/observability/hint.go`）。本仓库模型层没有重试、压缩有独立 Span，没有消费者就不建通道。
3. **少了 Retry Event**（`reagent.retry.*`，`go-reagent/pi/harness/observability/span.go:62-97`）。本仓库的重试在工具链上，已经有日志。
4. **TTFT 只写 `Usage`，不写 Span 的第二份**。go-reagent 为了在 Span 上也能读 TTFT，定义了一个包内私有接口 `streamTimingReader` 让 tracer 从 meter 里抠快照（`go-reagent/pi/harness/observability/tracing_provider.go:18-24, 96-113`）。本方案的 Span 直接读 `Usage.TTFTMS`，少一个接口。
5. **少了 `observability switch`**。go-reagent 也没有，这条一致。
6. **计量层不写日志**。go-reagent 在 `measure` 里 `logsdk.Error`／`Warn`／`Info`（`usage_meter.go:113-167`）。Span 上已经有全部数字，日志里不再抄一份；失败路径的错误由循环透出，不需要日志再报一次。
7. **严格模式的错误码**：go-reagent 用 `ErrAIGeneration`，本仓库也用 `ErrAIGeneration`（20000）——这条一致，是照搬。

pi.dev 那套回调式契约（事实 1、2）本方案**没有采用**。理由写在决策 1 的备选列里：它的收益是"换供应商不用改业务代码"，代价是自建 NOOP 与内存两套实现、把 `pi` 每个包的调用点都改成传 `TelemetryContext`。本仓库的消费者只有 `cmd/harness` 一个进程与一个 OTel 后端，这笔账不划算。**这是本方案与 pi.dev 最大的一处不同，也是唯一一处"路线不同"而非"少做了什么"。**

## 落地顺序

1. `go.mod` 加依赖（第 9 节）：先 `go list -m -versions github.com/PycMono/go-context-sdk` 确认 `v1.0.4` 在远端可见、再 `go get ...@v1.0.4`，**不落 `replace`**。`go build ./...` 应仍然全绿——此时还没人 import 新包。
2. `pi/observability/semantics.go` + `classify.go`（无依赖，先编译通过）。
3. `pi/observability/meter.go` + `meter_test.go`；`provider.go` + `provider_test.go`。
4. `pi/middleware/tracing.go` + `tracing_test.go`；改 `defaults.go`。
5. 改 `pi/loop.go`（抽 `step` + turn Span）——**先跑一遍现有测试确认搬迁无行为变化**，再加 Span。
6. 改 `pi/agent.go`：先只动装配与 `Run`（装饰器 + run Span），跑一遍测试；再动 `compactBeforeTurn`（压缩 Span）。
7. `pi/agent_observability_test.go`（压缩 Span 的四个断言）。放在 6 之后：它验的是 6 的产物，写在前面对不上。
8. 全量 `go test ./...`。
9. 改 `pi/observability/README.md`：现在的三行写的是"语义约定（semantics/**hint**）"，而本方案不做 hint（决策 10）；"价格与成本换算归业务层"这句与实现一致，保留。README 与代码对不上比没有 README 更糟——照实现改一遍。
10. 可选：`cmd/harness` 里加一个 `-otel-endpoint` 开关，装配 OTLP exporter。这是装配代码，不进库，单独一轮做。
