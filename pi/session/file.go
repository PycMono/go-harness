package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	pierrors "github.com/PycMono/go-harness/pi/error"
)

// sessionFile 是会话文件本身：路径，加上本进程最后一次与它对齐时的长度。
// 长度是"有没有别人写过"的全部依据——不引文件锁，靠长度变化早失败。
// Manager 用 nil 表示内存会话，所以这里不需要"要不要落盘"的开关。
type sessionFile struct {
	path string
	size int64
}

// load 按行读取会话文件，并把基线记成这次读到的字节数。首行必须是合法 header，
// 否则整份文件拒绝打开；首行之后的坏行直接跳过——进程崩在写一半时留下的残行被
// 忽略，此前写完的历史完好，这是崩溃安全语义的落点。
//
// 基线取读到的字节数，而不是读完之后再 Stat 一次：两次取长度之间有窗口，别的
// 进程在窗口里追加的内容会被算进基线，此后本进程再也发现不了。
func (f *sessionFile) load() (Entries, error) {
	data, err := os.ReadFile(f.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, pierrors.ErrSessionNotFound.Wrap(err)
		}

		return nil, f.invalid(err)
	}
	f.size = int64(len(data))

	lines := make([]string, 0, 16)
	for _, line := range strings.Split(string(data), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		return nil, f.invalid(pierrors.ErrSessionFileEmpty)
	}

	var header Entry
	if err = json.Unmarshal([]byte(lines[0]), &header); err != nil {
		return nil, f.invalid(pierrors.ErrSessionHeaderLineInvalid.Wrap(err))
	}
	if header.Type != EntryHeader {
		return nil, f.invalid(pierrors.ErrSessionHeaderTypeInvalid.Wrap(
			fmt.Errorf("首行 entry 类型为 %q", header.Type)))
	}
	if err = header.validate(); err != nil {
		return nil, f.invalid(err)
	}
	if header.Header.Version != sessionVersion {
		return nil, f.invalid(fmt.Errorf("不支持的会话文件版本 %d，当前版本为 %d", header.Header.Version, sessionVersion))
	}

	entries := make(Entries, 1, len(lines))
	entries[0] = header
	for _, line := range lines[1:] {
		var entry Entry
		if err = json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		if err = entry.validate(); err != nil {
			continue
		}
		entries = append(entries, entry)
	}

	return entries, nil
}

// create 独占新建会话文件并写入首行。独占是这套设计里唯一挡"撞名"的地方：重名
// 直接失败，而不是往别人的会话文件里追一个 header——那样 load 会把第二个 header
// 当普通 entry 收下，叶子一变，此前的历史就静默消失。
//
// 发布方式是"先写临时文件、再硬链接到目标路径"，不是直接 O_EXCL 建目标文件再写：
// 后者在建出文件和写 header 之间有窗口，并发的读侧正好撞进去会读到 0 字节或者
// 半行 header 的文件，把"别人正在创建"误报成"文件损坏"——比撞名更糟，调用方会
// 以为盘上的数据坏了。os.Link 是原子的，目标已存在时又必然失败（EEXIST），
// 独占与原子发布一次拿到：读侧要么看不到文件，要么看到完整的一份。
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

// invalid 给"文件不可用"挂上容器码 80000，原因留在 cause 上：CodeOf 答"这份文件
// 读不了"（调用方按它分流），errors.Is 答"为什么读不了"（日志与断言用）。
func (f *sessionFile) invalid(cause error) error {
	return pierrors.ErrSessionFileInvalid.Wrap(fmt.Errorf("会话文件 %s 不可用: %w", f.path, cause))
}
