package pi

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PycMono/go-harness/pi/ai"
	"github.com/PycMono/go-harness/pi/ai/providers"
	pierrors "github.com/PycMono/go-harness/pi/error"
	"github.com/PycMono/go-harness/pi/observability"
	"github.com/PycMono/go-harness/pi/schema"
	"github.com/PycMono/go-harness/pi/session"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// 本文件是唯一一个 agent 层的观测测试：run Span 与压缩 Span 只有装配出一个真
// Agent 才看得到。假 provider 从这里注入（newAgent 直接收 ai.Provider，绕过
// providers.New），所以一条真请求都不会发出去。
//
// 复用 pi/loop_test.go 里的 installRecordingProvider、attributeMap、
// fakeProvider、fakeStream、textResponse、userMessage——同包测试文件共享这些
// 名字，不要在这里重复定义。

// agentContextWindow 是测试用的上下文窗口。它的余量是按 NewWindow 的规则折过的：
// Reserve=16384 不变，KeepRecent 由 20000 折半成 10000，于是
// Enabled() 报 true（32768-16384 = 16384 > requestHeadroomTokens 8192），
// 触发线落在 16384 上——上下文估算超过 16384 就压。
const agentContextWindow = 32768

// writeAgentsFile 在 workDir 里放一份最小的 AGENTS.md。
//
// 这不是可选的装饰：Agent.Run → prepareRunContext → contextBuilder.Build →
// SystemPrompt 会读它，缺了就直接以 ErrAgentsFileMissing 失败，压缩那条路
// 一步都走不到。反过来说，"不放 AGENTS.md"正是构造"准备阶段失败"用例的手段。
func writeAgentsFile(t *testing.T, workDir string) {
	t.Helper()
	if err := os.WriteFile(
		filepath.Join(workDir, "AGENTS.md"), []byte("# 测试工程\n"), 0o600,
	); err != nil {
		t.Fatalf("写 AGENTS.md 失败: %v", err)
	}
}

// newTestAgent 装配一个真的 Agent。ProviderOptions 的其余字段（BaseURL、APIKey
// 等）在 newAgent 这条路上不会被读——只有 ContextWindow 有用——但整个结构体不能
// 是 nil，newAgent 会直接取它的字段。
func newTestAgent(
	t *testing.T,
	workDir string,
	provider ai.Provider,
	sessions *session.Manager,
) *Agent {
	t.Helper()
	agent, err := newAgent(context.Background(), provider, &Options{
		WorkDir: workDir,
		ProviderOptions: &providers.Options{
			ID:            "platform-x",
			Model:         "model-y",
			Protocol:      providers.ProtocolOpenAI,
			ContextWindow: agentContextWindow,
		},
		Session: sessions,
	})
	if err != nil {
		t.Fatalf("newAgent() error = %v", err)
	}

	return agent
}

// spanByName 按名字取一个结束了的 Span，数量不对就当场失败。
func spanByName(t *testing.T, spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	var found sdktrace.ReadOnlySpan
	for _, span := range spans {
		if span.Name() != name {
			continue
		}
		if found != nil {
			t.Fatalf("Span %q 出现了不止一次", name)
		}
		found = span
	}
	if found == nil {
		t.Fatalf("没找到 Span %q（实际有 %d 个：%v）", name, len(spans), spanNames(spans))
	}

	return found
}

func spanNames(spans []sdktrace.ReadOnlySpan) []string {
	names := make([]string, 0, len(spans))
	for _, span := range spans {
		names = append(names, span.Name())
	}

	return names
}

// turnSpanAt 取第 index 轮的 turn Span。一次运行里 turn Span 必然有多个，
// 名字认不出来，只能按 turn_index 认——所以 spanByName 用不到它身上。
func turnSpanAt(t *testing.T, spans []sdktrace.ReadOnlySpan, index int64) sdktrace.ReadOnlySpan {
	t.Helper()
	for _, span := range spans {
		if span.Name() != observability.SpanNameTurn {
			continue
		}
		if attributeMap(span.Attributes())[observability.AttrTurnIndex].AsInt64() == index {
			return span
		}
	}
	t.Fatalf("没找到 turn_index = %d 的 turn Span（实际有 %v）", index, spanNames(spans))

	return nil
}

// TestRunSpanCoversSuccessfulRun 钉住整棵树的形状：一次 Run 一个 pi.run，底下
// 每轮一个 pi.turn，turn 底下一次模型请求一个 chat。
func TestRunSpanCoversSuccessfulRun(t *testing.T) {
	recorder := installRecordingProvider(t)
	workDir := t.TempDir()
	writeAgentsFile(t, workDir)
	agent := newTestAgent(t, workDir, &fakeProvider{
		responses: []*schema.AssistantMessage{textResponse("你好")},
	}, session.InMemory())

	output, err := agent.Run(context.Background(), &RunInput{Prompt: "在吗"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if output == nil {
		t.Fatal("Run() 返回了 nil 输出")
	}

	spans := recorder.Ended()
	if len(spans) != 3 {
		t.Fatalf("Span 数 = %d（%v），want 3", len(spans), spanNames(spans))
	}
	runSpan := spanByName(t, spans, observability.SpanNameRun)
	if runSpan.Status().Code == codes.Error {
		t.Error("成功的运行不该把 run Span 标红")
	}
	attrs := attributeMap(runSpan.Attributes())
	if got, want := attrs["gen_ai.operation.name"].AsString(), "invoke_agent"; got != want {
		t.Errorf("gen_ai.operation.name = %q, want %q", got, want)
	}
	if got, want := attrs[observability.AttrGenAIAgentName].AsString(), observability.AgentName; got != want {
		t.Errorf("%s = %q, want %q", observability.AttrGenAIAgentName, got, want)
	}

	// 父传播：turn 挂在 run 底下，chat 挂在 turn 底下。
	turnSpan := spanByName(t, spans, observability.SpanNameTurn)
	if got, want := turnSpan.Parent().SpanID(), runSpan.SpanContext().SpanID(); got != want {
		t.Errorf("turn Span 的父不是 run Span")
	}
	chatSpan := spanByName(t, spans, observability.ChatSpanName("model-y"))
	if got, want := chatSpan.Parent().SpanID(), turnSpan.SpanContext().SpanID(); got != want {
		t.Errorf("chat Span 的父不是 turn Span")
	}
}

// TestRunSpanCoversPrepareFailure 是决策 17 前半的回归测试。Span 开在
// prepareRunContext 之前：不开的话这里一个 Span 都没有——Run 报了错、trace 上
// 却什么都没有，而"准备阶段为什么失败"恰恰是最要看 trace 的时候。
func TestRunSpanCoversPrepareFailure(t *testing.T) {
	recorder := installRecordingProvider(t)
	// 故意不写 AGENTS.md：contextBuilder.Build 会以 ErrAgentsFileMissing 失败。
	agent := newTestAgent(t, t.TempDir(), &fakeProvider{
		responses: []*schema.AssistantMessage{textResponse("不该跑到")},
	}, session.InMemory())

	output, err := agent.Run(context.Background(), &RunInput{Prompt: "在吗"})
	if err == nil {
		t.Fatal("没有 AGENTS.md 时 Run 应当报错")
	}
	if output != nil {
		t.Fatalf("准备阶段失败应当返回 nil 输出、运行中途失败才返回部分消息，得到 %v", output)
	}

	runSpan := spanByName(t, recorder.Ended(), observability.SpanNameRun)
	if runSpan.Status().Code != codes.Error {
		t.Error("准备失败时 run Span 应当红着")
	}
}

// TestRunSpanExcludesEntryValidation 钉住 Span 的起点：三个入口校验都在 Span
// 之外——那是调用方写错了参数，这一次运行根本没开始。
func TestRunSpanExcludesEntryValidation(t *testing.T) {
	cases := []struct {
		name  string
		input *RunInput
	}{
		{"输入为空", &RunInput{Prompt: "   "}},
		{"输入为 nil", nil},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := installRecordingProvider(t)
			workDir := t.TempDir()
			writeAgentsFile(t, workDir)
			agent := newTestAgent(t, workDir, &fakeProvider{
				responses: []*schema.AssistantMessage{textResponse("不该跑到")},
			}, session.InMemory())

			if _, err := agent.Run(context.Background(), testCase.input); err == nil {
				t.Fatal("入口校验应当失败")
			}
			if spans := recorder.Ended(); len(spans) != 0 {
				t.Fatalf("入口校验失败不该留下 run Span，得到 %v", spanNames(spans))
			}
		})
	}
}

// TestRunSpanCoversSessionWriteFailure 是决策 17 后半的回归测试：会话写入失败
// 是红的错误、绿 Span，正好是"部分失败记成成功"。
//
// 制造失败的办法：假 provider 在"模型返回"这一刻往会话文件尾巴上加一个字节，
// 于是紧接着写助手消息时 sessionFile.checkUnchanged 发现文件比记录的长了，
// Append 报 ErrSessionParentMismatch（pi/session/file.go:173-176）。这是仓库
// 既有的多写入者防护，不用为测试新开一个口子。
func TestRunSpanCoversSessionWriteFailure(t *testing.T) {
	recorder := installRecordingProvider(t)
	workDir := t.TempDir()
	writeAgentsFile(t, workDir)

	root := t.TempDir()
	sessions, err := session.OpenOrCreate(root, workDir, "session-write-failure")
	if err != nil {
		t.Fatalf("OpenOrCreate() error = %v", err)
	}

	provider := &fakeProvider{
		responses: []*schema.AssistantMessage{textResponse("你好")},
		onStream: func() {
			// 此刻盘上已经有这个会话了：OpenOrCreate 写了首行，prepareRunContext
			// 又把本轮输入写了下去，所以 checkUnchanged 记的长度非零。
			// 覆盖成一个字节，长度就对不上——截断到多少都行，它只比长度。
			if err := os.WriteFile(sessions.Path(), []byte("x"), 0o600); err != nil {
				t.Fatalf("改动会话文件失败: %v", err)
			}
		},
	}
	agent := newTestAgent(t, workDir, provider, sessions)

	output, err := agent.Run(context.Background(), &RunInput{Prompt: "在吗"})
	if err == nil {
		t.Fatal("会话写入失败时 Run 应当报错")
	}
	if pierrors.CodeOf(err) == 0 {
		t.Errorf("会话写入失败的错误应当带稳定码，得到 %v", err)
	}
	if output == nil {
		t.Fatal("运行本身跑完了，应当返回已经产生的消息序列")
	}

	runSpan := spanByName(t, recorder.Ended(), observability.SpanNameRun)
	if runSpan.Status().Code != codes.Error {
		t.Error("会话写入失败时 run Span 应当红着")
	}
}

// 下面的压缩用例共用一个历史。触发线是 agentContextWindow 折出来的 16384
// （见 agentContextWindow 的注释），这份历史正好踩在线上、又留得下一个非空的
// 摘要段：
//
//	u1                    最老的一条，会被摘要掉
//	assistant(40000 个 "a", Usage.InputTokens=20000)
//	                      保留段的第一条。真内容是 40000 个 ASCII 字符 ≈ 10000
//	                      token，正好够 findCutPoint 从新往旧数满 KeepRecent
//	                      (10000)；Usage 那 20000 是估算的基数，让 ContextTokens
//	                      报 20001 > 16384。用法当基数是最省的造法——不用真堆
//	                      两万 token 的正文。
//	u2                    叶子
//
// 计划成立时 TokensBefore = 20001，Summarize = [u1]。

// seededSession 造上面那份历史，返回会话。
func seededSession(t *testing.T) *session.Manager {
	t.Helper()
	sessions := session.InMemory()
	appendEntry(t, sessions, &schema.UserMessage{
		Content: schema.ContentBlocks{schema.TextBlock("u1")},
	})
	appendEntry(t, sessions, &schema.AssistantMessage{
		Content:      schema.ContentBlocks{schema.TextBlock(strings.Repeat("a", 40000))},
		Usage:        &schema.Usage{InputTokens: 20000},
		FinishReason: schema.FinishReasonStop,
	})
	appendEntry(t, sessions, &schema.UserMessage{
		Content: schema.ContentBlocks{schema.TextBlock("u2")},
	})

	return sessions
}

func appendEntry(t *testing.T, sessions *session.Manager, message schema.Message) {
	t.Helper()
	if err := sessions.Append(session.Entry{Type: session.EntryMessage, Message: message}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
}

// TestCompactionSpanNotOpenedBelowTriggerLine 是决策 13 的回归测试：Span 开在
// 函数顶上时这个断言会挂，而且挂的方式是"每轮多一个空 Span"——不会有人注意到。
func TestCompactionSpanNotOpenedBelowTriggerLine(t *testing.T) {
	recorder := installRecordingProvider(t)
	workDir := t.TempDir()
	writeAgentsFile(t, workDir)
	// 空会话：越不过触发线。
	agent := newTestAgent(t, workDir, &fakeProvider{
		responses: []*schema.AssistantMessage{textResponse("你好")},
	}, session.InMemory())

	if _, err := agent.Run(context.Background(), &RunInput{Prompt: "在吗"}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	for _, span := range recorder.Ended() {
		if span.Name() == observability.SpanNameCompaction {
			t.Fatal("没越过触发线却开了压缩 Span")
		}
	}
}

// TestCompactionSpanNotOpenedWithoutPlan 是第二个提前返回：越过了触发线但计划
// 不成立（保留区没数满，摘要段是空的），同样不该留 Span。
func TestCompactionSpanNotOpenedWithoutPlan(t *testing.T) {
	recorder := installRecordingProvider(t)

	sessions := session.InMemory()
	appendEntry(t, sessions, &schema.UserMessage{
		Content: schema.ContentBlocks{schema.TextBlock("u1")},
	})
	// Usage 大、正文小：ContextTokens 报 20001 越过触发线，但保留区从新往旧
	// 数不满 KeepRecent，findCutPoint 退到起点，摘要段为空 ⇒ 计划不成立。
	appendEntry(t, sessions, &schema.AssistantMessage{
		Content:      schema.ContentBlocks{schema.TextBlock("a1")},
		Usage:        &schema.Usage{InputTokens: 20000},
		FinishReason: schema.FinishReasonStop,
	})

	workDir := t.TempDir()
	writeAgentsFile(t, workDir)
	agent := newTestAgent(t, workDir, &fakeProvider{
		responses: []*schema.AssistantMessage{textResponse("你好")},
	}, sessions)

	if _, err := agent.Run(context.Background(), &RunInput{Prompt: "在吗"}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	for _, span := range recorder.Ended() {
		if span.Name() == observability.SpanNameCompaction {
			t.Fatal("计划不成立却开了压缩 Span")
		}
	}
}

// TestCompactionSpanOnSuccess 钉住压缩成功时的形状：恰好一个压缩 Span、带压缩
// 前的体量、绿的，且它下面只挂一个 chat Span（摘要请求）——本轮的正式请求那个
// chat Span 是它的兄弟，不是子节点。
func TestCompactionSpanOnSuccess(t *testing.T) {
	recorder := installRecordingProvider(t)
	workDir := t.TempDir()
	writeAgentsFile(t, workDir)
	sessions := seededSession(t)
	agent := newTestAgent(t, workDir, &fakeProvider{
		responses: []*schema.AssistantMessage{
			textResponse("## Goal\n把测试写完"), // 第一次 Stream：摘要请求
			textResponse("你好"),               // 第二次 Stream：正式请求
		},
	}, sessions)

	if _, err := agent.Run(context.Background(), &RunInput{Prompt: "在吗"}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	spans := recorder.Ended()
	compactionSpan := spanByName(t, spans, observability.SpanNameCompaction)
	if compactionSpan.Status().Code == codes.Error {
		t.Errorf("压缩成功时不该标红：%s", compactionSpan.Status().Description)
	}
	if got, want := attributeMap(compactionSpan.Attributes())[observability.AttrCompactionBeforeTokens].AsInt64(), int64(20001); got != want {
		t.Errorf("%s = %d, want %d", observability.AttrCompactionBeforeTokens, got, want)
	}

	// 压缩 Span 的子节点恰好一个 chat（摘要请求）。本轮的正式请求是它的兄弟。
	children := 0
	for _, span := range spans {
		if span.Parent().SpanID() == compactionSpan.SpanContext().SpanID() {
			children++
		}
	}
	if children != 1 {
		t.Fatalf("压缩 Span 的子节点数 = %d, want 1（本轮的正式请求是它的兄弟，不是子节点）", children)
	}
	// 摘要请求与正式请求是两个 chat Span，总共两个——压缩 Span 那一层是用来
	// 区分它们的，少一层就看不出哪个是压缩。
	chats := 0
	for _, span := range spans {
		if span.Name() == observability.ChatSpanName("model-y") {
			chats++
		}
	}
	if chats != 2 {
		t.Fatalf("chat Span 数 = %d, want 2（摘要 + 正式请求）", chats)
	}
}

// TestCompactionSpanRedButRunContinues 是决策 15：压缩失败时 Span 标红，而这一
// 轮照常跑下去、Run 的返回值是 (output, nil)。它同时锁住了"Span 标红"与
// "错误被吞"这两个看起来矛盾的行为。
func TestCompactionSpanRedButRunContinues(t *testing.T) {
	recorder := installRecordingProvider(t)
	workDir := t.TempDir()
	writeAgentsFile(t, workDir)
	agent := newTestAgent(t, workDir, &fakeProvider{
		responses: []*schema.AssistantMessage{
			// 摘要请求：正文为空 ⇒ summarize 报 ErrCompactionFailed("摘要为空")。
			// Usage 给一个空结构体，让响应先过计量的严格校验、走到文本那一步——
			// 不带 Usage 的话会先被计量以 20000 拦下，那样测的就不是"摘要为空"了。
			{Usage: &schema.Usage{}},
			textResponse("你好"),
		},
	}, seededSession(t))

	output, err := agent.Run(context.Background(), &RunInput{Prompt: "在吗"})
	if err != nil {
		t.Fatalf("压不动不算这一轮失败，Run() error = %v", err)
	}
	if output == nil {
		t.Fatal("Run() 返回了 nil 输出")
	}

	compactionSpan := spanByName(t, recorder.Ended(), observability.SpanNameCompaction)
	if compactionSpan.Status().Code != codes.Error {
		t.Error("压缩失败时 Span 必须红着——它存在的全部意义就是让人看见压缩没生效")
	}
	if got, want := compactionSpan.Status().Description, "50002"; got != want {
		t.Errorf("状态描述 = %q, want %q（ErrCompactionFailed）", got, want)
	}
}

// TestRejectedToolCallHasNoSpan 是决策 16：未注册的工具在 Scheduler 的查表处就
// 被拒了，不经过中间件链，所以没有 execute_tool Span。它们照常作为一条 IsError
// 的工具消息回给模型。
func TestRejectedToolCallHasNoSpan(t *testing.T) {
	recorder := installRecordingProvider(t)
	workDir := t.TempDir()
	writeAgentsFile(t, workDir)
	agent := newTestAgent(t, workDir, &fakeProvider{
		responses: []*schema.AssistantMessage{
			toolCallResponse("call-1", "not_registered", `{}`),
			textResponse("好，换条路"),
		},
	}, session.InMemory())

	output, err := agent.Run(context.Background(), &RunInput{Prompt: "在吗"})
	if err != nil {
		t.Fatalf("被拒绝的工具调用不该中断运行，Run() error = %v", err)
	}
	if output == nil {
		t.Fatal("Run() 返回了 nil 输出")
	}

	for _, span := range recorder.Ended() {
		if strings.HasPrefix(span.Name(), "execute_tool") {
			t.Fatalf("被拒绝的工具调用不该有 Span，却看到了 %q", span.Name())
		}
	}
	// 它仍然在 trace 上的证据是那一轮的 tools_requested：模型确实要过工具，
	// 只是没执行成功。
	turnSpan := turnSpanAt(t, recorder.Ended(), 0)
	if got := attributeMap(turnSpan.Attributes())[observability.AttrToolsRequested].AsInt64(); got != 1 {
		t.Errorf("%s = %d, want 1", observability.AttrToolsRequested, got)
	}
}
