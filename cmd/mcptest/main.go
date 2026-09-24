// mcptest 是 MCP 接入的驱动，两种跑法。
//
// 走 agent（默认）：任务交给模型，模型自己看工具、自己决定调哪个，工具回什么就答
// 什么。默认那个任务是要它查天气——凭记忆答出来的温度就是"假数据"，正好用来验证
// 它真的调了 exa 的 web_search_exa。
//
//	go run ./cmd/mcptest
//	go run ./cmd/mcptest -prompt "查一下上海明天的天气"
//
// 工作区里必须有 AGENTS.md：agent 的定义（身份、行为边界）从它读，缺了会在开跑前
// 报 10006。默认用驱动自带的 cmd/mcptest/testdata，那份 AGENTS.md 把身份写成"接入
// 了 MCP 工具的 agent"，并写了工具名以 mcp__ 开头。借别的工作区（比如 skilltest 那份
// 定义成"测试 agent，验证资源加载与技能发现流程"的）会让模型去找本地能力，看不见
// 联网工具，拒答——实测过。
//
// 会话落在 -sessions（默认 testdata/sessions）下的 <key>.jsonl：模型与工具说过什么
// 都在里面，带上同一个 -key 再跑就是接着聊，历史从会话重建。
//
// 不碰模型（-prompt 传空串）：把每个 server 的工具列出来，再按示例参数各调一次。
// 这条路不需要平台配置，也不用 Key。
//
//	go run ./cmd/mcptest -prompt ""
//	go run ./cmd/mcptest -prompt "" -call mcp__exa__web_search_exa \
//	    -args '{"query":"MCP Streamable HTTP","objective":"看传输是怎么定义的"}'
//
// 配置怎么读（读哪个文件、哪一段）是驱动自己的事，pi/mcp 只认 []ServerConfig。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/PycMono/go-harness/cmd/internal/mcpconfig"
	"github.com/PycMono/go-harness/pi"
	"github.com/PycMono/go-harness/pi/ai/providers"
	"github.com/PycMono/go-harness/pi/extension"
	"github.com/PycMono/go-harness/pi/mcp"
	"github.com/PycMono/go-harness/pi/schema"
	"github.com/PycMono/go-harness/pi/session"
	"github.com/PycMono/go-harness/pi/tools"
)

// closeTimeout 是停机清理（关闭扩展会话、终止 stdio 子进程）的预算。
const closeTimeout = 10 * time.Second

// maxClipRunes 是一次工具结果打印出来的上限：真 server 的一次搜索能回来几千字，
// 驱动里看个开头就够。
const maxClipRunes = 600

// clientName 是 initialize 报给远端的身份，与 cmd/harness 的缺省名字分开：两个
// 驱动接同一个远端时，那边看得出是谁在连。
const clientName = "go-harness-mcptest"

// defaultPrompt 是默认任务。挑天气是因为它必须用工具才答得准：模型凭记忆答出来的
// 温度就是假数据，正好用来验证它真的调了 MCP 工具。
const defaultPrompt = "查一下北京今天的天气。用可用的工具查真实数据，不要凭记忆回答。"

// config 是配置文件里与本驱动相关的那一段：走 agent 要平台，列工具只要 mcp。
// mcp.servers 解出来是文件形状（mcpconfig.Entry），pi/mcp 认的是另一份结构，两边
// 靠 servers() 转。
type config struct {
	CurrentPlatform string               `json:"currentPlatform"`
	Platforms       []*providers.Options `json:"platforms"`
	MCP             struct {
		Servers []mcpconfig.Entry `json:"servers"`
	} `json:"mcp"`
}

// servers 把配置里读进来的一项项转成 pi/mcp 认的运行期结构。打印、分组、装配都用
// 转出来这一份，停用与否、入口怎么显示就只有一份规则。
func (c *config) servers() []mcp.ServerConfig {
	return mcpconfig.ServerConfigs(c.MCP.Servers)
}

// platformOf 返回 currentPlatform 指向的平台；没对上就用第一个。与 cmd/harness 同一
// 条规则——两边读的是同一份 config.json，挑法不一致会让人以为配置坏了。
func platformOf(cfg *config) *providers.Options {
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

// demoArgs 是几个常见工具的示例参数：没给 -call 时按这张表各调一次，让真数据
// 出来。表里没有的工具不猜参数，只提示怎么自己调。key 是远端工具名，所以换
// tool_prefix 也照样匹配。
//
// 参数是照着远端的 inputSchema 写的，不是猜的：`web_fetch_exa` 要的是 `urls`
// 数组（写 `url` 会被对端以 -32602 打回来），`web_search_exa` 的必填是
// `query` 与 `objective` 两个。
var demoArgs = map[string]string{
	"web_search_exa":        `{"query":"MCP Streamable HTTP transport","objective":"了解 MCP 的 Streamable HTTP 传输是怎么定义的"}`,
	"web_fetch_exa":         `{"urls":["https://modelcontextprotocol.io/specification/2025-06-18/basic/transports"],"maxCharacters":2000}`,
	"read_wiki_structure":   `{"repoName":"modelcontextprotocol/go-sdk"}`,
	"ask_wiki_question":     `{"repoName":"modelcontextprotocol/go-sdk","question":"Streamable HTTP 传输是怎么实现的？"}`,
	"microsoft_docs_search": `{"query":"azure functions timeout"}`,
	"list_directory":        `{"path":"."}`,
}

// flags 是命令行参数。选项多了，收在一个结构里传，签名不至于长成一串。
type flags struct {
	configPath     string
	callName       string
	callArgs       string
	prompt         string
	workDir        string
	sessionsRoot   string
	sessionKey     string
	maxTurns       int
	timeout        time.Duration
	startupTimeout time.Duration
}

func parseFlags() flags {
	configPath := flag.String("config", "config.json", "配置文件路径，读里面的 mcp.servers 与平台")
	callName := flag.String("call", "", "只调这一个工具（本地名，如 mcp__exa__web_search_exa）；只在 -prompt 为空时用")
	callArgs := flag.String("args", "{}", "-call 的参数，JSON")
	prompt := flag.String("prompt", defaultPrompt, "交给 agent 的任务；传空串表示不跑模型，只列工具并按示例表各调一次")
	workDir := flag.String("workdir", "cmd/mcptest/testdata", "工具的工作目录（须含 AGENTS.md，agent 的定义从它读）")
	sessionsRoot := flag.String("sessions", "testdata/sessions", "会话根目录，相对当前目录；同一个会话 id 重复运行会续写同一个文件")
	sessionKey := flag.String("key", "mcp-001", "会话 id，直接当文件名；留空则新生成一个（chat-…）")
	maxTurns := flag.Int("max-turns", 0, "单次运行的模型调用次数上限，0 表示用默认值")
	timeout := flag.Duration("timeout", 2*time.Minute, "整轮运行的超时时间")
	startupTimeout := flag.Duration("startup-timeout", 60*time.Second, "启动期（含 MCP 连接与工具发现）的总超时时间")
	flag.Parse()

	return flags{
		configPath:     *configPath,
		callName:       *callName,
		callArgs:       *callArgs,
		prompt:         *prompt,
		workDir:        *workDir,
		sessionsRoot:   *sessionsRoot,
		sessionKey:     *sessionKey,
		maxTurns:       *maxTurns,
		timeout:        *timeout,
		startupTimeout: *startupTimeout,
	}
}

func main() {
	flags := parseFlags()

	cfg, err := loadConfig(flags.configPath)
	if err != nil {
		fail(err)
	}
	if err := run(flags, cfg); err != nil {
		fail(err)
	}
}

// run 按有没有任务分流：给了任务就走 agent，空串走"只列工具"那条老路。
func run(flags flags, cfg *config) error {
	if strings.TrimSpace(flags.prompt) == "" {
		ctx, cancel := context.WithTimeout(context.Background(), flags.timeout)
		defer cancel()

		if err := runTools(ctx, cfg, flags.callName, flags.callArgs); err != nil {
			return err
		}
		fmt.Println("\n=== 全部通过 ===")

		return nil
	}

	return runAgent(cfg, flags)
}

// runAgent 走真实 agent：装配（内置工具 + MCP 扩展）、跑一轮、把答复打出来。工具的
// 挑选交给模型，所以工具回什么、它就答什么——这是"真数据"与"模型自己编"的分界。
func runAgent(cfg *config, flags flags) error {
	opts := platformOf(cfg)
	if opts == nil || opts.APIKey == "" {
		return errors.New("config.json 里没有可用的平台（currentPlatform / platforms），跑 agent 需要 apiKey；只想看工具就加 -prompt ''")
	}

	// 工作目录定死成绝对路径：文件工具的路径解析、bash 的 cwd 都以它为根，
	// 相对路径会随进程 cwd 漂移。
	root, err := filepath.Abs(flags.workDir)
	if err != nil {
		return fmt.Errorf("解析工作目录失败: %w", err)
	}

	// 会话落盘：一轮跑完，模型与工具说过什么都在文件里；带上同一个 -key 再跑就是
	// 接着聊（历史从会话重建，压缩也只切到本轮之前）。留空则现生成一个 id，跑完
	// 把它打出来，想续上照着带。
	sessions, err := filepath.Abs(flags.sessionsRoot)
	if err != nil {
		return fmt.Errorf("解析会话根目录失败: %w", err)
	}
	key := flags.sessionKey
	if key == "" {
		key = session.NewSessionID()
	}
	manager, err := session.OpenOrCreate(sessions, root, key)
	if err != nil {
		return fmt.Errorf("取会话失败: %w", err)
	}

	// 头几行先打出来：装配（连 server、发现工具）都在 NewAgent 里面，那一段是安静的，
	// 一个连不上的 server 最长要等满启动期预算。先说清要做什么，卡在哪一步一眼能看出。
	fmt.Printf("=== %s (%s / %s) ===\n", opts.ID, opts.Protocol, opts.Model)
	fmt.Printf("workdir: %s\n任务: %s\n", root, flags.prompt)
	fmt.Printf("会话: %s（接着聊就带上 -key %s）\n", manager.Path(), key)

	servers := cfg.servers()
	fmt.Printf("接入 MCP：%s（启动期预算 %s，单个 server 的 timeout 在它之内）\n",
		serverNames(servers), flags.startupTimeout)

	// 启动期另给一个上限：它是这几个 server 加起来的总预算，单个 server 自己的
	// timeout 在它之下，两者取先到的那个。
	startupCtx, cancelStartup := context.WithTimeout(context.Background(), flags.startupTimeout)
	defer cancelStartup()

	extensions, err := mcp.NewExtension(servers, clientName)
	if err != nil {
		return err
	}

	agent, err := pi.NewAgent(startupCtx, &pi.Options{
		WorkDir:         root,
		ProviderOptions: opts,
		MaxTurns:        flags.maxTurns,
		Observer:        observe,
		TextObserver:    printDelta,
		Session:         manager,
		Extensions:      extensions,
	})
	if err != nil {
		return err
	}
	// 装配结束就把启动期预算放掉，别把它留给运行期。
	cancelStartup()
	fmt.Println("装配完成，开始跑。")

	// 停机期另起一个 ctx：运行期那个此时可能已经超时或被取消，拿它关会话，关闭
	// 请求本身会立刻失败。
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), closeTimeout)
		defer cancel()
		if err := agent.Close(closeCtx); err != nil {
			fmt.Fprintf(os.Stderr, "关闭扩展失败: %v\n", err)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), flags.timeout)
	defer cancel()

	output, err := agent.Run(ctx, &pi.RunInput{Prompt: flags.prompt})
	if err != nil {
		// 出错时答复可能已经生成了一部分，先把它打出来再报错。
		if output != nil && strings.TrimSpace(output.Text()) != "" {
			fmt.Printf("\n=== 出错前已生成的答复 ===\n%s\n", output.Text())
		}

		return err
	}

	fmt.Printf("\n=== 最终答复 ===\n%s\n", output.Text())

	return nil
}

// serverNames 列出将要接入的 server 与入口，装配前先报一遍：那一段没有别的输出，
// 谁知道驱动正在等哪个 server。
func serverNames(servers []mcp.ServerConfig) string {
	names := make([]string, 0, len(servers))
	for _, server := range servers {
		if !server.IsEnabled() {
			continue
		}
		names = append(names, fmt.Sprintf("%s(%s:%s)", server.Name, server.Transport, endpointOf(server)))
	}
	if len(names) == 0 {
		return "没有启用的 server"
	}

	return strings.Join(names, "、")
}

// printDelta 把模型边生成边吐出的文本直接打到终端。
func printDelta(delta string) {
	fmt.Print(delta)
}

// observe 把工具的开始事件打到终端，让人看得见模型到底调了哪个工具、参数是什么。
// 只有开始与增量事件经 emit 送出（pi/tools/event.go 的 EventObserver 契约），结束
// 事件由调度器从返回值交给循环，不会到这里——所以这里没有"成没成"那一行，工具的结果
// 与成败看中间件那几行 `"phase":"end","status":…,"byte_count":…` 的日志。事件会被
// 并发送出，但 fmt 自带互斥，这里的打印够用。与 cmd/harness 那份同一个形状。
func observe(event tools.Event) {
	if event.Phase != tools.EventStart {
		return
	}
	fmt.Printf("\n  → %s %s\n", event.Call.Name, compactArgs(event.Call.Arguments))
}

// compactArgs 把工具参数压成一行，太长就截断，避免刷屏。
func compactArgs(arguments json.RawMessage) string {
	compact := strings.Join(strings.Fields(string(arguments)), " ")
	const maxRunes = 100

	runes := []rune(compact)
	if len(runes) > maxRunes {
		return string(runes[:maxRunes]) + "…"
	}

	return string(runes)
}

// loadConfig 读配置文件。只认 mcp.servers 与平台两段，别的段不动。
func loadConfig(path string) (*config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取 %s 失败: %w", path, err)
	}

	var cfg config
	if err = json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %w", path, err)
	}

	return &cfg, nil
}

// runTools 是不开模型的那条路：配置 → 扩展 → 注册表 → 列工具 → 按示例表各调一次。
func runTools(ctx context.Context, cfg *config, callName, callArgs string) error {
	servers := cfg.servers()

	extensions, err := mcp.NewExtension(servers, clientName)
	if err != nil {
		return err
	}
	if len(extensions) == 0 {
		fmt.Printf("配置里没有启用的 mcp server（%d 项，enabled 都是 false？）。\n",
			len(servers))

		return nil
	}
	fmt.Printf("=== 配置里 %d 个 server，启用 %d 个 ===\n", len(servers), len(extensions))

	runtime, err := extension.NewRuntime(extensions)
	if err != nil {
		return err
	}
	defer func() {
		// 停机期用一个独立的 ctx：上面那个可能已经取消，拿它关会话只会立刻失败。
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), closeTimeout)
		defer cancel()
		if err := runtime.CloseAll(closeCtx); err != nil {
			fmt.Fprintf(os.Stderr, "关闭扩展失败: %v\n", err)
		}
	}()

	registry, err := tools.Register(nil)
	if err != nil {
		return err
	}
	if err := runtime.Register(ctx, registry); err != nil {
		return err
	}
	registry.Freeze()

	groups, skipped := groupByServer(registry, servers)
	printGroups(groups)
	if len(skipped) > 0 {
		// 停用的 server 没连过，它不是"接进来但没工具"，别混在一起说。
		fmt.Printf("跳过（enabled 为 false）：%s\n\n", strings.Join(skipped, "、"))
	}

	if strings.TrimSpace(callName) != "" {
		return callTool(ctx, registry, callName, callArgs)
	}

	// 没指定就按示例参数各调一次：先调的人想看的本来就是"真能拿到数据吗"。
	// 失败的都记下来，一个工具的示例参数过时不该把后面的调用全挡掉。
	var failures []error
	for _, group := range groups {
		for _, item := range group.tools {
			arguments, ok := demoArgs[item.remote]
			if !ok {
				continue
			}
			if err := callTool(ctx, registry, item.local, arguments); err != nil {
				failures = append(failures, err)
			}
		}
	}

	return errors.Join(failures...)
}

// tool 是一个接进来的工具：本地的定义与它在远端的原名。
type tool struct {
	definition schema.ToolDefinition
	local      string
	remote     string
}

// serverGroup 是一个 server 接进来的工具，打印与调用都按它分组。
type serverGroup struct {
	config mcp.ServerConfig
	tools  []tool
}

// groupByServer 把注册表里的工具按 server 分组，顺序跟配置一致，返回的第二个值
// 是停用的 server 名。本地名形如 <tool_prefix>__<server>__<远端名>，这里按
// "__<server>__" 这段切——不假定前缀是什么，tool_prefix 换了也切得对。
//
// 停用的 server 在这里就摘掉：它根本没连过，按"接进来了但没工具"打印会误导。
func groupByServer(registry *tools.Registry, servers []mcp.ServerConfig) ([]serverGroup, []string) {
	definitions := registry.Definitions()

	groups := make([]serverGroup, 0, len(servers))
	var skipped []string
	for _, server := range servers {
		if !server.IsEnabled() {
			skipped = append(skipped, server.Name)

			continue
		}

		group := serverGroup{config: server}
		marker := "__" + server.Name + "__"
		for _, definition := range definitions {
			index := strings.Index(definition.Name, marker)
			if index < 0 {
				continue
			}
			group.tools = append(group.tools, tool{
				definition: definition,
				local:      definition.Name,
				remote:     definition.Name[index+len(marker):],
			})
		}
		groups = append(groups, group)
	}

	return groups, skipped
}

// printGroups 打印每个 server 与它的工具。模型看到的就是名字、展示名与说明，
// 装配完先让人看一眼。
func printGroups(groups []serverGroup) {
	for _, group := range groups {
		fmt.Printf("\n--- %s（%s：%s）\n", group.config.Name,
			group.config.Transport, endpointOf(group.config))
		if len(group.tools) == 0 {
			// 必需 server 走到这里之前就整体失败了，所以空组只可能是非必需 server
			// 连不上、或者白名单里的名字远端没有。
			fmt.Println("    （没接进来：非必需 server 连不上，或白名单里的名字远端没有）")
		}
		for _, item := range group.tools {
			fmt.Printf("    %s", item.local)
			if label := item.definition.Label; label != "" && label != item.local {
				fmt.Printf("（%s）", label)
			}
			fmt.Printf(" —— %s\n", firstLine(item.definition.Description))
		}
	}
	fmt.Println()
}

// endpointOf 打印 server 的入口：http 是 url，stdio 是那串命令。
func endpointOf(config mcp.ServerConfig) string {
	if config.Transport == mcp.TransportStdio {
		return strings.Join(append([]string{config.Command}, config.Args...), " ")
	}

	return config.URL
}

// callTool 调一次工具，把文本与结构化内容都打出来。
func callTool(ctx context.Context, registry *tools.Registry, name, arguments string) error {
	_, tool, ok := registry.Lookup(name)
	if !ok {
		return fmt.Errorf("工具 %s 不在注册表里", name)
	}

	output, err := tool.Execute(ctx, json.RawMessage(arguments), nil)
	if err != nil {
		return fmt.Errorf("调用 %s 出错: %w", name, err)
	}
	text, err := output.Content.Text()
	if err != nil {
		return err
	}

	fmt.Printf("=== 调用 %s\n参数: %s\n结果: %s\n", name, arguments, clip(text))
	if output.Details != nil {
		fmt.Printf("结构化内容: %v\n", clip(fmt.Sprint(output.Details)))
	}

	return nil
}

// clip 把长结果裁到 maxClipRunes 个字符，裁在 rune 边界上，中文不会半个字。
func clip(text string) string {
	runes := []rune(strings.TrimSpace(text))
	if len(runes) <= maxClipRunes {
		return string(runes)
	}

	return string(runes[:maxClipRunes]) + "…（截断）"
}

// firstLine 只取首行，工具说明可能很长。
func firstLine(text string) string {
	text = strings.TrimSpace(text)
	if index := strings.IndexByte(text, '\n'); index >= 0 {
		return text[:index] + " …"
	}

	return text
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "失败:", err)
	os.Exit(1)
}
