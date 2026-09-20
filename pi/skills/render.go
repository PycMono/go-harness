package skills

import (
	"strings"
)

/*
	把技能列表渲染成提示词字符串

*/

// 将 XML 的五个保留字符替换为实体形式，
// 保证技能的 name、description 等自由文本嵌入目录后仍是格式完整的 XML。
var xmlTextReplacer = strings.NewReplacer(
	"&", "&amp;",
	"<", "&lt;",
	">", "&gt;",
	`"`, "&quot;",
	"'", "&apos;",
)

// Render 将当前 Skill 快照渲染成注入系统提示词的 XML 目录。
// 长度由发现期保证：description 超过 1024 字符的 Skill 在解析时已被拒绝，
// 这里只做过滤和转义，不做渲染期截断。
func (s *Snapshot) Render() string {
	if len(s.skills) == 0 {
		return ""
	}

	var builder strings.Builder
	builder.WriteString(skillPromptInstructions)
	for _, skill := range s.skills {
		builder.WriteString(skill.renderEntry(skill.Description))
	}
	builder.WriteString(skillPromptClosing)

	return builder.String()
}
