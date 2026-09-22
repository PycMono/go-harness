package main

import (
	"bufio"
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
)

// sessiontest 验证 pi/session 的持久化与重建：按会话 id 取会话 → 第一轮 Run →
// 再用同一个 id 取一次（等价于换进程）跑第二轮 Run，证明"客户说了什么"跨 Run 可回放。
//
// -offline 时不调模型：直接往会话里追加消息再重建，用来离线检查存储格式与
// 重建结果。默认（不带 -offline）需要 config.json 里的模型平台配置。
//
// 会话文件默认落在 <当前目录>/testdata/sessions 下（-sessions 可改），跑完不删：
// 文件名就是会话 id（chat-001.jsonl），同一个 id 重复运行就续写同一个文件；-key
// 留空则用 session.NewSessionID() 现生成一个，想接着聊就带上印出来的那个 id。

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

func main() {
	configPath := flag.String("config", "config.json", "模型平台配置文件路径")
	workDir := flag.String("workdir", "cmd/skilltest/testdata", "会话归属的工作区（须含 AGENTS.md）")
	sessionsRoot := flag.String("sessions", "testdata/sessions", "会话根目录，相对当前目录；同一个会话 id 重复运行会续写同一个文件")
	firstPrompt := flag.String("prompt", "记住：我的工单号是 8899。", "第一轮交给 agent 的任务")
	secondPrompt := flag.String("prompt2", "我的工单号是多少？", "第二轮交给 agent 的任务")
	offline := flag.Bool("offline", false, "不调用模型，只验证会话写入与上下文重建")
	timeout := flag.Duration("timeout", 5*time.Minute, "整轮运行的超时时间")
	sessionKey := flag.String("key", "chat-001", "会话 id，直接当文件名；留空则新生成一个（chat-…）")
	interactive := flag.Bool("interactive", false, "一轮一轮地聊：每行一轮，每轮都先按键取会话")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	root, err := filepath.Abs(*workDir)
	if err != nil {
		fail(fmt.Errorf("解析工作目录失败: %w", err))
	}

	sessions, err := filepath.Abs(*sessionsRoot)
	if err != nil {
		fail(fmt.Errorf("解析会话根目录失败: %w", err))
	}

	// 会话 id 决定文件（<id>.jsonl）：给定了就续那个会话，留空就现生成一个。
	key := *sessionKey
	if key == "" {
		key = session.NewSessionID()
		fmt.Printf("=== 新会话 id ===\n%s\n（想接着聊就带上：-key %s）\n\n", key, key)
	}

	// 第一阶段：按会话键取会话。键第一次用就是新建，第二次用就续上——
	// 调用方不需要判断"该不该建"。
	manager, err := session.OpenOrCreate(sessions, root, key)
	if err != nil {
		fail(fmt.Errorf("取会话失败: %w", err))
	}
	fmt.Printf("=== 会话根目录 ===\n%s\n\n", sessions)
	fmt.Printf("=== 会话文件 ===\n%s\n\n", manager.Path())

	if *offline {
		if *interactive {
			interactiveAppend(sessions, root, key)
			return
		}
		offlineReplay(sessions, root, key, manager, *firstPrompt)
		return
	}

	cfg, err := loadConfig(*configPath)
	if err != nil {
		fail(err)
	}
	opts := currentPlatform(cfg)
	if opts == nil || opts.APIKey == "" {
		fail(fmt.Errorf("当前平台未配置 apiKey（离线验证请加 -offline）"))
	}

	if *interactive {
		interactiveChat(ctx, opts, root, sessions, key)
		return
	}

	// 第二阶段：第一轮 Run，输入与回复都逐条落盘。
	fmt.Printf("=== %s (%s / %s) ===\n", opts.ID, opts.Protocol, opts.Model)
	runTurn(ctx, opts, root, manager, *firstPrompt)
	printEntries(manager)

	// 第三阶段：拿同一个键再取一次——等价于换一个进程接着聊，历史从盘上重建。
	reopened, err := session.OpenOrCreate(sessions, root, key)
	if err != nil {
		fail(fmt.Errorf("重新打开会话失败: %w", err))
	}
	fmt.Printf("\n=== 重新打开后重建的上下文 ===\n")
	printMessages(reopened.BuildMessages())

	// 第四阶段：第二轮 Run，调用方只给本轮输入。
	runTurn(ctx, opts, root, reopened, *secondPrompt)
	printEntries(reopened)

	fmt.Printf("\n=== 两轮之后的完整重建 ===\n")
	printMessages(reopened.BuildMessages())
}

// runTurn 用同一个会话跑一轮：history 只从会话重建，调用方只给本轮输入。
func runTurn(ctx context.Context, opts *providers.Options, root string, manager *session.Manager, prompt string) {
	agent, err := pi.NewAgent(&pi.Options{
		WorkDir:         root,
		ProviderOptions: opts,
		Session:         manager,
		TextObserver:    func(delta string) { fmt.Print(delta) },
	})
	if err != nil {
		fail(err)
	}

	fmt.Printf("\n--- 输入: %s ---\n", prompt)
	output, err := agent.Run(ctx, &pi.RunInput{
		Input: &pi.Message{ContentType: "text", SenderType: "customer", Content: prompt},
	})
	if err != nil {
		if output != nil {
			printMessages(output.Messages())
		}
		fail(err)
	}
	fmt.Printf("\n--- 答复: %s ---\n", output.Text())
}

// offlineReplay 不调模型：直接往会话里追加一轮对话，再重建，验证存储与折叠。
func offlineReplay(sessions, root, sessionKey string, manager *session.Manager, prompt string) {
	appendPair(manager, prompt, "记下了")

	fmt.Printf("=== 离线模式：直接追加 2 条消息后的重建 ===\n")
	printEntries(manager)
	printMessages(manager.BuildMessages())

	// 再取一次（同一个键），证明磁盘上的形态能还原出同样的上下文。
	reopened, err := session.OpenOrCreate(sessions, root, sessionKey)
	if err != nil {
		fail(fmt.Errorf("重新打开会话失败: %w", err))
	}
	fmt.Printf("\n=== 重新打开后重建的上下文 ===\n")
	printMessages(reopened.BuildMessages())
}

// interactiveChat 从标准输入一轮一轮地聊：每行是一轮输入，:q 或 Ctrl-D 结束。
// 每一轮都先按键重新取一次会话，再交给一个新建的 agent 跑——等价于"每轮换一个
// 进程"，所以屏幕上看到的答复只能来自会话文件里的历史，不是上一轮的内存残留。
func interactiveChat(ctx context.Context, opts *providers.Options, root, sessions, sessionKey string) {
	scanner := bufio.NewScanner(os.Stdin)
	fmt.Printf("=== 一轮一轮聊（每行一轮，:q 结束）===\n")

	for round := 1; ; round++ {
		fmt.Print("> ")
		if !scanner.Scan() {
			return
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if line == ":q" {
			return
		}

		manager, err := session.OpenOrCreate(sessions, root, sessionKey)
		if err != nil {
			fail(fmt.Errorf("取会话失败: %w", err))
		}

		fmt.Printf("\n--- 第 %d 轮 ---\n", round)
		runTurn(ctx, opts, root, manager, line)
		reportRound(round, manager)
	}
}

// interactiveAppend 是 -offline 下的一轮一轮：不调模型，把每行输入当用户消息追加，
// 再补一条固定答复。用来在没有模型的情况下看会话文件一轮轮变长。
func interactiveAppend(sessions, root, sessionKey string) {
	scanner := bufio.NewScanner(os.Stdin)
	fmt.Printf("=== 一轮一轮聊（离线，每行一轮，:q 结束）===\n")

	for round := 1; ; round++ {
		fmt.Print("> ")
		if !scanner.Scan() {
			return
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if line == ":q" {
			return
		}

		manager, err := session.OpenOrCreate(sessions, root, sessionKey)
		if err != nil {
			fail(fmt.Errorf("取会话失败: %w", err))
		}
		appendPair(manager, line, "记下了")
		reportRound(round, manager)
	}
}

// appendPair 追加一轮"用户说了什么 + 助手回什么"，离线模式下代替真实 Run。
func appendPair(manager *session.Manager, prompt, reply string) {
	userMessage, err := schema.NewUserMessage(schema.ContentBlocks{schema.TextBlock(prompt)})
	if err != nil {
		fail(fmt.Errorf("构造用户消息失败: %w", err))
	}
	assistantMessage, err := schema.NewAssistantMessage(
		schema.ContentBlocks{schema.TextBlock(reply)}, nil, "", nil)
	if err != nil {
		fail(fmt.Errorf("构造助手消息失败: %w", err))
	}
	turns := schema.Messages{userMessage, assistantMessage}
	for _, message := range turns {
		if err := manager.Append(session.Entry{Type: session.EntryMessage, Message: message}); err != nil {
			fail(fmt.Errorf("追加消息失败: %w", err))
		}
	}
}

// reportRound 报这一轮之后文件里的 entry 条数与字节数：会话是一轮轮往同一个文件里
// 加长的，这两个数字就是"数据到底写进去没有"的直接证据。
func reportRound(round int, manager *session.Manager) {
	info, err := os.Stat(manager.Path())
	if err != nil {
		fail(fmt.Errorf("查看会话文件失败: %w", err))
	}
	fmt.Printf("--- 第 %d 轮结束：entry %d 条，文件 %d 字节 %s ---\n\n",
		round, len(manager.Entries()), info.Size(), manager.Path())
}

// messageMissing 是消息载荷缺失时的标记文本。接口为 nil 就是没有载荷（header entry
// 就是这样），展示路径给一条带标记的行，而不是空串或 panic。
const messageMissing = "!消息载荷缺失"

// printEntries 打印会话文件里的 entry 链条。
func printEntries(manager *session.Manager) {
	entries := manager.Entries()
	fmt.Printf("\n=== 会话 entry（%d 条，叶子 %s）===\n", len(entries), manager.LeafID())
	for index, entry := range entries {
		fmt.Printf("%2d %s", index, renderEntry(entry))
	}
}

// renderEntry 把一条 entry 渲染成一行（不含行首的序号列）。header 行只有元信息，
// 它的 Message 是 nil 接口，这条路径不碰载荷；message 行委派给消息侧的投影。
func renderEntry(entry session.Entry) string {
	switch entry.Type {
	case session.EntryHeader:
		return fmt.Sprintf("[%s] header %s workdir=%s\n", entry.ID, entry.Header.ID, entry.Header.WorkDir)
	case session.EntryMessage:
		return fmt.Sprintf("[%s] ← %s message %s: %s\n",
			entry.ID, entry.ParentID, messageRole(entry.Message), messageText(entry.Message))
	default:
		return fmt.Sprintf("[%s] ← %s %s\n", entry.ID, entry.ParentID, entry.Type)
	}
}

// printMessages 按顺序打印重建出来的上下文。
func printMessages(messages schema.Messages) {
	fmt.Println()
	if len(messages) == 0 {
		fmt.Println("（空）")
		return
	}
	for index, message := range messages {
		fmt.Printf("%2d %s", index, renderMessage(message))
	}
}

// renderMessage 把重建出来的一条消息渲染成一行（不含行首的序号列）。纯函数、不碰
// 标准输出，打印与断言共用同一份渲染。
func renderMessage(message schema.Message) string {
	return fmt.Sprintf("[%s] %s\n", messageRole(message), messageText(message))
}

// messageRole 返回消息角色的展示词；载荷缺失（header entry 的 nil 接口）时用 "?" 占位，
// 不去解引用。
func messageRole(message schema.Message) schema.Role {
	if message == nil {
		return "?"
	}
	return message.Role()
}

// messageText 返回消息的可展示正文：图片块换成脱敏占位（[图片: scheme://host/path]），
// 再取首行并限长。entry 行与重建上下文行共用这份投影，两处只差前缀。
func messageText(message schema.Message) string {
	if message == nil {
		return messageMissing
	}
	return firstLine(displayText(schema.ContentOf(message)))
}

// displayText 把内容块投影成可展示文本：图片块换成脱敏占位，消息不再因为带图而打印成
// 空。脏块（图片块缺 Image）连占位都投影不出来，严格的 Text() 会报错，这时给一条带标记
// 的降级文本——空串会让人分不清"这条消息没文本"和"渲染失败了"。
func displayText(content schema.ContentBlocks) string {
	text, err := content.WithImagePlaceholders().Text()
	if err != nil {
		return fmt.Sprintf("[内容渲染失败: %v]", err)
	}
	return text
}

// firstLine 只取首行并限长，上下文里的系统提示词与摘要可能很长。
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
	fmt.Fprintf(os.Stderr, "失败: %v\n", err)
	os.Exit(1)
}
