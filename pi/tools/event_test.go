package tools

import (
	"testing"

	"github.com/PycMono/go-harness/pi/schema"
)

// 本文件是 pi/tools/event.go 的第一组测试：钉住结束事件到工具结果消息的转换——
// 身份字段原样搬运、内容是副本、身份不全时明确报错而不是交出一条半成品消息。

const (
	eventToolCallID = "call-read-file"
	eventToolName   = "read_file"
	eventImageURL   = "https://example.test/screenshot.png"
)

// TestResultMessagePreservesIdentityAndContent 覆盖转换的产物：动态类型、身份
// 三件套（调用 ID、工具名、是否错误）与内容块逐块保留，图片块也在其中。
func TestResultMessagePreservesIdentityAndContent(t *testing.T) {
	cases := []struct {
		name    string
		event   Event
		want    schema.ContentBlocks
		isError bool
	}{
		{
			name: "成功结果的文本",
			event: NewEndEvent(schema.ToolCall{ID: eventToolCallID, Name: eventToolName},
				schema.ToolOutput{Content: schema.ContentBlocks{schema.TextBlock("文件读完了")}},
				false, 0),
			want: schema.ContentBlocks{schema.TextBlock("文件读完了")},
		},
		{
			name: "失败结果保留 IsError",
			event: NewEndEvent(schema.ToolCall{ID: eventToolCallID, Name: eventToolName},
				schema.ToolOutput{Content: schema.ContentBlocks{schema.TextBlock("没有这个文件")}},
				true, 0),
			want:    schema.ContentBlocks{schema.TextBlock("没有这个文件")},
			isError: true,
		},
		{
			name: "图片块原样进工具结果",
			event: NewEndEvent(schema.ToolCall{ID: eventToolCallID, Name: eventToolName},
				schema.ToolOutput{Content: schema.ContentBlocks{
					schema.TextBlock("截图"),
					schema.ImageBlock(eventImageURL),
				}},
				false, 0),
			want: schema.ContentBlocks{
				schema.TextBlock("截图"),
				schema.ImageBlock(eventImageURL),
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			message, err := testCase.event.ResultMessage()
			if err != nil {
				t.Fatalf("ResultMessage: %v", err)
			}
			result, ok := message.(*schema.ToolResultMessage)
			if !ok {
				t.Fatalf("ResultMessage 的动态类型 = %T，想要 *schema.ToolResultMessage", message)
			}
			if result == nil {
				t.Fatal("ResultMessage 交出来的是 typed nil 指针")
			}
			if got := result.Role(); got != schema.RoleTool {
				t.Fatalf("Role() = %q，想要 %q", got, schema.RoleTool)
			}
			if result.ToolCallID != eventToolCallID {
				t.Fatalf("ToolCallID = %q，想要 %q", result.ToolCallID, eventToolCallID)
			}
			if result.ToolName != eventToolName {
				t.Fatalf("ToolName = %q，想要 %q", result.ToolName, eventToolName)
			}
			if result.IsError != testCase.isError {
				t.Fatalf("IsError = %v，想要 %v", result.IsError, testCase.isError)
			}
			assertEventContentBlocks(t, result.Content, testCase.want)
		})
	}
}

// TestResultMessageCopiesContent 钉住"内容是副本"：事件之后被改写，已经交给模型的
// 那条消息不能跟着变——工具实现持有事件内容块的所有权，消息不是它的别名。
func TestResultMessageCopiesContent(t *testing.T) {
	// imageBlock 与下面事件里的那个块共享同一个 Image 指针，后面从这条路径改写，
	// 等价于改事件自己的图片内容。
	imageBlock := schema.ImageBlock(eventImageURL)
	event := NewEndEvent(schema.ToolCall{ID: eventToolCallID, Name: eventToolName},
		schema.ToolOutput{Content: schema.ContentBlocks{
			schema.TextBlock("原始正文"),
			imageBlock,
		}},
		false, 0)

	message, err := event.ResultMessage()
	if err != nil {
		t.Fatalf("ResultMessage: %v", err)
	}
	result, ok := message.(*schema.ToolResultMessage)
	if !ok {
		t.Fatalf("ResultMessage 的动态类型 = %T，想要 *schema.ToolResultMessage", message)
	}

	// 改写事件自己的内容块：文本、底层数组的元素、图片指针指向的结构体，以及整条切片。
	event.Content[0].Text = "改过的正文"
	*imageBlock.Image = schema.ImageContent{URL: "https://example.test/other.png"}
	event.Content[1] = schema.TextBlock("整块换掉")
	event.Content = append(event.Content, schema.TextBlock("追加的块"))

	assertEventContentBlocks(t, result.Content, schema.ContentBlocks{
		schema.TextBlock("原始正文"),
		schema.ImageBlock(eventImageURL),
	})
}

// TestResultMessageRejectsIncompleteIdentity 钉住身份不全就是错误：工具结果靠
// 调用 ID 与工具名回填给发起它的那次调用，缺一个都不该产出消息。返回值必须是
// nil 接口——typed nil 会骗过调用方的 message == nil 判定。
func TestResultMessageRejectsIncompleteIdentity(t *testing.T) {
	cases := []struct {
		name string
		call schema.ToolCall
	}{
		{name: "缺调用 ID", call: schema.ToolCall{Name: eventToolName}},
		{name: "缺工具名", call: schema.ToolCall{ID: eventToolCallID}},
		{name: "两者都缺", call: schema.ToolCall{}},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			event := NewEndEvent(testCase.call,
				schema.ToolOutput{Content: schema.ContentBlocks{schema.TextBlock("正文")}},
				false, 0)

			message, err := event.ResultMessage()
			if err == nil {
				t.Fatalf("身份不全仍产出了消息: %T", message)
			}
			if message != nil {
				t.Fatalf("出错时 message 的动态类型 = %T，想要 nil 接口", message)
			}
		})
	}
}

// assertEventContentBlocks 逐块比对内容块：类型、文本与图片 URL 都要一致。
func assertEventContentBlocks(t *testing.T, got, want schema.ContentBlocks) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("内容块数 = %d，想要 %d", len(got), len(want))
	}
	for index := range want {
		if got[index].Type != want[index].Type {
			t.Fatalf("内容块 %d 类型 = %q，想要 %q", index, got[index].Type, want[index].Type)
		}
		if got[index].Text != want[index].Text {
			t.Fatalf("内容块 %d 文本 = %q，想要 %q", index, got[index].Text, want[index].Text)
		}
		if (got[index].Image == nil) != (want[index].Image == nil) {
			t.Fatalf("内容块 %d 图片 = %+v，想要 %+v", index, got[index].Image, want[index].Image)
		}
		if got[index].Image != nil && got[index].Image.URL != want[index].Image.URL {
			t.Fatalf("内容块 %d 图片 URL = %q，想要 %q", index, got[index].Image.URL, want[index].Image.URL)
		}
	}
}
