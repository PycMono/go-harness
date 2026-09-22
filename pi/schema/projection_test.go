package schema

import (
	"reflect"
	"strings"
	"testing"

	anthropicsdk "github.com/anthropics/anthropic-sdk-go"
	openaisdk "github.com/openai/openai-go/v3"
)

// 本文件钉住任务三的文本投影规则：
//   - Content.Text() 保持严格，遇到图片块报错——只有真正需要纯文本的调用方能用它；
//   - 工具结果到 Anthropic 时图片原样进 tool_result（该协议支持多模态工具结果），
//     不做占位降级；没有图片时全部文本合成一个 text 成员，有图片时每个内容块各自成
//     一个成员且保持内容顺序，纯图片结果在图片前补一个锚点文本；
//   - 工具结果到 OpenAI 时只出文本，选词按 pi.dev 的三路规则（有文本用文本、无文本
//     有图片用固定英文占位、都没有用另一个固定占位）。

const (
	projectionToolText  = "工具结果正文"
	projectionToolImage = "https://example.test/tool.png"
	projectionToolID    = "call-projection"
)

// TestContentTextRejectsImageBlocks 钉住 Content.Text() 的严格语义：它拒绝任何非
// 文本块，调用方拿不到"半截文本 + 悄悄丢图"的结果。
func TestContentTextRejectsImageBlocks(t *testing.T) {
	cases := []struct {
		name    string
		content ContentBlocks
	}{
		{
			name:    "文本 + 图片",
			content: ContentBlocks{TextBlock(projectionToolText), ImageBlock(projectionToolImage)},
		},
		{
			name:    "只有图片",
			content: ContentBlocks{ImageBlock(projectionToolImage)},
		},
		{
			name:    "未知块类型",
			content: ContentBlocks{{Type: ContentType("audio")}},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			text, err := testCase.content.Text()
			if err == nil {
				t.Fatalf("Text() = %q, nil，想要报错", text)
			}
			if text != "" {
				t.Fatalf("报错时文本 = %q，想要空串", text)
			}
		})
	}

	text, err := ContentBlocks{TextBlock("前"), TextBlock("后")}.Text()
	if err != nil {
		t.Fatalf("纯文本 Text(): %v", err)
	}
	if text != "前后" {
		t.Fatalf("纯文本 Text() = %q，想要 %q", text, "前后")
	}
}

// TestWithImagePlaceholdersRedactsImages 钉住共享的脱敏占位行为：图片块换成
// ImagePlaceholderText 的精确字符串，文本块原样保留，结果里没有图片块。
// 任务三没有新增"文本 + 占位"的辅助函数（没有调用方），所以这里直接钉住它组合
// 出来的原料。
func TestWithImagePlaceholdersRedactsImages(t *testing.T) {
	content := ContentBlocks{TextBlock(projectionToolText), ImageBlock(projectionToolImage)}

	redacted := content.WithImagePlaceholders()
	if len(redacted) != 2 {
		t.Fatalf("占位后块数 = %d，想要 2", len(redacted))
	}
	if redacted[0].Type != ContentTypeText || redacted[0].Text != projectionToolText {
		t.Fatalf("redacted[0] = %+v，想要原文本块 %q", redacted[0], projectionToolText)
	}
	const wantPlaceholder = "[图片: https://example.test/tool.png]"
	if redacted[1].Type != ContentTypeText || redacted[1].Text != wantPlaceholder {
		t.Fatalf("redacted[1] = %+v，想要占位文本 %q", redacted[1], wantPlaceholder)
	}
	// 同一处实现产出的字符串：占位词不会出现第二份副本。
	if redacted[1].Text != ImagePlaceholderText(projectionToolImage) {
		t.Fatalf("占位 = %q，ImagePlaceholderText = %q，两者必须一致",
			redacted[1].Text, ImagePlaceholderText(projectionToolImage))
	}
	for _, block := range redacted {
		if block.Type == ContentTypeImage || block.Image != nil {
			t.Fatalf("占位后仍有图片块: %+v", block)
		}
	}

	// 没有图片时文本块不变，且可以被严格投影取回。
	textOnly := ContentBlocks{TextBlock(projectionToolText)}
	unchanged := textOnly.WithImagePlaceholders()
	if !reflect.DeepEqual(unchanged, textOnly) {
		t.Fatalf("无图块集合被改动: %+v，想要 %+v", unchanged, textOnly)
	}
	text, err := unchanged.Text()
	if err != nil {
		t.Fatalf("占位后纯文本 Text(): %v", err)
	}
	if text != projectionToolText {
		t.Fatalf("占位后纯文本 = %q，想要 %q", text, projectionToolText)
	}
}

// TestToAnthropicToolResultCarriesImages 钉住 Anthropic 侧 tool_result 的多模态
// 内容：文本成员与图片成员按内容顺序排列，ToolUseID 与 IsError 不变。
func TestToAnthropicToolResultCarriesImages(t *testing.T) {
	block := projectionAnthropicToolResult(t, &ToolResultMessage{
		Content:    ContentBlocks{TextBlock(projectionToolText), ImageBlock(projectionToolImage)},
		ToolCallID: projectionToolID, ToolName: "read_file", IsError: true,
	})

	if block.ToolUseID != projectionToolID {
		t.Fatalf("tool_use_id = %q，想要 %q", block.ToolUseID, projectionToolID)
	}
	if !block.IsError.Valid() || !block.IsError.Value {
		t.Fatalf("is_error = %v (valid=%v)，想要 true", block.IsError.Value, block.IsError.Valid())
	}

	projectionAssertContent(t, block.Content, []projectionContentMember{
		{text: projectionToolText},
		{image: projectionToolImage},
	})
}

// TestToAnthropicToolResultPreservesBlockOrder 钉住有图片时 tool_result 的成员顺序：
// 每个内容块各自成一个成员，按内容顺序排列——文本块不合并，排在图片之后的文本块也不
// 会被挪到图片前面。判据照搬 pi.dev 的 convertContentBlocks（anthropic-messages.ts:148-163）：
// 只要内容里有图片块就逐块映射；有文本块就不补锚点，空文本块也算"有文本块"。
func TestToAnthropicToolResultPreservesBlockOrder(t *testing.T) {
	cases := []struct {
		name    string
		content ContentBlocks
		want    []projectionContentMember
	}{
		{
			name:    "文本 + 图片 + 文本",
			content: ContentBlocks{TextBlock("前"), ImageBlock(projectionToolImage), TextBlock("后")},
			want: []projectionContentMember{
				{text: "前"},
				{image: projectionToolImage},
				{text: "后"},
			},
		},
		{
			name:    "空文本块也算有文本块，不补锚点",
			content: ContentBlocks{TextBlock(""), ImageBlock(projectionToolImage)},
			want: []projectionContentMember{
				{text: ""},
				{image: projectionToolImage},
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			block := projectionAnthropicToolResult(t, &ToolResultMessage{
				Content:    testCase.content,
				ToolCallID: projectionToolID, ToolName: "read_file",
			})

			projectionAssertContent(t, block.Content, testCase.want)
		})
	}
}

// TestToAnthropicImageOnlyToolResult 钉住纯图片工具结果：图片成员前补一个锚点文本成员
// （pi.dev 在 anthropic-messages.ts:165-169 补的 "(see attached image)"），不发出没有
// 文本锚点的裸图片 content。
func TestToAnthropicImageOnlyToolResult(t *testing.T) {
	block := projectionAnthropicToolResult(t, &ToolResultMessage{
		Content:    ContentBlocks{ImageBlock(projectionToolImage)},
		ToolCallID: projectionToolID, ToolName: "read_file",
	})

	projectionAssertContent(t, block.Content, []projectionContentMember{
		{text: "(see attached image)"},
		{image: projectionToolImage},
	})
}

// TestToAnthropicMultiTextToolResultUnchanged 钉住没有图片时的多文本块仍是逐字节一致的
// 旧形状：全部文本块按顺序拼成一个 text 成员，整块（含未导出字段）与旧路径
// NewToolResultBlock 的产物 DeepEqual。
func TestToAnthropicMultiTextToolResultUnchanged(t *testing.T) {
	block := projectionAnthropicToolResult(t, &ToolResultMessage{
		Content:    ContentBlocks{TextBlock("前"), TextBlock("后")},
		ToolCallID: projectionToolID, ToolName: "read_file", IsError: true,
	})

	want := anthropicsdk.NewToolResultBlock(projectionToolID, "前后", true)
	if want.OfToolResult == nil {
		t.Fatal("旧路径构造失败")
	}
	if !reflect.DeepEqual(*block, *want.OfToolResult) {
		t.Fatalf("tool_result = %+v，想要与旧路径逐字段一致: %+v", *block, *want.OfToolResult)
	}
}

// TestToAnthropicTextOnlyToolResultUnchanged 钉住纯文本工具结果的线格式与前
// 任务完全一致：整块等于旧路径的 NewToolResultBlock 产物。
func TestToAnthropicTextOnlyToolResultUnchanged(t *testing.T) {
	cases := []struct {
		name    string
		content ContentBlocks
		want    string
		isError bool
	}{
		{
			name:    "有文本",
			content: ContentBlocks{TextBlock(projectionToolText)},
			want:    projectionToolText,
			isError: true,
		},
		{
			name:    "空结果",
			content: nil,
			want:    "",
			isError: false,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			block := projectionAnthropicToolResult(t, &ToolResultMessage{
				Content:    testCase.content,
				ToolCallID: projectionToolID, ToolName: "read_file", IsError: testCase.isError,
			})

			want := anthropicsdk.NewToolResultBlock(projectionToolID, testCase.want, testCase.isError)
			if want.OfToolResult == nil {
				t.Fatal("旧路径构造失败")
			}
			if !reflect.DeepEqual(*block, *want.OfToolResult) {
				t.Fatalf("tool_result = %+v，想要与旧路径逐字段一致: %+v", *block, *want.OfToolResult)
			}
		})
	}
}

// TestToOpenAIToolResultTextProjection 钉住 pi.dev 的三路选词：有文本用文本
// （拼接口径与 Content.Text() 一致）、无文本有图片用固定英文占位、两者都没有
// 用另一个固定占位。tool 消息在 OpenAI 线上只有文本位置，图片一律不进去。
func TestToOpenAIToolResultTextProjection(t *testing.T) {
	cases := []struct {
		name    string
		content ContentBlocks
		want    string
	}{
		{
			name:    "有文本",
			content: ContentBlocks{TextBlock(projectionToolText)},
			want:    projectionToolText,
		},
		{
			name:    "有文本 + 图片：只出文本",
			content: ContentBlocks{TextBlock(projectionToolText), ImageBlock(projectionToolImage)},
			want:    projectionToolText,
		},
		{
			name:    "无文本 + 图片",
			content: ContentBlocks{ImageBlock(projectionToolImage)},
			want:    "(see attached image)",
		},
		{
			name:    "空结果",
			content: nil,
			want:    "(no tool output)",
		},
		{
			name:    "空文本块",
			content: ContentBlocks{TextBlock("")},
			want:    "(no tool output)",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			params, err := Messages{&ToolResultMessage{
				Content:    testCase.content,
				ToolCallID: projectionToolID, ToolName: "read_file", IsError: true,
			}}.ToOpenAIMessages()
			if err != nil {
				t.Fatalf("ToOpenAIMessages: %v", err)
			}
			if len(params) != 1 {
				t.Fatalf("OpenAI 消息条数 = %d，想要 1", len(params))
			}
			if variants := openAIVariantName(params[0]); variants != "OfTool" {
				t.Fatalf("联合分支 = %q，想要 OfTool", variants)
			}

			tool := params[0].OfTool
			if tool.ToolCallID != projectionToolID {
				t.Fatalf("tool_call_id = %q，想要 %q", tool.ToolCallID, projectionToolID)
			}
			if !tool.Content.OfString.Valid() || tool.Content.OfString.Value != testCase.want {
				t.Fatalf("tool content = %q (valid=%v)，想要 %q",
					tool.Content.OfString.Value, tool.Content.OfString.Valid(), testCase.want)
			}
			// 文本形式与 parts 形式互斥：图片没有可落的地方，这里必须是字符串形式。
			if len(tool.Content.OfArrayOfContentParts) != 0 {
				t.Fatalf("tool content 用了 parts 形式: %+v", tool.Content.OfArrayOfContentParts)
			}
		})
	}
}

// TestToOpenAITextOnlyToolResultUnchanged 钉住有文本的工具结果与旧路径逐字段一致。
func TestToOpenAITextOnlyToolResultUnchanged(t *testing.T) {
	params, err := Messages{&ToolResultMessage{
		Content:    ContentBlocks{TextBlock(projectionToolText)},
		ToolCallID: projectionToolID, ToolName: "read_file", IsError: true,
	}}.ToOpenAIMessages()
	if err != nil {
		t.Fatalf("ToOpenAIMessages: %v", err)
	}

	want := openaisdk.ToolMessage(projectionToolText, projectionToolID)
	if !reflect.DeepEqual(params[0], want) {
		t.Fatalf("tool 消息 = %+v，想要与旧路径逐字段一致: %+v", params[0], want)
	}
}

// TestOpenAIToolResultFallbackIsNotPlaceholder 钉住 OpenAI tool 消息用的不是脱敏
// 占位文本：降级占位（[图片: …]）是摘要与日志路径的词，不属于 tool 消息。
func TestOpenAIToolResultFallbackIsNotPlaceholder(t *testing.T) {
	params, err := Messages{&ToolResultMessage{
		Content:    ContentBlocks{ImageBlock(projectionToolImage)},
		ToolCallID: projectionToolID, ToolName: "read_file",
	}}.ToOpenAIMessages()
	if err != nil {
		t.Fatalf("ToOpenAIMessages: %v", err)
	}

	got := params[0].OfTool.Content.OfString.Value
	if strings.Contains(got, "图片") {
		t.Fatalf("tool content = %q，不该出现脱敏占位词", got)
	}
	if got != "(see attached image)" {
		t.Fatalf("tool content = %q，想要 %q", got, "(see attached image)")
	}
}

// TestToolResultImageBlockWithoutImageFails 钉住脏块的防线：图片块缺 Image 时两条
// 投影路径都报错，不去解引用空指针。
func TestToolResultImageBlockWithoutImageFails(t *testing.T) {
	message := &ToolResultMessage{
		Content:    ContentBlocks{{Type: ContentTypeImage}},
		ToolCallID: projectionToolID, ToolName: "read_file",
	}

	messages := Messages{message}
	if _, err := messages.ToOpenAIMessages(); err == nil {
		t.Fatal("ToOpenAIMessages 对缺 Image 的图片块没有报错")
	}
	_, _, err := messages.ToAnthropicMessages()
	if err == nil {
		t.Fatal("ToAnthropicMessages 对缺 Image 的图片块没有报错")
	}
	const wantMessage = "image block requires image content"
	if !strings.Contains(err.Error(), wantMessage) {
		t.Fatalf("ToAnthropicMessages 报错 = %q，想要包含 %q", err, wantMessage)
	}
}

// TestAnthropicToolResultRejectsUnknownBlockTypes 钉住未知块类型在 Anthropic 工具结果
// 的两条分支上都报错：没有图片时不能因为"只拼文本"就把来路不明的块静默吞掉。
func TestAnthropicToolResultRejectsUnknownBlockTypes(t *testing.T) {
	cases := []struct {
		name    string
		content ContentBlocks
	}{
		{
			name:    "无图片",
			content: ContentBlocks{TextBlock(projectionToolText), {Type: ContentType("audio")}},
		},
		{
			name: "有图片",
			content: ContentBlocks{
				TextBlock(projectionToolText), ImageBlock(projectionToolImage),
				{Type: ContentType("audio")},
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, _, err := Messages{&ToolResultMessage{
				Content:    testCase.content,
				ToolCallID: projectionToolID, ToolName: "read_file",
			}}.ToAnthropicMessages()
			if err == nil {
				t.Fatal("ToAnthropicMessages 对未知块类型没有报错")
			}
			const wantMessage = `unsupported content type "audio"`
			if !strings.Contains(err.Error(), wantMessage) {
				t.Fatalf("ToAnthropicMessages 报错 = %q，想要包含 %q", err, wantMessage)
			}
		})
	}
}

// projectionAnthropicToolResult 取出一条工具结果转 Anthropic 后唯一的 tool_result 块。
func projectionAnthropicToolResult(
	t *testing.T, message Message,
) *anthropicsdk.ToolResultBlockParam {
	t.Helper()

	messages, system, err := Messages{message}.ToAnthropicMessages()
	if err != nil {
		t.Fatalf("ToAnthropicMessages: %v", err)
	}
	if len(system) != 0 {
		t.Fatalf("工具结果不该产出 system 块: %+v", system)
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
	block := messages[0].Content[0].OfToolResult
	if block == nil {
		t.Fatalf("内容块不是 tool_result: %+v", messages[0].Content[0])
	}

	return block
}

// projectionContentMember 描述 tool_result 里一个期望的内容成员：文本成员填 text，
// URL 图片成员填 image，两者恰好填一个（空文本成员靠 image 为空来区分）。
type projectionContentMember struct {
	text  string
	image string
}

// projectionAssertContent 逐成员断言 tool_result 的内容：成员数量、顺序、每个成员的类型
// 与取值，以及每个成员只填联合类型的一个分支。
func projectionAssertContent(
	t *testing.T,
	content []anthropicsdk.ToolResultBlockParamContentUnion,
	want []projectionContentMember,
) {
	t.Helper()

	if len(content) != len(want) {
		t.Fatalf("tool_result 内容成员数 = %d，想要 %d: %+v", len(content), len(want), content)
	}
	for index, member := range content {
		if want[index].image != "" {
			if member.OfImage == nil {
				t.Fatalf("content[%d] 不是 image 成员: %+v", index, member)
			}
			if member.OfImage.Source.OfURL == nil ||
				member.OfImage.Source.OfURL.URL != want[index].image {
				t.Fatalf("content[%d] 图片 source = %+v，想要 URL %q",
					index, member.OfImage.Source, want[index].image)
			}
			if member.OfImage.Source.OfBase64 != nil {
				t.Fatalf("content[%d] 同时填了 base64 source: %+v",
					index, member.OfImage.Source.OfBase64)
			}
			if member.OfText != nil {
				t.Fatalf("content[%d] 同时填了 text: %+v", index, member.OfText)
			}
			continue
		}
		if member.OfText == nil || member.OfText.Text != want[index].text {
			t.Fatalf("content[%d] = %+v，想要文本成员 %q", index, member, want[index].text)
		}
		if member.OfImage != nil {
			t.Fatalf("content[%d] 同时填了 image: %+v", index, member.OfImage)
		}
	}
}
