package mcp

import (
	"context"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"sort"
	"strings"

	logsdk "github.com/PycMono/go-logger-sdk"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	pierrors "github.com/PycMono/go-harness/pi/error"
	"github.com/PycMono/go-harness/pi/extension"
	"github.com/PycMono/go-harness/pi/tools"
)

// NewExtension 把配置里的每个 server 变成一个扩展：一个 server 一个扩展，失败
// 粒度才能落在单个 server 上——连不上 exa 不该掀掉整个 Agent。返回切片而不是
// 单个扩展，装配方直接 append 进 Options.Extensions；按 server 拆分的循环关在
// 这个包内，pi 根包不认识 server 这个概念。
//
// clientName 是 initialize 时报给远端的身份，空串取 defaultClientName。它是
// client 级、不是 server 级的：同一个 harness 对每个 server 报同一个名字，所以
// 它进这个参数而不是进 ServerConfig（远端拿它做统计与兼容分支，身份随 server
// 变没有意义）。
//
// 配置怎么读（读哪个文件、哪一段）不在这里：本包只认 []ServerConfig，装配方自己
// 读自己那份文件，就像它读平台配置那样。
func NewExtension(servers []ServerConfig, clientName string) ([]extension.Extension, error) {
	if clientName == "" {
		clientName = defaultClientName
	}

	extensions := make([]extension.Extension, 0, len(servers))
	for index := range servers {
		// 拷一份再校验：validate 会填 Timeout 与 AllowTools 的默认值、把
		// header_env 换成头值，不该改到调用方那份。
		config := servers[index]
		// enabled:false 的条目不接也不校验：停用它是个明确的选择（Key 还没
		// 拿到、服务正在维护），不该因为它而让整套装配失败。
		if !config.IsEnabled() {
			continue
		}
		if err := config.validate(); err != nil {
			return nil, err
		}

		extensions = append(extensions, &serverExtension{
			config:     config,
			clientName: clientName,
			stderr:     newStderrTail(config),
		})
	}

	return extensions, nil
}

type serverExtension struct {
	config ServerConfig
	// clientName 是 initialize 时报给远端的名字，NewExtension 已经填过缺省，
	// 所有扩展共一份。
	clientName string
	// stderr 收 stdio 子进程的 stderr 尾部，只在 server 级失败时读出来（见
	// stderr.go 的三条取舍）。http 那条路上它一直是空的。
	stderr  *stderrTail
	session *sdkmcp.ClientSession // Register 连上后拿到；Close 关它
}

func (e *serverExtension) Name() string { return "mcp:" + e.config.Name }

// Register 连接这个 server 并产出它的工具；登记由 Runtime 拿这个扩展的名字做
// owner 完成。连上之后自己关会话：跳过（非必需的 server 连不上）与列工具失败这两
// 条路返回空列表，Runtime 那边看着都是"成功、零个工具"，收尾不会替它关，漏关就是
// 连接与后台 goroutine 的泄漏（反复建 Agent 会累积）。成功那条路把会话记在
// e.session 上，此后归 Close 管——登记失败时 Runtime 也是靠它收尾，所以要在交出去
// 之前就记上。
func (e *serverExtension) Register(ctx context.Context) ([]tools.Tool, error) {
	session, err := e.connect(ctx)
	if err != nil {
		return nil, e.skipOrFail(ctx, "connect", err)
	}

	items, err := e.discover(ctx, session)
	if err != nil {
		_ = session.Close()

		return nil, e.skipOrFail(ctx, "list tools", err)
	}

	e.session = session

	return items, nil
}

func (e *serverExtension) Close(ctx context.Context) error {
	// Close 只关会话。从没连上的、连上后被跳过的 server，session 都是 nil。
	if e.session == nil {
		return nil
	}

	return e.session.Close()
}

// connect 建会话，initialize 握手也在内，整段共用一个 config.Timeout。
func (e *serverExtension) connect(ctx context.Context) (*sdkmcp.ClientSession, error) {
	transport, err := newTransport(e.config, e.stderr)
	if err != nil {
		return nil, err
	}
	connectCtx, cancel := context.WithTimeout(ctx, e.config.Timeout)
	defer cancel()

	client := sdkmcp.NewClient(&sdkmcp.Implementation{
		Name:    e.clientName,
		Version: clientVersion,
	}, nil)
	session, err := client.Connect(connectCtx, transport, nil)
	if err != nil {
		// 超时与取消原样返回：启动期因 ctx 超时而失败是 40001，不是 81002。
		if ctxErr := connectCtx.Err(); ctxErr != nil {
			return nil, ctxErr
		}

		return nil, pierrors.ErrMCPConnectFailed.Wrap(fmt.Errorf(
			"server %q: %w", e.config.Name, err))
	}

	return session, nil
}

// discover 拉全量工具，过白名单与名字闸，产出本地工具。列工具另算一个
// config.Timeout：迭代器只收我们给的这一个 ctx，SDK 内部翻多少页都算在这一轮里。
func (e *serverExtension) discover(
	ctx context.Context,
	session *sdkmcp.ClientSession,
) ([]tools.Tool, error) {
	listCtx, cancel := context.WithTimeout(ctx, e.config.Timeout)
	defer cancel()

	remote := make(map[string]*sdkmcp.Tool)
	for item, err := range session.Tools(listCtx, nil) {
		if err != nil {
			if ctxErr := listCtx.Err(); ctxErr != nil {
				return nil, ctxErr
			}

			return nil, pierrors.ErrMCPConnectFailed.Wrap(fmt.Errorf(
				"server %q 列工具: %w", e.config.Name, err))
		}

		remote[item.Name] = item
	}

	if missing := e.missingAllowed(remote); len(missing) > 0 {
		return nil, pierrors.ErrMCPToolNotFound.Wrap(fmt.Errorf(
			"server %q 白名单里的工具远端没有: %s",
			e.config.Name, strings.Join(missing, ", ")))
	}

	names := e.selected(remote)
	items := make([]tools.Tool, 0, len(names))
	for _, name := range names {
		item, err := newRemoteTool(
			remote[name], session, e.config.Name, e.config.ToolPrefix,
			e.config.Timeout)
		if err != nil {
			return nil, err
		}

		items = append(items, item)
	}

	return items, nil
}

// selected 要注册的远端工具名，按名字排序：这个次序决定工具在 Provider 请求里
// 的次序，不排的话每次装配都可能不一样。
func (e *serverExtension) selected(remote map[string]*sdkmcp.Tool) []string {
	names := make([]string, 0, len(remote))
	for name := range remote {
		if len(e.config.AllowTools) > 0 && !slices.Contains(e.config.AllowTools, name) {
			continue
		}

		names = append(names, name)
	}
	sort.Strings(names)

	return names
}

// missingAllowed 白名单里远端没有的名字，保持白名单自己的顺序。
func (e *serverExtension) missingAllowed(remote map[string]*sdkmcp.Tool) []string {
	var missing []string
	for _, name := range e.config.AllowTools {
		if _, exists := remote[name]; !exists {
			missing = append(missing, name)
		}
	}

	return missing
}

// skipOrFail 决定一个 server 级失败是致命还是跳过，跳过那条 Warn 记在这里——
// 只有这一层知道是哪个 server、哪一步、为什么。子进程 stderr 的尾部也在这里
// 贴上：两条路（致命与跳过）都从这一个出口出去，贴在一处就都带上了。用 %w 包，
// 错误码与错误链不变。
func (e *serverExtension) skipOrFail(ctx context.Context, stage string, err error) error {
	if tail := e.stderr.tail(); tail != "" {
		err = fmt.Errorf("%w（子进程 stderr 最后几行: %s）", err, tail)
	}
	if e.config.Required {
		return err
	}
	logsdk.Warn(ctx, "mcp server skipped",
		logsdk.Any("server", e.config.Name),
		logsdk.Any("stage", stage),
		logsdk.Any("err", err),
	)

	return nil
}

// newTransport 装配两种传输。transport 的判别归 validate：它已经把取值收敛到
// http 与 stdio，走到这里 default 那一支只兜底。stderr 只给 stdio 用，是子进程
// 的收尾线索（见 stderr.go）。
func newTransport(config ServerConfig, stderr io.Writer) (sdkmcp.Transport, error) {
	if config.Transport == TransportStdio {
		command := exec.Command(config.Command, config.Args...)
		command.Dir = config.Cwd
		command.Stderr = stderr
		// 没配 env 就给 nil：nil 表示继承父进程环境，stdio server 多半要
		// PATH 之类才起得来。配了就按"父进程环境 + 覆盖"给全，否则连
		// PATH 都没了。
		if len(config.Env) > 0 {
			command.Env = environ(config.Env)
		}

		return &sdkmcp.CommandTransport{Command: command}, nil
	}

	return &sdkmcp.StreamableClientTransport{
		Endpoint: config.URL,
		// 不设 http.Client.Timeout：设了就是两套账——http.Client.Timeout 按每次
		// 尝试算，而 SDK 自己的重试会绕着它从头再来一次。超时统一走 ctx。
		HTTPClient: &http.Client{Transport: headerRoundTripper{
			base:    http.DefaultTransport,
			headers: config.Headers,
		}},
	}, nil
}

// environ 是父进程环境加上配置里的覆盖，按变量名排序——顺序稳定，同名的以后
// 出现的为准。
func environ(overrides map[string]string) []string {
	env := os.Environ()
	for _, name := range slices.Sorted(maps.Keys(overrides)) {
		env = append(env, name+"="+overrides[name])
	}

	return env
}

// headerRoundTripper 把配置里的固定头加在 SDK 发出的每个请求上——SDK 只收
// *http.Client，没有别的注入口。用户头盖不到 SDK 自己的头（Host /
// Content-Length / Mcp-Session-Id / Content-Type / Accept）：validate 已经把
// 这几个拒了，所以这里不必再挡一次。
type headerRoundTripper struct {
	base    http.RoundTripper
	headers map[string]string
}

func (t headerRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	// Clone 会连 Header 一起深拷，改副本不动调用方那一份。
	cloned := request.Clone(request.Context())
	for name, value := range t.headers {
		// Set 而不是 Add：配置里写的值是"这个头的全部内容"，追加会变成用户
		// 以为在覆盖、实际拼出重复头。
		cloned.Header.Set(name, value)
	}

	return t.base.RoundTrip(cloned)
}
