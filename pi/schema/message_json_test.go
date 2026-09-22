package schema

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// 本文件钉住消息联合的线格式：七个形状的规范字节（键序与 omitempty 都在字符串
// 里）、旧形状 JSON 的解码结果、Messages 序列的编解码，以及脏 JSON 的报错。这些
// 字节必须与重构前 Message 结构体编码出的字节完全一致，判定标准不是"键集相同"：
// 盘上已有的会话文件不迁移也能读，且重新编码后逐字节相同。

const (
	wireImageURL = "https://example.test/wire.png"
	wireToolID   = "call-wire"
	wireToolName = "read_file"
)

// wireShape 是一个形状的规范字节，以及它应该等价的那条联合值。wantJSON 是手写的、
// 与旧结构体编码结果一致的字节，不是本包编码出来的结果——它才是断言。
type wireShape struct {
	name     string
	message  Message
	wantJSON string
}

// wireShapes 覆盖七种形状：system、user 文本、user 文本+图片、assistant 文本
// （带 usage 与 finish_reason）、assistant 文本+工具调用、tool 结果、tool 结果
// 带图片。最后一个的 IsError 为 false，验证 is_error 被 omitempty 省掉。
func wireShapes() []wireShape {
	return []wireShape{
		{
			name:     "system 纯文本",
			message:  &SystemMessage{Content: ContentBlocks{TextBlock("system prompt")}},
			wantJSON: `{"role":"system","content":[{"type":"text","text":"system prompt"}]}`,
		},
		{
			name:     "user 纯文本",
			message:  &UserMessage{Content: ContentBlocks{TextBlock("hello")}},
			wantJSON: `{"role":"user","content":[{"type":"text","text":"hello"}]}`,
		},
		{
			name: "user 文本加图片",
			message: &UserMessage{Content: ContentBlocks{
				TextBlock("look at this"), ImageBlock(wireImageURL),
			}},
			wantJSON: `{"role":"user","content":[{"type":"text","text":"look at this"},` +
				`{"type":"image","image":{"url":"https://example.test/wire.png"}}]}`,
		},
		{
			name: "assistant 文本带用量与结束原因",
			message: &AssistantMessage{
				Content: ContentBlocks{TextBlock("reply")},
				Usage: &Usage{
					InputTokens: 12, OutputTokens: 3,
					PlatformID: "openai", Model: "gpt-test",
				},
				FinishReason: FinishReasonStop,
			},
			wantJSON: `{"role":"assistant","content":[{"type":"text","text":"reply"}],` +
				`"usage":{"input_tokens":12,"output_tokens":3,` +
				`"input_price_usd_per_million_tokens":0,"output_price_usd_per_million_tokens":0,` +
				`"cost_usd":0,"latency_ms":0,"platform_id":"openai","model":"gpt-test"},` +
				`"finish_reason":"stop"}`,
		},
		{
			name: "assistant 文本加工具调用",
			message: &AssistantMessage{
				Content: ContentBlocks{TextBlock("calling")},
				ToolCalls: ToolCalls{{
					ID: wireToolID, Name: wireToolName,
					Arguments: json.RawMessage(`{"path":"README.md"}`),
				}},
			},
			wantJSON: `{"role":"assistant","content":[{"type":"text","text":"calling"}],` +
				`"tool_calls":[{"id":"call-wire","name":"read_file","arguments":{"path":"README.md"}}]}`,
		},
		{
			name: "tool 结果",
			message: &ToolResultMessage{
				Content:    ContentBlocks{TextBlock("contents")},
				ToolCallID: wireToolID, ToolName: wireToolName, IsError: true,
			},
			wantJSON: `{"role":"tool","content":[{"type":"text","text":"contents"}],` +
				`"tool_call_id":"call-wire","tool_name":"read_file","is_error":true}`,
		},
		{
			name: "tool 结果加图片且不是错误",
			message: &ToolResultMessage{
				Content:    ContentBlocks{TextBlock("screenshot"), ImageBlock(wireImageURL)},
				ToolCallID: wireToolID, ToolName: "screenshot",
			},
			wantJSON: `{"role":"tool","content":[{"type":"text","text":"screenshot"},` +
				`{"type":"image","image":{"url":"https://example.test/wire.png"}}],` +
				`"tool_call_id":"call-wire","tool_name":"screenshot"}`,
		},
	}
}

// TestMessageJSONShapeByteIdentity 每个形状都要满足两件事：编码结果逐字节等于手写
// 的规范字节（键序与 omitempty 都在里面），且编解码一轮之后再次编码仍是同一串字节。
func TestMessageJSONShapeByteIdentity(t *testing.T) {
	for _, shape := range wireShapes() {
		t.Run(shape.name, func(t *testing.T) {
			first, err := json.Marshal(shape.message)
			if err != nil {
				t.Fatalf("编码 %T: %v", shape.message, err)
			}
			if got := string(first); got != shape.wantJSON {
				t.Fatalf("编码结果 = %s，想要 %s", got, shape.wantJSON)
			}

			decoded, err := DecodeMessage(first)
			if err != nil {
				t.Fatalf("DecodeMessage(%s): %v", first, err)
			}
			second, err := json.Marshal(decoded)
			if err != nil {
				t.Fatalf("二次编码 %T: %v", decoded, err)
			}
			if string(second) != string(first) {
				t.Fatalf("二次编码 = %s，想要与首次逐字节相同 %s", second, first)
			}
		})
	}
}

// TestDecodeMessageOldShapeJSON 旧形状的 JSON（手写、与重构前的编码结果同形）必须
// 解码成对应的具体类型，且字段值与写进 JSON 时一致。
func TestDecodeMessageOldShapeJSON(t *testing.T) {
	for _, shape := range wireShapes() {
		t.Run(shape.name, func(t *testing.T) {
			decoded, err := DecodeMessage([]byte(shape.wantJSON))
			if err != nil {
				t.Fatalf("DecodeMessage(%s): %v", shape.wantJSON, err)
			}
			requireSameMessage(t, decoded, shape.message)
		})
	}
}

// TestToolResultJSONOmitsFalseIsError 钉住 is_error 的 omitempty 语义：false 与
// "没写"在线格式上不可区分，旧的编码器就是不写它，保持一致。
func TestToolResultJSONOmitsFalseIsError(t *testing.T) {
	encoded, err := json.Marshal(&ToolResultMessage{
		Content: ContentBlocks{TextBlock("ok")}, ToolCallID: wireToolID, ToolName: wireToolName,
	})
	if err != nil {
		t.Fatalf("编码 tool 结果: %v", err)
	}
	if strings.Contains(string(encoded), "is_error") {
		t.Fatalf("IsError 为 false 时不该写出 is_error: %s", encoded)
	}
}

// TestMessageJSONOmitsEmptyOptionalFields 钉住其余可选字段的 omitempty 语义：没有
// 内容、用量、工具调用的消息就是"只有 role"，与旧编码器一致。
func TestMessageJSONOmitsEmptyOptionalFields(t *testing.T) {
	cases := []struct {
		name     string
		message  Message
		wantJSON string
	}{
		{name: "system 无内容", message: &SystemMessage{}, wantJSON: `{"role":"system"}`},
		{name: "user 无内容", message: &UserMessage{}, wantJSON: `{"role":"user"}`},
		{name: "assistant 无内容", message: &AssistantMessage{}, wantJSON: `{"role":"assistant"}`},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			encoded, err := json.Marshal(testCase.message)
			if err != nil {
				t.Fatalf("编码 %T: %v", testCase.message, err)
			}
			if got := string(encoded); got != testCase.wantJSON {
				t.Fatalf("编码结果 = %s，想要 %s", got, testCase.wantJSON)
			}

			decoded, err := DecodeMessage(encoded)
			if err != nil {
				t.Fatalf("DecodeMessage(%s): %v", encoded, err)
			}
			if decoded.Role() != testCase.message.Role() {
				t.Fatalf("解码出的角色 = %q，想要 %q", decoded.Role(), testCase.message.Role())
			}
		})
	}
}

// TestDecodeMessageRejectsDirtyJSON 脏 JSON 一律在 schema 边界报错，且错误里点名是
// 哪个值或哪个字段不对。
func TestDecodeMessageRejectsDirtyJSON(t *testing.T) {
	cases := []struct {
		name         string
		line         string
		wantContains string
	}{
		{
			name:         "未知 role",
			line:         `{"role":"wizard","content":[{"type":"text","text":"hi"}]}`,
			wantContains: "wizard",
		},
		{
			name:         "缺少 role",
			line:         `{"content":[{"type":"text","text":"hi"}]}`,
			wantContains: "role",
		},
		{
			name:         "tool 结果缺少 tool_call_id",
			line:         `{"role":"tool","content":[{"type":"text","text":"out"}],"tool_name":"read_file"}`,
			wantContains: "tool_call_id",
		},
		{
			name:         "tool 结果缺少 tool_name",
			line:         `{"role":"tool","content":[{"type":"text","text":"out"}],"tool_call_id":"call-wire"}`,
			wantContains: "tool_name",
		},
		{
			name: "assistant 带图片块",
			line: `{"role":"assistant","content":[{"type":"image","image":{"url":"` +
				wireImageURL + `"}}]}`,
			wantContains: "image",
		},
		{
			name: "tool 结果带 tool_calls",
			line: `{"role":"tool","content":[{"type":"text","text":"out"}],"tool_call_id":"call-wire",` +
				`"tool_name":"read_file","tool_calls":[{"id":"call-wire","name":"read_file","arguments":{}}]}`,
			wantContains: "tool_calls",
		},
		{
			name:         "assistant 带 tool_call_id",
			line:         `{"role":"assistant","content":[{"type":"text","text":"out"}],"tool_call_id":"call-wire"}`,
			wantContains: "tool_call_id",
		},
		{
			name:         "user 带 is_error",
			line:         `{"role":"user","content":[{"type":"text","text":"out"}],"is_error":true}`,
			wantContains: "is_error",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			message, err := DecodeMessage([]byte(testCase.line))
			if err == nil {
				t.Fatalf("DecodeMessage(%s) 没有报错，解出了 %#v", testCase.line, message)
			}
			if !strings.Contains(err.Error(), testCase.wantContains) {
				t.Fatalf("错误 %q 没有点名 %q", err, testCase.wantContains)
			}
		})
	}
}

// TestMessagesJSONRoundTrip Messages 序列逐条走各自具体类型的编码器，编解码一轮
// 后逐字节相同。
func TestMessagesJSONRoundTrip(t *testing.T) {
	messages := make(Messages, 0, len(wireShapes()))
	for _, shape := range wireShapes() {
		messages = append(messages, shape.message)
	}

	first, err := json.Marshal(messages)
	if err != nil {
		t.Fatalf("编码 Messages: %v", err)
	}

	var decoded Messages
	if err = json.Unmarshal(first, &decoded); err != nil {
		t.Fatalf("解码 Messages: %v", err)
	}
	if len(decoded) != len(messages) {
		t.Fatalf("解码出的条数 = %d，想要 %d", len(decoded), len(messages))
	}
	for index := range messages {
		requireSameMessage(t, decoded[index], messages[index])
	}

	second, err := json.Marshal(decoded)
	if err != nil {
		t.Fatalf("二次编码 Messages: %v", err)
	}
	if string(second) != string(first) {
		t.Fatalf("二次编码 = %s，想要与首次逐字节相同 %s", second, first)
	}
}

// TestMessagesUnmarshalJSONReportsBadElement 序列里有一条坏消息时整段报错，错误
// 里点名是哪条、坏在哪。
func TestMessagesUnmarshalJSONReportsBadElement(t *testing.T) {
	data := `[{"role":"user","content":[{"type":"text","text":"hi"}]},` +
		`{"role":"wizard","content":[{"type":"text","text":"hi"}]}]`

	var messages Messages
	err := json.Unmarshal([]byte(data), &messages)
	if err == nil {
		t.Fatalf("坏消息没有让整段解码失败: %#v", messages)
	}
	if !strings.Contains(err.Error(), "wizard") {
		t.Fatalf("错误 %q 没有点名坏在哪", err)
	}
	if !strings.Contains(err.Error(), "1") {
		t.Fatalf("错误 %q 没有点名是第几条", err)
	}
}

// TestMessagesJSONNilAndEmpty 空的 Messages 编解码与旧的 []*Message 同形：nil 编码
// 成 null，解码也回到 nil；空切片保持空切片。
func TestMessagesJSONNilAndEmpty(t *testing.T) {
	encoded, err := json.Marshal(Messages(nil))
	if err != nil {
		t.Fatalf("编码 nil Messages: %v", err)
	}
	if got := string(encoded); got != "null" {
		t.Fatalf("nil Messages 编码 = %s，想要 null", got)
	}

	var decoded Messages
	if err = json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("解码 null: %v", err)
	}
	if decoded != nil {
		t.Fatalf("null 解码成了 %#v，想要 nil", decoded)
	}

	encoded, err = json.Marshal(Messages{})
	if err != nil {
		t.Fatalf("编码空 Messages: %v", err)
	}
	if got := string(encoded); got != "[]" {
		t.Fatalf("空 Messages 编码 = %s，想要 []", got)
	}
}

// requireSameMessage 断言两个联合值是同一个具体类型，且字段逐一相等。
func requireSameMessage(t *testing.T, got, want Message) {
	t.Helper()

	if reflect.TypeOf(got) != reflect.TypeOf(want) {
		t.Fatalf("解码出的类型 = %T，想要 %T", got, want)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("解码出的消息 = %+v，想要 %+v", got, want)
	}
	if got.Role() != want.Role() {
		t.Fatalf("解码出的角色 = %q，想要 %q", got.Role(), want.Role())
	}
}
