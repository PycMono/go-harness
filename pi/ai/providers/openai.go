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
	// supportsImageInput 来自 Options.SupportsImageInput，决定工具结果的图片要不要
	// 以合成 user 消息补发。
	supportsImageInput bool
}

func NewOpenAI(opts *Options) ai.Provider {
	return &OpenAIImpl{
		client: openaisdk.NewClient(
			option.WithAPIKey(opts.APIKey),
			option.WithBaseURL(opts.BaseURL),
			option.WithMaxRetries(0),
		),
		model:              opts.Model,
		name:               opts.ID,
		supportsImageInput: opts.SupportsImageInput,
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
	// 工具结果的图片没有 tool 消息位置可去，转换之后再按模型能力补一条合成 user 消息。
	openAIMessages = insertToolResultImages(msgs, openAIMessages, o.supportsImageInput)

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

// toolResultImageNotice 是合成 user 消息的锚点文本，字面量照搬 pi.dev
// （openai-completions.ts:1453）。
const toolResultImageNotice = "Attached image(s) from tool result:"

// toolResultImageBatch 是一段连续工具结果运行里收集到的图片。
type toolResultImageBatch struct {
	// toolCallID 是这段运行最后一条工具结果的 tool_call_id，合成 user 消息插在它后面。
	toolCallID string
	// urls 是按"消息顺序 → 块顺序"收集到的图片 URL。
	urls []string
}

// insertToolResultImages 给携带图片的工具结果补一条合成 user 消息。OpenAI 的 tool
// 消息只有文本位置，图片进不去；补发形状与插入位置对齐 pi.dev 的
// openai-completions.ts:1396-1458：
//
//   - 运行（run）是 msgs 里极大的一段连续 ToolResultMessage。图片跨整段收集，所以两条
//     相邻的带图工具结果只补一条 user 消息，不是两条；结果顺序是
//     assistant(tool_calls)、tool、tool、user（计划第 118 行钉住的顺序）。
//   - 只有 supportsImageInput 为真、且这段运行至少收到一张图时才补。开关为假时一条都
//     不补，图片由任务三给工具消息的文本兜底（有文本用文本、无文本有图用
//     "(see attached image)"），不会静默丢图。
//   - 合成消息的内容是锚点文本加每张图一个 image_url 成员，URL 逐字取
//     Image.URL——schema.ImageContent 只有 URL，没有 base64 与 MIME，所以这不是
//     pi.dev 的 data: URI。
//   - 不补 pi.dev 那条可选的 "I have processed the tool results." 助手消息：它由
//     compat.requiresAssistantAfterToolResult 控制（:1441-1446），go-harness 没有这个
//     兼容位，计划钉住的顺序就是不含它的那一种。
//   - 插入点按 tool_call_id 匹配 OfTool 分支，不按下标：下标对齐是没有任何东西保证的
//     不变量，一旦破了就是静默的错序错图。
//
// 包级函数，不碰协议转换也不读 provider，测试可以直接喂消息与参数。
func insertToolResultImages(
	msgs schema.Messages,
	params []openaisdk.ChatCompletionMessageParamUnion,
	supportsImageInput bool,
) []openaisdk.ChatCompletionMessageParamUnion {
	if !supportsImageInput {
		return params
	}

	batches := collectToolResultImageBatches(msgs)
	if len(batches) == 0 {
		return params
	}

	// 按 params 的顺序单趟插：批次与参数里 tool_call_id 的出现顺序一致，所以游标只前进。
	// 某个批次在 params 里找不到对应 tool 消息时后面的批次也不会插——那说明 msgs 与
	// params 不是同一次转换的产物，属于上游 bug，不该在这里猜着补。
	result := make([]openaisdk.ChatCompletionMessageParamUnion, 0, len(params)+len(batches))
	next := 0
	for _, param := range params {
		result = append(result, param)
		if next < len(batches) && param.OfTool != nil &&
			param.OfTool.ToolCallID == batches[next].toolCallID {
			result = append(result, toolResultImageUserMessage(batches[next].urls))
			next++
		}
	}

	return result
}

// collectToolResultImageBatches 扫出所有至少收到一张图的工具结果运行，按出现顺序返回。
func collectToolResultImageBatches(msgs schema.Messages) []toolResultImageBatch {
	batches := make([]toolResultImageBatch, 0)
	for index := 0; index < len(msgs); {
		if _, ok := msgs[index].(*schema.ToolResultMessage); !ok {
			index++
			continue
		}

		batch := toolResultImageBatch{}
		for ; index < len(msgs); index++ {
			tool, ok := msgs[index].(*schema.ToolResultMessage)
			if !ok {
				break
			}
			batch.toolCallID = tool.ToolCallID
			batch.urls = append(batch.urls, toolResultImageURLs(tool.Content)...)
		}
		if len(batch.urls) > 0 {
			batches = append(batches, batch)
		}
	}

	return batches
}

// toolResultImageURLs 按块顺序取出一条工具结果里的图片 URL。image 块缺 Image 时跳过
// 而不是 panic：这种脏块在上游 Messages.Validate 就被拒了，这里只是不让它把整条合成
// 消息带塌。
func toolResultImageURLs(blocks schema.ContentBlocks) []string {
	var urls []string
	for _, block := range blocks {
		if block.Type == schema.ContentTypeImage && block.Image != nil {
			urls = append(urls, block.Image.URL)
		}
	}

	return urls
}

// toolResultImageUserMessage 组装携带工具结果图片的合成 user 消息，成员形状与既有的
// 用户图片路径一致（schema 的 toOpenAIUserMessage）：锚点文本在前，之后每张图一个
// image_url 成员。
func toolResultImageUserMessage(imageURLs []string) openaisdk.ChatCompletionMessageParamUnion {
	parts := make([]openaisdk.ChatCompletionContentPartUnionParam, 0, len(imageURLs)+1)
	parts = append(parts, openaisdk.ChatCompletionContentPartUnionParam{
		OfText: &openaisdk.ChatCompletionContentPartTextParam{Text: toolResultImageNotice},
	})
	for _, imageURL := range imageURLs {
		parts = append(parts, openaisdk.ChatCompletionContentPartUnionParam{
			OfImageURL: &openaisdk.ChatCompletionContentPartImageParam{
				ImageURL: openaisdk.ChatCompletionContentPartImageImageURLParam{URL: imageURL},
			},
		})
	}

	return openaisdk.UserMessage(parts)
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
	result := &schema.AssistantMessage{
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
