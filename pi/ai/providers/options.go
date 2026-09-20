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
