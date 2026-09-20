// Package impl 提供内置的四个工具：read / write / edit / bash。
package impl

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"

	pierrors "github.com/PycMono/go-harness/pi/error"
	"github.com/PycMono/go-harness/pi/schema"
	"github.com/PycMono/go-harness/pi/tools"
)

// NewDefaultTools 返回内置工具集。read / write / edit 的相对路径相对 workDir
// 展开，bash 以 workDir 为工作目录执行命令。
func NewDefaultTools(workDir string) []tools.Tool {
	return []tools.Tool{
		NewReadTool(workDir),
		NewWriteTool(workDir),
		NewEditTool(workDir),
		NewBashTool(workDir),
	}
}

// resolvePath 把工具收到的路径解析成绝对路径：绝对路径原样使用，相对路径
// 相对工作目录展开。
func resolvePath(workDir, path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", pierrors.ErrToolInvalidArguments.Wrap(errors.New("path must not be empty"))
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}

	return filepath.Join(workDir, path), nil
}

// decodeArgs 把工具参数解成具体类型，拒绝未知字段与多余内容。
func decodeArgs[T any](args json.RawMessage) (T, error) {
	var input T
	decoder := json.NewDecoder(bytes.NewReader(args))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return input, pierrors.ErrToolInvalidArguments.Wrap(err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return input, pierrors.ErrToolInvalidArguments.Wrap(errors.New("arguments contain trailing content"))
	}

	return input, nil
}

// textOutput 把一段文本包成工具返回值。
func textOutput(text string) *schema.ToolOutput {
	return &schema.ToolOutput{Content: schema.ContentBlocks{schema.TextBlock(text)}}
}

// textOutputWithDetails 同上，并附带结构化详情（如 edit 的 diff）。
func textOutputWithDetails(text string, details any) *schema.ToolOutput {
	output := textOutput(text)
	output.Details = details

	return output
}
