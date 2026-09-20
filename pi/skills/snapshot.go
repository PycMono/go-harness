package skills

// Snapshot Skill 汇总。
// 校验通过且胜出 → 进 skills，不产生 Diagnostic
// 任何环节被淘汰 → 进 diagnostics，不产生 Summary
type Snapshot struct {
	skills      Summarys
	diagnostics []Diagnostic
}

// newSnapshot 复制 Skill 摘要和诊断信息，创建不受调用方后续切片修改影响的快照。
func newSnapshot(skills []Summary, diagnostics []Diagnostic) *Snapshot {
	items := append([]Summary(nil), skills...)
	return &Snapshot{
		skills:      items,
		diagnostics: append([]Diagnostic(nil), diagnostics...),
	}
}

// Skills 返回快照中可用 Skill 摘要的副本。
func (s *Snapshot) Skills() []Summary {
	return append([]Summary(nil), s.skills...)
}

// Empty 表示快照中是否没有可用 Skill。
func (s *Snapshot) Empty() bool {
	return len(s.skills) == 0
}
