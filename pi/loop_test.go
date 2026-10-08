package pi

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/PycMono/go-harness/pi/ai"
	pierrors "github.com/PycMono/go-harness/pi/error"
	"github.com/PycMono/go-harness/pi/observability"
	"github.com/PycMono/go-harness/pi/schema"
	"github.com/PycMono/go-harness/pi/tools"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// 本文件钉住 turn Span 与它外面的两处退出判定。抽 step 是本方案唯一的结构
// 改动（run 里有六个返回点，手工 End 一定会漏），所以这里最要紧的一条不是
// "Span 开了没有"，而是"不该开的时候一个都没开"。
//
// 数值属性一律用 AsInt64 读，不要用 AsString：它只在类型本来就是 STRING 时
// 返回内容，别的类型返回空串（otel@v1.45.0 attribute/value.go:284），用它断言
// 会看着通过、其实什么都没验。

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

// loopTool 是循环测试里真正被调用的那个工具：原样吐一段固定文本。定义与
// pi/tools/impl 里的内置工具同形，好让 Register 的输入校验真的跑一遍。
type loopTool struct{ text string }

func (t loopTool) Definition() schema.ToolDefinition {
	return schema.ToolDefinition{
		Name:         "read",
		Label:        "Read",
		Description: "读一个文本文件。",
		ParallelSafe: true,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{"type": "string"},
			},
			"required": []string{"path"},
		},
	}
}

func (t loopTool) Execute(
	context.Context, json.RawMessage, *tools.UpdateEmitter,
) (*schema.ToolOutput, error) {
	return &schema.ToolOutput{Content: schema.ContentBlocks{schema.TextBlock(t.text)}}, nil
}

// toolScheduler 造一个接了若干工具的调度器。不挂中间件链：这几条测试量的是
// turn Span，链上的 Tracing 会在工具执行时另开一个 execute_tool Span，混进来
// 就数不清了。
func toolScheduler(t *testing.T, items ...tools.Tool) *tools.Scheduler {
	t.Helper()
	registry, err := tools.Register(items)
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	registry.Freeze()

	return tools.NewScheduler(registry, 1, nil)
}

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

// newTwoTurnLoop 造一个"第 0 轮调工具、第 1 轮收尾"的循环。两轮才会留下两个 turn
// Span——只有一轮带工具调用的响应会让循环当场结束，那样量的是一个 Span。
//
// runContext 的 Tools 取自 loop.definitions()，与 pi/agent.go:270 把
// a.loop.definitions() 交给 ContextBuilder 是同一条路。
func newTwoTurnLoop(t *testing.T) (*Loop, *Context) {
	t.Helper()
	loop := NewLoop(&fakeProvider{responses: []*schema.AssistantMessage{
		toolCallResponse("call-1", "read", `{"path":"a.txt"}`),
		textResponse("读完了"),
	}}, WithScheduler(toolScheduler(t, loopTool{text: "文件内容"})))

	runContext := runContextFor(t, nil, schema.Messages{userMessage("你好")})
	runContext.Tools = loop.definitions()

	return loop, runContext
}

func TestLoopOpensOneTurnSpanPerTurn(t *testing.T) {
	recorder := installRecordingProvider(t)
	loop, runContext := newTwoTurnLoop(t)

	messages, err := loop.run(context.Background(), runContext)
	if err != nil {
		t.Fatalf("run() error = %v", err)
	}
	// 1 条本轮输入 + 第 0 轮的助手消息与工具结果 + 第 1 轮的助手消息。
	if len(messages) != 4 {
		t.Fatalf("消息数 = %d, want 4", len(messages))
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
		if got, want := attrs[observability.AttrToolsAvailable].AsInt64(), int64(1); got != want {
			t.Errorf("第 %d 个 Span 的 %s = %d, want %d",
				index, observability.AttrToolsAvailable, got, want)
		}
	}

	// 请求了几个工具只有"要求调工具"的那些轮有得记：第 1 轮模型直接收尾，
	// 这个属性不该出现，出现 0 也不行——0 与"没问"是两回事。
	if got := attributeMap(spans[0].Attributes())[observability.AttrToolsRequested].AsInt64(); got != 1 {
		t.Errorf("第 0 个 Span 的 %s = %d, want 1", observability.AttrToolsRequested, got)
	}
	if _, ok := attributeMap(spans[1].Attributes())[observability.AttrToolsRequested]; ok {
		t.Errorf("第 1 个 Span 不该有 %s", observability.AttrToolsRequested)
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

// TestLoopTurnSpanExcludesMaxTurnsBound 同理：撞上上限的那一轮还没跑起来，
// 不该留下 Span。上限只设 1，第 0 轮照常跑完（它就是那唯一的一个 Span），
// 第 1 轮在轮首被拦下。
func TestLoopTurnSpanExcludesMaxTurnsBound(t *testing.T) {
	recorder := installRecordingProvider(t)
	loop, runContext := newTwoTurnLoop(t)
	loop.maxTurns = 1

	_, err := loop.run(context.Background(), runContext)
	if err == nil {
		t.Fatal("撞上 maxTurns 时 run 应当报错")
	}
	if got, want := pierrors.CodeOf(err), pierrors.ErrRunLimitExceeded.Code(); got != want {
		t.Errorf("错误码 = %d, want %d（%v）", got, want, err)
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
