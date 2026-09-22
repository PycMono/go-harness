package schema

import (
	"encoding/json"
	"strings"
	"testing"
)

// 本文件覆盖四元消息联合自身的契约：Role 判别、构造函数的产物、各角色的图片
// 规则与工具结果的身份字段。序列级校验与双协议转换的行为基线在
// conversion_characterization_test.go，本文件不重复钉那一部分。

const (
	variantImageURL = "https://example.test/variant.png"
	variantToolID   = "call-variant"
	variantToolName = "read_file"
)

// TestMessageRoles 四种具体类型的 Role 方法各自返回对应常量。
func TestMessageRoles(t *testing.T) {
	cases := []struct {
		name    string
		message Message
		want    Role
	}{
		{
			name:    "system",
			message: &SystemMessage{Content: ContentBlocks{TextBlock("系统提示词")}},
			want:    RoleSystem,
		},
		{
			name:    "user",
			message: &UserMessage{Content: ContentBlocks{TextBlock("用户输入")}},
			want:    RoleUser,
		},
		{
			name:    "assistant",
			message: &AssistantMessage{Content: ContentBlocks{TextBlock("模型输出")}},
			want:    RoleAssistant,
		},
		{
			name: "tool",
			message: &ToolResultMessage{
				Content:    ContentBlocks{TextBlock("工具结果")},
				ToolCallID: variantToolID, ToolName: variantToolName,
			},
			want: RoleTool,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := testCase.message.Role(); got != testCase.want {
				t.Fatalf("Role() = %q，想要 %q", got, testCase.want)
			}
		})
	}
}

// TestMessageConstructors 构造函数返回对应的具体类型，字段逐个落到该类型自己的
// 字段上——联合类型没有其他角色的可选字段可以误填。
func TestMessageConstructors(t *testing.T) {
	usage := &Usage{InputTokens: 7, OutputTokens: 11}
	toolCalls := ToolCalls{{
		ID: variantToolID, Name: variantToolName,
		Arguments: json.RawMessage(`{"path":"README.md"}`),
	}}

	cases := []struct {
		name   string
		build  func(t *testing.T) Message
		assert func(t *testing.T, message Message)
	}{
		{
			name: "system",
			build: func(t *testing.T) Message {
				t.Helper()
				message, err := NewSystemMessage(ContentBlocks{TextBlock("系统提示词")})
				if err != nil {
					t.Fatalf("NewSystemMessage: %v", err)
				}
				return message
			},
			assert: func(t *testing.T, message Message) {
				t.Helper()
				system, ok := message.(*SystemMessage)
				if !ok {
					t.Fatalf("具体类型 = %T，想要 *SystemMessage", message)
				}
				assertVariantText(t, system.Content, "系统提示词")
			},
		},
		{
			name: "user 文本 + 图片",
			build: func(t *testing.T) Message {
				t.Helper()
				message, err := NewUserMessage(ContentBlocks{
					TextBlock("用户输入"),
					ImageBlock(variantImageURL),
				})
				if err != nil {
					t.Fatalf("NewUserMessage: %v", err)
				}
				return message
			},
			assert: func(t *testing.T, message Message) {
				t.Helper()
				user, ok := message.(*UserMessage)
				if !ok {
					t.Fatalf("具体类型 = %T，想要 *UserMessage", message)
				}
				if len(user.Content) != 2 {
					t.Fatalf("内容块数 = %d，想要 2（content = %+v）", len(user.Content), user.Content)
				}
				assertVariantTextBlock(t, user.Content[0], "用户输入")
				if user.Content[1].Type != ContentTypeImage || user.Content[1].Image == nil ||
					user.Content[1].Image.URL != variantImageURL {
					t.Fatalf("图片块 = %+v，想要 %q", user.Content[1], variantImageURL)
				}
			},
		},
		{
			name: "assistant",
			build: func(t *testing.T) Message {
				t.Helper()
				message, err := NewAssistantMessage(
					ContentBlocks{TextBlock("模型输出")}, usage, FinishReasonToolUse, toolCalls)
				if err != nil {
					t.Fatalf("NewAssistantMessage: %v", err)
				}
				return message
			},
			assert: func(t *testing.T, message Message) {
				t.Helper()
				assistant, ok := message.(*AssistantMessage)
				if !ok {
					t.Fatalf("具体类型 = %T，想要 *AssistantMessage", message)
				}
				assertVariantText(t, assistant.Content, "模型输出")
				if assistant.Usage != usage {
					t.Fatalf("Usage = %+v，想要传进去的那一份 %+v", assistant.Usage, usage)
				}
				if assistant.FinishReason != FinishReasonToolUse {
					t.Fatalf("FinishReason = %q，想要 %q", assistant.FinishReason, FinishReasonToolUse)
				}
				if len(assistant.ToolCalls) != 1 || assistant.ToolCalls[0].ID != variantToolID ||
					assistant.ToolCalls[0].Name != variantToolName {
					t.Fatalf("ToolCalls = %+v，想要一条 %q/%q 的调用",
						assistant.ToolCalls, variantToolID, variantToolName)
				}
				if string(assistant.ToolCalls[0].Arguments) != `{"path":"README.md"}` {
					t.Fatalf("ToolCalls[0].Arguments = %q，想要原始 JSON 文本",
						assistant.ToolCalls[0].Arguments)
				}
			},
		},
		{
			name: "tool 结果",
			build: func(t *testing.T) Message {
				t.Helper()
				message, err := NewToolResultMessage(
					ContentBlocks{TextBlock("工具结果")}, variantToolID, variantToolName, true)
				if err != nil {
					t.Fatalf("NewToolResultMessage: %v", err)
				}
				return message
			},
			assert: func(t *testing.T, message Message) {
				t.Helper()
				result, ok := message.(*ToolResultMessage)
				if !ok {
					t.Fatalf("具体类型 = %T，想要 *ToolResultMessage", message)
				}
				assertVariantText(t, result.Content, "工具结果")
				if result.ToolCallID != variantToolID {
					t.Fatalf("ToolCallID = %q，想要 %q", result.ToolCallID, variantToolID)
				}
				if result.ToolName != variantToolName {
					t.Fatalf("ToolName = %q，想要 %q", result.ToolName, variantToolName)
				}
				if !result.IsError {
					t.Fatalf("IsError = false，想要 true")
				}
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			testCase.assert(t, testCase.build(t))
		})
	}
}

// TestMessageImageRules 图片块是各角色自己的规则：user 与 tool 结果允许携带，
// system 与 assistant 拒绝，且拒绝时的错误要指出是哪个角色。
func TestMessageImageRules(t *testing.T) {
	imageContent := ContentBlocks{TextBlock("正文"), ImageBlock(variantImageURL)}

	cases := []struct {
		name         string
		message      Message
		wantRejected bool
	}{
		{
			name:    "user 允许图片",
			message: &UserMessage{Content: imageContent.Clone()},
		},
		{
			name: "tool 结果允许图片",
			message: &ToolResultMessage{
				Content: imageContent.Clone(), ToolCallID: variantToolID, ToolName: variantToolName,
			},
		},
		{
			name:         "assistant 拒绝图片",
			message:      &AssistantMessage{Content: imageContent.Clone()},
			wantRejected: true,
		},
		{
			name:         "system 拒绝图片",
			message:      &SystemMessage{Content: imageContent.Clone()},
			wantRejected: true,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			err := testCase.message.Validate()
			if testCase.wantRejected {
				if err == nil {
					t.Fatalf("role %q 接受了图片", testCase.message.Role())
				}
				if !strings.Contains(err.Error(), string(testCase.message.Role())) {
					t.Fatalf("错误 %q 没有指出角色 %q", err.Error(), testCase.message.Role())
				}
				return
			}
			if err != nil {
				t.Fatalf("role %q 拒绝了合法图片: %v", testCase.message.Role(), err)
			}
			// 允许图片时内容块必须原样留下：校验不能悄悄丢掉图片块。
			content := messageContent(testCase.message)
			if len(content) != 2 {
				t.Fatalf("内容块数 = %d，想要 2", len(content))
			}
			if content[1].Type != ContentTypeImage || content[1].Image == nil ||
				content[1].Image.URL != variantImageURL {
				t.Fatalf("图片块 = %+v，想要 %q", content[1], variantImageURL)
			}
		})
	}
}

// TestMessageImageRulesAllowTextOnly 被拒绝的是图片，不是内容本身：纯文本的
// system 与 assistant 消息必须照常通过，否则上面的拒绝用例证明不了是图片规则
// 拦下的。
func TestMessageImageRulesAllowTextOnly(t *testing.T) {
	cases := []struct {
		name    string
		message Message
		want    string
	}{
		{
			name:    "system",
			message: &SystemMessage{Content: ContentBlocks{TextBlock("系统提示词")}},
			want:    "系统提示词",
		},
		{
			name:    "assistant",
			message: &AssistantMessage{Content: ContentBlocks{TextBlock("模型输出")}},
			want:    "模型输出",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if err := testCase.message.Validate(); err != nil {
				t.Fatalf("纯文本消息过不了校验: %v", err)
			}
			assertVariantText(t, messageContent(testCase.message), testCase.want)
		})
	}
}

// TestToolResultMessageRequiresToolIdentity 工具结果必须同时带调用 ID 与工具名，
// 缺一个都不行：两者共同决定这条结果回填给哪次调用。
func TestToolResultMessageRequiresToolIdentity(t *testing.T) {
	cases := []struct {
		name       string
		toolCallID string
		toolName   string
		wantCause  string
	}{
		{name: "两者齐全", toolCallID: variantToolID, toolName: variantToolName},
		{name: "缺 tool_call_id", toolName: variantToolName, wantCause: "tool_call_id"},
		{name: "缺 tool_name", toolCallID: variantToolID, wantCause: "tool_name"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			message := &ToolResultMessage{
				Content:    ContentBlocks{TextBlock("工具结果")},
				ToolCallID: testCase.toolCallID,
				ToolName:   testCase.toolName,
			}

			err := message.Validate()
			if testCase.wantCause == "" {
				if err != nil {
					t.Fatalf("身份字段齐全却过不了校验: %v", err)
				}
				assertVariantText(t, message.Content, "工具结果")
				return
			}
			if err == nil {
				t.Fatalf("缺 %s 却过了校验", testCase.wantCause)
			}
			if !strings.Contains(err.Error(), testCase.wantCause) {
				t.Fatalf("错误 %q 没有提到 %q", err.Error(), testCase.wantCause)
			}
		})
	}
}

// TestMessageConstructorsRejectInvalidInput 构造函数跑一次本地校验：具体类型
// Validate 拒绝的输入，构造函数同样拒绝，并且不返回半成品消息。
func TestMessageConstructorsRejectInvalidInput(t *testing.T) {
	cases := []struct {
		name      string
		build     func() (Message, error)
		wantCause string
	}{
		{
			name: "system 带图片",
			build: func() (Message, error) {
				return NewSystemMessage(ContentBlocks{TextBlock("正文"), ImageBlock(variantImageURL)})
			},
			wantCause: string(RoleSystem),
		},
		{
			name: "user 图片 URL 非法",
			build: func() (Message, error) {
				return NewUserMessage(ContentBlocks{ImageBlock("ftp://example.test/variant.png")})
			},
			wantCause: "ftp://example.test/variant.png",
		},
		{
			name: "assistant 带图片",
			build: func() (Message, error) {
				return NewAssistantMessage(
					ContentBlocks{TextBlock("正文"), ImageBlock(variantImageURL)}, nil, FinishReasonStop, nil)
			},
			wantCause: string(RoleAssistant),
		},
		{
			name: "tool 结果缺 tool_call_id",
			build: func() (Message, error) {
				return NewToolResultMessage(ContentBlocks{TextBlock("工具结果")}, "", variantToolName, false)
			},
			wantCause: "tool_call_id",
		},
		{
			name: "tool 结果缺 tool_name",
			build: func() (Message, error) {
				return NewToolResultMessage(ContentBlocks{TextBlock("工具结果")}, variantToolID, "", false)
			},
			wantCause: "tool_name",
		},
		{
			name: "tool 结果带非法内容块类型",
			build: func() (Message, error) {
				return NewToolResultMessage(
					ContentBlocks{{Type: ContentType("audio")}}, variantToolID, variantToolName, false)
			},
			wantCause: "unsupported content type",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			message, err := testCase.build()
			if err == nil {
				t.Fatalf("构造函数接受了非法输入，产出 %+v", message)
			}
			if message != nil {
				t.Fatalf("构造函数失败时仍返回了消息: %+v", message)
			}
			if !strings.Contains(err.Error(), testCase.wantCause) {
				t.Fatalf("错误 %q 没有提到 %q", err.Error(), testCase.wantCause)
			}
		})
	}
}

// TestMessagesValidateRejectsEmptyToolCallID 序列级校验沿用工具结果的身份规则：
// 逐条调用消息自己的 Validate，而不是回头读统一结构体的字段。
func TestMessagesValidateRejectsEmptyToolCallID(t *testing.T) {
	cases := []struct {
		name     string
		messages Messages
		wantErr  bool
	}{
		{
			name: "工具结果缺 tool_call_id",
			messages: Messages{
				&UserMessage{Content: ContentBlocks{TextBlock("用户输入")}},
				&ToolResultMessage{Content: ContentBlocks{TextBlock("工具结果")}, ToolName: variantToolName},
			},
			wantErr: true,
		},
		{
			name: "四角色齐全",
			messages: Messages{
				&SystemMessage{Content: ContentBlocks{TextBlock("系统提示词")}},
				&UserMessage{Content: ContentBlocks{TextBlock("用户输入"), ImageBlock(variantImageURL)}},
				&AssistantMessage{Content: ContentBlocks{TextBlock("模型输出")}},
				&ToolResultMessage{
					Content:    ContentBlocks{TextBlock("工具结果")},
					ToolCallID: variantToolID, ToolName: variantToolName,
				},
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			err := testCase.messages.Validate()
			if !testCase.wantErr {
				if err != nil {
					t.Fatalf("合法序列过不了校验: %v", err)
				}
				if got := len(testCase.messages); got != 4 {
					t.Fatalf("消息条数 = %d，想要 4", got)
				}
				return
			}
			if err == nil {
				t.Fatalf("非法序列过了校验")
			}
			if !strings.Contains(err.Error(), "tool_call_id") {
				t.Fatalf("错误 %q 没有提到 tool_call_id", err.Error())
			}
		})
	}
}

// assertVariantText 断言内容块正好是一个指定文本，且不夹带图片。
func assertVariantText(t *testing.T, content ContentBlocks, want string) {
	t.Helper()

	if len(content) != 1 {
		t.Fatalf("内容块数 = %d，想要 1（content = %+v）", len(content), content)
	}
	assertVariantTextBlock(t, content[0], want)
}

// assertVariantTextBlock 断言单个内容块是指定文本，且不夹带图片。带图片的用例
// 内容块不止一个，按位置取块断言文本时复用本函数。
func assertVariantTextBlock(t *testing.T, block ContentBlock, want string) {
	t.Helper()

	if block.Type != ContentTypeText || block.Text != want {
		t.Fatalf("内容块 = %+v，想要文本块 %q", block, want)
	}
	if block.Image != nil {
		t.Fatalf("文本块夹带了图片: %+v", block.Image)
	}
}

// messageContent 取出消息的内容块，供四个具体类型共用。
func messageContent(message Message) ContentBlocks {
	switch typed := message.(type) {
	case *SystemMessage:
		return typed.Content
	case *UserMessage:
		return typed.Content
	case *AssistantMessage:
		return typed.Content
	case *ToolResultMessage:
		return typed.Content
	default:
		return nil
	}
}
