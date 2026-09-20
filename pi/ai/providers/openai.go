package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/PycMono/go-harness/pi/ai"
	pierrors "github.com/PycMono/go-harness/pi/error"
	"github.com/PycMono/go-harness/pi/schema"
	openaisdk "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/respjson"
	openaisstream "github.com/openai/openai-go/v3/packages/ssestream"
)

type OpenAIImpl struct {
	client openaisdk.Client
	model  string
	name   string
}

func NewOpenAI(opts *Options) ai.Provider {
	return &OpenAIImpl{
		client: openaisdk.NewClient(
			option.WithAPIKey(opts.APIKey),
			option.WithBaseURL(opts.BaseURL),
			option.WithMaxRetries(0),
		),
		model: opts.Model,
		name:  opts.ID,
	}
}

func (o *OpenAIImpl) Stream(
	ctx context.Context,
	msgs schema.Messages,
	definitions schema.ToolDefinitions,
) ai.Stream {
	if err := msgs.Validate(); err != nil {
		return newFailedStream(pierrors.ErrAIGeneration.Wrap(fmt.Errorf("%s 消息校验失败: %w", o.name, err)))
	}

	openAIMessages, err := msgs.ToOpenAIMessages()
	if err != nil {
		return newFailedStream(pierrors.ErrAIGeneration.Wrap(fmt.Errorf("%s 消息转换失败: %w", o.name, err)))
	}

	openAITools, err := definitions.ToOpenAITools()
	if err != nil {
		return newFailedStream(pierrors.ErrAIGeneration.Wrap(fmt.Errorf("%s 工具定义转换失败: %w", o.name, err)))
	}

	params := openaisdk.ChatCompletionNewParams{
		Model:         o.model,
		Messages:      openAIMessages,
		StreamOptions: openaisdk.ChatCompletionStreamOptionsParam{IncludeUsage: openaisdk.Bool(true)},
	}
	if len(openAITools) > 0 {
		params.Tools = openAITools
	}

	return &openAIStream{provider: o, stream: o.client.Chat.Completions.NewStreaming(ctx, params)}
}

func (o *OpenAIImpl) classifyError(err error) error {
	code := pierrors.ErrAIGeneration
	switch {
	case errors.Is(err, context.Canceled):
		code = pierrors.ErrCanceled
	case errors.Is(err, context.DeadlineExceeded):
		code = pierrors.ErrDeadlineExceeded
	}

	if apiErr, ok := errors.AsType[*openaisdk.Error](err); ok {
		switch {
		case apiErr.Code == "context_length_exceeded":
			code = pierrors.ErrAIContextOverflow
		case apiErr.Code == "insufficient_quota":
			code = pierrors.ErrAIQuotaExceeded
		default:
			code = HTTPStatusToCode(apiErr.StatusCode)
		}
	} else if IsTransientNetwork(err) {
		code = pierrors.ErrAITransient
	}

	return code.Wrap(err)
}

type openAIStream struct {
	streamState
	provider    *OpenAIImpl
	stream      *openaisstream.Stream[openaisdk.ChatCompletionChunk]
	accumulator openaisdk.ChatCompletionAccumulator
	pending     []schema.StreamEvent
	usageSeen   bool
}

func (s *openAIStream) Next() bool {
	if len(s.pending) > 0 {
		s.current = s.pending[0]
		s.pending = s.pending[1:]
		return true
	}

	if s.terminal {
		return false
	}
	if s.start() {
		return true
	}
	if s.stream == nil {
		return s.fail(s.err)
	}

	for s.stream.Next() {
		chunk := s.stream.Current()
		if !s.accumulator.AddChunk(chunk) {
			return s.fail(pierrors.ErrAIGeneration.Wrap(errors.New("流式响应拼接失败")))
		}
		if chunk.JSON.Usage.Valid() && chunk.Usage.JSON.PromptTokens.Valid() &&
			chunk.Usage.JSON.CompletionTokens.Valid() {
			s.usageSeen = true
		}
		for _, choice := range chunk.Choices {
			if choice.Delta.Content != "" {
				s.pending = append(s.pending, schema.StreamEvent{
					Type: schema.StreamEventTextDelta, TextDelta: choice.Delta.Content,
				})
			}
		}

		if len(s.pending) > 0 {
			s.current = s.pending[0]
			s.pending = s.pending[1:]
			return true
		}
	}

	if err := s.stream.Err(); err != nil {
		return s.fail(s.provider.classifyError(err))
	}
	if err := s.finish(); err != nil {
		return s.fail(err)
	}
	return s.done()
}

func (s *openAIStream) Close() error {
	if s.stream == nil {
		return nil
	}
	return s.stream.Close()
}

func (s *openAIStream) finish() error {
	response := s.accumulator.ChatCompletion
	if len(response.Choices) == 0 {
		return pierrors.ErrAIGeneration.Wrap(fmt.Errorf("%s API 返回空 choices", s.provider.name))
	}

	message := response.Choices[0].Message
	result := &schema.Message{
		Role:         schema.RoleAssistant,
		FinishReason: openAIFinishReason(response.Choices[0].FinishReason),
	}
	if message.Content != "" {
		result.Content = []schema.ContentBlock{schema.TextBlock(message.Content)}
	}
	for _, toolCall := range message.ToolCalls {
		if toolCall.Type != "function" {
			continue
		}
		result.ToolCalls = append(result.ToolCalls, schema.ToolCall{
			ID:        toolCall.ID,
			Name:      toolCall.Function.Name,
			Arguments: json.RawMessage(toolCall.Function.Arguments),
		})
	}

	if s.usageSeen {
		result.Usage = mapOpenAIUsage(response.Usage)
	}
	s.result = result
	return nil
}

func (s *streamState) start() bool {
	if s.started {
		return false
	}

	s.started = true
	s.current = schema.StreamEvent{Type: schema.StreamEventStart}
	return true
}

func (s *streamState) fail(err error) bool {
	s.err = err
	s.terminal = true
	s.current = schema.StreamEvent{Type: schema.StreamEventError}
	return true
}

func (s *streamState) done() bool {
	s.terminal = true
	s.current = schema.StreamEvent{Type: schema.StreamEventDone}
	return true
}

// prompt_cache_hit_tokens）。ExtraFields 条目对未知字段标记为 invalid，
// 因此不能用 Field.Valid()，只按 Raw() 判空并解析；缺失、null 或非数字
func toInt64(fields map[string]respjson.Field, name string) int64 {
	field, ok := fields[name]
	if !ok {
		return 0
	}
	raw := field.Raw()
	if raw == "" || raw == "null" {
		return 0
	}
	var value int64
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return 0
	}

	return value
}

func openAIFinishReason(reason string) schema.FinishReason {
	switch reason {
	case "tool_calls", "function_call":
		return schema.FinishReasonToolUse
	case "length":
		return schema.FinishReasonLength
	default:
		return schema.FinishReasonStop
	}
}

// mapOpenAIUsage 按 §9.2 归一化 OpenAI 兼容协议的 Usage：
// cached_tokens → CacheReadTokens；reasoning_tokens → ReasoningTokens；
// InputTokens = prompt_tokens。DeepSeek 的 prompt_cache_hit_tokens 是非标
// 字段，经 ExtraFields 读取；其 prompt_tokens 本身即 hit + miss 总量。
// 字段缺失时对应分项为 0，总量与分项必须一致由 §9.1 校验兜底。
func mapOpenAIUsage(usage openaisdk.CompletionUsage) *schema.Usage {
	mapped := &schema.Usage{
		InputTokens:  usage.PromptTokens,
		OutputTokens: usage.CompletionTokens,
	}
	if usage.JSON.PromptTokensDetails.Valid() {
		mapped.CacheReadTokens = usage.PromptTokensDetails.CachedTokens
	}
	if usage.JSON.CompletionTokensDetails.Valid() {
		mapped.ReasoningTokens = usage.CompletionTokensDetails.ReasoningTokens
	}
	if hit := toInt64(usage.JSON.ExtraFields, "prompt_cache_hit_tokens"); hit > 0 {
		mapped.CacheReadTokens = hit
	}
	return mapped
}
