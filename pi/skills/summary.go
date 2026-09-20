package skills

import (
	"fmt"
)

// Summary 保存一个可用 Skill 的目录摘要。技能是渐进式加载的。
type Summary struct {
	// Name 是 Skill 的唯一名称。
	Name string
	// Description 说明 Skill 的用途。
	Description string
	// Location 是相对于工作区的 SKILL.md 路径。
	Location string
	// Version 是根据 SKILL.md 内容生成的版本标识。
	Version string
	// Source 表示 Skill 来自哪个约定目录。
	Source Source
}

// Diagnostic 没有被发现扫描进 Summary 的原因，简单理解为：任何环节被淘汰 → 进 diagnostics，不产生 Summary
type Diagnostic struct {
	// Path 是相关 SKILL.md 的工作区相对路径。
	Path string
	// Severity 是诊断信息的严重程度。
	Severity Severity
	// Code 是稳定的机器可读诊断代码。
	Code int
	// Message 是适合展示的诊断说明。
	Message string
}

type Summarys []Summary

// renderEntry 将该 Skill 摘要和给定描述清理并转义为安全的 XML <skill> 节点。
func (s Summary) renderEntry(description string) string {
	return fmt.Sprintf(
		"  <skill>\n    <name>%s</name>\n    <description>%s</description>\n    <location>%s</location>\n    <version>%s</version>\n  </skill>\n",
		xmlTextReplacer.Replace(sanitizeXMLText(s.Name)),
		xmlTextReplacer.Replace(sanitizeXMLText(description)),
		xmlTextReplacer.Replace(sanitizeXMLText(s.Location)),
		xmlTextReplacer.Replace(sanitizeXMLText(s.Version)),
	)
}
