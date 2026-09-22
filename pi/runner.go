package pi

import (
	"context"
	"fmt"
	"strings"

	pierrors "github.com/PycMono/go-harness/pi/error"
	"github.com/PycMono/go-harness/pi/schema"
)

// MaxImagesPerMessage 是单条用户输入允许附加的图片数量上限。
const MaxImagesPerMessage = 4

// Runner 定义 Agent 的单次运行行为。
type Runner interface {
	Run(context.Context, *RunInput) (*RunOutput, error)
}

// RunInput 是一次运行的当前输入。历史由 Options.Session 管理，不由调用方传入。
type RunInput struct {
	// Prompt 是本轮用户输入文本。
	Prompt string
	// Images 是本轮用户输入附带的图片。
	Images []schema.ImageContent
}

// ContextBlock 表示 ContextBuilder 可使用的一段业务上下文；它不是 RunInput 的参数，
// 保留该类型是为了让内部上下文组装和离线测试使用明确的结构。
type ContextBlock struct {
	Name     string `json:"name"`
	Content  string `json:"content"`
	Priority int    `json:"priority,omitempty"`
}

func (r *RunInput) Validate() error {
	if strings.TrimSpace(r.Prompt) == "" {
		return pierrors.ErrRequestInvalid.Wrap(fmt.Errorf("run input prompt must not be empty"))
	}
	if len(r.Images) > MaxImagesPerMessage {
		return pierrors.ErrRequestInvalid.Wrap(fmt.Errorf(
			"at most %d images per message, got %d", MaxImagesPerMessage, len(r.Images)))
	}
	for index := range r.Images {
		if err := r.Images[index].Validate(); err != nil {
			return pierrors.ErrRequestInvalid.Wrap(fmt.Errorf("image %d: %w", index, err))
		}
	}

	return nil
}

func (r *RunInput) promptMessage() (schema.Message, error) {
	content := schema.ContentBlocks{schema.TextBlock(r.Prompt)}
	for index := range r.Images {
		content = append(content, schema.ContentBlock{
			Type:  schema.ContentTypeImage,
			Image: new(r.Images[index]),
		})
	}

	return schema.NewUserMessage(content)
}

type RunOutput struct {
	message schema.Messages
}

// Messages 返回本轮运行产生的完整消息序列。
func (r *RunOutput) Messages() schema.Messages {
	return r.message
}

// Message 返回本轮最后一条助手消息；没有助手消息时返回 nil。
func (r *RunOutput) Message() schema.Message {
	for index := len(r.message) - 1; index >= 0; index-- {
		if r.message[index].Role() == schema.RoleAssistant {
			return r.message[index]
		}
	}

	return nil
}

// Text 返回最终助手消息的纯文本内容；没有助手消息或内容不是纯文本时返回空串。
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
