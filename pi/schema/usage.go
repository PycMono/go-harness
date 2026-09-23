package schema

// 本文件承载一次模型响应的用量与流事件表示：Usage 是标准化后的令牌用量、价格、
// 成本与延迟，价格与成本由业务层填充；StreamEvent 是与具体模型 SDK 无关的响应
// 事件。两者都不依赖任何模型 SDK，也不参与协议转换。

// MaxUsageDecimalExclusive 是以 DECIMAL(20,12) 存储价格和单次调用成本时的上限，不包含该值本身。
const MaxUsageDecimalExclusive = 100_000_000

// CostQuality 是成本可信度枚举（设计 §9.1）。
type CostQuality string

const (
	// CostQualityExact 表示 Provider 分项足以按配置价格重算成本。
	CostQualityExact CostQuality = "exact"
	// CostQualityEstimated 表示成本只能估算。
	CostQualityEstimated CostQuality = "estimated"
)

type StreamEventType string

const (
	StreamEventStart     StreamEventType = "start"
	StreamEventTextDelta StreamEventType = "text_delta"
	StreamEventDone      StreamEventType = "done"
	StreamEventError     StreamEventType = "error"
)

// Usage 保存一次模型响应的标准化令牌用量、价格、成本和延迟数据。
type Usage struct {
	// InputTokens 是输入令牌数。
	InputTokens int64 `json:"input_tokens"`
	// OutputTokens 是输出令牌数。
	OutputTokens int64 `json:"output_tokens"`
	// InputPriceUSDPerMillionTokens 是每百万输入令牌的美元价格。
	InputPriceUSDPerMillionTokens float64 `json:"input_price_usd_per_million_tokens"`
	// OutputPriceUSDPerMillionTokens 是每百万输出令牌的美元价格。
	OutputPriceUSDPerMillionTokens float64 `json:"output_price_usd_per_million_tokens"`
	// CostUSD 是本次响应的美元成本。
	CostUSD float64 `json:"cost_usd"`
	// LatencyMS 是本次响应的延迟，单位为毫秒。
	LatencyMS int64 `json:"latency_ms"`
	// PlatformID 是提供模型服务的平台标识。
	PlatformID string `json:"platform_id"`
	// Model 是生成响应的模型名称。
	Model string `json:"model"`
	// TTFTMS 是首个非空 Text Delta 的延迟毫秒数；nil 表示未观测到
	// Text Delta（如纯 Tool Call 响应），0 表示已观测但不足 1ms（设计 §9.1）。
	TTFTMS *int64 `json:"ttft_ms,omitempty"`
	// CostQuality 表示成本可信度（§9.1）：exact 表示分项足以按配置价格
	// 重算；estimated 不能混入精确成本报表。缺省空值按 estimated 处理。
	CostQuality CostQuality `json:"cost_quality,omitempty"`
	// CacheReadTokens 是缓存读取令牌数（输入的子集，已含在 InputTokens 里）。
	CacheReadTokens int64 `json:"cache_read_tokens,omitempty"`
	// CacheWriteTokens 是缓存写入令牌数（同样是输入的子集）。
	CacheWriteTokens int64 `json:"cache_write_tokens,omitempty"`
	// ReasoningTokens 是推理令牌数（输出的子集，已含在 OutputTokens 里）。
	ReasoningTokens int64 `json:"reasoning_tokens,omitempty"`
	// CacheReadPriceUSDPerMillionTokens 是每百万缓存读取令牌的美元价格。
	CacheReadPriceUSDPerMillionTokens float64 `json:"cache_read_price_usd_per_million_tokens,omitempty"`
	// CacheWritePriceUSDPerMillionTokens 是每百万缓存写入令牌的美元价格。
	CacheWritePriceUSDPerMillionTokens float64 `json:"cache_write_price_usd_per_million_tokens,omitempty"`
}

// TotalTokens 是一次调用的总令牌数：输入 + 输出。
//
// 只加这两项，不加缓存读写与推理——它们各自是输入/输出的子集（见字段说明），
// 所以"输入 + 输出"已经是全部，四项相加等于把缓存算三遍，调用方按它估上下文
// 大小会凭空高一截（压缩的触发线就跟着失真）。两个 provider 的映射也照此填：
// anthropic.go 的 InputTokens 已经把 cache 读写加了进去，openai.go 的
// PromptTokens 本身就含 cached_tokens。
//
// 接收者为 nil（响应没有用量）时返回 0；分项为负（脏数据）时同样返回 0，
// 不用负数去参与判断。
func (u *Usage) TotalTokens() int64 {
	if u == nil {
		return 0
	}
	total := u.InputTokens + u.OutputTokens
	if total < 0 {
		return 0
	}

	return total
}

// StreamEvent 是与具体模型 SDK 无关的模型响应事件。
type StreamEvent struct {
	Type      StreamEventType
	TextDelta string
}
