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
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// 本文件钉住 chat Span：什么时候开、什么时候结束、红了没有、属性对不对。
// tracingStream 的生命周期跨 Stream 的多次调用，五条出口（正常、报错、取消、
// 提前 Close、重复 Result）都要走到 span.End()，所以断言里最常见的是
// "Span 数等于几"。
//
// 数值属性一律用 AsInt64 读，不要用 AsString：attribute.Value.AsString() 只在
// 类型本来就是 STRING 时返回内容，对 INT64 直接返回空串（otel@v1.45.0
// attribute/value.go:284 就是 `return v.stringly`，而 Int64Value 只填 numeric）。
// 用 AsString 读 token 数会读到 ""，断言看着像通过、其实什么都没验。

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
	for key, want := range map[string]string{
		"gen_ai.operation.name": "chat",
		"gen_ai.provider.name":  "anthropic",
		"gen_ai.request.model":  "claude-x",
		"gen_ai.response.model": meterModel,
		AttrUsagePlatformID:     meterPlatformID,
	} {
		if got := attrs[key].AsString(); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	for key, want := range map[string]int64{
		"gen_ai.usage.input_tokens":  1200,
		"gen_ai.usage.output_tokens": 80,
		AttrUsageCacheRead:           900,
		AttrUsageCacheWrite:          100,
		AttrUsageReasoning:           30,
		AttrUsageLatencyMS:           1800,
		AttrUsageTTFTMS:              420,
	} {
		if got := attrs[key].AsInt64(); got != want {
			t.Errorf("%s = %d, want %d", key, got, want)
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
	if got := attrs["gen_ai.usage.input_tokens"].AsInt64(); got != 7 {
		t.Errorf("input_tokens = %d, want 7", got)
	}
}
