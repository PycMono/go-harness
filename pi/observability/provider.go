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
