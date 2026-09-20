package impl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	pierrors "github.com/PycMono/go-harness/pi/error"
	"github.com/PycMono/go-harness/pi/schema"
	"github.com/PycMono/go-harness/pi/tools"
)

// 单次 read 的返回上限：一次最多读入这么多字节（再多也不读进内存），
// 一页最多返回这么多行。
const (
	maxReadLines = 2000
	maxReadBytes = 50 * 1024
)

// ReadTool 分页读取一个文本文件，可用 offset/limit 取下一页。
type ReadTool struct{ workDir string }

func NewReadTool(workDir string) *ReadTool { return &ReadTool{workDir: workDir} }

var _ tools.Tool = (*ReadTool)(nil)

func (t *ReadTool) Definition() schema.ToolDefinition {
	return schema.ToolDefinition{
		Name:         "read",
		Label:        "Read",
		Description:  "按行读取一个 UTF-8 文本文件。一页最多 2000 行且不超过 50 KiB；出现 Use offset=N to continue 时用 offset 继续读下一页。",
		ParallelSafe: true,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":   map[string]any{"type": "string", "description": "文件路径，相对路径相对工作目录展开"},
				"offset": map[string]any{"type": "integer", "minimum": 1, "description": "起始行号，从 1 开始，默认 1"},
				"limit": map[string]any{
					"type": "integer", "minimum": 1, "maximum": maxReadLines,
					"description": fmt.Sprintf("本页最多返回的行数，默认 %d", maxReadLines),
				},
			},
			"required":             []string{"path"},
			"additionalProperties": false,
		},
	}
}

func (t *ReadTool) Execute(ctx context.Context, args json.RawMessage, _ *tools.UpdateEmitter) (*schema.ToolOutput, error) {
	input, err := decodeArgs[readArgs](args)
	if err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, pierrors.ErrCanceled.Wrap(err)
	}

	path, err := resolvePath(t.workDir, input.Path)
	if err != nil {
		return nil, err
	}
	content, more, err := readCapped(path)
	if err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, pierrors.ErrCanceled.Wrap(err)
	}

	offset := max(input.Offset, 1)
	limit := maxReadLines
	if input.Limit > 0 {
		limit = min(input.Limit, maxReadLines)
	}

	text, err := pageText(content, more, offset, limit)
	if err != nil {
		return nil, err
	}

	return textOutput(text), nil
}

// readCapped 至多读入 maxReadBytes+1 字节：多读的那一个字节只用于判断文件
// 是否还有后续内容。返回值 more 为 true 时，content 已在最后一处换行处切断，
// 只包含完整的行。
func readCapped(path string) (content []byte, more bool, err error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, false, pierrors.ErrToolResourceNotFound.Wrap(fmt.Errorf("打开文件失败: %w", err))
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return nil, false, pierrors.ErrToolResourceNotFound.Wrap(fmt.Errorf("检查文件失败: %w", err))
	}
	if !info.Mode().IsRegular() {
		return nil, false, pierrors.ErrToolInvalidArguments.Wrap(errors.New("只允许读取普通文件"))
	}

	content, err = io.ReadAll(io.LimitReader(file, maxReadBytes+1))
	if err != nil {
		return nil, false, pierrors.ErrToolRuntime.Wrap(fmt.Errorf("读取文件内容失败: %w", err))
	}
	if len(content) <= maxReadBytes {
		return content, false, nil
	}

	content, more = content[:maxReadBytes], true
	// 切断处必须落在行边界上，否则末行会是个半截行。切掉换行本身，
	// 让最后一个元素仍是完整的一行。
	if index := bytes.LastIndexByte(content, '\n'); index >= 0 {
		return content[:index], true, nil
	}

	return nil, false, pierrors.ErrToolInvalidArguments.Wrap(
		fmt.Errorf("首行超过 %d 字节，read 无法按行返回，请改用 bash 命令查看", maxReadBytes),
	)
}

// pageText 从 offset 行开始取至多 limit 行，同时受 maxReadBytes 字节预算约束；
// 还有剩余内容时在末尾附上续读位置。
func pageText(content []byte, more bool, offset, limit int) (string, error) {
	if bytes.IndexByte(content, 0) >= 0 {
		return "", pierrors.ErrToolInvalidArguments.Wrap(errors.New("文件包含 NUL 字节，疑似二进制内容"))
	}
	if !utf8.Valid(content) {
		return "", pierrors.ErrToolInvalidArguments.Wrap(errors.New("文件内容不是有效的 UTF-8 文本"))
	}

	lines := strings.Split(string(content), "\n")
	// 文件以换行结尾时 Split 会多出一个空元素，它不是一个真实的行，
	// 不能拿来判断「后面还有内容」或统计总行数。
	lineCount := len(lines)
	if lineCount > 1 && lines[lineCount-1] == "" {
		lineCount--
	}
	if offset > lineCount && !more {
		return "", pierrors.ErrToolInvalidArguments.Wrap(
			fmt.Errorf("offset %d 超出文件末尾（共 %d 行）", offset, lineCount),
		)
	}

	selected := make([]string, 0, min(limit, len(lines)))
	byteCount, cursor := 0, offset-1
	for ; cursor < len(lines) && len(selected) < limit; cursor++ {
		cost := len(lines[cursor])
		if len(selected) > 0 {
			cost++
		}
		if byteCount+cost > maxReadBytes {
			break
		}
		selected = append(selected, lines[cursor])
		byteCount += cost
	}
	if len(selected) == 0 {
		// 只扫描了文件开头的一段，offset 落在这一段之后：不静默返回空，
		// 明确告诉调用方这个 offset 读不到。
		if more && cursor >= len(lines) {
			return "", pierrors.ErrToolInvalidArguments.Wrap(
				fmt.Errorf("offset %d 超出单次读取的扫描范围（只扫描文件前 %d 字节）", offset, maxReadBytes),
			)
		}

		return "", nil
	}

	text := strings.Join(selected, "\n")
	if more || hasContent(lines[cursor:]) {
		end := offset + len(selected) - 1
		text += fmt.Sprintf("\n\n[Showing lines %d-%d. Use offset=%d to continue.]", offset, end, end+1)
	}

	return text, nil
}

// hasContent 报告剩余行里是否还有真实内容。空行本身也是内容，只有
// 文件末尾换行带来的那个空元素不算。
func hasContent(lines []string) bool {
	for _, line := range lines {
		if line != "" {
			return true
		}
	}

	return false
}

type readArgs struct {
	Path   string `json:"path"`
	Offset int    `json:"offset"`
	Limit  int    `json:"limit"`
}
