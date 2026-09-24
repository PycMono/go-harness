package mcp

import (
	"slices"
	"strings"
	"sync"
)

const (
	// stderrKeepBytes 是留住的子进程 stderr 上限。要的只是"起不来时说清原因"，
	// 不需要全量，也不该把远端能塞多少东西就存多少。
	stderrKeepBytes = 4 << 10
	// stderrShownLines 是贴进错误与日志的行数上限。
	stderrShownLines = 8
	// stderrMinSecretRunes 是参与隐去的凭据值最短长度：比这短的多半是 "1"、"dev"、
	// PATH 片段这类，替换它们会把 stderr 切得读不懂，而真正的 Key 都比这长。
	stderrMinSecretRunes = 8
	// redactedPlaceholder 是凭据值的替换文本。
	redactedPlaceholder = "[已隐去]"
)

// stderrTail 收子进程 stderr 的尾部。stdio server 起不来时（包没装、Node 崩了、
// 版本不对）它吐的那几行往往是唯一能说明原因的东西，而协议层看到的只有
// `calling "initialize": EOF`。
//
// 三条取舍：
//   - 只留尾部。一直写就一直丢前面的，内存有上限；写满了也不阻断子进程。
//   - 只在 server 级失败（连接、工具发现）时读，正常接入时不读——健康的 server
//     也爱往 stderr 写 npm 警告之类，进日志只是噪声。
//   - 读的时候按已知凭据（配置里的头值与 env 值）过一遍，值换成占位文本：子进程
//     把自己环境里的 Key 打出来是很常见的事。
type stderrTail struct {
	mu      sync.Mutex
	buf     []byte
	dropped bool
	secrets []string
}

// newStderrTail 造一个收集器。凭据表在装配时定一次：头值（header_env 已经换成
// 真值）与 env 值都是我们交给子进程的东西，都可能被它打回 stderr。
func newStderrTail(config ServerConfig) *stderrTail {
	secrets := make([]string, 0, len(config.Headers)+len(config.Env))
	for _, value := range config.Headers {
		secrets = append(secrets, value)
	}
	for _, value := range config.Env {
		secrets = append(secrets, value)
	}
	secrets = slices.DeleteFunc(secrets, func(value string) bool {
		return len([]rune(value)) < stderrMinSecretRunes
	})

	return &stderrTail{secrets: secrets}
}

// Write 收一段 stderr。它永远返回写完（错误吞掉）：收集失败不该把子进程带走。
func (t *stderrTail) Write(chunk []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.buf = append(t.buf, chunk...)
	if len(t.buf) > stderrKeepBytes {
		// 三下标切片拿一个零容量的切片，append 只能重新分配：否则留下的那份
		// 底下还挂着一个一直变长的数组。
		t.buf = append(t.buf[:0:0], t.buf[len(t.buf)-stderrKeepBytes:]...)
		t.dropped = true
	}

	return len(chunk), nil
}

// tail 返回最后几行，凭据已隐去，空行去掉；没收到东西时返回空串。多行并成一行：
// 它要进的是日志字段与错误文案，换行会把一条日志切碎。
func (t *stderrTail) tail() string {
	t.mu.Lock()
	raw := string(t.buf)
	dropped := t.dropped
	secrets := slices.Clone(t.secrets)
	t.mu.Unlock()

	lines := make([]string, 0, stderrShownLines)
	for _, line := range strings.Split(raw, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	trimmed := len(lines) > stderrShownLines
	if trimmed {
		lines = lines[len(lines)-stderrShownLines:]
	}
	if len(lines) == 0 {
		return ""
	}

	text := strings.Join(lines, " / ")
	if dropped || trimmed {
		// 前面还有内容被丢掉了，别让人把这几行当成全部。
		text = "… " + text
	}

	return redact(text, secrets)
}

// redact 把凭据值换成占位文本。纯字符串替换：不猜格式、不做正则——认得的凭据是
// 我们自己装上去的那几个值，知道就隐掉，不知道的不假装能认出来。
func redact(text string, secrets []string) string {
	for _, secret := range secrets {
		text = strings.ReplaceAll(text, secret, redactedPlaceholder)
	}

	return text
}
