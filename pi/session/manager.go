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
	leafID  string
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

// BuildMessages 重建给模型的 history：从当前叶子沿 ParentID 追到根，取路径上的
// 对话消息。返回的消息是会话状态的只读视图，调用方不得修改。
func (m *Manager) BuildMessages() schema.Messages {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.entries.pathToRoot(m.leafID).messagesOf()
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

// newManager 从读好的快照装配 Manager：叶子取文件里最后一条 entry。
func newManager(file *sessionFile, entries Entries) *Manager {
	return &Manager{
		file:    file,
		entries: entries,
		leafID:  entries[len(entries)-1].ID,
	}
}
