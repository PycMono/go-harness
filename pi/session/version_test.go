package session

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenOrCreateRejectsUnsupportedSessionVersion(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "workspace")
	if err := os.MkdirAll(filepath.Join(root, encodeWorkDir(workDir)), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	path := filepath.Join(root, encodeWorkDir(workDir), "chat.jsonl")
	data := `{"type":"session","id":"chat","timestamp":"2026-01-01T00:00:00Z","header":{"id":"chat","version":1,"created_at":"2026-01-01T00:00:00Z","work_dir":"` + workDir + `"}}` + "\n"
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	if _, err := OpenOrCreate(root, workDir, "chat"); err == nil {
		t.Fatal("OpenOrCreate() accepted an unsupported session version")
	}
}
