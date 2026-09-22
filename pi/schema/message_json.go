package schema

import (
	"encoding/json"
	"fmt"
	"strings"
)

// 本文件承载消息联合的 JSON 编解码：四个具体类型各自的 MarshalJSON、按 role 分发
// 的 DecodeMessage，以及 Messages 序列的两个方法。线格式以重构前的 Message 结构体
// 为准：字段名、声明顺序与 omitempty 语义逐字对齐，盘上已有的会话文件不迁移即可
// 读取，重新编码后也是同一串字节。

// messageWire 是四个具体类型共用的线格式载体，取自旧 Message 结构体的字段与顺序。
// 编码时各具体类型只填自己拥有的字段（其余零值被 omitempty 省掉），解码时先解成
// 它再按 role 分发。**字段顺序就是盘上字节的键序**，role 在最前、其余按旧结构体
// 的声明顺序排列；调换顺序仍能读，但会让新写的行与已有的行不再逐字节一致。
type messageWire struct {
	Role         Role          `json:"role"`
	Content      ContentBlocks `json:"content,omitempty"`
	Usage        *Usage        `json:"usage,omitempty"`
	FinishReason FinishReason  `json:"finish_reason,omitempty"`
	ToolCalls    ToolCalls     `json:"tool_calls,omitempty"`
	ToolCallID   string        `json:"tool_call_id,omitempty"`
	ToolName     string        `json:"tool_name,omitempty"`
	IsError      bool          `json:"is_error,omitempty"`
}

// MarshalJSON 输出 system 消息的线格式：只有 role 与 content。
func (m *SystemMessage) MarshalJSON() ([]byte, error) {
	return json.Marshal(messageWire{Role: RoleSystem, Content: m.Content})
}

// MarshalJSON 输出 user 消息的线格式：只有 role 与 content（可含图片块）。
func (m *UserMessage) MarshalJSON() ([]byte, error) {
	return json.Marshal(messageWire{Role: RoleUser, Content: m.Content})
}

// MarshalJSON 输出 assistant 消息的线格式：content、usage、finish_reason、tool_calls。
func (m *AssistantMessage) MarshalJSON() ([]byte, error) {
	return json.Marshal(messageWire{
		Role:         RoleAssistant,
		Content:      m.Content,
		Usage:        m.Usage,
		FinishReason: m.FinishReason,
		ToolCalls:    m.ToolCalls,
	})
}

// MarshalJSON 输出工具结果消息的线格式：content、tool_call_id、tool_name、is_error。
// is_error 为 false 时不写：与旧结构体的 omitempty 一致，"false"与"没写"在线格式上
// 本来就不区分。
func (m *ToolResultMessage) MarshalJSON() ([]byte, error) {
	return json.Marshal(messageWire{
		Role:       RoleTool,
		Content:    m.Content,
		ToolCallID: m.ToolCallID,
		ToolName:   m.ToolName,
		IsError:    m.IsError,
	})
}

// DecodeMessage 解码一条消息：先读 role，再解成对应的具体类型，最后跑该类型的本地
// 校验。role 缺失或未知、字段组合不属于这个角色、字段自身不合法都在这里报错——入口
// 边界拦下，不让脏数据落成上下文里的一条假消息。
//
// 返回的是接口：具体类型是实现细节，调用方按 Role() 或类型开关分支。
func DecodeMessage(data []byte) (Message, error) {
	var wire messageWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return nil, fmt.Errorf("decode message: %w", err)
	}

	message, err := wire.message()
	if err != nil {
		return nil, err
	}
	if err = message.Validate(); err != nil {
		return nil, err
	}

	return message, nil
}

// message 按 role 把线格式分发到具体类型。四个类型的字段集合互不重叠：属于别的
// 角色的字段出现在这行 JSON 里，说明它不是自己声称的那条消息，报错而不是静默丢掉
// ——静默丢字段会让"这条结果回填给哪次调用"这类信息凭空消失。
func (wire messageWire) message() (Message, error) {
	if wire.Role == "" {
		return nil, fmt.Errorf("message role %q is required", wire.Role)
	}

	if foreign := wire.foreignFields(); len(foreign) > 0 {
		return nil, fmt.Errorf("message role %q must not carry %s",
			wire.Role, strings.Join(foreign, ", "))
	}

	switch wire.Role {
	case RoleSystem:
		return &SystemMessage{Content: wire.Content}, nil
	case RoleUser:
		return &UserMessage{Content: wire.Content}, nil
	case RoleAssistant:
		return &AssistantMessage{
			Content:      wire.Content,
			Usage:        wire.Usage,
			FinishReason: wire.FinishReason,
			ToolCalls:    wire.ToolCalls,
		}, nil
	case RoleTool:
		return &ToolResultMessage{
			Content:    wire.Content,
			ToolCallID: wire.ToolCallID,
			ToolName:   wire.ToolName,
			IsError:    wire.IsError,
		}, nil
	default:
		return nil, fmt.Errorf("unsupported message role %q", wire.Role)
	}
}

// foreignFields 列出这行 JSON 里出现、但不属于 wire.Role 这个角色的字段名。只有
// "写了就一定看得出来"的字段能进这个列表：is_error 是 bool，写了 false 与没写在线
// 格式上不可区分，所以不查它。role 未知时按"没有外来字段"处理，由调用方报未知角色。
func (wire messageWire) foreignFields() []string {
	var foreign []string
	appendIf := func(field string, present bool) {
		if present {
			foreign = append(foreign, field)
		}
	}

	switch wire.Role {
	case RoleAssistant:
		// assistant 拥有 content、usage、finish_reason、tool_calls。
		appendIf("tool_call_id", wire.ToolCallID != "")
		appendIf("tool_name", wire.ToolName != "")
		appendIf("is_error", wire.IsError)
	case RoleTool:
		// tool 结果拥有 content、tool_call_id、tool_name、is_error。
		appendIf("usage", wire.Usage != nil)
		appendIf("finish_reason", wire.FinishReason != "")
		appendIf("tool_calls", len(wire.ToolCalls) > 0)
	case RoleSystem, RoleUser:
		// system 与 user 都只有 content。
		appendIf("usage", wire.Usage != nil)
		appendIf("finish_reason", wire.FinishReason != "")
		appendIf("tool_calls", len(wire.ToolCalls) > 0)
		appendIf("tool_call_id", wire.ToolCallID != "")
		appendIf("tool_name", wire.ToolName != "")
		appendIf("is_error", wire.IsError)
	}

	return foreign
}

// MarshalJSON 逐条调用具体类型的编码器：Messages 是接口切片，元素的编码只能由各自
// 的 MarshalJSON 决定。nil 序列编码成 null，与旧的 []*Message 同形。
func (m Messages) MarshalJSON() ([]byte, error) {
	return json.Marshal([]Message(m))
}

// UnmarshalJSON 逐条调用 DecodeMessage，任一条解不出来就整段报错并点名是第几条：
// 序列级校验（角色必须已知）由每条消息自己的解码保证，这里不重复。
func (m *Messages) UnmarshalJSON(data []byte) error {
	var items []json.RawMessage
	if err := json.Unmarshal(data, &items); err != nil {
		return fmt.Errorf("decode messages: %w", err)
	}
	if items == nil {
		*m = nil

		return nil
	}

	messages := make(Messages, 0, len(items))
	for index, item := range items {
		message, err := DecodeMessage(item)
		if err != nil {
			return fmt.Errorf("message %d: %w", index, err)
		}
		messages = append(messages, message)
	}
	*m = messages

	return nil
}
