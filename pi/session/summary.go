package session

import (
	"fmt"
	"strings"

	"github.com/PycMono/go-harness/pi/schema"
)

// transcriptBudget 是这次摘要请求能分给序列化正文的字符额度：窗口减去余量，
// 再减去固定部分（模板、标签、旧摘要）已经占掉的字符，最后折成"按 ASCII 比例
// 计的字符数"。不是正数时返回 0，调用方据此走最小序列化。
//
// 固定部分必须算进来：不然"上限"只限住了正文，模板与旧摘要可以把它顶出去，
// 这个数就只是正文的限额，不是请求的限额。
func (p Plan) transcriptBudget(window Window, fixedChars int) int {
	if !window.Enabled() {
		return 0
	}
	chars := int(window.Tokens-requestHeadroomTokens)*asciiCharsPerToken - fixedChars
	if chars <= 0 {
		return 0
	}

	return chars
}

// transcriptLines 把一条消息投影成若干行（助手消息的工具调用各占一行）。摘要
// 模型看到的是"要被阅读的对话"，不是"要继续的对话"——所以用显式标记而不是消息
// 角色本身，免得模型接话。图片块换成脱敏占位（复用 schema 的占位投影），不让带图
// 的消息在摘要请求里变成空行。
func transcriptLines(message schema.Message, limit int) []string {
	text, err := message.Blocks().WithImagePlaceholders().Text()
	if err != nil {
		// 脏块投影不出文本：给一条带标记的降级文本，不静默丢消息。
		text = fmt.Sprintf("[内容无法投影: %v]", err)
	}

	switch message.Role() {
	case schema.RoleAssistant:
		lines := []string{transcriptLine("[Assistant]", text, limit)}
		if assistant, ok := message.(*schema.AssistantMessage); ok {
			for _, call := range assistant.ToolCalls {
				lines = append(lines, transcriptLine("[Assistant tool calls]",
					call.Name+" "+string(call.Arguments), limit))
			}
		}

		return lines
	case schema.RoleTool:
		// 工具结果是长文本的主要来源：一条几万字符的输出原样塞进去，请求还没发
		// 出去就先撞窗口。第二级降级对它是唯一默认就截的角色。
		return []string{transcriptLine("[Tool result]", text, limit)}
	default:
		return []string{transcriptLine("["+string(message.Role())+"]", text, limit)}
	}
}

// transcriptLine 写一行 "标记: 正文"；空正文也保留标记，占位行不会被静默吃掉。
func transcriptLine(label, text string, limit int) string {
	if strings.TrimSpace(text) == "" {
		return label + ":"
	}

	return label + ": " + truncateForSummary(text, limit)
}

// truncateForSummary 截断一段文本，并留下"截掉了多少"的标记：读摘要的模型需要
// 知道这里是不完整的，否则会把半句输出当成全部。
func truncateForSummary(text string, max int) string {
	runes := []rune(text)
	if max <= 0 || len(runes) <= max {
		return text
	}

	return string(runes[:max]) + fmt.Sprintf("\n[... 另有 %d 个字符被截断]", len(runes)-max)
}

// sumChars 按与估算同一套口径数一段文本的"折合字符数"。
func sumChars(lines []string) int {
	total := 0
	for _, line := range lines {
		total += estimateTextChars(line)
	}

	return total
}
