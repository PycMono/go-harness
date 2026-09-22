package schema

import "fmt"

// 本文件承载消息的四元判别联合：Message 接口与四个具体类型，各自的构造函数
// 与本地校验。protocol.go 保留序列级校验与双协议转换。各具体类型只拥有自己
// 角色的字段，角色由 Role 方法给出而不是结构体字段——同一类型上不能既有
// Role 字段又有 Role 方法，字段集合也因此不会互相串味。

// Message 表示对话上下文中的一条消息，取值必为 SystemMessage、UserMessage、
// AssistantMessage、ToolResultMessage 之一。message 是私有标记方法，把实现
// 限制在本包内，因此类型开关只需列出这四种。
type Message interface {
	// Role 返回本条消息的角色，也是判别联合的判别依据。
	Role() Role
	// Validate 只校验本条消息自己的字段；角色已由具体类型确定，不重复传入。
	Validate() error
	// message 是私有标记方法，封闭本接口的实现集合。
	message()
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
