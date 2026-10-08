package observability

import (
	"context"
	"testing"
	"time"

	"github.com/PycMono/go-harness/pi/ai"
	pierrors "github.com/PycMono/go-harness/pi/error"
	"github.com/PycMono/go-harness/pi/schema"
)

// 本文件钉住计量层：补哪几笔账、什么算脏数据、哪些形态有意放行。
// 计时用假钟（每次调用 +10ms），时钟注入点是 UsageMeter.now 这个未导出字段，
// 所以测试必须留在包内。

const (
	meterPlatformID = "platform-x"
	meterModel      = "model-y"
)

// fakeProvider 每次 Stream 都返回同一个流。
type fakeProvider struct {
	stream ai.Stream
	calls  int
}

func (p *fakeProvider) Stream(
	context.Context, schema.Messages, schema.ToolDefinitions,
) ai.Stream {
	p.calls++

	return p.stream
}

// fakeStream 是可控的流：events 依次吐出，response/err 是 Result 的产物。
type fakeStream struct {
	events   []schema.StreamEvent
	response *schema.AssistantMessage
	err      error
	index    int
	closed   bool
}

func (s *fakeStream) Next() bool {
	if s.index >= len(s.events) {
		return false
	}
	s.index++

	return true
}

func (s *fakeStream) Current() schema.StreamEvent { return s.events[s.index-1] }

func (s *fakeStream) Result() (*schema.AssistantMessage, error) { return s.response, s.err }

func (s *fakeStream) Close() error {
	s.closed = true

	return nil
}

// meteredStream 把假流套上 UsageMeter，时钟换成每次调用 +10ms 的假钟。
// 假钟返回 10ms × 调用次数，所以：
//
//	Stream 里取起点     → 第 1 次 → 10ms
//	Next 里取 TTFT      → 第 2 次 → 20ms（TTFT = 10ms）
//	Result 里取终点     → 第 2 或 3 次 → 延迟 = 10ms 或 20ms
func meteredStream(t *testing.T, stream ai.Stream) ai.Stream {
	t.Helper()
	meter, err := NewUsageMeter(&fakeProvider{stream: stream}, meterPlatformID, meterModel)
	if err != nil {
		t.Fatalf("NewUsageMeter() error = %v", err)
	}
	var calls int
	meter.now = func() time.Time {
		calls++

		return time.Unix(0, 0).Add(time.Duration(calls) * 10 * time.Millisecond)
	}

	return meter.Stream(context.Background(), nil, nil)
}

// consume 按调用方的正常读法把流读到结束。
func consume(stream ai.Stream) {
	for stream.Next() {
		_ = stream.Current()
	}
}

func TestNewUsageMeterRequiresOwnership(t *testing.T) {
	cases := []struct {
		name       string
		platformID string
		model      string
	}{
		{"平台为空", "", meterModel},
		{"模型为空", meterPlatformID, ""},
		{"平台只有空白", "  ", meterModel},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := NewUsageMeter(&fakeProvider{}, testCase.platformID, testCase.model); err == nil {
				t.Fatal("归属信息不全时应当报错")
			}
		})
	}
}

func TestMeterFillsLatencyAndOwnership(t *testing.T) {
	usage := &schema.Usage{InputTokens: 100, OutputTokens: 20}
	response := &schema.AssistantMessage{Usage: usage}

	got, err := meteredStream(t, &fakeStream{response: response}).Result()
	if err != nil {
		t.Fatalf("Result() error = %v", err)
	}
	if got.Usage.LatencyMS != 10 {
		t.Errorf("LatencyMS = %d, want 10", got.Usage.LatencyMS)
	}
	if got.Usage.PlatformID != meterPlatformID {
		t.Errorf("PlatformID = %q, want %q", got.Usage.PlatformID, meterPlatformID)
	}
	if got.Usage.Model != meterModel {
		t.Errorf("Model = %q, want %q", got.Usage.Model, meterModel)
	}
	// 复制一份再改：就地改会连带改到别人手里的那一份。
	if usage.LatencyMS != 0 || usage.PlatformID != "" {
		t.Errorf("原消息被就地改了：LatencyMS = %d, PlatformID = %q",
			usage.LatencyMS, usage.PlatformID)
	}
	if got.Usage == usage {
		t.Error("Usage 没有复制一份")
	}
}

func TestMeterTTFTOnlyWhenTextFlows(t *testing.T) {
	t.Run("有文本增量则记 TTFT", func(t *testing.T) {
		stream := meteredStream(t, &fakeStream{
			events:   []schema.StreamEvent{{Type: schema.StreamEventTextDelta, TextDelta: "你"}},
			response: &schema.AssistantMessage{Usage: &schema.Usage{}},
		})
		consume(stream)
		got, err := stream.Result()
		if err != nil {
			t.Fatalf("Result() error = %v", err)
		}
		if got.Usage.TTFTMS == nil {
			t.Fatal("TTFTMS = nil, want 有值")
		}
		if *got.Usage.TTFTMS != 10 {
			t.Errorf("TTFTMS = %d, want 10", *got.Usage.TTFTMS)
		}
		if *got.Usage.TTFTMS > got.Usage.LatencyMS {
			t.Errorf("TTFT(%d) 不该大于延迟(%d)", *got.Usage.TTFTMS, got.Usage.LatencyMS)
		}
	})

	t.Run("纯工具调用响应不记 TTFT", func(t *testing.T) {
		stream := meteredStream(t, &fakeStream{
			response: &schema.AssistantMessage{Usage: &schema.Usage{}},
		})
		consume(stream)
		got, err := stream.Result()
		if err != nil {
			t.Fatalf("Result() error = %v", err)
		}
		if got.Usage.TTFTMS != nil {
			t.Errorf("TTFTMS = %d, want nil（没有文本增量）", *got.Usage.TTFTMS)
		}
	})
}

func TestMeterRejectsMissingUsage(t *testing.T) {
	got, err := meteredStream(t, &fakeStream{
		response: &schema.AssistantMessage{},
	}).Result()
	if pierrors.CodeOf(err) != pierrors.ErrAIGeneration.Code() {
		t.Fatalf("CodeOf(err) = %d, want 20000", pierrors.CodeOf(err))
	}
	if got != nil {
		t.Errorf("失败时应当返回 nil 响应，得到 %v", got)
	}
}

// TestMeterRejectsNegativeSubfields 是决策 18 的回归测试：只查输入输出的写法
// 会在这五条里漏掉三条（缓存读、缓存写、推理）。
func TestMeterRejectsNegativeSubfields(t *testing.T) {
	cases := map[string]schema.Usage{
		"input":       {InputTokens: -1},
		"output":      {OutputTokens: -1},
		"cache_read":  {CacheReadTokens: -1},
		"cache_write": {CacheWriteTokens: -1},
		"reasoning":   {ReasoningTokens: -1},
	}
	for name := range cases {
		t.Run(name, func(t *testing.T) {
			usage := cases[name]
			_, err := meteredStream(t, &fakeStream{
				response: &schema.AssistantMessage{Usage: &usage},
			}).Result()
			if pierrors.CodeOf(err) != pierrors.ErrAIGeneration.Code() {
				t.Fatalf("CodeOf(err) = %d, want 20000", pierrors.CodeOf(err))
			}
		})
	}
}

// TestMeterAllowsSubsetViolation 是上一条的反面：子集约束有意不查，改了校验的
// 人会挂在这里。两个内置 provider 都满足它，但那是映射约定不是协议保证——
// 强制会把一个能跑通的请求打掉，代价比记一个可疑数字大。
func TestMeterAllowsSubsetViolation(t *testing.T) {
	usage := &schema.Usage{InputTokens: 400, CacheReadTokens: 500}
	got, err := meteredStream(t, &fakeStream{
		response: &schema.AssistantMessage{Usage: usage},
	}).Result()
	if err != nil {
		t.Fatalf("子集违例被拒了（决策 18 说好不查的）：%v", err)
	}
	if got.Usage.CacheReadTokens != 500 {
		t.Errorf("CacheReadTokens = %d, want 500", got.Usage.CacheReadTokens)
	}
}

func TestMeterMeasuresOnce(t *testing.T) {
	stream := meteredStream(t, &fakeStream{
		response: &schema.AssistantMessage{Usage: &schema.Usage{}},
	})
	first, firstErr := stream.Result()
	if firstErr != nil {
		t.Fatalf("第一次 Result() error = %v", firstErr)
	}
	second, secondErr := stream.Result()
	if secondErr != nil {
		t.Fatalf("第二次 Result() error = %v", secondErr)
	}
	if first != second {
		t.Fatal("两次 Result 返回了不同对象")
	}
	if second.Usage.LatencyMS != 10 {
		t.Errorf("第二次 Result 重新计量了：LatencyMS = %d, want 10", second.Usage.LatencyMS)
	}
}
