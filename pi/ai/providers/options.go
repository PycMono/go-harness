package providers

import (
	"fmt"
	"strings"

	"github.com/PycMono/go-harness/pi/ai"
	pierrors "github.com/PycMono/go-harness/pi/error"
)

// Protocol identifies the wire protocol used by a model platform.
type Protocol string

const (
	ProtocolOpenAI    Protocol = "openai"
	ProtocolAnthropic Protocol = "anthropic"
)

type Options struct {
	ID       string   `json:"id"`
	Protocol Protocol `json:"protocol"`
	BaseURL  string   `json:"baseURL"`
	APIKey   string   `json:"apiKey"`
	Model    string   `json:"model"`
	// SupportsImageInput 表示该模型接受图片输入。OpenAI 协议的 tool 消息只有文本
	// 位置，工具结果的图片进不去，只有本开关为真时才在工具结果之后补一条合成 user
	// 消息携带图片；缺省为假，图片按占位文本降级，不会静默丢弃（见同包 openai.go
	// 的 insertToolResultImages）。Anthropic 不读这个开关：它的 tool_result 内容块
	// 本身就接受图片，走的是 schema 的 anthropicToolResultContent。
	SupportsImageInput bool `json:"supportsImageInput,omitempty"`
}

// Validate 校验
func (opts *Options) Validate() error {
	opts.ID = strings.TrimSpace(opts.ID)
	opts.BaseURL = strings.TrimSpace(opts.BaseURL)
	opts.APIKey = strings.TrimSpace(opts.APIKey)
	opts.Model = strings.TrimSpace(opts.Model)

	if len(opts.APIKey) == 0 {
		return pierrors.ErrAIAPIKeyRequired
	}
	if len(opts.Model) == 0 {
		return pierrors.ErrAIModelRequired
	}
	if len(opts.BaseURL) == 0 {
		return pierrors.ErrAIBaseURLRequired
	}

	return nil
}

// New validates opts and constructs its protocol-specific provider.
func New(opts *Options) (ai.Provider, error) {
	if err := opts.Validate(); err != nil {
		return nil, err
	}

	switch opts.Protocol {
	case ProtocolOpenAI:
		return NewOpenAI(opts), nil
	case ProtocolAnthropic:
		return NewAnthropic(opts), nil
	default:
		return nil, fmt.Errorf("不支持的 Provider protocol %q，可选值: openai, anthropic", opts.Protocol)
	}
}
