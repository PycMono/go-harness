package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/PycMono/go-harness/pi/ai"
	pierrors "github.com/PycMono/go-harness/pi/error"
	"github.com/PycMono/go-harness/pi/tools"
	anthropicsdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	anthropicstream "github.com/anthropics/anthropic-sdk-go/packages/ssestream"
)

type AnthropicImpl struct {
	client anthropicsdk.Client
	model  string
	name   string
}

func NewAnthropic(opts *Options) ai.Provider {
	return &AnthropicImpl{
		client: anthropicsdk.NewClient(
			option.WithAPIKey(opts.APIKey),
			option.WithBaseURL(opts.BaseURL),
			option.WithMaxRetries(0),
		),
		model: opts.Model,
		name:  opts.ID,
	}
}

func (p *AnthropicImpl) Stream(
	ctx context.Context,
	msgs ai.Messages,
	availableTools tools.ToolDefinitions,
) ai.Stream {
	if err := msgs.Validate(); err != nil {
		return newFailedStream(pierrors.ErrAIGeneration.Wrap(fmt.Errorf("%s 消息校验失败: %w", p.name, err)))
	}

	messages, system, err := msgs.ToAnthropicMessages()
	if err != nil {
		return newFailedStream(pierrors.ErrAIGeneration.Wrap(fmt.Errorf("%s 消息转换失败: %w", p.name, err)))
	}
	tools, err := availableTools.ToAnthropicTools()
	if err != nil {
		return newFailedStream(pierrors.ErrAIGeneration.Wrap(fmt.Errorf("%s 工具定义转换失败: %w", p.name, err)))
	}

	params := anthropicsdk.MessageNewParams{
		Model:     p.model,
		MaxTokens: 4096,
		Messages:  messages,
		System:    system,
	}
	if len(tools) > 0 {
		params.Tools = tools
	}

	return &anthropicStream{provider: p, stream: p.client.Messages.NewStreaming(ctx, params)}
}

type anthropicStream struct {
	streamState
	provider *AnthropicImpl
	stream   *anthropicstream.Stream[anthropicsdk.MessageStreamEventUnion]
	message  anthropicsdk.Message
}

func (s *anthropicStream) Next() bool {
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
		event := s.stream.Current()
		if err := s.message.Accumulate(event); err != nil {
			return s.fail(pierrors.ErrAIGeneration.Wrap(err))
		}
		switch current := event.AsAny().(type) {
		case anthropicsdk.ContentBlockDeltaEvent:
			if delta, ok := current.Delta.AsAny().(anthropicsdk.TextDelta); ok && delta.Text != "" {
				s.current = ai.StreamEvent{Type: ai.StreamEventTextDelta, TextDelta: delta.Text}
				return true
			}
		case anthropicsdk.MessageStopEvent:
			if err := s.finish(); err != nil {
				return s.fail(err)
			}
			return s.done()
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

func (s *anthropicStream) Close() error {
	if s.stream == nil {
		return nil
	}
	return s.stream.Close()
}

func (s *anthropicStream) finish() error {
	if s.message.StopReason == anthropicsdk.StopReasonModelContextWindowExceeded {
		return pierrors.ErrAIContextOverflow.Wrap(
			errors.New("model context window exceeded"),
		)
	}

	result := &ai.Message{Role: ai.RoleAssistant, FinishReason: anthropicFinishReason(s.message.StopReason)}
	for _, block := range s.message.Content {
		switch block.Type {
		case "text":
			result.Content = append(result.Content, tools.TextBlock(block.Text))
		case "tool_use":
			result.ToolCalls = append(result.ToolCalls, tools.ToolCall{
				ID:        block.ID,
				Name:      block.Name,
				Arguments: append(json.RawMessage(nil), block.Input...),
			})
		}
	}
	usage := s.message.Usage
	result.Usage = &ai.Usage{
		InputTokens:      usage.InputTokens + usage.CacheReadInputTokens + usage.CacheCreationInputTokens,
		OutputTokens:     usage.OutputTokens,
		CacheReadTokens:  usage.CacheReadInputTokens,
		CacheWriteTokens: usage.CacheCreationInputTokens,
	}
	s.result = result
	return nil
}

func (p *AnthropicImpl) classifyError(err error) error {
	code := pierrors.ErrAIGeneration
	switch {
	case errors.Is(err, context.Canceled):
		code = pierrors.ErrCanceled
	case errors.Is(err, context.DeadlineExceeded):
		code = pierrors.ErrDeadlineExceeded
	}
	if apiErr, ok := errors.AsType[*anthropicsdk.Error](err); ok {
		switch apiErr.Type() {
		case anthropicsdk.ErrorTypeBillingError:
			code = pierrors.ErrAIQuotaExceeded
		case anthropicsdk.ErrorTypeRateLimitError:
			code = pierrors.ErrAIRateLimited
		case anthropicsdk.ErrorTypeTimeoutError,
			anthropicsdk.ErrorTypeOverloadedError,
			anthropicsdk.ErrorTypeAPIError:
			code = pierrors.ErrAITransient
		case anthropicsdk.ErrorTypeAuthenticationError,
			anthropicsdk.ErrorTypePermissionError:
			code = pierrors.ErrAIUnauthorized
		default:
			code = HTTPStatusToCode(apiErr.StatusCode)
		}
	} else if IsTransientNetwork(err) {
		code = pierrors.ErrAITransient
	}
	return code.Wrap(err)
}

func anthropicFinishReason(reason anthropicsdk.StopReason) ai.FinishReason {
	switch reason {
	case anthropicsdk.StopReasonToolUse:
		return ai.FinishReasonToolUse
	case anthropicsdk.StopReasonMaxTokens:
		return ai.FinishReasonLength
	default:
		return ai.FinishReasonStop
	}
}
