package ai

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
	// 是其子集；Output 是总输出，Reasoning 是其子集。
	// CacheReadTokens 是缓存读取令牌数。
	CacheReadTokens int64 `json:"cache_read_tokens,omitempty"`
	// CacheWriteTokens 是缓存写入令牌数。
	CacheWriteTokens int64 `json:"cache_write_tokens,omitempty"`
	// ReasoningTokens 是推理令牌数。
	ReasoningTokens int64 `json:"reasoning_tokens,omitempty"`
	// CacheReadPriceUSDPerMillionTokens 是每百万缓存读取令牌的美元价格。
	CacheReadPriceUSDPerMillionTokens float64 `json:"cache_read_price_usd_per_million_tokens,omitempty"`
	// CacheWritePriceUSDPerMillionTokens 是每百万缓存写入令牌的美元价格。
	CacheWritePriceUSDPerMillionTokens float64 `json:"cache_write_price_usd_per_million_tokens,omitempty"`
}
