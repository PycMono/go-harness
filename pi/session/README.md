# pi/session 设计文档

Session 上下文管理：持久化"客户说了什么、聊了什么"，并在后续运行中重建模型上下文。

参考实现：pi.dev（[badlogic/pi-mono](https://github.com/badlogic/pi-mono)）的 session 机制，核心源码在 `packages/coding-agent/src/core/session-manager.ts`（约 1900 行）与 `packages/coding-agent/src/core/compaction/`（约 1000 行）。本文档先概括它的做法，再给出映射到 go-harness 的方案。

## 现状与问题

当前 go-harness 没有会话持久化：

- `RunInput.History` 由调用方每轮自己带进来，`RunOutput.Messages()` 返回就丢；
- `pi/context.go` 的 `ContextBuilder.Build` 每次从零组装上下文；
- 剪枝（prune）与压缩（compaction）此前只有一句预留（原 `pi/context/` 脚手架，已删除）；Phase 1 都不做，落点定在 `pi/session`（见"压缩"）。

也就是说"客户聊了什么"现在存在调用方手里：进程重启即丢，压缩无从谈起。session 层要补的就是这块存储与恢复。

## pi.dev 的实现概括

1、一个会话一个 JSONL 文件，只追加。首行是 session header（`type/id/timestamp/cwd/parentSession/version`），之后每行一条 entry，逐条 `appendFileSync` 落盘；文件按工作目录编码存放：`~/.pi/agent/sessions/--编码后的cwd--/<ISO 时间>_<会话 id>.jsonl`（名字里带时间戳是为了目录倒序排就是按时间倒序，第 10 条；早先这里写成 `<uuidv7>.jsonl`，漏了前缀）。

2、每条 entry 携带 `id`（8 位随机）与 `parentId`，追加历史是线性的，但通过树结构读：从 `leafId` 沿 `parentId` 走到根再反转。这使"编辑后重发"的分叉天然成立——分叉只是换了片叶子，历史一行不改。

3、entry 类型分明：`message`（对话消息）、`compaction`（压缩边界）、`branch_summary`（分叉摘要）、`custom`（扩展存状态，不进模型上下文）、`custom_message`（扩展注入，进上下文）、`model_change` / `usage` / `label` / `session_info` 等设置类。参与不参与上下文是每类 entry 自己的属性。

4、上下文重建是纯函数：`buildSessionPath`（叶子追根出路径）→ `buildContextEntries`（找路径上最后一个 compaction，用它的 `summary` 替代之前的消息，从 `firstKeptEntryId` 起保留其后的消息）→ 转 LLM 消息。存储形态与"喂给模型什么"是两层，互不纠缠。

5、崩溃安全：读取按行解析，坏行直接跳过。进程崩在写一半时残行被忽略，此前历史完好。

6、压缩触发：`tokens > contextWindow - reserveTokens`（默认 reserve 16384、keepRecent 20000）；token 估算 = 最后一次真实 usage + 其后消息 chars/4 保守估计。

7、存储可插拔：默认文件实现之外还有 `packages/session-backends/sqlite-node`。

8、harness 层（`packages/agent/src/harness/agent-harness.ts`）：session 挂在**构造**参数上且必填——`AgentHarnessOptions.session: Session`（:519），注释明写"一个 harness 绑一个已打开的 session"（:621）；`prompt` 只收本轮输入，**没有任何历史入参**（:550-551）；调用方自带历史时靠公开的 `appendMessage`（:543）写进会话，而不是另开一个"传历史"的入口。不落盘的会话不是另一个类，是同一个 `SessionManager` 的 `persist=false` 形态（`static inMemory()`，session-manager.ts:1662）。这三点合起来就是：**history 只有会话这一个来源，装不装盘只是同一个来源的两种形态。**

本方案的 `pi.Options.Session` 必填、删掉 `RunInput.History`、`session.InMemory()` 就是照这三点落的（见"接入点"）。

9、文件里的一行有两种：`FileEntry = SessionHeader | SessionEntry`（session-manager.ts:178）——首行是 `SessionHeader`（:40-48，字段 `type/id/timestamp/cwd/parentSession/version`），与 entry **平级**，不是 entry 的一个种类；`SessionEntry` 是 10 个接口的判别联合（:165-176），每类的字段长在自己身上（如 `CompactionEntry` 的 `summary` / `firstKeptEntryId` / `tokensBefore`，:88-102），没有"一个通用 entry 带若干可空载荷"的写法。`version` 与 `parentSession` 都是真在用的：`CURRENT_SESSION_VERSION = 3`（:38），v1 的文件没有 `version` 字段；`parentSession` 由 `NewSessionOptions` 写入（:49-52）。

10、id 分两层，两层都不靠概率。会话 id 是 `uuidv7()`（`createSessionId`，session-manager.ts:230-232；实现在 `packages/ai/src/utils/uuid.ts:8-45`：48 位毫秒时间戳在前，普通调用时对上一个时间戳取 `max`，时钟回退也不会让 id 倒退）；文件名是 `${ISO 时间的 : 与 . 换成 -}_${sessionId}.jsonl`（:990-991，fork :1702-1703），所以会话列表按文件名倒序排就是按创建时间倒序（:863），唯一性全交给 id 那一段。entry id 只有 8 位十六进制（32 bit，比我们的 41 bit 还小），但生成时对着会话的 `byId` 查重、重试 100 次、兜底返回完整 UUID（:243-250，`byId` 声明在 :906）；首行落盘用 `openSync(file, "wx")`（:1086）独占新建，重名直接抛错，之后的写入才是 `appendFileSync`（:1090）。

## 目标与非目标

目标（Phase 1）：

- 会话消息持久化：本轮 input、每轮 assistant 消息、工具结果按发生顺序追加落盘；
- 跨进程重建：下次 Run 的 history 从 session 重建，调用方不再自己维护 History；
- 接上既有装配点：`Agent` 必填接入会话（`pi.Options.Session`）。单轮运行用 `session.InMemory()`，要跨轮/跨进程就用文件会话，按会话键 `session.OpenOrCreate(root, workDir, 键)` 取（第八轮，键由调用方给）；`RunInput.History` 随之删除——history 只认会话这一个来源。

非目标（明确不做）：

- 不做压缩：Phase 1 既不实现算法，存储格式里也不留 `compaction` entry——没有任何代码会写出这种 entry，读侧折叠就是一条永不触发的死路。等真做压缩时，entry 类型、读侧折叠与算法一起加回（见"压缩"）；
- Phase 1 不做分支/回放 UI（树结构已在存储里预留，动作 Phase 3）；
- 不做 SQLite 等其他 backend，只留接口位。

## 包设计

新包 `pi/session`，依赖方向：`pi` → `pi/session` → `pi/schema` + `pi/error`。session 不认识 loop、agent、provider。

```
pi/session/
├── entry.go       # Entry 联合类型 + ID / ParentID / 时间戳 + Entries 上的追根与取消息
├── file.go        # sessionFile：JSONL 读写（按行加载、坏行跳过、追加写、单写者长度校验）
├── manager.go     # SessionManager：创建 / 打开 / 追加 / 树查询
├── compaction.go  # 压缩算法：触发判定、切点选择、token 估算（Phase 2，纯函数不碰存储）
└── backend.go     # 存储接口，文件实现为默认（Phase 1 只有文件实现，接口位待 Phase 2 再落）
```

`compaction.go` 与 `backend.go` 是 Phase 2 的落点，Phase 1 不建空文件。

压缩算法独立成 `compaction.go`：它只是"给消息序列和预算，返回切点与被压缩区间"的纯函数，可以拿 entry 快照离线单测，不碰文件。pi.dev 同样把 compaction 独立成目录，不跟 session-manager 混放。原先预留的 `pi/context/` 目录删除：它描述的组装职责早已由 `pi/context.go` 实现，压缩算法归入本包后，"上下文组装（pi/context.go 文件）"与"session 域（pi/session 包）"两个概念不再撞名。

### Entry 模型

对齐 pi.dev 但精简到两类：

```go
type EntryType string

const (
	EntryHeader  EntryType = "session" // 首行
	EntryMessage EntryType = "message" // 对话消息（含工具调用与结果）
)

// Entry 是 session 文件里的一行。
type Entry struct {
	Type      EntryType     `json:"type"`
	ID        string        `json:"id"`                  // 8 位随机 id，会话内唯一
	ParentID  string        `json:"parent_id,omitempty"` // 树结构；Phase 1 全部线性
	Timestamp string        `json:"timestamp"`

	Header  *Header         `json:"header,omitempty"`  // EntryHeader
	Message *schema.Message `json:"message,omitempty"` // EntryMessage
}

// Header 是会话首行。
type Header struct {
	ID       string `json:"id"`
	Version  int    `json:"version"`
	CreatedAt string `json:"created_at"`
	WorkDir  string `json:"work_dir"`
	// ParentSession 预留：fork 出的会话指回父文件路径。
	ParentSession string `json:"parent_session,omitempty"`
}

```

`ParentID` 在 Phase 1 全部串成线性（每条指向上一条），但字段一次到位——pi 也是 v2 迁移才补的树，这里一次到位避免将来的格式迁移。压缩不在这份格式里：`compaction` entry、`Compaction` 结构体与读侧折叠随压缩功能一起去掉了（`Entry` 只有 `Header` 与 `Message` 两个载荷），做压缩时一起加回。

`Header` 的名字与 pi.dev 一致（`SessionHeader`，session-manager.ts:40），字段逐项对应：`cwd` ↔ `WorkDir`、`timestamp` ↔ `CreatedAt`、`parentSession` ↔ `ParentSession`。两处层级差别：一是首行与 entry 的关系——pi.dev 分成 `FileEntry = SessionHeader | SessionEntry` 两条分支，我们用 `Entry.Header` 这个可空指针承载（Go 没有 union，tag + 可空载荷是等价写法，第 9 条）；二是字段的兑现程度——pi.dev 的 `version`（已到 3）和 `parentSession` 都真在读写，我们这边 `Version` 没有任何地方校验、`ParentSession` 恒空。后一条是账：留着是因为改动格式的成本随文件数增长，等会话列表/格式迁移真需要时再兑现，或者届时按"无读者即删"裁掉。字段值与行为无关这件事也写进了 `Entry.validate` 的注释——它只查"载荷在不在"，不看载荷里的值。

`Entry` 之外有一个集合类型 `type Entries []Entry`，代表"一次会话的 entry 快照"。重建上下文和它用到的全部查找都落在这个集合上（见"上下文重建"），接收者恒定、方法不修改自身，仍然是纯函数，可以拿快照离线单测。`Manager.entries` / `Manager.Entries()` / `sessionFile.load` 都用它，不出现裸的 `[]Entry`。

### 文件布局

```
sessions/
└── --编码后的workDir--/            # 路径中的 / 与 : 替换为 -
    └── <会话 id>.jsonl
```

- 目录编码只做分组展示用途，不承担唯一性（同款不可逆替换，pi.dev 亦然）；隔离以 session 文件本身为准，header 里保存原始 workDir。调用方按客户分域时把 sessions 根目录设成每客户一个即可。
- 文件名就是调用方给的会话 id（第八轮起，第九轮把参数语义正名为会话 id）：id 是每个会话自己的身份，一个会话一个文件。要开一次新会话就用 `session.NewSessionID()`（`chat-` 前缀 + 26 位 base32 随机）拿一个 id —— 生成器与打开是两个动作，`OpenOrCreate` 仍然只按 id 找文件，不判断"该不该建"。之前是 `<时间戳>-<8位随机>`，随机源 `crypto/rand.Text()`；改掉的理由是键必须能按业务找回来——崩溃之后只知道"这是客户 42 的会话"，靠时间戳+随机名的文件名什么也定位不到，只能再存一张 id→文件 的表。代价说明白：文件名不再自带时间排序，pi.dev 那种"取最近一个"（`--continue`，靠目录 mtime）在我们这里没有依据了，将来真要做，得读 header 的 `CreatedAt`。
- 键的合法性是 `^[A-Za-z0-9](?:[A-Za-z0-9._-]*[A-Za-z0-9])?$`（借 pi.dev `assertValidSessionId`，session-manager.ts:235），另加 128 字符上限（我们加的：键要当文件名，超长会被文件系统以 `ENAMETOOLONG` 拒掉，不如在参数校验里说清楚）。这道校验同时是路径穿越的防线——键会变成文件名，`../evil`、`a/b` 在正则上就过不去，非 ASCII 的客户名也在这一条被挡下。
- 撞名的兜底是文件系统而不是概率：`sessionFile.create` 独占新建，重名报 `ErrSessionAlreadyExists`(80011)。为什么必须独占：追加式写入不会报错，第二个 header 会被 `load` 当普通 entry 收下，叶子一被顶掉，`pathToRoot` 到它就停——此前的历史静默消失（实测：`BuildMessages()` 返回 0 条消息）。
- 独占的发布方式在第八轮改过：先写同目录下的临时文件、`Sync`、再用 `os.Link` 硬链接到目标路径。原来直接 `O_CREATE|O_EXCL` 建目标文件再写首行，建出文件和写 header 之间有窗口，并发的读侧撞进去会读到 0 字节或者半行 header 的文件，把"别人正在创建"误报成 `ErrSessionFileEmpty`(80004)——比撞名更糟，调用方会以为盘上的数据坏了。`os.Link` 原子，且目标已存在时必然失败（`EEXIST`），独占与原子发布一次拿到：读侧要么看不到文件，要么看到完整的一份。守卫是 `TestOpenOrCreateConcurrentCallersNeverSeePartialFile`（20 轮 × 8 并发，改之前三次运行三次红），以及 `TestCreateRefusesExistingSessionFile` 守着 `EEXIST` 这条（把 `os.Link` 换成 `os.Rename` 会静默覆盖别人的会话文件，该测试立刻红）。
- 写：追加走 `O_APPEND`，不带 `O_CREATE`（文件在 Append 路径上消失会先被 `checkUnchanged` 拦下，不在这里悄悄重建）；每条 entry 一次 `Sync`（不是只 flush 缓冲区——崩溃安全语义要求落盘），一行一个 JSON。
- 单写者契约：一个 session 文件在任一时刻只归一个 Agent 实例写。包内锁只防进程内并发，不防多进程——不做文件锁，防呆靠两道校验：`Append` 要求显式提供的 `ParentID` 必须是当前叶子，且落盘前比对文件长度是否仍等于 `sessionFile.size`。另一个进程写过之后，本进程的下一次 Append 直接失败，错误早暴露而不是静默交错。
- `sessionFile.size` 的取法有讲究：`load` 记的是**这次真正读到的字节数**，`append` 写完后前移。不能读完之后再 `Stat` 一次取长度——两次取长度之间有窗口，别的进程在窗口里追加的内容会被算进基线，此后本进程再也发现不了。
- 读：按行解析，坏行跳过（对齐 pi 的崩溃语义）。
- 首行必须是 header 且必须能解析：首行就是坏的，整份文件拒绝打开（取严格者）；首行之后的坏行一律跳过。
- leaf 规则：线性模式下打开会话后 leaf = 文件里最后一条 entry；将来支持分叉后改为显式传入 leaf，检测到多个叶子而未显式指定时拒绝隐式选择。
- 图像 URL 原样落盘：`schema.Message` 里的 image 块连同 URL 一起进会话文件（回放要能还原图）。仓库里另有 `schema.ImagePlaceholderText`，那是给压缩摘要投影用的脱敏口径，不用于会话存储。

### Manager

```go
type Manager struct{ ... }

func OpenOrCreate(root, workDir, sessionID string) (*Manager, error) // 按键取会话：在就打开，不在就建
func InMemory() *Manager                             // 不落盘的会话
func (m *Manager) Path() string                      // 会话文件路径；内存会话为空
func (m *Manager) Append(entry Entry) error          // 追加，落盘后返回；header 只属于首行，这里拒收
func (m *Manager) Entries() Entries                  // 全量快照（副本）
func (m *Manager) LeafID() string
func (m *Manager) BuildMessages() schema.Messages    // 重建给模型的 history
```

`Append` 的 ID / ParentID 留空即由 Manager 补齐（ID 随机、ParentID 取当前叶子）；显式给了 `ParentID` 就必须是当前叶子（Phase 1 线性约束），防乱序写入。新建会话的叶子起点是 header，第一条消息挂在 header 上。

`OpenOrCreate` 是唯一的取会话入口（第八轮，`Create` 降为包内 `createSession` 不再导出）：键在就打开、不在就用这个键建一个。同一个键调多少次都落到同一个文件，所以调用方不需要判断"该不该建"——每轮调一次即可，也不需要进程级的初始化状态。两个必须分清的语义：一、文件损坏、或者不属于这个工作区时一律拒开、绝不覆盖（`ErrSessionFileInvalid` / `ErrSessionWorkDirMismatch`，后者见下）；二、并发的同键调用里抢输的一方收到 `ErrSessionAlreadyExists` 后**重新打开**，撞名不甩给调用方——但 `O_EXCL`（现在是 `os.Link`）只裁决"谁建"，不裁决"谁能写"，跨进程单写者仍由调用方保证，它不解决并发写。

工作区核对为什么必要：目录名是 `encodeWorkDir` 的有损编码，`/a-b` 和 `/a/b` 编到同一个目录，键又由调用方给，同一个文件路径可能被两个工作区同时指到；不核对的话，两边会把彼此的会话当成自己的（`TestOpenOrCreateRefusesWorkDirMismatch`）。

0 字节文件**不自愈**，报 `ErrSessionFileEmpty`。pi.dev 遇到空文件会重写一个合法 header（session-manager.ts:938-947），这条我们选了相反的一边：空文件只可能来自崩了一半的创建，静默重写是在赌"那不是我正在写的文件"——而 pi.dev 之所以必须自愈，是因为它懒写、空文件在它的世界里是正常中间态；我们即时写，它是异常。守卫是 `TestOpenOrCreateRefusesEmptySessionFile`。

落盘与否不是两个类型：`InMemory()` 拿到的是 `file == nil` 的实例，`Append` / `BuildMessages` / `Entries` 走完全相同的代码路径，只有 `write` 跳过文件 IO、单写者校验直接放行。这样上层装配不必为"这轮要不要留档"分叉，对齐 pi.dev 的同一个 `SessionManager` 两种形态（`private persist: boolean`）。`nil` 而不是 `persist bool`：要不要落盘和"文件在哪、写到第几字节"本来就是同一件事的两面，`file` 有了，长度也有了；再放一个布尔开关就等于允许"有文件但不写"这种不存在的状态。内存会话没有文件也没有 header：链条的根就是第一条 entry（`ParentID` 为空）。

#### 存储层：`sessionFile`

会话文件本身收成一个结构体——路径，加上本进程最后一次与它对齐时的长度。散成自由函数，长度就得在调用方手里传来传去（原来是 `Manager` 自己存了一份）：

```go
// Manager 用 nil 表示内存会话，所以 sessionFile 不需要"要不要落盘"的开关。
type sessionFile struct {
	path string
	size int64 // 最后一次读到 / 写完时文件应有的长度
}

func (f *sessionFile) load() (Entries, error)      // 读全量 entry，基线取读到的字节数
func (f *sessionFile) create(entry Entry) error     // 独占新建并写首行（临时文件 + 硬链接发布）
func (f *sessionFile) append(entry Entry) error     // 追加一行并前移基线
func (f *sessionFile) checkUnchanged() error        // 长度变了说明别人写过
func (f *sessionFile) invalid(cause error) error    // 文件不可用，挂稳定码
```

`load` 的两个错误分流是调用方的分叉点：文件不存在报 `ErrSessionNotFound`（80003，调用方据此去新建）；其余读不出来的原因（目录、权限不足）一律归 `ErrSessionFileInvalid`（80000）——后者必须上报，当成"没有会话"去静默新建就吞掉了真正的故障。`os.Stat` 没有一并收进来：`append` 里那次是写完后取自己刚写的长度，`checkUnchanged` 里那次是写前探别人的动静，错误语义不同，刻意分开。

### 上下文重建

重建是两步：`Entries.pathToRoot` 从叶子沿 `ParentID` 追到根，`Entries.messagesOf` 取出路径上的对话消息。两步都是 `Entries` 上的方法（包外通过 `Manager.BuildMessages()` 走一遍）：

```go
func (m *Manager) BuildMessages() schema.Messages {
	return m.entries.pathToRoot(m.leafID).messagesOf()
}
```

方法而不是自由函数：接收者本来就是同一份快照，收敛后不出现"把 `[]Entry` 当第一个参数传来传去"的写法。仍然是纯函数——接收者不变、方法也不修改自身，输入 entry 快照、输出 `schema.Messages`，与存储实现无关。一个边角语义：链断在中间（中间某行坏掉）时保留从叶子走得通的一段，而不是丢掉整条路径。

## 接入点

session 是横切能力，但仓库里真正需要动的位置只有下面几处。原则：`pi/session` 自己不认识任何上层，所有接线在 `pi`（agent 层）完成，`pi/tools`、`pi/context.go` 的组装逻辑全部不动（`context.go` 只修了 `CurrentInputIndex` 的指向，见下）。

### 0. 调用方：每轮 Run 之前按键取会话（第八轮）

调用方（业务侧）的完整序列，三行，没有第四行：

```go
manager, err := session.OpenOrCreate(root, workDir, sessionKey) // sessionKey 由调用方给（客户 id、工单号…）
agent, err := pi.NewAgent(&pi.Options{WorkDir: workDir, ProviderOptions: opts, Session: manager})
output, err := agent.Run(ctx, &pi.RunInput{Input: msg})
```

要点四条：

- **id 由调用方给，含义也由调用方定**。默认立场是"不替调用方生成 id"：id 若由库生成，业务方就得把返回值存进自己的库（多一次写入、多一处不一致），下次请求才能拿它找回来；用业务本来就有的那个（工单号、客户 id），这层存储就省掉了。真需要"随便开一次新会话"，用 `NewSessionID()` 现拿一个随机 id（第九轮补的生成器），拿到的返回值仍需调用方自己记着——`OpenOrCreate` 不会替你保存它。
- **每轮取一次是安全的**，因为 `OpenOrCreate` 幂等：键第一次用是新建，以后是续上。所以调用方不需要判断"该不该建"，也不需要进程级初始化——这正是 `Create` 被降为内部函数的原因（`Create` 把"想要会话"和"现在就写文件"绑在一起，调用时机只能靠调用方自己查文件来决定）。
- **会话可以在一个 Manager 上续多轮**（`pi` 侧的 `Run` 不接会话参数，注入点在装配期）。这与 pi.dev 的差别只在装配的寿命：pi.dev 是 1 个会话 ↔ 1 个 harness ↔ N 轮 `prompt`（长活），我们是每轮新建 agent、会话在它上面续（无状态 worker）。代价是每轮重装工具与循环，换来的是调用方不用管实例生命周期。
- **单写者归属仍在调用方**：同一个键在一个时刻只能有一个写入者，`OpenOrCreate` 不解决并发写（见 Manager 一节）。

### 1. `pi/agent.go` + `pi/loop.go` —— 事件发生时逐条追加

`pi.Options` 新增（必填）：

```go
// Session 是会话管理器，必填。history 只认这一个来源：本轮运行前的历史
// 从会话重建，本轮产生的消息逐条写回会话。
Session *session.Manager
```

追加时机对齐 pi.dev 的逐条 append，不在 Run 结束后批量扫描结果——模型调用中途崩溃时，已发生的消息必须已经在盘上：

1. 装配期校验 session 非空，缺了直接报 `ErrInitialization`——不设"没有会话"的第二条路径，否则 history 又会冒出来源；
2. `history = a.session.BuildMessages()`（`Agent.history`），交给 `ContextBuilder.Build`（现有逻辑不变）；
3. **然后**才 Append input 消息——顺序不能反：先追加再重建的话，这条输入会既在重建出来的历史里、又被 `ContextBuilder.Build` 当作本轮输入追加一次，模型看到两遍同样的话。追加仍在任何模型调用之前（Agent 本来就持有这条消息，不存在边界推断问题；`Context.CurrentInputIndex` 不作为 session 依据，它的指向问题另行修正）；
4. assistant 消息与工具结果在产生时逐条 Append。为此 `loop.go` 加一个可选回调：

```go
// WithMessageObserver 接收循环逐条产生的模型消息与工具结果消息；
// 不设置时行为不变。与 TextObserver 一样在单线程控制流中同步调用。
func WithMessageObserver(observer func(*schema.Message)) LoopOption
```

Agent 接上 `session.Append`；Run 出错时已产生的 entry 保留（崩溃安全语义：恢复后从最后一次成功写入处继续）。写入失败没有从回调返回错误的通道，所以先攒在 `Agent.writeErr` 上，等 `loop.run` 返回后由 `Run` 透出——否则会出现"消息已经发给模型了、盘上没有"这种最隐蔽的缺段。缓冲落在 Agent 而不是 Run 的局部变量上，是因为循环在构造期建一次、观察者挂死在上面；`Run` 开头把它清零，一个 Run 一轮账。

`loop.go` 的改动只有这一个 hook（`observe` 两处调用点），循环逻辑本身不动。

### 2. `pi/runner.go` + `pi/agent.go` —— 删掉 `RunInput.History`，改用会话承接

`RunInput.History` 删除：history 只认会话这一个来源，调用方不再有第二个入口。原来那条互斥校验与 `ErrSessionHistoryConflict`(80003) 一并消失——校验一个已经不存在的字段没有意义。

调用方自带历史（历史存在自己的库或缓存里）时，自己把业务消息转成 `schema.Message` 再逐条 `manager.Append`——`Message.Message2AI()` 与 `session.Manager.Append` 都是公开的，转换没有第二套口径。这里一度有个 `pi.AppendHistory` 包办这件事，因为只有测试在调、没有真实调用方，已删除（第七轮）。

三种调用方各自的写法：

| 调用方 | 写法 |
|---|---|
| 会话要留档（默认） | 每轮 `session.OpenOrCreate(root, workDir, 会话键)` → `Options.Session`；同一键跨轮、跨进程都续同一个文件 |
| 只想单轮跑一次 | `Session: session.InMemory()`，跑完就丢 |
| 自己另有历史存储 | `session.InMemory()` + 自己转好（`Message2AI`）后逐条 `manager.Append`，之后每轮由 Agent 自己追加 |

pi.dev 那边灌历史也走公开的写入方法 `appendMessage`（agent-harness.ts:543）而不是"传历史"的构造参数，这一点仍然对齐；差别只是我们的对应物降了一层，是 `session.Manager.Append`，不是 `pi` 上的一个包办函数。

### 3. `pi/error` —— 新增哨兵码段

session 域的错误码取 **80000 段**（40000/40001 已被 `ErrCanceled`/`ErrDeadlineExceeded` 占用，70000 段归 skill）：

```
80000  ErrSessionFileInvalid     文件缺 header 或首行损坏，或路径读不出来
80001  ErrSessionAppendFailed    追加写入失败
80002  ErrSessionParentMismatch  ParentID 不是当前叶子，或文件被其他写入者改过
80003  ErrSessionNotFound        会话文件不存在
80011  ErrSessionAlreadyExists   会话文件已存在（Create 独占新建撞名）
```

上面五个答"哪个环节出错"，调用方按它分流。下面八个答"为什么"，挂在上面这些码的 cause 上（`CodeOf` 取环境码、`errors.Is` 问具体原因）：

```
80004  ErrSessionFileEmpty             会话文件为空
80005  ErrSessionHeaderLineInvalid     会话首行不是合法 JSON
80006  ErrSessionHeaderTypeInvalid     会话首行不是 session entry
80007  ErrSessionEntryIDMissing        entry id 不能为空
80008  ErrSessionHeaderPayloadMissing  header entry 缺少 header 载荷
80009  ErrSessionMessagePayloadMissing message entry 缺少 message 载荷
80010  ErrSessionEntryTypeUnsupported  entry 类型不受支持
80012  ErrSessionHeaderNotAppendable   header 只能由 Create 写入
```

分开的理由是同一份 `Entry.validate` 服务于两条路径：坏行被 `load` 丢掉时挂在 80000 下，`Append` 拒收时挂在 80001 下。调用方要问的"哪个环节"和"为什么"不是一个问题，一个码答不了两个。八个原因码跟 `ErrWorkDirRequired` 是同一条路子：声明成值而不是临时字符串，日志与断言才问得出来；同一分界也解释了 10012 `ErrSessionsRootRequired`——它是同一类装配期必填路径参数，只是属于会话存储的根目录（原先借用 `ErrInitialization`，调用方分不出"参数没给全"和"初始化内部失败"）。

80000 与 80003 的区别也是给调用方分流用的：打开时报 80003 可以放心去建一个新会话，报 80000 时不能——那是"有会话但读不出来"，得先查权限和路径。80003 一度被 `ErrSessionHistoryConflict` 占用，那条互斥校验随 `RunInput.History` 删除后，码位归还给打开会话的这个真实分叉。

装配期缺 session 复用 `ErrInitialization`，不单开码：它和"ProviderOptions 为空"是同一类装配错误。

### 4. `cmd/sessiontest` —— 新验证端子

对齐 `cmd/skilltest` 的形态：按会话 id 取会话（`-key`，默认 `chat-001`）→ 第一轮 Run → 再按同一个 id 取一次（等价于换进程）第二轮 Run → 打印两轮的重建上下文，证明"客户说了什么"跨 Run 可回放。会话文件默认落在当前目录的 `testdata/sessions/<工作区编码>/<会话 id>.jsonl`，跑完不删；同一个 id 重复运行就续写同一个文件（`-sessions` 可改，`/testdata/` 已在 `.gitignore` 里）。`-key` 留空则用 `session.NewSessionID()` 现生成一个并印出来，想接着聊就带上那个 id。`-offline` 时不调模型，直接追加消息再重建，用来离线检查存储格式与折叠。`-interactive` 是同一套东西的一轮一轮形态：每行输入一轮、`:q` 结束，每轮都先按 id 重新取会话再交给一个新建的 agent，跑完报一次该文件的 entry 条数与字节数——`-offline -interactive` 可以不调模型地看文件一轮轮变长。这是 Phase 1 的验收方式。

### 5. 各 README 的依赖方向补一行

- `pi/session/README.md`（本文档）：依赖 `pi/schema` + `pi/error`；
- 仓库根 README 的包列表加 `pi/session` 一行。

### 不接入的位置（明确不动的）

| 位置 | 为什么不动 |
|---|---|
| `pi/loop.go` | 只加 `WithMessageObserver` 一个 hook（逐条吐出消息），循环逻辑、退出条件、消息顺序全部不动；session 的读写决策都在 agent 层 |
| `pi/context.go` | `ContextBuilder.Build` 的入参本来就是 history，来源换成 session 后签名不变。`CurrentInputIndex` 的指向与 `Priority` 排序失效是本方案之外的两个既有缺陷，顺手修掉（见下） |
| `pi/tools` / `pi/middleware` | 工具执行域与 session 无关，中间件链只罩工具调用 |
| `pi/skills` / `pi/prompt.go` | System Prompt 组装每轮照常发生，不进 session（技能与 AGENTS.md 随工作区实时变化，写入历史反而会冻结旧内容） |

### 顺手修掉的两个既有缺陷（`pi/context.go`）

接线时发现 `ContextBuilder.Build` 有两处与注释不符，都不属于 session，但都在接线点上，一并修掉，各带一条回归测试：

- `CurrentInputIndex` 原来取 `len(messages)-1`，指向的是最后一个上下文块（没有块时是系统提示词），不是本轮输入——真实下标是 `len(history)`。修好后压缩（Phase 2）才有可靠的"本次真实输入"锚点。
- `Priority` 排序排在副本上、遍历用的却是排序前的另一份副本，导致 `ContextBlock.Priority` 完全没生效，同时还改写了调用方传进来的切片。改成排副本、遍历副本。

### Phase 2 的接入点（压缩）

压缩触发需要调模型生成摘要，所以动作在 agent 层（唯一持有 provider 的地方）：`ContextBuilder.Build` 之后按 usage 判定超限 → 调模型生成 summary → 追加一条压缩 entry → 重建消息。`compaction.go` 出算法（切点、token 估算），`pi/session` 出存储与读侧折叠，agent 出编排。三处各管一段。

## 压缩（Phase 2 概要）

Phase 1 没有压缩，相关的三样东西一起不存在：`compaction` entry 类型、`Compaction` 结构体、读侧折叠（`BuildMessages` 现在只是"追根 + 取消息"）。做压缩时按这个顺序加回，缺一样都跑不起来：

1. 存储：`Entry.Compaction` 载荷 + `EntryCompaction` 类型 + `validate` 的对应分支与原因码（原 80010 已让给 `ErrSessionEntryTypeUnsupported`，届时取下一个空位）；
2. 读侧：`BuildMessages` 在追根之后按路径上最后一个压缩边界折叠——summary 替代边界之前的历史，`FirstKeptEntryID` 起的消息原样保留（pi.dev 同款，session-manager.ts:455-472）；
3. 算法：`compaction.go`，纯函数，不碰存储，骨架照抄 pi.dev——

- 触发：`tokens > contextWindow - reserveTokens`；默认 reserve 16384、keepRecentTokens 20000，内部常量，不暴露配置；
- 预算边界约束：`keepRecentTokens < contextWindow - reserveTokens` 必须成立，构造时校验；小窗口模型按比例收缩（先缩 keepRecent，仍不满足则缩 reserve），避免保留区本身超过可用预算；
- token 估算：最后一次真实 `Usage` + 其后消息 chars/4。chars/4 只是保守启发式（偏高估），只用于"要不要压、从哪切"的判定，不代表真实 token 数；
- 切点：keepRecentTokens 预算内最早一条消息；更早的（含上一个压缩摘要）调模型压成 summary，写进压缩 entry，`FirstKeptEntryID` 指向切点。

分工：`compaction.go` 出算法，`pi/session` 其余部分出存储与折叠，agent 出编排（持有 provider，触发判定后调模型生成 summary 再写入）。

## 与 pi.dev 的差异

| 项 | pi.dev | 本方案 | 原因 |
|---|---|---|---|
| 目录归属 | 跟随 cwd（终端单用户） | sessions 根目录可配，默认按 workDir 编码 | 业务服务的会话归属由调用方语义决定（可按客户分），不能写死 |
| entry 类型 | 10 种 | 2 种（`session` 首行、`message`） | 业务场景用不到 thinking_level / label 等，需要时再加。pi.dev 的 `compaction` 与另外 7 种的区别是要么属于压缩（Phase 2 一起加回）、要么属于分叉（Phase 3）；留着写不出这种 entry 的读侧逻辑是死代码 |
| 树结构 | v2 起支持分叉 | 存储层预留，动作 Phase 3 | parentID 成本极低，避免将来格式迁移 |
| 后端 | 文件 + SQLite | 仅文件，留接口位 | 先验证语义，再考虑替换 |
| 一条会话内的多路对话 | `lane`：`AgentHarness.lane(name)`，`AgentLane` 带 `steer` / `followUp` / `nextRun` | 无，留给 Phase 3 的树/分叉 | lane 的语义只看了签名、没有细读实现，不照搬 |
| 首行与 entry 的表达 | 每类 entry 一个接口组成判别联合；首行 `SessionHeader` 与 entry 平级（`FileEntry = SessionHeader 或 SessionEntry`，:178） | 单结构体 + 可空载荷指针：`Entry{Type, Header, Message}` | Go 没有 union，tag + 可空载荷是等价写法；名字与字段逐一对应，见第 9 条 |

反过来说，"session 必填 + history 只有一个来源 + 不落盘是同一个类型的另一种形态（`file == nil`，即 pi.dev 的 `persist=false`）"这三点是对齐 pi.dev 的，不列入差异表。

## 实施顺序

1、`entry.go` + `file.go`：模型与读写，含坏行跳过、首行 header 校验的单元测试；
2、`manager.go`：追加约束与重建，Phase 1 完成；
3、`pi/agent.go` 接线（含 Run 进入即追加 input）+ `pi/loop.go` 加 `WithMessageObserver` + cmd 下加最小验证端子（类似 `cmd/skilltest`）；
4、Phase 2（压缩）、Phase 3（分叉）另行评审。

以上 1–3 已完成。测试落在 `pi/session/*_test.go`（entry/file/manager 三块）与 `pi/session_test.go`（agent 接线）。

**评审后的一次收敛**（第 3 步之后）：原方案让 `Options.Session` 可选、`RunInput.History` 保留，两者同时提供时以 `ErrSessionHistoryConflict` 拒绝。这条"两个开关 + 一条互斥校验"被拿掉了——history 只认会话一个来源、会话改成必填、`RunInput.History` 与 80003 删除，调用方自带历史改走 `pi.AppendHistory`。改动的依据是第 8 条列出的 pi.dev harness 三点（会话构造期必填、`prompt` 无历史入参、`appendMessage` 是唯一的灌注口）。删掉的是概念而不是能力：无状态多轮与"调用方自己存历史"这两种用法都还在，只是都走会话这一条装配路径。相应地，"不接 session 时行为与现在完全一致"这条回归守卫随该分支一起删除，替代它的是 `TestNewAgentRejectsMissingSession`（缺会话在装配期报错）与 `TestRunContinuesFromSeededInMemorySession`（灌入历史后接着聊）。

**结构收敛**（同一次评审的第二轮）：`BuildMessages` / `pathToRoot` / `messagesOf` / `lastCompactionIndex` / `indexOfEntry` 五个自由函数收敛成 `Entries` 上的方法（`pi/session/context.go` 并入 `entry.go`），读写三个函数收成 `sessionFile` 结构体（`file == nil` 即内存会话，取代 `persist` 开关）。基线取法同时修正：改成"`load` 读到的字节数"，去掉 `Open` 里读完之后的那次 `os.Stat`——两次取长度之间有窗口，别人的追加会被算进基线。这轮改动没有动对外行为，守卫是 `entry_test.go` 的五个重建测试（收敛前后同一份断言）与 `file_test.go` 的基线测试（两处 `f.size` 赋值分别摘掉，测试各自失败）。

**错误码收敛**（第三轮）：包内不再有裸 `fmt.Errorf` 当返回值——`Entry.validate` 的五个原因、`load` 的三个首行原因各有一个稳定码（当时是 80004–80011，压缩回退后为 80004–80010），`sessionFile.invalid` 收 error 并用 `%w` 保住原因链，`Create` 的空根目录从 `ErrInitialization`(10001) 换成 `ErrSessionsRootRequired`(10012)。对外可见的变化只有一处：`Create("")` 的码从 10001 变成 10012，调用方若按 10001 分流会感知到。守卫是六条新测试（空文件、首行非 JSON、首行缺 id、缺载荷、未知类型、空 root）加 `TestOpenRejectsFileWithoutHeader` 上补的一条原因断言（首行不是 session）。破坏实验是把 `invalid` 的 `%w` 换成 `%v`——四条断言同时红，证明测的是原因链而不是容器码。

**id 与新建的保障**（第五轮，对齐 pi.dev 的三处）：一、`newEntryID` 收下调用方的查重函数（`Entries.hasID`），候选撞了就重试（100 次，pi.dev 同款），仍撞退到整串随机——pi.dev 的 `generateId(byId)` 也是这个形状（session-manager.ts:243-250），它的 id 只有 32 bit，靠的不是位数而是查重；二、`sessionFile.create` 用 `O_CREATE|O_EXCL` 独占新建（pi.dev 首行落盘用 `openSync(file, "wx")`，session-manager.ts:1086），撞名报 `ErrSessionAlreadyExists`(80011) 且不碰已有内容——第八轮把这里的发布方式换成了临时文件 + `os.Link`，独占语义与原因码不变；三、`Create` 只读一次钟（该函数第八轮降为包内 `createSession`），`Timestamp` / `CreatedAt` 来自同一个 `time.Time`（之前是三次 `time.Now()`；文件名不再带时间戳）。顺带删掉手写的 base36 字母表与 `% len(alphabet)` 取模，随机源换成 stdlib 的 `crypto/rand.Text()`，`newEntryID` / `newSessionID` 的 `(string, error)` 签名与调用方那几处不会失败的错误分支一起消失。同时堵掉同一条漏洞的第二个入口：`Append` 拒收 header（`ErrSessionHeaderNotAppendable`(80012)），`write` 里原来"header 不进快照"的特例随 `Create` 改走 `create` 一起删除。守卫是三条新测试（查重退到长 id、撞名报 80011 且文件不变、Append 拒收 header），逐条破坏验证：去掉 `!exists(id)` 判断、去掉 `O_EXCL`、删掉 header 守卫，各自只让对应那条红。

一处如实说明：第三条（一次读钟）没有测试。RFC3339 是秒精度，老代码那三次 `time.Now()` 只有跨秒才看得出差别，写出来的断言在老代码上也不会红，所以它是改结构而不是改行为——三个字符串现在来自同一个变量，靠的是代码本身而不是测试。

**压缩回退**（第四轮）：`Entry.Compaction` 载荷、`EntryCompaction` 类型、`Compaction` 结构体、读侧折叠（`buildMessages` 的边界分支、`lastCompactionIndex`、`indexOfEntry`、`summaryMessage`、`compactionSummaryPrefix`）与 `ErrSessionCompactionPayloadMissing`(80010) 一起删掉；`ErrSessionEntryTypeUnsupported` 从 80011 收成 80010，码段不留洞。理由是没有任何代码能写出这种 entry：`Append` 只认 header 与 message，折叠分支永远走不到，留着等于用一段没人跑的代码声称格式已经支持压缩。代价写在明处——将来做压缩时 entry 类型、读侧折叠、算法一起加回（见"压缩"），那是一次格式扩展。这轮没有对外行为变化，守卫是全库编译与 `go test ./...`：删除本身写不出测试，`pi/session` 的守卫测试随压缩用例一并删掉（第五轮又加回三份与压缩无关的），能覆盖它的只剩 `pi` 包的接线测试。

**文件收敛**（第六轮）：`pi/sessionwriter.go`（`sessionWriter` 类型与三个方法）与 `pi/history.go`（`AppendHistory`）删除，内容并回 `pi/agent.go`。两处都是只在一个装配点用一次的胶水：writer 干的活是"把 `session.Append` 接上观察者、接住错误"，独立成类型之后同一个错误游标要在两个文件之间传；`AppendHistory` 是会话的灌注口，本来就该挨着往外读的 `Agent.history`。写入失败改成 `Agent.writeErr` 一个字段加构造期闭包，缓冲落在 `Agent` 而不是 `Run` 的局部变量上——循环在构造期建一次、观察者挂死在上面，`Run` 开头清零，一个 Run 一轮账。对外行为不变（`pi.AppendHistory` 只是换了文件），守卫是既有用例加新增的 `TestRunSurfacesSessionWriteFailure`：跑的过程中把会话文件删掉，`Run` 必须透出 `ErrSessionAppendFailed`、同时照旧返回已经产生的消息序列。`writeErr` 这条路径从第五轮起只有代码没有测试，这条补上；去掉 `Run` 里的透出分支，只有它一条红。

**删掉 `AppendHistory`**（第七轮）：`pi.AppendHistory` 删除，连带 `pi/history_test.go` 两条只守它的测试一起删。理由是它**没有任何调用方**——仓库里调它的只有那两条测试（`cmd/*` 全用 `InMemory()` 且不灌历史），而它服务的场景（调用方另有历史存储）在 `session.Manager.Append` + 导出的 `Message.Message2AI()` 面前只是三行循环。留着就是一个"只有测试在用"的公开 API 加两份守卫，与压缩回退那轮同一条标准（没有任何代码能写出的格式不留）。代价写在明处：**"先整份转完再逐条落"这条不变式从库里挪到了调用方手里**——历史里有一条不合法时，不写完这件事现在由调用方自己保证。接真实业务方冷启动迁移时如果这个口径值得收回来，加回一个带真实调用点的版本，届时它会有真实的三元判断（会话是新建的吗）而不是先占位。守卫是 `TestRunContinuesFromSeededInMemorySession` 改写后的版本：灌历史改由测试自己 `Message2AI` + `Append`，断言的还是"会话里已有历史时这轮接着聊"。

**取会话改按业务键**（第八轮）：`Create(root, workDir)` 降为包内 `createSession(path, workDir, sessionID)`，公开入口换成 `OpenOrCreate(root, workDir, sessionID)`——键在就打开、不在就用这个键建，文件名从 `<时间戳>-<8位随机>.jsonl` 变成 `<键>.jsonl`，`newSessionID` 删除。理由是 `Create` 把"想要会话"和"现在就写文件"绑在一起，生成的 id 又是调用方必须自己找地方存的返回值，于是"什么时候建会话"只能由调用方查一遍文件来决定——那本来就是这个包的判断。改成键之后取会话变成幂等操作（每轮调一次是安全的，不需要进程级初始化状态），id 的所有权也回到调用方（业务键本来就幂等，崩溃后不需要索引就能定位文件）。代价写在明处，三条：一、键必填，不能"随便开一个新会话"（pi.dev 默认自己生成 uuidv7，session-manager.ts:969）；二、文件名不再带时间戳，"取最近一个会话"（pi.dev 的 `--continue`，靠目录 mtime）在我们这里没有依据了，将来要做只能读 header 的 `CreatedAt`；三、键会变成文件名，所以补了字符集与长度校验（借 pi.dev `assertValidSessionId` 的正则，128 字符上限是我们加的），非 ASCII 的客户名在这一条被挡下，同一道闸也是路径穿越的防线。同时补两条语义：工作区核对（`encodeWorkDir` 有损，`/a-b` 与 `/a/b` 编到同一个目录，撞上必须拒，新码 `ErrSessionWorkDirMismatch` 80013）与撞名自救（抢输的一方重新打开，不把 `ErrSessionAlreadyExists` 甩给调用方；但发布只裁决"谁建"，不裁决"谁能写"，单写者仍在调用方）。0 字节文件明确**不自愈**，报 `ErrSessionFileEmpty`——pi.dev 在 session-manager.ts:938-947 会重写 header，那份自愈的前提是它的懒写（空文件在那边是正常中间态），我们是即时写，它是异常。守卫是十二条新测试（同键续上、新建、不同键隔离、空键、七种非法键、必填参数、并发同键、工作区不一致、坏文件不改写、空文件不自愈、并发读不到半成品）。破坏实验五处：关掉字符校验、不吸收撞名、不核对工作区、空文件当可自愈、把 `os.Link` 换成 `os.Rename`，各自只让对应那条红。

同一轮还有一条要单独说明，因为它不是设计里想到的、是破坏实验挖出来的真 bug：原本"`O_EXCL` 建出目标文件、紧接着写首行"之间有一个窗口，并发的读侧撞进去会读到 0 字节的文件，把"别人正在创建"误报成 `ErrSessionFileEmpty`——比撞名更糟，调用方会以为盘上的数据坏了。`TestOpenOrCreateConcurrentCallersNeverSeePartialFile`（20 轮 × 8 并发）在改之前三次运行三次红；发布方式改成"先写同目录临时文件、`Sync`、再 `os.Link` 硬链接到目标路径"之后稳定绿（`os.Link` 原子，目标已存在时必然 `EEXIST`，独占与原子发布一次拿到：读侧要么看不到文件，要么看到完整的一份）。`TestCreateRefusesExistingSessionFile` 守着 `EEXIST` 这条——把 `os.Link` 换成会静默覆盖的 `os.Rename`，它立刻红。

同轮还删掉了 `Open(path)`。它唯一的用途是"调用方手里有确切文件路径、直接开那个文件"（pi.dev 的 `--resume`、Claude Code 的 `--resume <绝对路径>` 都有这个能力），但我们的会话按键寻址、文件位置由包内算（`encodeWorkDir` 未导出，调用方本来就算不出路径），拿 `Manager.Path()` 的返回值再 `Open` 回去等价于同一个键再调一次 `OpenOrCreate`，纯绕路；它还是绕过工作区核对的后门——`Open` 不校验 `header.WorkDir`，正好把上面那条核对整个跳过去。唯一真实调用方是 `cmd/sessiontest` 的离线回放，已改走 `OpenOrCreate`。代价写在明处：**失去"从任意路径续接文件"的能力**，将来真要它（比如一个运维工具要拿某个转录文件接着跑），带真实调用点加回，届时它会有明确的调用场景而不是一个空位。

**会话 id 直接当文件名**（第九轮）：`OpenOrCreate` 的第三个参数按"会话 id"来读——它直接就是文件名（`<会话 id>.jsonl`），并补回一个生成器 `session.NewSessionID()`（`chat-` 前缀 + 26 位 base32 随机，`crypto/rand.Text()`，必然过 `validateSessionID`）。第八轮删掉 `newSessionID` 时留的代价第一条是"不能随便开一个新会话"（pi.dev 默认自己生成 uuidv7，session-manager.ts:969），这轮收回来一半：想开新会话就 `OpenOrCreate(root, workDir, session.NewSessionID())`——生成与打开仍是两个动作，`OpenOrCreate` 依旧只按 id 找文件、不判断"该不该建"，返回值仍要调用方自己记着。前缀只归生成器，文件名里不额外加：否则调用方自带 `chat-001` 这类 id 会得到 `chat-chat-001.jsonl`。同轮 `cmd/sessiontest` 的默认落点从"临时目录 + 退出时删"改成当前目录的 `testdata/sessions`（跑完不删，`/testdata/` 进 `.gitignore`），并加了 `-interactive`：每行一轮、每轮重新按 id 取会话交给新建的 agent，跑完报该文件的 entry 条数与字节数——"数据到底写进去没有"从此是屏幕上的一行数字。守卫是 `pi/session/manager_test.go` 四条（文件名等于会话 id、同 id 两次落到同一文件、生成的 id 带前缀且过校验、生成的 id 落盘成 `chat-….jsonl`）。破坏实验两处：给文件名硬加前缀、去掉 `NewSessionID` 的前缀，各自只让对应的测试红。