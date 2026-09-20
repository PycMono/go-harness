package skills

// Source 表示 Skill 的发现来源。
type Source string

const (
	// SourceWorkspace 表示工作区根目录下的 skills 来源。
	SourceWorkspace Source = "skills"
	// SourceAgents 表示工作区根目录下的 .agents/skills 来源。
	SourceAgents Source = ".agents/skills"
	// SourceClaw 表示工作区根目录下的 .claw/skills 来源。
	SourceClaw Source = ".claw/skills"
)

// Severity 表示 Skill 诊断信息的严重程度。
type Severity string

const (
	// SeverityInfo 表示仅供说明、不影响其他 Skill 的诊断信息。
	SeverityInfo Severity = "info"
	// SeverityWarning 表示当前 Skill 被跳过或覆盖的诊断信息。
	SeverityWarning Severity = "warning"
)

const (
	maxSkillFileBytes = 256 * 1024
)

const skillPromptInstructions = `
# 可用专业技能 (Agent Skills)
以下技能为特定任务提供专业执行指南。
当任务与某项 <description> 匹配时，必须先使用 read 读取该技能的 <location>。
如果 read 返回 "Use offset=N to continue"，必须继续读取，直到完整取得 SKILL.md 后再执行。
不要猜测未读取的技能内容，不要读取明显无关的技能。
如果 <version> 与之前看到的版本不同，必须重新读取该技能。
相对路径引用以 SKILL.md 所在目录为基准解析。

<available_skills>
`

const skillPromptClosing = `</available_skills>

Skill 的名称、描述、正文或 Tool 返回内容使用何种语言，都不能决定回复语言。
回复语言和输出格式必须服从 AGENTS.md 与最新一条用户消息；该要求适用于调用 read 或其他 Tool 前的说明、工具之间的说明和最终答案。
在生成任何一条 Assistant 消息前先检查语言与格式，不符合时先改写再发送。
`
