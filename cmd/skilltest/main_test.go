package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/PycMono/go-harness/pi/schema"
)

// 本文件钉住消息序列的展示渲染，也就是 printTranscript 逐条打印的每一行：四种变体
// 各自的形状、图片消息降级成脱敏占位而不是空行、工具调用与工具结果的标记。渲染被抽成
// 纯函数，这里直接比对字符串。
//
// 与 cmd/harness 的差别只有工具调用那行：这里原样打印参数，不压成一行。

const (
	renderImageURL = "https://example.test/skill.png"
	renderToolText = "SKILL.md 共 42 行"
)

// TestRenderMessage 逐变体钉住渲染结果，展示格式与迁移前逐字节一致。
func TestRenderMessage(t *testing.T) {
	cases := []struct {
		name    string
		message schema.Message
		want    string
	}{
		{
			name:    "system 文本",
			message: &schema.SystemMessage{Content: schema.ContentBlocks{schema.TextBlock("系统提示词")}},
			want:    "[system] 系统提示词\n",
		},
		{
			name:    "user 文本",
			message: &schema.UserMessage{Content: schema.ContentBlocks{schema.TextBlock("看看有哪些技能")}},
			want:    "[user] 看看有哪些技能\n",
		},
		{
			name: "user 文本加图片",
			message: &schema.UserMessage{Content: schema.ContentBlocks{
				schema.TextBlock("照这张图写"), schema.ImageBlock(renderImageURL),
			}},
			want: "[user] 照这张图写[图片: https://example.test/skill.png]\n",
		},
		{
			name:    "user 多行取首行",
			message: &schema.UserMessage{Content: schema.ContentBlocks{schema.TextBlock("第一行\n第二行")}},
			want:    "[user] 第一行 …\n",
		},
		{
			name:    "assistant 文本",
			message: &schema.AssistantMessage{Content: schema.ContentBlocks{schema.TextBlock("我先读技能")}},
			want:    "[assistant] 我先读技能\n",
		},
		{
			name: "assistant 带工具调用",
			message: &schema.AssistantMessage{
				Content: schema.ContentBlocks{schema.TextBlock("我来读")},
				ToolCalls: schema.ToolCalls{
					{ID: "call-1", Name: "read", Arguments: json.RawMessage(`{"path":"SKILL.md"}`)},
					{ID: "call-2", Name: "bash", Arguments: json.RawMessage(`{"command":"ls"}`)},
				},
			},
			// 这里原样打印参数，每个调用各占一行。
			want: "[assistant] 我来读\n" +
				"            └ call read {\"path\":\"SKILL.md\"}\n" +
				"            └ call bash {\"command\":\"ls\"}\n",
		},
		{
			name: "tool 结果",
			message: &schema.ToolResultMessage{
				Content:    schema.ContentBlocks{schema.TextBlock(renderToolText)},
				ToolCallID: "call-1",
				ToolName:   "read",
			},
			want: "[tool:read] result: " + renderToolText + "\n",
		},
		{
			name: "tool 错误结果",
			message: &schema.ToolResultMessage{
				Content:    schema.ContentBlocks{schema.TextBlock("文件不存在")},
				ToolCallID: "call-2",
				ToolName:   "read",
				IsError:    true,
			},
			want: "[tool:read] error: 文件不存在\n",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := renderMessage(testCase.message); got != testCase.want {
				t.Fatalf("renderMessage() = %q，想要 %q", got, testCase.want)
			}
		})
	}
}

// TestRenderMessageDegradesImages 钉住图片消息的降级：渲染出文本加脱敏占位，绝不因为
// 图片块就打印成空行。
func TestRenderMessageDegradesImages(t *testing.T) {
	cases := []struct {
		name    string
		message schema.Message
		want    string
	}{
		{
			name:    "user 只有图片",
			message: &schema.UserMessage{Content: schema.ContentBlocks{schema.ImageBlock(renderImageURL)}},
			want:    "[user] [图片: https://example.test/skill.png]\n",
		},
		{
			name: "assistant 文本加图片",
			message: &schema.AssistantMessage{Content: schema.ContentBlocks{
				schema.TextBlock("正文"), schema.ImageBlock(renderImageURL),
			}},
			want: "[assistant] 正文[图片: https://example.test/skill.png]\n",
		},
		{
			name: "tool 结果只有图片",
			message: &schema.ToolResultMessage{
				Content:    schema.ContentBlocks{schema.ImageBlock(renderImageURL)},
				ToolCallID: "call-3",
				ToolName:   "read",
			},
			want: "[tool:read] result: [图片: https://example.test/skill.png]\n",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := renderMessage(testCase.message); got != testCase.want {
				t.Fatalf("renderMessage() = %q，想要 %q", got, testCase.want)
			}
		})
	}
}

// TestRenderMessageDirtyImageBlock 钉住脏块（图片块缺 Image）：严格投影在这里会报错，
// 渲染必须留下带标记的降级文本，不能返回空串。
func TestRenderMessageDirtyImageBlock(t *testing.T) {
	cases := []struct {
		name    string
		message schema.Message
	}{
		{
			name:    "user 脏图片块",
			message: &schema.UserMessage{Content: schema.ContentBlocks{{Type: schema.ContentTypeImage}}},
		},
		{
			name: "tool 结果脏图片块",
			message: &schema.ToolResultMessage{
				Content:    schema.ContentBlocks{{Type: schema.ContentTypeImage}},
				ToolCallID: "call-4",
				ToolName:   "read",
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := renderMessage(testCase.message)
			if strings.TrimSpace(got) == "" {
				t.Fatal("脏块的渲染结果为空串，与「这条消息没文本」分不开")
			}
			if !strings.Contains(got, "内容渲染失败") {
				t.Fatalf("脏块的渲染结果 = %q，想要带上「内容渲染失败」标记", got)
			}
		})
	}
}
