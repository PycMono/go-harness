package pi

import (
	"testing"

	"github.com/PycMono/go-harness/pi/schema"
)

// 本文件只补 pi/runner_test.go 没覆盖到的 RunOutput 读取行为：Message() 的 nil 接口
// 契约，以及 Text() 在非纯文本最终消息上的退化。Message2AI 的规则覆盖仍归
// pi/runner_test.go，这里不重复。

// TestRunOutputMessageNeverReturnsTypedNil 钉住 Message() 的 nil 契约：没有
// assistant 消息时交出来的必须是 nil 接口。typed nil（装着 (*schema.UserMessage)(nil)
// 的非 nil 接口）会骗过调用方的 message == nil 判定，让"没有答复"看起来像有答复。
func TestRunOutputMessageNeverReturnsTypedNil(t *testing.T) {
	toolResult := &schema.ToolResultMessage{
		Content:    schema.ContentBlocks{schema.TextBlock("工具结果")},
		ToolCallID: "call-read-file", ToolName: "read_file",
	}
	final := &schema.AssistantMessage{Content: schema.ContentBlocks{schema.TextBlock("最终答复")}}

	cases := []struct {
		name     string
		messages schema.Messages
		want     schema.Message
	}{
		{
			name:     "空序列",
			messages: nil,
			want:     nil,
		},
		{
			name: "只有非 assistant 消息",
			messages: schema.Messages{
				&schema.UserMessage{Content: schema.ContentBlocks{schema.TextBlock("客户")}},
				toolResult,
			},
			want: nil,
		},
		{
			name: "取的是最后一条 assistant 消息",
			messages: schema.Messages{
				&schema.UserMessage{Content: schema.ContentBlocks{schema.TextBlock("客户")}},
				&schema.AssistantMessage{Content: schema.ContentBlocks{schema.TextBlock("先看一眼")}},
				toolResult,
				final,
			},
			want: final,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			output := &RunOutput{message: testCase.messages}

			message := output.Message()
			if testCase.want == nil {
				if message != nil {
					t.Fatalf("Message() 的动态类型 = %T，想要 nil 接口", message)
				}
				return
			}
			if message != testCase.want {
				t.Fatalf("Message() 的动态类型 = %T，不是序列里最后那条 assistant 消息", message)
			}
			if got := message.Role(); got != schema.RoleAssistant {
				t.Fatalf("Role() = %q，想要 %q", got, schema.RoleAssistant)
			}
		})
	}
}

// TestRunOutputTextOnNonTextContent 钉住 Text() 的退化：最终 assistant 消息的内容
// 不是纯文本时交回空串。纯文本那条必须拿得到正文，否则"所有情况都空"也能让这个
// 断言成立。
func TestRunOutputTextOnNonTextContent(t *testing.T) {
	cases := []struct {
		name     string
		messages schema.Messages
		want     string
	}{
		{
			name: "没有模型消息",
			messages: schema.Messages{
				&schema.UserMessage{Content: schema.ContentBlocks{schema.TextBlock("客户")}},
			},
			want: "",
		},
		{
			name: "纯文本最终消息",
			messages: schema.Messages{
				&schema.AssistantMessage{Content: schema.ContentBlocks{schema.TextBlock("最终答复")}},
			},
			want: "最终答复",
		},
		{
			name: "最终消息只有图片块",
			// 联合类型的校验拒绝 assistant 带图，这里只能直接构造，钉住 Text() 遇到
			// 非纯文本内容时的退化路径。
			messages: schema.Messages{
				&schema.AssistantMessage{Content: schema.ContentBlocks{
					schema.ImageBlock(characterizationImageURL),
				}},
			},
			want: "",
		},
		{
			name: "最终消息是纯文本加图片块",
			messages: schema.Messages{
				&schema.AssistantMessage{Content: schema.ContentBlocks{
					schema.TextBlock("最终答复"),
					schema.ImageBlock(characterizationImageURL),
				}},
			},
			want: "",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			output := &RunOutput{message: testCase.messages}

			if got := output.Text(); got != testCase.want {
				t.Fatalf("Text() = %q，想要 %q", got, testCase.want)
			}
		})
	}
}
