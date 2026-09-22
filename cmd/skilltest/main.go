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
	"github.com/PycMono/go-harness/pi/session"
	"github.com/PycMono/go-harness/pi/tools"
)

// skilltest 用来验证系统提示词组装（AGENTS.md + Agent Skills）和 loop 的串联：
// 先离线调用 pi.SystemPrompt 打印组装后的系统提示词，
// 再（除非 -check-only）像 cmd/harness 一样跑一轮完整的模型循环。

// platformConfig 对应仓库根目录的 config.json，与 cmd/harness 的格式一致。
type platformConfig struct {
	CurrentPlatform string               `json:"currentPlatform"`
	Platforms       []*providers.Options `json:"platforms"`
}

func currentPlatform(cfg *platformConfig) *providers.Options {
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

func loadConfig(path string) (*platformConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取 %s 失败: %w", path, err)
	}
	var cfg platformConfig
	if err = json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %w", path, err)
	}

	return &cfg, nil
}

func observe(event tools.Event) {
	switch event.Phase {
	case tools.EventStart:
		fmt.Printf("\n  → %s\n", event.Call.Name)
	case tools.EventEnd:
		status := "ok"
		if event.IsError {
			status = fmt.Sprintf("failed(code=%d)", event.ErrorCode)
		}
		fmt.Printf("  ← %s %s\n", event.Call.Name, status)
	}
}

func printDelta(delta string) {
	fmt.Print(delta)
}

func main() {
	configPath := flag.String("config", "config.json", "模型平台配置文件路径")
	workDir := flag.String("workdir", "cmd/skilltest/testdata", "资源加载的工作区（须含 AGENTS.md）")
	prompt := flag.String("prompt",
		"查看有哪些可用技能。如果存在与「打个招呼」相关的技能，先用 read 完整读取它的 SKILL.md，再按技能内容执行。",
		"交给 agent 的任务")
	checkOnly := flag.Bool("check-only", false, "只组装并打印系统提示词，不调用模型")
	maxParallel := flag.Int("max-parallel", 4, "同一批工具调用的并发上限")
	maxTurns := flag.Int("max-turns", 0, "单次运行的模型调用次数上限，0 表示用默认值")
	timeout := flag.Duration("timeout", 5*time.Minute, "整轮运行的超时时间")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	root, err := filepath.Abs(*workDir)
	if err != nil {
		fail(fmt.Errorf("解析工作目录失败: %w", err))
	}

	// 第一阶段：直接调用 pi.SystemPrompt，验证 AGENTS.md 与 Skill 发现。
	sysPrompt, err := pi.SystemPrompt(ctx, root)
	if err != nil {
		fail(fmt.Errorf("系统提示词组装失败（工作区需包含 AGENTS.md，技能放在 skills/ 等约定目录下）: %w", err))
	}
	printSystemPrompt(root, sysPrompt)

	if *checkOnly {
		return
	}

	// 第二阶段：跑完整 loop，验证技能目录注入系统提示词后模型能按纪律 read SKILL.md。
	cfg, err := loadConfig(*configPath)
	if err != nil {
		fail(err)
	}
	opts := currentPlatform(cfg)
	if opts == nil || opts.APIKey == "" {
		fail(fmt.Errorf("当前平台未配置 apiKey"))
	}

	agent, err := pi.NewAgent(&pi.Options{
		WorkDir:         root,
		ProviderOptions: opts,
		MaxParallel:     *maxParallel,
		Observer:        observe,
		TextObserver:    printDelta,
		MaxTurns:        *maxTurns,
		Session:         session.InMemory(),
	})
	if err != nil {
		fail(err)
	}

	fmt.Printf("=== %s (%s / %s) ===\n", opts.ID, opts.Protocol, opts.Model)
	fmt.Printf("workdir: %s\n任务: %s\n\n", root, *prompt)

	println("-------------------------")

	output, err := agent.Run(ctx, &pi.RunInput{
		Input: &pi.Message{ContentType: "text", SenderType: "customer", Content: *prompt},
	})
	if err != nil {
		if output != nil {
			printTranscript(output.Messages())
		}
		fail(err)
	}

	printTranscript(output.Messages())
	fmt.Printf("\n=== 最终答复 ===\n%s\n", output.Text())
}

// printSystemPrompt 打印组装后的系统提示词供人工检查。
func printSystemPrompt(root, prompt string) {
	fmt.Printf("=== 系统提示词检查 ===\n")
	fmt.Printf("workdir: %s\n", root)
	fmt.Printf("系统提示词总长度: %d 字符\n", len([]rune(prompt)))

	if prompt == "" {
		fmt.Println("系统提示词为空")
		return
	}

	// 技能目录在系统提示词的 <available_skills> 标签内，逐条列出。
	if index := strings.Index(prompt, "<available_skills>"); index >= 0 {
		end := strings.Index(prompt[index:], "</available_skills>")
		if end >= 0 {
			fmt.Printf("\n--- 技能目录 ---\n%s\n", prompt[index:index+end+len("</available_skills>")])
		}
	} else {
		fmt.Println("未发现任何技能（<available_skills> 缺失）")
	}

	fmt.Printf("\n--- 系统提示词全文 ---\n%s\n", prompt)
}

// printTranscript 按顺序打印消息序列，工具调用与工具结果各压成一行。
func printTranscript(messages schema.Messages) {
	fmt.Printf("\n=== 消息序列 ===\n")
	for _, message := range messages {
		text, _ := message.Content.Text()
		switch message.Role {
		case schema.RoleAssistant:
			fmt.Printf("[assistant] %s\n", text)
			for _, call := range message.ToolCalls {
				fmt.Printf("            └ call %s %s\n", call.Name, string(call.Arguments))
			}
		case schema.RoleTool:
			status := "result"
			if message.IsError {
				status = "error"
			}
			fmt.Printf("[tool:%s] %s: %s\n", message.ToolName, status, firstLine(text))
		default:
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
