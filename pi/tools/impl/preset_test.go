package impl

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pierrors "github.com/PycMono/go-harness/pi/error"
	"github.com/PycMono/go-harness/pi/schema"
	"github.com/PycMono/go-harness/pi/tools"
)

func newRegistry(t *testing.T, workDir string) *tools.Registry {
	t.Helper()
	registry, err := tools.Register(NewDefaultTools(workDir))
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	return registry
}

func call(t *testing.T, registry *tools.Registry, name string, args map[string]any) tools.Event {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	event, err := tools.NewScheduler(registry, 4, nil).Execute(context.Background(), schema.ToolCall{
		ID: "call-1", Name: name, Arguments: raw,
	})
	if err != nil {
		t.Fatalf("%s: scheduler error: %v", name, err)
	}

	return event
}

func textOf(event tools.Event) string {
	parts := make([]string, 0, len(event.Content))
	for _, block := range event.Content {
		parts = append(parts, block.Text)
	}

	return strings.Join(parts, "")
}

func TestDefinitions(t *testing.T) {
	registry := newRegistry(t, t.TempDir())

	names := make([]string, 0, 4)
	for _, definition := range registry.Definitions() {
		names = append(names, definition.Name)
	}
	want := []string{"bash", "edit", "read", "write"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("definitions = %v, want %v", names, want)
	}
}

func TestWriteReadEditRoundTrip(t *testing.T) {
	workDir := t.TempDir()
	registry := newRegistry(t, workDir)

	event := call(t, registry, "write", map[string]any{"path": "sub/a.txt", "content": "hello\nworld\n"})
	if event.IsError {
		t.Fatalf("write failed: %s", textOf(event))
	}
	if got := textOf(event); !strings.HasPrefix(got, "Successfully wrote to sub/a.txt") {
		t.Fatalf("write text = %q", got)
	}
	if details, ok := event.Details.(WriteDetails); !ok || !details.Changed || details.Bytes != 12 {
		t.Fatalf("write details = %#v", event.Details)
	}

	event = call(t, registry, "read", map[string]any{"path": "sub/a.txt"})
	if event.IsError || textOf(event) != "hello\nworld\n" {
		t.Fatalf("read = %q (err=%v)", textOf(event), event.IsError)
	}

	// offset/limit：从第 2 行起读 1 行。
	event = call(t, registry, "read", map[string]any{"path": "sub/a.txt", "offset": 2, "limit": 1})
	if textOf(event) != "world" {
		t.Fatalf("read offset = %q", textOf(event))
	}

	event = call(t, registry, "edit", map[string]any{
		"path":  "sub/a.txt",
		"edits": []map[string]any{{"oldText": "world", "newText": "golang"}},
	})
	if event.IsError {
		t.Fatalf("edit failed: %s", textOf(event))
	}
	if !strings.Contains(textOf(event), "Applied 1 edits") {
		t.Fatalf("edit text = %q", textOf(event))
	}

	content, err := os.ReadFile(filepath.Join(workDir, "sub/a.txt"))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(content) != "hello\ngolang\n" {
		t.Fatalf("file = %q", string(content))
	}

	// 失败路径：无匹配 / 不唯一。
	event = call(t, registry, "edit", map[string]any{
		"path":  "sub/a.txt",
		"edits": []map[string]any{{"oldText": "不存在的内容", "newText": "x"}},
	})
	if event.ErrorCode != pierrors.ErrToolEditNoMatch.Code() {
		t.Fatalf("expected no-match, got %q code=%d", textOf(event), event.ErrorCode)
	}

	if err := os.WriteFile(filepath.Join(workDir, "dup.txt"), []byte("x\nx\n"), 0o644); err != nil {
		t.Fatalf("seed dup: %v", err)
	}
	event = call(t, registry, "edit", map[string]any{
		"path":  "dup.txt",
		"edits": []map[string]any{{"oldText": "x", "newText": "y"}},
	})
	if event.ErrorCode != pierrors.ErrToolEditNotUnique.Code() {
		t.Fatalf("expected not-unique, got code=%d text=%q", event.ErrorCode, textOf(event))
	}
}

func TestBash(t *testing.T) {
	workDir := t.TempDir()
	registry := newRegistry(t, workDir)

	event := call(t, registry, "bash", map[string]any{"command": "echo hello && pwd"})
	if event.IsError {
		t.Fatalf("bash failed: %s", textOf(event))
	}
	if !strings.HasPrefix(textOf(event), "hello\n") || !strings.Contains(textOf(event), workDir) {
		t.Fatalf("bash output = %q", textOf(event))
	}

	// stderr 合并。
	event = call(t, registry, "bash", map[string]any{"command": "echo out; echo err 1>&2"})
	if textOf(event) != "out\nerr" {
		t.Fatalf("merged output = %q", textOf(event))
	}

	// 无输出。
	event = call(t, registry, "bash", map[string]any{"command": "true"})
	if textOf(event) != "(no output)" {
		t.Fatalf("empty output = %q", textOf(event))
	}

	// 非零退出码：输出与退出码都返回，且是错误事件。
	event = call(t, registry, "bash", map[string]any{"command": "echo before; exit 3"})
	if !event.IsError {
		t.Fatalf("expected error event, got %q", textOf(event))
	}
	// 错误事件的内容是 err.Error()，按仓库既有约定带 code|msg 前缀。
	if !strings.Contains(textOf(event), "before\n\nCommand exited with code 3") {
		t.Fatalf("exit text = %q", textOf(event))
	}

	// 超时。
	start := time.Now()
	event = call(t, registry, "bash", map[string]any{"command": "sleep 5", "timeout": 1})
	if event.ErrorCode != pierrors.ErrToolTimeout.Code() {
		t.Fatalf("expected timeout, got code=%d text=%q", event.ErrorCode, textOf(event))
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("timeout took %v", elapsed)
	}
}

func TestBashInheritsContext(t *testing.T) {
	workDir := t.TempDir()
	registry := newRegistry(t, workDir)
	raw, _ := json.Marshal(map[string]any{"command": "sleep 5"})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	event, err := tools.NewScheduler(registry, 1, nil).Execute(ctx, schema.ToolCall{ID: "c", Name: "bash", Arguments: raw})
	if err != nil {
		t.Fatalf("scheduler error: %v", err)
	}
	if event.ErrorCode != pierrors.ErrCanceled.Code() {
		t.Fatalf("expected canceled, got code=%d text=%q", event.ErrorCode, textOf(event))
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("cancel took %v", elapsed)
	}
}

func TestBadArgumentsRejectedBySchema(t *testing.T) {
	registry := newRegistry(t, t.TempDir())

	event := call(t, registry, "read", map[string]any{"path": "x", "nope": 1})
	if event.ErrorCode != pierrors.ErrToolInvalidArguments.Code() {
		t.Fatalf("expected invalid arguments, got code=%d text=%q", event.ErrorCode, textOf(event))
	}
}

func TestReadPaging(t *testing.T) {
	workDir := t.TempDir()
	registry := newRegistry(t, workDir)

	// 3000 行：第一页 2000 行并给续读位置，第二页拿到剩余且不再提示。
	var builder strings.Builder
	for line := 1; line <= 3000; line++ {
		fmt.Fprintf(&builder, "line-%d\n", line)
	}
	if err := os.WriteFile(filepath.Join(workDir, "big.txt"), []byte(builder.String()), 0o644); err != nil {
		t.Fatalf("seed big: %v", err)
	}

	event := call(t, registry, "read", map[string]any{"path": "big.txt"})
	text := textOf(event)
	if !strings.HasPrefix(text, "line-1\n") {
		t.Fatalf("first page head = %q", text[:min(40, len(text))])
	}
	if !strings.HasSuffix(text, "[Showing lines 1-2000. Use offset=2001 to continue.]") {
		t.Fatalf("first page tail = %q", text[max(0, len(text)-60):])
	}

	event = call(t, registry, "read", map[string]any{"path": "big.txt", "offset": 2001})
	text = textOf(event)
	if !strings.HasPrefix(text, "line-2001\n") || !strings.Contains(text, "line-3000") {
		t.Fatalf("second page head = %q", text[:min(40, len(text))])
	}
	if strings.Contains(text, "Use offset=") {
		t.Fatalf("last page should not ask to continue: %q", text[max(0, len(text)-60):])
	}
}

func TestReadByteBudget(t *testing.T) {
	workDir := t.TempDir()
	registry := newRegistry(t, workDir)

	// 每行 4001 字节，20 行共 80KB：单页受 50KiB 预算约束，只能放下 12 行。
	line := strings.Repeat("x", 4000) + "\n"
	if err := os.WriteFile(filepath.Join(workDir, "wide.txt"), []byte(strings.Repeat(line, 20)), 0o644); err != nil {
		t.Fatalf("seed wide: %v", err)
	}

	event := call(t, registry, "read", map[string]any{"path": "wide.txt"})
	text := textOf(event)
	if !strings.HasSuffix(text, "[Showing lines 1-12. Use offset=13 to continue.]") {
		t.Fatalf("page tail = %q", text[max(0, len(text)-60):])
	}
	if len(text) > maxReadBytes+120 {
		t.Fatalf("page size = %d, want <= %d", len(text), maxReadBytes+120)
	}

	event = call(t, registry, "read", map[string]any{"path": "wide.txt", "offset": 13})
	if strings.Contains(textOf(event), "Use offset=") {
		t.Fatalf("last page should not ask to continue")
	}
}

func TestWriteNoopAndValidation(t *testing.T) {
	workDir := t.TempDir()
	registry := newRegistry(t, workDir)
	target := filepath.Join(workDir, "a.txt")

	event := call(t, registry, "write", map[string]any{"path": "a.txt", "content": "same\n"})
	if details, ok := event.Details.(WriteDetails); !ok || !details.Changed || details.Bytes != 5 {
		t.Fatalf("first write details = %#v", event.Details)
	}
	before, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	// 内容一致再写一次：不落盘，mtime 不变。
	time.Sleep(20 * time.Millisecond)
	event = call(t, registry, "write", map[string]any{"path": "a.txt", "content": "same\n"})
	if details, ok := event.Details.(WriteDetails); !ok || details.Changed {
		t.Fatalf("noop write details = %#v", event.Details)
	}
	if !strings.Contains(textOf(event), "No changes") {
		t.Fatalf("noop text = %q", textOf(event))
	}
	after, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatalf("file was rewritten: %v -> %v", before.ModTime(), after.ModTime())
	}

	// 目标是目录：拒绝覆盖。
	if err = os.Mkdir(filepath.Join(workDir, "dir"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	event = call(t, registry, "write", map[string]any{"path": "dir", "content": "x"})
	if event.ErrorCode != pierrors.ErrToolInvalidArguments.Code() {
		t.Fatalf("expected invalid arguments for directory, got code=%d text=%q", event.ErrorCode, textOf(event))
	}

	// 内容含 NUL：拒绝。
	event = call(t, registry, "write", map[string]any{"path": "b.txt", "content": "a\u0000b"})
	if event.ErrorCode != pierrors.ErrToolInvalidArguments.Code() {
		t.Fatalf("expected invalid arguments for NUL, got code=%d text=%q", event.ErrorCode, textOf(event))
	}
	if _, err = os.Stat(filepath.Join(workDir, "b.txt")); !os.IsNotExist(err) {
		t.Fatalf("b.txt should not exist")
	}
}
