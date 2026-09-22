package pi

import (
	"strings"
	"testing"

	pierrors "github.com/PycMono/go-harness/pi/error"
	"github.com/PycMono/go-harness/pi/schema"
)

// 本文件是 Step 0 的取值基线：把 Message2AI 当下的校验规则、命中顺序与产物逐条
// 钉住，供 schema.Message 变成四 variant 联合类型的重构对照。断言描述的是"现在是
// 什么"，不是"应该是什么"。

// characterizationImageURL 是走得到校验通过的那张图。
const characterizationImageURL = "https://example.test/cat.png"

// TestMessage2AISuccessCases 覆盖校验通过时的产物：角色、内容块顺序与内容原样保留。
func TestMessage2AISuccessCases(t *testing.T) {
	cases := []struct {
		name    string
		message Message
		want    schema.Message
	}{
		{
			name: "customer 纯文本",
			// 前后空格只参与"是不是空白"的判定，不参与清洗：正文原样进内容块。
			message: Message{ContentType: "text", SenderType: "customer", Content: "  基线用户输入  "},
			want: &schema.UserMessage{
				Content: schema.ContentBlocks{schema.TextBlock("  基线用户输入  ")},
			},
		},
		{
			name:    "ai 纯文本",
			message: Message{ContentType: "text", SenderType: "ai", Content: "基线模型输出"},
			want: &schema.AssistantMessage{
				Content: schema.ContentBlocks{schema.TextBlock("基线模型输出")},
			},
		},
		{
			name: "customer 文本 + 一张图",
			message: Message{
				ContentType: "text", SenderType: "customer", Content: "看图",
				ImageURLs: []string{characterizationImageURL},
			},
			want: &schema.UserMessage{
				Content: schema.ContentBlocks{
					schema.TextBlock("看图"),
					schema.ImageBlock(characterizationImageURL),
				},
			},
		},
		{
			name: "customer 文本 + 四张图（上限）",
			message: Message{
				ContentType: "text", SenderType: "customer", Content: "四张图",
				ImageURLs: []string{
					"https://example.test/1.png",
					"https://example.test/2.png",
					"https://example.test/3.png",
					"https://example.test/4.png",
				},
			},
			want: &schema.UserMessage{
				Content: schema.ContentBlocks{
					schema.TextBlock("四张图"),
					schema.ImageBlock("https://example.test/1.png"),
					schema.ImageBlock("https://example.test/2.png"),
					schema.ImageBlock("https://example.test/3.png"),
					schema.ImageBlock("https://example.test/4.png"),
				},
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got, err := testCase.message.Message2AI()
			if err != nil {
				t.Fatalf("Message2AI: %v", err)
			}
			if got.Role() != testCase.want.Role() {
				t.Fatalf("role = %q，想要 %q", got.Role(), testCase.want.Role())
			}
			// 文本块在前、图片块按 ImageURLs 的顺序追加在后。
			assertContentBlocks(t, contentOf(t, got), contentOf(t, testCase.want))
			// Message2AI 只产出 role + content，不碰其他载荷。
			usage, toolCalls := assistantPayloadOf(got)
			if usage != nil {
				t.Fatalf("Message2AI 填了 Usage: %+v", usage)
			}
			if len(toolCalls) != 0 {
				t.Fatalf("Message2AI 填了 ToolCalls: %+v", toolCalls)
			}
		})
	}
}

// TestMessage2AIFailureCases 覆盖每一条校验规则：每条失败都必须挂 ErrRequestInvalid。
// 用例构造得让"能命中的规则只有一条"，所以只断言错误码就足以区分是哪条规则拦下的。
func TestMessage2AIFailureCases(t *testing.T) {
	tooManyImages := []string{
		"https://example.test/1.png",
		"https://example.test/2.png",
		"https://example.test/3.png",
		"https://example.test/4.png",
		"https://example.test/5.png",
	}

	cases := []struct {
		name    string
		message Message
	}{
		{
			name:    "content type 不是 text",
			message: Message{ContentType: "markdown", SenderType: "customer", Content: "正文"},
		},
		{
			name:    "content type 为空",
			message: Message{ContentType: "", SenderType: "customer", Content: "正文"},
		},
		{
			name:    "content 为空串",
			message: Message{ContentType: "text", SenderType: "customer", Content: ""},
		},
		{
			name:    "content 全是空白",
			message: Message{ContentType: "text", SenderType: "customer", Content: " \t\n "},
		},
		{
			name:    "sender type 未知",
			message: Message{ContentType: "text", SenderType: "system", Content: "正文"},
		},
		{
			name:    "sender type 为空",
			message: Message{ContentType: "text", Content: "正文"},
		},
		{
			name: "ai 消息带图片",
			message: Message{
				ContentType: "text", SenderType: "ai", Content: "正文",
				ImageURLs: []string{characterizationImageURL},
			},
		},
		{
			name: "图片数超过上限",
			message: Message{
				ContentType: "text", SenderType: "customer", Content: "正文",
				ImageURLs: tooManyImages,
			},
		},
		{
			name: "图片 URL 不是 http/https",
			message: Message{
				ContentType: "text", SenderType: "customer", Content: "正文",
				ImageURLs: []string{"ftp://example.test/cat.png"},
			},
		},
		{
			name: "图片 URL 缺 host",
			message: Message{
				ContentType: "text", SenderType: "customer", Content: "正文",
				ImageURLs: []string{"https://"},
			},
		},
		{
			name: "图片 URL 缺 scheme",
			message: Message{
				ContentType: "text", SenderType: "customer", Content: "正文",
				ImageURLs: []string{"example.test/cat.png"},
			},
		},
		{
			name: "图片 URL 解析失败",
			message: Message{
				ContentType: "text", SenderType: "customer", Content: "正文",
				ImageURLs: []string{"://missing-scheme"},
			},
		},
		{
			name: "第二张图非法也拦得住",
			message: Message{
				ContentType: "text", SenderType: "customer", Content: "正文",
				ImageURLs: []string{characterizationImageURL, "ftp://example.test/2.png"},
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got, err := testCase.message.Message2AI()
			requireRequestInvalid(t, err)
			if got != nil {
				t.Fatalf("校验失败时仍返回了消息: %+v", got)
			}
		})
	}
}

// TestMessage2AIValidationOrder 钉住规则之间的先后顺序：多条规则同时不满足时，
// 现在命中的是哪一条。错误码都一样，所以这里靠原因文案区分——这正是这条顺序的
// 可观测形式。
func TestMessage2AIValidationOrder(t *testing.T) {
	badURLs := []string{
		"ftp://example.test/1.png",
		"ftp://example.test/2.png",
		"ftp://example.test/3.png",
		"ftp://example.test/4.png",
		"ftp://example.test/5.png",
	}

	cases := []struct {
		name      string
		message   Message
		wantCause string
	}{
		{
			name:      "content type 先于 content 是否为空",
			message:   Message{ContentType: "markdown", SenderType: "customer", Content: ""},
			wantCause: "content type",
		},
		{
			name:      "content 先于 sender type",
			message:   Message{ContentType: "text", SenderType: "system", Content: "  "},
			wantCause: "must not be empty",
		},
		{
			name: "sender type 先于图片规则",
			message: Message{
				ContentType: "text", SenderType: "system", Content: "正文",
				ImageURLs: []string{"ftp://example.test/cat.png"},
			},
			wantCause: "sender type",
		},
		{
			name: "只允许 customer 带图 先于 图片数量上限",
			message: Message{
				ContentType: "text", SenderType: "ai", Content: "正文",
				ImageURLs: badURLs,
			},
			wantCause: "may attach images",
		},
		{
			name: "图片数量上限 先于 图片 URL 校验",
			message: Message{
				ContentType: "text", SenderType: "customer", Content: "正文",
				ImageURLs: badURLs,
			},
			wantCause: "at most",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got, err := testCase.message.Message2AI()
			requireRequestInvalid(t, err)
			if !strings.Contains(err.Error(), testCase.wantCause) {
				t.Fatalf("错误 %q 不含 %q，命中的不是想要的那条规则", err.Error(), testCase.wantCause)
			}
			if got != nil {
				t.Fatalf("校验失败时仍返回了消息: %+v", got)
			}
		})
	}
}

// contentOf 取联合消息的内容块。内容块挂在具体类型上，role 之外没有公共读取口，
// 只能断言具体类型再读；Message2AI 只产出 user 与 assistant 两种。
func contentOf(t *testing.T, message schema.Message) schema.ContentBlocks {
	t.Helper()

	switch typed := message.(type) {
	case *schema.UserMessage:
		return typed.Content
	case *schema.AssistantMessage:
		return typed.Content
	default:
		t.Fatalf("消息的动态类型 = %T，不是 *schema.UserMessage 或 *schema.AssistantMessage", message)
		return nil
	}
}

// assistantPayloadOf 取 assistant 消息的用量与工具调用；其余角色没有这两样载荷，
// 按"没填"返回零值。
func assistantPayloadOf(message schema.Message) (*schema.Usage, schema.ToolCalls) {
	assistant, ok := message.(*schema.AssistantMessage)
	if !ok {
		return nil, nil
	}

	return assistant.Usage, assistant.ToolCalls
}

// requireRequestInvalid 断言 err 挂了 ErrRequestInvalid 这个稳定码。
func requireRequestInvalid(t *testing.T, err error) {
	t.Helper()

	if err == nil {
		t.Fatalf("想要 ErrRequestInvalid，实际 nil")
	}
	if !pierrors.ErrRequestInvalid.Match(err) {
		t.Fatalf("错误码 = %d，想要 %d（err = %v）",
			pierrors.CodeOf(err), pierrors.ErrRequestInvalid.Code(), err)
	}
}

// assertContentBlocks 逐块比对内容块：类型、文本、图片 URL 都要一致。
func assertContentBlocks(t *testing.T, got, want schema.ContentBlocks) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("内容块数 = %d，想要 %d（got = %+v）", len(got), len(want), got)
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
