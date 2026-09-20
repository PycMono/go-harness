package pi

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PycMono/go-harness/pi/ai"
	"github.com/PycMono/go-harness/pi/schema"
	"github.com/PycMono/go-harness/pi/tools"
	"github.com/PycMono/go-harness/pi/tools/impl"
)

// fakeStream 像真 provider 一样按 start → 逐个 text delta → done 的顺序吐事件，
// 走完之后 Result 才有效。
type fakeStream struct {
	deltas  []string
	result  *schema.Message
	current schema.StreamEvent
	index   int
	started bool
	done    bool
}

func (s *fakeStream) Next() bool {
	switch {
	case !s.started:
		s.started = true
		s.current = schema.StreamEvent{Type: schema.StreamEventStart}
	case s.index < len(s.deltas):
		s.current = schema.StreamEvent{Type: schema.StreamEventTextDelta, TextDelta: s.deltas[s.index]}
		s.index++
	case !s.done:
		s.done = true
		s.current = schema.StreamEvent{Type: schema.StreamEventDone}
	default:
		return false
	}

	return true
}

func (s *fakeStream) Current() schema.StreamEvent { return s.current }

func (s *fakeStream) Result() (*schema.Message, error) { return s.result, nil }

func (s *fakeStream) Close() error { return nil }

// fakeResponse 是一次预置的模型响应：流式吐出的增量文本，以及流走完后的完整消息。
type fakeResponse struct {
	deltas  []string
	message *schema.Message
}

// fakeProvider 按顺序返回预置的响应，并记下每次调用收到的对话历史。
type fakeProvider struct {
	responses []fakeResponse
	calls     []schema.Messages
}

func (p *fakeProvider) Stream(_ context.Context, messages schema.Messages, _ schema.ToolDefinitions) ai.Stream {
	p.calls = append(p.calls, append(schema.Messages(nil), messages...))
	if len(p.responses) == 0 {
		return &fakeStream{}
	}
	response := p.responses[0]
	p.responses = p.responses[1:]

	return &fakeStream{deltas: response.deltas, result: response.message}
}

// response 是构造预置响应的小工具：消息内容同时按单字符切成增量文本。
func response(message *schema.Message) fakeResponse {
	text, err := message.Content.Text()
	if err != nil || text == "" {
		return fakeResponse{message: message}
	}

	deltas := make([]string, 0, len(text))
	for _, r := range text {
		deltas = append(deltas, string(r))
	}

	return fakeResponse{deltas: deltas, message: message}
}

func TestLoopExecutesToolsAndStops(t *testing.T) {
	workDir := t.TempDir()
	registry, err := tools.Register(impl.NewDefaultTools(workDir))
	if err != nil {
		t.Fatalf("注册工具失败: %v", err)
	}

	provider := &fakeProvider{responses: []fakeResponse{
		{message: &schema.Message{Role: schema.RoleAssistant, ToolCalls: schema.ToolCalls{{
			ID:        "call-1",
			Name:      "write",
			Arguments: []byte(`{"path":"hello.txt","content":"你好\n"}`),
		}}}},
		response(&schema.Message{Role: schema.RoleAssistant, Content: schema.ContentBlocks{schema.TextBlock("写好了")}}),
	}}

	var streamed strings.Builder
	loop := NewLoop(provider,
		WithScheduler(tools.NewScheduler(registry, 4, nil)),
		WithTextObserver(func(delta string) { streamed.WriteString(delta) }),
	)
	runContext := &Context{
		Messages: schema.Messages{
			{Role: schema.RoleUser, Content: schema.ContentBlocks{schema.TextBlock("写个文件")}},
		},
		Tools: loop.definitions(),
	}

	messages, err := loop.run(context.Background(), runContext)
	if err != nil {
		t.Fatalf("运行失败: %v", err)
	}

	content, err := os.ReadFile(filepath.Join(workDir, "hello.txt"))
	if err != nil {
		t.Fatalf("工具没有真的写文件: %v", err)
	}
	if string(content) != "你好\n" {
		t.Fatalf("文件内容不对: %q", content)
	}

	// 上下文 + 模型消息 + 工具结果 + 最终模型消息
	if len(messages) != 4 {
		t.Fatalf("消息序列长度应为 4，实际 %d", len(messages))
	}
	if messages[1].Role != schema.RoleAssistant || len(messages[1].ToolCalls) != 1 {
		t.Fatalf("第二条应为带工具调用的助手消息: %+v", messages[1])
	}
	if messages[2].Role != schema.RoleTool || messages[2].ToolCallID != "call-1" || messages[2].IsError {
		t.Fatalf("第三条应为成功的工具结果: %+v", messages[2])
	}
	if messages[3].Role != schema.RoleAssistant {
		t.Fatalf("最后一条应为模型答复: %+v", messages[3])
	}

	// 增量文本必须真的透出来：如果只是把流读干净、丢掉 delta，这里是空的。
	if streamed.String() != "写好了" {
		t.Fatalf("增量文本没有透出，实际收到 %q", streamed.String())
	}

	// 第二轮必须带上工具结果，否则模型看不到执行反馈。
	if len(provider.calls) != 2 {
		t.Fatalf("应调用模型两次，实际 %d", len(provider.calls))
	}
	if len(provider.calls[1]) != 3 {
		t.Fatalf("第二轮上下文应包含工具结果，实际 %d 条", len(provider.calls[1]))
	}
	if len(provider.calls[0]) != 1 {
		t.Fatalf("第一轮上下文不应含工具消息，实际 %d 条", len(provider.calls[0]))
	}
}

func TestLoopKeepsToolErrorsInContext(t *testing.T) {
	registry, err := tools.Register(impl.NewDefaultTools(t.TempDir()))
	if err != nil {
		t.Fatalf("注册工具失败: %v", err)
	}

	provider := &fakeProvider{responses: []fakeResponse{
		{message: &schema.Message{Role: schema.RoleAssistant, ToolCalls: schema.ToolCalls{{
			ID:        "call-1",
			Name:      "read",
			Arguments: []byte(`{"path":"不存在.txt"}`),
		}}}},
		response(&schema.Message{Role: schema.RoleAssistant, Content: schema.ContentBlocks{schema.TextBlock("读不到")}}),
	}}

	loop := NewLoop(provider, WithScheduler(tools.NewScheduler(registry, 4, nil)))
	messages, err := loop.run(context.Background(), &Context{Tools: loop.definitions()})
	if err != nil {
		t.Fatalf("工具失败不应中断循环: %v", err)
	}
	if len(messages) != 3 {
		t.Fatalf("消息序列长度应为 3，实际 %d", len(messages))
	}
	if !messages[1].IsError {
		t.Fatalf("工具失败应标记 IsError: %+v", messages[1])
	}
	text, _ := messages[1].Content.Text()
	if !strings.Contains(text, "30002") {
		t.Fatalf("工具错误应带错误码，实际 %q", text)
	}
}

func TestLoopStopsAtMaxTurns(t *testing.T) {
	registry, err := tools.Register(impl.NewDefaultTools(t.TempDir()))
	if err != nil {
		t.Fatalf("注册工具失败: %v", err)
	}

	// 模型每一轮都要求执行工具，永远不收敛。
	always := &schema.Message{Role: schema.RoleAssistant, ToolCalls: schema.ToolCalls{{
		ID:        "call",
		Name:      "bash",
		Arguments: []byte(`{"command":"true"}`),
	}}}
	provider := &fakeProvider{responses: []fakeResponse{
		{message: always}, {message: always}, {message: always}, {message: always}, {message: always},
	}}

	loop := NewLoop(provider,
		WithScheduler(tools.NewScheduler(registry, 4, nil)),
		WithMaxTurns(3),
	)
	if _, err := loop.run(context.Background(), &Context{Tools: loop.definitions()}); err == nil {
		t.Fatal("撞到轮次上限应报错")
	}
	if len(provider.calls) != 3 {
		t.Fatalf("应正好调用模型 3 次，实际 %d", len(provider.calls))
	}
}
