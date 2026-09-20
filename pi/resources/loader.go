package resources

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"unicode/utf8"

	pierrors "github.com/PycMono/go-harness/pi/error"
	"github.com/PycMono/go-harness/pi/resources/skills"
)

const maxAgentsFileBytes = 1024 * 1024

const corePrompt = `# Agent Runtime 核心纪律

1. 必须遵守工作区 AGENTS.md 中定义的身份、职责和行为边界。
2. 必须根据当前任务判断是否存在匹配的 Skill；使用 Skill 前，必须通过 read 完整读取对应的 SKILL.md。
3. 只能调用当前请求中实际提供定义的工具，不得虚构或模拟工具调用。
4. 当没有提供工具定义时，你正处于 Thinking 阶段：只能分析和规划，不得声称工具已执行，也不得编造外部事实。
5. 工具执行失败时，必须依据真实错误处理，不得声称操作成功。
6. 最终回答必须以当前上下文、Skill 指令和真实工具结果为依据。
`

/*
	资源加载管理
	1、加载 agents
	2、加载 skills
*/

// Loader 资源加载管理
type Loader struct {
	agents []byte
	skills *skills.Snapshot
}

// Load 读取工作区的 AGENTS.md 内容并发现 Skill 快照。
func Load(ctx context.Context, workDir string) (*Loader, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if strings.TrimSpace(workDir) == "" {
		return nil, pierrors.ErrWorkspaceInvalid.Wrap(fmt.Errorf("workDir is required"))
	}

	// 先加载agent
	agents, err := loadAgents(workDir)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// 再加载 skill
	snapshot, err := skills.Discover(workDir)
	if err != nil {
		return nil, pierrors.ErrWorkspaceInvalid.Wrap(fmt.Errorf("发现 Agent Skills 失败: %w", err))
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}

	return &Loader{agents: agents, skills: snapshot}, nil
}

// SystemPrompt 核心指令、技能目录和工作区 AGENTS.md 组装成完整的系统提示词。
func (s Loader) SystemPrompt() string {
	var builder strings.Builder
	builder.WriteString(corePrompt)

	skillPrompt := s.skills.Render()
	if skillPrompt != "" {
		builder.WriteString("\n")
		builder.WriteString(skillPrompt)
	}

	// 组装 agent
	builder.WriteString("\n# Agent 定义（来自 AGENTS.md）\n\n")
	builder.Write(s.agents)
	builder.WriteString("\n")

	return builder.String()
}

// 打开工作区并读取 AGENTS.md：
// 必须是工作区根下的普通文件、不超过 1 MiB、有效 UTF-8 且非空白。
// 每个失败点对应 pi/errors 的具体哨兵码；OS 错误以哨兵 Wrap 挂原因链。
func loadAgents(workDir string) ([]byte, error) {
	if strings.TrimSpace(workDir) == "" {
		return nil, pierrors.ErrWorkDirRequired
	}
	root, err := os.OpenRoot(workDir)
	if err != nil {
		return nil, pierrors.ErrWorkDirUnopenable.Wrap(err)
	}
	defer root.Close()

	info, err := root.Lstat("AGENTS.md")
	if errors.Is(err, fs.ErrNotExist) {
		return nil, pierrors.ErrAgentsFileMissing
	}
	if err != nil {
		return nil, pierrors.ErrAgentsFileUnreadable.Wrap(err)
	}
	if !info.Mode().IsRegular() {
		return nil, pierrors.ErrAgentsFileNotRegular
	}
	if info.Size() > maxAgentsFileBytes {
		return nil, pierrors.ErrAgentsFileTooLarge
	}

	file, err := root.Open("AGENTS.md")
	if err != nil {
		return nil, pierrors.ErrAgentsFileUnreadable.Wrap(err)
	}
	defer file.Close()

	content, err := io.ReadAll(io.LimitReader(file, maxAgentsFileBytes+1))
	if err != nil {
		return nil, pierrors.ErrAgentsFileUnreadable.Wrap(err)
	}
	if len(content) > maxAgentsFileBytes {
		return nil, pierrors.ErrAgentsFileTooLarge
	}
	if !utf8.Valid(content) || bytes.IndexByte(content, 0) >= 0 {
		return nil, pierrors.ErrAgentsFileNotUTF8
	}
	if strings.TrimSpace(string(content)) == "" {
		return nil, pierrors.ErrAgentsFileEmpty
	}

	return content, nil
}
