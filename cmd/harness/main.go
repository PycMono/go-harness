package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/PycMono/go-harness/pi"
	"github.com/PycMono/go-harness/pi/ai/providers"
	"github.com/PycMono/go-harness/pi/schema"
	"github.com/PycMono/go-harness/pi/tools"
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

func loadConfig(path string) (*harnessConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取 %s 失败: %w", path, err)
	}
	var cfg harnessConfig
	if err = json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %w", path, err)
	}

	return &cfg, nil
}

// printDelta 把模型边生成边吐出的文本直接打到终端。开头的换行是为了让流式
// 文本结束时能和后面的事件行分开——文本增量本身不带换行。
func printDelta(delta string) {
	fmt.Print(delta)
}

// observe 把工具的生命周期事件打到终端，让人看得见循环里发生了什么。事件会被
// 并发送出，但 fmt 自带互斥，这里的打印够用。
func observe(event tools.Event) {
	switch event.Phase {
	case tools.EventStart:
		fmt.Printf("\n  → %s %s\n", event.Call.Name, compactArgs(event.Call.Arguments))
	case tools.EventEnd:
		status := "ok"
		if event.IsError {
			status = fmt.Sprintf("failed(code=%d)", event.ErrorCode)
		}
		fmt.Printf("  ← %s %s\n", event.Call.Name, status)
	}
}

// compactArgs 把工具参数压成一行，太长就截断，避免刷屏。
func compactArgs(arguments json.RawMessage) string {
	compact := strings.Join(strings.Fields(string(arguments)), " ")
	const maxRunes = 100

	runes := []rune(compact)
	if len(runes) > maxRunes {
		return string(runes[:maxRunes]) + "…"
	}

	return compact
}

func main() {
	configPath := flag.String("config", "config.json", "模型平台配置文件路径")
	workDir := flag.String("workdir", ".", "工具的工作目录")
	prompt := flag.String("prompt",
		"先用 bash 看看当前目录里有什么，然后把 Go 版本号写进 version.txt，最后读回来确认内容。",
		"交给 agent 的任务")
	maxParallel := flag.Int("max-parallel", 4, "同一批工具调用的并发上限")
	maxTurns := flag.Int("max-turns", 0, "单次运行的模型调用次数上限，0 表示用默认值")
	timeout := flag.Duration("timeout", 5*time.Minute, "整轮运行的超时时间")
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		fail(err)
	}
	opts := currentPlatform(cfg)
	if opts == nil || opts.APIKey == "" {
		fail(fmt.Errorf("当前平台未配置 apiKey"))
	}

	// 工作目录定死成绝对路径：工具的路径解析、bash 的 cwd 都以它为根，
	// 相对路径会随进程 cwd 漂移。
	root, err := filepath.Abs(*workDir)
	if err != nil {
		fail(fmt.Errorf("解析工作目录失败: %w", err))
	}

	agent, err := pi.NewAgent(&pi.Options{
		WorkDir:         root,
		ProviderOptions: opts,
		MaxParallel:     *maxParallel,
		Observer:        observe,
		TextObserver:    printDelta,
		MaxTurns:        *maxTurns,
	})
	if err != nil {
		fail(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	fmt.Printf("=== %s (%s / %s) ===\n", opts.ID, opts.Protocol, opts.Model)
	fmt.Printf("workdir: %s\n任务: %s\n\n", root, *prompt)

	output, err := agent.Run(ctx, &pi.RunInput{
		Input: &pi.Message{ContentType: "text", SenderType: "customer", Content: *prompt},
	})
	if err != nil {
		// 出错时消息序列仍是部分有效的，先把已经产生的对话打出来再退出。
		if output != nil {
			printTranscript(output.Messages())
		}
		fail(err)
	}

	printTranscript(output.Messages())
	fmt.Printf("\n=== 最终答复 ===\n%s\n", output.Text())
}

// printTranscript 按顺序打印消息序列，工具调用与工具结果各压成一行。
func printTranscript(messages schema.Messages) {
	// 前面的流式文本没有换行结尾，这里补一个。
	fmt.Printf("\n=== 消息序列 ===\n")
	for _, message := range messages {
		switch message.Role {
		case schema.RoleAssistant:
			text, _ := message.Content.Text()
			fmt.Printf("[assistant] %s\n", text)
			for _, call := range message.ToolCalls {
				fmt.Printf("            └ call %s %s\n", call.Name, compactArgs(call.Arguments))
			}
		case schema.RoleTool:
			text, _ := message.Content.Text()
			status := "result"
			if message.IsError {
				status = "error"
			}
			fmt.Printf("[tool:%s] %s: %s\n", message.ToolName, status, firstLine(text))
		default:
			text, _ := message.Content.Text()
			fmt.Printf("[%s] %s\n", message.Role, firstLine(text))
		}
	}
}

// firstLine 只取首行并限长，消息序列里系统提示词与上下文可能很长。
func firstLine(text string) string {
	text = strings.TrimSpace(text)
	if index := strings.IndexByte(text, '\n'); index >= 0 {
		text = text[:index] + " …"
	}
	const maxRunes = 160

	runes := []rune(text)
	if len(runes) > maxRunes {
		return string(runes[:maxRunes]) + "…"
	}

	return text
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "运行失败: %v\n", err)
	os.Exit(1)
}
