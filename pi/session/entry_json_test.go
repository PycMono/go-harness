package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/PycMono/go-harness/pi/schema"
)

// 本文件钉住会话文件这一层的解码：旧 JSONL 的每条消息解成对应的具体类型、坏行策略、
// header 行不带 message 键，以及最强的一条——金标准 fixture 逐行解码再编码后与盘上
// 字节完全一致。

const (
	entryJSONWorkDir   = "/workspace/decode-variants"
	entryJSONSessionID = "chat-decode-variants"
	entryJSONImageURL  = "https://example.test/decode.png"
	entryJSONToolID    = "call-decode"
	entryJSONToolName  = "read_file"
	entryJSONTimestamp = "2026-09-22T03:41:51Z"
)

// entryJSONLine 是一条 message 行的 id、父 id 与 message 载荷（载荷按旧结构体的线格式
// 手写，不由本包编码）。载荷为空串表示这一行没有 message 键。
type entryJSONLine struct {
	id      string
	parent  string
	payload string
}

// writeEntryJSONFile 按行写一个会话文件：首行是 header，其余行按给定顺序串起来。
// 目录按 encodeWorkDir 的规则建，和 OpenOrCreate 找文件的方式一致。
func writeEntryJSONFile(t *testing.T, root, workDir, sessionID string, lines []entryJSONLine) string {
	t.Helper()

	header := fmt.Sprintf(
		`{"type":"session","id":%q,"timestamp":%q,`+
			`"header":{"id":%q,"version":1,"created_at":%q,"work_dir":%q}}`,
		sessionID, entryJSONTimestamp, sessionID, entryJSONTimestamp, workDir)

	encoded := make([]string, 0, len(lines)+1)
	encoded = append(encoded, header)
	for _, line := range lines {
		if line.payload == "" {
			encoded = append(encoded, fmt.Sprintf(
				`{"type":"message","id":%q,"parent_id":%q,"timestamp":%q}`,
				line.id, line.parent, entryJSONTimestamp))
			continue
		}
		encoded = append(encoded, fmt.Sprintf(
			`{"type":"message","id":%q,"parent_id":%q,"timestamp":%q,"message":%s}`,
			line.id, line.parent, entryJSONTimestamp, line.payload))
	}

	path := filepath.Join(root, encodeWorkDir(workDir), sessionID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("建会话目录: %v", err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(encoded, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("写会话文件: %v", err)
	}

	return path
}

// TestSessionLoadDecodesOldShapeMessages 旧形状的会话文件逐条解成具体类型，字段也读得
// 出来：这是"旧 JSONL 无需迁移即可读取"在文件这一层的落点。
func TestSessionLoadDecodesOldShapeMessages(t *testing.T) {
	root := t.TempDir()
	lines := []entryJSONLine{
		{id: "MSG00001", parent: entryJSONSessionID, payload: `{"role":"system","content":[{"type":"text","text":"system prompt"}]}`},
		{id: "MSG00002", parent: "MSG00001", payload: `{"role":"user","content":[{"type":"text","text":"hello"}]}`},
		{id: "MSG00003", parent: "MSG00002", payload: `{"role":"user","content":[{"type":"text","text":"look"},` +
			`{"type":"image","image":{"url":"` + entryJSONImageURL + `"}}]}`},
		{id: "MSG00004", parent: "MSG00003", payload: `{"role":"assistant","content":[{"type":"text","text":"reply"}],` +
			`"usage":{"input_tokens":12,"output_tokens":3,"input_price_usd_per_million_tokens":0,` +
			`"output_price_usd_per_million_tokens":0,"cost_usd":0,"latency_ms":0,"platform_id":"openai",` +
			`"model":"gpt-test"},"finish_reason":"stop"}`},
		{id: "MSG00005", parent: "MSG00004", payload: `{"role":"assistant","content":[{"type":"text","text":"calling"}],` +
			`"tool_calls":[{"id":"` + entryJSONToolID + `","name":"` + entryJSONToolName + `","arguments":{"path":"README.md"}}]}`},
		{id: "MSG00006", parent: "MSG00005", payload: `{"role":"tool","content":[{"type":"text","text":"contents"}],` +
			`"tool_call_id":"` + entryJSONToolID + `","tool_name":"` + entryJSONToolName + `","is_error":true}`},
		{id: "MSG00007", parent: "MSG00006", payload: `{"role":"tool","content":[{"type":"text","text":"screenshot"},` +
			`{"type":"image","image":{"url":"` + entryJSONImageURL + `"}}],"tool_call_id":"` + entryJSONToolID + `",` +
			`"tool_name":"screenshot"}`},
	}
	path := writeEntryJSONFile(t, root, entryJSONWorkDir, entryJSONSessionID, lines)

	manager, err := OpenOrCreate(root, entryJSONWorkDir, entryJSONSessionID)
	if err != nil {
		t.Fatalf("OpenOrCreate: %v", err)
	}
	if manager.Path() != filepath.FromSlash(path) {
		t.Fatalf("打开的是 %q，想要 %q", manager.Path(), path)
	}

	entries := manager.Entries()
	if len(entries) != len(lines)+1 {
		t.Fatalf("entry 条数 = %d，想要 %d（1 行 header + %d 条消息）",
			len(entries), len(lines)+1, len(lines))
	}

	wantTypes := []schema.Message{
		&schema.SystemMessage{}, &schema.UserMessage{}, &schema.UserMessage{},
		&schema.AssistantMessage{}, &schema.AssistantMessage{},
		&schema.ToolResultMessage{}, &schema.ToolResultMessage{},
	}
	wantRoles := []schema.Role{
		schema.RoleSystem, schema.RoleUser, schema.RoleUser,
		schema.RoleAssistant, schema.RoleAssistant, schema.RoleTool, schema.RoleTool,
	}
	for index, want := range wantTypes {
		entry := entries[index+1]
		if entry.Type != EntryMessage {
			t.Fatalf("第 %d 条 entry 类型 = %q，想要 %q", index+1, entry.Type, EntryMessage)
		}
		if reflect.TypeOf(entry.Message) != reflect.TypeOf(want) {
			t.Fatalf("第 %d 条消息类型 = %T，想要 %T", index+1, entry.Message, want)
		}
		if got := entry.Message.Role(); got != wantRoles[index] {
			t.Fatalf("第 %d 条消息角色 = %q，想要 %q", index+1, got, wantRoles[index])
		}
	}

	// 3：图片块落在 user 消息上，URL 读得出来。
	imageMessage, ok := entries[3].Message.(*schema.UserMessage)
	if !ok {
		t.Fatalf("第 3 条消息 = %T，想要 *schema.UserMessage", entries[3].Message)
	}
	if len(imageMessage.Content) != 2 {
		t.Fatalf("第 3 条消息内容块数 = %d，想要 2", len(imageMessage.Content))
	}
	if imageMessage.Content[1].Image == nil || imageMessage.Content[1].Image.URL != entryJSONImageURL {
		t.Fatalf("第 3 条消息图片块 = %+v，想要 URL %q", imageMessage.Content[1].Image, entryJSONImageURL)
	}

	// 4：assistant 的用量与结束原因。
	assistant, ok := entries[4].Message.(*schema.AssistantMessage)
	if !ok {
		t.Fatalf("第 4 条消息 = %T，想要 *schema.AssistantMessage", entries[4].Message)
	}
	if assistant.Usage == nil || assistant.Usage.Model != "gpt-test" {
		t.Fatalf("第 4 条消息用量 = %+v，想要 model %q", assistant.Usage, "gpt-test")
	}
	if assistant.FinishReason != schema.FinishReasonStop {
		t.Fatalf("第 4 条消息结束原因 = %q，想要 %q", assistant.FinishReason, schema.FinishReasonStop)
	}

	// 5：assistant 的工具调用。
	calling, ok := entries[5].Message.(*schema.AssistantMessage)
	if !ok {
		t.Fatalf("第 5 条消息 = %T，想要 *schema.AssistantMessage", entries[5].Message)
	}
	if len(calling.ToolCalls) != 1 || calling.ToolCalls[0].ID != entryJSONToolID {
		t.Fatalf("第 5 条消息工具调用 = %+v，想要 id %q", calling.ToolCalls, entryJSONToolID)
	}

	// 6：工具结果的身份字段。
	toolResult, ok := entries[6].Message.(*schema.ToolResultMessage)
	if !ok {
		t.Fatalf("第 6 条消息 = %T，想要 *schema.ToolResultMessage", entries[6].Message)
	}
	if toolResult.ToolCallID != entryJSONToolID || toolResult.ToolName != entryJSONToolName || !toolResult.IsError {
		t.Fatalf("第 6 条消息 = %+v，想要 id=%q name=%q is_error=true",
			toolResult, entryJSONToolID, entryJSONToolName)
	}

	// 7：带图片、且不是错误的工具结果。
	imageResult, ok := entries[7].Message.(*schema.ToolResultMessage)
	if !ok {
		t.Fatalf("第 7 条消息 = %T，想要 *schema.ToolResultMessage", entries[7].Message)
	}
	if len(imageResult.Content) != 2 || imageResult.IsError {
		t.Fatalf("第 7 条消息 = %+v，想要 2 个内容块且 is_error=false", imageResult)
	}

	// 重建出的历史条数与顺序与文件一致：坏行策略没吃掉任何一条好行。
	if messages := manager.BuildMessages(); len(messages) != len(lines) {
		t.Fatalf("重建出的消息条数 = %d，想要 %d", len(messages), len(lines))
	}
}

// TestSessionLoadDropsBadMessageLines 坏行策略是"丢掉这一行，不是整份文件失败"：
// 解不出来的行、以及载荷与类型对不上的行都跳过，好行照旧留在历史里。这是既有行为
// 的显式化，不是新行为——坏行只影响自己。
func TestSessionLoadDropsBadMessageLines(t *testing.T) {
	root := t.TempDir()
	lines := []entryJSONLine{
		{id: "GOOD0001", parent: entryJSONSessionID, payload: `{"role":"user","content":[{"type":"text","text":"before"}]}`},
		// 未知 role：解码就失败。
		{id: "BAD00001", parent: "GOOD0001", payload: `{"role":"wizard","content":[{"type":"text","text":"unknown role"}]}`},
		// 工具结果缺 tool_name：解码跑不过 Validate。
		{id: "BAD00002", parent: "GOOD0001", payload: `{"role":"tool","content":[{"type":"text","text":"no name"}],"tool_call_id":"call-1"}`},
		// message 键整个缺掉：解码过得去，载荷是空，被 entry.validate 挡下。
		{id: "BAD00003", parent: "GOOD0001", payload: ""},
		{id: "GOOD0002", parent: "GOOD0001", payload: `{"role":"assistant","content":[{"type":"text","text":"after"}]}`},
	}
	writeEntryJSONFile(t, root, entryJSONWorkDir, entryJSONSessionID+"-bad", lines)

	manager, err := OpenOrCreate(root, entryJSONWorkDir, entryJSONSessionID+"-bad")
	if err != nil {
		t.Fatalf("OpenOrCreate: %v", err)
	}

	entries := manager.Entries()
	gotIDs := make([]string, 0, len(entries))
	for _, entry := range entries {
		gotIDs = append(gotIDs, entry.ID)
	}
	wantIDs := []string{entryJSONSessionID + "-bad", "GOOD0001", "GOOD0002"}
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Fatalf("留下的 entry id = %v，想要 %v（三条坏行都该被丢掉）", gotIDs, wantIDs)
	}
	if manager.LeafID() != "GOOD0002" {
		t.Fatalf("叶子 = %q，想要 %q", manager.LeafID(), "GOOD0002")
	}

	// 好行的历史完好：parent 链没断，两条消息都在。
	messages := manager.BuildMessages()
	if len(messages) != 2 {
		t.Fatalf("重建出的消息条数 = %d，想要 2", len(messages))
	}
	for index, want := range []schema.Role{schema.RoleUser, schema.RoleAssistant} {
		if got := messages[index].Role(); got != want {
			t.Fatalf("messages[%d] 角色 = %q，想要 %q", index, got, want)
		}
	}
}

// TestHeaderEntryMarshalsWithoutMessageKey header 行没有 message 载荷，编码出来就不该
// 有 message 键；message 行反过来必须有。
func TestHeaderEntryMarshalsWithoutMessageKey(t *testing.T) {
	header := Entry{
		Type: EntryHeader, ID: entryJSONSessionID, Timestamp: entryJSONTimestamp,
		Header: &Header{
			ID: entryJSONSessionID, Version: sessionVersion,
			CreatedAt: entryJSONTimestamp, WorkDir: entryJSONWorkDir,
		},
	}
	encoded, err := json.Marshal(header)
	if err != nil {
		t.Fatalf("编码 header entry: %v", err)
	}
	if strings.Contains(string(encoded), `"message"`) {
		t.Fatalf("header entry 写出了 message 键: %s", encoded)
	}

	message := Entry{
		Type: EntryMessage, ID: "MSG00001", ParentID: entryJSONSessionID,
		Timestamp: entryJSONTimestamp,
		Message:   &schema.UserMessage{Content: schema.ContentBlocks{schema.TextBlock("hi")}},
	}
	encoded, err = json.Marshal(message)
	if err != nil {
		t.Fatalf("编码 message entry: %v", err)
	}
	if !strings.Contains(string(encoded), `"message":{`) {
		t.Fatalf("message entry 没写出 message 键: %s", encoded)
	}
}

// TestGoldenFixtureReencodeByteIdentical 是最强的兼容证明：把盘上的金标准 fixture 按行
// 解码成 Entry（走新的 Entry.UnmarshalJSON），再编码回去，逐字节等于原文件。任何一行
// 不一致都说明编解码的键序或 omitempty 语义与旧编码器不同——该改的是编码器，不是断言。
//
// 这条能成立是因为 fixture 的字节就是重构前编码器的输出（Ruling 12 在重构前的代码上
// 验过同样的往返）。断言用原文件整体比对，所以行尾换行与空行也一并钉住。
func TestGoldenFixtureReencodeByteIdentical(t *testing.T) {
	data, err := os.ReadFile(goldenFixture)
	if err != nil {
		t.Fatalf("读取 fixture %s: %v", goldenFixture, err)
	}

	rawLines := strings.Split(string(data), "\n")
	// 文件以换行结尾，Split 会多出一个空串；中间不允许有空行。
	if rawLines[len(rawLines)-1] != "" {
		t.Fatalf("fixture 末尾没有换行: %q", rawLines[len(rawLines)-1])
	}
	rawLines = rawLines[:len(rawLines)-1]
	if len(rawLines) != 6 {
		t.Fatalf("fixture 行数 = %d，想要 6", len(rawLines))
	}

	entries := make([]Entry, 0, len(rawLines))
	for index, line := range rawLines {
		entries = append(entries, entryOf(t, line, index+1))
	}

	for index, line := range rawLines {
		number := index + 1
		entry := entries[index]
		t.Run(fmt.Sprintf("第 %d 行", number), func(t *testing.T) {
			reencoded, err := json.Marshal(entry)
			if err != nil {
				t.Fatalf("第 %d 行重新编码失败: %v", number, err)
			}
			if string(reencoded) != line {
				t.Fatalf("第 %d 行重新编码后与盘上字节不同:\n盘上: %s\n编码: %s", number, line, reencoded)
			}
			t.Logf("第 %d 行：%d 字节，重新编码后逐字节相同", number, len(line))
		})
	}

	// 整体再来一次：把解出来的 entry 逐行编回去拼成文件，与盘上的字节完全一致。
	rebuilt := make([]string, 0, len(entries))
	for index, entry := range entries {
		reencoded, err := json.Marshal(entry)
		if err != nil {
			t.Fatalf("第 %d 行重新编码失败: %v", index+1, err)
		}
		rebuilt = append(rebuilt, string(reencoded))
	}
	if got := strings.Join(rebuilt, "\n") + "\n"; got != string(data) {
		t.Fatalf("整份文件重新编码后不一致:\n盘上: %s\n编码: %s", data, got)
	}
}

// entryOf 解出一行 entry，失败即终止（供整体比对复用解出来的值）。
func entryOf(t *testing.T, line string, number int) Entry {
	t.Helper()

	var entry Entry
	if err := json.Unmarshal([]byte(line), &entry); err != nil {
		t.Fatalf("第 %d 行解码失败: %v", number, err)
	}

	return entry
}
