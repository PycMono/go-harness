package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
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
func consumeStream(stream ai.Stream) (schema.Message, error) {
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

	// 固定的一问一答，文本块校验不会拦下什么；构造函数按 role 落到对应的具体类型。
	systemMessage, err := schema.NewSystemMessage(
		schema.ContentBlocks{schema.TextBlock("你是一个简洁的测试助手。")})
	if err != nil {
		fmt.Fprintf(os.Stderr, "构造 system 消息失败: %v\n", err)
		os.Exit(1)
	}
	userMessage, err := schema.NewUserMessage(
		schema.ContentBlocks{schema.TextBlock("用一句话介绍 Go 语言。")})
	if err != nil {
		fmt.Fprintf(os.Stderr, "构造 user 消息失败: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("=== %s (%s / %s) 流式响应 ===\n", opts.ID, opts.Protocol, opts.Model)
	message, err := consumeStream(provider.Stream(ctx, schema.Messages{systemMessage, userMessage}, nil))
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s 调用失败: %v\n", opts.ID, err)
		os.Exit(1)
	}
	// Result() 没有产出或流失败时返回的是 nil 接口，这个判断就是"有没有结果"。
	if message != nil {
		fmt.Print(renderResult(message))
	}
}

// renderResult 把流式结果渲染成展示文本。finish_reason 与 usage 只存在于 assistant
// 变体上，别的角色只报角色并说明形态不同——不把零值当成真实结果打印。纯函数、不碰
// 标准输出，打印与断言共用同一份渲染。
func renderResult(message schema.Message) string {
	assistant, ok := message.(*schema.AssistantMessage)
	if !ok {
		return fmt.Sprintf("结果: role=%s 不是 assistant 变体，没有 finish_reason 与 usage\n",
			message.Role())
	}

	var builder strings.Builder
	fmt.Fprintf(&builder, "结果: role=%s finish=%s text=%q\n",
		assistant.Role(), assistant.FinishReason, displayText(assistant.Content))
	if assistant.Usage != nil {
		fmt.Fprintf(&builder, "Usage: input=%d output=%d cache_read=%d\n",
			assistant.Usage.InputTokens, assistant.Usage.OutputTokens, assistant.Usage.CacheReadTokens)
	}
	return builder.String()
}

// displayText 把内容块投影成可展示文本：图片块换成脱敏占位（[图片: scheme://host/path]），
// 结果不再因为带图而打印成空。脏块（图片块缺 Image）连占位都投影不出来，严格的 Text()
// 会报错，这时给一条带标记的降级文本——空串会让人分不清"这条消息没文本"和"渲染失败了"。
func displayText(content schema.ContentBlocks) string {
	text, err := content.WithImagePlaceholders().Text()
	if err != nil {
		return fmt.Sprintf("[内容渲染失败: %v]", err)
	}
	return text
}
