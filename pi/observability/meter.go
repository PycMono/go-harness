package observability

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/PycMono/go-harness/pi/ai"
	pierrors "github.com/PycMono/go-harness/pi/error"
	"github.com/PycMono/go-harness/pi/schema"
)

// UsageMeter 给每次成功的模型响应补上计量：延迟、TTFT 与归属（平台、模型）。
// 它不做金额换算——token → 美元要按业务自己的价格表算，本包不接触任何金额，
// 也不读写 Usage 里的价格与成本字段。
//
// 严格模式：响应缺 Usage、任一用量分项为负、或延迟为负，都返回 ErrAIGeneration。
// 宁可明确失败，也不要让"账丢了"静默变成一行 0、"账是脏的"静默变成一行负数
// ——计费与预算都建立在这些数字上。分项查全部五个（输入、输出、缓存读写、
// 推理），子集约束不查，理由见 negativeTokens。
type UsageMeter struct {
	next       ai.Provider
	platformID string
	model      string
	now        func() time.Time
}

// NewUsageMeter 构造计量装饰器。platformID 与 model 都会写进 Usage 的归属
// 字段，两者都不能为空——它们的用途正是"按平台、按模型对账"。
func NewUsageMeter(next ai.Provider, platformID, model string) (*UsageMeter, error) {
	platformID = strings.TrimSpace(platformID)
	model = strings.TrimSpace(model)
	switch {
	case platformID == "":
		return nil, errors.New("usage meter: platform id is required")
	case model == "":
		return nil, errors.New("usage meter: model is required")
	}

	return &UsageMeter{next: next, platformID: platformID, model: model, now: time.Now}, nil
}

// Stream 包一层流。
//
// 计时口径：startedAt 取在**调用下层 Stream 之前**。下层的 Stream 不是纯粹建对象
// ——两个 provider 都在里面就把请求发出去了（`pi/ai/providers/anthropic.go` 的
// `p.client.Messages.NewStreaming(ctx, params)` 就在 Stream 里），先调下层再取
// 时刻会把建连与首包前的时间整段漏掉。反过来写成先取时刻，量到的是"从进入装饰器
// 到 Result 返回"，包含请求构造、网络、以及调用方读完整个流的时间，也就是这一轮
// 模型调用的全部墙钟；TTFT 是其中的一段（到首个非空文本增量为止），所以恒有
// TTFT ≤ Latency。
func (m *UsageMeter) Stream(
	ctx context.Context,
	messages schema.Messages,
	tools schema.ToolDefinitions,
) ai.Stream {
	startedAt := m.now()

	return &meterStream{
		next:      m.next.Stream(ctx, messages, tools),
		meter:     m,
		startedAt: startedAt,
	}
}

type meterStream struct {
	next      ai.Stream
	meter     *UsageMeter
	startedAt time.Time

	current schema.StreamEvent
	// ttft 是首个非空文本增量的延迟快照；hasTTFT 为 false 表示这一轮没有
	// 文本增量（纯工具调用响应），TTFTMS 保持 nil。
	ttft    time.Duration
	hasTTFT bool
	// resolved 保证 measure 只跑一次：Result 可能被调用多次（下游的装饰器
	// 与循环都可能读它）。
	resolved bool
	response *schema.AssistantMessage
	err      error
}

func (s *meterStream) Next() bool {
	if !s.next.Next() {
		return false
	}
	s.current = s.next.Current()
	if !s.hasTTFT && s.current.Type == schema.StreamEventTextDelta && s.current.TextDelta != "" {
		s.ttft = s.meter.now().Sub(s.startedAt)
		s.hasTTFT = true
	}

	return true
}

func (s *meterStream) Current() schema.StreamEvent { return s.current }

func (s *meterStream) Result() (*schema.AssistantMessage, error) {
	if s.resolved {
		return s.response, s.err
	}
	s.resolved = true
	response, err := s.next.Result()
	s.response, s.err = s.meter.measure(s.startedAt, response, err)
	// TTFT 与延迟是两笔账：延迟在 measure 里补（它只看起止时刻），TTFT 要
	// 等流读到第一个文本增量才知道，所以在这里补。不足 1ms 记 0，不是 nil
	// ——nil 的含义是"没有文本增量"。
	if s.err == nil && s.hasTTFT {
		ttftMS := s.ttft.Milliseconds()
		s.response.Usage.TTFTMS = &ttftMS
	}

	return s.response, s.err
}

func (s *meterStream) Close() error { return s.next.Close() }

// measure 校验并补上延迟与归属。失败的请求原样返回——它的账不可信，不该
// 被当成一次计量。
func (m *UsageMeter) measure(
	startedAt time.Time,
	response *schema.AssistantMessage,
	err error,
) (*schema.AssistantMessage, error) {
	latencyMS := m.now().Sub(startedAt).Milliseconds()
	if err != nil {
		return response, err
	}
	if response == nil || response.Usage == nil {
		return nil, pierrors.ErrAIGeneration.Wrap(fmt.Errorf(
			"provider %s/%s 的响应缺少用量", m.platformID, m.model))
	}
	if negativeTokens(response.Usage) || latencyMS < 0 {
		return nil, pierrors.ErrAIGeneration.Wrap(fmt.Errorf(
			"provider %s/%s 的用量为负", m.platformID, m.model))
	}

	// 复制一份再改：下层 Provider 可能把 Usage 指针复用在下一条消息上，
	// 就地改会连带改到别人手里的那一份。
	measured := *response
	usage := *response.Usage
	usage.LatencyMS = latencyMS
	usage.PlatformID = m.platformID
	usage.Model = m.model
	measured.Usage = &usage

	return &measured, nil
}

// negativeTokens 检查全部用量分项非负。只查输入输出是不够的：缓存读写与推理是
// 各自父项的**子集**（`pi/schema/usage.go:53-58`），负的子集同样是脏数据，放过去
// 会当成可信账写进 Span。
//
// 子集约束本身（CacheRead+CacheWrite ≤ InputTokens、Reasoning ≤ OutputTokens）不
// 在这里强制。两个内置 provider 都满足它，但那是**映射约定**，不是协议保证：第三方
// provider 或代理把缓存报在输入之外时，强制会把一个本来跑得通的请求打掉，代价比
// 记一个可疑数字大。约定写在 pi/schema 的字段注释里，不在这里收税。
func negativeTokens(usage *schema.Usage) bool {
	return usage.InputTokens < 0 ||
		usage.OutputTokens < 0 ||
		usage.CacheReadTokens < 0 ||
		usage.CacheWriteTokens < 0 ||
		usage.ReasoningTokens < 0
}
