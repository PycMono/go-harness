# pi/observability 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 给 `go-harness` 装上观测层：每个 Run／每轮／每次模型请求／每次工具执行各一个 Span，每次成功的模型响应补上延迟、TTFT 与归属，业务代码不直接 import OpenTelemetry。

**Architecture:** 新增 `pi/observability` 包提供 Span 名与属性键的词汇表、错误分类器、以及两个套在 `ai.Provider` 上的装饰器（`TracingProvider` 在外、`UsageMeter` 在内）。工具执行那一层做成 `pi/middleware` 里的一个 handler。`pi/loop.go` 的循环体抽成 `step`，整轮括进 `pi.turn` Span；`pi/agent.go` 的 `Run` 括进 `pi.run` Span、`compactBeforeTurn` 括进 `pi.compact_context` Span。

**Tech Stack:** Go 1.26.1、`github.com/PycMono/go-context-sdk/tracing`（OTel façade）、`go.opentelemetry.io/otel`（v1.45.0，仅 `codes`／`attribute`／`trace` 三个包）、OpenTelemetry Go SDK（仅测试用）。

**Spec:** `docs/superpowers/plans/2026-10-08-observability-design.md`

## Global Constraints

- Go 版本：`go 1.26.1`（`go.mod` 现状，不动）。
- 依赖（4 条，全部是精确版本）：
  - `github.com/PycMono/go-context-sdk v1.0.4` —— 新增，**已在远端发布**，不需要 `replace`
  - `go.opentelemetry.io/otel v1.45.0` —— 由 `// indirect` 转直接
  - `go.opentelemetry.io/otel/trace v1.45.0` —— 由 `// indirect` 转直接
  - `go.opentelemetry.io/otel/sdk v1.45.0` —— **新增，仅测试用**（`sdktrace.NewTracerProvider` + `tracetest.NewSpanRecorder`）
- **绝不**在 `go.mod` 里落 `go mod edit -replace`。本机能编译靠的是同仓的 `sdk/go-context-sdk` 目录，绝对路径进 `go.mod` 会断掉 CI 与其他人的机器。
- 业务代码（`pi/**` 与 `cmd/**`）**不 import** `go.opentelemetry.io/otel/*`。唯一例外是 `pi/observability/classify.go` 与 `provider.go`——它们是本仓库与 OTel 的边界层。（`pi/observability` 的测试文件不受此限。）
- Span 上**只许**出现稳定错误码（`pi/error` 的 `CodeOf`），**不许**出现 `err.Error()` 的正文。
- Span 上**不许**记录提示词、模型输出、工具参数与输出的正文。只记长度与元数据。
- 本包**不碰** `Usage` 里的金额字段（`CostUSD`、`*PriceUSDPerMillionTokens`、`CostQuality`）。
- 测试**不得**使用 `t.Parallel()`：`go-context-sdk/tracing` 从**全局** `otel.TracerProvider` 取 tracer，同一包内的测试会互相踩。
- 每个测试文件里的 `installRecordingProvider` 必须在 `t.Cleanup` 里还原上一个 provider 并 `Shutdown`，否则会污染同包其他测试。
- commit 信息用中文，格式照仓库现有风格（`feat:` / `refactor:` 前缀 + 正文 + 文件清单 + `Co-Authored-By: Claude Code <noreply@anthropic.com>` 尾巴）。

## 这份计划与 spec 的四处出入（都是有意的，先说明）

1. **依赖是 4 条不是 3 条。** spec 第 9 节写"加 3 条依赖"，漏掉了测试要用的 `go.opentelemetry.io/otel/sdk`。没有它就没有内存 Span 导出器，四个测试文件一个都写不成。第 1、2 两个任务的 `go get` 步骤把它补上。
2. **多一个测试文件 `pi/observability/classify_test.go`。** spec 的测试清单里没有它，但 `ClassifyError`／`ErrorFields` 有真实分支（`nil` → 空串、无码错误 → `"unknown"`），而 `"unknown"` 那条分支**没有任何别的测试会走到**（其余测试里的错误都带码）。不加就是一条永远没跑过的分支。
3. **多一个测试文件 `pi/loop_test.go`。** spec 的落地顺序第 5 步只说"先跑一遍现有测试确认搬迁无行为变化"，但仓库里 `pi/` 包**没有任何循环测试**（只有 `pi/extension/runtime_test.go`），那条安全网实际不存在。抽 `step` 是本方案唯一的结构改动，必须有它自己的回归测试。
4. **`pi/agent_observability_test.go` 从 4 条断言扩到 6 条**，补上 spec 决策 16（被拒绝的工具调用没有 Span）与决策 17（run Span 覆盖准备阶段与会话写入错）——这两条决策在 spec 里没有任何测试覆盖。

## 实施记录（2026-10-08 落地，共 8 个 commit）

八个任务按顺序落地，`951b7fc` → `3e6e8e0`。生产代码与本文一字不差；**出入全在测试代码里**，下面逐条记下来，免得后来人对着本文的代码块写不出能过的测试。

| # | 位置 | 本文写的 | 实际写的 | 为什么 |
|---|---|---|---|---|
| 1 | Task 4 测试块 | `fields := make(map[string]attribute.Value, …)` | 改名为 `attrs` | 参数就叫 `fields []contexttracing.Field`，`:=` 无新变量时是编译错误 |
| 2 | Task 3／4 测试块 | 数值属性用 `.AsString()` | 用 `.AsInt64()`／`.AsBool()` | `attribute.Value.AsString()` 只在类型本来就是 STRING 时返回内容，INT64 返回空串（otel@v1.45.0 `attribute/value.go:284` 就是 `return v.stringly`，而 `Int64Value` 只填 numeric）。用它断言会看着通过、其实什么都没验。"没有正文泄漏"那条循环反而要改成 `.String()`——`AsString()` 对非字符串返回空串，正是漏检 |
| 3 | Task 4 测试块 | 没列 `reflect` | 补 import | `TestTracingIsOutermostInDefaults` 用了 `reflect.ValueOf` |
| 4 | Task 4 测试块 | 参数长度写 17 | 16，且提成 `runToolArgs` 常量 | `{"path":"a.txt"}` 是 16 字节 |
| 5 | Task 5 `TestLoopOpensOneTurnSpanPerTurn` | 两条 `textResponse` | 第 0 轮 `toolCallResponse` + 接了工具的调度器，第 1 轮 `textResponse` | 两条纯文本响应会让第 0 轮就 `stop`，循环只跑一轮：断言的两个 Span 与三条消息都到不了（实跑是 `turn Span 数 = 1, want 2`）。改成两轮后是 2 个 Span、4 条消息 |
| 6 | Task 5 `TestLoopTurnSpanExcludesMaxTurnsBound` | 用"没有调度器"报错 | 用真的撞 maxTurns（`maxTurns = 1` + 第 0 轮要工具） | 本文那条与测试名不符：它验的其实是 `WithScheduler(nil)` 那条路。改成真的撞上限，并断言 `ErrRunLimitExceeded` 的码 |
| 7 | Task 7 `TestRejectedToolCallHasNoSpan` | `spanByName(t, spans, SpanNameTurn)` | 新增 `turnSpanAt(t, spans, index)`，按 `pi.turn.index` 认 | 被拒绝的工具调用会照常开第二轮，一次运行有两个 `pi.turn`，`spanByName` 的"出现了不止一次"分支会当场失败 |
| 8 | Task 6 `Run` 改写 | 闭包里直接 `messages` 与 `started` | 同左 | 闭包里的 `runContext, err :=` 与 `Run` 的 `err` 不是同一个变量，但语义一致，写法没有出入 |

另外一条不在代码里：`go.mod` 在 Task 3 之后跑 `go mod tidy`，`go.opentelemetry.io/otel/sdk` 由 `// indirect` 转成直接依赖，并带入 `github.com/google/uuid v1.6.0 // indirect`（OTel SDK 自己的依赖）。`grep -n replace go.mod` 最终无输出。

## 文件清单

| 文件 | 动作 | 职责 |
|---|---|---|
| `go.mod` | 改 | 4 条依赖 |
| `pi/observability/semantics.go` | 新增 | Span 名与属性键的唯一出处 |
| `pi/observability/classify.go` | 新增 | 错误码分类器、错误属性、手工 Span 的收尾 |
| `pi/observability/classify_test.go` | 新增 | 上面那个的单元测试 |
| `pi/observability/meter.go` | 新增 | `UsageMeter`：延迟、TTFT、归属、严格校验 |
| `pi/observability/meter_test.go` | 新增 | 上面那个的单元测试 |
| `pi/observability/provider.go` | 新增 | `TracingProvider` + `Wrap`：chat Span、装饰顺序 |
| `pi/observability/provider_test.go` | 新增 | 上面那个的单元测试 |
| `pi/middleware/tracing.go` | 新增 | 工具执行的 handler |
| `pi/middleware/tracing_test.go` | 新增 | 上面那个的单元测试 |
| `pi/middleware/defaults.go` | 改 | 链首加 `Tracing` |
| `pi/loop.go` | 改 | 抽 `step`，整轮括进 `pi.turn` Span |
| `pi/loop_test.go` | 新增 | turn Span 的单元测试 |
| `pi/agent.go` | 改 | 装配装饰器、`Run` 括进 `pi.run`、`compactBeforeTurn` 括进 `pi.compact_context` |
| `pi/agent_observability_test.go` | 新增 | agent 层：run Span 与压缩 Span 的断言 |
| `pi/observability/README.md` | 改 | 照实现改掉"hint"那句 |

一个字都不改：`pi/ai/provider.go`、`pi/schema/{usage,message,tools}.go`、`pi/error/errors.go`、`pi/middleware/{handler,logging,recovery,retry}.go`、`pi/session/**`、`pi/tools/**`。

## 落地顺序

1 → 2 → 3 → 4 → 5 → 6 → 7 → 8。第 2 步依赖第 1 步的 `semantics.go`（`meter.go` 不用属性键，但同包）；第 3 步依赖第 2 步的 `UsageMeter`（`TracingProvider` 要套在它外面）；第 4、5 步只依赖第 1 步；第 6 步依赖第 3、4、5 步。

---

### Task 1: 语义常量与错误分类

**Files:**
- Create: `pi/observability/semantics.go`
- Create: `pi/observability/classify.go`
- Create: `pi/observability/classify_test.go`
- Modify: `go.mod`

**Interfaces:**
- Consumes: `pi/error` 的 `CodeOf(err error) int`（`pi/error/errors.go:186`）；`go-context-sdk/tracing` 的 `Field`／`KV`。
- Produces: 常量 `AgentName`、`SpanNameRun`、`SpanNameTurn`、`SpanNameCompaction`、全部 `Attr*`；函数 `ChatSpanName(model string) string`、`ToolSpanName(tool string) string`、`ClassifyError(err error) string`、`ErrorFields(err error) []contexttracing.Field`、包内 `spanError(span trace.Span, err error)`。后面的任务全部按这些名字引用。

- [ ] **Step 1: 写 `pi/observability/semantics.go`**

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

- [ ] **Step 2: 写 `pi/observability/classify.go`**

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

- [ ] **Step 3: 写 `pi/observability/classify_test.go`**

```go
package observability

import (
	"errors"
	"testing"

	pierrors "github.com/PycMono/go-harness/pi/error"
)

// 本文件钉住错误分类这一层：Span 上只许出现稳定码，不许出现错误正文。
// 两个函数都是纯函数，直接比字符串，不装 Span 导出器。

func TestClassifyErrorReturnsStableCode(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil 错误", nil, ""},
		{"带码的错误", pierrors.ErrAIGeneration, "20000"},
		{"包了一层的带码错误", pierrors.ErrCompactionFailed.Wrap(errors.New("摘要为空")), "50002"},
		{"没有码的错误", errors.New("boom"), "0"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := ClassifyError(testCase.err); got != testCase.want {
				t.Fatalf("ClassifyError() = %q, want %q", got, testCase.want)
			}
		})
	}
}

// TestClassifyErrorNeverLeaksMessage 是这一层的红线：正文不进 Span。
// 用一个只可能出现在正文里的字符串当哨兵，任何一条路径带上它就是失败。
func TestClassifyErrorNeverLeaksMessage(t *testing.T) {
	const sentinel = "用户手机号 13800000000"
	err := pierrors.ErrAIGeneration.Wrap(errors.New(sentinel))

	for _, got := range []string{ClassifyError(err), ClassifyError(errors.New(sentinel))} {
		if got == sentinel {
			t.Fatalf("分类结果带上了错误正文：%q", got)
		}
	}
}

func TestErrorFields(t *testing.T) {
	t.Run("nil 错误没有属性", func(t *testing.T) {
		if fields := ErrorFields(nil); fields != nil {
			t.Fatalf("ErrorFields(nil) = %v, want nil", fields)
		}
	})

	t.Run("带码的错误两项都有", func(t *testing.T) {
		fields := ErrorFields(pierrors.ErrCompactionFailed.Wrap(errors.New("摘要为空")))
		got := attributeMap(fields)
		if got[AttrErrorType].AsString() != "50002" {
			t.Errorf("%s = %q, want %q", AttrErrorType, got[AttrErrorType].AsString(), "50002")
		}
		if got[AttrErrorCode].AsString() != "50002" {
			t.Errorf("%s = %q, want %q", AttrErrorCode, got[AttrErrorCode].AsString(), "50002")
		}
	})

	t.Run("无码的错误只留 error.type=unknown", func(t *testing.T) {
		fields := ErrorFields(errors.New("boom"))
		if len(fields) != 1 {
			t.Fatalf("属性数 = %d, want 1", len(fields))
		}
		got := attributeMap(fields)
		if got[AttrErrorType].AsString() != "unknown" {
			t.Errorf("%q = %q, want %q", AttrErrorType, got[AttrErrorType].AsString(), "unknown")
		}
		if _, ok := got[AttrErrorCode]; ok {
			t.Errorf("无码的错误不该有 %s：写 0 会与\"没分类\"混在同一个桶里", AttrErrorCode)
		}
	})
}

// attributeMap 把属性切片折成 key → value，断言时按名字取。
func attributeMap(fields []contexttracing.Field) map[string]attribute.Value {
	fields := make(map[string]attribute.Value, len(fields))
	for _, field := range fields {
		fields[string(field.Key)] = field.Value
	}

	return fields
}
```

第 3 步的 `attributeMap` 需要 import `contexttracing` 与 `attribute`。**注意**：这个 helper 会被 Task 2、3、4 的测试文件重复定义——Go 的测试文件同属一个包，不能重复定义同名函数。所以本文件里的 `attributeMap` 签名收的是 `[]contexttracing.Field`；后续任务如果也需要它，**不要**在新文件里再写一份，直接复用本文件的。修正后的 import 块是：

```go
import (
	"errors"
	"testing"

	contexttracing "github.com/PycMono/go-context-sdk/tracing"
	pierrors "github.com/PycMono/go-harness/pi/error"
	"go.opentelemetry.io/otel/attribute"
)
```

- [ ] **Step 4: 加依赖**

```bash
cd /Users/allen/projects/work/github/go-harness
go list -m -versions github.com/PycMono/go-context-sdk
```

Expected: 输出里能看到 `v1.0.4`。看不到就停下来问人，不要继续。

```bash
go get github.com/PycMono/go-context-sdk@v1.0.4
go mod tidy
go build ./...
```

Expected: `go build ./...` 全绿。`go.mod` 里 `go.opentelemetry.io/otel` 与 `go.opentelemetry.io/otel/trace` 从 `// indirect` 段搬到直接依赖段。

- [ ] **Step 5: 跑测试**

```bash
go test ./pi/observability/... -v
```

Expected: PASS，三个测试函数全绿。

- [ ] **Step 6: vet**

```bash
go vet ./pi/observability/...
```

Expected: 无输出。

- [ ] **Step 7: Commit**

```bash
git add go.mod go.sum pi/observability/semantics.go pi/observability/classify.go pi/observability/classify_test.go
git commit -m "$(cat <<'EOF'
feat: 观测层词汇表与错误分类

pi/observability 的第一块：Span 名与属性键的唯一出处，以及错误码分类器。

semantics.go 里没有任何 import——它是一张纯常量表，跨包使用（pi 与
pi/middleware 都要拼 Span 名），字符串字面量只在这里出现一次。

classify.go 是错误进 Span 的唯一通道。分类结果只许是稳定码：错误正文可能
带用户数据，而且基数不受控，前端按它分组会炸。未分类的错误（stdlib 的
errors.New 之类，CodeOf 返回 0）只写 error.type=unknown，不写 pi.error.code
——写 0 会与"没分类"混在同一个桶里。

spanError 不导出：它的签名里有 trace.Span，导出等于让 otel 类型进入本包的
公开 API。唯一的调用者在同包的 provider.go。

依赖加 go-context-sdk v1.0.4（含 tracing/scope.go 与 tracing/preset.go 的
第一个已发布 tag），两条 otel 由 indirect 转直接。

- go.mod：加 go-context-sdk v1.0.4，otel 与 otel/trace 转直接依赖
- pi/observability/semantics.go：5 个 Span 名、18 个属性键、AgentName
- pi/observability/classify.go：ClassifyError、ErrorFields、spanError
- pi/observability/classify_test.go：稳定码、正文不外泄、unknown 分支

Co-Authored-By: Claude Code <noreply@anthropic.com>
EOF
)"
```

---

### Task 2: UsageMeter

**Files:**
- Create: `pi/observability/meter.go`
- Create: `pi/observability/meter_test.go`

**Interfaces:**
- Consumes: Task 1 的 `ClassifyError`；`pi/ai` 的 `Provider` 与 `Stream`；`pi/schema` 的 `Usage`、`AssistantMessage`、`StreamEvent`、`StreamEventTextDelta`；`pi/error` 的 `ErrAIGeneration`。
- Produces: `UsageMeter`、`NewUsageMeter(next ai.Provider, platformID, model string) (*UsageMeter, error)`、`(*UsageMeter).Stream(ctx, messages, tools) ai.Stream`。包内 `meterStream`、`negativeTokens(usage *schema.Usage) bool`。

- [ ] **Step 1: 写失败的测试 `pi/observability/meter_test.go`**

```go
package observability

import (
	"context"
	"testing"
	"time"

	"github.com/PycMono/go-harness/pi/ai"
	pierrors "github.com/PycMono/go-harness/pi/error"
	"github.com/PycMono/go-harness/pi/schema"
)

// 本文件钉住计量层：补哪几笔账、什么算脏数据、哪些形态有意放行。
// 计时用假钟（每次调用 +10ms），时钟注入点是 UsageMeter.now 这个未导出字段，
// 所以测试必须留在包内。

const (
	meterPlatformID = "platform-x"
	meterModel      = "model-y"
)

// fakeProvider 每次 Stream 都返回同一个流。
type fakeProvider struct {
	stream ai.Stream
	calls  int
}

func (p *fakeProvider) Stream(
	context.Context, schema.Messages, schema.ToolDefinitions,
) ai.Stream {
	p.calls++

	return p.stream
}

// fakeStream 是可控的流：events 依次吐出，response/err 是 Result 的产物。
type fakeStream struct {
	events   []schema.StreamEvent
	response *schema.AssistantMessage
	err      error
	index    int
	closed   bool
}

func (s *fakeStream) Next() bool {
	if s.index >= len(s.events) {
		return false
	}
	s.index++

	return true
}

func (s *fakeStream) Current() schema.StreamEvent { return s.events[s.index-1] }

func (s *fakeStream) Result() (*schema.AssistantMessage, error) { return s.response, s.err }

func (s *fakeStream) Close() error {
	s.closed = true

	return nil
}

// meteredStream 把假流套上 UsageMeter，时钟换成每次调用 +10ms 的假钟。
// 假钟返回 base + 10ms × 调用次数，所以：
//
//	Stream 里取起点     → 第 1 次 → base+10ms
//	Next 里取 TTFT      → 第 2 次 → base+20ms（TTFT = 10ms）
//	Result 里取终点     → 第 2 或 3 次 → 延迟 = 10ms 或 20ms
func meteredStream(t *testing.T, stream ai.Stream) ai.Stream {
	t.Helper()
	meter, err := NewUsageMeter(&fakeProvider{stream: stream}, meterPlatformID, meterModel)
	if err != nil {
		t.Fatalf("NewUsageMeter() error = %v", err)
	}
	var calls int
	meter.now = func() time.Time {
		calls++

		return time.Unix(0, 0).Add(time.Duration(calls) * 10 * time.Millisecond)
	}

	return meter.Stream(context.Background(), nil, nil)
}

// consume 按调用方的正常读法把流读到结束。
func consume(stream ai.Stream) {
	for stream.Next() {
		_ = stream.Current()
	}
}

func TestNewUsageMeterRequiresOwnership(t *testing.T) {
	cases := []struct {
		name       string
		platformID string
		model      string
	}{
		{"平台为空", "", meterModel},
		{"模型为空", meterPlatformID, ""},
		{"平台只有空白", "  ", meterModel},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := NewUsageMeter(&fakeProvider{}, testCase.platformID, testCase.model); err == nil {
				t.Fatal("归属信息不全时应当报错")
			}
		})
	}
}

func TestMeterFillsLatencyAndOwnership(t *testing.T) {
	usage := &schema.Usage{InputTokens: 100, OutputTokens: 20}
	response := &schema.AssistantMessage{Usage: usage}

	got, err := meteredStream(t, &fakeStream{response: response}).Result()
	if err != nil {
		t.Fatalf("Result() error = %v", err)
	}
	if got.Usage.LatencyMS != 10 {
		t.Errorf("LatencyMS = %d, want 10", got.Usage.LatencyMS)
	}
	if got.Usage.PlatformID != meterPlatformID {
		t.Errorf("PlatformID = %q, want %q", got.Usage.PlatformID, meterPlatformID)
	}
	if got.Usage.Model != meterModel {
		t.Errorf("Model = %q, want %q", got.Usage.Model, meterModel)
	}
	// 复制一份再改：就地改会连带改到别人手里的那一份。
	if usage.LatencyMS != 0 || usage.PlatformID != "" {
		t.Errorf("原消息被就地改了：LatencyMS = %d, PlatformID = %q",
			usage.LatencyMS, usage.PlatformID)
	}
	if got.Usage == usage {
		t.Error("Usage 没有复制一份")
	}
}

func TestMeterTTFTOnlyWhenTextFlows(t *testing.T) {
	t.Run("有文本增量则记 TTFT", func(t *testing.T) {
		stream := meteredStream(t, &fakeStream{
			events:   []schema.StreamEvent{{Type: schema.StreamEventTextDelta, TextDelta: "你"}},
			response: &schema.AssistantMessage{Usage: &schema.Usage{}},
		})
		consume(stream)
		got, err := stream.Result()
		if err != nil {
			t.Fatalf("Result() error = %v", err)
		}
		if got.Usage.TTFTMS == nil {
			t.Fatal("TTFTMS = nil, want 有值")
		}
		if *got.Usage.TTFTMS != 10 {
			t.Errorf("TTFTMS = %d, want 10", *got.Usage.TTFTMS)
		}
		if *got.Usage.TTFTMS > got.Usage.LatencyMS {
			t.Errorf("TTFT(%d) 不该大于延迟(%d)", *got.Usage.TTFTMS, got.Usage.LatencyMS)
		}
	})

	t.Run("纯工具调用响应不记 TTFT", func(t *testing.T) {
		stream := meteredStream(t, &fakeStream{
			response: &schema.AssistantMessage{Usage: &schema.Usage{}},
		})
		consume(stream)
		got, err := stream.Result()
		if err != nil {
			t.Fatalf("Result() error = %v", err)
		}
		if got.Usage.TTFTMS != nil {
			t.Errorf("TTFTMS = %d, want nil（没有文本增量）", *got.Usage.TTFTMS)
		}
	})
}

func TestMeterRejectsMissingUsage(t *testing.T) {
	got, err := meteredStream(t, &fakeStream{
		response: &schema.AssistantMessage{},
	}).Result()
	if pierrors.CodeOf(err) != pierrors.ErrAIGeneration.Code() {
		t.Fatalf("CodeOf(err) = %d, want 20000", pierrors.CodeOf(err))
	}
	if got != nil {
		t.Errorf("失败时应当返回 nil 响应，得到 %v", got)
	}
}

// TestMeterRejectsNegativeSubfields 是决策 18 的回归测试：只查输入输出的写法
// 会在这五条里漏掉三条（缓存读、缓存写、推理）。
func TestMeterRejectsNegativeSubfields(t *testing.T) {
	cases := map[string]schema.Usage{
		"input":       {InputTokens: -1},
		"output":      {OutputTokens: -1},
		"cache_read":  {CacheReadTokens: -1},
		"cache_write": {CacheWriteTokens: -1},
		"reasoning":   {ReasoningTokens: -1},
	}
	for name := range cases {
		t.Run(name, func(t *testing.T) {
			usage := cases[name]
			_, err := meteredStream(t, &fakeStream{
				response: &schema.AssistantMessage{Usage: &usage},
			}).Result()
			if pierrors.CodeOf(err) != pierrors.ErrAIGeneration.Code() {
				t.Fatalf("CodeOf(err) = %d, want 20000", pierrors.CodeOf(err))
			}
		})
	}
}

// TestMeterAllowsSubsetViolation 是上一条的反面：子集约束有意不查，改了校验的
// 人会挂在这里。两个内置 provider 都满足它，但那是映射约定不是协议保证——
// 强制会把一个能跑通的请求打掉，代价比记一个可疑数字大。
func TestMeterAllowsSubsetViolation(t *testing.T) {
	usage := &schema.Usage{InputTokens: 400, CacheReadTokens: 500}
	got, err := meteredStream(t, &fakeStream{
		response: &schema.AssistantMessage{Usage: usage},
	}).Result()
	if err != nil {
		t.Fatalf("子集违例被拒了（决策 18 说好不查的）：%v", err)
	}
	if got.Usage.CacheReadTokens != 500 {
		t.Errorf("CacheReadTokens = %d, want 500", got.Usage.CacheReadTokens)
	}
}

func TestMeterMeasuresOnce(t *testing.T) {
	stream := meteredStream(t, &fakeStream{
		response: &schema.AssistantMessage{Usage: &schema.Usage{}},
	})
	first, firstErr := stream.Result()
	if firstErr != nil {
		t.Fatalf("第一次 Result() error = %v", firstErr)
	}
	second, secondErr := stream.Result()
	if secondErr != nil {
		t.Fatalf("第二次 Result() error = %v", secondErr)
	}
	if first != second {
		t.Fatal("两次 Result 返回了不同对象")
	}
	if second.Usage.LatencyMS != 10 {
		t.Errorf("第二次 Result 重新计量了：LatencyMS = %d, want 10", second.Usage.LatencyMS)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

```bash
go test ./pi/observability/... 2>&1 | head -20
```

Expected: 编译失败，`undefined: NewUsageMeter`（以及 `undefined: UsageMeter`）。

- [ ] **Step 3: 写 `pi/observability/meter.go`**

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

- [ ] **Step 4: 加测试依赖并跑测试**

```bash
go get go.opentelemetry.io/otel/sdk@v1.45.0
go test ./pi/observability/... -v
```

Expected: PASS，`TestNewUsageMeterRequiresOwnership`、`TestMeterFillsLatencyAndOwnership`、`TestMeterTTFTOnlyWhenTextFlows`（两个子测试）、`TestMeterRejectsMissingUsage`、`TestMeterRejectsNegativeSubfields`（五个子测试）、`TestMeterAllowsSubsetViolation`、`TestMeterMeasuresOnce` 全绿。

- [ ] **Step 5: vet**

```bash
go vet ./pi/observability/...
```

Expected: 无输出。

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum pi/observability/meter.go pi/observability/meter_test.go
git commit -m "$(cat <<'EOF'
feat: 用量计量装饰器

给每次成功的模型响应补上延迟、TTFT 与归属（平台、模型），不做金额换算——
价格表归业务，同一份 token 账不同客户算出来的钱不一样。

计时口径：startedAt 取在调用下层 Stream 之前。下层的 Stream 不是纯粹建对象，
两个 provider 都在里面就把请求发出去了（anthropic.go 的 Messages.NewStreaming
就在 Stream 里），先调下层再取时刻会漏掉建连与首包前的时间。

严格模式：缺 Usage、任一用量分项为负、延迟为负，都返回 ErrAIGeneration。
分项查全部五个——缓存读写与推理是各自父项的子集，负的子集同样是脏数据。
子集约束本身有意不查：两个内置 provider 满足它，但那是映射约定不是协议保证，
第三方 provider 把缓存报在输入之外时强制会把一个能跑通的请求打掉。

Result 可能被调用多次（下游装饰器与循环都可能读），resolved 保证只计量一次。
补账前先复制一份 Usage：下层可能把指针复用在下一条消息上。

- pi/observability/meter.go：UsageMeter、NewUsageMeter、meterStream、negativeTokens
- pi/observability/meter_test.go：假钟、缺 Usage、五个负分项、子集放行、只计量一次
- go.mod：加 otel/sdk v1.45.0（测试用的内存 Span 导出器）

Co-Authored-By: Claude Code <noreply@anthropic.com>
EOF
)"
```

---

### Task 3: TracingProvider

**Files:**
- Create: `pi/observability/provider.go`
- Create: `pi/observability/provider_test.go`

**Interfaces:**
- Consumes: Task 1 的 `ChatSpanName`、`Attr*`、`ClassifyError`、`ErrorFields`、`spanError`；Task 2 的 `NewUsageMeter`（`Wrap` 里用）；`go-context-sdk/tracing` 的 `StartSpan`、`WithKV`、`OperationName`、`ProviderName`、`RequestModel`、`ResponseModel`、`FinishReasons`、`InputTokens`、`OutputTokens`。
- Produces: `TracingProvider`、`NewTracingProvider(next ai.Provider, provider, model string) *TracingProvider`、`Wrap(provider ai.Provider, platformID, model, protocol string) (ai.Provider, error)`。包内 `tracingStream`、`errStreamAbandoned`。Task 6 用 `Wrap`。

- [ ] **Step 1: 写失败的测试 `pi/observability/provider_test.go`**

```go
package observability

import (
	"context"
	"errors"
	"testing"

	contexttracing "github.com/PycMono/go-context-sdk/tracing"
	"github.com/PycMono/go-harness/pi/ai"
	pierrors "github.com/PycMono/go-harness/pi/error"
	"github.com/PycMono/go-harness/pi/schema"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// 本文件钉住 chat Span：什么时候开、什么时候结束、红了没有、属性对不对。
// tracingStream 的生命周期跨 Stream 的多次调用，五条出口（正常、报错、取消、
// 提前 Close、重复 Result）都要走到 span.End()，所以断言里最常见的是
// "Span 数等于几"。

// installRecordingProvider 把全局 TracerProvider 换成内存导出器，并在测试结束时
// 还原。go-context-sdk 的 StartSpan 从全局取 tracer，所以这是唯一的注入点；
// 也正因为它是全局的，本包的测试都不能用 t.Parallel()。
func installRecordingProvider(t *testing.T) (*sdktrace.TracerProvider, *tracetest.SpanRecorder) {
	t.Helper()
	previousProvider := otel.GetTracerProvider()
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		otel.SetTracerProvider(previousProvider)
		if err := provider.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown tracer provider: %v", err)
		}
	})

	return provider, recorder
}

// singleEndedSpan 取出唯一一个结束了的 Span，数量不对就当场失败。
func singleEndedSpan(t *testing.T, recorder *tracetest.SpanRecorder) sdktrace.ReadOnlySpan {
	t.Helper()
	spans := recorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("结束的 Span 数 = %d, want 1", len(spans))
	}

	return spans[0]
}

// runChat 走一遍正常调用：建流、读空、取结果、关流。
func runChat(t *testing.T, provider ai.Provider) (*schema.AssistantMessage, error) {
	t.Helper()
	stream := provider.Stream(context.Background(), nil, nil)
	defer func() { _ = stream.Close() }()
	for stream.Next() {
		_ = stream.Current()
	}

	return stream.Result()
}

func TestTracingProviderRecordsChatSpan(t *testing.T) {
	_, recorder := installRecordingProvider(t)
	ttftMS := int64(420)
	response := &schema.AssistantMessage{
		FinishReason: schema.FinishReasonStop,
		Usage: &schema.Usage{
			InputTokens: 1200, OutputTokens: 80,
			CacheReadTokens: 900, CacheWriteTokens: 100, ReasoningTokens: 30,
			LatencyMS: 1800, TTFTMS: &ttftMS,
			PlatformID: meterPlatformID, Model: meterModel,
		},
	}
	provider := NewTracingProvider(
		&fakeProvider{stream: &fakeStream{response: response}}, "anthropic", "claude-x")

	if _, err := runChat(t, provider); err != nil {
		t.Fatalf("Result() error = %v", err)
	}

	span := singleEndedSpan(t, recorder)
	if got, want := span.Name(), "chat claude-x"; got != want {
		t.Errorf("Span 名 = %q, want %q", got, want)
	}
	if got := span.SpanKind(); got != trace.SpanKindClient {
		t.Errorf("SpanKind = %v, want %v", got, trace.SpanKindClient)
	}
	attrs := attributeMap(span.Attributes())
	want := map[string]string{
		"gen_ai.operation.name":          "chat",
		"gen_ai.provider.name":           "anthropic",
		"gen_ai.request.model":           "claude-x",
		"gen_ai.usage.input_tokens":      "1200",
		"gen_ai.usage.output_tokens":     "80",
		AttrUsageCacheRead:               "900",
		AttrUsageCacheWrite:              "100",
		AttrUsageReasoning:               "30",
		AttrUsagePlatformID:              meterPlatformID,
		AttrUsageLatencyMS:               "1800",
		AttrUsageTTFTMS:                  "420",
		"gen_ai.response.model":          meterModel,
	}
	for key, wantValue := range want {
		if got := attrs[key].AsString(); got != wantValue {
			t.Errorf("%s = %q, want %q", key, got, wantValue)
		}
	}
	if got := attrs["gen_ai.response.finish_reasons"].AsStringSlice(); len(got) != 1 || got[0] != string(schema.FinishReasonStop) {
		t.Errorf("finish_reasons = %v, want [%s]", got, schema.FinishReasonStop)
	}
}

func TestTracingProviderMarksFailedChat(t *testing.T) {
	_, recorder := installRecordingProvider(t)
	provider := NewTracingProvider(&fakeProvider{stream: &fakeStream{
		err: pierrors.ErrAITransient.Wrap(errors.New("上游 503")),
	}}, "openai", "gpt-x")

	if _, err := runChat(t, provider); err == nil {
		t.Fatal("Result() 应当返回错误")
	}

	span := singleEndedSpan(t, recorder)
	if span.Status().Code != codes.Error {
		t.Errorf("状态码 = %v, want %v", span.Status().Code, codes.Error)
	}
	if got, want := span.Status().Description, "20001"; got != want {
		t.Errorf("状态描述 = %q, want %q（稳定码，不是错误正文）", got, want)
	}
	attrs := attributeMap(span.Attributes())
	if got := attrs[AttrErrorType].AsString(); got != "20001" {
		t.Errorf("%s = %q, want 20001", AttrErrorType, got)
	}
	// 失败的请求账不可信，不写 token。
	if _, ok := attrs["gen_ai.usage.input_tokens"]; ok {
		t.Error("失败的请求不该带 token 属性")
	}
}

// TestTracingProviderEndsAbandonedStream 钉住"提前放弃"这条出口：读两个事件就
// Close、没走 Result，Span 仍要结束，而且是红的。
func TestTracingProviderEndsAbandonedStream(t *testing.T) {
	_, recorder := installRecordingProvider(t)
	provider := NewTracingProvider(&fakeProvider{stream: &fakeStream{
		events: []schema.StreamEvent{
			{Type: schema.StreamEventTextDelta, TextDelta: "a"},
			{Type: schema.StreamEventTextDelta, TextDelta: "b"},
		},
	}}, "openai", "gpt-x")

	stream := provider.Stream(context.Background(), nil, nil)
	if !stream.Next() || !stream.Next() {
		t.Fatal("假流应当吐出两个事件")
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	span := singleEndedSpan(t, recorder)
	if span.Status().Code != codes.Error {
		t.Error("提前放弃的流应当把 Span 标红")
	}
	if got := attributeMap(span.Attributes())[AttrStreamChunkCount].AsInt64(); got != 2 {
		t.Errorf("%s = %d, want 2", AttrStreamChunkCount, got)
	}
}

// TestTracingProviderEndsRepeatedResultOnce 钉住 resolved：Result 调两次，
// Span 只结束一次（否则导出器里会多出一个同名 Span）。
func TestTracingProviderEndsRepeatedResultOnce(t *testing.T) {
	_, recorder := installRecordingProvider(t)
	provider := NewTracingProvider(&fakeProvider{stream: &fakeStream{
		response: &schema.AssistantMessage{Usage: &schema.Usage{}},
	}}, "openai", "gpt-x")

	stream := provider.Stream(context.Background(), nil, nil)
	for stream.Next() {
	}
	if _, err := stream.Result(); err != nil {
		t.Fatalf("第一次 Result() error = %v", err)
	}
	if _, err := stream.Result(); err != nil {
		t.Fatalf("第二次 Result() error = %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	if spans := recorder.Ended(); len(spans) != 1 {
		t.Fatalf("结束的 Span 数 = %d, want 1", len(spans))
	}
}

// TestTracingProviderNestsUnderParent 钉住父传播：这是最容易写错的一条。
func TestTracingProviderNestsUnderParent(t *testing.T) {
	_, recorder := installRecordingProvider(t)
	provider := NewTracingProvider(&fakeProvider{stream: &fakeStream{
		response: &schema.AssistantMessage{Usage: &schema.Usage{}},
	}}, "openai", "gpt-x")

	parentCtx, parent := contexttracing.StartSpan(context.Background(), "outer")
	stream := provider.Stream(parentCtx, nil, nil)
	for stream.Next() {
	}
	if _, err := stream.Result(); err != nil {
		t.Fatalf("Result() error = %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	parent.End()

	spans := recorder.Ended()
	if len(spans) != 2 {
		t.Fatalf("结束的 Span 数 = %d, want 2", len(spans))
	}
	var chat sdktrace.ReadOnlySpan
	for _, span := range spans {
		if span.Name() == "chat gpt-x" {
			chat = span
		}
	}
	if chat == nil {
		t.Fatal("没找到 chat Span")
	}
	if got, want := chat.Parent().SpanID(), parent.SpanContext().SpanID(); got != want {
		t.Errorf("chat Span 的父 SpanID = %s, want %s（outer）", got, want)
	}
}

// TestWrapFixesDecoratorOrder 钉住装饰顺序：Span 上的 token 属性来自内层计量层
// 补好的 Usage，顺序反了就读不到——而这里恰好是唯一固定顺序的地方。
func TestWrapFixesDecoratorOrder(t *testing.T) {
	_, recorder := installRecordingProvider(t)
	wrapped, err := Wrap(&fakeProvider{stream: &fakeStream{
		response: &schema.AssistantMessage{Usage: &schema.Usage{InputTokens: 7}},
	}}, meterPlatformID, meterModel, "anthropic")
	if err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}

	if _, err := runChat(t, wrapped); err != nil {
		t.Fatalf("Result() error = %v", err)
	}

	attrs := attributeMap(singleEndedSpan(t, recorder).Attributes())
	// 归属是计量层补的，Span 上能读到它，说明计量层套在里面。
	if got := attrs[AttrUsagePlatformID].AsString(); got != meterPlatformID {
		t.Errorf("%s = %q, want %q", AttrUsagePlatformID, got, meterPlatformID)
	}
	if got := attrs["gen_ai.usage.input_tokens"].AsString(); got != "7" {
		t.Errorf("input_tokens = %q, want 7", got)
	}
}
```

**注意**：本文件用到 `attributeMap`（Task 1 的 `classify_test.go` 里已定义）、`fakeProvider` 与 `fakeStream`（Task 2 的 `meter_test.go` 里已定义）。同包测试文件共享这些名字，**不要**在本文件里重复定义，否则编译报"redeclared"。import 里如果没有用到 `ai` 或 `contexttracing`，按编译器提示删掉。

- [ ] **Step 2: 跑测试确认失败**

```bash
go test ./pi/observability/... 2>&1 | head -20
```

Expected: 编译失败，`undefined: NewTracingProvider`、`undefined: Wrap`。

- [ ] **Step 3: 写 `pi/observability/provider.go`**

```go
package observability

import (
	"context"
	"errors"

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

- [ ] **Step 4: 跑测试确认通过**

```bash
go test ./pi/observability/... -v
```

Expected: PASS，六个新测试全绿。

- [ ] **Step 5: govet**

```bash
go vet ./pi/observability/...
```

Expected: 无输出。

- [ ] **Step 6: 带着 race 跑一遍**

```bash
go test -race ./pi/observability/...
```

Expected: PASS。

- [ ] **Step 7: Commit**

```bash
git add pi/observability/provider.go pi/observability/provider_test.go
git commit -m "$(cat <<'EOF'
feat: 模型请求的追踪装饰器

每次物理模型请求开一个 chat {model} Span，SpanKind 是 Client——它是出网的
那一跳。Wrap 是本仓库唯一固定装饰顺序的地方：Loop → TracingProvider →
UsageMeter → 原始 Provider。Span 上的 token 属性来自计量层补好的 Usage，
顺序反了就读不到，所以顺序不能由调用方随手拼。

Span 从 Stream 一直活到 Result 或 Close：Stream 只建流、量到的是连接建立，
真正的一次生成要到流读完才结束。五条出口（正常、报错、ctx 取消、提前 Close、
重复 Result）都走同一个 finish，resolved 保证 Span 只结束一次。

失败的请求只留 Span、耗时与错误分类，不写 token——这一轮的账不可信。提前
放弃的流标红并写上事件条数：一个块都没吐和吐了几百次是两回事。

业务代码不 import otel/trace 类型：StartSpan 与属性都经 go-context-sdk 的
façade，本文件是仓库跟 OTel 的边界层之一。

- pi/observability/provider.go：TracingProvider、NewTracingProvider、Wrap、
  tracingStream、errStreamAbandoned
- pi/observability/provider_test.go：属性与 SpanKind、失败只留码、提前放弃、
  重复 Result 只结束一次、父传播、Wrap 的装饰顺序

Co-Authored-By: Claude Code <noreply@anthropic.com>
EOF
)"
```

---

### Task 4: 工具执行的 Span

**Files:**
- Create: `pi/middleware/tracing.go`
- Create: `pi/middleware/tracing_test.go`
- Modify: `pi/middleware/defaults.go`

**Interfaces:**
- Consumes: Task 1 的 `ToolSpanName`、`AttrTool*`、`ClassifyError`、`ErrorFields`；`pi/tools` 的 `Handler`、`Execution`。
- Produces: `func Tracing(e *tools.Execution)`，以及 `Defaults()` 的新顺序。Task 6 的 agent 测试依赖这个顺序。

- [ ] **Step 1: 先看默认链现在的样子**

```bash
cat pi/middleware/defaults.go
```

Expected: 能看到现有实现与它的注释（链的顺序、每条 handler 的职责）。**下一步会整份覆盖它，先读一遍确保没有漏掉现有注释里说的事**——覆盖时要把现有注释里仍然成立的部分保留下来，只在链首插入 `Tracing` 并补上它那一段说明。

- [ ] **Step 2: 写失败的测试 `pi/middleware/tracing_test.go`**

```go
package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	contexttracing "github.com/PycMono/go-context-sdk/tracing"
	"github.com/PycMono/go-harness/pi/observability"
	"github.com/PycMono/go-harness/pi/schema"
	"github.com/PycMono/go-harness/pi/tools"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// 本文件钉住工具 Span：一次执行一个 Span、只记长度不记正文、工具失败也标红但
// 不改链的走向，以及 Tracing 排在最外层时重试不额外增生 Span。

func installRecordingProvider(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	previousProvider := otel.GetTracerProvider()
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		otel.SetTracerProvider(previousProvider)
		if err := provider.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown tracer provider: %v", err)
		}
	})

	return recorder
}

func attributeMap(attrs []attribute.KeyValue) map[string]attribute.Value {
	fields := make(map[string]attribute.Value, len(attrs))
	for _, attr := range attrs {
		fields[string(attr.Key)] = attr.Value
	}

	return fields
}

// stubTool 是链尾真正被调用的那个工具：原样吐一段文本，或按 err 失败。
type stubTool struct {
	text string
	err  error
}

func (t stubTool) Definition() schema.ToolDefinition {
	return schema.ToolDefinition{Name: "read_file", ParallelSafe: true}
}

func (t stubTool) Execute(
	context.Context, json.RawMessage, *tools.UpdateEmitter,
) (*schema.ToolOutput, error) {
	if t.err != nil {
		return nil, t.err
	}

	return &schema.ToolOutput{Content: schema.ContentBlocks{schema.TextBlock(t.text)}}, nil
}

// runTool 用给定的链驱动一次工具执行，返回那个 Execution。
func runTool(handlers []tools.Handler, tool stubTool) *tools.Execution {
	execution := &tools.Execution{
		Ctx:        context.Background(),
		Call:       schema.ToolCall{ID: "call-1", Name: "read_file", Arguments: json.RawMessage(`{"path":"a.txt"}`)},
		Definition: tool.Definition(),
		Tool:       tool,
	}
	execution.Run(handlers)

	return execution
}

func TestTracingRecordsToolSpan(t *testing.T) {
	recorder := installRecordingProvider(t)
	const output = "hello"

	execution := runTool([]tools.Handler{Tracing}, stubTool{text: output})

	spans := recorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("结束的 Span 数 = %d, want 1", len(spans))
	}
	span := spans[0]
	if got, want := span.Name(), "execute_tool read_file"; got != want {
		t.Errorf("Span 名 = %q, want %q", got, want)
	}
	attrs := attributeMap(span.Attributes())
	want := map[string]string{
		"gen_ai.operation.name":              "execute_tool",
		"gen_ai.tool.name":                   "read_file",
		"gen_ai.tool.call.id":                "call-1",
		observability.AttrToolIsError:        "false",
		observability.AttrToolParallelSafe:   "true",
		observability.AttrToolArgumentsSize:  "17",
		observability.AttrToolOutputSize:     "5",
	}
	for key, wantValue := range want {
		if got := attrs[key].AsString(); got != wantValue {
			t.Errorf("%s = %q, want %q", key, got, wantValue)
		}
	}
	// 正文不上 Span：参数与输出都不许出现在属性里。
	for key, value := range attrs {
		if text := value.AsString(); text == output || text == `{"path":"a.txt"}` {
			t.Errorf("属性 %s 带上了正文：%q", key, text)
		}
	}
	_ = execution
}

func TestTracingKeepsChainBehaviorOnToolError(t *testing.T) {
	recorder := installRecordingProvider(t)
	toolErr := errors.New("文件不存在")

	execution := runTool([]tools.Handler{Tracing}, stubTool{err: toolErr})

	if !errors.Is(execution.Err, toolErr) {
		t.Errorf("链的结果 = %v, want %v（工具失败要原样传下去）", execution.Err, toolErr)
	}
	span := recorder.Ended()[0]
	if span.Status().Code != codes.Error {
		t.Error("工具失败时 Span 应当标红")
	}
	if got := attributeMap(span.Attributes())[observability.AttrToolIsError].AsString(); got != "true" {
		t.Errorf("%s = %q, want true", observability.AttrToolIsError, got)
	}
}

// TestTracingIsOutermostInDefaults 钉住链序：Tracing 必须排第一。排在里面的话，
// 一次被重试两次的调用会在后端留下三个 execute_tool，而"哪一次尝试慢"这件事
// 恰恰是重试场景下最该看清的。
func TestTracingIsOutermostInDefaults(t *testing.T) {
	handlers := Defaults()
	if len(handlers) != 4 {
		t.Fatalf("链长 = %d, want 4", len(handlers))
	}
	// 函数值不能直接比，reflect 能取到代码指针。这是唯一能直接断言"谁在链首"
	// 的写法——改用"跑一遍数 Span 数"验证不了链序，因为不重试时任何顺序都只
	// 产生一个 Span。
	if got, want := reflect.ValueOf(handlers[0]).Pointer(), reflect.ValueOf(Tracing).Pointer(); got != want {
		t.Fatal("Tracing 不在链首：重试的每一次尝试会各自增生一个 Span")
	}
}

// TestTracingKeepsOneSpanAcrossRetries 是上一条的行为面：整条默认链跑一次成功
// 的执行，只该留下一个 execute_tool。
func TestTracingKeepsOneSpanAcrossRetries(t *testing.T) {
	recorder := installRecordingProvider(t)

	runTool(Defaults(), stubTool{text: "ok"})

	spans := recorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("结束的 Span 数 = %d, want 1", len(spans))
	}
	if got, want := spans[0].Name(), "execute_tool read_file"; got != want {
		t.Errorf("Span 名 = %q, want %q", got, want)
	}
}
```

- [ ] **Step 3: 跑测试确认失败**

```bash
go test ./pi/middleware/... 2>&1 | head -20
```

Expected: 编译失败，`undefined: Tracing`。

- [ ] **Step 4: 写 `pi/middleware/tracing.go`**

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

- [ ] **Step 5: 改 `pi/middleware/defaults.go`**

把 `Tracing` 插到链首，并补上它那段说明。整份文件：

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

**注意**：`Logging`／`RetryTransient()`／`PanicRecovery` 这三个名字与顺序来自原文件，逐字抄下来。如果原文件里还有别的注释或函数，保留它们，只改 `Defaults()` 与它的文档注释。

- [ ] **Step 6: 跑测试确认通过**

```bash
go test ./pi/middleware/... -v
```

Expected: PASS。

- [ ] **Step 7: 全仓编译与 vet**

```bash
go build ./... && go vet ./...
```

Expected: 无输出。

- [ ] **Step 8: Commit**

```bash
git add pi/middleware/tracing.go pi/middleware/tracing_test.go pi/middleware/defaults.go
git commit -m "$(cat <<'EOF'
feat: 工具执行的追踪中间件

每次进入执行链的工具调用开一个 execute_tool {name} Span，只记元数据与长度：
参数与输出的正文可能很长、可能带用户数据，落进追踪后端就是另一份要合规处理
的副本。只留 arguments_size 与 output_size，正文一个字不上 Span。

handler 顺手把 e.Ctx 换成 Span 的 ctx，链上更靠内的 handler 与真实工具调用
都拿它做父上下文，工具自己的日志因而也落在同一个 trace 上。

工具的失败是业务性的：它作为一条 IsError 的工具消息回给模型，不中断调度。
Span 照样标红（含 error.type 与稳定码），但返回值原样是 e.Err，链的走向不变。

Tracing 排在最外层：重试的每一次尝试落在同一个 Span 里，排在里面的话一次被
重试两次的调用会在后端留下三个 execute_tool。

未注册的工具与参数校验失败在 Scheduler 的入口就返回了，不经过本链，所以没有
Span——它们什么都没执行，挂在 execute_tool {name} 名下是假的。

- pi/middleware/tracing.go：Tracing、toolOutputSize
- pi/middleware/defaults.go：链首加 Tracing，顺序 Tracing → Logging →
  RetryTransient → PanicRecovery
- pi/middleware/tracing_test.go：属性与长度、正文不上 Span、工具失败标红且
  不改链的走向、重试不增生 Span

Co-Authored-By: Claude Code <noreply@anthropic.com>
EOF
)"
```

---

### Task 5: 一轮一个 Span

**Files:**
- Modify: `pi/loop.go`（`run` 抽成 `run` + `step`）
- Create: `pi/loop_test.go`

**Interfaces:**
- Consumes: Task 1 的 `SpanNameTurn`、`AttrTurnIndex`、`AttrToolsAvailable`、`AttrToolsRequested`、`ClassifyError`。
- Produces: `func (l *Loop) step(ctx context.Context, state *runState, turn int) (stop bool, err error)`。Task 6、7 通过它间接获得 turn Span。

- [ ] **Step 1: 先记下搬迁前的基线**

```bash
go build ./... && go test ./... 2>&1 | tail -20
```

Expected: 全绿（仓库现有测试只有 `pi/extension/runtime_test.go` 与几个 `cmd/*/main_test.go`）。把这份输出记下来，第 4 步之后要对比。

- [ ] **Step 2: 写失败的测试 `pi/loop_test.go`**

```go
package pi

import (
	"context"
	"encoding/json"
	"testing"

	contexttracing "github.com/PycMono/go-context-sdk/tracing"
	"github.com/PycMono/go-harness/pi/ai"
	pierrors "github.com/PycMono/go-harness/pi/error"
	"github.com/PycMono/go-harness/pi/observability"
	"github.com/PycMono/go-harness/pi/schema"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// 本文件钉住 turn Span 与它外面的两处退出判定。抽 step 是本方案唯一的结构
// 改动（run 里有六个返回点，手工 End 一定会漏），所以这里最要紧的一条不是
// "Span 开了没有"，而是"不该开的时候一个都没开"。

func installRecordingProvider(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	previousProvider := otel.GetTracerProvider()
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		otel.SetTracerProvider(previousProvider)
		if err := provider.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown tracer provider: %v", err)
		}
	})

	return recorder
}

func attributeMap(attrs []attribute.KeyValue) map[string]attribute.Value {
	fields := make(map[string]attribute.Value, len(attrs))
	for _, attr := range attrs {
		fields[string(attr.Key)] = attr.Value
	}

	return fields
}

// fakeProvider 按调用序返回预设响应，用尽后重复最后一个。errors 按下标与之
// 平行，某一位非 nil 时那次调用以该错误收尾。
//
// onStream 每次 Stream 之前调一次，是测试往"模型调用"这一刻插钩子的地方
// （Task 6 用它制造会话写入失败）。
type fakeProvider struct {
	responses []*schema.AssistantMessage
	errors    []error
	onStream  func()
	calls     int
}

func (p *fakeProvider) Stream(
	context.Context, schema.Messages, schema.ToolDefinitions,
) ai.Stream {
	index := p.calls
	p.calls++
	if p.onStream != nil {
		p.onStream()
	}

	response := &schema.AssistantMessage{Usage: &schema.Usage{}}
	if len(p.responses) > 0 {
		if index < len(p.responses) {
			response = p.responses[index]
		} else {
			response = p.responses[len(p.responses)-1]
		}
	}
	var err error
	if index < len(p.errors) {
		err = p.errors[index]
	}

	return &fakeStream{response: response, err: err}
}

type fakeStream struct {
	response *schema.AssistantMessage
	err      error
}

func (s *fakeStream) Next() bool { return false }

func (s *fakeStream) Current() schema.StreamEvent { return schema.StreamEvent{} }

func (s *fakeStream) Result() (*schema.AssistantMessage, error) { return s.response, s.err }

func (s *fakeStream) Close() error { return nil }

// runContextFor 造一个 loop.run 能直接吃的 Context：三段按 CurrentInputIndex 切，
// 这里只放一条用户消息当本轮输入，历史段留空。
func runContextFor(t *testing.T, history, input schema.Messages) *Context {
	t.Helper()
	messages := make(schema.Messages, 0, len(history)+len(input))
	messages = append(messages, history...)
	messages = append(messages, input...)

	return &Context{Messages: messages, CurrentInputIndex: len(history)}
}

// toolCallResponse 造一条要求调用工具的助手消息。直接填结构体而不走
// NewAssistantMessage：那个构造函数的签名是
// (ContentBlocks, *Usage, FinishReason, ToolCalls) (Message, error)，返回的是
// 接口，这里要的是具体类型。
func toolCallResponse(id, name string, arguments string) *schema.AssistantMessage {
	return &schema.AssistantMessage{
		Content:   schema.ContentBlocks{schema.TextBlock("调用工具")},
		Usage:     &schema.Usage{},
		ToolCalls: schema.ToolCalls{{ID: id, Name: name, Arguments: json.RawMessage(arguments)}},
	}
}

// textResponse 造一条不带工具调用的助手消息，模型据此结束运行。
func textResponse(text string) *schema.AssistantMessage {
	return &schema.AssistantMessage{
		Content:      schema.ContentBlocks{schema.TextBlock(text)},
		Usage:        &schema.Usage{},
		FinishReason: schema.FinishReasonStop,
	}
}

// userMessage 造一条最简的用户消息。同样直接填结构体：NewUserMessage 返回接口，
// 而 run 只读 Blocks()，不校验。
func userMessage(text string) schema.Message {
	return &schema.UserMessage{Content: schema.ContentBlocks{schema.TextBlock(text)}}
}

func TestLoopOpensOneTurnSpanPerTurn(t *testing.T) {
	recorder := installRecordingProvider(t)
	loop := NewLoop(&fakeProvider{responses: []*schema.AssistantMessage{
		textResponse("第一轮"),
		textResponse("第二轮"),
	}})

	messages, err := loop.run(context.Background(), runContextFor(t, nil, schema.Messages{
		userMessage("你好"),
	}))
	if err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if len(messages) != 3 {
		t.Fatalf("消息数 = %d, want 3（1 条输入 + 2 条模型消息）", len(messages))
	}

	spans := recorder.Ended()
	if len(spans) != 2 {
		t.Fatalf("turn Span 数 = %d, want 2", len(spans))
	}
	for index, span := range spans {
		if got, want := span.Name(), observability.SpanNameTurn; got != want {
			t.Errorf("第 %d 个 Span 名 = %q, want %q", index, got, want)
		}
		attrs := attributeMap(span.Attributes())
		if got := attrs[observability.AttrTurnIndex].AsInt64(); got != int64(index) {
			t.Errorf("第 %d 个 Span 的 %s = %d, want %d",
				index, observability.AttrTurnIndex, got, index)
		}
	}
}

// TestLoopTurnSpanExcludesCancelledRun 是抽 step 那条改动的回归测试：轮首的
// 取消判定落在轮与轮之间、不属于任何一轮。开在 Span 里的话这里会多出一个空 Span。
func TestLoopTurnSpanExcludesCancelledRun(t *testing.T) {
	recorder := installRecordingProvider(t)
	loop := NewLoop(&fakeProvider{responses: []*schema.AssistantMessage{textResponse("不该跑到")}})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := loop.run(ctx, runContextFor(t, nil, schema.Messages{userMessage("你好")})); err == nil {
		t.Fatal("已取消的 ctx 应当让 run 报错")
	}

	if spans := recorder.Ended(); len(spans) != 0 {
		t.Fatalf("turn Span 数 = %d, want 0（取消判定在 Span 之外）", len(spans))
	}
}

// TestLoopTurnSpanExcludesMaxTurnsBound 同理：撞上上限那一轮还没跑起来，
// 不该留下 Span。
func TestLoopTurnSpanExcludesMaxTurnsBound(t *testing.T) {
	recorder := installRecordingProvider(t)
	loop := NewLoop(
		&fakeProvider{responses: []*schema.AssistantMessage{
			toolCallResponse("call-1", "read_file", `{"path":"a.txt"}`),
		}},
		WithMaxTurns(1),
		WithScheduler(nil),
	)

	// maxTurns 为 1、模型又要调工具、且没有调度器：第 0 轮会以"没有接入工具
	// 调度器"结束，走不到第 1 轮。这里只验 Span 数——第 0 轮跑起来了，所以
	// 恰好一个。
	if _, err := loop.run(context.Background(), runContextFor(t, nil, schema.Messages{userMessage("你好")})); err == nil {
		t.Fatal("模型要调工具却没有调度器时应当报错")
	}

	if spans := recorder.Ended(); len(spans) != 1 {
		t.Fatalf("turn Span 数 = %d, want 1", len(spans))
	}
}

// TestLoopTurnSpanMarksFailedTurn 钉住"这一轮红了"：失败的返回值经 step 交给
// 闭包，Span 标 Error 且描述是稳定码。
func TestLoopTurnSpanMarksFailedTurn(t *testing.T) {
	recorder := installRecordingProvider(t)
	loop := NewLoop(&fakeProvider{
		responses: []*schema.AssistantMessage{textResponse("x")},
		errors:    []error{pierrors.ErrAITransient.Wrap(context.DeadlineExceeded)},
	})

	if _, err := loop.run(context.Background(), runContextFor(t, nil, schema.Messages{userMessage("你好")})); err == nil {
		t.Fatal("模型调用失败时 run 应当报错")
	}

	spans := recorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("turn Span 数 = %d, want 1", len(spans))
	}
	if spans[0].Status().Code != codes.Error {
		t.Error("失败的那一轮 Span 应当标红")
	}
	if got, want := spans[0].Status().Description, "20001"; got != want {
		t.Errorf("状态描述 = %q, want %q", got, want)
	}
}

var _ = contexttracing.WithSpan
```

**注意**：最后那行 `var _ = contexttracing.WithSpan` 与 import 同理——用不到就删掉。这个文件里的 `fakeProvider`／`fakeStream`／`attributeMap`／`installRecordingProvider` 会在 Task 6 的 `pi/agent_observability_test.go` 里复用，**不要**在那里重复定义。

另外：`schema.NewAssistantMessage`、`schema.UserMessage{}`、`schema.ToolCalls`、`ToolCall` 的确切构造方式按 `pi/schema` 的实际 API 调整——写的时候对着 `pi/schema/message.go` 核一遍，构造不出来的话用最小可编译的等价写法（比如直接 `&schema.AssistantMessage{...}` 填字段）。`pi/loop.go` 里的 `state.produced` 追加的是 `*schema.AssistantMessage`。

- [ ] **Step 3: 跑测试确认失败**

```bash
go test ./pi/... 2>&1 | head -20
```

Expected: 编译失败或 `turn Span 数 = 0, want 2`——此时 `run` 里还没有 Span。

- [ ] **Step 4: 改 `pi/loop.go`：抽 `step` 并加上 Span**

把现有 `run` 里 `for turn := 0; ; turn++ {` 的循环体整段搬进新函数 `step`，`run` 只剩两处轮首判定与调用 `step`。**搬迁之外不改任何一行逻辑**——消息序列的推进顺序、每处 `return` 的时机、`l.deliver` 的两处位置都逐字照抄。

`run` 改成：

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

			// ……以下整段从原 run 的循环体逐字搬过来，只把每个 `return state.messages(), X`
			// 改成 `return X`，把两处 `return state.messages(), nil` 里的正常结束那处改成
			// `stop = true; return nil`……
			//
			// 具体地，搬过来之后函数体是：
			l.deliver(l.drainSteering(), state)

			if l.beforeTurn != nil {
				head, err := l.beforeTurn(ctx, state.turn())
				if err != nil {
					return err
				}
				if head != nil {
					state.head = head
				}
			}

			l.deliver(l.drainSteering(), state)

			message, err := l.complete(ctx, state)
			if err != nil {
				return err
			}
			assistant := message
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
				result, err := results[index].ResultMessage()
				if err != nil {
					return pierrors.ErrInternal.Wrap(err)
				}
				state.produced = append(state.produced, result)
				l.observe(result)
				toolResults = append(toolResults, result)
			}

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

**搬迁时逐条保留原注释**：`l.deliver(l.drainSteering(), state)` 上面那两段关于"插话为什么先于压缩"和"压缩期间写进来的消息在这里补捞"的注释、`state.produced = append(...)` 上面关于"工具结果必须紧跟助手消息"的注释、`results[index].ResultMessage()` 出错那处关于"结束事件缺身份是调度器坏了"的注释、`l.afterTurn` 上面关于"判定为什么放在轮末"的注释——全部原样跟在对应语句上面。上面代码块里为了省篇幅略掉了几处，**不要**真的省掉。

`pi/loop.go` 的 import 增加两行：

```go
	contexttracing "github.com/PycMono/go-context-sdk/tracing"
	"github.com/PycMono/go-harness/pi/observability"
```

- [ ] **Step 5: 跑测试确认通过**

```bash
go test ./pi/... -v 2>&1 | tail -30
```

Expected: PASS，`pi/loop_test.go` 四个测试全绿。

- [ ] **Step 6: 确认搬迁没有行为变化**

```bash
go build ./... && go vet ./... && go test ./...
```

Expected: 全绿，与 Step 1 记录的基线一致（除了新增的 `pi/loop_test.go`）。

- [ ] **Step 7: 带着 race 跑一遍**

```bash
go test -race ./pi/... ./pi/middleware/... ./pi/observability/...
```

Expected: PASS。

- [ ] **Step 8: Commit**

```bash
git add pi/loop.go pi/loop_test.go
git commit -m "$(cat <<'EOF'
feat: 一轮一个 Span

run 的循环体抽成 step，整轮括进 pi.turn Span。这是本方案唯一的结构改动，
搬迁之外没有别的变化：消息序列的推进顺序、deliver 的两处位置、每处 return
的时机都逐字照抄。

抽函数不是为了拆逻辑，是为了给 Span 一个包得住整轮的进出点：run 里有六个
返回点，手工 End 一定会漏一个没结束的 Span，而这种漏只在生产环境的后端上
看得见。step 只有一个出口，Span 一定只结束一次。

轮首的两处退出判定（ctx 取消、maxTurns）留在 Span 之外，落在轮与轮之间、
不属于任何一轮——Span 里只有真正跑起来的那一轮的工作。这两条各有回归测试。

- pi/loop.go：run 只留轮首判定与调用 step；step 开 turn Span，写 turn_index
  与 tools_available，模型要工具时补 tools_requested
- pi/loop_test.go：每轮一个 Span 与轮次序号、取消与超预算时零个 Span、
  失败那一轮标红且描述是稳定码

Co-Authored-By: Claude Code <noreply@anthropic.com>
EOF
)"
```

---

### Task 6: 装配与 `pi.run`

**Files:**
- Modify: `pi/agent.go`（`newAgent` 的装配、`Run` 的 run Span）
- Create: `pi/agent_observability_test.go`

**Interfaces:**
- Consumes: Task 3 的 `observability.Wrap(provider, platformID, model, protocol)`；Task 5 的 `Loop.step`（`run` 的签名不变）；Task 1 的 `SpanNameRun`、`AttrGenAIAgentName`、`AgentName`、`ClassifyError`。
- Produces: 装配后的 `Agent.provider` 是最外层 `TracingProvider`；`Agent.Run` 的错误与 `pi.run` Span 的红绿一一对应。Task 7 在同一文件里继续加压缩的断言。

- [ ] **Step 1: 写失败的测试 `pi/agent_observability_test.go`**

```go
package pi

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/PycMono/go-harness/pi/ai"
	"github.com/PycMono/go-harness/pi/ai/providers"
	pierrors "github.com/PycMono/go-harness/pi/error"
	"github.com/PycMono/go-harness/pi/observability"
	"github.com/PycMono/go-harness/pi/schema"
	"github.com/PycMono/go-harness/pi/session"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/codes"
)

// 本文件是唯一一个 agent 层的观测测试：run Span 与压缩 Span 只有装配出一个真
// Agent 才看得到。假 provider 从这里注入（newAgent 直接收 ai.Provider，绕过
// providers.New），所以一条真请求都不会发出去。
//
// 复用 pi/loop_test.go 里的 installRecordingProvider、attributeMap、
// fakeProvider、fakeStream、textResponse、userMessage——同包测试文件共享这些
// 名字，不要在这里重复定义。

// agentContextWindow 是测试用的上下文窗口。它的余量是按 NewWindow 的规则折过的：
// Reserve=16384 不变，KeepRecent 由 20000 折半成 10000，于是
// Enabled() 报 true（32768-16384 = 16384 > requestHeadroomTokens 8192），
// 触发线落在 16384 上——上下文估算超过 16384 就压。
const agentContextWindow = 32768

// writeAgentsFile 在 workDir 里放一份最小的 AGENTS.md。
//
// 这不是可选的装饰：Agent.Run → prepareRunContext → contextBuilder.Build →
// SystemPrompt 会读它，缺了就直接以 ErrAgentsFileMissing 失败，压缩那条路
// 一步都走不到。反过来说，"不放 AGENTS.md"正是构造"准备阶段失败"用例的手段。
func writeAgentsFile(t *testing.T, workDir string) {
	t.Helper()
	if err := os.WriteFile(
		filepath.Join(workDir, "AGENTS.md"), []byte("# 测试工程\n"), 0o600,
	); err != nil {
		t.Fatalf("写 AGENTS.md 失败: %v", err)
	}
}

// newTestAgent 装配一个真的 Agent。ProviderOptions 的其余字段（BaseURL、APIKey
// 等）在 newAgent 这条路上不会被读——只有 ContextWindow 有用——但整个结构体不能
// 是 nil，newAgent 会直接取它的字段。
func newTestAgent(
	t *testing.T,
	workDir string,
	provider ai.Provider,
	sessions *session.Manager,
) *Agent {
	t.Helper()
	agent, err := newAgent(context.Background(), provider, &Options{
		WorkDir: workDir,
		ProviderOptions: &providers.Options{
			ID:            "platform-x",
			Model:         "model-y",
			Protocol:      providers.ProtocolOpenAI,
			ContextWindow: agentContextWindow,
		},
		Session: sessions,
	})
	if err != nil {
		t.Fatalf("newAgent() error = %v", err)
	}

	return agent
}

// spanByName 按名字取一个结束了的 Span，数量不对就当场失败。
func spanByName(t *testing.T, spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	var found sdktrace.ReadOnlySpan
	for _, span := range spans {
		if span.Name() != name {
			continue
		}
		if found != nil {
			t.Fatalf("Span %q 出现了不止一次", name)
		}
		found = span
	}
	if found == nil {
		t.Fatalf("没找到 Span %q（实际有 %d 个：%v）", name, len(spans), spanNames(spans))
	}

	return found
}

func spanNames(spans []sdktrace.ReadOnlySpan) []string {
	names := make([]string, 0, len(spans))
	for _, span := range spans {
		names = append(names, span.Name())
	}

	return names
}

// TestRunSpanCoversSuccessfulRun 钉住整棵树的形状：一次 Run 一个 pi.run，底下
// 每轮一个 pi.turn，turn 底下一次模型请求一个 chat。
func TestRunSpanCoversSuccessfulRun(t *testing.T) {
	recorder := installRecordingProvider(t)
	workDir := t.TempDir()
	writeAgentsFile(t, workDir)
	agent := newTestAgent(t, workDir, &fakeProvider{
		responses: []*schema.AssistantMessage{textResponse("你好")},
	}, session.InMemory())

	output, err := agent.Run(context.Background(), &RunInput{Prompt: "在吗"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if output == nil {
		t.Fatal("Run() 返回了 nil 输出")
	}

	spans := recorder.Ended()
	if len(spans) != 3 {
		t.Fatalf("Span 数 = %d（%v），want 3", len(spans), spanNames(spans))
	}
	runSpan := spanByName(t, spans, observability.SpanNameRun)
	if runSpan.Status().Code == codes.Error {
		t.Error("成功的运行不该把 run Span 标红")
	}
	attrs := attributeMap(runSpan.Attributes())
	if got, want := attrs["gen_ai.operation.name"].AsString(), "invoke_agent"; got != want {
		t.Errorf("gen_ai.operation.name = %q, want %q", got, want)
	}
	if got, want := attrs[observability.AttrGenAIAgentName].AsString(), observability.AgentName; got != want {
		t.Errorf("%s = %q, want %q", observability.AttrGenAIAgentName, got, want)
	}

	// 父传播：turn 挂在 run 底下，chat 挂在 turn 底下。
	turnSpan := spanByName(t, spans, observability.SpanNameTurn)
	if got, want := turnSpan.Parent().SpanID(), runSpan.SpanContext().SpanID(); got != want {
		t.Errorf("turn Span 的父不是 run Span")
	}
	chatSpan := spanByName(t, spans, observability.ChatSpanName("model-y"))
	if got, want := chatSpan.Parent().SpanID(), turnSpan.SpanContext().SpanID(); got != want {
		t.Errorf("chat Span 的父不是 turn Span")
	}
}

// TestRunSpanCoversPrepareFailure 是决策 17 前半的回归测试。Span 开在
// prepareRunContext 之前：不开的话这里一个 Span 都没有——Run 报了错、trace 上
// 却什么都没有，而"准备阶段为什么失败"恰恰是最要看 trace 的时候。
func TestRunSpanCoversPrepareFailure(t *testing.T) {
	recorder := installRecordingProvider(t)
	// 故意不写 AGENTS.md：contextBuilder.Build 会以 ErrAgentsFileMissing 失败。
	agent := newTestAgent(t, t.TempDir(), &fakeProvider{
		responses: []*schema.AssistantMessage{textResponse("不该跑到")},
	}, session.InMemory())

	output, err := agent.Run(context.Background(), &RunInput{Prompt: "在吗"})
	if err == nil {
		t.Fatal("没有 AGENTS.md 时 Run 应当报错")
	}
	if output != nil {
		t.Fatalf("准备阶段失败应当返回 nil 输出、运行中途失败才返回部分消息，得到 %v", output)
	}

	runSpan := spanByName(t, recorder.Ended(), observability.SpanNameRun)
	if runSpan.Status().Code != codes.Error {
		t.Error("准备失败时 run Span 应当红着")
	}
}

// TestRunSpanExcludesEntryValidation 钉住 Span 的起点：三个入口校验都在 Span
// 之外——那是调用方写错了参数，这一次运行根本没开始。
func TestRunSpanExcludesEntryValidation(t *testing.T) {
	cases := []struct {
		name  string
		input *RunInput
	}{
		{"输入为空", &RunInput{Prompt: "   "}},
		{"输入为 nil", nil},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := installRecordingProvider(t)
			workDir := t.TempDir()
			writeAgentsFile(t, workDir)
			agent := newTestAgent(t, workDir, &fakeProvider{
				responses: []*schema.AssistantMessage{textResponse("不该跑到")},
			}, session.InMemory())

			if _, err := agent.Run(context.Background(), testCase.input); err == nil {
				t.Fatal("入口校验应当失败")
			}
			if spans := recorder.Ended(); len(spans) != 0 {
				t.Fatalf("入口校验失败不该留下 run Span，得到 %v", spanNames(spans))
			}
		})
	}
}

// TestRunSpanCoversSessionWriteFailure 是决策 17 后半的回归测试：会话写入失败
// 是红的错误、绿 Span，正好是"部分失败记成成功"。
//
// 制造失败的办法：假 provider 在"模型返回"这一刻往会话文件尾巴上加一个字节，
// 于是紧接着写助手消息时 sessionFile.checkUnchanged 发现文件比记录的长了，
// Append 报 ErrSessionParentMismatch（pi/session/file.go:173-176）。这是仓库
// 既有的多写入者防护，不用为测试新开一个口子。
func TestRunSpanCoversSessionWriteFailure(t *testing.T) {
	recorder := installRecordingProvider(t)
	workDir := t.TempDir()
	writeAgentsFile(t, workDir)

	root := t.TempDir()
	sessions, err := session.OpenOrCreate(root, workDir, "session-write-failure")
	if err != nil {
		t.Fatalf("OpenOrCreate() error = %v", err)
	}

	provider := &fakeProvider{
		responses: []*schema.AssistantMessage{textResponse("你好")},
		onStream: func() {
			// 此刻盘上已经有这个会话了：OpenOrCreate 写了首行，prepareRunContext
			// 又把本轮输入写了下去，所以 checkUnchanged 记的长度非零。
			// 覆盖成一个字节，长度就对不上——截断到多少都行，它只比长度。
			if err := os.WriteFile(sessions.Path(), []byte("x"), 0o600); err != nil {
				t.Fatalf("改动会话文件失败: %v", err)
			}
		},
	}
	agent := newTestAgent(t, workDir, provider, sessions)

	output, err := agent.Run(context.Background(), &RunInput{Prompt: "在吗"})
	if err == nil {
		t.Fatal("会话写入失败时 Run 应当报错")
	}
	if pierrors.CodeOf(err) == 0 {
		t.Errorf("会话写入失败的错误应当带稳定码，得到 %v", err)
	}
	if output == nil {
		t.Fatal("运行本身跑完了，应当返回已经产生的消息序列")
	}

	runSpan := spanByName(t, recorder.Ended(), observability.SpanNameRun)
	if runSpan.Status().Code != codes.Error {
		t.Error("会话写入失败时 run Span 应当红着")
	}
}
```

**注意**：`sdktrace.ReadOnlySpan` 与 `sdktrace "go.opentelemetry.io/otel/sdk/trace"` 即使 `pi/loop_test.go` 里已经用过，本文件仍要单独 import——每个文件有自己的 import 块。

- [ ] **Step 2: 跑测试确认失败**

```bash
go test ./pi/... -run 'TestRun' 2>&1 | head -30
```

Expected: 失败，四处。`TestRunSpanCoversSuccessfulRun` 报 `没找到 Span "pi.run"（实际有 1 个：[pi.turn]）`——此时 `newAgent` 还没套装饰器，模型调用那层没有 chat Span，只有 Task 5 写好的 turn Span。`TestRunSpanCoversPrepareFailure` 报 Span 数 = 0。`TestRunSpanCoversSessionWriteFailure` 前面几条断言现在就能过（写入失败此刻已经由 `Run` 透出），挂在最后那句"run Span 应当红着"上——因为根本没有 run Span。`TestRunSpanExcludesEntryValidation` 本来就该通过（入口校验在 Span 之外），它守的是 Step 4 之后不回归。

- [ ] **Step 3: 改 `pi/agent.go` 的装配（`newAgent`）**

两处：加 `ProviderOptions` 判空，套装饰器，然后把 `provider` 全部换成 `traced`。

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
```

后面照抄现有代码，只把两处 `provider` 换成 `traced`：`agent := &Agent{... provider: traced, ...}` 与 `NewLoop(traced, ...)`。其余（`impl.NewDefaultTools`、`tools.Register`、`extension.NewRuntime`、`registry.Freeze()`、`maxParallel`）一字不动。

- [ ] **Step 4: 改 `pi/agent.go` 的 `Run`**

```go
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

`pi/agent.go` 的 import 增加两行：

```go
	contexttracing "github.com/PycMono/go-context-sdk/tracing"
	"github.com/PycMono/go-harness/pi/observability"
```

- [ ] **Step 5: 跑测试确认通过**

```bash
go test ./pi/... -v -run 'TestRun' 2>&1 | tail -30
```

Expected: PASS，四个测试（含子测试）全绿。

- [ ] **Step 6: 全量回归**

```bash
go build ./... && go vet ./... && go test ./...
```

Expected: 全绿。

- [ ] **Step 7: Commit**

```bash
git add pi/agent.go pi/agent_observability_test.go
git commit -m "$(cat <<'EOF'
feat: 整次运行一个 Span

装配处套上观测装饰器，Run 整段括进 pi.run Span。装饰器拿的是 ProviderOptions
的归属信息（ID / Model / Protocol），所以 newAgent 也补一次 ProviderOptions
判空——NewAgent 已经判过，但 newAgent 被测试直接调用，判空不能只留在上层。

run Span 从 prepareRunContext 之前就开，到会话写入错误的判定为止。这两处都
是补的漏：原来准备上下文失败时 Run 报了错、trace 上什么都没有；会话写入失败
时调用方收到错误、run Span 却是绿的，正好是"部分失败记成成功"。

闭包的返回值与 Run 的返回值合成一件事：Run 返回错误 ⇔ Span 是红的。唯一在
Span 之外的是三个入口校验——调用方写错了参数，这一次运行根本没开始。准备
阶段失败要返回 nil 输出、运行中途失败要返回已产生的部分，这个既有形状由
started 局部量保住。

- pi/agent.go：newAgent 加 ProviderOptions 判空与 observability.Wrap；Run 的
  run Span 覆盖 prepareRunContext 与 errors.Join(runErr, writeErr)
- pi/agent_observability_test.go：成功运行的整棵树与父传播、准备失败时 Span
  仍红、入口校验无 Span、会话写入失败时 Span 红

Co-Authored-By: Claude Code <noreply@anthropic.com>
EOF
)"
```

---

### Task 7: 压缩的 Span

**Files:**
- Modify: `pi/agent.go`（`compactBeforeTurn`）
- Modify: `pi/agent_observability_test.go`（追加压缩的断言）

**Interfaces:**
- Consumes: Task 6 的 `newTestAgent`、`writeAgentsFile`、`agentContextWindow`、`spanByName`；Task 1 的 `SpanNameCompaction`、`AttrCompactionBeforeTokens`；Task 3 的 `TracingProvider`（摘要请求的 chat Span 由它白送）。
- Produces: `compactBeforeTurn` 里的 `pi.compact_context` Span；`summarize` 一个字不改。

- [ ] **Step 1: 在 `pi/agent_observability_test.go` 末尾追加压缩的测试**

```go
// 下面的压缩用例共用一个历史。触发线是 agentContextWindow 折出来的 16384
// （见 agentContextWindow 的注释），这份历史正好踩在线上、又留得下一个非空的
// 摘要段：
//
//	u1                    最老的一条，会被摘要掉
//	assistant(40000 个 "a", Usage.InputTokens=20000)
//	                      保留段的第一条。真内容是 40000 个 ASCII 字符 ≈ 10000
//	                      token，正好够 findCutPoint 从新往旧数满 KeepRecent
//	                      (10000)；Usage 那 20000 是估算的基数，让 ContextTokens
//	                      报 20001 > 16384。用法当基数是最省的造法——不用真堆
//	                      两万 token 的正文。
//	u2                    叶子
//
// 计划成立时 TokensBefore = 20001，Summarize = [u1]。

// seededSession 造上面那份历史，返回会话。
func seededSession(t *testing.T) *session.Manager {
	t.Helper()
	sessions := session.InMemory()
	appendEntry(t, sessions, &schema.UserMessage{
		Content: schema.ContentBlocks{schema.TextBlock("u1")},
	})
	appendEntry(t, sessions, &schema.AssistantMessage{
		Content:      schema.ContentBlocks{schema.TextBlock(strings.Repeat("a", 40000))},
		Usage:        &schema.Usage{InputTokens: 20000},
		FinishReason: schema.FinishReasonStop,
	})
	appendEntry(t, sessions, &schema.UserMessage{
		Content: schema.ContentBlocks{schema.TextBlock("u2")},
	})

	return sessions
}

func appendEntry(t *testing.T, sessions *session.Manager, message schema.Message) {
	t.Helper()
	if err := sessions.Append(session.Entry{Type: session.EntryMessage, Message: message}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
}

// TestCompactionSpanNotOpenedBelowTriggerLine 是决策 13 的回归测试：Span 开在
// 函数顶上时这个断言会挂，而且挂的方式是"每轮多一个空 Span"——不会有人注意到。
func TestCompactionSpanNotOpenedBelowTriggerLine(t *testing.T) {
	recorder := installRecordingProvider(t)
	workDir := t.TempDir()
	writeAgentsFile(t, workDir)
	// 空会话：越不过触发线。
	agent := newTestAgent(t, workDir, &fakeProvider{
		responses: []*schema.AssistantMessage{textResponse("你好")},
	}, session.InMemory())

	if _, err := agent.Run(context.Background(), &RunInput{Prompt: "在吗"}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	for _, span := range recorder.Ended() {
		if span.Name() == observability.SpanNameCompaction {
			t.Fatal("没越过触发线却开了压缩 Span")
		}
	}
}

// TestCompactionSpanNotOpenedWithoutPlan 是第二个提前返回：越过了触发线但计划
// 不成立（保留区没数满，摘要段是空的），同样不该留 Span。
func TestCompactionSpanNotOpenedWithoutPlan(t *testing.T) {
	recorder := installRecordingProvider(t)

	sessions := session.InMemory()
	appendEntry(t, sessions, &schema.UserMessage{
		Content: schema.ContentBlocks{schema.TextBlock("u1")},
	})
	// Usage 大、正文小：ContextTokens 报 20001 越过触发线，但保留区从新往旧
	// 数不满 KeepRecent，findCutPoint 退到起点，摘要段为空 ⇒ 计划不成立。
	appendEntry(t, sessions, &schema.AssistantMessage{
		Content:      schema.ContentBlocks{schema.TextBlock("a1")},
		Usage:        &schema.Usage{InputTokens: 20000},
		FinishReason: schema.FinishReasonStop,
	})

	workDir := t.TempDir()
	writeAgentsFile(t, workDir)
	agent := newTestAgent(t, workDir, &fakeProvider{
		responses: []*schema.AssistantMessage{textResponse("你好")},
	}, sessions)

	if _, err := agent.Run(context.Background(), &RunInput{Prompt: "在吗"}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	for _, span := range recorder.Ended() {
		if span.Name() == observability.SpanNameCompaction {
			t.Fatal("计划不成立却开了压缩 Span")
		}
	}
}

// TestCompactionSpanOnSuccess 钉住压缩成功时的形状：恰好一个压缩 Span、带压缩
// 前的体量、绿的，且它下面只挂一个 chat Span（摘要请求）——本轮的正式请求那个
// chat Span 是它的兄弟，不是子节点。
func TestCompactionSpanOnSuccess(t *testing.T) {
	recorder := installRecordingProvider(t)
	workDir := t.TempDir()
	writeAgentsFile(t, workDir)
	sessions := seededSession(t)
	agent := newTestAgent(t, workDir, &fakeProvider{
		responses: []*schema.AssistantMessage{
			textResponse("## Goal\n把测试写完"), // 第一次 Stream：摘要请求
			textResponse("你好"),               // 第二次 Stream：正式请求
		},
	}, sessions)

	if _, err := agent.Run(context.Background(), &RunInput{Prompt: "在吗"}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	spans := recorder.Ended()
	compactionSpan := spanByName(t, spans, observability.SpanNameCompaction)
	if compactionSpan.Status().Code == codes.Error {
		t.Errorf("压缩成功时不该标红：%s", compactionSpan.Status().Description)
	}
	if got, want := attributeMap(compactionSpan.Attributes())[observability.AttrCompactionBeforeTokens].AsInt64(), int64(20001); got != want {
		t.Errorf("%s = %d, want %d", observability.AttrCompactionBeforeTokens, got, want)
	}

	// 压缩 Span 的子节点恰好一个 chat（摘要请求）。本轮的正式请求是它的兄弟。
	children := 0
	for _, span := range spans {
		if span.Parent().SpanID() == compactionSpan.SpanContext().SpanID() {
			children++
		}
	}
	if children != 1 {
		t.Fatalf("压缩 Span 的子节点数 = %d, want 1（本轮的正式请求是它的兄弟，不是子节点）", children)
	}
	// 摘要请求与正式请求是两个 chat Span，总共两个——压缩 Span 那一层是用来
	// 区分它们的，少一层就看不出哪个是压缩。
	chats := 0
	for _, span := range spans {
		if span.Name() == observability.ChatSpanName("model-y") {
			chats++
		}
	}
	if chats != 2 {
		t.Fatalf("chat Span 数 = %d, want 2（摘要 + 正式请求）", chats)
	}
}

// TestCompactionSpanRedButRunContinues 是决策 15：压缩失败时 Span 标红，而这一
// 轮照常跑下去、Run 的返回值是 (output, nil)。它同时锁住了"Span 标红"与
// "错误被吞"这两个看起来矛盾的行为。
func TestCompactionSpanRedButRunContinues(t *testing.T) {
	recorder := installRecordingProvider(t)
	workDir := t.TempDir()
	writeAgentsFile(t, workDir)
	agent := newTestAgent(t, workDir, &fakeProvider{
		responses: []*schema.AssistantMessage{
			// 摘要请求：正文为空 ⇒ summarize 报 ErrCompactionFailed("摘要为空")。
			// Usage 给一个空结构体，让响应先过计量的严格校验、走到文本那一步——
			// 不带 Usage 的话会先被计量以 20000 拦下，那样测的就不是"摘要为空"了。
			{Usage: &schema.Usage{}},
			textResponse("你好"),
		},
	}, seededSession(t))

	output, err := agent.Run(context.Background(), &RunInput{Prompt: "在吗"})
	if err != nil {
		t.Fatalf("压不动不算这一轮失败，Run() error = %v", err)
	}
	if output == nil {
		t.Fatal("Run() 返回了 nil 输出")
	}

	compactionSpan := spanByName(t, recorder.Ended(), observability.SpanNameCompaction)
	if compactionSpan.Status().Code != codes.Error {
		t.Error("压缩失败时 Span 必须红着——它存在的全部意义就是让人看见压缩没生效")
	}
	if got, want := compactionSpan.Status().Description, "50002"; got != want {
		t.Errorf("状态描述 = %q, want %q（ErrCompactionFailed）", got, want)
	}
}

// TestRejectedToolCallHasNoSpan 是决策 16：未注册的工具在 Scheduler 的查表处就
// 被拒了，不经过中间件链，所以没有 execute_tool Span。它们照常作为一条 IsError
// 的工具消息回给模型。
func TestRejectedToolCallHasNoSpan(t *testing.T) {
	recorder := installRecordingProvider(t)
	workDir := t.TempDir()
	writeAgentsFile(t, workDir)
	agent := newTestAgent(t, workDir, &fakeProvider{
		responses: []*schema.AssistantMessage{
			toolCallResponse("call-1", "not_registered", `{}`),
			textResponse("好，换条路"),
		},
	}, session.InMemory())

	output, err := agent.Run(context.Background(), &RunInput{Prompt: "在吗"})
	if err != nil {
		t.Fatalf("被拒绝的工具调用不该中断运行，Run() error = %v", err)
	}
	if output == nil {
		t.Fatal("Run() 返回了 nil 输出")
	}

	for _, span := range recorder.Ended() {
		if strings.HasPrefix(span.Name(), "execute_tool") {
			t.Fatalf("被拒绝的工具调用不该有 Span，却看到了 %q", span.Name())
		}
	}
	// 它仍然在 trace 上的证据是那一轮的 tools_requested：模型确实要过工具，
	// 只是没执行成功。
	turnSpan := spanByName(t, recorder.Ended(), observability.SpanNameTurn)
	if got := attributeMap(turnSpan.Attributes())[observability.AttrToolsRequested].AsInt64(); got != 1 {
		t.Errorf("%s = %d, want 1", observability.AttrToolsRequested, got)
	}
}
```

**注意**：`pi/loop_test.go` 里 `fakeStream.Result()` 永远返回同一个 `*schema.AssistantMessage`，所以按调用序给响应是靠 `fakeProvider.responses` 的下标——摘要请求拿到下标 0、正式请求拿到下标 1。这正是 `fakeProvider` 那个 `calls` 计数器的用途，不要改成别的方式。

这段要在 Task 6 的 import 块里补上 `strings`（`strings.Repeat`、`strings.HasPrefix`）。

- [ ] **Step 2: 跑测试确认失败**

```bash
go test ./pi/... -run 'TestCompaction|TestRejectedToolCall' 2>&1 | head -30
```

Expected: 四个压缩用例里 `TestCompactionSpanOnSuccess` 与 `TestCompactionSpanRedButRunContinues` 报 "没找到 Span \"pi.compact_context\""（另外两个是"不该有 Span"的断言，现在本来就没有，会直接通过——它们守的是 Step 3 之后不回归）。`TestRejectedToolCallHasNoSpan` 走 Task 5 已经写好的 turn Span，直接通过。

- [ ] **Step 3: 改 `pi/agent.go` 的 `compactBeforeTurn`**

两个提前返回保持在 Span 之外；Span 开在它们之后，套一个局部闭包。

```go
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
```

这段替换掉原来的 `summary, err := a.summarize(...)` 到 `a.report(CompactionEvent{TokensBefore: plan.TokensBefore})` 之间的全部代码。函数最后那行 `return a.session.MessagesAt(a.headLeafID), nil` 保持不变。

`summarize` 一行都不改，只在它的文档注释里补上它为什么不碰观测：

```go
// summarize 用当前模型生成一段摘要。这是一次独立、不带工具的调用：不进循环、
// 不发工具描述、不接文本观察者（摘要不该流到用户屏幕上）。
//
// 它不碰观测：这次模型调用的 chat Span 由装配处的 TracingProvider 开（装饰器
// 白送的），它自己只负责"摘要合不合格"。压缩的账（压前多大、成没成）由调用者
// compactBeforeTurn 记。
```

- [ ] **Step 4: 跑测试确认通过**

```bash
go test ./pi/... -v -run 'TestCompaction|TestRejectedToolCall' 2>&1 | tail -30
```

Expected: PASS。

- [ ] **Step 5: 全量回归 + race**

```bash
go build ./... && go vet ./... && go test ./... && go test -race ./pi/...
```

Expected: 全绿。

- [ ] **Step 6: Commit**

```bash
git add pi/agent.go pi/agent_observability_test.go
git commit -m "$(cat <<'EOF'
feat: 压缩的 Span

压缩 Span 开在 compactBeforeTurn 的调用者这一侧，不开在 summarize 里。两件事
只有装饰器看不见，要靠它补：这次是压缩、压之前上下文多大；以及 Compact 落盘
失败时那个错误该落在哪个 Span 上——它原来落在任何 Span 之外。

Span 开在两个提前返回之后。compactBeforeTurn 每轮都被调，第一个检查 OverLine
在绝大多数轮次上直接返回，开在函数顶上等于每轮留一个空的 pi.compact_context。

压不动时闭包把错误返回出去让 Span 标红，调用方再用一个局部量把它吞掉：压不动
不算这一轮失败（历史还是完整的，只是继续贴着窗口跑，真溢出了让 provider 报
20003），但 Span 必须红——否则它永远是绿的，而它存在的全部意义正是让人看见
"压缩没生效"。落盘失败不吞，与会话写入失败同类。

只记 before_tokens 一项：摘要多长看子 chat Span 的 output_tokens，压缩成没成
看 Span 的状态，不在父 Span 上再抄一遍。

- pi/agent.go：compactBeforeTurn 的压缩 Span 与 failure 局部量；summarize 只
  补一句注释，函数体一字不动
- pi/agent_observability_test.go：不越线零个 Span、无计划零个 Span、成功时
  一个 Span 且只挂一个子 chat、失败时 Span 红而 Run 照常返回、被拒绝的工具
  调用没有 execute_tool Span

Co-Authored-By: Claude Code <noreply@anthropic.com>
EOF
)"
```

---

### Task 8: README 与收尾

**Files:**
- Modify: `pi/observability/README.md`

**Interfaces:**
- Consumes: 前七个任务的全部产物。
- Produces: 无代码接口。它是这一轮的收尾闸门。

- [ ] **Step 1: 读现在的 README**

```bash
cat pi/observability/README.md
```

- [ ] **Step 2: 照实现改一遍**

要点只有两条：现在那三行里的"语义约定（semantics/**hint**）"要改掉——本方案不做 hint（决策 10），提示语义那条通道没有消费者就没有建；"价格与成本换算归业务层"那句与实现一致，保留。另外补上实际的三个层次（`pi.run` / `pi.turn` / `chat` / `execute_tool` / `pi.compact_context`）与两处"不做"（不记正文、不碰金额）。README 与代码对不上比没有 README 更糟。

- [ ] **Step 3: 最终全量验证**

```bash
go build ./... && go vet ./... && go test ./... && go test -race ./...
```

Expected: 全绿。

- [ ] **Step 4: 确认没有把本地 replace 带进去**

```bash
grep -n "replace" go.mod
```

Expected: 无输出。有输出就删掉那行——绝对路径的 replace 会断掉 CI 与其他人的机器。

- [ ] **Step 5: Commit**

```bash
git add pi/observability/README.md
git commit -m "$(cat <<'EOF'
docs: 照实现重写 pi/observability 的 README

原文写的"语义约定（semantics/hint）"里那个 hint 本方案不做——提示语义那条
通道服务的是"重试的哪一次尝试""压缩触发的还是正常生成的"这类区分，本仓库的
模型层没有重试、压缩是独立 Span，没有消费者就不建通道。README 与代码对不上
比没有 README 更糟，照实现改一遍。

顺带把四层 Span 与两处"不做"（不记提示词与输出的正文、不碰金额字段）写清楚。

- pi/observability/README.md：去掉 hint，补上实际的 Span 分层与两条不做的边界

Co-Authored-By: Claude Code <noreply@anthropic.com>
EOF
)"
```

---

## 完成之后

这一轮只交付库内的一层。**接后端（OTLP exporter）不在本轮**——那是 `cmd/harness` 里一个 main 函数级的装配，单独一轮做。`pi/observability` 只保证"有 Provider 就上，没有就 noop"。

`pi/schema/usage.go` 与 `pi/observability/README.md` 里那些标着 `§9.1` 的说法**不是**本方案的设计依据，别拿它们当规格；本方案的规格是 `docs/superpowers/plans/2026-10-08-observability-design.md` 与本计划。
