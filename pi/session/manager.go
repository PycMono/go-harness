package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	pierrors "github.com/PycMono/go-harness/pi/error"
	"github.com/PycMono/go-harness/pi/schema"
)

// Manager 管理一个会话：追加 entry、维护叶子、重建模型上下文。一个会话文件在
// 任一时刻只归一个 Manager 写——包内锁只防进程内并发，不防多进程，跨进程写入
// 靠 Append 的文件长度校验早暴露。
// 落盘与否由 file 是否为 nil 决定，而不是分成两个类型：内存会话（InMemory）与
// 文件会话走同一套 Append / BuildMessages，上层装配不需要为"这轮要不要留档"分支。
type Manager struct {
	mu      sync.Mutex
	file    *sessionFile
	entries Entries
	leafID  string // 最后一条ID
	// headerVersion 是首行写的格式版本，装配时读一次。判断"这份会话能不能收压缩
	
	// 边界"要看它，而那个判断每轮都会走，不该每轮读盘（见 canWriteBoundary）。
	headerVersion int
}

// OpenOrCreate 按会话 id 取一个会话：文件（<会话 id>.jsonl）在就打开，不在就用这个
// id 建一个。同一个 id 调多少次拿到的都是同一个文件，所以调用方不需要判断"该不该
// 建"——每轮调一次就行。要开一次新会话就先拿个 id：NewSessionID()。
//
// id 由调用方给，含义也由调用方定（客户 id、工单号之类，或者 NewSessionID() 的随机
// id）。它直接当文件名，所以只允许字母数字与 - _ .，且首尾必须是字母数字：这道校验
// 同时是路径穿越的防线（"../evil"、"a/b" 在这里就被挡下）。
//
// 文件损坏、或者不属于这个工作区时一律拒开，绝不覆盖：盘上的东西不是我们的，
// 我们唯一的权利是不读它。
func OpenOrCreate(root, workDir, sessionID string) (*Manager, error) {
	// 参数校验全在碰磁盘之前：非法键在这里被挡下，root 下不会多出任何东西。
	if strings.TrimSpace(root) == "" {
		return nil, pierrors.ErrSessionsRootRequired
	}
	if strings.TrimSpace(workDir) == "" {
		return nil, pierrors.ErrWorkDirRequired
	}
	if err := validateSessionID(sessionID); err != nil {
		return nil, err
	}

	directory := filepath.Join(root, encodeWorkDir(workDir))
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return nil, pierrors.ErrInitialization.Wrap(err)
	}
	path := filepath.Join(directory, sessionID+".jsonl")

	manager, err := openSession(path, workDir)
	if err == nil {
		return manager, nil
	}
	if !errors.Is(err, pierrors.ErrSessionNotFound) {
		return nil, err
	}

	// 走到这里只剩一种情况：文件不存在。用 O_EXCL 独占新建，并发的同键调用里只有
	// 一个建得成；抢输的一方收到"已存在"后重新打开——撞名不是调用方的错误，不该
	// 甩出去，我们也不引入文件锁：O_EXCL 只裁决"谁建"，不裁决"谁能写"。
	if err = createSession(path, workDir, sessionID); err != nil && !errors.Is(err, pierrors.ErrSessionAlreadyExists) {
		return nil, err
	}

	return openSession(path, workDir)
}

// InMemory 返回一个不落盘的会话：entry 只留在内存里，Append 不做任何文件 IO。
// 调用方自己维护历史、或只想单跑一轮时用它——装配路径与文件会话完全相同，
// 上层不必为"这轮要不要留档"写两套代码。
func InMemory() *Manager {
	return &Manager{}
}

// Path 返回会话文件路径；内存会话返回空字符串。
func (m *Manager) Path() string {
	if m.file == nil {
		return ""
	}

	return m.file.path
}

// Entries 返回全量 entry 快照。切片是副本，entry 本身是只读视图，调用方不得
// 修改。
func (m *Manager) Entries() Entries {
	m.mu.Lock()
	defer m.mu.Unlock()

	return append(Entries(nil), m.entries...)
}

// LeafID 返回当前叶子的 entry id，调用方据此串 ParentID。
func (m *Manager) LeafID() string {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.leafID
}

// Append 追加一条 entry：ID 为空时生成（先跟会话里已有的 id 查重），ParentID 为空
// 时取当前叶子，显式提供的 ParentID 必须是当前叶子。文件被别的写入者动过同样报
// ErrSessionParentMismatch——宁可早失败，也不要静默交错出两份互不相认的历史。
func (m *Manager) Append(entry Entry) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.appendLocked(entry)
}

// appendLocked 是 Append 与 Compact 的公共实现，调用方必须已经持有锁。
func (m *Manager) appendLocked(entry Entry) error {
	// header 只属于首行，Create 之外没有第二个写它的入口：多一条 header，叶子就
	// 被顶到它身上，pathToRoot 到它为止，此前历史静默消失。
	if entry.Type == EntryHeader {
		return pierrors.ErrSessionAppendFailed.Wrap(pierrors.ErrSessionHeaderNotAppendable)
	}
	// 压缩边界是 v3 才有的东西。v2 文件不升版、也不重写：首行永远写着 v2，塞进
	// 边界只会让旧二进制跳过它、把 parent 链断在那里。所以这里不是"升级之后接着
	// 写"，而是拒绝——写不进去比写进去更诚实。
	if entry.Type == EntryCompaction && !m.canWriteBoundary() {
		return pierrors.ErrSessionCompactionVersionUnsupported.Wrap(fmt.Errorf(
			"会话文件版本为 %d，不支持压缩边界（需要 %d）", m.headerVersion, sessionVersion))
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

// BuildMessages 重建给模型的 history：从当前叶子沿 ParentID 追到根，按读侧规则
// 折叠（压缩边界之前的消息由摘要替代），取路径上的对话消息。返回的消息是会话状态
// 的只读视图，调用方不得修改。
func (m *Manager) BuildMessages() schema.Messages {
	m.mu.Lock()
	defer m.mu.Unlock()

	path := m.entries.pathToRoot(m.leafID)
	messages, _ := path.foldedContext(len(path))

	return messages
}

// MessagesAt 取"折叠到 upto 为止"的历史段（upto 是本轮开始时的叶子 id）。压缩
// 当场换历史时用它，而不是自己拿计划里的字段拼：折叠规则（认哪条边界、摘要怎么
// 投影、哪些消息算数）只此一份，历史段因此不可能与读侧不一致。upto 指不着时返回
// nil——调用方据此不改写历史，而不是拿一份可能重复的历史去顶替。
func (m *Manager) MessagesAt(upto string) schema.Messages {
	m.mu.Lock()
	defer m.mu.Unlock()

	path := m.entries.pathToRoot(m.leafID)
	index := path.indexOf(upto)
	if index < 0 {
		return nil
	}
	messages, _ := path.foldedContext(index + 1)

	return messages
}

// ContextTokens 估算这次请求有多大：折叠到 upto 为止的历史，加上 extra。
//
// 上界必须是 upto（本轮开始时的叶子），不能是文件尾。本轮输入与本轮产生的消息在
// 盘上位于 upto 之后（只能往后追加，本轮开始时它们都还不存在），折到文件尾会把
// 同一段消息既算进历史段、又算进 extra，数两遍——而 extra 正是为了补上它们才存的。
//
// extra 是本轮还没进历史的那些（本轮输入、系统提示词、本轮已产生的消息）：在列表
// 里位于折叠段之后，所以它们的 usage 一律按新鲜算。正常路径上这条成立——压缩只
// 发生在某一轮的开头，紧接着这一轮就产出带 usage 的回复，下一次判定拿到的最新
// usage 必然写在最新边界之后。
//
// 唯一的例外是"边界之后一条 usage 都没有"（那一轮的回复没带用量）：扫描会退到
// 压缩前那条账上，估算偏大。代价有界——触发一次判定、多摘一次已经摘过的保留段，
// 落盘的边界仍然正确（TokensBefore 只记录、不进模型）。要堵住它得让调用方记下
// "压缩当场已产出的条数"、作为 extra 的第二个新鲜下标，这轮不做：多一个字段和一个
// 参数，换的是这个少见分支里省一次摘要调用。
func (m *Manager) ContextTokens(upto string, extra schema.Messages) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()

	path := m.entries.pathToRoot(m.leafID)
	index := path.indexOf(upto)
	if index < 0 {
		// 上界指不着（本轮起点不在当前路径上）：宁可按整条路径高估。触发只是多
		// 判一次，而 PlanCompaction 同样指不着 upto、不会落盘，估错不落地。
		index = len(path) - 1
	}
	messages, freshFrom := path.foldedContext(index + 1)
	if len(extra) > 0 {
		messages = append(messages[:len(messages):len(messages)], extra...)
	}

	return estimateContextTokens(messages, freshFrom)
}

// PlanCompaction 对当前路径做一次压缩计划。upto 是本轮开始时的叶子 id：
// 本轮输入与本轮产生的消息不在压缩范围内，必须原样留在请求里。upto 为空或
// 指不着时返回 nil 与 false。
//
// 文件版本过旧的会话直接返回 nil 与 false，连计划都不做：v2 文件写不了边界（见
// appendLocked），做了计划也只是白调一次摘要模型，而上下文并不会因此变短——
// 于是每一轮都会重新越线、重新白跑。
func (m *Manager) PlanCompaction(window Window, upto string) (*Plan, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.canWriteBoundary() {
		return nil, false
	}

	return m.entries.pathToRoot(m.leafID).plan(window, upto)
}

// Compact 落盘一次压缩：在同一个锁里核对计划仍然成立（计划成立时的叶子就是当前
// 叶子、边界指针在当前路径上且不晚于边界自己），再写边界。
//
// 指针校验必须在这儿做而不是在 validate 里：validate 只看得到一条 entry，
// 看不到路径，而"指得着"是路径级的事实。
//
// plan 为 nil 是调用方的错误（计划只有 PlanCompaction 一个来源，拿到非 nil 的
// 计划才该走到这里），返回 90000 而不是让它解引用时 panic。
func (m *Manager) Compact(plan *Plan, summary string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if plan == nil {
		return pierrors.ErrInternal.Wrap(errors.New("压缩计划为空"))
	}
	if plan.LeafID != m.leafID {
		return pierrors.ErrSessionCompactionPointerStale.Wrap(fmt.Errorf(
			"计划成立于叶子 %s，当前叶子是 %s", plan.LeafID, m.leafID))
	}
	path := m.entries.pathToRoot(m.leafID)
	if kept := path.indexOf(plan.FirstKeptEntryID); kept < 0 || kept > len(path)-1 {
		return pierrors.ErrSessionCompactionPointerStale.Wrap(fmt.Errorf(
			"first_kept_entry_id=%s 不在当前路径上", plan.FirstKeptEntryID))
	}

	return m.appendLocked(plan.Entry(summary))
}

// canWriteBoundary 报告这份会话能不能收压缩边界。内存会话能（没有首行要对谁诚实），
// 文件会话要求首行版本就是当前版本。
func (m *Manager) canWriteBoundary() bool {
	return m.file == nil || m.headerVersion >= sessionVersion
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

// createSession 独占新建会话文件并写入 header。不导出：想开新会话就走
// OpenOrCreate，键没被用过自然就是新建——"创建"不该是一个独立动作，否则调用方
// 又得判断"什么时候该建"，这正是它要解决的问题。
func createSession(path, workDir, sessionID string) error {
	// 一次读钟：entry 的 Timestamp 与 header 的 CreatedAt 都从它来。分两次
	// time.Now() 会让"创建于何时"出现两个不同答案。
	timestamp := time.Now().UTC()
	header := Entry{
		Type:      EntryHeader,
		ID:        sessionID,
		Timestamp: timestampText(timestamp),
		Header: &Header{
			ID:        sessionID,
			Version:   sessionVersion,
			CreatedAt: timestampText(timestamp),
			WorkDir:   workDir,
		},
	}

	file := &sessionFile{path: path}

	return file.create(header)
}

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

// newManager 从读好的快照装配 Manager：叶子取文件里最后一条 entry，格式版本取
// 首行（读侧已保证它在可读范围内）。
func newManager(file *sessionFile, entries Entries) *Manager {
	return &Manager{
		file:          file,
		entries:       entries,
		leafID:        entries[len(entries)-1].ID,
		headerVersion: entries[0].Header.Version,
	}
}
