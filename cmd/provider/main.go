package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/PycMono/go-harness/pi/ai"
	"github.com/PycMono/go-harness/pi/ai/providers"
	"github.com/PycMono/go-harness/pi/schema"
)

// harnessConfig 对应仓库根目录的 config.json，平台条目直接映射为 Options。
type harnessConfig struct {
	CurrentPlatform string               `json:"currentPlatform"`
	Platforms       []*providers.Options `json:"platforms"`
}

// currentPlatform 返回 config.json 中 currentPlatform 指向的平台。
func currentPlatform(cfg *harnessConfig) *providers.Options {
	for _, opts := range cfg.Platforms {
		if opts.ID == cfg.CurrentPlatform {
			return opts
		}
	}
	if len(cfg.Platforms) > 0 {
		return cfg.Platforms[0]
	}
	return nil
}

func buildProvider(opts *providers.Options) ai.Provider {
	switch opts.Protocol {
	case providers.ProtocolOpenAI:
		return providers.NewOpenAI(opts)
	case providers.ProtocolAnthropic:
		return providers.NewAnthropic(opts)
	default:
		return nil
	}
}

// consumeStream 消费整个流并打印事件，返回 Result。
func consumeStream(stream ai.Stream) (*schema.Message, error) {
	defer stream.Close()

	for stream.Next() {
		event := stream.Current()
		switch event.Type {
		case schema.StreamEventStart:
			fmt.Println("[event] start")
		case schema.StreamEventTextDelta:
			fmt.Printf("[event] text_delta: %q\n", event.TextDelta)
		case schema.StreamEventDone:
			fmt.Println("[event] done")
		case schema.StreamEventError:
			fmt.Println("[event] error")
		}
	}
	return stream.Result()
}

func main() {
	data, err := os.ReadFile("config.json")
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取 config.json 失败: %v\n", err)
		os.Exit(1)
	}
	var cfg harnessConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		fmt.Fprintf(os.Stderr, "解析 config.json 失败: %v\n", err)
		os.Exit(1)
	}

	// 用 config.json 的 currentPlatform 直连真实模型，流式调用。
	opts := currentPlatform(&cfg)
	if opts == nil || opts.APIKey == "" {
		fmt.Fprintln(os.Stderr, "当前平台未配置 apiKey")
		os.Exit(1)
	}
	provider := buildProvider(opts)
	if provider == nil {
		fmt.Fprintf(os.Stderr, "未知协议: %q\n", opts.Protocol)
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	fmt.Printf("=== %s (%s / %s) 流式响应 ===\n", opts.ID, opts.Protocol, opts.Model)
	message, err := consumeStream(provider.Stream(ctx, schema.Messages{
		{Role: schema.RoleSystem, Content: schema.ContentBlocks{schema.TextBlock("你是一个简洁的测试助手。")}},
		{Role: schema.RoleUser, Content: schema.ContentBlocks{schema.TextBlock("用一句话介绍 Go 语言。")}},
	}, nil))
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s 调用失败: %v\n", opts.ID, err)
		os.Exit(1)
	}
	if message != nil {
		text, _ := message.Content.Text()
		fmt.Printf("结果: role=%s finish=%s text=%q\n", message.Role, message.FinishReason, text)
		if message.Usage != nil {
			fmt.Printf("Usage: input=%d output=%d cache_read=%d\n",
				message.Usage.InputTokens, message.Usage.OutputTokens, message.Usage.CacheReadTokens)
		}
	}
}
