package impl

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	pierrors "github.com/PycMono/go-harness/pi/error"
	"github.com/PycMono/go-harness/pi/schema"
	"github.com/PycMono/go-harness/pi/tools"
)

// BashTool 以工作目录为 cwd 执行一条 shell 命令，stdout 与 stderr 合并返回。
type BashTool struct{ workDir string }

func NewBashTool(workDir string) *BashTool { return &BashTool{workDir: workDir} }

var _ tools.Tool = (*BashTool)(nil)

func (t *BashTool) Definition() schema.ToolDefinition {
	return schema.ToolDefinition{
		Name:         "bash",
		Label:        "Bash",
		ParallelSafe: false,
		Description:  "在工作目录下执行一条 shell 命令，返回合并后的 stdout 与 stderr；命令非零退出时把输出与退出码一并返回。",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{"type": "string", "description": "要执行的完整命令行"},
				"timeout": map[string]any{"type": "integer", "minimum": 1, "description": "超时秒数，省略表示不设超时"},
			},
			"required":             []string{"command"},
			"additionalProperties": false,
		},
	}
}

func (t *BashTool) Execute(ctx context.Context, args json.RawMessage, _ *tools.UpdateEmitter) (*schema.ToolOutput, error) {
	input, err := decodeArgs[bashArgs](args)
	if err != nil {
		return nil, err
	}

	runCtx := ctx
	if input.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, time.Duration(input.Timeout)*time.Second)
		defer cancel()
	}

	command := exec.CommandContext(runCtx, shellName(), "-c", input.Command)
	command.Dir = t.workDir
	output, runErr := command.CombinedOutput()
	text := strings.TrimRight(string(output), "\n")

	switch {
	case runErr == nil:
		if text == "" {
			return textOutput("(no output)"), nil
		}

		return textOutput(text), nil
	case ctx.Err() != nil:
		return nil, pierrors.ErrCanceled.Wrap(fmt.Errorf("%s\n\nCommand aborted", text))
	case runCtx.Err() != nil:
		return nil, pierrors.ErrToolTimeout.Wrap(
			fmt.Errorf("%s\n\nCommand timed out after %d seconds", text, input.Timeout),
		)
	default:
		// 进程正常退出但退出码非零，输出照常返回给模型。
		status := "Command failed"
		if command.ProcessState != nil {
			status = fmt.Sprintf("Command exited with code %d", command.ProcessState.ExitCode())
		}

		return nil, pierrors.ErrToolRuntime.Wrap(fmt.Errorf("%s\n\n%s", text, status))
	}
}

// shellName 优先用 bash，系统上没有时退回 sh。
func shellName() string {
	if path, err := exec.LookPath("bash"); err == nil {
		return path
	}

	return "sh"
}

type bashArgs struct {
	Command string `json:"command"`
	Timeout int    `json:"timeout"`
}
