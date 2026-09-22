// Package schema 定义与模型平台无关的消息、内容块、工具 Schema、用量与流事件
// 表示，并负责到 OpenAI / Anthropic 两套协议的转换。本包不依赖 SDK 内任何
// 其他包。文件划分：message.go 承载消息这一个层次（Message 接口、四个具体类型
// 与 Messages 序列）及其本地校验，message_json.go 承载一条消息的线格式（与旧
// 结构体逐字节一致），message_content.go 承载内容块值类型，message_convert.go
// 承载到 OpenAI / Anthropic 的双协议转换，usage.go 承载用量与流事件，tools.go
// 承载工具侧（工具 Schema 与调用参数）。
package schema

import "fmt"

// 本文件承载消息这一个层次：Message 接口、四个具体类型与 Messages 序列，各自的
// 构造函数与本地校验，以及判别联合用到的 Role 与结束原因 FinishReason。一条消息
// 的线格式在 message_json.go，内容块值类型在 message_content.go，双协议转换在
// message_convert.go。各具体类型只拥有自己角色的字段，角色由 Role 方法给出而不是
// 结构体字段——同一类型上不能既有 Role 字段又有 Role 方法，字段集合也因此不会互相
// 串味。

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

// Message 表示对话上下文中的一条消息，取值必为 SystemMessage、UserMessage、
// AssistantMessage、ToolResultMessage 之一。message 是私有标记方法，把实现
// 限制在本包内，因此类型开关只需列出这四种。
type Message interface {
	// Role 返回本条消息的角色，也是判别联合的判别依据。
	Role() Role
	// Blocks 返回本条消息的内容块。只读展示、降级投影和日志这些不关心具体变体、
	// 只要内容的路径用它，免去每个调用点各写一遍类型分支。叫 Blocks 而不是
	// Content，是因为四个具体类型上都有 Content 字段，Go 不允许同名字段与方法
	// 共存。接口本身为 nil 时不能调用；可能拿到 nil 的路径（如 header entry 的
	// 空消息）由调用方先判空。
	Blocks() ContentBlocks
	// Validate 只校验本条消息自己的字段；角色已由具体类型确定，不重复传入。
	Validate() error
	// message 是私有标记方法，封闭本接口的实现集合。
	message()
}

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

// SystemMessage 表示系统提示词消息，只接受纯文本。
type SystemMessage struct {
	// Content 保存消息的内容块。
	Content ContentBlocks `json:"content,omitempty"`
}

// NewSystemMessage 构造 system 消息并跑一次本地校验。
func NewSystemMessage(content ContentBlocks) (Message, error) {
	message := &SystemMessage{Content: content}
	if err := message.Validate(); err != nil {
		return nil, err
	}
	return message, nil
}

// Role 返回 RoleSystem。
func (m *SystemMessage) Role() Role { return RoleSystem }

// Blocks 返回消息的内容块。
func (m *SystemMessage) Blocks() ContentBlocks { return m.Content }

func (m *SystemMessage) message() {}

// Validate 校验内容块，并拒绝图片块：系统提示词没有多模态输入的位置。
func (m *SystemMessage) Validate() error {
	if err := m.Content.Validate(); err != nil {
		return err
	}
	for _, block := range m.Content {
		if block.Type == ContentTypeImage {
			return fmt.Errorf("role %q must not carry image blocks", RoleSystem)
		}
	}
	return nil
}

// UserMessage 表示用户输入消息，允许携带图片块。
type UserMessage struct {
	// Content 保存消息的内容块。
	Content ContentBlocks `json:"content,omitempty"`
}

// NewUserMessage 构造 user 消息并跑一次本地校验。
func NewUserMessage(content ContentBlocks) (Message, error) {
	message := &UserMessage{Content: content}
	if err := message.Validate(); err != nil {
		return nil, err
	}
	return message, nil
}

// Role 返回 RoleUser。
func (m *UserMessage) Role() Role { return RoleUser }

// Blocks 返回消息的内容块。
func (m *UserMessage) Blocks() ContentBlocks { return m.Content }

func (m *UserMessage) message() {}

// Validate 校验内容块；用户输入是图片块的合法归属，不再附加角色限制。
func (m *UserMessage) Validate() error {
	return m.Content.Validate()
}

// AssistantMessage 表示模型输出消息，拒绝图片块，并可携带用量、结束原因与
// 工具调用。
type AssistantMessage struct {
	// Content 保存消息的内容块。
	Content ContentBlocks `json:"content,omitempty"`
	// Usage 保存生成当前消息时产生的用量信息。
	Usage *Usage `json:"usage,omitempty"`
	// FinishReason 保存模型结束当前响应的统一原因。
	FinishReason FinishReason `json:"finish_reason,omitempty"`
	// ToolCalls 保存模型请求执行的工具调用，允许同时包含多个调用。
	ToolCalls ToolCalls `json:"tool_calls,omitempty"`
}

// NewAssistantMessage 构造 assistant 消息并跑一次本地校验。
func NewAssistantMessage(
	content ContentBlocks, usage *Usage, finishReason FinishReason, toolCalls ToolCalls,
) (Message, error) {
	message := &AssistantMessage{
		Content: content, Usage: usage, FinishReason: finishReason, ToolCalls: toolCalls,
	}
	if err := message.Validate(); err != nil {
		return nil, err
	}
	return message, nil
}

// Role 返回 RoleAssistant。
func (m *AssistantMessage) Role() Role { return RoleAssistant }

// Blocks 返回消息的内容块。
func (m *AssistantMessage) Blocks() ContentBlocks { return m.Content }

func (m *AssistantMessage) message() {}

// Validate 校验内容块，并拒绝图片块：模型输出一侧没有图片输入。
func (m *AssistantMessage) Validate() error {
	if err := m.Content.Validate(); err != nil {
		return err
	}
	for _, block := range m.Content {
		if block.Type == ContentTypeImage {
			return fmt.Errorf("role %q must not carry image blocks", RoleAssistant)
		}
	}
	return nil
}

// ToolResultMessage 表示工具执行结果消息，回填给发起调用的那次 ToolCall。
type ToolResultMessage struct {
	// Content 保存消息的内容块，允许携带图片块。
	Content ContentBlocks `json:"content,omitempty"`
	// ToolCallID 保存当前工具结果所对应的工具调用 ID。
	ToolCallID string `json:"tool_call_id,omitempty"`
	// ToolName 保存当前工具结果所对应的工具名称。
	ToolName string `json:"tool_name,omitempty"`
	// IsError 表示当前工具结果是否为错误结果。
	IsError bool `json:"is_error,omitempty"`
}

// NewToolResultMessage 构造工具结果消息并跑一次本地校验。
func NewToolResultMessage(
	content ContentBlocks, toolCallID, toolName string, isError bool,
) (Message, error) {
	message := &ToolResultMessage{
		Content: content, ToolCallID: toolCallID, ToolName: toolName, IsError: isError,
	}
	if err := message.Validate(); err != nil {
		return nil, err
	}
	return message, nil
}

// Role 返回 RoleTool。
func (m *ToolResultMessage) Role() Role { return RoleTool }

// Blocks 返回消息的内容块。
func (m *ToolResultMessage) Blocks() ContentBlocks { return m.Content }

func (m *ToolResultMessage) message() {}

// Validate 先校验工具身份，再校验内容块。身份优先与旧的序列级校验顺序一致：
// 调用 ID 与工具名称共同决定这条结果回填给哪次调用，缺一个都不算结果。
// 图片块在这里合法，工具结果允许返回图片。
func (m *ToolResultMessage) Validate() error {
	if m.ToolCallID == "" {
		return fmt.Errorf("tool message requires tool_call_id")
	}
	if m.ToolName == "" {
		return fmt.Errorf("tool message requires tool_name")
	}
	return m.Content.Validate()
}
