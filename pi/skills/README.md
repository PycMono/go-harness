# pi/skills

SKILL.md 的发现、解析与 Prompt 渲染。

- 发现（discovery）：扫描工作区内的 skill 目录。
- 解析（parser）：SKILL.md 的 frontmatter（名称校验 `^[a-z0-9]+(?:-[a-z0-9]+)*$` 等）与正文。
- 快照（snapshot）：每次 Run 重新读取，保证技能内容随工作区实时变化。
- Prompt 渲染（prompt/xml_text）：把技能概要渲染进 System Prompt，正文在工具调用时按需加载。

本包不决定何时调用技能，只提供技能素材；组装归 `pi`（`prompt.go`）。