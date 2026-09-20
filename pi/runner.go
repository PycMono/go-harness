package pi

import (
	"context"
	"fmt"
	"strings"

	"github.com/PycMono/go-harness/pi/ai"
	pierrors "github.com/PycMono/go-harness/pi/error"
	"github.com/PycMono/go-harness/pi/tools"
)

// MaxImagesPerMessage 是单条消息允许附加的图片数量上限。每张图按固定
const MaxImagesPerMessage = 4

// Runner 定义无状态 Agent 的单次运行行为。
type Runner interface {
	Run(context.Context, *RunInput) (*RunOutput, error)
}

// Message 表示调用方传入的一条业务消息。
type Message struct {
	// ContentType 表示消息内容类型
	ContentType string
	// CreateTS 是调用方提供的创建时间戳。
	CreateTS string
	// FileURL 是调用方提供的文件地址；
	FileURL string
	// TalkerName 是消息发送方的展示名称
	SenderName string
	// Content 是消息正文。
	Content string
	// ImageURLs 是可选附加的图片 URL 列表；正文必填，图片以 image 块追加在
	// 文本块之后发送给模型。URL 必须为 http/https。
	ImageURLs []string
	// SenderType 表示消息由 AI 或客户发送。
	SenderType string
}

// Message2AI 校验业务消息并转换为模型内部消息。
func (message Message) Message2AI() (*ai.Message, error) {
	if message.ContentType != "text" {
		return nil, pierrors.ErrRequestInvalid.Wrap(fmt.Errorf(
			"message content type must be %q, got %q",
			"text",
			message.ContentType,
		))
	}
	if strings.TrimSpace(message.Content) == "" {
		return nil, pierrors.ErrRequestInvalid.Wrap(
			fmt.Errorf("message content must not be empty"))
	}

	var role ai.Role
	switch message.SenderType {
	case "customer":
		role = ai.RoleUser
	case "ai":
		role = ai.RoleAssistant
	default:
		return nil, pierrors.ErrRequestInvalid.Wrap(fmt.Errorf(
			"unsupported message sender type %q",
			message.SenderType,
		))
	}

	content := []tools.ContentBlock{tools.TextBlock(message.Content)}
	if len(message.ImageURLs) > 0 {
		if role != ai.RoleUser {
			return nil, pierrors.ErrRequestInvalid.Wrap(
				fmt.Errorf("only customer messages may attach images"))
		}
		if len(message.ImageURLs) > MaxImagesPerMessage {
			return nil, pierrors.ErrRequestInvalid.Wrap(fmt.Errorf(
				"at most %d images per message, got %d",
				MaxImagesPerMessage, len(message.ImageURLs)))
		}
		for _, imageURL := range message.ImageURLs {
			block := tools.ImageBlock(imageURL)
			if err := block.Validate(); err != nil {
				return nil, pierrors.ErrRequestInvalid.Wrap(err)
			}
			content = append(content, block)
		}
	}
	return &ai.Message{Role: role, Content: content}, nil
}

type RunInput struct {
	History []*Message // History 是本轮运行开始前、面向业务的文本会话历史。
	Input   *Message
	Context []*ContextBlock // Context 是本轮额外注入的业务上下文。
}

// ContextBlock 表示运行时注入到会话历史之前的一段业务上下文。
type ContextBlock struct {
	// Name 是上下文名称。
	Name string `json:"name"`
	// Content 是上下文内容。
	Content string `json:"content"`
	// Priority 决定上下文的排列顺序，数值越大越靠前。
	Priority int `json:"priority,omitempty"`
}

func (r *RunInput) Validate() error {
	for index, block := range r.Context {
		if strings.TrimSpace(block.Name) == "" {
			return pierrors.ErrRequestInvalid.Wrap(fmt.Errorf("context block %d name must not be empty", index))
		}
		if strings.TrimSpace(block.Content) == "" {
			return pierrors.ErrRequestInvalid.Wrap(fmt.Errorf("context block %d content must not be empty", index))
		}
	}

	return nil
}

type RunOutput struct {
	message ai.Messages
}
