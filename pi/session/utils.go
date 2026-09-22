package session

import (
	"crypto/rand"
	"fmt"
	"regexp"
	"strings"
	"time"

	pierrors "github.com/PycMono/go-harness/pi/error"
)

// sessionIDPattern 是会话键唯一合法的形状：字母数字开头结尾，中间允许 - _ .。
// 借 pi.dev 的同名正则（session-manager.ts:235）。它同时也是路径穿越的防线——
// 键会变成文件名，"../evil"、"a/b" 这类在正则上就过不去。
var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9._-]*[A-Za-z0-9])?$`)

// sessionIDMaxLength 是会话键的长度上限。pi.dev 没有这一条，是我们加的：键要当
// 文件名，超长会被文件系统以 ENAMETOOLONG 拒掉，不如在参数校验里说清楚。
const sessionIDMaxLength = 128

// validateSessionID 校验调用方给的会话键。空与不合法分开报码：空是"忘了给"，
// 不合法是"给了个不能当文件名的"，调用方对这两种的处理不一样。
func validateSessionID(sessionID string) error {
	if sessionID == "" {
		return pierrors.ErrSessionIDRequired
	}
	if len(sessionID) > sessionIDMaxLength || !sessionIDPattern.MatchString(sessionID) {
		return pierrors.ErrSessionIDInvalid.Wrap(fmt.Errorf(
			"会话 id %q 不合法（%d 字符）", sessionID, len(sessionID)))
	}

	return nil
}

// NewSessionID 生成一个新的会话 id：chat- 前缀 + 26 位 base32 随机
// （crypto/rand.Text），必然过 validateSessionID。调用方要"开一次新会话"时用它：
// 把返回值记下来，下次带着它调 OpenOrCreate 就续上同一个文件。id 直接当文件名，
// 所以它既是身份也是句柄。
func NewSessionID() string {
	return sessionIDPrefix + rand.Text()
}

// newEntryID 生成一个会话内唯一的随机 id：候选先跟会话里已有的 id 比一遍
// （exists 由调用方给，通常是 Entries.hasID），撞了重试，仍撞就退到整串随机。
// id 是 parent 链的键，撞了会让 pathToRoot 的索引互相覆盖，所以查重不是保险，
// 是这条链能不能读对的前提。
func newEntryID(exists func(id string) bool) string {
	for attempt := 0; attempt < entryIDAttempts; attempt++ {
		if id := randomText(entryIDLength); !exists(id) {
			return id
		}
	}

	return rand.Text()
}

// randomText 取 length 个随机字符，字符集是 crypto/rand.Text 的 RFC 4648 base32
// （每字符 5 bit）。Text 恒为 26 字符，length 不超过它就不会越界。
func randomText(length int) string {
	return rand.Text()[:length]
}

// timestampText 是 entry 时间戳的统一写法，RFC3339 UTC，与 header 的 CreatedAt
// 同款。
func timestampText(timestamp time.Time) string {
	return timestamp.UTC().Format(time.RFC3339)
}

// now 是"此刻"的 timestampText。
func now() string {
	return timestampText(time.Now())
}

// encodeWorkDir 把工作目录编码成一个目录名：/ 与 : 替换为 -。
// 只做分组展示，不可逆也不承担唯一性，隔离以 session 文件本身为准。
func encodeWorkDir(workDir string) string {
	encoded := strings.NewReplacer("/", "-", ":", "-", "\\", "-").Replace(workDir)

	return "--" + strings.Trim(encoded, "-") + "--"
}
