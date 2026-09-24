package pi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/PycMono/go-harness/pi/ai"
	"github.com/PycMono/go-harness/pi/ai/providers"
	pierrors "github.com/PycMono/go-harness/pi/error"
	"github.com/PycMono/go-harness/pi/schema"
	"github.com/PycMono/go-harness/pi/session"
	"github.com/PycMono/go-harness/pi/tools"
)

// 本文件钉住循环检测的三个层次：机制（循环在轮末开的那条钩子给出的现场）、
// 判据（一轮的签名是一个纯函数）、策略（连续几轮相同就终止，端到端真的带着
// 50001 停下来）。provider 桩与工具桩是本包第一批需要模型与工具的测试，
// 后来的插话测试直接用这一份，不另写。

// 第一组：钩子给出的现场。
//
// 钩子只在带工具调用的轮之后调用——模型不再要求调用工具时运行已经结束，
// "还要不要开下一轮"这个问题不存在。结果消息必须与调用按下标对齐：判据按
// 下标把结果配到调用上，错位会让 A 的调用配上 B 的结果。

func TestAfterTurnReportsEachToolTurn(t *testing.T) {
	calls := schema.ToolCalls{
		{ID: "call-1", Name: "echo", Arguments: json.RawMessage(`{"text":"a"}`)},
		{ID: "call-2", Name: "echo", Arguments: json.RawMessage(`{"text":"b"}`)},
	}
	provider := &stubProvider{script: []*schema.AssistantMessage{
		assistantWithCalls(calls),
		assistantPlain("结束"),
	}}

	var reports []TurnReport
	loop := newTestLoop(t, provider, WithAfterTurn(func(_ context.Context, report TurnReport) error {
		reports = append(reports, report)

		return nil
	}))

	if _, err := loop.run(context.Background(), testRunContext()); err != nil {
		t.Fatalf("运行不该报错：%v", err)
	}

	if len(reports) != 1 {
		t.Fatalf("只有带工具调用的那一轮该问钩子，拿到 %d 次", len(reports))
	}
	if reports[0].Index != 0 {
		t.Errorf("轮次序号：想要 0，拿到 %d", reports[0].Index)
	}
	if len(reports[0].ToolResults) != len(calls) {
		t.Fatalf("结果条数：想要 %d，拿到 %d", len(calls), len(reports[0].ToolResults))
	}
	// 按下标对齐：第 i 条结果回填的是第 i 次调用。用不同的参数让两条调用的
	// 结果可区分，否则错位了也看不出来。
	for index := range calls {
		result := reports[0].ToolResults[index]
		if text, want := result.Blocks()[0].Text, string(calls[index].Arguments); text != want {
			t.Errorf("第 %d 条结果配错了调用：想要 %s，拿到 %s", index, want, text)
		}
	}
}

// 第二组：一轮的签名。它是这条判据的全部，三条要求各有一组用例——参数的
// 写法无关、顺序照抄、结果也要进来。

func TestSignatureEquality(t *testing.T) {
	cases := []struct {
		name         string
		left         schema.ToolCalls
		leftResults  schema.Messages
		right        schema.ToolCalls
		rightResults schema.Messages
		wantEqual    bool
	}{
		{
			name:      "参数键序不同算同一轮",
			left:      callsOf(callSpec{"echo", `{"a":1,"b":2}`}),
			right:     callsOf(callSpec{"echo", `{"b":2,"a":1}`}),
			wantEqual: true,
		},
		{
			name:  "调用顺序不同算两轮",
			left:  callsOf(callSpec{"echo", `{"a":1}`}, callSpec{"echo", `{"a":2}`}),
			right: callsOf(callSpec{"echo", `{"a":2}`}, callSpec{"echo", `{"a":1}`}),
		},
		{
			name:  "参数不同算两轮",
			left:  callsOf(callSpec{"echo", `{"a":1}`}),
			right: callsOf(callSpec{"echo", `{"a":2}`}),
		},
		{
			// 这条就是轮询场景：连着问同一个状态，第三次问出"完成"。判据
			// 带上结果之后它不再是"没进展"。
			name:         "调用相同结果不同算两轮",
			left:         callsOf(callSpec{"echo", `{"a":1}`}),
			leftResults:  toolResults("运行中"),
			right:        callsOf(callSpec{"echo", `{"a":1}`}),
			rightResults: toolResults("完成"),
		},
		{
			name:         "调用相同结果相同算同一轮",
			left:         callsOf(callSpec{"echo", `{"a":1}`}),
			leftResults:  toolResults("完成"),
			right:        callsOf(callSpec{"echo", `{"a":1}`}),
			rightResults: toolResults("完成"),
			wantEqual:    true,
		},
		{
			// 默认解析进 float64 会把这两个数塌成一个，宽容方向是误报。
			name:  "超过 2^53 的相邻整数算两轮",
			left:  callsOf(callSpec{"echo", `{"n":9007199254740993}`}),
			right: callsOf(callSpec{"echo", `{"n":9007199254740992}`}),
		},
		{
			// 取舍的代价，不是缺陷：UseNumber 保字面量，代价是 1 与 1.0 算
			// 两轮。写进用例免得将来被人当 bug 改掉。
			name:  "数字写法 1 与 1.0 算两轮",
			left:  callsOf(callSpec{"echo", `{"n":1}`}),
			right: callsOf(callSpec{"echo", `{"n":1.0}`}),
		},
		{
			// 已知的粗：图片块只按"媒体类型 + 载荷长度"比，同类型同长度的
			// 两张图会撞成同一条结果。撞了的后果是多停一次运行。
			name:         "图片结果同类型同长度算同一轮",
			left:         callsOf(callSpec{"echo", `{"a":1}`}),
			leftResults:  schema.Messages{imageResult("AAAA", "image/png")},
			right:        callsOf(callSpec{"echo", `{"a":1}`}),
			rightResults: schema.Messages{imageResult("BBBB", "image/png")},
			wantEqual:    true,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			left := signatureOf(testCase.left, testCase.leftResults)
			right := signatureOf(testCase.right, testCase.rightResults)
			if equal := left == right; equal != testCase.wantEqual {
				t.Errorf("签名相等应为 %v，实际 %v\n左：%q\n右：%q",
					testCase.wantEqual, equal, left, right)
			}
		})
	}
}

// 模型给过截断或拼接错位的 JSON 时原样返回：它是模型给的字符串，比不出错，
// 也不该在这里报错。原样返回还有一层意思——不能编回去（编回去会丢掉后面那
// 段，让两个不同的参数撞成同一个签名）。
func TestSignatureKeepsUnparseableArguments(t *testing.T) {
	cases := []struct {
		name       string
		arguments  string
		wantSubstr string
	}{
		{name: "截断的 JSON", arguments: `{"a":`, wantSubstr: `{"a":`},
		{name: "值后面跟着多余内容", arguments: `{"a":1} x`, wantSubstr: `{"a":1} x`},
		{name: "两个值拼在一起", arguments: `{"a":1}{"b":2}`, wantSubstr: `{"a":1}{"b":2}`},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			signature := signatureOf(callsOf(callSpec{"echo", testCase.arguments}), nil)
			if !strings.Contains(signature, testCase.wantSubstr) {
				t.Errorf("参数该原样出现在签名里：%q 不在 %q 里", testCase.wantSubstr, signature)
			}
		})
	}

	// 拼接错位的那条不能与它前面的合法值撞成同一个签名。
	mangled := signatureOf(callsOf(callSpec{"echo", `{"a":1}{"b":2}`}), nil)
	valid := signatureOf(callsOf(callSpec{"echo", `{"a":1}`}), nil)
	if mangled == valid {
		t.Errorf("拼接错位的参数与截断到第一个值的结果撞了：%q", mangled)
	}
}

// 结果条数比调用条数少时不 panic：这一轮退化成只看调用。兜底分支要有一条
// 用例，否则它是死代码；不这么退化的话就得越界取结果，而钩子在运行路径上，
// panic 会把整次运行连同已产生的消息一起丢掉。
func TestSignatureToleratesMissingResults(t *testing.T) {
	calls := callsOf(callSpec{"echo", `{"a":1}`}, callSpec{"echo", `{"a":2}`})
	want := "echo:{\"a\":1}=>完成\necho:{\"a\":2}"

	if got := signatureOf(calls, toolResults("完成")); got != want {
		t.Errorf("少一条结果时该退化成只看调用：\n想要 %q\n拿到 %q", want, got)
	}
}

// 签名会进错误文案，所以要能截断。按 UTF-8 边界切：切出半个汉字比截短更糟。
func TestClipSignature(t *testing.T) {
	if short := clipSignature("abc"); short != "abc" {
		t.Errorf("短签名该原样返回，拿到 %q", short)
	}

	long := strings.Repeat("汉", 200)
	clipped := clipSignature(long)
	if !strings.HasSuffix(clipped, "…") {
		t.Errorf("长签名该带省略号，拿到 %q", clipped)
	}
	if !utf8.ValidString(clipped) {
		t.Error("截断后不是合法 UTF-8：切出了半个汉字")
	}
	if len(clipped) > 240+len("…") {
		t.Errorf("截断长度超了：%d 字节", len(clipped))
	}
}

// 第三组：连续计数。判据只有一个状态——上一轮的签名和它连续出现了几次。
// 这里全部是纯函数与结构体，不需要 provider。

func TestLoopGuardStopsOnThirdIdenticalTurn(t *testing.T) {
	guard := &loopGuard{limit: 3}
	for turn := 0; turn < 2; turn++ {
		if err := guard.observe(reportAt(turn, `{"a":1}`, "完成")); err != nil {
			t.Fatalf("第 %d 轮不该判循环：%v", turn, err)
		}
	}

	err := guard.observe(reportAt(2, `{"a":1}`, "完成"))
	if err == nil {
		t.Fatal("连续三轮相同该判循环")
	}
	if code := pierrors.CodeOf(err); code != 50001 {
		t.Errorf("错误码：想要 50001，拿到 %d", code)
	}
	if !errors.Is(err, pierrors.ErrRunLoopDetected) {
		t.Errorf("该是 ErrRunLoopDetected，拿到 %v", err)
	}
	// 文案里要能看出停在第几轮、停在哪个签名上——日志不必再拼现场。
	if !strings.Contains(err.Error(), "3") || !strings.Contains(err.Error(), `echo:{"a":1}`) {
		t.Errorf("文案该带轮数与签名，拿到 %q", err.Error())
	}
}

// 判到循环之后不是只报一次就哑：只要还在重复，每一轮都报，否则上层把这一次
// 错误当成可忽略的告警就又能跑下去了。
func TestLoopGuardKeepsReporting(t *testing.T) {
	guard := &loopGuard{limit: 3}
	for turn := 0; turn < 5; turn++ {
		err := guard.observe(reportAt(turn, `{"a":1}`, "完成"))
		if turn < 2 && err != nil {
			t.Fatalf("第 %d 轮不该判循环：%v", turn, err)
		}
		if turn >= 2 && pierrors.CodeOf(err) != 50001 {
			t.Fatalf("第 %d 轮该判循环，拿到 %v", turn, err)
		}
	}
}

func TestLoopGuardResetsOnDifferentSignature(t *testing.T) {
	guard := &loopGuard{limit: 3}
	for turn := 0; turn < 2; turn++ {
		guard.observe(reportAt(turn, `{"a":1}`, "完成"))
	}

	// 换一轮别的：计数从头开始，原签名的那两轮不再有效。
	if err := guard.observe(reportAt(2, `{"a":2}`, "完成")); err != nil {
		t.Fatalf("换了签名不该判循环：%v", err)
	}
	first := 3
	for turn := first; turn < first+2; turn++ {
		if err := guard.observe(reportAt(turn, `{"a":1}`, "完成")); err != nil {
			t.Fatalf("中断之后第 %d 轮不该判循环：%v", turn, err)
		}
	}
	if code := pierrors.CodeOf(guard.observe(reportAt(first+2, `{"a":1}`, "完成"))); code != 50001 {
		t.Errorf("中断之后要重新数满三轮才报，拿到 %d", code)
	}
}

// 结果在变就不算循环：轮询类工具靠反复问同一个东西推进，第三次问出"完成"的
// 那一刻不该被当成卡住。这是整条判据要躲的误报方向。
func TestLoopGuardIgnoresChangingResults(t *testing.T) {
	guard := &loopGuard{limit: 3}
	for turn, result := range []string{"运行中", "运行中", "完成", "完成"} {
		if err := guard.observe(reportAt(turn, `{"a":1}`, result)); err != nil {
			t.Fatalf("结果在变不该判循环（第 %d 轮）：%v", turn, err)
		}
	}
}

// limit <= 0 表示关闭。装配不做条件判断，关不关由值决定，所以这条要在判据
// 自己身上钉住。
func TestLoopGuardDisabledByNonPositiveLimit(t *testing.T) {
	for _, limit := range []int{0, -1} {
		guard := &loopGuard{limit: limit}
		for turn := 0; turn < 10; turn++ {
			if err := guard.observe(reportAt(turn, `{"a":1}`, "完成")); err != nil {
				t.Fatalf("limit=%d 时不该判循环（第 %d 轮）：%v", limit, turn, err)
			}
		}
	}
}

// 一个 Run 一轮账：跨 Run 留着计数的话，两次毫不相干的运行各调一次同一个
// 工具就会被算成重复。
func TestLoopGuardResetClearsCount(t *testing.T) {
	guard := &loopGuard{limit: 3}
	for turn := 0; turn < 2; turn++ {
		guard.observe(reportAt(turn, `{"a":1}`, "完成"))
	}
	guard.reset()

	if err := guard.observe(reportAt(0, `{"a":1}`, "完成")); err != nil {
		t.Fatalf("重置之后该从头数：%v", err)
	}
}

// 阈值旋钮：0 取默认值，负数关闭，正数就是它自己。与 MaxTurns 的"<= 0 取
// 默认值"不同——关闭需要一个表达方式。
func TestLoopGuardLimit(t *testing.T) {
	cases := []struct {
		configured int
		want       int
	}{
		{configured: 0, want: defaultLoopGuardTurns},
		{configured: 5, want: 5},
		{configured: -1, want: -1},
	}
	for _, testCase := range cases {
		if got := loopGuardLimit(testCase.configured); got != testCase.want {
			t.Errorf("LoopGuardTurns=%d：想要 %d，拿到 %d", testCase.configured, testCase.want, got)
		}
	}
}

// 第四组：端到端。走 newAgent 这条装配路径，验的是"装上去之后真的会停"，
// 以及最要紧的那条反例——轮询不该被误停。

func TestLoopGuardStopsRun(t *testing.T) {
	call := schema.ToolCall{ID: "call-1", Name: "echo", Arguments: json.RawMessage(`{"a":1}`)}
	// 脚本只有一条，用完一直重复：模型永远在要同一个工具、拿同一份结果。
	provider := &stubProvider{script: []*schema.AssistantMessage{
		assistantWithCalls(schema.ToolCalls{call}),
	}}
	agent := newTestAgent(t, provider, nil)

	output, err := agent.Run(context.Background(), &RunInput{Prompt: "开始"})
	if err == nil {
		t.Fatal("连续三轮完全相同的轮该终止运行")
	}
	if code := pierrors.CodeOf(err); code != 50001 {
		t.Errorf("错误码：想要 50001，拿到 %d（%v）", code, err)
	}
	if output == nil || len(output.Messages()) == 0 {
		t.Fatal("终止时已产生的消息该照常返回")
	}

	turns := 0
	for _, message := range output.Messages() {
		if message.Role() == schema.RoleAssistant {
			turns++
		}
	}
	if turns != defaultLoopGuardTurns {
		t.Errorf("该在第 %d 轮停下来，实际跑了 %d 轮", defaultLoopGuardTurns, turns)
	}
}

// 关掉之后同样的形态要能跑完：这条与上一条只差一个配置，用来钉住"负数关闭"
// 这条语义真的走到了判据里，而不是被装配处忽略。
func TestLoopGuardDisabledRunKeepsGoing(t *testing.T) {
	call := schema.ToolCall{ID: "call-1", Name: "echo", Arguments: json.RawMessage(`{"a":1}`)}
	provider := &stubProvider{script: []*schema.AssistantMessage{
		assistantWithCalls(schema.ToolCalls{call}),
		assistantWithCalls(schema.ToolCalls{call}),
		assistantWithCalls(schema.ToolCalls{call}),
		assistantPlain("好了"),
	}}
	agent := newTestAgent(t, provider, func(opts *Options) { opts.LoopGuardTurns = -1 })

	if _, err := agent.Run(context.Background(), &RunInput{Prompt: "开始"}); err != nil {
		t.Fatalf("关掉检测之后不该报错：%v", err)
	}
}

// 这条比上面几条都重要：它守的是"不误停"。轮询类工具连着三轮问同一个东西，
// 第三次问出"完成"——调用一直一样，结果在变，不该被判成循环。
func TestLoopGuardKeepsPollingRunGoing(t *testing.T) {
	poll := &pollingTool{replies: []string{"运行中", "运行中", "完成"}}
	call := schema.ToolCall{ID: "call-1", Name: "poll", Arguments: json.RawMessage(`{}`)}
	provider := &stubProvider{script: []*schema.AssistantMessage{
		assistantWithCalls(schema.ToolCalls{call}),
		assistantWithCalls(schema.ToolCalls{call}),
		assistantWithCalls(schema.ToolCalls{call}),
		assistantPlain("好了"),
	}}
	agent := newTestAgent(t, provider, func(opts *Options) {
		opts.Tools = []tools.Tool{poll}
	})

	output, err := agent.Run(context.Background(), &RunInput{Prompt: "开始"})
	if err != nil {
		t.Fatalf("轮询不该被误停：%v", err)
	}
	if len(output.Messages()) == 0 {
		t.Fatal("运行该产出消息")
	}
	// 最后一次请求里要有第三次轮询的结果：模型看得见"完成"，运行才是正常结束的。
	last := provider.requests[len(provider.requests)-1]
	seen := false
	for _, message := range last {
		for _, block := range message.Blocks() {
			if strings.Contains(block.Text, "完成") {
				seen = true
			}
		}
	}
	if !seen {
		t.Error("第三次轮询的结果该进到请求里")
	}
}

// 桩与夹具。

// pollingTool 每次返回不同的输出，但调用参数始终一样——轮询类工具就是这个
// 形态（问任务状态、等构建结束）。它不在默认工具里，只在需要它的用例里注册。
type pollingTool struct {
	replies []string
	calls   int
}

func (t *pollingTool) Definition() schema.ToolDefinition {
	return schema.ToolDefinition{
		Name:        "poll",
		Description: "每次返回脚本里的下一条",
		InputSchema: map[string]any{
			"type":                 "object",
			"properties":           map[string]any{},
			"additionalProperties": true,
		},
	}
}

func (t *pollingTool) Execute(
	_ context.Context, _ json.RawMessage, _ *tools.UpdateEmitter,
) (*schema.ToolOutput, error) {
	reply := t.replies[len(t.replies)-1]
	if t.calls < len(t.replies) {
		reply = t.replies[t.calls]
	}
	t.calls++

	return &schema.ToolOutput{
		Content: schema.ContentBlocks{schema.TextBlock(reply)},
	}, nil
}

// newTestAgent 走 newAgent 这条装配路径（NewAgent 与测试共用它）。ContextWindow
// 留 0，窗口就是关的，压不压不掺进这几条用例。
func newTestAgent(t *testing.T, provider ai.Provider, configure func(*Options)) *Agent {
	t.Helper()

	workDir := t.TempDir()
	// 工作区没有 AGENTS.md 时系统提示词组装会报 10006，先给一份。
	if err := os.WriteFile(filepath.Join(workDir, "AGENTS.md"), []byte("测试用工作区。\n"), 0o644); err != nil {
		t.Fatalf("写 AGENTS.md 失败：%v", err)
	}

	opts := &Options{
		WorkDir:         workDir,
		ProviderOptions: &providers.Options{},
		Session:         session.InMemory(),
	}
	if configure != nil {
		configure(opts)
	}

	agent, err := newAgent(context.Background(), provider, opts)
	if err != nil {
		t.Fatalf("装配 agent 失败：%v", err)
	}
	t.Cleanup(func() { _ = agent.Close(context.Background()) })

	return agent
}

// echoTool 把参数原样吐回来当输出。参数不同则结果不同，正好用来验按下标
// 对齐；参数相同则结果逐字节相同，正好用来造"整轮没有变化"。
type echoTool struct{}

func (echoTool) Definition() schema.ToolDefinition {
	return schema.ToolDefinition{
		Name:         "echo",
		Description:  "把参数原样返回",
		ParallelSafe: true,
		InputSchema: map[string]any{
			"type":                 "object",
			"properties":           map[string]any{},
			"additionalProperties": true,
		},
	}
}

func (echoTool) Execute(
	_ context.Context, arguments json.RawMessage, _ *tools.UpdateEmitter,
) (*schema.ToolOutput, error) {
	return &schema.ToolOutput{
		Content: schema.ContentBlocks{schema.TextBlock(string(arguments))},
	}, nil
}

// reportAt 造一轮的现场：一次 echo 调用加它的结果。判据只读 ToolCalls 与
// ToolResults，Message 一起填上是为了这份现场与循环里真给出的那份同形。
func reportAt(index int, arguments string, result string) TurnReport {
	return TurnReport{
		Index:       index,
		Message:     assistantWithCalls(callsOf(callSpec{"echo", arguments})),
		ToolResults: toolResults(result),
	}
}

// callSpec 是一次工具调用的最小描述：判据只看名称与参数，ID 不进签名。
type callSpec struct {
	name string
	args string
}

func callsOf(specs ...callSpec) schema.ToolCalls {
	calls := make(schema.ToolCalls, 0, len(specs))
	for index, spec := range specs {
		calls = append(calls, schema.ToolCall{
			ID:        fmt.Sprintf("call-%d", index),
			Name:      spec.name,
			Arguments: json.RawMessage(spec.args),
		})
	}

	return calls
}

func toolResults(texts ...string) schema.Messages {
	messages := make(schema.Messages, 0, len(texts))
	for index, text := range texts {
		messages = append(messages, &schema.ToolResultMessage{
			Content:    schema.ContentBlocks{schema.TextBlock(text)},
			ToolCallID: fmt.Sprintf("call-%d", index),
			ToolName:   "echo",
		})
	}

	return messages
}

func imageResult(data string, mimeType string) schema.Message {
	return &schema.ToolResultMessage{
		Content: schema.ContentBlocks{
			{Type: schema.ContentTypeImage, Image: &schema.ImageContent{Data: data, MIMEType: mimeType}},
		},
		ToolCallID: "call-0",
		ToolName:   "echo",
	}
}

func newTestLoop(t *testing.T, provider ai.Provider, options ...LoopOption) *Loop {
	t.Helper()

	registry, err := tools.Register([]tools.Tool{echoTool{}})
	if err != nil {
		t.Fatalf("注册测试工具失败：%v", err)
	}
	options = append(
		[]LoopOption{WithScheduler(tools.NewScheduler(registry, 1, nil))},
		options...,
	)

	return NewLoop(provider, options...)
}

// testRunContext 是最小可用的上下文：一条本轮输入，历史段为空。
func testRunContext() *Context {
	return &Context{
		Messages: schema.Messages{
			&schema.UserMessage{Content: schema.ContentBlocks{schema.TextBlock("开始")}},
		},
		CurrentInputIndex: 0,
	}
}

func assistantWithCalls(calls schema.ToolCalls) *schema.AssistantMessage {
	return &schema.AssistantMessage{ToolCalls: calls, FinishReason: schema.FinishReasonToolUse}
}

func assistantPlain(text string) *schema.AssistantMessage {
	return &schema.AssistantMessage{
		Content:      schema.ContentBlocks{schema.TextBlock(text)},
		FinishReason: schema.FinishReasonStop,
	}
}

// stubProvider 按脚本吐响应：第 n 次 Stream 调用返回脚本第 n 条，脚本用完
// 之后一直重复最后一条。它同时记下每一次请求的消息序列，供断言注入位置。
type stubProvider struct {
	script   []*schema.AssistantMessage
	requests []schema.Messages
	index    int
}

func (p *stubProvider) Stream(
	_ context.Context, messages schema.Messages, _ schema.ToolDefinitions,
) ai.Stream {
	p.requests = append(p.requests, messages)
	response := (*schema.AssistantMessage)(nil)
	if len(p.script) > 0 {
		response = p.script[len(p.script)-1]
	}
	if p.index < len(p.script) {
		response = p.script[p.index]
	}
	p.index++

	return &stubStream{message: response}
}

type stubStream struct {
	message *schema.AssistantMessage
	read    bool
}

func (s *stubStream) Next() bool {
	if s.read {
		return false
	}
	s.read = true

	return true
}

func (s *stubStream) Current() schema.StreamEvent { return schema.StreamEvent{} }

func (s *stubStream) Result() (*schema.AssistantMessage, error) { return s.message, nil }

func (s *stubStream) Close() error { return nil }
