package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/PycMono/go-harness/pi/observability"
	"github.com/PycMono/go-harness/pi/schema"
	"github.com/PycMono/go-harness/pi/tools"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// 本文件钉住工具 Span：一次执行一个 Span、只记长度不记正文、工具失败也标红但
// 不改链的走向，以及 Tracing 排在最外层时重试不额外增生 Span。
//
// 布尔与数值属性分别用 AsBool / AsInt64 读，不要用 AsString：它只在类型本来就是
// STRING 时返回内容，别的类型返回空串（otel@v1.45.0 attribute/value.go:284），
// 用它断言会看着通过、其实什么都没验。

// runToolArgs 是驱动一次工具执行用的参数，几个属性断言直接量它的长度。
const runToolArgs = `{"path":"a.txt"}`

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
		Call:       schema.ToolCall{ID: "call-1", Name: "read_file", Arguments: json.RawMessage(runToolArgs)},
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
	for key, want := range map[string]string{
		"gen_ai.operation.name": "execute_tool",
		"gen_ai.tool.name":      "read_file",
		"gen_ai.tool.call.id":   "call-1",
	} {
		if got := attrs[key].AsString(); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	if got := attrs[observability.AttrToolIsError].AsBool(); got {
		t.Errorf("%s = %v, want false", observability.AttrToolIsError, got)
	}
	if got := attrs[observability.AttrToolParallelSafe].AsBool(); !got {
		t.Errorf("%s = %v, want true", observability.AttrToolParallelSafe, got)
	}
	if got, want := attrs[observability.AttrToolArgumentsSize].AsInt64(), int64(len(runToolArgs)); got != want {
		t.Errorf("%s = %d, want %d", observability.AttrToolArgumentsSize, got, want)
	}
	if got, want := attrs[observability.AttrToolOutputSize].AsInt64(), int64(len(output)); got != want {
		t.Errorf("%s = %d, want %d", observability.AttrToolOutputSize, got, want)
	}
	// 正文不上 Span：参数与输出都不许出现在属性里。用 String() 而不是 AsString()
	// ——后者对非字符串类型返回空串，漏检。
	for key, value := range attrs {
		if text := value.String(); text == output || text == runToolArgs {
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
	if got := attributeMap(span.Attributes())[observability.AttrToolIsError].AsBool(); !got {
		t.Errorf("%s = %v, want true", observability.AttrToolIsError, got)
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
