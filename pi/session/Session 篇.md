# 从零手搓 Harness 之会话管理：一个 JSONL 文件，把聊过的都存下来

## 前言

这是从零开始搭建 Agent Harness（`go-harness`）的第五篇。上一篇把技能从代码里挪出去了，模型"会什么"不再写死在提示词里。但还有一样东西一直攥在调用方手里：客户说过什么。

前面几篇跑起来的形态，history 是调用方每轮自己传进来的，Run 返回就丢。业务侧要想接着聊，只能自己建一张表把对话存下来，下一轮再拼回去。这里有几个绕不开的问题：进程重启怎么办、扩容到三台机器怎么保证同一个客户落到同一份历史上、拼出来的历史跟模型上一轮看到的对不对得上（工具调用和工具结果错位，模型就开始胡言乱语）。

这一篇把这层存储补上：`pi/session`。一个会话一个 JSONL 文件，只追加；下一轮从盘上追根重建历史，调用方只需要给一个会话 id。

完整代码参考 GitHub：[https://github.com/PycMono/go-harness](https://github.com/PycMono/go-harness)，本文涉及的所有文件（`pi/session/`、`pi/agent.go`、`cmd/sessiontest/`）都在仓库里可以直接翻。`pi/session` 一共五个源文件、671 行（不含测试），读完不费劲。

## 流程图

整个会话域就两个动作，一个读一个写：

```text
调用方每轮
   │  session.OpenOrCreate(root, workDir, 会话 id)
   ▼
pi/session.Manager（一个会话一个实例）
   │
   ├── 读：BuildMessages()
   │        └─ pathToRoot(叶子) ──► messagesOf() ──► schema.Messages
   │                                                    │
   │                                            交给 ContextBuilder 组装
   │
   └── 写：Append(entry)
            └─ 校验（拒收 header / ParentID 必须是叶子 / 文件长度没被人动过）
                 └─ sessionFile.append() ──► 一行 JSON + Sync
                                                │
                                    <会话 id>.jsonl（只追加）
```

图里有一个反直觉的地方：存储是线性的（一行接一行往后加），读却是按树读的。每条 entry 带一个 `parent_id` 指向上一行，重建时从叶子沿着 `parent_id` 一路追到根再反转。这么做的好处是"编辑后重发"这种分叉天然成立——分叉只是换了片叶子，已经写下去的历史一行都不用改。Phase 1 全部串成线性，但字段一次到位，免得将来为树结构再做一次格式迁移。

## 代码层级划分

```
pi/session/
├── constant.go   # EntryType、文件格式版本、id 长度与重试次数
├── entry.go      # Entry 联合类型 + Header + Entries 上的追根与取消息
├── file.go       # sessionFile：JSONL 读写（按行加载、坏行跳过、独占新建、追加写）
├── manager.go    # Manager：取会话 / 追加 / 叶子 / 重建上下文
└── utils.go      # 会话 id 校验与生成、entry id 生成、时间戳、工作目录编码
```

依赖方向是单向的：`pi` → `pi/session` → `pi/schema` + `pi/error`。session 不认识 loop、不认识 provider、不认识工具，所有接线都在 agent 层完成。

| 文件 | 职责 | 对外 |
|---|---|---|
| `entry.go` | 一行长什么样，怎么从叶子追到根 | `Entry` / `Header` / `Entries` |
| `file.go` | 字节怎么落盘、怎么读回来 | 不导出 |
| `manager.go` | 一个会话的生命周期 | `Manager` |
| `utils.go` | id 的形状与随机源 | `NewSessionID` |

## 代码实战

### 1. 一条 entry 长什么样

```go
package session

// EntryType 表示 session 文件里一行 entry 的类型。
type EntryType string

const (
	EntryHeader  EntryType = "session" // 首行，只出现一次
	EntryMessage EntryType = "message" // 对话消息（含工具调用与结果）
)

// Entry 是 session 文件里的一行。Type 决定哪个载荷非空：session 行填 Header，
// message 行填 Message。
type Entry struct {
	Type      EntryType      `json:"type"`
	ID        string         `json:"id"`
	ParentID  string         `json:"parent_id,omitempty"`
	Timestamp string         `json:"timestamp"`
	Header    *Header        `json:"header,omitempty"`
	Message   schema.Message `json:"message,omitempty"`
}

// Header 是会话首行。
type Header struct {
	ID            string `json:"id"`
	Version       int    `json:"version"`
	CreatedAt     string `json:"created_at"`
	WorkDir       string `json:"work_dir"`
	ParentSession string `json:"parent_session,omitempty"`
}
```

`Entry` 是"单结构体 + 可空载荷"的写法，不是 Go 意义上的接口联合。这不是偷懒：`Message` 那一栏已经是 `schema.Message` 四元联合接口了，如果 `Entry` 也做成接口联合，文件里每行就要多一层"我是哪种 entry"的判别，而实际上只有两种行，其中一种还只出现一次。tag + 可空载荷表达力是够的，代价是 `Header` / `Message` 两个字段看起来像"可以同时有"，靠 `validate` 兜住。

实际落盘长这样（`cmd/sessiontest -offline` 跑出来的原文，三行）：

```json
{"type":"session","id":"chat-001","timestamp":"2026-09-22T08:29:19Z","header":{"id":"chat-001","version":2,"created_at":"2026-09-22T08:29:19Z","work_dir":"/Users/allen/projects/work/github/go-harness/cmd/skilltest/testdata"}}
{"type":"message","id":"46NILJOL","parent_id":"chat-001","timestamp":"2026-09-22T08:29:19Z","message":{"role":"user","content":[{"type":"text","text":"记住：我的工单号是 8899。"}]}}
{"type":"message","id":"IQPPLKZU","parent_id":"46NILJOL","timestamp":"2026-09-22T08:29:19Z","message":{"role":"assistant","content":[{"type":"text","text":"记下了"}]}}
```

三条能看出全部格式约定：首行是 header，`id` 与 header 里的 `id` 同值，是整条父链的根；后面每行的 `parent_id` 指向上一条的 `id`；`id` 是 8 位随机字符串，会话内唯一就够，不需要全局唯一。一行一条 entry，整个文件就是一次对话的时间线。

这里有个 Go 的坑要单独说：`Message` 是接口，`encoding/json` 填不了非空接口字段——它不知道怎么从 JSON 里挑一个具体类型出来。所以 `Entry` 得自己实现 `UnmarshalJSON`：

```go
func (entry *Entry) UnmarshalJSON(data []byte) error {
	type Alias Entry
	fields := struct {
		*Alias
		Message json.RawMessage `json:"message"`
	}{Alias: (*Alias)(entry)}

	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if len(fields.Message) == 0 {
		entry.Message = nil

		return nil
	}

	message, err := schema.DecodeMessage(fields.Message)
	if err != nil {
		return err
	}
	entry.Message = message

	return nil
}
```

`Alias` 只是把 `Entry` 的字段连同 tag 借过来，自己不带方法集，所以不会绕回来再调一次本方法。同名字段 `Message` 在这一层被 `json.RawMessage` 抢走（浅层字段优先），剩下的字段照旧按 tag 解。`schema.DecodeMessage` 按 JSON 里的 `role` 恢复具体类型，所以"哪条消息是哪个类型"这件事只有消息层知道，entry 层只管把原始字节交过去。

### 2. 追根：重建上下文是纯函数

写下去是线性的，读回来是树的。整个重建就两步，都是 `Entries` 上的方法：

```go
// pathToRoot 从叶子沿 ParentID 追到根，再反转成从根到叶的顺序。链断在中间
// （中间某行坏掉）时保留从叶子走得通的一段，而不是丢掉整条路径。
func (entries Entries) pathToRoot(leafID string) Entries {
	indexByID := make(map[string]int, len(entries))
	for index, entry := range entries {
		indexByID[entry.ID] = index
	}

	reversed := make(Entries, 0, len(entries))
	// 计数上限兜住 id 互相指向的死循环：坏文件不该把重建卡死。
	for id, steps := leafID, 0; id != "" && steps < len(entries); steps++ {
		index, ok := indexByID[id]
		if !ok {
			break
		}
		reversed = append(reversed, entries[index])
		id = entries[index].ParentID
	}

	path := make(Entries, len(reversed))
	for index := range reversed {
		path[len(reversed)-1-index] = reversed[index]
	}

	return path
}

// messagesOf 取出路径上的对话消息，跳过 header。
func (entries Entries) messagesOf() schema.Messages {
	messages := make(schema.Messages, 0, len(entries))
	for _, entry := range entries {
		if entry.Type == EntryMessage && entry.Message != nil {
			messages = append(messages, entry.Message)
		}
	}

	return messages
}
```

`Manager.BuildMessages` 就是把这两步串起来：

```go
func (m *Manager) BuildMessages() schema.Messages {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.entries.pathToRoot(m.leafID).messagesOf()
}
```

两个细节值得留意。

一是 `steps < len(entries)` 这个上限。它不是为了性能，是为了兜住一个坏文件：如果两行的 `id` / `parent_id` 互相指向，没有上限的循环会直接卡死。文件坏掉不该把重建拖死，最多是重建出来的消息少几条。

二是"链断在中间"的语义。`pathToRoot` 走到一个不存在的 `parent_id` 就 `break` 而不是报错，保留从叶子走得通的那一段。这跟读文件时的坏行跳过是同一条策略：坏掉的那一段丢掉，它前面的历史完好，能救多少救多少。

把重建做成纯函数还有个直接的好处：`Entries` 是快照，方法不改自身、接收者不变，所以拿一串 entry 就能离线验证重建结果，不用碰文件、不用起会话。

### 3. 写：O_APPEND + 每条 Sync + 坏行跳过

崩溃安全这件事，三条约定落在三个地方：

```go
// load 按行读取会话文件，并把基线记成这次读到的字节数。首行必须是合法 header，
// 否则整份文件拒绝打开；首行之后的坏行直接跳过——进程崩在写一半时留下的残行被
// 忽略，此前写完的历史完好，这是崩溃安全语义的落点。
func (f *sessionFile) load() (Entries, error) {
	data, err := os.ReadFile(f.path)
	// ... 省略读取错误分流
	f.size = int64(len(data))

	lines := make([]string, 0, 16)
	for _, line := range strings.Split(string(data), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	// ... 省略空文件判断
	var header Entry
	if err = json.Unmarshal([]byte(lines[0]), &header); err != nil {
		return nil, f.invalid(pierrors.ErrSessionHeaderLineInvalid.Wrap(err))
	}
	// ... 省略首行的类型与载荷校验

	entries := make(Entries, 1, len(lines))
	entries[0] = header
	for _, line := range lines[1:] {
		var entry Entry
		if err = json.Unmarshal([]byte(line), &entry); err != nil {
			continue // 坏行跳过
		}
		if err = entry.validate(); err != nil {
			continue // 载荷对不上的行同样跳过
		}
		entries = append(entries, entry)
	}

	return entries, nil
}
```

首行严、其余宽，这个不对称是刻意的。首行是 header，它坏了整份文件就没有身份（不知道属于哪个工作区、哪个版本），只能拒绝打开；首行之后的坏行只可能是"崩在写一半时的那一行"，跳过它，前面写完的历史一条不少。取严格者会丢掉整份会话，取宽松者只丢最后一行。

写入侧：

```go
// append 把一条 entry 追加落盘。O_APPEND 即可，不带 O_CREATE：文件在 Append 路径
// 上消失会先被 checkUnchanged 拦下，不在这里悄悄重建。
func (f *sessionFile) append(entry Entry) error {
	file, err := os.OpenFile(f.path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return pierrors.ErrSessionAppendFailed.Wrap(err)
	}
	defer file.Close()

	return f.writeLine(file, entry)
}

// writeLine 把一条 entry 作为一行 JSON 写进 file，并把基线前移到写入后的长度。
// 每条 entry 一次 Sync：崩在写一半时残行被读侧的坏行语义忽略，而不是丢掉此前
// 已经写完的历史。
func (f *sessionFile) writeLine(file *os.File, entry Entry) error {
	data, err := json.Marshal(entry)
	if err != nil {
		return pierrors.ErrSessionAppendFailed.Wrap(err)
	}
	data = append(data, '\n')

	if _, err = file.Write(data); err != nil {
		return pierrors.ErrSessionAppendFailed.Wrap(err)
	}
	if err = file.Sync(); err != nil {
		return pierrors.ErrSessionAppendFailed.Wrap(err)
	}

	info, err := file.Stat()
	if err != nil {
		return pierrors.ErrSessionAppendFailed.Wrap(err)
	}
	// 前移基线，否则下次 Append 会把自己刚写的这条当成别人写的。
	f.size = info.Size()

	return nil
}
```

三条约定在这里凑齐：追加用 `O_APPEND`，一行一条 `Sync`（不是只 flush 缓冲区），写完立刻把基线前移。第一条保证多个写入者至少不会撕裂同一行；第二条是整个"崩了不丢历史"的地基——消息已经发给模型、盘上没有，是这套系统里最难查的一类段丢；第三条看着像顺手，其实是下一次 `Append` 的自我校验前提。

### 4. 独占新建：为什么不是 O_EXCL 建目标文件

会话文件第一次出现的时候，必须独占。理由跟"撞名"的严重性有关：追加式写入永远不会报错，第二个 header 会被 `load` 当成普通 entry 收下，叶子一被顶到它身上，`pathToRoot` 追到它就停——此前的历史静默消失，没有任何报错，重建出来是空的历史。

独占的常规写法是 `O_CREATE|O_EXCL` 建目标文件、紧接着写首行。我一开始就是这么写的，后来撞上一个更糟的问题：建出文件和写 header 之间有一个窗口，并发的读侧正好撞进去，会读到 0 字节或者半行 header 的文件，把"别人正在创建"误报成"文件损坏"。这比撞名还难受——调用方会以为盘上的数据坏了，去查数据，而其实什么都没坏，只是有人正在建。

现在的发布方式是"先写同目录下的临时文件、再硬链接到目标路径"：

```go
func (f *sessionFile) create(entry Entry) error {
	temp, err := os.CreateTemp(filepath.Dir(f.path), "."+filepath.Base(f.path)+".creating-")
	if err != nil {
		return pierrors.ErrSessionAppendFailed.Wrap(err)
	}
	tempPath := temp.Name()
	// 发布成功后这次删除是空操作（inode 已挂在目标名下）；失败时收掉残骸。
	defer os.Remove(tempPath)

	if err = f.writeLine(temp, entry); err != nil {
		temp.Close()

		return err
	}
	if err = temp.Close(); err != nil {
		return pierrors.ErrSessionAppendFailed.Wrap(err)
	}
	// CreateTemp 建的是 0600，会话文件沿用之前的 0644。
	if err = os.Chmod(tempPath, 0o644); err != nil {
		return pierrors.ErrSessionAppendFailed.Wrap(err)
	}

	if err = os.Link(tempPath, f.path); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return pierrors.ErrSessionAlreadyExists.Wrap(err)
		}

		return pierrors.ErrSessionAppendFailed.Wrap(err)
	}

	return nil
}
```

`os.Link` 一次拿到两件东西：它是原子的，而且目标已存在时必然失败（`EEXIST`）。所以读侧要么看不到文件，要么看到完整的一份，中间态不可见；独占与原子发布不再需要两次系统调用去凑。临时文件写在同一目录（`os.Link` 不能跨文件系统）并带点前缀，正常发布完之后那行 `defer os.Remove` 是个空操作——inode 已经挂在目标名下了，删掉的是临时目录项。

### 5. 三个守卫：header、叶子、文件长度

`Append` 是唯一的写入口，三个校验都在这儿：

```go
// Append 追加一条 entry：ID 为空时生成（先跟会话里已有的 id 查重），ParentID 为空
// 时取当前叶子，显式提供的 ParentID 必须是当前叶子。文件被别的写入者动过同样报
// ErrSessionParentMismatch——宁可早失败，也不要静默交错出两份互不相认的历史。
func (m *Manager) Append(entry Entry) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// header 只属于首行，Create 之外没有第二个写它的入口：多一条 header，叶子就
	// 被顶到它身上，pathToRoot 到它为止，此前历史静默消失。
	if entry.Type == EntryHeader {
		return pierrors.ErrSessionAppendFailed.Wrap(pierrors.ErrSessionHeaderNotAppendable)
	}
	if m.file != nil {
		if err := m.file.checkUnchanged(); err != nil {
			return err
		}
	}
	if entry.ID == "" {
		entry.ID = newEntryID(m.entries.hasID)
	}
	if entry.ParentID == "" {
		entry.ParentID = m.leafID
	} else if entry.ParentID != m.leafID {
		return pierrors.ErrSessionParentMismatch.Wrap(fmt.Errorf(
			"entry %s 的 parent_id=%s 不是当前叶子 %s", entry.ID, entry.ParentID, m.leafID))
	}
	if entry.Timestamp == "" {
		entry.Timestamp = now()
	}
	if err := entry.validate(); err != nil {
		return pierrors.ErrSessionAppendFailed.Wrap(err)
	}

	if err := m.write(entry); err != nil {
		return err
	}
	m.leafID = entry.ID

	return nil
}
```

三个守卫对应的三个故障：

第一个是"多写一条 header"。上面说过后果——叶子被顶掉，历史静默消失。所以 `Append` 干脆拒收 `EntryHeader`，header 只能由建会话那条路径写。

第二个是"写乱了顺序"。`ParentID` 留空就自动取当前叶子（正常路径不用管）；显式给了就必须等于当前叶子。这条约束是"线性历史"的机械保证，Phase 1 没有分叉，所以显式传一个非叶子的 `ParentID` 一定是调用方算错了。

第三个是"另一个进程也在写这个文件"。这里没有文件锁，靠的是长度：

```go
// checkUnchanged 确认文件仍停在本进程最后一次对齐的位置：长度变了，说明另一个
// 写入者动过这个会话。
func (f *sessionFile) checkUnchanged() error {
	info, err := os.Stat(f.path)
	if err != nil {
		return pierrors.ErrSessionAppendFailed.Wrap(err)
	}
	if f.size != 0 && info.Size() != f.size {
		return pierrors.ErrSessionParentMismatch.Wrap(fmt.Errorf(
			"会话文件 %s 已被其他写入者改动（%d → %d 字节）", f.path, f.size, info.Size()))
	}

	return nil
}
```

为什么不上文件锁：这套机制的定位是"单写者契约"，一个 session 文件在任一时刻只归一个 Agent 实例写，包内的 `sync.Mutex` 管住进程内并发就够了。跨进程并发写本来就不在支持范围内，锁能让它"不出错"反而更危险——两份历史交错写进一个文件，谁都不报错，读出来的顺序是错的。宁愿早失败：另一个进程写过之后，本进程的下一次 `Append` 立刻报错。

`f.size` 的取法有讲究，值得单独说：`load` 记的是这次真正读到的字节数，不能读完之后再 `Stat` 一次取长度。两次取长度之间有窗口，别的进程正好在窗口里追加，那份内容会被算进基线，此后本进程再也发现不了。基线必须是"我读的时候文件有多长"，不是"我读完之后文件有多长"。

### 6. 取会话：id 由调用方给

会话的入口只有一个函数：

```go
// OpenOrCreate 按会话 id 取一个会话：文件（<会话 id>.jsonl）在就打开，不在就用这个
// id 建一个。同一个 id 调多少次拿到的都是同一个文件，所以调用方不需要判断"该不该
// 建"——每轮调一次就行。要开一次新会话就先拿个 id：NewSessionID()。
func OpenOrCreate(root, workDir, sessionID string) (*Manager, error) { ... }
```

它是幂等的：键第一次用是新建，以后是续上。所以调用方不需要判断"该不该建"，也不需要进程级的初始化状态，每轮调一次就行。这一点决定了 `Create` 不是一个公开 API——"创建"独立成动作的话，调用方又得查一遍文件来决定什么时候调它，而那本来就是这个包的判断。

会话 id 直接当文件名：

```
sessions/
└── --编码后的workDir--/            # 路径中的 / 与 : 替换为 -
    └── <会话 id>.jsonl
```

id 的含义由调用方定，用业务本来就有的那个最好——工单号、客户 id。理由很实际：id 若由库生成，业务方就得把返回值存进自己的库，多一次写入、多一处不一致，下次请求才能拿它找回来；用业务键就省掉了这层存储，崩溃之后也只需要"这是客户 42 的会话"这一句话就能定位文件。

真需要"随便开一次新会话"，用 `NewSessionID()` 现拿一个：

```go
// NewSessionID 生成一个新的会话 id：chat- 前缀 + 26 位 base32 随机
// （crypto/rand.Text），必然过 validateSessionID。
func NewSessionID() string {
	return sessionIDPrefix + rand.Text()
}
```

id 会变成文件名，所以它有一道形状校验，这道校验同时是路径穿越的防线：

```go
// sessionIDPattern 是会话键唯一合法的形状：字母数字开头结尾，中间允许 - _ .。
// 借 pi.dev 的同名正则（session-manager.ts:235）。
var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9._-]*[A-Za-z0-9])?$`)

// sessionIDMaxLength 是会话键的长度上限。pi.dev 没有这一条，是我们加的：键要当
// 文件名，超长会被文件系统以 ENAMETOOLONG 拒掉，不如在参数校验里说清楚。
const sessionIDMaxLength = 128
```

`../evil`、`a/b` 这类输入在正则上就过不去，非 ASCII 的客户名也在这一条被挡下——想用中文客户名做 id，得先自己编码成合法形状。长度上限是我们加的，pi.dev 只校验字符集不校验长度；理由是不想让文件系统抛 `ENAMETOOLONG`，那是个说不清楚的错误。

打开已有会话时还有一道核对：

```go
// openSession 打开会话文件，并核对它确实属于这个工作区。目录名是 encodeWorkDir 的
// 有损编码（"/a-b" 与 "/a/b" 编到同一个目录），键又由调用方给，所以同一个路径可能
// 被两个工作区同时指到；不核对的话，两边会把彼此的会话当成自己的。
func openSession(path, workDir string) (*Manager, error) {
	file := &sessionFile{path: path}
	entries, err := file.load()
	if err != nil {
		return nil, err
	}
	if header := entries[0].Header; header.WorkDir != workDir {
		return nil, file.invalid(pierrors.ErrSessionWorkDirMismatch.Wrap(fmt.Errorf(
			"会话 %s 属于工作区 %q，不是 %q", path, header.WorkDir, workDir)))
	}

	return newManager(file, entries), nil
}
```

目录名那层是展示用的编码，不可逆（`/` `:` `\` 全替换成 `-`），所以同一份会话文件可能被两个工作区同时指到。header 里存的 `work_dir` 是唯一的原始值，核对只能看它。

顺带一提，撞名不用调用方管。并发的同键调用里只有一个建得成，抢输的一方收到"已存在"后重新打开：

```go
	if err = createSession(path, workDir, sessionID); err != nil && !errors.Is(err, pierrors.ErrSessionAlreadyExists) {
		return nil, err
	}

	return openSession(path, workDir)
```

撞名不是调用方的错误，不该甩出去。但也要说清楚：独占发布只裁决"谁建"，不裁决"谁能写"，跨进程的单写者仍然由调用方保证。

### 7. InMemory：不落盘不是另一个类型

`InMemory()` 返回的 `Manager` 和文件会话是同一个类型，区别只在 `file == nil`：

```go
// InMemory 返回一个不落盘的会话：entry 只留在内存里，Append 不做任何文件 IO。
func InMemory() *Manager {
	return &Manager{}
}

// write 落盘一条 entry，写失败时内存状态不动。内存会话没有文件，只推进内存状态。
func (m *Manager) write(entry Entry) error {
	if m.file != nil {
		if err := m.file.append(entry); err != nil {
			return err
		}
	}
	m.entries = append(m.entries, entry)

	return nil
}
```

`Append` / `BuildMessages` / `Entries` 走的是完全相同的代码路径，只有 `write` 跳过文件 IO、`checkUnchanged` 不调用。上层装配因此不需要为"这轮要不要留档"分叉——只想单跑一轮就 `Session: session.InMemory()`，一行差别，其余代码一样。

这里用 `*sessionFile` 的 nil 而不是一个 `persist bool`：要不要落盘和"文件在哪、写到第几字节"本来就是同一件事的两面，`file` 有了，长度也有了；再放一个布尔开关就等于允许"有文件但不写"这种不存在的状态。内存会话没有文件也没有 header，父链的根就是第一条 entry（`ParentID` 为空）。

### 8. 接线：让循环自己往会话里写

前面七节都是 `pi/session` 内部的事，最后一节是它跟 agent 的接缝。会话是横切能力，但仓库里真正需要动的地方只有两处。

第一处是 `pi.Options` 新增一个必填字段，以及装配期的校验：

```go
// Session 是会话管理器，必填。history 只认这一个来源：本轮运行前的历史
// 从会话重建，本轮产生的消息逐条写回会话。只跑单轮用 session.InMemory()。
Session *session.Manager
```

```go
func newAgent(provider ai.Provider, opts *Options) (*Agent, error) {
	if opts.Session == nil {
		return nil, pierrors.ErrInitialization.Wrap(errors.New("session must not be nil"))
	}
	// ...
```

不给"没有会话"留第二条路径。留了的话，history 就会冒出来源——调用方又能自己传历史了，那这套存储就没意义了。

第二处是把 `session.Append` 挂到循环上。这里要的是"消息产生的当口就落盘"，不是等 Run 结束后批量扫一遍结果——模型调用中途崩掉时，已经发生的消息必须已经在盘上：

```go
	// 会话写入接在循环的逐条消息观察者上：模型消息与工具结果产生的当口就落盘，
	// 而不是等 Run 结束后批量补写。
	agent.loop = NewLoop(
		provider,
		// ...
		WithMessageObserver(func(message schema.Message) {
			// 已经出过错就不再往下写：一次写入失败会滚成一串，真正有用的只有第一个。
			if agent.writeErr != nil {
				return
			}
			agent.writeErr = agent.session.Append(session.Entry{Type: session.EntryMessage, Message: message})
		}),
	)
```

`WithMessageObserver` 是 `pi/loop.go` 这轮唯一新增的东西，循环逻辑本身一行没动。观察者在循环的控制流里同步调用，没有返回错误的通道，所以写入失败只能先攒在 `Agent.writeErr` 上，等 `loop.run` 返回后由 `Run` 透出：

```go
	// 会话没写下去比模型出错更隐蔽：消息可能已经发给模型了，但盘上没有，
	// 下一轮重建出来的历史就缺一段，必须让调用方知道。
	if a.writeErr != nil {
		return &RunOutput{message: messages}, a.writeErr
	}
```

缓冲为什么落在 `Agent` 而不是 `Run` 的局部变量上：循环是构造期建一次、观察者挂死在它上面的，没有一个合适的时机把 `Run` 的局部变量交给它。所以 `Agent` 扛这个字段，`Run` 开头清零，一个 Run 一轮账。

还有一处顺序，看着不起眼但错了很隐蔽：

```go
// history 组装本轮的历史消息：从会话重建，并把本轮输入也交给会话记账。
func (a *Agent) history(inputMessage schema.Message) (schema.Messages, error) {
	// 重建必须在追加本轮输入之前：先落盘再重建的话，这条输入会既在历史里、
	// 又被上下文组装再追加一次。
	history := a.session.BuildMessages()
	// 趁模型还没开始跑就落盘：中途崩了，恢复时从客户这句话之后接着聊。
	if err := a.session.Append(session.Entry{Type: session.EntryMessage, Message: inputMessage}); err != nil {
		return nil, err
	}

	return history, nil
}
```

先重建、再追加，顺序不能反。反过来写，这条输入会既出现在重建出来的历史里、又被 `ContextBuilder.Build` 当作本轮输入追加一次，模型看到两遍同样的话。追加本身仍在任何模型调用之前，所以"客户这句话"在模型开始跑之前就已经在盘上了。

`RunInput` 这轮也换掉了：`Prompt` 加 `Images` 两个字段，历史不再通过它传。

## 跑起来看

`cmd/sessiontest` 是这层的验证端子，形态照 `cmd/skilltest` 来。离线跑一遍（`-offline` 不调模型，只验证写入与重建）：

```
$ go run ./cmd/sessiontest -offline

=== 会话根目录 ===
/Users/allen/projects/work/github/go-harness/testdata/sessions

=== 会话文件 ===
/Users/allen/projects/work/github/go-harness/testdata/sessions/--Users-allen-projects-work-github-go-harness-cmd-skilltest-testdata--/chat-001.jsonl

=== 离线模式：直接追加 2 条消息后的重建 ===

=== 会话 entry（3 条，叶子 IQPPLKZU）===
 0 [chat-001] header chat-001 workdir=/Users/allen/projects/work/github/go-harness/cmd/skilltest/testdata
 1 [46NILJOL] ← chat-001 message user: 记住：我的工单号是 8899。
 2 [IQPPLKZU] ← 46NILJOL message assistant: 记下了

 0 [user] 记住：我的工单号是 8899。
 1 [assistant] 记下了

=== 重新打开后重建的上下文 ===

 0 [user] 记住：我的工单号是 8899。
 1 [assistant] 记下了
```

中间那段是内存里的链条，最后一段是拿同一个 id 重新 `OpenOrCreate` 之后从盘上重建的——两段一样，说明盘上的形态能还原出同样的上下文。

`-interactive` 是一轮一轮的形态，每轮都先按 id 重新取一次会话再交给一个新建的 agent，等价于每轮换一个进程：

```
$ printf '我的工单号是 8899\n我的工单号是多少？\n:q\n' | go run ./cmd/sessiontest -offline -interactive -key chat-002

=== 一轮一轮聊（离线，每行一轮，:q 结束）===
> --- 第 1 轮结束：entry 3 条，文件 577 字节 ---

> --- 第 2 轮结束：entry 5 条，文件 932 字节 ---
```

（上面两行的会话文件路径为了排版略掉了，实际输出里带着完整路径。）

每轮报一次 entry 条数与文件字节数。"数据到底写进去没有"从此是屏幕上的一行数字，不用去猜。

接上真实模型就是去掉 `-offline`，两轮之间隔一次 `OpenOrCreate`：

```
$ go run ./cmd/sessiontest -key chat-001
```

第一轮让它记住工单号，第二轮只问"我的工单号是多少"。第二轮的回答只能来自会话文件里的历史——中间那次重新打开已经把内存状态清掉了。

## 与 pi.dev 差在哪

会话这层的参考实现是 pi.dev（[badlogic/pi-mono](https://github.com/badlogic/pi-mono)），核心代码在 `packages/coding-agent/src/core/session-manager.ts`。骨架是对齐的：一个会话一个 JSONL 文件、只追加、每条 entry 带 id 与 parentId、从叶子追根重建上下文、坏行跳过。这几条是它验证过的形状，没必要重新发明。有差别的是下面几处：

| 项 | pi.dev | go-harness |
|---|---|---|
| 文件位置 | 跟随 cwd：`~/.pi/agent/sessions/--编码后的cwd--/<ISO 时间>_<会话 id>.jsonl`（`session-manager.ts:990-991`） | sessions 根目录可配，`--编码后的workDir--/<会话 id>.jsonl` |
| 会话 id | 库自己生成，`createSessionId()` 是 uuidv7（`:230-232`） | 调用方给，直接当文件名；要随机的用 `NewSessionID()` |
| entry 类型 | 10 种判别联合：message / compaction / branch_summary / label / usage……（`:165-176`） | 2 种：`session` 首行、`message` |
| 首行与 entry 的关系 | `FileEntry = SessionHeader \| SessionEntry`，两者平级（`:178`） | 单结构体 + 可空载荷指针，`Entry{Type, Header, Message}` |
| 0 字节文件 | 重写一个合法 header，自愈（`:946`） | 拒绝打开，报 `ErrSessionFileEmpty` |
| 文件版本 | `CURRENT_SESSION_VERSION = 3`（`:38`） | 2，读取时严格比对 |

逐条说下取舍。

目录归属：pi.dev 是终端单用户工具，会话跟着 cwd 走最自然；业务服务的会话归属由调用方语义决定（按客户分、按工单分都行），写死成 cwd 就错了，所以根目录做成参数。

会话 id：pi.dev 默认自己生成 uuidv7，`--continue` 靠目录 mtime 找最近的会话。我们选了"id 由调用方给"，理由是幂等——同一个业务键调几次都落到同一个文件，调用方不需要在库里存一张 id 映射表。代价是丢掉了"取最近一个会话"的能力，将来真要做，只能读 header 的 `CreatedAt`。

entry 类型：pi.dev 那 10 种里，`compaction` / `branch_summary` 属于压缩和分叉（我们还没做），`label` / `model_change` / `session_info` 属于它的产品形态需要而业务场景用不到。只留 2 种，需要时再加——留着写不出来的读侧分支就是死代码。

首行的表达：Go 没有联合类型，pi.dev 那边 `SessionHeader` 与 10 种 `SessionEntry` 平级、各自是独立接口，我们用"单结构体 + 两个可空载荷"的等价写法。字段一个一个对着 `SessionHeader`（`:40-46`）来的：`cwd` ↔ `WorkDir`、`timestamp` ↔ `CreatedAt`、`parentSession` ↔ `ParentSession`。

0 字节文件：这一条两边选了相反的边。pi.dev 遇到空文件会重写一个合法 header（`:946`），我们拒绝打开。差别在写入时机：pi.dev 是懒写，空文件在它的世界里是正常中间态，所以必须自愈；我们是即时写，空文件只可能来自崩了一半的创建——静默重写等于在赌"那不是我正在写的文件"。

还有三条是 harness 层的对齐，不算差异：会话挂在构造参数上且必填（`AgentHarnessOptions.session`，`agent-harness.ts:519`）、`prompt` 没有任何历史入参（`:550-551`）、调用方自带历史时走公开的 `appendMessage`（`:543`）而不是另开一个"传历史"的入口。我们这边的对应物是 `pi.Options.Session` 必填、`RunInput.History` 删除、灌历史走 `session.Manager.Append`。

## 总结

回到流程图。存储层（`entry.go` + `file.go`）只有两件事：一行 entry 长什么样，和字节怎么落盘。崩溃安全是三条约定凑出来的——追加、每条 Sync、坏行跳过；独占新建靠 `os.Link` 一次拿到原子发布与撞名检测。

会话层（`manager.go`）把存储包成一个幂等入口：按 id 取会话，在就打开、不在就建，调用方每轮调一次，不需要判断"该不该建"。重建是纯函数，从叶子追根、取消息，跟存储实现无关。

装配层（`agent.go`）只有两处接线：构造期校验会话必填，然后把 `session.Append` 挂到循环的逐条消息观察者上。循环逻辑一行没动，history 从此只有会话这一个来源。

下一篇打算写上下文组装，也就是 `ContextBuilder` 那一块——系统提示词、技能目录、AGENTS.md 和历史怎么排成一次请求。感兴趣的话关注一下，防止走丢。
