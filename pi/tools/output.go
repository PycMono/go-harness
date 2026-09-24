package tools

import (
	"strings"
	"unicode/utf8"

	"github.com/PycMono/go-harness/pi/schema"
)

// UpdateEmitter receives incremental updates from a running tool.
type UpdateEmitter func(schema.ToolUpdate)

// LimitText 返回文本总量不超过 maxBytes 的 ToolOutput 副本。非文本块不
// 占用额度；发生截断时保证 UTF-8 完整，并追加固定提示及 truncated 详情。
// maxBytes 小于零时按零处理。截断提示本身不计入 maxBytes。
func LimitText(output schema.ToolOutput, maxBytes int) schema.ToolOutput {
	if maxBytes < 0 {
		maxBytes = 0
	}

	remaining := maxBytes
	limited := make(schema.ContentBlocks, 0, len(output.Content)+1)
	truncated := false
	for _, block := range output.Content {
		if block.Type != schema.ContentTypeText {
			limited = append(limited, block)
			continue
		}

		text := strings.ToValidUTF8(block.Text, "�")
		if len(text) <= remaining {
			block.Text = text
			limited = append(limited, block)
			remaining -= len(text)
			continue
		}
		cut := remaining
		for cut > 0 && !utf8.ValidString(text[:cut]) {
			cut--
		}

		block.Text = text[:cut]
		limited = append(limited, block, schema.TextBlock(OutputTruncationMarker))
		truncated = true
		break
	}

	output.Content = limited
	if !truncated {
		return output
	}

	switch details := output.Details.(type) {
	case map[string]any:
		cloned := make(map[string]any, len(details)+1)
		for key, value := range details {
			cloned[key] = value
		}
		cloned["truncated"] = true
		output.Details = cloned
	case nil:
		output.Details = map[string]any{"truncated": true}
	default:
		output.Details = map[string]any{"tool_details": details, "truncated": true}
	}

	return output
}
