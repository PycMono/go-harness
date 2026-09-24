package pi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/PycMono/go-harness/pi/ai"
	"github.com/PycMono/go-harness/pi/ai/providers"
	pierrors "github.com/PycMono/go-harness/pi/error"
	"github.com/PycMono/go-harness/pi/schema"
	"github.com/PycMono/go-harness/pi/session"
	"github.com/PycMono/go-harness/pi/tools"
)

// 本文件是插话通道（Loop.Steer / drainSteering 与 run 里两处取走点）的测试。

// TestSteerRejectsUnusableMessages 三个拒绝理由都走 10002，且被拒绝的消息不留半条。
func TestSteerRejectsUnusableMessages(t *testing.T) {
	cases := []struct {
		name    string
		message schema.Message
	}{
		{"接口本身为 nil", nil},
		{"装着一个类型化 nil", (*schema.UserMessage)(nil)},
		{
			"角色不是 user",
			&schema.SystemMessage{Content: schema.ContentBlocks{schema.TextBlock("换方向")}},
		},
		{
			"消息自身不合法",
			&schema.UserMessage{Content: schema.ContentBlocks{schema.ImageBlock("ftp://example.com/a.png")}},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			loop := NewLoop(nil)
			err := loop.Steer(c.message)
			if err == nil {
				t.Fatal("Steer 应当拒绝这条消息，实际返回 nil")
			}
			if code := pierrors.CodeOf(err); code != pierrors.ErrRequestInvalid.Code() {
				t.Fatalf("错误码应为 %d，实际 %d（%v）",
					pierrors.ErrRequestInvalid.Code(), code, err)
			}
			if pending := loop.drainSteering(); pending != nil {
				t.Fatalf("被拒绝的消息不该留在队列里，实际留下 %d 条", len(pending))
			}
		})
	}
}

// TestSteerCopiesQueuedMessage 入队的是副本：之后怎么改原件都不影响已排队的那条，
// 图片连 Image 指向的内容一起复制。
func TestSteerCopiesQueuedMessage(t *testing.T) {
	t.Run("文本", func(t *testing.T) {
		loop := NewLoop(nil)
		message := &schema.UserMessage{Content: schema.ContentBlocks{schema.TextBlock("往左走")}}
		if err := loop.Steer(message); err != nil {
			t.Fatalf("Steer 失败：%v", err)
		}

		message.Content[0].Text = "往右走"

		pending := loop.drainSteering()
		if len(pending) != 1 {
			t.Fatalf("队列里应当有 1 条，实际 %d 条", len(pending))
		}
		if text, err := pending[0].Blocks().Text(); err != nil || text != "往左走" {
			t.Fatalf("排队的那条应保持入队时的内容，实际 %q（%v）", text, err)
		}
	})

	t.Run("图片", func(t *testing.T) {
		loop := NewLoop(nil)
		image := schema.ImageContent{Data: "入队时的数据", MIMEType: "image/png"}
		message := &schema.UserMessage{Content: schema.ContentBlocks{
			{Type: schema.ContentTypeImage, Image: &image},
		}}
		if err := loop.Steer(message); err != nil {
			t.Fatalf("Steer 失败：%v", err)
		}

		image.Data = "改过之后的数据"

		pending := loop.drainSteering()
		if len(pending) != 1 {
			t.Fatalf("队列里应当有 1 条，实际 %d 条", len(pending))
		}
		queued := pending[0].Blocks()[0].Image
		if queued == nil || queued.Data != "入队时的数据" {
			t.Fatalf("排队的图片应保持入队时的数据，实际 %+v", queued)
		}
	})
}

// TestDrainSteeringTakesAllAndClears 一次取走全部、按写入顺序，取走即清空。
func TestDrainSteeringTakesAllAndClears(t *testing.T) {
	loop := NewLoop(nil)
	for _, text := range []string{"第一条", "第二条", "第三条"} {
		if err := loop.Steer(&schema.UserMessage{
			Content: schema.ContentBlocks{schema.TextBlock(text)},
		}); err != nil {
			t.Fatalf("Steer 失败：%v", err)
		}
	}

	pending := loop.drainSteering()
	if len(pending) != 3 {
		t.Fatalf("一次应当取走 3 条，实际 %d 条", len(pending))
	}
	want := []string{"第一条", "第二条", "第三条"}
	for index, message := range pending {
		text, err := message.Blocks().Text()
		if err != nil || text != want[index] {
			t.Fatalf("第 %d 条应为 %q，实际 %q（%v）", index, want[index], text, err)
		}
	}

	if again := loop.drainSteering(); again != nil {
		t.Fatalf("取走之后队列应当为空，实际还有 %d 条", len(again))
	}
}

// TestSteeringDeliveredBeforeTurnHook 取走点一在 beforeTurn 之前：钩子里应当
// 已经看到排队的插话（也就是它进了 turn.extra()，参与这一轮的窗口判定），
// 运行开始前写入的消息走的也是这条路。
func TestSteeringDeliveredBeforeTurnHook(t *testing.T) {
	provider := &stubProvider{script: []*schema.AssistantMessage{assistantPlain("收到")}}

	var seen schema.Messages
	loop := newTestLoop(t, provider, WithBeforeTurn(
		func(_ context.Context, turn Turn) (schema.Messages, error) {
			seen = append(schema.Messages(nil), turn.Produced...)

			return nil, nil
		},
	))

	if err := loop.Steer(&schema.UserMessage{Content: schema.ContentBlocks{schema.TextBlock("往左走")}}); err != nil {
		t.Fatalf("Steer 失败：%v", err)
	}
	if _, err := loop.run(context.Background(), testRunContext()); err != nil {
		t.Fatalf("run 失败：%v", err)
	}

	if texts := textsOf(seen); len(texts) != 1 || texts[0] != "往左走" {
		t.Fatalf("钩子里应当已经看到那条插话，实际 %v", texts)
	}
	if texts := textsOf(provider.requests[0]); len(texts) != 2 || texts[1] != "往左走" {
		t.Fatalf("第一次请求应当带着那条插话，实际 %v", texts)
	}
}

// TestSteeringDeliveredAfterBeforeTurnHook 取走点二在 beforeTurn 之后：钩子
// 执行期间（压缩要调一次模型，慢的话是几秒）写进来的消息在本轮就交付，不等到
// 下一轮。只有取走点一时这条会晚一轮。
func TestSteeringDeliveredAfterBeforeTurnHook(t *testing.T) {
	provider := &stubProvider{script: []*schema.AssistantMessage{
		assistantWithCall("call-0", "echo", `{"a":1}`),
		assistantPlain("收到"),
	}}

	var loop *Loop
	loop = newTestLoop(t, provider, WithBeforeTurn(
		func(_ context.Context, _ Turn) (schema.Messages, error) {
			return nil, loop.Steer(&schema.UserMessage{
				Content: schema.ContentBlocks{schema.TextBlock("压缩期间写的")},
			})
		},
	))

	if _, err := loop.run(context.Background(), testRunContext()); err != nil {
		t.Fatalf("run 失败：%v", err)
	}

	if texts := textsOf(provider.requests[0]); len(texts) != 2 || texts[1] != "压缩期间写的" {
		t.Fatalf("第一次请求里就该有钩子期间写的那条，实际 %v", texts)
	}
}

// 桩与夹具。

// stubProvider 按脚本吐助手消息，并记下每一次请求的消息序列——后面几条用例的
// 断言都吃这一份记录。脚本用完之后重复最后一条，免得测试要为"多跑的那一轮"
// 补脚本。
type stubProvider struct {
	script   []*schema.AssistantMessage
	requests []schema.Messages
	index    int
}

func (p *stubProvider) Stream(_ context.Context, messages schema.Messages, _ schema.ToolDefinitions) ai.Stream {
	p.requests = append(p.requests, append(schema.Messages(nil), messages...))
	message := p.script[len(p.script)-1]
	if p.index < len(p.script) {
		message = p.script[p.index]
	}
	p.index++

	return &stubStream{message: message}
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

// steeringProvider 在桩之上加一个"每次请求 provider 之前"的回调。Run 是同步
// 阻塞的，测试要在运行期间写入，只能在循环调用 provider 的这个当口动手。
type steeringProvider struct {
	*stubProvider
	beforeStream func(request int)
}

func (p *steeringProvider) Stream(
	ctx context.Context, messages schema.Messages, definitions schema.ToolDefinitions,
) ai.Stream {
	if p.beforeStream != nil {
		p.beforeStream(p.stubProvider.index)
	}

	return p.stubProvider.Stream(ctx, messages, definitions)
}

// newTestAgent 走与 NewAgent 相同的装配路径（agent.go 的 newAgent），只把
// provider 换成桩。上下文窗口取 0（不触发压缩），会话用内存实现。
func newTestAgent(t *testing.T, provider ai.Provider, configure func(*Options)) *Agent {
	t.Helper()
	workDir := t.TempDir()
	// ContextBuilder 要求工作目录里有 AGENTS.md，缺了会以 10006 失败。
	if err := os.WriteFile(filepath.Join(workDir, "AGENTS.md"), []byte("测试用"), 0o644); err != nil {
		t.Fatalf("写 AGENTS.md 失败：%v", err)
	}

	options := &Options{
		WorkDir:         workDir,
		ProviderOptions: &providers.Options{},
		Session:         session.InMemory(),
		Tools:           []tools.Tool{echoTool{}},
	}
	if configure != nil {
		configure(options)
	}
	agent, err := newAgent(context.Background(), provider, options)
	if err != nil {
		t.Fatalf("装配 agent 失败：%v", err)
	}
	t.Cleanup(func() { _ = agent.Close(context.Background()) })

	return agent
}

// echoTool 把参数原样吐回来当输出，用来让一轮里产生一条工具结果。
type echoTool struct{}

func (echoTool) Definition() schema.ToolDefinition {
	return schema.ToolDefinition{
		Name:        "echo",
		Description: "把参数原样返回",
		// InputSchema 必填：注册时会对着 metaschema 校验，nil 会以 30008 失败。
		InputSchema: map[string]any{
			"type":                 "object",
			"properties":           map[string]any{},
			"additionalProperties": true,
		},
		ParallelSafe: true,
	}
}

func (echoTool) Execute(
	_ context.Context, arguments json.RawMessage, _ *tools.UpdateEmitter,
) (*schema.ToolOutput, error) {
	return &schema.ToolOutput{Content: schema.ContentBlocks{schema.TextBlock(string(arguments))}}, nil
}

// newTestLoop 建一个带 echoTool 的循环：脚本里要有工具调用时，循环得能真的把
// 它执行掉、把结果写回序列。
func newTestLoop(t *testing.T, provider ai.Provider, options ...LoopOption) *Loop {
	t.Helper()
	registry, err := tools.Register([]tools.Tool{echoTool{}})
	if err != nil {
		t.Fatalf("注册工具失败：%v", err)
	}
	registry.Freeze()

	return NewLoop(provider, append(options,
		WithScheduler(tools.NewScheduler(registry, 1, nil)))...)
}

// testRunContext 是一轮运行的起点：一条用户输入、没有历史、没有工具描述。
func testRunContext() *Context {
	return &Context{
		Messages: schema.Messages{
			&schema.UserMessage{Content: schema.ContentBlocks{schema.TextBlock("开始")}},
		},
		CurrentInputIndex: 0,
	}
}

func assistantPlain(text string) *schema.AssistantMessage {
	return &schema.AssistantMessage{
		Content:      schema.ContentBlocks{schema.TextBlock(text)},
		FinishReason: schema.FinishReasonStop,
	}
}

func assistantWithCall(id, name, arguments string) *schema.AssistantMessage {
	return &schema.AssistantMessage{
		ToolCalls:    schema.ToolCalls{{ID: id, Name: name, Arguments: json.RawMessage(arguments)}},
		FinishReason: schema.FinishReasonToolUse,
	}
}

func textsOf(messages schema.Messages) []string {
	result := make([]string, 0, len(messages))
	for _, message := range messages {
		text, err := message.Blocks().Text()
		if err != nil {
			result = append(result, "<非纯文本>")
			continue
		}
		result = append(result, text)
	}

	return result
}

// TestSteeringLandsAfterToolResults 端到端：第二轮的工具结果落盘之后、下一次
// 请求发出之前写入，它在第三次请求里紧跟在工具结果之后——工具结果必须紧跟发起
// 它的助手消息，插话排在这一对之后。
func TestSteeringLandsAfterToolResults(t *testing.T) {
	provider := &steeringProvider{stubProvider: &stubProvider{
		script: []*schema.AssistantMessage{
			assistantWithCall("call-0", "echo", `{"a":1}`),
			assistantWithCall("call-1", "echo", `{"a":2}`),
			assistantPlain("收到"),
		},
	}}
	agent := newTestAgent(t, provider, nil)
	provider.beforeStream = func(request int) {
		// 第二次请求正在等模型返回的当口写入，交付落在下一轮的取走点——
		// 那时第二轮的工具结果已经写回，第三次请求还没发出。
		if request != 1 {
			return
		}
		if err := agent.Steer(&schema.UserMessage{
			Content: schema.ContentBlocks{schema.TextBlock("往左走")},
		}); err != nil {
			t.Errorf("Steer 失败：%v", err)
		}
	}

	if _, err := agent.Run(context.Background(), &RunInput{Prompt: "开始"}); err != nil {
		t.Fatalf("Run 失败：%v", err)
	}

	texts := textsOf(provider.requests[2])
	// 请求是本轮固定段（本轮输入、系统提示词）在前，之后是这一轮已经产生的：
	// 两条助手消息与它们各自的工具结果，最后才是插话。这里只看结尾那三条——
	// 工具结果紧跟发起它的助手消息，插话紧跟工具结果。
	if len(texts) != 7 {
		t.Fatalf("第三次请求应当有 7 条消息，实际 %d 条：%v", len(texts), texts)
	}
	if texts[4] != "" || texts[5] != `{"a":2}` || texts[6] != "往左走" {
		t.Fatalf("插话应当紧跟第二条工具结果，实际结尾是 %v", texts[3:])
	}
}

// TestSteeringPersistsToSession 写入的插话走的是与模型消息相同的落盘路径，
// 会话里能重建出来，顺序也落在工具结果之后。
func TestSteeringPersistsToSession(t *testing.T) {
	provider := &steeringProvider{stubProvider: &stubProvider{
		script: []*schema.AssistantMessage{
			assistantWithCall("call-0", "echo", `{"a":1}`),
			assistantPlain("收到"),
		},
	}}
	agent := newTestAgent(t, provider, nil)
	provider.beforeStream = func(request int) {
		if request != 0 {
			return
		}
		if err := agent.Steer(&schema.UserMessage{
			Content: schema.ContentBlocks{schema.TextBlock("往左走")},
		}); err != nil {
			t.Errorf("Steer 失败：%v", err)
		}
	}

	if _, err := agent.Run(context.Background(), &RunInput{Prompt: "开始"}); err != nil {
		t.Fatalf("Run 失败：%v", err)
	}

	persisted := textsOf(agent.session.BuildMessages())
	// 顺序断言走消息序列本身：插话夹在工具结果与它之后那条模型消息之间——
	// 这就是"工具结果必须紧跟发起它的助手消息 + 插话排在轮次边界上"。
	want := []string{"开始", "", `{"a":1}`, "往左走", "收到"}
	if len(persisted) != len(want) {
		t.Fatalf("会话里应当有 %d 条消息，实际 %d 条：%v", len(want), len(persisted), persisted)
	}
	for index := range want {
		if persisted[index] != want[index] {
			t.Fatalf("会话第 %d 条应为 %q，实际 %q（全序列 %v）",
				index, want[index], persisted[index], persisted)
		}
	}
}

// TestSteeringBeforeRun 运行开始前写入的消息在第一次模型调用之前交付。
func TestSteeringBeforeRun(t *testing.T) {
	provider := &steeringProvider{stubProvider: &stubProvider{
		script: []*schema.AssistantMessage{assistantPlain("收到")},
	}}
	agent := newTestAgent(t, provider, nil)

	// 对外只有 Steerer 这一个入口：这里就按调用方拿到的手柄用法走一遍。
	steerer := Steerer(agent)
	message := &schema.UserMessage{Content: schema.ContentBlocks{schema.TextBlock("往左走")}}
	if err := steerer.Steer(message); err != nil {
		t.Fatalf("Steer 失败：%v", err)
	}

	// 入队之后改原件：请求里出现的仍是入队时的那一份（决策 13 的请求级断言）。
	message.Content[0].Text = "往右走"

	if _, err := agent.Run(context.Background(), &RunInput{Prompt: "开始"}); err != nil {
		t.Fatalf("Run 失败：%v", err)
	}

	if texts := textsOf(provider.requests[0]); len(texts) != 3 || texts[2] != "往左走" {
		t.Fatalf("第一次请求应当带着入队时的那条插话，实际 %v", texts)
	}
}

// TestSteeringKeptForNextRun 运行结束之后写入的消息留给下一次 Run。
func TestSteeringKeptForNextRun(t *testing.T) {
	provider := &steeringProvider{stubProvider: &stubProvider{
		script: []*schema.AssistantMessage{assistantPlain("收到")},
	}}
	agent := newTestAgent(t, provider, nil)

	if _, err := agent.Run(context.Background(), &RunInput{Prompt: "第一轮"}); err != nil {
		t.Fatalf("第一次 Run 失败：%v", err)
	}
	if err := agent.Steer(&schema.UserMessage{
		Content: schema.ContentBlocks{schema.TextBlock("第二轮再走")},
	}); err != nil {
		t.Fatalf("Steer 失败：%v", err)
	}
	if _, err := agent.Run(context.Background(), &RunInput{Prompt: "第二轮"}); err != nil {
		t.Fatalf("第二次 Run 失败：%v", err)
	}

	request := provider.requests[len(provider.requests)-1]
	texts := textsOf(request)
	if len(texts) == 0 || texts[len(texts)-1] != "第二轮再走" {
		t.Fatalf("第二次 Run 的第一次请求应当带着那条插话，实际 %v", texts)
	}
}

// TestSteeringUnderConcurrency 十个 goroutine 各写一条，一条都不能丢。
func TestSteeringUnderConcurrency(t *testing.T) {
	const writers = 10
	provider := &steeringProvider{stubProvider: &stubProvider{
		script: []*schema.AssistantMessage{
			assistantWithCall("call-0", "echo", `{"a":1}`),
			assistantPlain("收到"),
		},
	}}
	agent := newTestAgent(t, provider, nil)
	provider.beforeStream = func(request int) {
		if request != 0 {
			return
		}
		var group sync.WaitGroup
		for index := range writers {
			group.Add(1)
			go func() {
				defer group.Done()
				if err := agent.Steer(&schema.UserMessage{
					Content: schema.ContentBlocks{schema.TextBlock(fmt.Sprintf("第 %d 条", index))},
				}); err != nil {
					t.Errorf("Steer 失败：%v", err)
				}
			}()
		}
		group.Wait()
	}

	if _, err := agent.Run(context.Background(), &RunInput{Prompt: "开始"}); err != nil {
		t.Fatalf("Run 失败：%v", err)
	}

	// 十条都应当在第二次请求里出现（第一次请求发出的时候它们还没写完），
	// 且按写入顺序排在那条工具结果之后。
	texts := textsOf(provider.requests[1])
	if len(texts) != 4+writers {
		t.Fatalf("第二次请求应当有 %d 条消息，实际 %d 条：%v", 4+writers, len(texts), texts)
	}
	seen := make(map[string]bool, writers)
	for _, text := range texts[4:] {
		seen[text] = true
	}
	for index := range writers {
		if want := fmt.Sprintf("第 %d 条", index); !seen[want] {
			t.Fatalf("第 %d 条插话丢了，第二次请求里只有 %v", index, texts)
		}
	}
}

// TestRunReportsRunErrorAndWriteErrorTogether 运行错与会话写入错同时发生时两个
// 都要在错误链上：只报运行错会吞掉"盘上的历史缺了一段"，只报写入错会吞掉模型
// 为什么停。CodeOf 拿到的仍是运行错的码——errors.Join 先左后右，运行错在左。
func TestRunReportsRunErrorAndWriteErrorTogether(t *testing.T) {
	provider := &steeringProvider{stubProvider: &stubProvider{
		// 每一轮都是同一个调用、同一条结果：跑到循环检测的阈值就收尾。
		script: []*schema.AssistantMessage{assistantWithCall("call-0", "echo", `{"a":1}`)},
	}}

	var sessionPath string
	agent := newTestAgent(t, provider, func(options *Options) {
		manager, err := session.OpenOrCreate(t.TempDir(), options.WorkDir, "steering-dual-error")
		if err != nil {
			t.Fatalf("建会话失败：%v", err)
		}
		options.Session = manager
		sessionPath = manager.Path()
	})
	provider.beforeStream = func(request int) {
		// 让接下来的会话写入没有落脚点。删文件而不是改权限：以 root 跑测试时
		// 只读权限拦不住写。
		if request != 0 {
			return
		}
		if err := os.Remove(sessionPath); err != nil {
			t.Fatalf("删会话文件失败：%v", err)
		}
	}

	_, err := agent.Run(context.Background(), &RunInput{Prompt: "开始"})
	if err == nil {
		t.Fatal("运行错与写入错同时发生，Run 应当报错")
	}
	if code := pierrors.CodeOf(err); code != pierrors.ErrRunLoopDetected.Code() {
		t.Fatalf("错误码应当是运行错 %d，实际 %d：%v",
			pierrors.ErrRunLoopDetected.Code(), code, err)
	}
	if !errors.Is(err, pierrors.ErrSessionAppendFailed) {
		t.Fatalf("会话写入失败应当也在错误链上：%v", err)
	}
}
