package tools

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/PycMono/go-reagent/pi/ai"
	pierrors "github.com/PycMono/go-reagent/pi/errors"
)

type EditOperation struct {
	OldText string  `json:"oldText"`
	NewText *string `json:"newText"`
}

type EditDetails struct {
	Diff             string `json:"diff"`
	Patch            string `json:"patch"`
	Replacements     int    `json:"replacements"`
	FirstChangedLine int    `json:"firstChangedLine"`
}

type EditTool struct{ workspace *Workspace }

func NewEditTool(workspace *Workspace) *EditTool { return &EditTool{workspace: workspace} }

func (t *EditTool) Definition() ai.ToolDefinition {
	return ai.ToolDefinition{
		Name:        "edit",
		Description: "对工作区内的现有 UTF-8 文本文件执行一批原子替换；每个 oldText 必须在原始内容中唯一匹配，且范围不得重叠。",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{"type": "string", "description": "相对于工作区的文件路径"},
				"edits": map[string]any{
					"type":     "array",
					"minItems": 1,
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"oldText": map[string]any{"type": "string", "description": "原始文件中要替换的唯一文本片段"},
							"newText": map[string]any{"type": "string", "description": "替换文本；可为空字符串以删除匹配片段"},
						},
						"required":             []string{"oldText", "newText"},
						"additionalProperties": false,
					},
				},
			},
			"required":             []string{"path", "edits"},
			"additionalProperties": false,
		},
	}
}

func (t *EditTool) Execute(ctx context.Context, args json.RawMessage, _ ai.UpdateEmitter) (ai.ToolOutput, error) {
	output, details, err := t.executeWithDetails(ctx, args)
	if err != nil {
		return ai.ToolOutput{}, err
	}
	return ai.ToolOutput{Content: []ai.ContentBlock{ai.TextBlock(output)}, Details: details}, nil
}

func (t *EditTool) execute(ctx context.Context, args json.RawMessage) (string, error) {
	output, _, err := t.executeWithDetails(ctx, args)
	return output, err
}

func (t *EditTool) executeWithDetails(ctx context.Context, args json.RawMessage) (string, EditDetails, error) {
	if !utf8.Valid(args) {
		return "", EditDetails{}, errors.New("参数不是有效的 UTF-8 JSON")
	}

	input, err := decodeEditArgs(args)
	if err != nil {
		return "", EditDetails{}, err
	}
	path, err := cleanRelativePath(input.Path, true)
	if err != nil {
		return "", EditDetails{}, err
	}
	if len(input.Edits) == 0 {
		return "", EditDetails{}, errors.New("edits 必须是非空数组")
	}
	for index, edit := range input.Edits {
		if strings.TrimSpace(edit.OldText) == "" {
			return "", EditDetails{}, fmt.Errorf("edits[%d].oldText 不能为空", index)
		}
		if edit.NewText == nil {
			return "", EditDetails{}, fmt.Errorf("edits[%d].newText 字段不能为空缺失", index)
		}
	}

	if err := ctx.Err(); err != nil {
		return "", EditDetails{}, fmt.Errorf("修改已取消: %w", err)
	}
	info, err := t.workspace.Stat(path)
	if err != nil {
		return "", EditDetails{}, fmt.Errorf("检查文件失败: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", EditDetails{}, errors.New("只允许修改普通文件")
	}
	file, err := t.workspace.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return "", EditDetails{}, fmt.Errorf("打开文件失败: %w", err)
	}
	defer file.Close()
	if err := ctx.Err(); err != nil {
		return "", EditDetails{}, fmt.Errorf("修改已取消: %w", err)
	}

	contentBytes, err := io.ReadAll(file)
	if err != nil {
		return "", EditDetails{}, fmt.Errorf("读取文件内容失败: %w", err)
	}
	if !utf8.Valid(contentBytes) {
		return "", EditDetails{}, errors.New("文件内容不是有效的 UTF-8 文本")
	}
	if bytes.IndexByte(contentBytes, 0) >= 0 {
		return "", EditDetails{}, errors.New("文件包含 NUL 字节，疑似二进制内容")
	}

	originalContent := string(contentBytes)
	matches := make([]plannedEdit, len(input.Edits))
	for index, edit := range input.Edits {
		match, matchErr := findUniqueTextMatch(originalContent, edit.OldText)
		if matchErr != nil {
			return "", EditDetails{}, fmt.Errorf("edits[%d]: %w", index, matchErr)
		}
		matches[index] = plannedEdit{
			index:       index,
			match:       match,
			replacement: replacementForMatch(originalContent, match, *edit.NewText),
		}
	}
	slices.SortFunc(matches, func(a, b plannedEdit) int { return cmp.Compare(a.match.start, b.match.start) })
	for index := 1; index < len(matches); index++ {
		if matches[index].match.start < matches[index-1].match.end {
			return "", EditDetails{}, errors.New("edits 包含重叠或嵌套范围")
		}
	}
	updated := originalContent
	for index := len(matches) - 1; index >= 0; index-- {
		planned := matches[index]
		updated = updated[:planned.match.start] + planned.replacement + updated[planned.match.end:]
	}
	if err := ctx.Err(); err != nil {
		return "", EditDetails{}, fmt.Errorf("修改已取消: %w", err)
	}

	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", EditDetails{}, fmt.Errorf("定位文件写入位置失败: %w", err)
	}
	written, err := file.Write([]byte(updated))
	if err != nil {
		return "", EditDetails{}, fmt.Errorf("写回文件失败: %w", err)
	}
	if written != len(updated) {
		return "", EditDetails{}, fmt.Errorf("写回文件失败: %w", io.ErrShortWrite)
	}
	if err := file.Truncate(int64(len(updated))); err != nil {
		return "", EditDetails{}, fmt.Errorf("截断文件失败: %w", err)
	}

	details := EditDetails{
		Diff:             buildUnifiedEditDiff(path, originalContent, updated),
		Patch:            buildStructuredEditPatch(path, originalContent, updated),
		Replacements:     len(matches),
		FirstChangedLine: 1 + strings.Count(originalContent[:matches[0].match.start], "\n"),
	}
	return fmt.Sprintf("Applied %d edits to %s", len(matches), path), details, nil
}

func writeAll(writer io.Writer, content []byte) error {
	for len(content) > 0 {
		written, err := writer.Write(content)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		content = content[written:]
	}
	return nil
}

type editArgs struct {
	Path  string          `json:"path"`
	Edits []EditOperation `json:"edits"`
}

type plannedEdit struct {
	index       int
	match       textMatch
	replacement string
}

func decodeEditArgs(args json.RawMessage) (editArgs, error) {
	decoder := json.NewDecoder(bytes.NewReader(args))
	decoder.DisallowUnknownFields()

	var input editArgs
	if err := decoder.Decode(&input); err != nil {
		return editArgs{}, fmt.Errorf("参数解析失败: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return editArgs{}, fmt.Errorf("参数包含多余内容: %w", err)
		}
		return editArgs{}, errors.New("参数包含多余 JSON 内容")
	}
	return input, nil
}

func buildUnifiedEditDiff(path, original, updated string) string {
	originalLines, originalFinalNewline := editDiffLines(original)
	updatedLines, updatedFinalNewline := editDiffLines(updated)
	var diff strings.Builder
	fmt.Fprintf(&diff, "--- a/%s\n+++ b/%s\n@@ -%s +%s @@\n", path, path, editDiffRange(len(originalLines)), editDiffRange(len(updatedLines)))
	writeEditDiffLines(&diff, '-', originalLines, originalFinalNewline, true)
	writeEditDiffLines(&diff, '+', updatedLines, updatedFinalNewline, true)
	return diff.String()
}

func buildStructuredEditPatch(path, original, updated string) string {
	originalLines, originalFinalNewline := editDiffLines(original)
	updatedLines, updatedFinalNewline := editDiffLines(updated)
	var patch strings.Builder
	patch.WriteString("*** Begin Patch\n*** Update File: ")
	patch.WriteString(path)
	patch.WriteString("\n@@\n")
	writeEditDiffLines(&patch, '-', originalLines, originalFinalNewline, false)
	writeEditDiffLines(&patch, '+', updatedLines, updatedFinalNewline, false)
	patch.WriteString("*** End Patch")
	return patch.String()
}

func editDiffLines(content string) ([]string, bool) {
	normalized, _ := normalizeNewlinesWithBoundaries(content)
	if normalized == "" {
		return nil, false
	}
	hasFinalNewline := strings.HasSuffix(normalized, "\n")
	if hasFinalNewline {
		normalized = strings.TrimSuffix(normalized, "\n")
	}
	if normalized == "" {
		return []string{""}, hasFinalNewline
	}
	return strings.Split(normalized, "\n"), hasFinalNewline
}

func editDiffRange(lineCount int) string {
	if lineCount == 0 {
		return "0,0"
	}
	return fmt.Sprintf("1,%d", lineCount)
}

func writeEditDiffLines(builder *strings.Builder, prefix byte, lines []string, hasFinalNewline, markMissingNewline bool) {
	for _, line := range lines {
		builder.WriteByte(prefix)
		builder.WriteString(line)
		builder.WriteByte('\n')
	}
	if markMissingNewline && len(lines) > 0 && !hasFinalNewline {
		builder.WriteString("\\ No newline at end of file\n")
	}
}

type textMatch struct {
	start int
	end   int
	level int
}

func findUniqueTextMatch(content, oldText string) (textMatch, error) {
	if strings.TrimSpace(oldText) == "" {
		return textMatch{}, fmt.Errorf("oldText 不能为空")
	}

	exactMatches := matchesAtIndexes(occurrenceIndexes(content, oldText), len(oldText), 1, nil)
	if match, found, err := requireUniqueMatch(exactMatches); found || err != nil {
		return match, err
	}

	normalizedContent, boundaries := normalizeNewlinesWithBoundaries(content)
	normalizedOld, _ := normalizeNewlinesWithBoundaries(oldText)
	newlineMatches := matchesAtIndexes(
		occurrenceIndexes(normalizedContent, normalizedOld),
		len(normalizedOld),
		2,
		boundaries,
	)
	if match, found, err := requireUniqueMatch(newlineMatches); found || err != nil {
		return match, err
	}

	trimmedOld := strings.TrimSpace(normalizedOld)
	trimmedMatches := matchesAtIndexes(
		occurrenceIndexes(normalizedContent, trimmedOld),
		len(trimmedOld),
		3,
		boundaries,
	)
	if match, found, err := requireUniqueMatch(trimmedMatches); found || err != nil {
		return match, err
	}

	lineMatches := lineByLineMatches(content, normalizedOld)
	if match, found, err := requireUniqueMatch(lineMatches); found || err != nil {
		return match, err
	}

	return textMatch{}, pierrors.ErrToolEditNoMatch.Wrap(
		errors.New("在文件中未找到 oldText，请先调用 read 确认文件内容和缩进"),
	)
}

func occurrenceIndexes(content, needle string) []int {
	if needle == "" {
		return nil
	}
	var indexes []int
	for offset := 0; offset <= len(content)-len(needle); {
		index := strings.Index(content[offset:], needle)
		if index < 0 {
			break
		}
		index += offset
		indexes = append(indexes, index)
		offset = index + 1
	}
	return indexes
}

func matchesAtIndexes(indexes []int, length, level int, boundaries []int) []textMatch {
	matches := make([]textMatch, 0, len(indexes))
	for _, index := range indexes {
		start, end := index, index+length
		if boundaries != nil {
			start = boundaries[start]
			end = boundaries[end]
		}
		matches = append(matches, textMatch{start: start, end: end, level: level})
	}
	return matches
}

func requireUniqueMatch(matches []textMatch) (textMatch, bool, error) {
	switch len(matches) {
	case 0:
		return textMatch{}, false, nil
	case 1:
		return matches[0], true, nil
	default:
		return textMatch{}, false, pierrors.ErrToolEditNotUnique.Wrap(
			fmt.Errorf("模糊匹配到了 %d 处相似代码，请提供更多上下文以确保唯一性", len(matches)),
		)
	}
}

func normalizeNewlinesWithBoundaries(input string) (string, []int) {
	normalized := make([]byte, 0, len(input))
	boundaries := make([]int, 1, len(input)+1)
	for index := 0; index < len(input); {
		if input[index] == '\r' && index+1 < len(input) && input[index+1] == '\n' {
			normalized = append(normalized, '\n')
			index += 2
			boundaries = append(boundaries, index)
			continue
		}
		normalized = append(normalized, input[index])
		index++
		boundaries = append(boundaries, index)
	}
	return string(normalized), boundaries
}

type textLine struct {
	text  string
	start int
	end   int
}

func lineByLineMatches(content, oldText string) []textMatch {
	normalizedContent, boundaries := normalizeNewlinesWithBoundaries(content)
	contentLines := splitTextLines(normalizedContent, boundaries)
	trimmedOld := strings.TrimSpace(oldText)
	if trimmedOld == "" {
		return nil
	}
	oldLines := strings.Split(trimmedOld, "\n")
	if len(contentLines) < len(oldLines) {
		return nil
	}
	for index := range oldLines {
		oldLines[index] = strings.TrimSpace(oldLines[index])
	}

	var matches []textMatch
	for start := 0; start <= len(contentLines)-len(oldLines); start++ {
		matched := true
		for offset := range oldLines {
			if strings.TrimSpace(contentLines[start+offset].text) != oldLines[offset] {
				matched = false
				break
			}
		}
		if matched {
			last := start + len(oldLines) - 1
			matches = append(matches, textMatch{
				start: contentLines[start].start,
				end:   contentLines[last].end,
				level: 4,
			})
		}
	}
	return matches
}

func splitTextLines(normalized string, boundaries []int) []textLine {
	var lines []textLine
	lineStart := 0
	for index := 0; index <= len(normalized); index++ {
		if index < len(normalized) && normalized[index] != '\n' {
			continue
		}
		lines = append(lines, textLine{
			text:  normalized[lineStart:index],
			start: boundaries[lineStart],
			end:   boundaries[index],
		})
		lineStart = index + 1
	}
	return lines
}

func replacementForMatch(content string, match textMatch, newText string) string {
	style := firstNewlineStyle(content[match.start:match.end])
	if style == "" {
		style = dominantNewlineStyle(content)
	}
	if style == "" {
		return newText
	}

	normalized, _ := normalizeNewlinesWithBoundaries(newText)
	if style == "\r\n" {
		return strings.ReplaceAll(normalized, "\n", "\r\n")
	}
	return normalized
}

func firstNewlineStyle(content string) string {
	for index := 0; index < len(content); index++ {
		switch content[index] {
		case '\r':
			if index+1 < len(content) && content[index+1] == '\n' {
				return "\r\n"
			}
		case '\n':
			return "\n"
		}
	}
	return ""
}

func dominantNewlineStyle(content string) string {
	crlfCount := strings.Count(content, "\r\n")
	lfCount := strings.Count(content, "\n") - crlfCount
	if crlfCount > lfCount {
		return "\r\n"
	}
	if lfCount > crlfCount {
		return "\n"
	}
	return firstNewlineStyle(content)
}
