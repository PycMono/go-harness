package pi

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/PycMono/go-harness/pi/schema"
)

// 本文件是 pi/context.go 的第一组测试：钉住 Build 的组装顺序。go-harness 会故意
// 把系统消息插在对话中间（本轮输入之后），顺序本身就是协议约定的可观测形式，
// 所以这里断言的是每个位置上的具体消息，而不是"大致包含"。

// newPromptWorkDir 造一个最小工作区：SystemPrompt 只要求根下有非空的 AGENTS.md，
// 技能目录可以不存在。
func newPromptWorkDir(t *testing.T) string {
	t.Helper()

	workDir := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(workDir, "AGENTS.md"), []byte("# 测试身份\n\n只做测试。\n"), 0o644,
	); err != nil {
		t.Fatalf("写 AGENTS.md: %v", err)
	}

	return workDir
}

// TestBuildKeepsAssembledOrder 钉住组装顺序：历史 → 本轮输入 → 系统提示词 →
// 上下文块（按 Priority 从大到小），以及 CurrentInputIndex 指向本轮输入。
func TestBuildKeepsAssembledOrder(t *testing.T) {
	workDir := newPromptWorkDir(t)
	ctx := context.Background()
	systemPrompt, err := SystemPrompt(ctx, workDir)
	if err != nil {
		t.Fatalf("SystemPrompt: %v", err)
	}

	history := schema.Messages{
		&schema.UserMessage{Content: schema.ContentBlocks{schema.TextBlock("上一轮客户")}},
		&schema.AssistantMessage{Content: schema.ContentBlocks{schema.TextBlock("上一轮模型")}},
	}
	input := &schema.UserMessage{Content: schema.ContentBlocks{schema.TextBlock("本轮客户")}}

	cases := []struct {
		name       string
		history    schema.Messages
		blocks     []*ContextBlock
		wantRoles  []schema.Role
		wantSystem []string
		wantIndex  int
	}{
		{
			name:    "没有历史时输入在前、系统提示词紧随其后",
			history: nil,
			blocks:  nil,
			wantRoles: []schema.Role{
				schema.RoleUser, schema.RoleSystem,
			},
			wantSystem: []string{systemPrompt},
			wantIndex:  0,
		},
		{
			name:    "历史排在输入之前，系统提示词仍在输入之后",
			history: history,
			blocks:  nil,
			wantRoles: []schema.Role{
				schema.RoleUser, schema.RoleAssistant, schema.RoleUser, schema.RoleSystem,
			},
			wantSystem: []string{systemPrompt},
			wantIndex:  2,
		},
		{
			name:    "上下文块按 Priority 从大到小排在系统提示词之后",
			history: history,
			blocks: []*ContextBlock{
				{Name: "低优先", Content: "低优先内容", Priority: 1},
				{Name: "高优先", Content: "高优先内容", Priority: 9},
			},
			wantRoles: []schema.Role{
				schema.RoleUser, schema.RoleAssistant, schema.RoleUser,
				schema.RoleSystem, schema.RoleSystem, schema.RoleSystem,
			},
			wantSystem: []string{
				systemPrompt,
				"# Context: 高优先\n高优先内容",
				"# Context: 低优先\n低优先内容",
			},
			wantIndex: 2,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			built, err := NewContextBuilder(workDir).Build(
				ctx, testCase.history, input, testCase.blocks, nil)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}

			assertRoles(t, built.Messages, testCase.wantRoles)
			// 历史是原样搬过来的：位置与取值都不变。
			for index, message := range testCase.history {
				if built.Messages[index] != message {
					t.Fatalf("历史第 %d 条 = %T，不是调用方传进来的那条", index, built.Messages[index])
				}
			}
			// 本轮输入落在历史之后、系统提示词之前。
			if built.Messages[testCase.wantIndex] != schema.Message(input) {
				t.Fatalf("下标 %d 的消息 = %T，不是本轮输入", testCase.wantIndex, built.Messages[testCase.wantIndex])
			}
			if built.CurrentInputIndex != testCase.wantIndex {
				t.Fatalf("CurrentInputIndex = %d，想要 %d", built.CurrentInputIndex, testCase.wantIndex)
			}
			// 输入之后的每一条都是 system，正文逐条对得上。
			for offset, want := range testCase.wantSystem {
				index := testCase.wantIndex + 1 + offset
				system, ok := built.Messages[index].(*schema.SystemMessage)
				if !ok {
					t.Fatalf("下标 %d 的动态类型 = %T，想要 *schema.SystemMessage", index, built.Messages[index])
				}
				if got := messageText(t, system); got != want {
					t.Fatalf("下标 %d 的正文 = %q，想要 %q", index, got, want)
				}
			}
		})
	}
}

// TestBuildCopiesContextBlocks 钉住"排的是副本"：Priority 决定注入顺序，但排序
// 不该改写调用方传进来的切片，否则同一份上下文块复用一次就乱了。
func TestBuildCopiesContextBlocks(t *testing.T) {
	workDir := newPromptWorkDir(t)
	blocks := []*ContextBlock{
		{Name: "低优先", Content: "低优先内容", Priority: 1},
		{Name: "高优先", Content: "高优先内容", Priority: 9},
	}
	input := &schema.UserMessage{Content: schema.ContentBlocks{schema.TextBlock("本轮客户")}}

	built, err := NewContextBuilder(workDir).Build(context.Background(), nil, input, blocks, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if len(blocks) != 2 || blocks[0].Name != "低优先" || blocks[1].Name != "高优先" {
		t.Fatalf("调用方传进来的上下文块被排序改写了: %q, %q", blocks[0].Name, blocks[1].Name)
	}
	if len(built.Messages) != 4 {
		t.Fatalf("消息条数 = %d，想要 4（输入 + 系统提示词 + 两个上下文块）", len(built.Messages))
	}
}
