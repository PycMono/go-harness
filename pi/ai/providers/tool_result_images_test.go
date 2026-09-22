package providers

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/PycMono/go-harness/pi/schema"
	openaisdk "github.com/openai/openai-go/v3"
)

const (
	// toolResultImageNoticeText 是 pi.dev 的锚点文本字面量（openai-completions.ts:1453），
	// 独立于生产代码里的同名常量写死，才能真正钉住它。
	toolResultImageNoticeText = "Attached image(s) from tool result:"
	toolResultImageURL1       = "https://example.test/first.png"
	toolResultImageURL2       = "https://example.test/second.png"
	toolResultImageCallIDA    = "call-image-a"
	toolResultImageCallIDB    = "call-image-b"
	toolResultImageToolA      = "read_file"
	toolResultImageToolB      = "screenshot"
	toolResultImageTextA      = "第一张扫描结果"
	toolResultImageTextB      = "第二张扫描结果"
	toolResultImageTextOnly   = "纯文本工具结果"
	toolResultImageArgs       = `{"path":"a.go"}`
)

// toolResultImageAssistant 构造一条发起工具调用的助手消息，作为工具结果运行的前缀。
func toolResultImageAssistant(toolCallID, toolName string) *schema.AssistantMessage {
	return &schema.AssistantMessage{
		ToolCalls: schema.ToolCalls{{
			ID: toolCallID, Name: toolName, Arguments: json.RawMessage(toolResultImageArgs),
		}},
	}
}

// TestInsertToolResultImages 钉住 OpenAI 工具结果图片的补发契约，形状照搬 pi.dev 的
// openai-completions.ts:1396-1458：
//
//   - 运行是 msgs 里极大的一段连续 ToolResultMessage，图片跨整段收集，所以两条相邻
//     的带图工具结果只补一条 user 消息；
//   - 合成 user 消息插在这段运行最后一条工具结果之后，顺序是 assistant、tool、tool、user；
//   - 合成消息的内容是固定锚点文本加每张图一个 image_url，URL 按"消息顺序 → 块顺序"；
//   - 开关为假、运行没收到图、或运行里根本没有图时，一条都不补；
//   - 不补 pi.dev 那条可选的 "I have processed the tool results." 助手消息
//     （它由 compat.requiresAssistantAfterToolResult 控制，go-harness 没有这个兼容位）。
//
// 每个用例同时钉住工具消息自己的文本：开关关掉时图片靠任务三的占位文本兜底，
// 不能既没合成消息又没占位，变成静默丢图。
func TestInsertToolResultImages(t *testing.T) {
	imageOnlyB := schema.ContentBlocks{
		schema.TextBlock(toolResultImageTextB), schema.ImageBlock(toolResultImageURL2),
	}
	dirtyImage := schema.ContentBlocks{{Type: schema.ContentTypeImage}}

	cases := []struct {
		name     string
		messages schema.Messages
		// params 为 nil 时用 messages.ToOpenAIMessages() 的结果，也就是 Provider 真正
		// 递给本函数的那份参数；非 nil 用于构造 ToOpenAIMessages 会拒绝、但本函数
		// 仍必须安全处理的脏输入（image 块缺 Image）。
		params     []openaisdk.ChatCompletionMessageParamUnion
		supports   bool
		wantKinds  []string
		wantImages [][]string
		wantTools  []string
	}{
		{
			name: "单条带图工具结果在助手消息之后补一条 user",
			messages: schema.Messages{
				toolResultImageAssistant(toolResultImageCallIDA, toolResultImageToolA),
				&schema.ToolResultMessage{
					Content: schema.ContentBlocks{
						schema.TextBlock(toolResultImageTextA), schema.ImageBlock(toolResultImageURL1),
					},
					ToolCallID: toolResultImageCallIDA, ToolName: toolResultImageToolA,
				},
			},
			supports:   true,
			wantKinds:  []string{"OfAssistant", "OfTool", "OfUser"},
			wantImages: [][]string{{toolResultImageURL1}},
			wantTools:  []string{toolResultImageTextA},
		},
		{
			name: "两条连续带图工具结果合成一条 user 并按序带两张图",
			messages: schema.Messages{
				toolResultImageAssistant(toolResultImageCallIDA, toolResultImageToolA),
				&schema.ToolResultMessage{
					Content:    schema.ContentBlocks{schema.ImageBlock(toolResultImageURL1)},
					ToolCallID: toolResultImageCallIDA, ToolName: toolResultImageToolA,
				},
				&schema.ToolResultMessage{
					Content:    imageOnlyB,
					ToolCallID: toolResultImageCallIDB, ToolName: toolResultImageToolB,
				},
			},
			supports:   true,
			wantKinds:  []string{"OfAssistant", "OfTool", "OfTool", "OfUser"},
			wantImages: [][]string{{toolResultImageURL1, toolResultImageURL2}},
			wantTools:  []string{"(see attached image)", toolResultImageTextB},
		},
		{
			name: "被助手消息隔开的两次运行各自补一条 user",
			messages: schema.Messages{
				toolResultImageAssistant(toolResultImageCallIDA, toolResultImageToolA),
				&schema.ToolResultMessage{
					Content:    schema.ContentBlocks{schema.ImageBlock(toolResultImageURL1)},
					ToolCallID: toolResultImageCallIDA, ToolName: toolResultImageToolA,
				},
				toolResultImageAssistant(toolResultImageCallIDB, toolResultImageToolB),
				&schema.ToolResultMessage{
					Content:    schema.ContentBlocks{schema.ImageBlock(toolResultImageURL2)},
					ToolCallID: toolResultImageCallIDB, ToolName: toolResultImageToolB,
				},
			},
			supports:   true,
			wantKinds:  []string{"OfAssistant", "OfTool", "OfUser", "OfAssistant", "OfTool", "OfUser"},
			wantImages: [][]string{{toolResultImageURL1}, {toolResultImageURL2}},
			wantTools:  []string{"(see attached image)", "(see attached image)"},
		},
		{
			name: "开关为假时不补 user，工具消息仍带着占位文本",
			messages: schema.Messages{
				toolResultImageAssistant(toolResultImageCallIDA, toolResultImageToolA),
				&schema.ToolResultMessage{
					Content:    schema.ContentBlocks{schema.ImageBlock(toolResultImageURL1)},
					ToolCallID: toolResultImageCallIDA, ToolName: toolResultImageToolA,
				},
			},
			supports:  false,
			wantKinds: []string{"OfAssistant", "OfTool"},
			wantTools: []string{"(see attached image)"},
		},
		{
			name: "纯文本工具结果即使开关为真也不补 user",
			messages: schema.Messages{
				toolResultImageAssistant(toolResultImageCallIDA, toolResultImageToolA),
				&schema.ToolResultMessage{
					Content:    schema.ContentBlocks{schema.TextBlock(toolResultImageTextOnly)},
					ToolCallID: toolResultImageCallIDA, ToolName: toolResultImageToolA,
				},
			},
			supports:  true,
			wantKinds: []string{"OfAssistant", "OfTool"},
			wantTools: []string{toolResultImageTextOnly},
		},
		{
			name: "图片块缺 Image 时收不到图，不补 user 也不 panic",
			messages: schema.Messages{
				&schema.ToolResultMessage{
					Content:    dirtyImage,
					ToolCallID: toolResultImageCallIDA, ToolName: toolResultImageToolA,
				},
			},
			params: []openaisdk.ChatCompletionMessageParamUnion{
				openaisdk.ToolMessage("(no tool output)", toolResultImageCallIDA),
			},
			supports:  true,
			wantKinds: []string{"OfTool"},
			wantTools: []string{"(no tool output)"},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			params := testCase.params
			if params == nil {
				converted, err := testCase.messages.ToOpenAIMessages()
				if err != nil {
					t.Fatalf("ToOpenAIMessages: %v", err)
				}
				params = converted
			}

			got := insertToolResultImages(testCase.messages, params, testCase.supports)

			kinds := make([]string, 0, len(got))
			for _, param := range got {
				kinds = append(kinds, openAIParamVariant(param))
			}
			if !reflect.DeepEqual(kinds, testCase.wantKinds) {
				t.Fatalf("消息联合分支序列 = %v，想要 %v", kinds, testCase.wantKinds)
			}

			var userImages [][]string
			var toolTexts []string
			for _, param := range got {
				switch {
				case param.OfUser != nil:
					urls, anchor := openAIUserImages(t, param)
					if anchor != toolResultImageNoticeText {
						t.Fatalf("合成 user 消息的首段文本 = %q，想要 %q", anchor, toolResultImageNoticeText)
					}
					userImages = append(userImages, urls)
				case param.OfTool != nil:
					toolTexts = append(toolTexts, param.OfTool.Content.OfString.Value)
				}
			}
			if !reflect.DeepEqual(userImages, testCase.wantImages) {
				t.Fatalf("合成 user 消息的图片 URL = %v，想要 %v", userImages, testCase.wantImages)
			}
			if !reflect.DeepEqual(toolTexts, testCase.wantTools) {
				t.Fatalf("工具消息文本 = %v，想要 %v", toolTexts, testCase.wantTools)
			}
		})
	}
}

// TestSupportsImageInputReachesOpenAIProvider 钉住能力开关的两段路：Options 直接从配置
// JSON 解出来（缺省 false），并被 NewOpenAI 读进 provider 的字段。开关只有真的走到
// Stream 才有意义，解出来却读不到等于没有。
func TestSupportsImageInputReachesOpenAIProvider(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{
			name: "缺省为假",
			raw:  `{"id":"platform","protocol":"openai","baseURL":"https://example.test/v1","apiKey":"key","model":"model"}`,
		},
		{
			name: "配置为真",
			raw:  `{"id":"platform","protocol":"openai","baseURL":"https://example.test/v1","apiKey":"key","model":"model","supportsImageInput":true}`,
			want: true,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var opts Options
			if err := json.Unmarshal([]byte(testCase.raw), &opts); err != nil {
				t.Fatalf("解析配置: %v", err)
			}
			if opts.SupportsImageInput != testCase.want {
				t.Fatalf("Options.SupportsImageInput = %v，想要 %v",
					opts.SupportsImageInput, testCase.want)
			}

			provider, ok := NewOpenAI(&opts).(*OpenAIImpl)
			if !ok {
				t.Fatal("NewOpenAI 没有返回 *OpenAIImpl")
			}
			if provider.supportsImageInput != testCase.want {
				t.Fatalf("OpenAIImpl.supportsImageInput = %v，想要 %v",
					provider.supportsImageInput, testCase.want)
			}
		})
	}
}

// TestInsertToolResultImagesMatchesInsertionByToolCallID 钉住插入点是按 tool_call_id
// 匹配出来的，不是按消息下标算出来的：ToOpenAIMessages 目前对四种角色都恰好一条参数
// 换一条消息，可这个对齐没有任何东西保证，一破就是静默的错序错图。这里故意让参数比
// 消息多一条，下标一旦错位，合成消息就会插到工具消息前面去。
func TestInsertToolResultImagesMatchesInsertionByToolCallID(t *testing.T) {
	messages := schema.Messages{
		toolResultImageAssistant(toolResultImageCallIDA, toolResultImageToolA),
		&schema.ToolResultMessage{
			Content:    schema.ContentBlocks{schema.ImageBlock(toolResultImageURL1)},
			ToolCallID: toolResultImageCallIDA, ToolName: toolResultImageToolA,
		},
	}
	converted, err := messages.ToOpenAIMessages()
	if err != nil {
		t.Fatalf("ToOpenAIMessages: %v", err)
	}

	// 在助手消息与工具消息之间塞一条与 msgs 不对应的参数，打破下标对齐。
	params := []openaisdk.ChatCompletionMessageParamUnion{
		converted[0],
		{OfAssistant: &openaisdk.ChatCompletionAssistantMessageParam{
			Content: openaisdk.ChatCompletionAssistantMessageParamContentUnion{
				OfString: openaisdk.String("旁白"),
			},
		}},
		converted[1],
	}

	got := insertToolResultImages(messages, params, true)

	kinds := make([]string, 0, len(got))
	for _, param := range got {
		kinds = append(kinds, openAIParamVariant(param))
	}
	want := []string{"OfAssistant", "OfAssistant", "OfTool", "OfUser"}
	if !reflect.DeepEqual(kinds, want) {
		t.Fatalf("消息联合分支序列 = %v，想要 %v", kinds, want)
	}
	if got[2].OfTool.ToolCallID != toolResultImageCallIDA {
		t.Fatalf("工具消息的 tool_call_id = %q，想要 %q",
			got[2].OfTool.ToolCallID, toolResultImageCallIDA)
	}
	urls, anchor := openAIUserImages(t, got[3])
	if anchor != toolResultImageNoticeText {
		t.Fatalf("合成 user 消息的首段文本 = %q，想要 %q", anchor, toolResultImageNoticeText)
	}
	if !reflect.DeepEqual(urls, []string{toolResultImageURL1}) {
		t.Fatalf("合成 user 消息的图片 URL = %v，想要 %v", urls, []string{toolResultImageURL1})
	}
}

// TestInsertToolResultImagesIsNoopWithoutRuns 钉住空输入与无运行输入：没有工具结果时
// 参数原样返回，不会因为多出一条 user 消息而改变空对话的形状。
func TestInsertToolResultImagesIsNoopWithoutRuns(t *testing.T) {
	messages := schema.Messages{
		&schema.UserMessage{Content: schema.ContentBlocks{schema.TextBlock(toolResultImageTextOnly)}},
	}
	params, err := messages.ToOpenAIMessages()
	if err != nil {
		t.Fatalf("ToOpenAIMessages: %v", err)
	}

	got := insertToolResultImages(messages, params, true)
	if len(got) != len(params) {
		t.Fatalf("参数条数 = %d，想要 %d", len(got), len(params))
	}
	if openAIParamVariant(got[0]) != "OfUser" {
		t.Fatalf("唯一一条参数的联合分支 = %q，想要 %q", openAIParamVariant(got[0]), "OfUser")
	}
	if got[0].OfUser.Content.OfString.Value != toolResultImageTextOnly {
		t.Fatalf("用户消息文本 = %q，想要 %q",
			got[0].OfUser.Content.OfString.Value, toolResultImageTextOnly)
	}
}

// openAIParamVariant 给出一条参数的联合分支名，断言"哪条消息落在哪个分支上"。
func openAIParamVariant(param openaisdk.ChatCompletionMessageParamUnion) string {
	switch {
	case param.OfSystem != nil:
		return "OfSystem"
	case param.OfUser != nil:
		return "OfUser"
	case param.OfAssistant != nil:
		return "OfAssistant"
	case param.OfTool != nil:
		return "OfTool"
	default:
		return "未知"
	}
}

// openAIUserImages 取出 user 消息首段文本之后的图片 URL，并按序校验每个成员的类型：
// 合成消息只由文本锚点与 image_url 组成，出现别的成员就是形状错了。
func openAIUserImages(t *testing.T, param openaisdk.ChatCompletionMessageParamUnion) ([]string, string) {
	t.Helper()

	parts := param.OfUser.Content.OfArrayOfContentParts
	if len(parts) == 0 {
		t.Fatal("合成 user 消息没有内容成员")
	}
	if parts[0].OfText == nil {
		t.Fatal("合成 user 消息的首个成员不是文本")
	}

	urls := make([]string, 0, len(parts)-1)
	for index, part := range parts[1:] {
		if part.OfImageURL == nil {
			t.Fatalf("第 %d 个内容成员不是 image_url", index+1)
		}
		urls = append(urls, part.OfImageURL.ImageURL.URL)
	}
	return urls, parts[0].OfText.Text
}
