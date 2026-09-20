package ai

import (
	"encoding/json"
	"fmt"

	"github.com/PycMono/go-harness/pi/tools"
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

// Message 表示对话上下文中传递的一条消息。
type Message struct {
	// Role 表示消息角色。
	Role Role `json:"role"`
	// Content 保存消息的内容块。
	Content tools.ContentBlocks `json:"content,omitempty"`
	// Usage 保存生成当前模型消息时产生的用量信息。
	Usage *Usage `json:"usage,omitempty"`
	// FinishReason 保存模型结束当前响应的统一原因。
	FinishReason FinishReason `json:"finish_reason,omitempty"`
	// ToolCalls 保存模型请求执行的工具调用，允许同时包含多个调用。
	ToolCalls tools.ToolCalls `json:"tool_calls,omitempty"`
	// ToolCallID 保存当前工具结果所对应的工具调用 ID。
	ToolCallID string `json:"tool_call_id,omitempty"`
	// ToolName 保存当前工具结果所对应的工具名称。
	ToolName string `json:"tool_name,omitempty"`
	// IsError 表示当前工具结果是否为错误结果。
	IsError bool `json:"is_error,omitempty"`
}

type Messages []*Message

// Validate 校验整个消息序列：角色必须已知，tool 消息必须携带 ToolCallID，
// 内容块必须合法且只允许 user 消息携带图片。Provider 在入口边界统一调用，
// 非法输入在协议转换前拦截，避免落成平台侧的模糊错误。
func (m Messages) Validate() error {
	for _, message := range m {
		switch message.Role {
		case RoleSystem, RoleUser, RoleAssistant, RoleTool:
		default:
			return fmt.Errorf("unsupported message role %q", message.Role)
		}
		if message.Role == RoleTool && message.ToolCallID == "" {
			return fmt.Errorf("tool message requires tool_call_id")
		}
		if err := validateContentForRole(message.Content, message.Role); err != nil {
			return fmt.Errorf("message role %q: %w", message.Role, err)
		}
	}
	return nil
}

// validateContentForRole 先校验内容块自身的合法性，再强制只有 user 消息
// 可以携带图片。内容块合法性归 pi/tools，角色相关的限制是消息层策略，
// 因此留在本包，避免 tools 反向依赖 Role。
func validateContentForRole(blocks tools.ContentBlocks, role Role) error {
	if err := blocks.Validate(); err != nil {
		return err
	}
	if role == RoleUser {
		return nil
	}
	for _, block := range blocks {
		if block.Type == tools.ContentTypeImage {
			return fmt.Errorf("role %q must not carry image blocks", role)
		}
	}
	return nil
}

func (m Messages) ToOpenAIMessages() ([]openaisdk.ChatCompletionMessageParamUnion, error) {
	result := make([]openaisdk.ChatCompletionMessageParamUnion, 0, len(m))
	for _, message := range m {
		switch message.Role {
		case RoleSystem:
			text, err := message.Content.Text()
			if err != nil {
				return nil, err
			}
			result = append(result, openaisdk.SystemMessage(text))
		case RoleUser:
			user, err := message.toOpenAIUserMessage()
			if err != nil {
				return nil, err
			}
			result = append(result, user)
		case RoleTool:
			text, err := message.Content.Text()
			if err != nil {
				return nil, err
			}
			result = append(result, openaisdk.ToolMessage(text, message.ToolCallID))
		case RoleAssistant:
			text, err := message.Content.Text()
			if err != nil {
				return nil, err
			}
			assistant := openaisdk.ChatCompletionAssistantMessageParam{}
			if text != "" {
				assistant.Content = openaisdk.ChatCompletionAssistantMessageParamContentUnion{OfString: openaisdk.String(text)}
			}
			for _, toolCall := range message.ToolCalls {
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
		switch message.Role {
		case RoleSystem:
			text, err := message.Content.Text()
			if err != nil {
				return nil, nil, err
			}
			system = append(system, anthropicsdk.TextBlockParam{Text: text})
		case RoleUser:
			var blocks []anthropicsdk.ContentBlockParamUnion
			for _, block := range message.Content {
				switch block.Type {
				case tools.ContentTypeText:
					blocks = append(blocks, anthropicsdk.NewTextBlock(block.Text))
				case tools.ContentTypeImage:
					blocks = append(blocks, anthropicsdk.NewImageBlock(anthropicsdk.URLImageSourceParam{URL: block.Image.URL}))
				}
			}
			result = append(result, anthropicsdk.NewUserMessage(blocks...))
		case RoleTool:
			text, err := message.Content.Text()
			if err != nil {
				return nil, nil, err
			}
			result = append(result, anthropicsdk.NewUserMessage(
				anthropicsdk.NewToolResultBlock(message.ToolCallID, text, message.IsError),
			))
		case RoleAssistant:
			text, err := message.Content.Text()
			if err != nil {
				return nil, nil, err
			}
			var blocks []anthropicsdk.ContentBlockParamUnion
			if text != "" {
				blocks = append(blocks, anthropicsdk.NewTextBlock(text))
			}

			for _, toolCall := range message.ToolCalls {
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
func (m *Message) toOpenAIUserMessage() (openaisdk.ChatCompletionMessageParamUnion, error) {
	hasImage := false
	for _, block := range m.Content {
		if block.Type == tools.ContentTypeImage {
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
		case tools.ContentTypeText:
			parts = append(parts, openaisdk.ChatCompletionContentPartUnionParam{
				OfText: &openaisdk.ChatCompletionContentPartTextParam{Text: block.Text},
			})
		case tools.ContentTypeImage:
			parts = append(parts, openaisdk.ChatCompletionContentPartUnionParam{
				OfImageURL: &openaisdk.ChatCompletionContentPartImageParam{
					ImageURL: openaisdk.ChatCompletionContentPartImageImageURLParam{URL: block.Image.URL},
				},
			})
		}
	}

	return openaisdk.UserMessage(parts), nil
}
