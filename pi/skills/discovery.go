package skills

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	pierrors "github.com/PycMono/go-harness/pi/error"
)

// skillSources 按优先级顺序列出 Skill 的发现来源，
// 切片顺序即同名 Skill 的覆盖顺序；Source 的字符串值就是对应的工作区相对目录。
var skillSources = []Source{SourceWorkspace, SourceAgents, SourceClaw}

// Discover 技能发现
// 查找指定目录下的技能
func Discover(workDir string) (*Snapshot, error) {
	return discover(workDir)
}

// discover 使用指定运行环境发现 Skill，供包内测试注入稳定的环境条件。
func discover(workDir string) (*Snapshot, error) {
	absoluteWorkDir, err := filepath.Abs(workDir)
	if err != nil {
		return nil, pierrors.ErrWorkspaceInvalid.Wrap(fmt.Errorf("解析技能工作区失败: %w", err))
	}
	root, err := os.OpenRoot(absoluteWorkDir)
	if err != nil {
		return nil, pierrors.ErrWorkspaceInvalid.Wrap(fmt.Errorf("打开技能工作区失败: %w", err))
	}
	defer root.Close()

	// 根据路径获取技能
	bySource := make([][]Summary, len(skillSources))
	diagnostics := make([]Diagnostic, 0)
	for index, source := range skillSources {
		candidates, sourceDiagnostics, err := discoverSkillSource(root, source)
		if err != nil {
			return nil, pierrors.ErrWorkspaceInvalid.Wrap(fmt.Errorf("发现 Agent Skills 失败: %w", err))
		}
		bySource[index] = candidates
		diagnostics = append(diagnostics, sourceDiagnostics...)
	}

	winners, mergeDiagnostics := mergeDiscoveredSkills(bySource)
	diagnostics = append(diagnostics, mergeDiagnostics...)
	return newSnapshot(winners, diagnostics), nil
}

// discoverSkillSource 递归扫描一个 Skill 来源目录，读取并解析其中的 SKILL.md。
// 它会跳过软链接和非普通文件，拒绝过大的文件和不符合当前环境要求的 Skill，
// 并将单个 Skill 的问题记录为诊断，避免影响同一来源中的其他有效 Skill。
func discoverSkillSource(
	root *os.Root,
	source Source,
) ([]Summary, []Diagnostic, error) {
	directory := string(source)
	info, err := root.Stat(filepath.FromSlash(directory))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("检查技能目录 %s 失败: %w", directory, err)
	}
	if !info.IsDir() {
		return nil, nil, nil
	}

	candidates := make([]Summary, 0)
	diagnostics := make([]Diagnostic, 0)
	err = fs.WalkDir(root.FS(), directory, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || entry.Name() != "SKILL.md" {
			return nil
		}

		// 读取文件
		summary, diagnostic, accepted := inspectSkillFile(root, path, source)
		if diagnostic != nil {
			diagnostics = append(diagnostics, *diagnostic)
		}
		if !accepted {
			return nil
		}
		candidates = append(candidates, summary)

		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("扫描技能目录 %s 失败: %w", directory, err)
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Location < candidates[j].Location
	})
	return candidates, diagnostics, nil
}

// inspectSkillFile 检查、读取并解析一个 SKILL.md，返回可合并的摘要或诊断信息。
func inspectSkillFile(root *os.Root, path string, source Source) (Summary, *Diagnostic, bool) {
	location := filepath.ToSlash(path)
	entryInfo, err := root.Lstat(filepath.FromSlash(path))
	if err != nil {
		diagnostic := sentinelDiagnostic(location, SeverityWarning, pierrors.ErrSkillFileUnreadable)
		return Summary{}, &diagnostic, false
	}
	if !entryInfo.Mode().IsRegular() {
		return Summary{}, nil, false
	}
	if entryInfo.Size() > maxSkillFileBytes {
		diagnostic := sentinelDiagnostic(location, SeverityWarning, pierrors.ErrSkillFileTooLarge)
		return Summary{}, &diagnostic, false
	}

	// 读取内容
	content, err := readLimitedSkillFile(root, filepath.FromSlash(path))
	if err != nil {
		diagnostic := sentinelDiagnostic(location, SeverityWarning, pierrors.ErrSkillFileUnreadable)
		return Summary{}, &diagnostic, false
	}
	if len(content) > maxSkillFileBytes {
		diagnostic := sentinelDiagnostic(location, SeverityWarning, pierrors.ErrSkillFileTooLarge)
		return Summary{}, &diagnostic, false
	}

	// 解析成技能对象
	parsed, err := parseToSkill(content)
	if err != nil {
		var coded *pierrors.CodeError
		if !errors.As(err, &coded) {
			coded = pierrors.ErrSkillFrontMatterInvalid
		}
		diagnostic := sentinelDiagnostic(location, SeverityWarning, coded)
		return Summary{}, &diagnostic, false
	}

	digest := sha256.Sum256(content)
	return Summary{
		Name:        parsed.Name,
		Description: parsed.Description,
		Location:    location,
		Version:     fmt.Sprintf("sha256:%x", digest[:8]),
		Source:      source,
	}, nil, true
}

// sentinelDiagnostic 用 pi/errors 稳定码错误值创建路径统一的 Skill 诊断信息。
func sentinelDiagnostic(path string, severity Severity, sentinel *pierrors.CodeError) Diagnostic {
	return Diagnostic{Path: filepath.ToSlash(path), Severity: severity, Code: sentinel.Code(), Message: sentinel.Message()}
}

// mergeDiscoveredSkills 裁决各来源候选中的同名冲突，
// 输出按 name 唯一的最终 Skill 名单和每个落选候选的诊断。
// bySource 与 skillSources 一一对应，下标越靠前优先级越高：
//   - 同一 name 跨来源重复时，最先出现的候选（即最高优先级来源的）胜出，
//     其余候选记 ErrSkillShadowed；
//   - 同一来源内部同名时无法裁决正确版本，该 name 整体作废，
//     全部候选记 ErrSkillDuplicateName；
//   - 已占用或已作废的 name 在后续来源中的候选不参与补位，
//     统一记 ErrSkillShadowed——宁可名字空缺，也不静默换实现。
//
// 赢家按来源优先级排列、来源内按 name 排序，对同一输入输出确定。
func mergeDiscoveredSkills(bySource [][]Summary) ([]Summary, []Diagnostic) {
	result := make([]Summary, 0)
	won := make(map[string]bool)
	blocked := make(map[string]bool)
	diagnostics := make([]Diagnostic, 0)

	for _, candidates := range bySource {
		groups := make(map[string][]Summary)
		for _, candidate := range candidates {
			groups[candidate.Name] = append(groups[candidate.Name], candidate)
		}
		names := make([]string, 0, len(groups))
		for name := range groups {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			group := groups[name]
			duplicate := len(group) > 1
			if duplicate {
				for _, candidate := range group {
					diagnostics = append(diagnostics, sentinelDiagnostic(candidate.Location,
						SeverityWarning, pierrors.ErrSkillDuplicateName))
				}
			}
			if blocked[name] || won[name] {
				for _, candidate := range group {
					diagnostics = append(diagnostics, sentinelDiagnostic(candidate.Location,
						SeverityInfo, pierrors.ErrSkillShadowed))
				}
				continue
			}
			if duplicate {
				blocked[name] = true
				continue
			}
			won[name] = true
			result = append(result, group[0])
		}
	}
	return result, diagnostics
}
