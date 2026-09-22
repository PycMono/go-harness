package schema

import (
	"encoding/json"
	"reflect"
	"testing"

	anthropicsdk "github.com/anthropics/anthropic-sdk-go"
	openaisdk "github.com/openai/openai-go/v3"
)

// 本文件是 Step 0 的取值基线：把 ToOpenAIMessages / ToAnthropicMessages 当下的
// 输出逐字段钉住，供 schema.Message 变成四variant 联合类型的重构对照。这些断言
// 描述的是"现在是什么"，不是"应该是什么"——重构后任何一条变红都意味着转换行为
// 变了，而不是测试写错了。
//
// 断言一律落在 SDK 的具体参数字段上，不看 %v / json.Marshal 的字符串形式：字符串
// 形式会把"哪个联合分支被填了、另一个是不是空的"这类关键差异抹平。

const (
	characterizationSystem    = "基线系统提示词"
	characterizationUser      = "基线用户输入"
	characterizationAssistant = "基线模型输出"
	characterizationToolText  = "基线工具结果"
	characterizationImageURL  = "https://example.test/cat.png"
	characterizationToolID    = "call-1"
	// characterizationToolArgs 是 ToolCall.Arguments 的原始 JSON 文本，OpenAI
	// 侧应当原样透传。
	characterizationToolArgs = `{"path":"README.md"}`
)

// TestToOpenAIMessagesCharacterization 覆盖四种角色到 OpenAI 聊天参数的转换。
func TestToOpenAIMessagesCharacterization(t *testing.T) {
	cases := []struct {
		name    string
		message *Message
		assert  func(t *testing.T, param openaisdk.ChatCompletionMessageParamUnion)
	}{
		{
			name:    "system 文本",
			message: &Message{Role: RoleSystem, Content: ContentBlocks{TextBlock(characterizationSystem)}},
			assert: func(t *testing.T, param openaisdk.ChatCompletionMessageParamUnion) {
				requireOpenAIVariant(t, param, "OfSystem")
				content := param.OfSystem.Content
				if !content.OfString.Valid() || content.OfString.Value != characterizationSystem {
					t.Fatalf("system content = %q (valid=%v)，想要 %q",
						content.OfString.Value, content.OfString.Valid(), characterizationSystem)
				}
				if len(content.OfArrayOfContentParts) != 0 {
					t.Fatalf("system content 用了 parts 形式: %v", content.OfArrayOfContentParts)
				}
			},
		},
		{
			name:    "user 纯文本",
			message: &Message{Role: RoleUser, Content: ContentBlocks{TextBlock(characterizationUser)}},
			assert: func(t *testing.T, param openaisdk.ChatCompletionMessageParamUnion) {
				requireOpenAIVariant(t, param, "OfUser")
				content := param.OfUser.Content
				if !content.OfString.Valid() || content.OfString.Value != characterizationUser {
					t.Fatalf("user content = %q (valid=%v)，想要 %q",
						content.OfString.Value, content.OfString.Valid(), characterizationUser)
				}
				if len(content.OfArrayOfContentParts) != 0 {
					t.Fatalf("无图 user 消息不该走 parts 形式: %v", content.OfArrayOfContentParts)
				}
			},
		},
		{
			name: "user 文本 + 图片",
			message: &Message{Role: RoleUser, Content: ContentBlocks{
				TextBlock(characterizationUser),
				ImageBlock(characterizationImageURL),
			}},
			assert: func(t *testing.T, param openaisdk.ChatCompletionMessageParamUnion) {
				requireOpenAIVariant(t, param, "OfUser")
				content := param.OfUser.Content
				// 有图片块时走 parts 形式，字符串形式必须空着。
				if content.OfString.Valid() {
					t.Fatalf("带图 user 消息不该同时填字符串形式: %q", content.OfString.Value)
				}
				parts := content.OfArrayOfContentParts
				if len(parts) != 2 {
					t.Fatalf("content parts 条数 = %d，想要 2", len(parts))
				}
				// 顺序跟着内容块走：文本在前，图片在后。
				if parts[0].OfText == nil || parts[0].OfText.Text != characterizationUser {
					t.Fatalf("parts[0] = %+v，想要文本块 %q", parts[0], characterizationUser)
				}
				if parts[0].OfImageURL != nil {
					t.Fatalf("parts[0] 同时填了 image_url: %+v", parts[0].OfImageURL)
				}
				if parts[1].OfImageURL == nil || parts[1].OfImageURL.ImageURL.URL != characterizationImageURL {
					t.Fatalf("parts[1] = %+v，想要图片块 %q", parts[1], characterizationImageURL)
				}
				if parts[1].OfText != nil {
					t.Fatalf("parts[1] 同时填了 text: %+v", parts[1].OfText)
				}
			},
		},
		{
			name:    "assistant 纯文本",
			message: &Message{Role: RoleAssistant, Content: ContentBlocks{TextBlock(characterizationAssistant)}},
			assert: func(t *testing.T, param openaisdk.ChatCompletionMessageParamUnion) {
				requireOpenAIVariant(t, param, "OfAssistant")
				assistant := param.OfAssistant
				if !assistant.Content.OfString.Valid() || assistant.Content.OfString.Value != characterizationAssistant {
					t.Fatalf("assistant content = %q (valid=%v)，想要 %q",
						assistant.Content.OfString.Value, assistant.Content.OfString.Valid(), characterizationAssistant)
				}
				if len(assistant.ToolCalls) != 0 {
					t.Fatalf("无工具调用的 assistant 带了 ToolCalls: %+v", assistant.ToolCalls)
				}
			},
		},
		{
			name: "assistant 文本 + 工具调用",
			message: &Message{
				Role:    RoleAssistant,
				Content: ContentBlocks{TextBlock(characterizationAssistant)},
				ToolCalls: ToolCalls{{
					ID: characterizationToolID, Name: "read_file",
					Arguments: json.RawMessage(characterizationToolArgs),
				}},
			},
			assert: func(t *testing.T, param openaisdk.ChatCompletionMessageParamUnion) {
				requireOpenAIVariant(t, param, "OfAssistant")
				assistant := param.OfAssistant
				if !assistant.Content.OfString.Valid() || assistant.Content.OfString.Value != characterizationAssistant {
					t.Fatalf("assistant content = %q，想要 %q",
						assistant.Content.OfString.Value, characterizationAssistant)
				}
				if len(assistant.ToolCalls) != 1 {
					t.Fatalf("ToolCalls 条数 = %d，想要 1", len(assistant.ToolCalls))
				}
				call := assistant.ToolCalls[0]
				if call.OfFunction == nil {
					t.Fatalf("ToolCalls[0] 不是 function 分支: %+v", call)
				}
				if call.OfCustom != nil {
					t.Fatalf("ToolCalls[0] 同时填了 custom 分支: %+v", call.OfCustom)
				}
				if call.OfFunction.ID != characterizationToolID {
					t.Fatalf("tool call id = %q，想要 %q", call.OfFunction.ID, characterizationToolID)
				}
				if call.OfFunction.Function.Name != "read_file" {
					t.Fatalf("tool call name = %q，想要 %q", call.OfFunction.Function.Name, "read_file")
				}
				// Arguments 是原始 JSON 文本原样透传，不解析、不重排。
				if call.OfFunction.Function.Arguments != characterizationToolArgs {
					t.Fatalf("tool call arguments = %q，想要原始 JSON %q",
						call.OfFunction.Function.Arguments, characterizationToolArgs)
				}
			},
		},
		{
			name: "tool 结果",
			message: &Message{
				Role: RoleTool, Content: ContentBlocks{TextBlock(characterizationToolText)},
				ToolCallID: characterizationToolID, ToolName: "read_file", IsError: true,
			},
			assert: func(t *testing.T, param openaisdk.ChatCompletionMessageParamUnion) {
				requireOpenAIVariant(t, param, "OfTool")
				tool := param.OfTool
				if !tool.Content.OfString.Valid() || tool.Content.OfString.Value != characterizationToolText {
					t.Fatalf("tool content = %q (valid=%v)，想要 %q",
						tool.Content.OfString.Value, tool.Content.OfString.Valid(), characterizationToolText)
				}
				if tool.ToolCallID != characterizationToolID {
					t.Fatalf("tool_call_id = %q，想要 %q", tool.ToolCallID, characterizationToolID)
				}
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			params, err := Messages{testCase.message}.ToOpenAIMessages()
			if err != nil {
				t.Fatalf("ToOpenAIMessages: %v", err)
			}
			if len(params) != 1 {
				t.Fatalf("OpenAI 消息条数 = %d，想要 1", len(params))
			}
			testCase.assert(t, params[0])
		})
	}
}

// TestToOpenAIMessagesKeepsRoleOrder 钉住多角色混排时的顺序，以及每种角色落在哪个
// 联合分支上。
func TestToOpenAIMessagesKeepsRoleOrder(t *testing.T) {
	params, err := Messages{
		{Role: RoleSystem, Content: ContentBlocks{TextBlock(characterizationSystem)}},
		{Role: RoleUser, Content: ContentBlocks{TextBlock(characterizationUser)}},
		{Role: RoleAssistant, Content: ContentBlocks{TextBlock(characterizationAssistant)}},
		{Role: RoleTool, Content: ContentBlocks{TextBlock(characterizationToolText)}, ToolCallID: characterizationToolID},
	}.ToOpenAIMessages()
	if err != nil {
		t.Fatalf("ToOpenAIMessages: %v", err)
	}
	if len(params) != 4 {
		t.Fatalf("OpenAI 消息条数 = %d，想要 4", len(params))
	}

	variants := make([]string, 0, len(params))
	for _, param := range params {
		variants = append(variants, openAIVariantName(param))
	}
	want := []string{"OfSystem", "OfUser", "OfAssistant", "OfTool"}
	if !reflect.DeepEqual(variants, want) {
		t.Fatalf("联合分支顺序 = %v，想要 %v", variants, want)
	}
}

// TestToAnthropicMessagesCharacterization 覆盖四种角色到 Anthropic 消息参数的转换。
// system 走单独的返回值，不占 messages 的位置。
func TestToAnthropicMessagesCharacterization(t *testing.T) {
	cases := []struct {
		name    string
		message *Message
		assert  func(t *testing.T, messages []anthropicsdk.MessageParam, system []anthropicsdk.TextBlockParam)
	}{
		{
			name:    "system 文本",
			message: &Message{Role: RoleSystem, Content: ContentBlocks{TextBlock(characterizationSystem)}},
			assert: func(t *testing.T, messages []anthropicsdk.MessageParam, system []anthropicsdk.TextBlockParam) {
				if len(messages) != 0 {
					t.Fatalf("system 不该进 messages，实际 %d 条", len(messages))
				}
				if len(system) != 1 {
					t.Fatalf("system 块数 = %d，想要 1", len(system))
				}
				if system[0].Text != characterizationSystem {
					t.Fatalf("system 文本 = %q，想要 %q", system[0].Text, characterizationSystem)
				}
			},
		},
		{
			name:    "user 纯文本",
			message: &Message{Role: RoleUser, Content: ContentBlocks{TextBlock(characterizationUser)}},
			assert: func(t *testing.T, messages []anthropicsdk.MessageParam, system []anthropicsdk.TextBlockParam) {
				if len(system) != 0 {
					t.Fatalf("user 消息不该产出 system 块: %+v", system)
				}
				if len(messages) != 1 {
					t.Fatalf("messages 条数 = %d，想要 1", len(messages))
				}
				if messages[0].Role != anthropicsdk.MessageParamRoleUser {
					t.Fatalf("role = %q，想要 %q", messages[0].Role, anthropicsdk.MessageParamRoleUser)
				}
				if len(messages[0].Content) != 1 {
					t.Fatalf("内容块数 = %d，想要 1", len(messages[0].Content))
				}
				block := messages[0].Content[0]
				if block.OfText == nil || block.OfText.Text != characterizationUser {
					t.Fatalf("内容块 = %+v，想要文本块 %q", block, characterizationUser)
				}
				if block.OfImage != nil {
					t.Fatalf("内容块同时填了 image: %+v", block.OfImage)
				}
			},
		},
		{
			name: "user 文本 + 图片",
			message: &Message{Role: RoleUser, Content: ContentBlocks{
				TextBlock(characterizationUser),
				ImageBlock(characterizationImageURL),
			}},
			assert: func(t *testing.T, messages []anthropicsdk.MessageParam, system []anthropicsdk.TextBlockParam) {
				if len(messages) != 1 {
					t.Fatalf("messages 条数 = %d，想要 1", len(messages))
				}
				if messages[0].Role != anthropicsdk.MessageParamRoleUser {
					t.Fatalf("role = %q，想要 %q", messages[0].Role, anthropicsdk.MessageParamRoleUser)
				}
				blocks := messages[0].Content
				if len(blocks) != 2 {
					t.Fatalf("内容块数 = %d，想要 2", len(blocks))
				}
				// 顺序跟着内容块走：文本在前，图片在后。
				if blocks[0].OfText == nil || blocks[0].OfText.Text != characterizationUser {
					t.Fatalf("blocks[0] = %+v，想要文本块 %q", blocks[0], characterizationUser)
				}
				if blocks[1].OfImage == nil {
					t.Fatalf("blocks[1] 不是 image 块: %+v", blocks[1])
				}
				if blocks[1].OfImage.Source.OfURL == nil ||
					blocks[1].OfImage.Source.OfURL.URL != characterizationImageURL {
					t.Fatalf("blocks[1] 图片 source = %+v，想要 URL %q",
						blocks[1].OfImage.Source, characterizationImageURL)
				}
				if blocks[1].OfImage.Source.OfBase64 != nil {
					t.Fatalf("blocks[1] 同时填了 base64 source: %+v", blocks[1].OfImage.Source.OfBase64)
				}
			},
		},
		{
			name:    "assistant 纯文本",
			message: &Message{Role: RoleAssistant, Content: ContentBlocks{TextBlock(characterizationAssistant)}},
			assert: func(t *testing.T, messages []anthropicsdk.MessageParam, system []anthropicsdk.TextBlockParam) {
				if len(messages) != 1 {
					t.Fatalf("messages 条数 = %d，想要 1", len(messages))
				}
				if messages[0].Role != anthropicsdk.MessageParamRoleAssistant {
					t.Fatalf("role = %q，想要 %q", messages[0].Role, anthropicsdk.MessageParamRoleAssistant)
				}
				if len(messages[0].Content) != 1 {
					t.Fatalf("内容块数 = %d，想要 1", len(messages[0].Content))
				}
				block := messages[0].Content[0]
				if block.OfText == nil || block.OfText.Text != characterizationAssistant {
					t.Fatalf("内容块 = %+v，想要文本块 %q", block, characterizationAssistant)
				}
				if block.OfToolUse != nil {
					t.Fatalf("无工具调用的 assistant 带了 tool_use: %+v", block.OfToolUse)
				}
			},
		},
		{
			name: "assistant 文本 + 工具调用",
			message: &Message{
				Role:    RoleAssistant,
				Content: ContentBlocks{TextBlock(characterizationAssistant)},
				ToolCalls: ToolCalls{{
					ID: characterizationToolID, Name: "read_file",
					Arguments: json.RawMessage(characterizationToolArgs),
				}},
			},
			assert: func(t *testing.T, messages []anthropicsdk.MessageParam, system []anthropicsdk.TextBlockParam) {
				if len(messages) != 1 {
					t.Fatalf("messages 条数 = %d，想要 1", len(messages))
				}
				if messages[0].Role != anthropicsdk.MessageParamRoleAssistant {
					t.Fatalf("role = %q，想要 %q", messages[0].Role, anthropicsdk.MessageParamRoleAssistant)
				}
				blocks := messages[0].Content
				if len(blocks) != 2 {
					t.Fatalf("内容块数 = %d，想要 2", len(blocks))
				}
				if blocks[0].OfText == nil || blocks[0].OfText.Text != characterizationAssistant {
					t.Fatalf("blocks[0] = %+v，想要文本块 %q", blocks[0], characterizationAssistant)
				}
				if blocks[1].OfToolUse == nil {
					t.Fatalf("blocks[1] 不是 tool_use 块: %+v", blocks[1])
				}
				toolUse := blocks[1].OfToolUse
				if toolUse.ID != characterizationToolID {
					t.Fatalf("tool_use id = %q，想要 %q", toolUse.ID, characterizationToolID)
				}
				if toolUse.Name != "read_file" {
					t.Fatalf("tool_use name = %q，想要 %q", toolUse.Name, "read_file")
				}
				// 与 OpenAI 侧相反：Arguments 在这里被解析成值再交给 SDK。
				want := map[string]any{"path": "README.md"}
				if !reflect.DeepEqual(toolUse.Input, want) {
					t.Fatalf("tool_use input = %#v，想要 %#v", toolUse.Input, want)
				}
			},
		},
		{
			name: "tool 结果",
			message: &Message{
				Role: RoleTool, Content: ContentBlocks{TextBlock(characterizationToolText)},
				ToolCallID: characterizationToolID, ToolName: "read_file", IsError: true,
			},
			assert: func(t *testing.T, messages []anthropicsdk.MessageParam, system []anthropicsdk.TextBlockParam) {
				if len(messages) != 1 {
					t.Fatalf("messages 条数 = %d，想要 1", len(messages))
				}
				// tool 结果在 Anthropic 线上是 user 消息，不是独立角色。
				if messages[0].Role != anthropicsdk.MessageParamRoleUser {
					t.Fatalf("role = %q，想要 %q", messages[0].Role, anthropicsdk.MessageParamRoleUser)
				}
				if len(messages[0].Content) != 1 {
					t.Fatalf("内容块数 = %d，想要 1", len(messages[0].Content))
				}
				block := messages[0].Content[0]
				if block.OfToolResult == nil {
					t.Fatalf("内容块不是 tool_result: %+v", block)
				}
				result := block.OfToolResult
				if result.ToolUseID != characterizationToolID {
					t.Fatalf("tool_use_id = %q，想要 %q", result.ToolUseID, characterizationToolID)
				}
				if !result.IsError.Valid() || !result.IsError.Value {
					t.Fatalf("is_error = %v (valid=%v)，想要 true", result.IsError.Value, result.IsError.Valid())
				}
				if len(result.Content) != 1 || result.Content[0].OfText == nil {
					t.Fatalf("tool_result 内容 = %+v，想要一个文本块", result.Content)
				}
				if result.Content[0].OfText.Text != characterizationToolText {
					t.Fatalf("tool_result 文本 = %q，想要 %q", result.Content[0].OfText.Text, characterizationToolText)
				}
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			messages, system, err := Messages{testCase.message}.ToAnthropicMessages()
			if err != nil {
				t.Fatalf("ToAnthropicMessages: %v", err)
			}
			testCase.assert(t, messages, system)
		})
	}
}

// TestToAnthropicMessagesKeepsOrderAroundSystem 钉住 system 块从消息序列里被摘出来
// 之后，其余消息的相对顺序不变。
func TestToAnthropicMessagesKeepsOrderAroundSystem(t *testing.T) {
	messages, system, err := Messages{
		{Role: RoleSystem, Content: ContentBlocks{TextBlock(characterizationSystem)}},
		{Role: RoleUser, Content: ContentBlocks{TextBlock(characterizationUser)}},
		{Role: RoleAssistant, Content: ContentBlocks{TextBlock(characterizationAssistant)}},
		{Role: RoleTool, Content: ContentBlocks{TextBlock(characterizationToolText)}, ToolCallID: characterizationToolID},
	}.ToAnthropicMessages()
	if err != nil {
		t.Fatalf("ToAnthropicMessages: %v", err)
	}
	if len(system) != 1 || system[0].Text != characterizationSystem {
		t.Fatalf("system 块 = %+v，想要一条 %q", system, characterizationSystem)
	}
	if len(messages) != 3 {
		t.Fatalf("messages 条数 = %d，想要 3", len(messages))
	}
	wantRoles := []anthropicsdk.MessageParamRole{
		anthropicsdk.MessageParamRoleUser,
		anthropicsdk.MessageParamRoleAssistant,
		anthropicsdk.MessageParamRoleUser,
	}
	for index, want := range wantRoles {
		if messages[index].Role != want {
			t.Fatalf("messages[%d].Role = %q，想要 %q", index, messages[index].Role, want)
		}
	}
}

// requireOpenAIVariant 断言联合类型里只有 want 这一个分支被填上。
func requireOpenAIVariant(t *testing.T, param openaisdk.ChatCompletionMessageParamUnion, want string) {
	t.Helper()

	if got := openAIVariantName(param); got != want {
		t.Fatalf("联合类型分支 = %q，想要 %q", got, want)
	}
}

// openAIVariantName 返回被填上的分支名；没有任何分支时返回空字符串。
func openAIVariantName(param openaisdk.ChatCompletionMessageParamUnion) string {
	switch {
	case param.OfSystem != nil:
		return "OfSystem"
	case param.OfUser != nil:
		return "OfUser"
	case param.OfAssistant != nil:
		return "OfAssistant"
	case param.OfTool != nil:
		return "OfTool"
	case param.OfDeveloper != nil:
		return "OfDeveloper"
	case param.OfFunction != nil:
		return "OfFunction"
	default:
		return ""
	}
}
