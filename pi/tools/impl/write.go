package impl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"unicode/utf8"

	pierrors "github.com/PycMono/go-harness/pi/error"
	"github.com/PycMono/go-harness/pi/schema"
	"github.com/PycMono/go-harness/pi/tools"
)

// WriteDetails 描述一次写入的结果。Path 是解析后的绝对路径，Changed 为 false
// 表示目标内容已经一致、没有真正写盘。
type WriteDetails struct {
	Path    string `json:"path"`
	Bytes   int    `json:"bytes"`
	Changed bool   `json:"changed"`
}

// WriteTool 整体写入一个文件，父目录不存在时自动创建。
type WriteTool struct{ workDir string }

func NewWriteTool(workDir string) *WriteTool { return &WriteTool{workDir: workDir} }

var _ tools.Tool = (*WriteTool)(nil)

func (t *WriteTool) Definition() schema.ToolDefinition {
	return schema.ToolDefinition{
		Name:         "write",
		Label:        "Write",
		Description:  "把一个文件的完整内容写入磁盘，覆盖已有内容；父目录不存在时自动创建。仅用于新文件或完整重写。",
		ParallelSafe: false,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":    map[string]any{"type": "string", "description": "相对于工作区的文件路径"},
				"content": map[string]any{"type": "string", "description": "文件的完整 UTF-8 文本内容"},
			},
			"required":             []string{"path", "content"},
			"additionalProperties": false,
		},
	}
}

func (t *WriteTool) Execute(ctx context.Context, args json.RawMessage, _ *tools.UpdateEmitter) (*schema.ToolOutput, error) {
	input, err := decodeArgs[writeArgs](args)
	if err != nil {
		return nil, err
	}

	path, err := resolvePath(t.workDir, input.Path)
	if err != nil {
		return nil, err
	}
	content := []byte(input.Content)
	if !utf8.Valid(content) {
		return nil, pierrors.ErrToolInvalidArguments.Wrap(errors.New("content 不是有效的 UTF-8 文本"))
	}
	if bytes.IndexByte(content, 0) >= 0 {
		return nil, pierrors.ErrToolInvalidArguments.Wrap(errors.New("content 包含 NUL 字节，疑似二进制内容"))
	}
	if err = ctx.Err(); err != nil {
		return nil, pierrors.ErrCanceled.Wrap(err)
	}

	exists, err := t.inspect(path)
	if err != nil {
		return nil, err
	}
	if exists {
		// 内容已经一致时不动盘：避免无意义的 mtime 变更，也避免触发下游的文件监听。
		current, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil, pierrors.ErrToolPermissionDenied.Wrap(fmt.Errorf("读取 %s 失败: %w", input.Path, readErr))
		}
		if bytes.Equal(current, content) {
			details := WriteDetails{Path: path, Bytes: len(current), Changed: false}

			return textOutputWithDetails(
				fmt.Sprintf("No changes to %s: the content is already identical.", input.Path), details,
			), nil
		}
	}

	if err = os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, pierrors.ErrToolPermissionDenied.Wrap(fmt.Errorf("创建父目录失败: %w", err))
	}
	if err = ctx.Err(); err != nil {
		return nil, pierrors.ErrCanceled.Wrap(err)
	}
	if err = os.WriteFile(path, content, 0o644); err != nil {
		return nil, pierrors.ErrToolPermissionDenied.Wrap(fmt.Errorf("写入 %s 失败: %w", input.Path, err))
	}

	details := WriteDetails{Path: path, Bytes: len(content), Changed: true}

	return textOutputWithDetails(
		fmt.Sprintf("Successfully wrote to %s (%d bytes)", input.Path, len(content)), details,
	), nil
}

// inspect 检查目标路径：不存在返回 false；存在但不是普通文件时拒绝覆盖
// （否则写目录、设备文件这类目标只会得到一句难懂的系统错误）。
func (t *WriteTool) inspect(path string) (bool, error) {
	info, err := os.Stat(path)
	switch {
	case err == nil:
		if !info.Mode().IsRegular() {
			return false, pierrors.ErrToolInvalidArguments.Wrap(
				fmt.Errorf("%s 不是普通文件，拒绝覆盖", path),
			)
		}

		return true, nil
	case os.IsNotExist(err):
		return false, nil
	default:
		return false, pierrors.ErrToolPermissionDenied.Wrap(fmt.Errorf("检查 %s 失败: %w", path, err))
	}
}

type writeArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}
