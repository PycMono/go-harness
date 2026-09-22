# 从零手搓 Harness 之 Agent Skills：技能写在 markdown 里，正文按需加载

## 前言

这是从零开始搭建 Agent Harness（`go-harness`）的第四篇。上一篇把主循环装上之后，模型已经能真的读文件、写文件、跑命令了，但它"会什么"这件事还是写死在 System Prompt 里的——想给 agent 加一条新能力，就得改代码、重新编译。这一篇把"专业知识"从代码里挪出去：技能就是工作区里的一个 markdown 文件（`SKILL.md`），改文件就生效，不用重新编译。

这套机制的核心思路是渐进式加载：System Prompt 里只放技能的名字、描述和路径，正文等模型判断任务匹配了再用 `read` 工具去读。技能多了不会把上下文撑爆，这是它和"把所有说明一股脑塞进提示词"最本质的区别。

完整代码参考 GitHub：[https://github.com/PycMono/go-harness](https://github.com/PycMono/go-harness)，本文涉及的所有文件（`pi/skills/`、`pi/prompt.go`、`pi/tools/impl/read.go`、`cmd/skilltest/`）都在仓库里可以直接翻。

## 流程图

下面是技能系统从发现到执行的整个流程，后面所有代码都在这张图里：

```text
工作区根目录
├── AGENTS.md
├── skills/                 ← SourceWorkspace（优先级最高）
├── .agents/skills/         ← SourceAgents
└── .claw/skills/           ← SourceClaw
        │ 每轮 Run 重新扫描
        ▼
┌─ Discover：扫描 + 解析 + 诊断 ──────────────┐
│  SKILL.md → frontmatter 校验 → Summary      │
│  任何环节被淘汰 → Diagnostic（不中断其他技能）│
└──────────────────────────────────────────────┘
        ▼ merge：同名裁决
      Snapshot（校验通过且胜出的技能列表）
        ▼ Render：转义成 XML 目录
System Prompt = 核心纪律 + 技能目录 + AGENTS.md
        ▼ 模型判断任务与某个 <description> 匹配
模型用 read 工具完整读取 SKILL.md → 按技能正文执行
```

图里有两层分工要留意：`pi/skills` 只负责"把技能素材准备好"，它不决定模型什么时候用技能——那是提示词纪律加模型自己的事；组装成 System Prompt 归 `pi/prompt.go`。

## 代码层级划分

所有技能相关代码集中在 `pi/skills`，加上组装和按需加载两处：

```
pi/skills/
├── constant.go     # 三个来源目录、诊断级别、提示词模板
├── discovery.go    # 扫描来源目录 + 同名裁决
├── parser.go       # SKILL.md frontmatter 与正文的解析校验
├── snapshot.go     # 发现结果快照
├── summary.go      # Summary / Diagnostic 模型
├── render.go       # 渲染成注入 System Prompt 的 XML 目录
└── utils.go        # 限额读取、XML 字符合法性
pi/prompt.go        # 核心纪律 + 技能目录 + AGENTS.md 组装
pi/tools/impl/read.go  # 分页读取，技能正文的按需加载通道
```

分工对应流程图的三段：

| 段 | 职责 | 关键文件 |
|---|---|---|
| 发现层 | 扫描来源、解析校验、同名裁决，产出快照 | `discovery.go` / `parser.go` |
| 渲染层 | 概要转义成 XML 目录，进 System Prompt | `render.go` / `prompt.go` |
| 加载层 | 模型用 read 分页读正文，按需消费 | `read.go` / `constant.go` 模板 |

先看技能文件本身长什么样，`cmd/skilltest/testdata/skills/greeting/SKILL.md`：

```markdown
---
name: greeting
description: 当用户要求打招呼、问好或执行问候流程时使用本技能。按固定步骤输出问候语。
---

# 打招呼技能

按以下步骤执行问候：

1. 向用户输出一句中文问候语。
2. 报告当前工作目录中可见的文件数量（可用 bash 统计）。
3. 以「技能执行完毕」结束回复。
```

一个技能就是一个目录加一份 `SKILL.md`：frontmatter 里只有 `name` 和 `description` 两个字段是必需的，description 是模型判断"要不要用这个技能"的唯一依据，所以要把触发时机写清楚——"当用户要求……时使用本技能"，而不是只写功能名。

### 详细代码参考

#### 1. 解析器：守门规则都在入口处

解析在 `pi/skills/parser.go`，全部校验集中在一个函数里：

```go
var skillNamePattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

func parseToSkill(content []byte) (*parsedSkill, error) {
	if bytes.IndexByte(content, 0) >= 0 {
		return nil, pierrors.ErrSkillBinaryContent
	}
	if !utf8.Valid(content) {
		return nil, pierrors.ErrSkillNotUTF8
	}

	normalized := strings.ReplaceAll(string(content), "\r\n", "\n")
	lines := strings.Split(normalized, "\n")
	if len(lines) == 0 || lines[0] != "---" {
		return nil, pierrors.ErrSkillFrontMatterMissing
	}

	closing := -1
	for index := 1; index < len(lines); index++ {
		if lines[index] == "---" {
			closing = index
			break
		}
	}
	if closing == -1 {
		return nil, pierrors.ErrSkillFrontMatterUnclosed
	}

	var metadata skillFrontMatter
	if err := yaml.Unmarshal([]byte(strings.Join(lines[1:closing], "\n")), &metadata); err != nil {
		return nil, pierrors.ErrSkillFrontMatterInvalid
	}

	name := strings.TrimSpace(metadata.Name)
	if name == "" {
		return nil, pierrors.ErrSkillNameMissing
	}
	if utf8.RuneCountInString(name) > 64 || !skillNamePattern.MatchString(name) {
		return nil, pierrors.ErrSkillNameInvalid
	}

	description := strings.TrimSpace(metadata.Description)
	if description == "" {
		return nil, pierrors.ErrSkillDescriptionMissing
	}
	if utf8.RuneCountInString(description) > 1024 {
		return nil, pierrors.ErrSkillDescriptionTooLong
	}
	if !isValidXMLText(description) {
		return nil, pierrors.ErrSkillFrontMatterControlChars
	}

	body := strings.TrimSpace(strings.Join(lines[closing+1:], "\n"))
	if body == "" {
		return nil, pierrors.ErrSkillBodyEmpty
	}

	return &parsedSkill{
		Name:                   name,
		Description:            description,
		DisableModelInvocation: metadata.DisableModelInvocation,
	}, nil
}
```

几条规则的来源说明一下：

1、name 限定小写字母、数字和中划线，最长 64 字符。技能名会出现在提示词里、被模型拿来说话，还会被 shell 场景当目录名，松散的命名迟早出事；
2、description 限长 1024 字符，这是渲染层的长度保证——目录里每个技能的描述是有上限的，渲染时就不用再截断；
3、description 里不允许 XML 控制字符，因为这份文本最终要嵌进 XML 格式的技能目录，非法字符在入口处就拦掉。

frontmatter 之外的正文只要求非空。技能写得怎么样是质量的事，格式守不住才是系统的事。

#### 2. 发现：三个来源，单个技能的问题不拖累别人

发现逻辑在 `pi/skills/discovery.go`。来源按优先级排好：

```go
var skillSources = []Source{SourceWorkspace, SourceAgents, SourceClaw}
```

三个约定目录依次是 `skills/`、`.agents/skills/`、`.claw/skills/`，切片顺序就是同名技能的覆盖顺序。扫描用 `os.Root` 圈在工作区内，软链接和非普通文件直接跳过，单个文件限长 256 KiB：

```go
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

	content, err := readLimitedSkillFile(root, filepath.FromSlash(path))
	if err != nil {
		diagnostic := sentinelDiagnostic(location, SeverityWarning, pierrors.ErrSkillFileUnreadable)
		return Summary{}, &diagnostic, false
	}

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
```

关键在返回值的设计：每个技能独立判定，"accepted 的进 Summary，被淘汰的进 Diagnostic"，一个技能的 frontmatter 写坏了只产生一条 warning 诊断，同一目录下其他技能照常生效。诊断信息带稳定错误码（`Code`）和人类可读的 `Message`，上层想打日志、想展示给用户，都有据可查。

#### 3. 同名裁决：宁可名字空缺，也不静默换实现

三个来源可能撞名，裁决规则在 `mergeDiscoveredSkills`：

- 同一 name 跨来源重复：优先级最高的来源胜出，其余候选记 `ErrSkillShadowed`；
- 同一来源内部同名：无法裁决正确版本，这个 name 整体作废，全部候选记 `ErrSkillDuplicateName`；
- 已占用或已作废的 name，后续来源里的候选不参与补位，统一记 `ErrSkillShadowed`。

第三条最值得说。撞名的候选作废之后，如果允许低优先级来源里的第三个同名技能"补位"顶上，用户看到的现象就是：明明写了 A，生效的却是谁也说不清的 C。补位看似聪明，实际是把覆盖关系变成了一笔糊涂账。这条规则翻译成一句话就是注释里那句：宁可名字空缺，也不静默换实现。

裁决完产出 `Snapshot`，快照在创建时就复制了切片，调用方后续怎么改原切片都影响不了它。同时给每个技能算了一个内容指纹：

```go
Version: fmt.Sprintf("sha256:%x", digest[:8]),
```

`SKILL.md` 内容一变，版本就变。这个版本会跟着渲染进 System Prompt，提示词里配套了一条纪律："如果 <version> 与之前看到的版本不同，必须重新读取该技能"——长会话里技能被人改了，模型不会拿着旧版本的内容继续干活。

#### 4. 组装：核心纪律 + 技能目录 + AGENTS.md

组装在 `pi/prompt.go`：

```go
func SystemPrompt(ctx context.Context, workDir string) (string, error) {
	agents, err := loadAgents(workDir)
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}

	snapshot, err := skills.Discover(workDir)
	if err != nil {
		return "", pierrors.ErrWorkspaceInvalid.Wrap(fmt.Errorf("发现 Agent Skills 失败: %w", err))
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}

	var builder strings.Builder
	builder.WriteString(corePrompt)

	if skillPrompt := snapshot.Render(); skillPrompt != "" {
		builder.WriteString("\n")
		builder.WriteString(skillPrompt)
	}

	builder.WriteString("\n# Agent 定义（来自 AGENTS.md）\n\n")
	builder.Write(agents)
	builder.WriteString("\n")

	return builder.String(), nil
}
```

注意这里没有缓存：每轮 Run 重新读 AGENTS.md、重新发现技能。代价是每次一次目录扫描，换来的是技能内容随工作区实时变化——往 `skills/` 里加一个目录，下一轮对话立刻可用。对一个跑在用户工作区里的 Harness 来说，这个取舍是值得的。

技能目录不是光秃秃地贴进去的，前面挂着一段使用纪律（`pi/skills/constant.go`）：

```go
const skillPromptInstructions = `
# 可用专业技能 (Agent Skills)
以下技能为特定任务提供专业执行指南。
当任务与某项 <description> 匹配时，必须先使用 read 读取该技能的 <location>。
如果 read 返回 "Use offset=N to continue"，必须继续读取，直到完整取得 SKILL.md 后再执行。
不要猜测未读取的技能内容，不要读取明显无关的技能。
如果 <version> 与之前看到的版本不同，必须重新读取该技能。
相对路径引用以 SKILL.md 所在目录为基准解析。
`
```

这段话和渲染出来的 XML 目录是配套的，它把"技能怎么用"的契约讲清楚：描述匹配才读、读到完整再执行、不猜没读过的内容。模型不守纪律是提示词工程的问题，但契约本身必须先写明白。

#### 5. 渲染：转义和净化缺一不可

渲染在 `pi/skills/render.go` 和 `summary.go`，每个技能输出一个 `<skill>` 节点：

```go
func (s Summary) renderEntry(description string) string {
	return fmt.Sprintf(
		"  <skill>\n    <name>%s</name>\n    <description>%s</description>\n    <location>%s</location>\n    <version>%s</version>\n  </skill>\n",
		xmlTextReplacer.Replace(sanitizeXMLText(s.Name)),
		xmlTextReplacer.Replace(sanitizeXMLText(description)),
		xmlTextReplacer.Replace(sanitizeXMLText(s.Location)),
		xmlTextReplacer.Replace(sanitizeXMLText(s.Version)),
	)
}
```

技能的 name 和 description 是用户写的自由文本，直接拼进 XML 目录，一个 `<` 就把格式砸了。所以每个字段过两道工序：`sanitizeXMLText` 把 XML 1.0 不允许的字符替换成 `�`，`xmlTextReplacer` 把五个保留字符（`& < > " '`）替换成实体形式。两道工序分工不同：前者处理"这个字符 XML 根本不认"，后者处理"这个字符会改变 XML 结构"。

长度这块渲染层什么都不做——description 超过 1024 字符的技能在解析时已经被拒了，渲染期不截断，只过滤和转义。校验的职责收在入口，越往后越薄。

#### 6. 正文按需加载：read 的分页契约

目录进了 System Prompt，正文还在磁盘上。模型读到匹配的 description 后，用 `read` 工具按路径读取，`pi/tools/impl/read.go`：

```go
const (
	maxReadLines = 2000
	maxReadBytes = 50 * 1024
)

text := strings.Join(selected, "\n")
if more || hasContent(lines[cursor:]) {
	end := offset + len(selected) - 1
	text += fmt.Sprintf("\n\n[Showing lines %d-%d. Use offset=%d to continue.]", offset, end, end+1)
}
```

一页最多 2000 行、50 KiB，超出就在末尾附上 `Use offset=N to continue`。这个约定和第 4 节提示词里的那句"必须继续读取，直到完整取得 SKILL.md"是同一个契约的两端：工具负责把"后面还有"说清楚，提示词负责要求模型读到底。半截技能正文去执行任务，比不执行还危险。

`readCapped` 还有一个防御细节：无论 offset 是多少，最多只扫描文件前 50 KiB，offset 落在扫描范围之后时不静默返回空，明确报"超出单次读取的扫描范围"。对模型来说，一个空结果和一个"你读不到"的错误是两回事——前者会让它以为技能是空的。

## 跑起来看

`cmd/skilltest` 是这个域的最小验证端子。先离线检查系统提示词组装（`-check-only` 不调模型）：

```
=== 系统提示词检查 ===
workdir: /Users/allen/projects/work/github/go-harness/cmd/skilltest/testdata
系统提示词总长度: 1814 字符

--- 技能目录 ---
<available_skills>
  <skill>
    <name>go-env-report</name>
    <description>当用户询问 Go 环境信息、版本或构建环境配置时使用本技能。收集 Go 工具链信息并生成环境报告。</description>
    <location>skills/go-env-report/SKILL.md</location>
    <version>sha256:81e6cfee42b852a7</version>
  </skill>
  <skill>
    <name>greeting</name>
    ...
</available_skills>
```

四个测试技能全部进入目录。然后把任务"如果存在与「打个招呼」相关的技能，先用 read 完整读取它的 SKILL.md，再按技能内容执行"交给完整 loop：

```
=== deepseek (openai / deepseek-chat) ===
[assistant] I'll check the available skills and read the greeting skill's SKILL.md in full.
            └ call read {"path": "skills/greeting/SKILL.md"}
[tool:read] result: --- …
[assistant] SKILL.md 已完整读取，现在执行技能第 2 步（统计当前工作目录可见文件数量）。
            └ call bash {"command": "ls -1 | wc -l"}
[tool:bash] result: 2
[assistant] 你好！很高兴见到你，欢迎使用 go-harness 测试环境。

按照打招呼技能的要求，我统计了当前工作目录中的可见文件数量：**2 个**。

技能执行完毕。
```

对照流程图看这条链路：模型先在目录里找到 `greeting` 的 `<location>`，用 read 读完整 SKILL.md，然后严格按技能正文的三步执行——问候、统计文件数、以「技能执行完毕」收尾。技能正文里的每一步都落了地，没有一步是模型自由发挥的。

整个过程里 `pi/skills` 没有执行任何技能逻辑，它只是把素材准备好：让模型在 1814 字符的 System Prompt 里看到四个技能的"简历"，正文留在磁盘上按需读取。

## 总结

回到流程图的三段。发现层把"会什么"从代码挪到工作区的 markdown：三个来源目录按优先级覆盖，解析规则全部收在 `parseToSkill` 一个函数里，单个技能的问题只产生诊断，不拖累别人；同名裁决宁可空缺也不静默换实现。

渲染层只往 System Prompt 里放 name / description / location / version 四个字段，转义和净化保证用户写的自由文本砸不了目录的格式；`Version` 指纹配合提示词纪律，长会话里技能改动也能被模型感知。

加载层把正文留在磁盘上，模型用 read 分页读取，`Use offset=N` 的工具契约和提示词纪律互为表里。

到这里，System Prompt 的两大素材（AGENTS.md 和技能）都齐了，但模型跑得越久，真正吃上下文的是对话历史和工具输出。下一篇写上下文工程：`pi/context` 怎么组装每轮的上下文、超限之后的压缩策略。感兴趣的话关注一下，防止走丢。