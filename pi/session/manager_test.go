package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 会话文件名 = 会话 id：id 是每个会话自己的身份，文件由它决定，一个会话一个文件。
func TestOpenOrCreateNamesFileAfterSessionID(t *testing.T) {
	root := t.TempDir()
	workDir := t.TempDir()

	manager, err := OpenOrCreate(root, workDir, "abc123")
	if err != nil {
		t.Fatalf("OpenOrCreate: %v", err)
	}

	want := filepath.Join(root, encodeWorkDir(workDir), "abc123.jsonl")
	if got := manager.Path(); got != want {
		t.Fatalf("会话文件路径 = %q，想要 %q", got, want)
	}
	if _, err = os.Stat(want); err != nil {
		t.Fatalf("会话文件不在盘上: %v", err)
	}
}

// 同一个会话 id 再取一次还是那个文件——"续接"不靠别的状态，靠 id。
func TestOpenOrCreateReopensTheSameFileBySessionID(t *testing.T) {
	root := t.TempDir()
	workDir := t.TempDir()

	first, err := OpenOrCreate(root, workDir, "abc123")
	if err != nil {
		t.Fatalf("OpenOrCreate: %v", err)
	}
	second, err := OpenOrCreate(root, workDir, "abc123")
	if err != nil {
		t.Fatalf("OpenOrCreate 第二次: %v", err)
	}

	if first.Path() != second.Path() {
		t.Fatalf("同一个 id 拿到了两个文件: %q / %q", first.Path(), second.Path())
	}
	if second.Path() != filepath.Join(root, encodeWorkDir(workDir), "abc123.jsonl") {
		t.Fatalf("第二次的路径不对: %q", second.Path())
	}
}

// NewSessionID 生成的 id 能直接当参数用（过得了 validateSessionID），带 chat- 前缀，
// 且两次不重样。
func TestNewSessionIDIsPrefixedValidAndUnique(t *testing.T) {
	first, second := NewSessionID(), NewSessionID()

	if !strings.HasPrefix(first, sessionIDPrefix) {
		t.Fatalf("会话 id %q 缺少前缀 %q", first, sessionIDPrefix)
	}
	if err := validateSessionID(first); err != nil {
		t.Fatalf("生成的会话 id %q 过不了校验: %v", first, err)
	}
	if first == second {
		t.Fatalf("两次生成同一个会话 id: %q", first)
	}
}

// 生成的 id 落到盘上就是 chat-….jsonl。
func TestOpenOrCreateAcceptsGeneratedSessionID(t *testing.T) {
	root := t.TempDir()
	workDir := t.TempDir()
	id := NewSessionID()

	manager, err := OpenOrCreate(root, workDir, id)
	if err != nil {
		t.Fatalf("OpenOrCreate: %v", err)
	}

	got := manager.Path()
	want := filepath.Join(root, encodeWorkDir(workDir), id+".jsonl")
	if got != want {
		t.Fatalf("会话文件路径 = %q，想要 %q", got, want)
	}
	if !strings.HasPrefix(filepath.Base(got), sessionIDPrefix) {
		t.Fatalf("文件名 %q 少了前缀 %q", filepath.Base(got), sessionIDPrefix)
	}
}
