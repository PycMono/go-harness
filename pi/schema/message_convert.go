package schema

import (
	"encoding/json"
	"fmt"
	"strings"

	anthropicsdk "github.com/anthropics/anthropic-sdk-go"
	openaisdk "github.com/openai/openai-go/v3"
)

// 本文件承载到 OpenAI / Anthropic 两套协议的转换，以及服务于转换的图片与工具结果
// 投影。这是本包唯一同时 import 两个模型 SDK 的文件；内容块自身的定义与校验在
// message_content.go，消息联合与序列级校验在 message.go。

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
			text, err := typed.Content.openAIToolResultText()
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
					blocks = append(blocks, block.Image.anthropicImageBlock())
				}
			}
			result = append(result, anthropicsdk.NewUserMessage(blocks...))
		case *ToolResultMessage:
			content, err := typed.Content.anthropicToolResultContent()
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
					ImageURL: openaisdk.ChatCompletionContentPartImageImageURLParam{URL: block.Image.imageInputURL()},
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
func (blocks ContentBlocks) openAIToolResultText() (string, error) {
	var text strings.Builder
	hasImage := false
	for _, block := range blocks {
		switch block.Type {
		case ContentTypeText:
			text.WriteString(block.Text)
		case ContentTypeImage:
			// 图片本身不进入 tool 消息，但缺 Image 的脏块要和别处一样被拒。
			if _, err := block.toolResultImageURL(); err != nil {
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

// anthropicToolResultContent 把工具结果的内容块投影成 tool_result 的内容成员，形状
// 对齐 pi.dev 的 convertContentBlocks（anthropic-messages.ts:128-176），三种情况：
//
//   - 没有图片块：全部文本块按顺序拼成一个 text 成员（拼接口径与 Content.Text() 一致，
//     不带分隔符）。空内容也保留这一个空 text 成员，与旧的 NewToolResultBlock 产物
//     逐字段一致。
//   - 有图片块：按内容顺序每个块各自成一个成员——文本块不合并，排在图片之后的文本块
//     也不会被挪到图片前面，内容顺序原样保留。
//   - 有图片块但没有文本块：在最前面补一个 "(see attached image)" 文本成员，作为图片的
//     锚点，不发出没有文本锚点的裸图片 content。判据是"有没有文本块"，空文本块照样算。
//
// Anthropic 的 tool_result 本身就接受图片成员（ToolResultBlockParamContentUnion.OfImage），
// 所以这条路径不降级、不丢图。两种分支都拒绝未知块类型，没有图片时也不会静默吞掉脏块。
func (blocks ContentBlocks) anthropicToolResultContent() ([]anthropicsdk.ToolResultBlockParamContentUnion, error) {
	hasImage := false
	for _, block := range blocks {
		if block.Type == ContentTypeImage {
			hasImage = true
			break
		}
	}

	if !hasImage {
		var text strings.Builder
		for _, block := range blocks {
			switch block.Type {
			case ContentTypeText:
				text.WriteString(block.Text)
			default:
				return nil, fmt.Errorf("unsupported content type %q", block.Type)
			}
		}
		return []anthropicsdk.ToolResultBlockParamContentUnion{
			{OfText: &anthropicsdk.TextBlockParam{Text: text.String()}},
		}, nil
	}

	content := make([]anthropicsdk.ToolResultBlockParamContentUnion, 0, len(blocks)+1)
	hasText := false
	for _, block := range blocks {
		switch block.Type {
		case ContentTypeText:
			hasText = true
			content = append(content, anthropicsdk.ToolResultBlockParamContentUnion{
				OfText: &anthropicsdk.TextBlockParam{Text: block.Text},
			})
		case ContentTypeImage:
			imageURL, err := block.toolResultImageURL()
			if err != nil {
				return nil, err
			}
			content = append(content, anthropicsdk.ToolResultBlockParamContentUnion{
				OfImage: block.Image.anthropicToolImageBlock(imageURL),
			})
		default:
			return nil, fmt.Errorf("unsupported content type %q", block.Type)
		}
	}

	if !hasText {
		content = append([]anthropicsdk.ToolResultBlockParamContentUnion{
			{OfText: &anthropicsdk.TextBlockParam{Text: "(see attached image)"}},
		}, content...)
	}

	return content, nil
}

// toolResultImageURL 取图片块的 URL；缺 Image 的脏块按内容块校验的措辞报错。
// 图片投影的两条路径都要挡住它，避免取 URL 时 panic。
func (block ContentBlock) toolResultImageURL() (string, error) {
	if block.Image == nil {
		return "", fmt.Errorf("image block requires image content")
	}

	return block.Image.imageInputURL(), nil
}

func (image *ImageContent) imageInputURL() string {
	if image == nil {
		return ""
	}
	if image.Data != "" {
		return "data:" + image.MIMEType + ";base64," + image.Data
	}
	return image.URL
}

func (image *ImageContent) anthropicImageBlock() anthropicsdk.ContentBlockParamUnion {
	if image != nil && image.Data != "" {
		return anthropicsdk.NewImageBlock(anthropicsdk.Base64ImageSourceParam{
			MediaType: anthropicsdk.Base64ImageSourceMediaType(image.MIMEType),
			Data:      image.Data,
		})
	}
	return anthropicsdk.NewImageBlock(anthropicsdk.URLImageSourceParam{URL: image.URL})
}

func (image *ImageContent) anthropicToolImageBlock(imageURL string) *anthropicsdk.ImageBlockParam {
	if image != nil && image.Data != "" {
		return &anthropicsdk.ImageBlockParam{Source: anthropicsdk.ImageBlockParamSourceUnion{
			OfBase64: &anthropicsdk.Base64ImageSourceParam{MediaType: anthropicsdk.Base64ImageSourceMediaType(image.MIMEType), Data: image.Data},
		}}
	}
	return &anthropicsdk.ImageBlockParam{Source: anthropicsdk.ImageBlockParamSourceUnion{
		OfURL: &anthropicsdk.URLImageSourceParam{URL: imageURL},
	}}
}
