package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/PycMono/go-harness/pi/schema"
)

// 金标准 fixture 的落点与身份。它由**重构前**的编码器一次性写出（生成器跑完即删），
// 所以这些字节不可能被重构后的代码重新生成——断言红了先怀疑代码，不要动 fixture。
//
// 放在 pi/session/testdata 而不是仓库根的 /testdata：后者在 .gitignore 里（/testdata/）。
//
// fixture 里没有"带图片的工具结果"这种形状：当前 ContentBlocks.ValidateForRole
// （protocol.go:297）只允许 user 角色携带图片块，旧编码器造不出来。
const (
	goldenWorkDir   = "/workspace/golden-baseline"
	goldenSessionID = "chat-golden"
	goldenFixture   = "testdata/--workspace-golden-baseline--/chat-golden.jsonl"

	goldenImageURL   = "https://example.test/golden.png"
	goldenToolCallID = "call_golden_1"
	goldenToolName   = "read_file"

	goldenUserText       = "first turn: plain text question"
	goldenImageText      = "second turn: question with an image"
	goldenReplyText      = "assistant reply to the first turn"
	goldenToolAskText    = "assistant asks for the read_file tool"
	goldenToolResultText = "tool result: README.md contents"
)

// TestGoldenFixtureWireFormat 是线格式钉：按原始行读 fixture，断言 entry 与 message
// 的键集、role 取值与内容块形状。这一半在重构后必须原样通过——它描述的是盘上字节
// 的兼容契约（旧 JSONL 无需迁移即可读取）。
func TestGoldenFixtureWireFormat(t *testing.T) {
	lines := readGoldenFixtureLines(t)
	if len(lines) != 6 {
		t.Fatalf("fixture 行数 = %d，想要 6（1 行 session header + 5 条消息）", len(lines))
	}

	entries := make([]map[string]any, 0, len(lines))
	for index, line := range lines {
		entries = append(entries, decodeGoldenObject(t, line, fmt.Sprintf("第 %d 行", index+1)))
	}

	t.Run("entry 层级", func(t *testing.T) {
		// header 行没有 parent_id（首行没有父），message 行的 parent_id 必然在。
		assertGoldenKeys(t, "header entry", entries[0], "type", "id", "timestamp", "header")
		if got := entries[0]["type"]; got != "session" {
			t.Fatalf("header entry type = %v，想要 %q", got, "session")
		}
		for index, entry := range entries[1:] {
			label := fmt.Sprintf("message entry %d", index+2)
			assertGoldenKeys(t, label, entry, "type", "id", "parent_id", "timestamp", "message")
			if got := entry["type"]; got != "message" {
				t.Fatalf("%s type = %v，想要 %q", label, got, "message")
			}
			if got, ok := entry["parent_id"].(string); !ok || got == "" {
				t.Fatalf("%s parent_id = %#v，想要非空字符串", label, entry["parent_id"])
			}
		}
	})

	t.Run("header 层级", func(t *testing.T) {
		header, ok := entries[0]["header"].(map[string]any)
		if !ok {
			t.Fatalf("header entry 的 header 载荷不是对象: %#v", entries[0]["header"])
		}
		assertGoldenKeys(t, "header", header, "id", "version", "created_at", "work_dir")
		if header["id"] != goldenSessionID {
			t.Fatalf("header.id = %v，想要 %q", header["id"], goldenSessionID)
		}
		if header["work_dir"] != goldenWorkDir {
			t.Fatalf("header.work_dir = %v，想要 %q", header["work_dir"], goldenWorkDir)
		}
		// version 是 float64：走 map[string]any 解出来的都是 JSON number。
		if version, ok := header["version"].(float64); !ok || version != 1 {
			t.Fatalf("header.version = %#v，想要 1", header["version"])
		}
	})

	t.Run("message 键集与 role", func(t *testing.T) {
		wantKeys := [][]string{
			{"content", "role"},               // 1 user：纯文本
			{"content", "role"},               // 2 user：文本 + 图片
			{"content", "role"},               // 3 assistant：纯文本
			{"content", "role", "tool_calls"}, // 4 assistant：文本 + 工具调用
			{"content", "role", "tool_call_id", "tool_name", "is_error"}, // 5 tool：结果
		}
		wantRoles := []string{"user", "user", "assistant", "assistant", "tool"}

		for index, want := range wantKeys {
			number := index + 1
			message := goldenMessageOf(t, entries[number], number)
			assertGoldenKeys(t, fmt.Sprintf("message %d", number), message, want...)
			if message["role"] != wantRoles[index] {
				t.Fatalf("message %d role = %v，想要 %q", number, message["role"], wantRoles[index])
			}
		}
	})

	t.Run("内容块与工具调用的 JSON 形状", func(t *testing.T) {
		// 1：纯文本块。
		blocks := goldenBlocksOf(t, entries[1], 1)
		if len(blocks) != 1 {
			t.Fatalf("message 1 内容块数 = %d，想要 1", len(blocks))
		}
		textBlock := goldenBlockOf(t, blocks[0], 1)
		assertGoldenKeys(t, "message 1 block 1", textBlock, "type", "text")
		if textBlock["type"] != "text" || textBlock["text"] != goldenUserText {
			t.Fatalf("message 1 block 1 = %#v，想要 text=%q", textBlock, goldenUserText)
		}

		// 2：文本块 + 图片块。图片块的形状就是兼容契约。
		blocks = goldenBlocksOf(t, entries[2], 2)
		if len(blocks) != 2 {
			t.Fatalf("message 2 内容块数 = %d，想要 2", len(blocks))
		}
		textBlock = goldenBlockOf(t, blocks[0], 2)
		if textBlock["type"] != "text" || textBlock["text"] != goldenImageText {
			t.Fatalf("message 2 block 1 = %#v，想要 text=%q", textBlock, goldenImageText)
		}
		imageBlock := goldenBlockOf(t, blocks[1], 2)
		assertGoldenKeys(t, "message 2 block 2", imageBlock, "type", "image")
		if imageBlock["type"] != "image" {
			t.Fatalf("message 2 block 2 类型 = %v，想要 %q", imageBlock["type"], "image")
		}
		image, ok := imageBlock["image"].(map[string]any)
		if !ok {
			t.Fatalf("image 块载荷不是对象: %#v", imageBlock["image"])
		}
		assertGoldenKeys(t, "image", image, "url")
		if image["url"] != goldenImageURL {
			t.Fatalf("image.url = %v，想要 %q", image["url"], goldenImageURL)
		}

		// 3：assistant 纯文本，与 message 1 同形。
		blocks = goldenBlocksOf(t, entries[3], 3)
		if len(blocks) != 1 {
			t.Fatalf("message 3 内容块数 = %d，想要 1", len(blocks))
		}
		if textBlock = goldenBlockOf(t, blocks[0], 3); textBlock["text"] != goldenReplyText {
			t.Fatalf("message 3 block 1 = %#v，想要 text=%q", textBlock, goldenReplyText)
		}

		// 4：assistant 的 tool_calls。
		message := goldenMessageOf(t, entries[4], 4)
		calls, ok := message["tool_calls"].([]any)
		if !ok || len(calls) != 1 {
			t.Fatalf("message 4 tool_calls = %#v，想要 1 条", message["tool_calls"])
		}
		call := goldenBlockOf(t, calls[0], 4)
		assertGoldenKeys(t, "tool_call", call, "id", "name", "arguments")
		if call["id"] != goldenToolCallID {
			t.Fatalf("tool_call.id = %v，想要 %q", call["id"], goldenToolCallID)
		}
		if call["name"] != goldenToolName {
			t.Fatalf("tool_call.name = %v，想要 %q", call["name"], goldenToolName)
		}
		// 参数在 JSONL 里是 JSON 对象而不是字符串：RawMessage 原样落盘。
		arguments, ok := call["arguments"].(map[string]any)
		if !ok {
			t.Fatalf("tool_call.arguments 不是 JSON 对象: %#v", call["arguments"])
		}
		if !reflect.DeepEqual(arguments, map[string]any{"path": "README.md"}) {
			t.Fatalf("tool_call.arguments = %#v，想要 %#v", arguments, map[string]any{"path": "README.md"})
		}

		// 5：tool 结果的三个标量字段。
		message = goldenMessageOf(t, entries[5], 5)
		if message["tool_call_id"] != goldenToolCallID {
			t.Fatalf("tool_call_id = %v，想要 %q", message["tool_call_id"], goldenToolCallID)
		}
		if message["tool_name"] != goldenToolName {
			t.Fatalf("tool_name = %v，想要 %q", message["tool_name"], goldenToolName)
		}
		if message["is_error"] != true {
			t.Fatalf("is_error = %v，想要 true", message["is_error"])
		}
	})
}

// TestGoldenFixtureRoundTrip 是回读钉：走公开的 session API 打开 fixture，断言重建
// 出的历史与写进去时一致。重构后字段访问会变成 accessor 调用，这一半预期要跟着改
// 一次——所以它刻意写小。
func TestGoldenFixtureRoundTrip(t *testing.T) {
	if _, err := os.Stat(goldenFixture); err != nil {
		t.Fatalf("金标准 fixture 不在盘上（先确认它没被 .gitignore 吃掉）: %v", err)
	}

	manager, err := OpenOrCreate("testdata", goldenWorkDir, goldenSessionID)
	if err != nil {
		t.Fatalf("OpenOrCreate: %v", err)
	}
	// 打开的是 fixture 本身，而不是被凭空新建出来的同名会话——新建的话文件里只有
	// header，下面的条数断言会失败，但先把这件事说清楚。
	if want := filepath.FromSlash(goldenFixture); manager.Path() != want {
		t.Fatalf("打开的是 %q，想要 fixture %q", manager.Path(), want)
	}

	messages := manager.BuildMessages()
	if len(messages) != 5 {
		t.Fatalf("重建出的消息条数 = %d，想要 5", len(messages))
	}

	wantRoles := []schema.Role{
		schema.RoleUser, schema.RoleUser,
		schema.RoleAssistant, schema.RoleAssistant,
		schema.RoleTool,
	}
	for index, want := range wantRoles {
		if messages[index].Role() != want {
			t.Fatalf("messages[%d].Role = %q，想要 %q", index, messages[index].Role(), want)
		}
	}

	// 1：user 纯文本。
	if got := goldenTextOf(t, messages[0]); got != goldenUserText {
		t.Fatalf("messages[0] 文本 = %q，想要 %q", got, goldenUserText)
	}

	// 2：user 文本 + 图片。
	content := goldenContentOf(t, messages[1])
	if len(content) != 2 {
		t.Fatalf("messages[1] 内容块数 = %d，想要 2", len(content))
	}
	// 带图消息取不出"整条文本"（Content.Text 遇到非文本块就报错），只能逐块看。
	if content[0].Type != schema.ContentTypeText {
		t.Fatalf("messages[1] 第一块类型 = %q，想要 %q",
			content[0].Type, schema.ContentTypeText)
	}
	if got := content[0].Text; got != goldenImageText {
		t.Fatalf("messages[1] 文本 = %q，想要 %q", got, goldenImageText)
	}
	if content[1].Type != schema.ContentTypeImage {
		t.Fatalf("messages[1] 第二块类型 = %q，想要 %q",
			content[1].Type, schema.ContentTypeImage)
	}
	if content[1].Image == nil || content[1].Image.URL != goldenImageURL {
		t.Fatalf("messages[1] 图片块 = %+v，想要 URL %q", content[1].Image, goldenImageURL)
	}

	// 3：assistant 纯文本。
	if got := goldenTextOf(t, messages[2]); got != goldenReplyText {
		t.Fatalf("messages[2] 文本 = %q，想要 %q", got, goldenReplyText)
	}

	// 4：assistant 文本 + 工具调用。
	if got := goldenTextOf(t, messages[3]); got != goldenToolAskText {
		t.Fatalf("messages[3] 文本 = %q，想要 %q", got, goldenToolAskText)
	}
	toolCalls := goldenToolCallsOf(t, messages[3])
	if len(toolCalls) != 1 {
		t.Fatalf("messages[3] 工具调用数 = %d，想要 1", len(toolCalls))
	}
	call := toolCalls[0]
	if call.ID != goldenToolCallID || call.Name != goldenToolName {
		t.Fatalf("工具调用 = %+v，想要 id=%q name=%q", call, goldenToolCallID, goldenToolName)
	}
	var arguments map[string]any
	if err = json.Unmarshal(call.Arguments, &arguments); err != nil {
		t.Fatalf("工具调用参数不是合法 JSON: %v", err)
	}
	if !reflect.DeepEqual(arguments, map[string]any{"path": "README.md"}) {
		t.Fatalf("工具调用参数 = %#v，想要 %#v", arguments, map[string]any{"path": "README.md"})
	}

	// 5：tool 结果。
	if got := goldenTextOf(t, messages[4]); got != goldenToolResultText {
		t.Fatalf("messages[4] 文本 = %q，想要 %q", got, goldenToolResultText)
	}
	toolResult := goldenToolResultOf(t, messages[4])
	if toolResult.ToolCallID != goldenToolCallID {
		t.Fatalf("messages[4].ToolCallID = %q，想要 %q", toolResult.ToolCallID, goldenToolCallID)
	}
	if toolResult.ToolName != goldenToolName {
		t.Fatalf("messages[4].ToolName = %q，想要 %q", toolResult.ToolName, goldenToolName)
	}
	if !toolResult.IsError {
		t.Fatalf("messages[4].IsError = false，想要 true")
	}
}

// goldenTextOf 取出消息的纯文本内容。
func goldenTextOf(t *testing.T, message schema.Message) string {
	t.Helper()

	text, err := goldenContentOf(t, message).Text()
	if err != nil {
		t.Fatalf("消息 %q 的内容块取不出文本: %v", message.Role(), err)
	}

	return text
}

// goldenContentOf 取出消息的内容块。Content 是各具体类型的字段而不是接口方法，
// 读它只能按类型断言；本辅助函数定义在本文件里，供这一半的断言共用。
func goldenContentOf(t *testing.T, message schema.Message) schema.ContentBlocks {
	t.Helper()

	switch typed := message.(type) {
	case *schema.SystemMessage:
		return typed.Content
	case *schema.UserMessage:
		return typed.Content
	case *schema.AssistantMessage:
		return typed.Content
	case *schema.ToolResultMessage:
		return typed.Content
	default:
		t.Fatalf("消息 %T 没有内容块", message)

		return nil
	}
}

// goldenToolCallsOf 取出助理消息的工具调用。
func goldenToolCallsOf(t *testing.T, message schema.Message) schema.ToolCalls {
	t.Helper()

	assistant, ok := message.(*schema.AssistantMessage)
	if !ok {
		t.Fatalf("消息 %T 不是助理消息，没有工具调用", message)
	}

	return assistant.ToolCalls
}

// goldenToolResultOf 取出工具结果消息，读它的身份字段。
func goldenToolResultOf(t *testing.T, message schema.Message) *schema.ToolResultMessage {
	t.Helper()

	result, ok := message.(*schema.ToolResultMessage)
	if !ok {
		t.Fatalf("消息 %T 不是工具结果消息", message)
	}

	return result
}

// readGoldenFixtureLines 按行读 fixture，丢掉空行。
func readGoldenFixtureLines(t *testing.T) []string {
	t.Helper()

	data, err := os.ReadFile(goldenFixture)
	if err != nil {
		t.Fatalf("读取 fixture %s: %v", goldenFixture, err)
	}
	lines := make([]string, 0, 8)
	for _, line := range strings.Split(string(data), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}

	return lines
}

// decodeGoldenObject 把一行原始 JSON 解成键值对象。
func decodeGoldenObject(t *testing.T, line, label string) map[string]any {
	t.Helper()

	var object map[string]any
	if err := json.Unmarshal([]byte(line), &object); err != nil {
		t.Fatalf("%s 不是合法的 JSON 对象: %v", label, err)
	}

	return object
}

// goldenMessageOf 取出 entry 里的 message 对象。
func goldenMessageOf(t *testing.T, entry map[string]any, number int) map[string]any {
	t.Helper()

	message, ok := entry["message"].(map[string]any)
	if !ok {
		t.Fatalf("message %d 行没有 message 对象: %#v", number, entry)
	}

	return message
}

// goldenBlocksOf 取出 message 的 content 数组。
func goldenBlocksOf(t *testing.T, entry map[string]any, number int) []any {
	t.Helper()

	content := goldenMessageOf(t, entry, number)["content"]
	blocks, ok := content.([]any)
	if !ok {
		t.Fatalf("message %d 的 content 不是数组: %#v", number, content)
	}

	return blocks
}

// goldenBlockOf 断言一个数组元素是 JSON 对象，并返回它。
func goldenBlockOf(t *testing.T, value any, number int) map[string]any {
	t.Helper()

	object, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("message %d 的数组元素不是 JSON 对象: %#v", number, value)
	}

	return object
}

// assertGoldenKeys 断言对象的键集完全等于 want（与顺序无关）。
func assertGoldenKeys(t *testing.T, label string, object map[string]any, want ...string) {
	t.Helper()

	got := make([]string, 0, len(object))
	for key := range object {
		got = append(got, key)
	}
	sort.Strings(got)
	expected := append([]string(nil), want...)
	sort.Strings(expected)

	if !reflect.DeepEqual(got, expected) {
		t.Fatalf("%s 键集 = %v，想要 %v", label, got, expected)
	}
}
