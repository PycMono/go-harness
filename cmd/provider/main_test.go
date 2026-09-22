package main

import (
	"strings"
	"testing"

	"github.com/PycMono/go-harness/pi/schema"
)

// 本文件钉住流式结果的展示渲染：finish_reason 与 usage 只存在于 assistant 变体上，
// 别的角色只报角色并说明形态不同，不把零值当成真实结果打印；图片块降级成脱敏占位
// 而不是让整行变成空文本。渲染被抽成纯函数，这里直接比对字符串。

const (
	renderResultImageURL = "https://example.test/cat.png"
	renderResultText     = "Go 是静态类型语言。"
)

// TestRenderResult 钉住结果行的渲染：assistant 变体带 finish_reason 与 usage，
// 非 assistant 变体给出定义好的降级行。
func TestRenderResult(t *testing.T) {
	cases := []struct {
		name    string
		message schema.Message
		want    string
	}{
		{
			name: "assistant 带用量",
			message: &schema.AssistantMessage{
				Content:      schema.ContentBlocks{schema.TextBlock(renderResultText)},
				FinishReason: schema.FinishReasonStop,
				Usage:        &schema.Usage{InputTokens: 12, OutputTokens: 7, CacheReadTokens: 3},
			},
			want: "结果: role=assistant finish=stop text=\"" + renderResultText + "\"\n" +
				"Usage: input=12 output=7 cache_read=3\n",
		},
		{
			name: "assistant 无用量",
			message: &schema.AssistantMessage{
				Content: schema.ContentBlocks{schema.TextBlock("你好")},
			},
			// 没有用量就不打 Usage 行，而不是打一行零值。
			want: "结果: role=assistant finish= text=\"你好\"\n",
		},
		{
			name: "assistant 带图片降级成占位",
			message: &schema.AssistantMessage{
				Content: schema.ContentBlocks{
					schema.TextBlock("看图"), schema.ImageBlock(renderResultImageURL),
				},
			},
			want: "结果: role=assistant finish= text=\"看图[图片: https://example.test/cat.png]\"\n",
		},
		{
			name: "非 assistant 变体",
			message: &schema.ToolResultMessage{
				Content:    schema.ContentBlocks{schema.TextBlock("工具结果")},
				ToolCallID: "call-1",
				ToolName:   "bash",
			},
			want: "结果: role=tool 不是 assistant 变体，没有 finish_reason 与 usage\n",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := renderResult(testCase.message)
			if got != testCase.want {
				t.Fatalf("renderResult() = %q，想要 %q", got, testCase.want)
			}
		})
	}
}

// TestRenderResultNonAssistantDoesNotPrintZeroValues 钉住非 assistant 变体的行为：
// 只报角色并说明形态不同，不把 finish_reason / usage 的零值当成真实结果打出来。
func TestRenderResultNonAssistantDoesNotPrintZeroValues(t *testing.T) {
	message := &schema.ToolResultMessage{
		Content:    schema.ContentBlocks{schema.TextBlock("工具结果")},
		ToolCallID: "call-1",
		ToolName:   "bash",
	}

	got := renderResult(message)
	if !strings.Contains(got, "role=tool") {
		t.Fatalf("renderResult() = %q，想要报出角色 tool", got)
	}
	for _, forbidden := range []string{"finish=", "Usage:"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("renderResult() = %q，不该出现 %q 这样的零值字段", got, forbidden)
		}
	}
}

// TestRenderResultDirtyImageBlock 钉住脏块（图片块缺 Image）：严格投影在这里会报错，
// 渲染必须留下带标记的降级文本，不能返回空串。
func TestRenderResultDirtyImageBlock(t *testing.T) {
	message := &schema.AssistantMessage{Content: schema.ContentBlocks{{Type: schema.ContentTypeImage}}}

	got := renderResult(message)
	if strings.TrimSpace(got) == "" {
		t.Fatal("脏块的渲染结果为空串，与「这条消息没文本」分不开")
	}
	if !strings.Contains(got, "内容渲染失败") {
		t.Fatalf("renderResult() = %q，想要带上「内容渲染失败」标记", got)
	}
}
