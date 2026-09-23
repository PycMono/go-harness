package main

import (
	"strings"
	"testing"

	"github.com/PycMono/go-harness/pi/schema"
	"github.com/PycMono/go-harness/pi/session"
)

// 本文件钉住会话视角的两处展示渲染：entry 链上的一行（printEntries）与重建上下文里
// 的一行（printMessages）。两处都把载荷投影成文本，所以图片块降级成脱敏占位而不是
// 空行；header 行的 Message 是 nil 接口，这条路径不能解引用它。渲染被抽成纯函数，
// 这里直接比对字符串。

const (
	renderImageURL = "https://example.test/session.png"
	renderWorkDir  = "/tmp/work"
)

// TestRenderEntry 钉住 entry 行的渲染，含 header 行（载荷为 nil 接口）与 message 行。
func TestRenderEntry(t *testing.T) {
	cases := []struct {
		name  string
		entry session.Entry
		want  string
	}{
		{
			name: "header 行没有消息载荷",
			entry: session.Entry{
				Type:   session.EntryHeader,
				ID:     "chat-001",
				Header: &session.Header{ID: "chat-001", WorkDir: renderWorkDir},
			},
			want: "[chat-001] header chat-001 workdir=" + renderWorkDir + "\n",
		},
		{
			name: "message 行",
			entry: session.Entry{
				Type:     session.EntryMessage,
				ID:       "entry-2",
				ParentID: "chat-001",
				Message:  &schema.UserMessage{Content: schema.ContentBlocks{schema.TextBlock("我的工单号是 8899")}},
			},
			want: "[entry-2] ← chat-001 message user: 我的工单号是 8899\n",
		},
		{
			name: "message 行带图片",
			entry: session.Entry{
				Type:     session.EntryMessage,
				ID:       "entry-3",
				ParentID: "entry-2",
				Message: &schema.UserMessage{Content: schema.ContentBlocks{
					schema.TextBlock("看图"), schema.ImageBlock(renderImageURL),
				}},
			},
			want: "[entry-3] ← entry-2 message user: 看图[图片: https://example.test/session.png]\n",
		},
		{
			name: "message 行工具错误结果",
			entry: session.Entry{
				Type:     session.EntryMessage,
				ID:       "entry-4",
				ParentID: "entry-3",
				Message: &schema.ToolResultMessage{
					Content:    schema.ContentBlocks{schema.TextBlock("文件不存在")},
					ToolCallID: "call-1",
					ToolName:   "read",
					IsError:    true,
				},
			},
			want: "[entry-4] ← entry-3 message tool: 文件不存在\n",
		},
		{
			name: "message 行载荷缺失",
			entry: session.Entry{
				Type:     session.EntryMessage,
				ID:       "entry-5",
				ParentID: "entry-4",
			},
			want: "[entry-5] ← entry-4 message ?: !消息载荷缺失\n",
		},
		{
			name: "压缩边界行",
			entry: session.Entry{
				Type:     session.EntryCompaction,
				ID:       "entry-6",
				ParentID: "entry-5",
				Compaction: &session.Compaction{
					Summary:          "目标：记住工单号\n第二轮…",
					FirstKeptEntryID: "entry-3",
					TokensBefore:     50440,
				},
			},
			want: "[entry-6] ← entry-5 compaction first_kept=entry-3 tokens_before=50440 summary: 目标：记住工单号 …\n",
		},
		{
			name: "压缩边界行载荷缺失",
			entry: session.Entry{
				Type:     session.EntryCompaction,
				ID:       "entry-7",
				ParentID: "entry-6",
			},
			want: "[entry-7] ← entry-6 compaction !消息载荷缺失\n",
		},
		{
			name: "未知 entry 类型",
			entry: session.Entry{
				Type:     session.EntryType("branch"),
				ID:       "entry-6",
				ParentID: "entry-5",
			},
			want: "[entry-6] ← entry-5 branch\n",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := renderEntry(testCase.entry); got != testCase.want {
				t.Fatalf("renderEntry() = %q，想要 %q", got, testCase.want)
			}
		})
	}
}

// TestRenderMessage 钉住重建上下文里的一行（printMessages 逐条打印的那个）。
func TestRenderMessage(t *testing.T) {
	cases := []struct {
		name    string
		message schema.Message
		want    string
	}{
		{
			name:    "system 文本",
			message: &schema.SystemMessage{Content: schema.ContentBlocks{schema.TextBlock("你是助手")}},
			want:    "[system] 你是助手\n",
		},
		{
			name:    "user 文本",
			message: &schema.UserMessage{Content: schema.ContentBlocks{schema.TextBlock("你好")}},
			want:    "[user] 你好\n",
		},
		{
			name: "user 图片降级成占位",
			message: &schema.UserMessage{Content: schema.ContentBlocks{
				schema.TextBlock("看图"), schema.ImageBlock(renderImageURL),
			}},
			want: "[user] 看图[图片: https://example.test/session.png]\n",
		},
		{
			name:    "多行取首行",
			message: &schema.UserMessage{Content: schema.ContentBlocks{schema.TextBlock("第一行\n第二行")}},
			want:    "[user] 第一行 …\n",
		},
		{
			name:    "载荷缺失",
			message: nil,
			want:    "[?] !消息载荷缺失\n",
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
	message := &schema.UserMessage{Content: schema.ContentBlocks{{Type: schema.ContentTypeImage}}}

	got := renderMessage(message)
	if strings.TrimSpace(got) == "" {
		t.Fatal("脏块的渲染结果为空串，与「这条消息没文本」分不开")
	}
	if !strings.Contains(got, "内容渲染失败") {
		t.Fatalf("renderMessage() = %q，想要带上「内容渲染失败」标记", got)
	}
}
