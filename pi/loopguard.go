package pi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	pierrors "github.com/PycMono/go-harness/pi/error"
	"github.com/PycMono/go-harness/pi/schema"
)

// defaultLoopGuardTurns 是循环检测的默认阈值：连续这么多轮的工具调用与结果
// 完全一样就判循环。取 3 的理由——读同一个文件两遍在正常流程里常见（先看
// 结构、改完再确认），三遍还一模一样，基本可以确定这一轮没有产生任何变化。
const defaultLoopGuardTurns = 3

// loopGuard 是内建的循环检测：连续若干轮的工具调用与结果都完全相同就判循环。
// 判据只有一条，因为它只需要一个状态：上一轮的签名和它连续出现了几次。
// "连续"而不是"窗口内累计"——连续是更强的信号、误报更低，也让状态退化
// 成两个字段；代价是抓不到 A→B→A→B 这种振荡，真出现了再加窗口。
type loopGuard struct {
	limit   int
	last    string
	repeats int
}

// observe 记下这一轮的签名并判定。返回 nil 表示这一轮不是循环。
// limit <= 0 表示关闭，直接返回——装配不做条件判断，关不关由值决定。
func (g *loopGuard) observe(report TurnReport) error {
	if g.limit <= 0 {
		return nil
	}

	signature := signatureOf(report.Message.ToolCalls, report.ToolResults)
	if signature == g.last {
		g.repeats++
	} else {
		g.last, g.repeats = signature, 1
	}
	if g.repeats < g.limit {
		return nil
	}

	return pierrors.ErrRunLoopDetected.Wrap(fmt.Errorf(
		"连续 %d 轮的工具调用与结果完全相同：%s", g.repeats, clipSignature(signature)))
}

// reset 清空计数。一个 Run 一轮账：跨 Run 留着的话，两次毫不相干的运行
// 各调一次同一个工具就会被算成重复。
func (g *loopGuard) reset() { g.last, g.repeats = "", 0 }

// loopGuardLimit 把装配处的配置翻成判据的阈值：0 取默认值，负数表示关闭
// （原样带过去，由 observe 吞掉），正数就是它自己。
func loopGuardLimit(configured int) int {
	if configured == 0 {
		return defaultLoopGuardTurns
	}

	return configured
}

// signatureOf 是一轮工具调用与结果的指纹：批内每个调用按原顺序取「工具名 +
// 规范化参数 + 结果」，再用换行拼起来。不做排序——调度器按调用在批内的原顺序
// 分波执行，[read a, write b] 与 [write b, read a] 的结果可能不同，排了序会把
// 它们算成同一轮。
//
// 分隔符用换行：结果文本里本来就有换行，所以严格说两条不同的记录理论上能拼出
// 同一个串（要结果里写出一段像下一条调用前缀的文字，还得连着三轮一样）。这
// 不是安全边界，撞了的后果是多停一次运行，不为它换一个更怪的字符。
func signatureOf(calls schema.ToolCalls, results schema.Messages) string {
	parts := make([]string, 0, len(calls))
	for index, call := range calls {
		part := call.Name + ":" + canonicalArguments(call.Arguments)
		// 结果按下标对齐（ExecuteBatch 的返回与 calls 一一对齐）。长度不一致时
		// 退化成只看调用：判据弱一点，比越界 panic 好——钩子在运行路径上，
		// panic 会把整次运行连同已产生的消息一起丢掉。
		if index < len(results) {
			part += "=>" + resultFingerprint(results[index])
		}
		parts = append(parts, part)
	}

	return strings.Join(parts, "\n")
}

// resultFingerprint 把一条工具结果压成可比的一行：各内容块的文本。带上结果比
// 不带严——调用相同、结果也相同才叫"这一轮什么也没变"，于是轮询类工具（连着
// 追问同一个状态，第三次返回"完成"）不会被误停。那是误报方向，本方案一路都在
// 躲它。
//
// 图片块取「媒体类型 + 载荷长度」，不取内容摘要：同类型、同长度的两张图会撞成
// 同一条结果（又是误报方向，但概率低得多）。真出现图片类工具再加摘要，现在
// 不为它引一个 crypto。
//
// 不单独带 IsError：内容一样就是没进展，成败那一栏由内容体现，多一栏不改变
// 判定，反而让"同一段报错、一次算失败一次算成功"变成两轮。
func resultFingerprint(message schema.Message) string {
	blocks := message.Blocks()
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if block.Image != nil {
			parts = append(parts, fmt.Sprintf("[image %s %d]",
				block.Image.MIMEType, len(block.Image.Data)))
			continue
		}
		parts = append(parts, block.Text)
	}

	return strings.Join(parts, "\n")
}

// canonicalArguments 把参数规范化成可比的字符串。模型对同一次调用两次可能
// 给出键序不同的 JSON（对象成员顺序在协议上没有意义），直接比原始字节会把
// 一次调用看成两次。解析再编回去就归了序——encoding/json 编 map 时按键排序，
// 这是标准库保证的行为。
//
// 解析用 UseNumber：默认解析进 float64 会把 1 与 1.0 归一（宽容），但同时
// 让超过 2^53 的整数掉精度，两个不同的数会被算成同一个。宽容的方向是误报，
// 误报会掐掉一次健康运行，比漏报更难查。UseNumber 保留字面量，代价是 1 与
// 1.0 算两个签名：那是漏报方向（同一件事因为数字写法不同被当成两轮），但模型
// 不会无缘无故在两轮之间改数字写法，撞上的概率远低于精度塌陷。
//
// 解析不了、或后面还跟着别的 token（模型给过截断或拼接错位的 JSON）时原样
// 返回：它是模型给的字符串，比不出错，也不该在这里报错。
func canonicalArguments(arguments json.RawMessage) string {
	decoder := json.NewDecoder(bytes.NewReader(arguments))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return string(arguments)
	}
	// 合法的 JSON 值之后除了 EOF 不该再有内容。有的话说明这串东西不是我们
	// 以为的一整个值，别编回去——编回去会丢掉后面那段，让两个不同的参数
	// 撞成同一个签名。
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return string(arguments)
	}
	encoded, err := json.Marshal(decoded)
	if err != nil {
		return string(arguments)
	}

	return string(encoded)
}

// clipSignature 把签名截到能进错误文案的长度，按 UTF-8 边界切，免得日志里
// 出现半个汉字。只用在错误文案上，不参与比较——比较用的是完整签名。
// 签名通常由 json.Marshal 产出、是合法 UTF-8；走 canonicalArguments 原样
// 返回那条路时可能不是（那是模型给的原始字节）。所以这里按 ValidString 往前
// 退到边界，最坏退到 0、文案只剩一个省略号——可接受的降级，不为它加分支。
func clipSignature(signature string) string {
	const max = 240
	if len(signature) <= max {
		return signature
	}
	cut := max
	for cut > 0 && !utf8.ValidString(signature[:cut]) {
		cut--
	}

	return signature[:cut] + "…"
}
