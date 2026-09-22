// Package schema 定义与模型平台无关的消息、内容块、工具 Schema、用量与流事件
// 表示，并负责到 OpenAI / Anthropic 两套协议的转换。本包不依赖 SDK 内任何
// 其他包。文件划分：protocol.go 承载消息侧（序列、内容块、用量、事件），
// message_variants.go 承载消息的四元联合类型及其构造与本地校验，
// message_json.go 承载消息的 JSON 编解码（线格式与旧结构体逐字节一致），
// tools.go 承载工具侧（工具 Schema 与调用参数）。
package schema

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	anthropicsdk "github.com/anthropics/anthropic-sdk-go"
	openaisdk "github.com/openai/openai-go/v3"
)

// Role 表示消息在大模型对话中的角色。
type Role string

const (
	RoleSystem    Role = "system"    // RoleSystem 表示系统提示词。
	RoleUser      Role = "user"      // RoleUser 表示用户输入。
	RoleAssistant Role = "assistant" // RoleAssistant 表示模型输出。
	RoleTool      Role = "tool"      // RoleTool 表示工具执行结果。
)

// FinishReason 表示模型结束当前响应的原因。
type FinishReason string

const (
	FinishReasonStop    FinishReason = "stop"
	FinishReasonToolUse FinishReason = "tool_use"
	FinishReasonLength  FinishReason = "length"
)

// Messages 是一条对话消息序列，元素为 Message 联合值。
type Messages []Message

// Validate 校验整个消息序列：角色必须已知，每条消息校验自己的字段（工具结果
// 的 tool_call_id 与 tool_name 由 ToolResultMessage.Validate 负责，图片规则由
// 各角色自己的 Validate 负责）。Provider 在入口边界统一调用，非法输入在协议
// 转换前拦截，避免落成平台侧的模糊错误。
func (m Messages) Validate() error {
	for _, message := range m {
		// 封闭联合下未知角色已不可表达，这条检查保留的是"角色必须已知"的
		// 序列级契约本身：入口不接受来路不明的角色。
		role := message.Role()
		switch role {
		case RoleSystem, RoleUser, RoleAssistant, RoleTool:
		default:
			return fmt.Errorf("unsupported message role %q", role)
		}
		if err := message.Validate(); err != nil {
			return fmt.Errorf("message role %q: %w", role, err)
		}
	}
	return nil
}

func (m Messages) ToOpenAIMessages() ([]openaisdk.ChatCompletionMessageParamUnion, error) {
	result := make([]openaisdk.ChatCompletionMessageParamUnion, 0, len(m))
	for _, message := range m {
		switch typed := message.(type) {
		case *SystemMessage:
			text, err := typed.Content.Text()
			if err != nil {
				return nil, err
			}
			result = append(result, openaisdk.SystemMessage(text))
		case *UserMessage:
			user, err := typed.toOpenAIUserMessage()
			if err != nil {
				return nil, err
			}
			result = append(result, user)
		case *ToolResultMessage:
			text, err := openAIToolResultText(typed.Content)
			if err != nil {
				return nil, err
			}
			result = append(result, openaisdk.ToolMessage(text, typed.ToolCallID))
		case *AssistantMessage:
			text, err := typed.Content.Text()
			if err != nil {
				return nil, err
			}
			assistant := openaisdk.ChatCompletionAssistantMessageParam{}
			if text != "" {
				assistant.Content = openaisdk.ChatCompletionAssistantMessageParamContentUnion{OfString: openaisdk.String(text)}
			}
			for _, toolCall := range typed.ToolCalls {
				assistant.ToolCalls = append(assistant.ToolCalls, openaisdk.ChatCompletionMessageToolCallUnionParam{
					OfFunction: &openaisdk.ChatCompletionMessageFunctionToolCallParam{
						ID: toolCall.ID,
						Function: openaisdk.ChatCompletionMessageFunctionToolCallFunctionParam{
							Name: toolCall.Name, Arguments: string(toolCall.Arguments),
						},
					},
				})
			}
			result = append(result, openaisdk.ChatCompletionMessageParamUnion{OfAssistant: &assistant})
		}
	}
	return result, nil
}

func (m Messages) ToAnthropicMessages() ([]anthropicsdk.MessageParam, []anthropicsdk.TextBlockParam, error) {
	result := make([]anthropicsdk.MessageParam, 0, len(m))
	var system []anthropicsdk.TextBlockParam

	for _, message := range m {
		switch typed := message.(type) {
		case *SystemMessage:
			text, err := typed.Content.Text()
			if err != nil {
				return nil, nil, err
			}
			system = append(system, anthropicsdk.TextBlockParam{Text: text})
		case *UserMessage:
			var blocks []anthropicsdk.ContentBlockParamUnion
			for _, block := range typed.Content {
				switch block.Type {
				case ContentTypeText:
					blocks = append(blocks, anthropicsdk.NewTextBlock(block.Text))
				case ContentTypeImage:
					blocks = append(blocks, anthropicsdk.NewImageBlock(anthropicsdk.URLImageSourceParam{URL: block.Image.URL}))
				}
			}
			result = append(result, anthropicsdk.NewUserMessage(blocks...))
		case *ToolResultMessage:
			content, err := anthropicToolResultContent(typed.Content)
			if err != nil {
				return nil, nil, err
			}
			result = append(result, anthropicsdk.NewUserMessage(
				anthropicsdk.ContentBlockParamUnion{OfToolResult: &anthropicsdk.ToolResultBlockParam{
					ToolUseID: typed.ToolCallID,
					IsError:   anthropicsdk.Bool(typed.IsError),
					Content:   content,
				}},
			))
		case *AssistantMessage:
			text, err := typed.Content.Text()
			if err != nil {
				return nil, nil, err
			}
			var blocks []anthropicsdk.ContentBlockParamUnion
			if text != "" {
				blocks = append(blocks, anthropicsdk.NewTextBlock(text))
			}

			for _, toolCall := range typed.ToolCalls {
				var input any
				if err = json.Unmarshal(toolCall.Arguments, &input); err != nil {
					return nil, nil, fmt.Errorf("tool call %q arguments: %w", toolCall.ID, err)
				}
				blocks = append(blocks, anthropicsdk.NewToolUseBlock(toolCall.ID, input, toolCall.Name))
			}
			result = append(result, anthropicsdk.NewAssistantMessage(blocks...))
		}
	}
	return result, system, nil
}

// toOpenAIUserMessage 映射 user 消息
func (m *UserMessage) toOpenAIUserMessage() (openaisdk.ChatCompletionMessageParamUnion, error) {
	hasImage := false
	for _, block := range m.Content {
		if block.Type == ContentTypeImage {
			hasImage = true
			break
		}
	}

	if !hasImage {
		text, err := m.Content.Text()
		if err != nil {
			return openaisdk.ChatCompletionMessageParamUnion{}, err
		}
		return openaisdk.UserMessage(text), nil
	}

	parts := make([]openaisdk.ChatCompletionContentPartUnionParam, 0, len(m.Content))
	for _, block := range m.Content {
		switch block.Type {
		case ContentTypeText:
			parts = append(parts, openaisdk.ChatCompletionContentPartUnionParam{
				OfText: &openaisdk.ChatCompletionContentPartTextParam{Text: block.Text},
			})
		case ContentTypeImage:
			parts = append(parts, openaisdk.ChatCompletionContentPartUnionParam{
				OfImageURL: &openaisdk.ChatCompletionContentPartImageParam{
					ImageURL: openaisdk.ChatCompletionContentPartImageImageURLParam{URL: block.Image.URL},
				},
			})
		}
	}

	return openaisdk.UserMessage(parts), nil
}

// openAIToolResultText 选出 OpenAI tool 消息的文本内容。OpenAI 的 tool 消息只有
// 文本位置，图片进不去，所以这条路径必须降级而不是报错。选词照搬 pi.dev 的三路
// 规则（openai-completions.ts:1409-1410）：有文本用文本（拼接口径与
// Content.Text() 一致）、没有文本但有图片用 "(see attached image)"、两者都没有用
// "(no tool output)"。后两个字符串是字面量，与脱敏占位 ImagePlaceholderText 无
// 关——那是摘要与日志路径的词，不进 tool 消息。
func openAIToolResultText(blocks ContentBlocks) (string, error) {
	var text strings.Builder
	hasImage := false
	for _, block := range blocks {
		switch block.Type {
		case ContentTypeText:
			text.WriteString(block.Text)
		case ContentTypeImage:
			// 图片本身不进入 tool 消息，但缺 Image 的脏块要和别处一样被拒。
			if _, err := toolResultImageURL(block); err != nil {
				return "", err
			}
			hasImage = true
		default:
			return "", fmt.Errorf("unsupported content type %q", block.Type)
		}
	}

	switch {
	case text.Len() > 0:
		return text.String(), nil
	case hasImage:
		return "(see attached image)", nil
	default:
		return "(no tool output)", nil
	}
}

// anthropicToolResultContent 把工具结果的内容块投影成 tool_result 的内容成员：
// 文本合成一个 text 成员，图片成员按内容顺序排在它后面。Anthropic 的 tool_result
// 本身就接受图片成员（ToolResultBlockParamContentUnion.OfImage），所以这条路径不
// 降级、不丢图。内容为空时保留一个空 text 成员，与旧的 NewToolResultBlock 产物
// 逐字段一致。
func anthropicToolResultContent(
	blocks ContentBlocks,
) ([]anthropicsdk.ToolResultBlockParamContentUnion, error) {
	var text strings.Builder
	images := make([]anthropicsdk.ToolResultBlockParamContentUnion, 0, len(blocks))
	for _, block := range blocks {
		switch block.Type {
		case ContentTypeText:
			text.WriteString(block.Text)
		case ContentTypeImage:
			imageURL, err := toolResultImageURL(block)
			if err != nil {
				return nil, err
			}
			images = append(images, anthropicsdk.ToolResultBlockParamContentUnion{
				OfImage: &anthropicsdk.ImageBlockParam{
					Source: anthropicsdk.ImageBlockParamSourceUnion{
						OfURL: &anthropicsdk.URLImageSourceParam{URL: imageURL},
					},
				},
			})
		default:
			return nil, fmt.Errorf("unsupported content type %q", block.Type)
		}
	}

	// 纯图片结果不塞空文本成员；其余情况（含空结果）都把文本成员放在最前面。
	if text.Len() == 0 && len(images) > 0 {
		return images, nil
	}

	return append([]anthropicsdk.ToolResultBlockParamContentUnion{
		{OfText: &anthropicsdk.TextBlockParam{Text: text.String()}},
	}, images...), nil
}

// toolResultImageURL 取图片块的 URL；缺 Image 的脏块按内容块校验的措辞报错。
// 图片投影的两条路径都要挡住它，避免取 URL 时 panic。
func toolResultImageURL(block ContentBlock) (string, error) {
	if block.Image == nil {
		return "", fmt.Errorf("image block requires image content")
	}

	return block.Image.URL, nil
}

// ContentType 表示消息内容块的类型。
type ContentType string

// ContentTypeText 表示纯文本内容块。
const ContentTypeText ContentType = "text"

// ContentTypeImage 表示 URL 图像内容块。
const ContentTypeImage ContentType = "image"

// ContentBlocks is an ordered collection of message content blocks.
type ContentBlocks []ContentBlock

// ContentBlock 表示消息中的一个内容块。联合类型取值受 Validate 约束：
// text 块不得携带 Image，image 块只携带 Image 不携带 Text。
type ContentBlock struct {
	// Type 表示内容块的类型。
	Type ContentType `json:"type"`
	// Text 保存文本内容。
	Text string `json:"text,omitempty"`
	// Image 保存 URL 图像内容；仅 Type 为 ContentTypeImage 时非空。
	Image *ImageContent `json:"image,omitempty"`
}

// ImageContent 表示一个 URL 图像内容。
type ImageContent struct {
	// URL 是图像的可访问地址；调用方必须保证推理服务商可访问且生命周期足够长。
	URL string `json:"url"`
}

// TextBlock 创建一个纯文本内容块。
func TextBlock(text string) ContentBlock {
	return ContentBlock{Type: ContentTypeText, Text: text}
}

// ImageBlock 创建一个 URL 图像内容块。
func ImageBlock(imageURL string) ContentBlock {
	return ContentBlock{Type: ContentTypeImage, Image: &ImageContent{URL: imageURL}}
}

// Validate 校验内容块的联合类型取值：text 块不得携带 Image，image 块必须
// 只携带合法 URL 的 Image，未知类型报错。校验集中在入口边界复用本函数，
// 不散落到使用方。
func (block ContentBlock) Validate() error {
	switch block.Type {
	case ContentTypeText:
		if block.Image != nil {
			return fmt.Errorf("text block must not carry an image")
		}
	case ContentTypeImage:
		if block.Text != "" {
			return fmt.Errorf("image block must not carry text")
		}
		if block.Image == nil {
			return fmt.Errorf("image block requires image content")
		}
		if err := block.Image.Validate(); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported content type %q", block.Type)
	}
	return nil
}

// Validate 校验图像内容的 URL：必须是带 host 的 http/https 地址。
func (image ImageContent) Validate() error {
	parsed, err := url.Parse(image.URL)
	if err != nil {
		return fmt.Errorf("image url %q: %w", image.URL, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("image url %q must use http or https", image.URL)
	}
	if parsed.Host == "" {
		return fmt.Errorf("image url %q requires a host", image.URL)
	}
	return nil
}

// Validate 校验内容块集合里的每个块及其联合类型取值。角色的图片规则不在这里
// 按角色分支：角色已由消息类型确定，由各具体类型的 Validate 各自附加。
func (blocks ContentBlocks) Validate() error {
	for _, block := range blocks {
		if err := block.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// Clone deep-copies the backing slice and Image pointers.
func (blocks ContentBlocks) Clone() ContentBlocks {
	if blocks == nil {
		return nil
	}
	cloned := make(ContentBlocks, len(blocks))
	for index, block := range blocks {
		cloned[index] = block
		if block.Image != nil {
			image := *block.Image
			cloned[index].Image = &image
		}
	}
	return cloned
}

// WithImagePlaceholders returns a copy where image blocks are replaced by
// redacted text placeholders.
func (blocks ContentBlocks) WithImagePlaceholders() ContentBlocks {
	result := make(ContentBlocks, 0, len(blocks))
	for _, block := range blocks {
		if block.Type == ContentTypeImage && block.Image != nil {
			result = append(result, TextBlock(ImagePlaceholderText(block.Image.URL)))
			continue
		}
		result = append(result, block)
	}
	return result
}

// ImagePlaceholderText 生成图像块的脱敏占位文本：只保留 scheme、host 与
// path，剥离查询参数与片段，避免签名、临时 Token 泄漏到模型上下文。降级
// 占位与压缩摘要投影共用本函数。
func ImagePlaceholderText(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return "[图片]"
	}
	brief := parsed.Scheme + "://" + parsed.Host + parsed.Path
	if parsed.Path == "" || parsed.Path == "/" {
		brief = parsed.Scheme + "://" + parsed.Host
	}
	return "[图片: " + brief + "]"
}

// Text concatenates text blocks in order and rejects non-text content.
func (blocks ContentBlocks) Text() (string, error) {
	var builder strings.Builder
	for _, block := range blocks {
		if block.Type != ContentTypeText {
			return "", fmt.Errorf("unsupported content type %q", block.Type)
		}
		builder.WriteString(block.Text)
	}
	return builder.String(), nil
}

// MaxUsageDecimalExclusive 是以 DECIMAL(20,12) 存储价格和单次调用成本时的上限，不包含该值本身。
const MaxUsageDecimalExclusive = 100_000_000

// CostQuality 是成本可信度枚举（设计 §9.1）。
type CostQuality string

const (
	// CostQualityExact 表示 Provider 分项足以按配置价格重算成本。
	CostQualityExact CostQuality = "exact"
	// CostQualityEstimated 表示成本只能估算。
	CostQualityEstimated CostQuality = "estimated"
)

// Usage 保存一次模型响应的标准化令牌用量、价格、成本和延迟数据。
type Usage struct {
	// InputTokens 是输入令牌数。
	InputTokens int64 `json:"input_tokens"`
	// OutputTokens 是输出令牌数。
	OutputTokens int64 `json:"output_tokens"`
	// InputPriceUSDPerMillionTokens 是每百万输入令牌的美元价格。
	InputPriceUSDPerMillionTokens float64 `json:"input_price_usd_per_million_tokens"`
	// OutputPriceUSDPerMillionTokens 是每百万输出令牌的美元价格。
	OutputPriceUSDPerMillionTokens float64 `json:"output_price_usd_per_million_tokens"`
	// CostUSD 是本次响应的美元成本。
	CostUSD float64 `json:"cost_usd"`
	// LatencyMS 是本次响应的延迟，单位为毫秒。
	LatencyMS int64 `json:"latency_ms"`
	// PlatformID 是提供模型服务的平台标识。
	PlatformID string `json:"platform_id"`
	// Model 是生成响应的模型名称。
	Model string `json:"model"`
	// TTFTMS 是首个非空 Text Delta 的延迟毫秒数；nil 表示未观测到
	// Text Delta（如纯 Tool Call 响应），0 表示已观测但不足 1ms（设计 §9.1）。
	TTFTMS *int64 `json:"ttft_ms,omitempty"`
	// CostQuality 表示成本可信度（§9.1）：exact 表示分项足以按配置价格
	// 重算；estimated 不能混入精确成本报表。缺省空值按 estimated 处理。
	CostQuality CostQuality `json:"cost_quality,omitempty"`
	// 是其子集；Output 是总输出，Reasoning 是其子集。
	// CacheReadTokens 是缓存读取令牌数。
	CacheReadTokens int64 `json:"cache_read_tokens,omitempty"`
	// CacheWriteTokens 是缓存写入令牌数。
	CacheWriteTokens int64 `json:"cache_write_tokens,omitempty"`
	// ReasoningTokens 是推理令牌数。
	ReasoningTokens int64 `json:"reasoning_tokens,omitempty"`
	// CacheReadPriceUSDPerMillionTokens 是每百万缓存读取令牌的美元价格。
	CacheReadPriceUSDPerMillionTokens float64 `json:"cache_read_price_usd_per_million_tokens,omitempty"`
	// CacheWritePriceUSDPerMillionTokens 是每百万缓存写入令牌的美元价格。
	CacheWritePriceUSDPerMillionTokens float64 `json:"cache_write_price_usd_per_million_tokens,omitempty"`
}

type StreamEventType string

const (
	StreamEventStart     StreamEventType = "start"
	StreamEventTextDelta StreamEventType = "text_delta"
	StreamEventDone      StreamEventType = "done"
	StreamEventError     StreamEventType = "error"
)

// StreamEvent 是与具体模型 SDK 无关的模型响应事件。
type StreamEvent struct {
	Type      StreamEventType
	TextDelta string
}
