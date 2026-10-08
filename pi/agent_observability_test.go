package pi

import (
	"context"
	"os"
	"path/filepath"
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
