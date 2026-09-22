package pi

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/PycMono/go-harness/pi/ai"
	pierrors "github.com/PycMono/go-harness/pi/error"
	"github.com/PycMono/go-harness/pi/schema"
	"github.com/PycMono/go-harness/pi/tools"
)

// 本文件是 pi/loop.go 的第一组测试：用假 Provider 离线驱动循环——模型响应取自
// 脚本，工具是确定性回声工具，不碰网络也不构造客户端。断言落在循环产生了哪些
// 消息、按什么顺序、又把哪几条交给了观察者。

const (
	loopToolName    = "echo"
	loopContextText = "客户这轮的输入"
	loopFinalText   = "最终答复"
)

// scriptedProvider 按脚本逐轮交出模型消息；脚本耗尽后交一条没有结果的流。
// inputs 记录每次 Stream 收到的消息序列副本，用来钉"工具结果回填给了哪一轮"。
type scriptedProvider struct {
	script []schema.Message
	calls  int
	inputs []schema.Messages
}

func (provider *scriptedProvider) Stream(
	_ context.Context, messages schema.Messages, _ schema.ToolDefinitions,
) ai.Stream {
	provider.inputs = append(provider.inputs, append(schema.Messages(nil), messages...))
	index := provider.calls
	provider.calls++
	if index >= len(provider.script) {
		return &scriptedStream{}
	}

	return &scriptedStream{message: provider.script[index]}
}

// scriptedStream 是一条没有增量事件的流：Result 把脚本里的消息原样交出来。
// 空脚本的流交出 nil 接口，这正是"模型什么都没产出"的形状。
type scriptedStream struct {
	message schema.Message
	err     error
}

func (stream *scriptedStream) Next() bool                      { return false }
func (stream *scriptedStream) Current() schema.StreamEvent     { return schema.StreamEvent{} }
func (stream *scriptedStream) Close() error                    { return nil }
func (stream *scriptedStream) Result() (schema.Message, error) { return stream.message, stream.err }

// echoTool 是循环测试的确定性工具：把参数里的 text 原样回显。同批的多次调用
// 因此各自带着自己能区分的正文。
type echoTool struct{}

func (echoTool) Definition() schema.ToolDefinition {
	return schema.ToolDefinition{
		Name:        loopToolName,
		Description: "回显参数里的 text",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"text": map[string]any{"type": "string"}},
			"required":   []string{"text"},
		},
	}
}

func (echoTool) Execute(
	_ context.Context, arguments json.RawMessage, _ *tools.UpdateEmitter,
) (*schema.ToolOutput, error) {
	var payload struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(arguments, &payload); err != nil {
		return nil, err
	}

	return &schema.ToolOutput{
		Content: schema.ContentBlocks{schema.TextBlock(payload.Text)},
	}, nil
}

// newEchoScheduler 注册回声工具并接上调度器：循环只消费调度器，不关心工具是谁。
func newEchoScheduler(t *testing.T) *tools.Scheduler {
	t.Helper()

	registry, err := tools.Register([]tools.Tool{echoTool{}})
	if err != nil {
		t.Fatalf("注册 echo 工具: %v", err)
	}
	registry.Freeze()

	return tools.NewScheduler(registry, 1, nil)
}

// loopContext 造循环的输入上下文：一条客户消息，等价于 ContextBuilder 的产物。
func loopContext() *Context {
	return &Context{Messages: schema.Messages{
		&schema.UserMessage{Content: schema.ContentBlocks{schema.TextBlock(loopContextText)}},
	}}
}

// assistantToolTurn 造一条要求调用 echo 的助手消息。
func assistantToolTurn(callID, text, arguments string) *schema.AssistantMessage {
	return &schema.AssistantMessage{
		Content: schema.ContentBlocks{schema.TextBlock(text)},
		ToolCalls: schema.ToolCalls{{
			ID: callID, Name: loopToolName, Arguments: json.RawMessage(arguments),
		}},
	}
}

// assertRoles 逐条比对消息角色；序列对不上就点出第一个错位的下标。
func assertRoles(t *testing.T, messages schema.Messages, want []schema.Role) {
	t.Helper()

	if len(messages) != len(want) {
		t.Fatalf("消息条数 = %d，想要 %d", len(messages), len(want))
	}
	for index, role := range want {
		if got := messages[index].Role(); got != role {
			t.Fatalf("messages[%d] 的角色 = %q，想要 %q", index, got, role)
		}
	}
}

// messageText 取一条消息的纯文本内容；内容块挂在具体类型上，只能断言类型再读。
func messageText(t *testing.T, message schema.Message) string {
	t.Helper()

	var blocks schema.ContentBlocks
	switch typed := message.(type) {
	case *schema.SystemMessage:
		blocks = typed.Content
	case *schema.UserMessage:
		blocks = typed.Content
	case *schema.AssistantMessage:
		blocks = typed.Content
	case *schema.ToolResultMessage:
		blocks = typed.Content
	default:
		t.Fatalf("消息的动态类型 = %T，没有可读的内容块", message)
	}
	text, err := blocks.Text()
	if err != nil {
		t.Fatalf("Content.Text: %v", err)
	}

	return text
}

// TestRunCallsToolThenAnswers 钉住一轮完整循环的消息序列与顺序：
// user → assistant(tool_calls) → tool_result → assistant(final)。
// 顺序同时从两侧钉：run 的返回值，以及第二轮调用前模型实际收到的上下文。
func TestRunCallsToolThenAnswers(t *testing.T) {
	const (
		toolCallID    = "call-echo-1"
		toolArguments = `{"text":"工具结果正文"}`
	)
	provider := &scriptedProvider{script: []schema.Message{
		assistantToolTurn(toolCallID, "先看一眼", toolArguments),
		&schema.AssistantMessage{Content: schema.ContentBlocks{schema.TextBlock(loopFinalText)}},
	}}
	loop := NewLoop(provider, WithScheduler(newEchoScheduler(t)))

	messages, err := loop.run(context.Background(), loopContext())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	assertRoles(t, messages, []schema.Role{
		schema.RoleUser, schema.RoleAssistant, schema.RoleTool, schema.RoleAssistant,
	})

	output, ok := messages[2].(*schema.ToolResultMessage)
	if !ok {
		t.Fatalf("messages[2] 的动态类型 = %T，想要 *schema.ToolResultMessage", messages[2])
	}
	if output.ToolCallID != toolCallID {
		t.Fatalf("工具结果的 ToolCallID = %q，想要 %q", output.ToolCallID, toolCallID)
	}
	if output.ToolName != loopToolName {
		t.Fatalf("工具结果的 ToolName = %q，想要 %q", output.ToolName, loopToolName)
	}
	if output.IsError {
		t.Fatal("成功的工具调用被标成了 IsError")
	}
	if got := messageText(t, output); got != "工具结果正文" {
		t.Fatalf("工具结果正文 = %q，想要 %q", got, "工具结果正文")
	}
	if got := messageText(t, messages[3]); got != loopFinalText {
		t.Fatalf("最终答复 = %q，想要 %q", got, loopFinalText)
	}

	// 第二轮调用前，上下文必须是"客户输入 + 发起调用的助手消息 + 工具结果"：
	// 工具结果紧跟在发起它的助手消息之后，两家协议都按这个顺序还原上下文。
	if provider.calls != 2 {
		t.Fatalf("模型调用次数 = %d，想要 2", provider.calls)
	}
	secondInput := provider.inputs[1]
	assertRoles(t, secondInput, []schema.Role{schema.RoleUser, schema.RoleAssistant, schema.RoleTool})
	if secondInput[1] != messages[1] {
		t.Fatalf("第二轮上下文里的助手消息 = %T，不是 run 产生的那条", secondInput[1])
	}
	if secondInput[2] != messages[2] {
		t.Fatalf("第二轮上下文里的工具结果 = %T，不是 run 产生的那条", secondInput[2])
	}
}

// TestRunKeepsEachToolResultSeparate 钉住同批多次工具调用的结果各自独立：
// 消息序列是值切片，若把手里的结果变量取址后逐条 append，所有元素会指向同一份
// 内存，历史看起来就是"每一条工具结果都长一样"。
func TestRunKeepsEachToolResultSeparate(t *testing.T) {
	provider := &scriptedProvider{script: []schema.Message{
		&schema.AssistantMessage{
			Content: schema.ContentBlocks{schema.TextBlock("并行看两个")},
			ToolCalls: schema.ToolCalls{
				{ID: "call-1", Name: loopToolName, Arguments: json.RawMessage(`{"text":"第一个结果"}`)},
				{ID: "call-2", Name: loopToolName, Arguments: json.RawMessage(`{"text":"第二个结果"}`)},
			},
		},
		&schema.AssistantMessage{Content: schema.ContentBlocks{schema.TextBlock(loopFinalText)}},
	}}
	loop := NewLoop(provider, WithScheduler(newEchoScheduler(t)))

	messages, err := loop.run(context.Background(), loopContext())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	assertRoles(t, messages, []schema.Role{
		schema.RoleUser, schema.RoleAssistant, schema.RoleTool, schema.RoleTool, schema.RoleAssistant,
	})

	for index, want := range []struct {
		callID string
		text   string
	}{{"call-1", "第一个结果"}, {"call-2", "第二个结果"}} {
		result, ok := messages[2+index].(*schema.ToolResultMessage)
		if !ok {
			t.Fatalf("messages[%d] 的动态类型 = %T，想要 *schema.ToolResultMessage", 2+index, messages[2+index])
		}
		if result.ToolCallID != want.callID {
			t.Fatalf("messages[%d] 的 ToolCallID = %q，想要 %q", 2+index, result.ToolCallID, want.callID)
		}
		if got := messageText(t, result); got != want.text {
			t.Fatalf("messages[%d] 的正文 = %q，想要 %q（两条工具结果串成了同一份）", 2+index, got, want.text)
		}
	}
}

// TestRunObserverReceivesEveryMessageInOrder 钉住观察者拿到的就是循环产生的
// 那几条、且同序：会话持久化靠它逐条落盘，少一条或顺序错了，重建出来的历史
// 就和模型看到的不一样。上下文是调用方给的，不由循环产生，所以观察者不收。
func TestRunObserverReceivesEveryMessageInOrder(t *testing.T) {
	runContext := loopContext()
	provider := &scriptedProvider{script: []schema.Message{
		assistantToolTurn("call-echo-1", "先看一眼", `{"text":"工具结果正文"}`),
		&schema.AssistantMessage{Content: schema.ContentBlocks{schema.TextBlock(loopFinalText)}},
	}}
	var observed schema.Messages
	loop := NewLoop(
		provider,
		WithScheduler(newEchoScheduler(t)),
		WithMessageObserver(func(message schema.Message) {
			observed = append(observed, message)
		}),
	)

	messages, err := loop.run(context.Background(), runContext)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	assertRoles(t, observed, []schema.Role{
		schema.RoleAssistant, schema.RoleTool, schema.RoleAssistant,
	})
	// 观察者收到的必须与返回值里"循环产生的那一段"逐条同源。
	produced := messages[len(runContext.Messages):]
	if len(observed) != len(produced) {
		t.Fatalf("观察者收到 %d 条，循环产生 %d 条", len(observed), len(produced))
	}
	for index := range produced {
		if observed[index] != produced[index] {
			t.Fatalf("观察者收到的第 %d 条 = %T，不是消息序列里的那一条",
				index, observed[index])
		}
	}
}

// TestRunStopsAtMaxTurns 钉住上限：模型每轮都要求调用工具时撞上限退出，
// 报 ErrRunLimitExceeded，而不是继续烧 token。
func TestRunStopsAtMaxTurns(t *testing.T) {
	const maxTurns = 2
	provider := &scriptedProvider{}
	// 脚本给到上限之后还有一轮：真跑过头了，模型调用次数会露出来。
	for turn := 0; turn <= maxTurns; turn++ {
		provider.script = append(provider.script,
			assistantToolTurn("call-echo-1", "还要调", `{"text":"工具结果正文"}`))
	}
	loop := NewLoop(provider, WithScheduler(newEchoScheduler(t)), WithMaxTurns(maxTurns))

	messages, err := loop.run(context.Background(), loopContext())
	if !pierrors.ErrRunLimitExceeded.Match(err) {
		t.Fatalf("错误码 = %d，想要 %d（err = %v）",
			pierrors.CodeOf(err), pierrors.ErrRunLimitExceeded.Code(), err)
	}
	if provider.calls != maxTurns {
		t.Fatalf("模型调用次数 = %d，想要 %d", provider.calls, maxTurns)
	}
	// 撞上限时返回的是已经产生的部分：上下文 + 每轮一条助手消息与一条工具结果。
	want := make([]schema.Role, 0, 1+2*maxTurns)
	want = append(want, schema.RoleUser)
	for turn := 0; turn < maxTurns; turn++ {
		want = append(want, schema.RoleAssistant, schema.RoleTool)
	}
	assertRoles(t, messages, want)
}

// TestRunStopsWhenContextCanceled 钉住取消：调用前已取消就一次模型调用都不发起；
// 跑到一半被取消则停下来报 ErrCanceled，并把已经产生的部分交回去。
func TestRunStopsWhenContextCanceled(t *testing.T) {
	cases := []struct {
		name         string
		cancelBefore bool
		cancelAfter  int
		wantCalls    int
		wantRoles    []schema.Role
	}{
		{
			name:         "调用前已取消",
			cancelBefore: true,
			wantCalls:    0,
			wantRoles:    []schema.Role{schema.RoleUser},
		},
		{
			name:        "工具结果之后取消",
			cancelAfter: 2,
			wantCalls:   1,
			wantRoles: []schema.Role{
				schema.RoleUser, schema.RoleAssistant, schema.RoleTool,
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			provider := &scriptedProvider{script: []schema.Message{
				assistantToolTurn("call-echo-1", "先看一眼", `{"text":"工具结果正文"}`),
				&schema.AssistantMessage{Content: schema.ContentBlocks{schema.TextBlock(loopFinalText)}},
			}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if testCase.cancelBefore {
				cancel()
			}
			var observed schema.Messages
			loop := NewLoop(
				provider,
				WithScheduler(newEchoScheduler(t)),
				WithMessageObserver(func(message schema.Message) {
					observed = append(observed, message)
					if len(observed) == testCase.cancelAfter {
						cancel()
					}
				}),
			)

			messages, err := loop.run(ctx, loopContext())
			if !pierrors.ErrCanceled.Match(err) {
				t.Fatalf("错误码 = %d，想要 %d（err = %v）",
					pierrors.CodeOf(err), pierrors.ErrCanceled.Code(), err)
			}
			if provider.calls != testCase.wantCalls {
				t.Fatalf("模型调用次数 = %d，想要 %d", provider.calls, testCase.wantCalls)
			}
			assertRoles(t, messages, testCase.wantRoles)
		})
	}
}

// TestRunRejectsNonAssistantModelMessage 钉住"模型响应必须是 assistant"这条不变量：
// 交回 nil 接口、或者交回别的具体类型，都按内部错误报，而不是读成"模型没有调用
// 工具"——那会让一次失败的调用看起来像正常收尾。
func TestRunRejectsNonAssistantModelMessage(t *testing.T) {
	cases := []struct {
		name    string
		message schema.Message
	}{
		{name: "没有结果（nil 接口）", message: nil},
		{name: "交回的是工具结果消息", message: &schema.ToolResultMessage{
			Content:    schema.ContentBlocks{schema.TextBlock("工具结果")},
			ToolCallID: "call-echo-1", ToolName: loopToolName,
		}},
		{name: "交回的是用户消息", message: &schema.UserMessage{
			Content: schema.ContentBlocks{schema.TextBlock("冒充用户")},
		}},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			provider := &scriptedProvider{script: []schema.Message{testCase.message}}
			var observed schema.Messages
			loop := NewLoop(
				provider,
				WithScheduler(newEchoScheduler(t)),
				WithMessageObserver(func(message schema.Message) {
					observed = append(observed, message)
				}),
			)

			messages, err := loop.run(context.Background(), loopContext())
			if !pierrors.ErrInternal.Match(err) {
				t.Fatalf("错误码 = %d，想要 %d（err = %v）",
					pierrors.CodeOf(err), pierrors.ErrInternal.Code(), err)
			}
			// 坏消息既不进消息序列，也不交给观察者：会话里不允许落下一条空载荷。
			assertRoles(t, messages, []schema.Role{schema.RoleUser})
			if len(observed) != 0 {
				t.Fatalf("观察者收到 %d 条，想要 0（坏消息一条都不该落进会话）", len(observed))
			}
		})
	}
}
