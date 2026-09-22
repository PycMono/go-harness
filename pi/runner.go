package pi

import (
	"context"
	"fmt"
	"strings"

	pierrors "github.com/PycMono/go-harness/pi/error"
	"github.com/PycMono/go-harness/pi/schema"
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

// Message2AI 校验业务消息并转换为模型内部消息。返回的是联合类型的接口：
// 校验失败时交回 nil 接口，而不是一个带类型的 nil 指针——后者在调用方眼里
// 依旧非 nil，会把"校验失败"读成"拿到了一条消息"。
func (message Message) Message2AI() (schema.Message, error) {
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

	var role schema.Role
	switch message.SenderType {
	case "customer":
		role = schema.RoleUser
	case "ai":
		role = schema.RoleAssistant
	default:
		return nil, pierrors.ErrRequestInvalid.Wrap(fmt.Errorf(
			"unsupported message sender type %q",
			message.SenderType,
		))
	}

	content := []schema.ContentBlock{schema.TextBlock(message.Content)}
	if len(message.ImageURLs) > 0 {
		if role != schema.RoleUser {
			return nil, pierrors.ErrRequestInvalid.Wrap(
				fmt.Errorf("only customer messages may attach images"))
		}
		if len(message.ImageURLs) > MaxImagesPerMessage {
			return nil, pierrors.ErrRequestInvalid.Wrap(fmt.Errorf(
				"at most %d images per message, got %d",
				MaxImagesPerMessage, len(message.ImageURLs)))
		}
		for _, imageURL := range message.ImageURLs {
			block := schema.ImageBlock(imageURL)
			if err := block.Validate(); err != nil {
				return nil, pierrors.ErrRequestInvalid.Wrap(err)
			}
			content = append(content, block)
		}
	}
	// 内容块在上面逐条校验过，构造函数不会再拦下什么；它按 role 落到对应的具体
	// 类型上，两条路径的失败都是 nil 接口。
	if role == schema.RoleUser {
		return schema.NewUserMessage(content)
	}

	return schema.NewAssistantMessage(content, nil, "", nil)
}

type RunInput struct {
	Input *Message
	// Context 是本轮额外注入的业务上下文。
	Context []*ContextBlock
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
	if r.Input == nil {
		return pierrors.ErrRequestInvalid.Wrap(fmt.Errorf("run input message must not be nil"))
	}
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
	message schema.Messages
}

// Messages 返回本轮运行产生的完整消息序列：组装好的上下文、每一轮模型消息、
// 以及每次工具调用的结果，按发生顺序排列。运行中途失败时序列同样有效，只到
// 出错前为止。
func (r *RunOutput) Messages() schema.Messages {
	return r.message
}

// Message 返回本轮最后一条模型消息，即面向调用方的最终答复；模型一次都没
// 有输出时返回 nil。返回的是接口，nil 必须是裸 nil——带类型的 nil 指针在这里
// 依旧非 nil，调用方的"没有答复"判定会静默走错分支。
func (r *RunOutput) Message() schema.Message {
	for index := len(r.message) - 1; index >= 0; index-- {
		if r.message[index].Role() == schema.RoleAssistant {
			return r.message[index]
		}
	}

	return nil
}

// Text 返回最终答复的纯文本内容；没有模型消息或内容不是纯文本时返回空字符串。
func (r *RunOutput) Text() string {
	message := r.Message()
	if message == nil {
		return ""
	}
	assistant, ok := message.(*schema.AssistantMessage)
	if !ok {
		return ""
	}
	text, err := assistant.Content.Text()
	if err != nil {
		return ""
	}

	return text
}
