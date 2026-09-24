# 从零手搓 Harness 之 MCP 接入：把别人的工具接进自己的注册表

## 前言

这是从零开始搭建 Agent Harness（`go-harness`）的第六篇。第三篇把主循环装上之后，模型手上一直是那四个工具：`read`、`write`、`edit`、`bash`（`impl.NewDefaultTools`，装配在 `pi/agent.go:113`）。想让它查个网页、查个库、读别人的文档站，只能自己写一个工具塞进 `Options.Tools`——每接一个能力就是一次发版。

MCP（Model Context Protocol）把"工具从哪来"这件事标准化了：外部进程或 HTTP 服务按 MCP 说话，客户端连上去把工具列表拉回来。生态里现成的 server 已经不少，文件系统、联网搜索、数据库、文档检索各有实现，换一个只改配置。

这一篇做两件事：`pi/extension` 落成一套最小扩展契约与启动期运行时，`pi/mcp` 作为它的第一个扩展——启动期连远端 server、把远端工具注册进 `tools.Registry`，跑起来之后模型分不出哪个工具是内置的、哪个是远端的。

完整代码参考 GitHub：[https://github.com/PycMono/go-harness](https://github.com/PycMono/go-harness)，本文涉及的文件（`pi/extension/`、`pi/mcp/`、`pi/tools/register.go`、`pi/agent.go`、`cmd/internal/mcpconfig/`、`cmd/mcptest/`）都在仓库里可以直接翻。

## 流程图

接入只发生在装配期，运行期是一次转发。

```text
装配期（pi.NewAgent 里，registry.Freeze 之前）
   │  mcp.NewExtension([]ServerConfig, clientName)
   │     └─ 一个 server 一个 serverExtension
   ▼
extension.Runtime.Register(ctx, registry)
   │
   ├── extension.Register(ctx)        ← 扩展：连 server、列工具、产出 []tools.Tool
   │        ├─ connect：newTransport → Client.Connect（initialize 握手在里面）
   │        ├─ discover：session.Tools() 迭代器翻页 → 名字闸 → newRemoteTool
   │        └─ 失败：required 为真往上抛（致命），为假记一条 Warn 返回空列表
   │
   └── registry.RegisterFor(扩展名, items)   ← Runtime：以扩展名为 owner 整批登记
            └─ 任一步失败：关掉这个扩展，再逆序关已启动的那批
   ▼
registry.Freeze()   工具表定死

运行期
   模型请求里是一份普通的 schema.ToolDefinition（名字带 mcp__ 前缀）
   ▼
Scheduler → remoteTool.Execute()
   │  套一层 config.Timeout
   ▼
session.CallTool(name, args) ──► 远端 server
   │
   └─ 内容块映射：text 直通、image 重编码成 base64、其余类型落占位文本
      成功 → schema.ToolOutput（进模型上下文）
      isError → error（循环不中断，模型看到错误文本）
```

图里有一处值得先点出来：**扩展不碰注册表**。扩展的 `Register` 只返回 `[]tools.Tool`，登记、owner、整批回滚、失败收尾都在 `extension.Runtime` 手里。这条是这一轮改出来的（原先的设想稿把注册面（`API`）递给扩展，让扩展回调进来登记），理由在后面第四节。

## 代码层级划分

```text
pi/
├── agent.go                    改：Options.Extensions、NewAgent 加 ctx、Close、Run 的关闭检查
├── error/
│   └── errors.go               改：81000 段五个码
├── tools/
│   ├── register.go             改：RegisterFor、Rollback
│   ├── constant.go             改：toolOutputTruncationMarker → OutputTruncationMarker
│   └── output.go               改：LimitText 对外（这轮它才有调用方）
├── extension/                  新增包
│   ├── extension.go            Extension / Closer 两个契约
│   ├── runtime.go              Runtime、Register、CloseAll、closeExtension
│   └── runtime_test.go
└── mcp/                        新增包，五个 .go 平铺
    ├── constant.go             TransportType、超时与名字前缀的默认值
    ├── config.go               ServerConfig / validate / header_env / validateHTTP / validateStdio
    ├── tool_impl.go            远端 Tool → tools.Tool：代理、内容映射、名字闸
    ├── extension.go            NewExtension、Register、Close、newTransport、skipOrFail
    └── stderr.go               stderrTail：stdio 子进程 stderr 的尾部采集与凭据隐去

cmd/
├── internal/mcpconfig/         新增包：配置文件里 mcp.servers 那一项的形状
│   ├── mcpconfig.go            Entry / Duration / ServerConfigs / 严格解码
│   └── mcpconfig_test.go
├── harness/
│   ├── main.go                 改：mcp.servers 配置、转成 []mcp.ServerConfig、NewExtension、启动期 ctx、Close
│   └── main_test.go            改：既有测试跟着 NewAgent 的 ctx 走
└── mcptest/                    新增：读配置连真 server 的驱动
    ├── main.go
    └── testdata/
        └── AGENTS.md           驱动自带的工作区：把身份写成"接入了 MCP 工具的 agent"
```

依赖方向单向：`pi` → `pi/mcp` → `pi/extension` + `pi/tools` + `pi/schema` + `pi/error`。反过来一条都没有——`pi/tools` 不认识 extension，`pi/extension` 不认识 mcp，读配置文件那件事更不在 `pi/mcp` 里（它只认 `[]ServerConfig`，谁读文件谁自己转）。`cmd/internal/mcpconfig` 放在 `internal` 下不是随手放的：Go 的 internal 规则正好把"只有 cmd 能读配置"这条边界钉死，`pi` 包想 import 也 import 不到。

| 文件 | 职责 | 对外 |
|---|---|---|
| `pi/extension/extension.go` | 扩展要实现的接口 | `Extension` / `Closer` |
| `pi/extension/runtime.go` | 按声明顺序接入、整批登记、失败收尾、逆序关闭 | `Runtime` / `NewRuntime` |
| `pi/tools/register.go` | 以 owner 为单位整批注册与摘除 | `RegisterFor` / `Rollback` |
| `pi/mcp/config.go` | 运行期那份 server 配置与校验 | `ServerConfig` |
| `pi/mcp/constant.go` | 传输常量、超时与工具名前缀的默认值 | `TransportType` / `TransportHTTP` / `TransportStdio` |
| `pi/mcp/extension.go` | 配置 → `[]extension.Extension`、连接与工具发现 | `NewExtension` |
| `pi/mcp/tool_impl.go` | 远端工具在本地的代理 | 不导出 |
| `pi/mcp/stderr.go` | stdio 子进程 stderr 的尾部采集与凭据隐去 | 不导出 |
| `cmd/internal/mcpconfig/mcpconfig.go` | 配置文件里 `mcp.servers` 的形状 | `Entry` / `ServerConfigs` |

`pi/mcp` 的对外面只有三个名字：`ServerConfig`、两个传输常量、`NewExtension`。代理工具、扩展实现、校验函数全部包内——使用方唯一的注入点是配置，传输由 `ServerConfig.Transport` 选而不是由使用方实现，不导出就不必承诺形状。

## 代码实战

### 1. 协议和传输：用官方 SDK，本仓库只写装配

```text
// go.mod:9
github.com/modelcontextprotocol/go-sdk v1.3.1
```

理由和 `pi/ai/providers` 包住 `openai-go`、`anthropic-sdk-go` 是同一条：线协议交给官方 SDK，本仓库只留自己的契约。MCP 是外部线协议，不是 harness 的内部机理，手写一遍 JSON-RPC 加 SSE 分帧大概七百行，换来的只有"自己实现过"。

SDK 把我们要的东西都做完了：`Client.Connect` 自己完成 `initialize` 握手与版本协商，`ClientSession.Tools` 是自动翻页的迭代器，`StreamableClientTransport` 带重连，`CommandTransport` 就是 stdio。所以"支持两种传输"落下来只是 `newTransport` 里的一个 if：

```go
// pi/mcp/extension.go:233
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
```

http 这条路上唯一要自己写的部分是带头：SDK 只收一个 `*http.Client`，没有别的注入口，所以用一个 `RoundTripper` 把配置里的固定头写进去。

```go
// pi/mcp/extension.go:279
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
```

用户头盖不到 SDK 自己管的头（`Host` / `Content-Length` / `Mcp-Session-Id` / `Content-Type` / `Accept`）：`validate` 已经把这几个拒了，所以这里不必再挡一次。

stdio 那条的进程收尾也交给 SDK：`CommandTransport.Connect` 起进程、把 stdin/stdout 接成双向通道（`mcp/cmd.go:26-47`），`Close` 先关 stdin 等进程退出、超时（`TerminateDuration`，缺省 5s）后 SIGTERM。我们不自己 `Wait`、不自己杀进程，两种传输的关闭路径因此是同一条——扩展的 `Close` 关会话，会话关传输。

### 2. 配置：文件一份、运行期一份

配置文件里那一段是这样的（`config.example.json`）：

```json
{
  "mcp": {
    "servers": [
      {
        "name": "exa",
        "enabled": true,
        "required": true,
        "transport": "http",
        "url": "https://mcp.exa.ai/mcp",
        "timeout": 60,
        "header_env": { "X-Api-Key": "EXA_API_KEY" },
        "allow_tools": ["web_search_exa", "web_fetch_exa"],
        "tool_prefix": ""
      },
      {
        "name": "fs",
        "enabled": false,
        "transport": "stdio",
        "command": "npx",
        "args": ["-y", "@modelcontextprotocol/server-filesystem", "."],
        "cwd": ".",
        "timeout": 60
      }
    ]
  }
}
```

字段名是照着生态里别的客户端抄的（`transport` / `url` / `header_env` / `allow_tools` / `tool_prefix`），抄过来的配置不至于因为名字对不上而静默失效。这面对我们来说新的是 `header_env` 一条：头值写的是环境变量名，装配时从环境里取值——凭据不进配置文件，也就不进版本库。`timeout` 两种写法都收，`60` 是六十秒，`"1m"` 也一样。

这份形状住在读配置的那一侧，`cmd/internal/mcpconfig` 的 `Entry`：

```go
// cmd/internal/mcpconfig/mcpconfig.go:66
// UnmarshalJSON 解码一个 server 条目，拒收不认识的字段。这个严格是必要的：
// encoding/json 默认把不认识的键静默丢掉，而配置多半是从别的客户端抄来的
// （`transport` 抄成 `type`、`header_env` 抄成 `headers`），错名字丢掉的是
// 凭据——server 照常起来，只是每个请求都不带 Key。宁可在这里报出字段名。
func (e *Entry) UnmarshalJSON(data []byte) error {
	// 别名类型：去掉方法集，否则这里会递归调回自己。
	type plain Entry

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()

	var decoded plain
	if err := decoder.Decode(&decoded); err != nil {
		return pierrors.ErrMCPConfigInvalid.Wrap(fmt.Errorf(
			"server 配置解不开（字段名对不上或类型不对，认得的字段: %s）: %w",
			recognizedFields, err))
	}
	*e = Entry(decoded)

	return nil
}
```

`pi/mcp` 那侧认的是另一份结构 `ServerConfig`：同样的字段，但没有 json 标签，也不认文件里的那些拼写。分成两份是刻意的——文件格式跟着别的客户端变、改动频繁，而且只对"读配置的人"有意义；运行期那份是 `pi/mcp` 与装配方之间的契约，手写 `ServerConfig` 的调用方（测试、别的驱动器）不必被迫经过 json。两边靠一个只搬值的函数接上：

```go
// cmd/internal/mcpconfig/mcpconfig.go:133
// ServerConfigs 把读进来的一项项转成运行期结构。只搬值，不做校验：那份结构上该有的
// 规矩（名字、transport、header_env、http 与 stdio 的字段）在 pi/mcp 装配时判，
// 手写 ServerConfig 的调用方与读文件的走同一条路。
func ServerConfigs(entries []Entry) []mcp.ServerConfig {
	configs := make([]mcp.ServerConfig, 0, len(entries))
	for _, entry := range entries {
		configs = append(configs, mcp.ServerConfig{
			Name:       entry.Name,
			Enabled:    entry.Enabled,
			Transport:  mcp.TransportType(entry.Transport),
			URL:        entry.URL,
			Headers:    entry.Headers,
			HeaderEnv:  entry.HeaderEnv,
			Command:    entry.Command,
			Args:       entry.Args,
			Env:        entry.Env,
			Cwd:        entry.Cwd,
			Required:   entry.Required,
			AllowTools: entry.AllowTools,
			ToolPrefix: entry.ToolPrefix,
			Timeout:    time.Duration(entry.Timeout),
		})
	}

	return configs
}
```

校验只有一条路径：`ServerConfig.validate`，在装配期跑。它做四件事，判完顺手把缺省值填上（所以收的是指针）：

1、名字与 `tool_prefix` 必须匹配 `^[a-z0-9][a-z0-9_-]*$`——它们会进工具名，而工具名要经得起两家 Provider 的口径。
2、`transport` 三个分支：http 不许给 `command`，stdio 不许给 `url`/`headers`/`header_env`，其他取值报 `ErrMCPTransportUnsupported`。
3、http 的 `url` 要过 scheme、host，并且拒绝 userinfo 与 query——鉴权一律走 headers，URL 上带凭据迟早会进日志。
4、`header_env` 在这一步换成真的头值：环境变量没设就报错，文案里只出现变量名，不出现值（`resolveHeaderEnv`）。

第 4 条要单独说一句：`validate` 之后，凭据只存在于 `config.Headers` 与请求头里，不进任何一处我们自己拼的文案。日志与错误的字段固定为 `server` / `stage` / `err`。

### 3. 扩展契约：只产出工具，不碰注册表

`pi/extension` 一共两个接口，二十行：

```go
// pi/extension/extension.go:10
// Extension 是启动期接入的扩展。
type Extension interface {
	// Name 是扩展的唯一标识，也是它提供的工具的 owner：注册时由 Runtime 拿这个
	// 名字登记，扩展自己说不上别的。
	Name() string
	// Register 在启动期产出这个扩展提供的工具。ctx 是装配方给的启动期 ctx：
	// 扩展拿它做网络连接（SDK 的 Client.Connect 必须收到 ctx）与日志，装配方
	// 也能用它给整个启动期设上限。
	//
	// 它不碰注册表：产出与登记分开，登记（owner、整批回滚、冻结）归 Runtime 的
	// Register。返回空列表表示这次什么都不接（非必需的 server 连不上就是这条
	// 路），不算失败。
	Register(ctx context.Context) ([]tools.Tool, error)
}

// Closer 是扩展可选的停机清理。Register 失败的那个扩展也会收到 Close——它可能
// 已经占下连接与子进程，收尾是它自己的事；只读不占资源的扩展不必实现它。
type Closer interface {
	Close(context.Context) error
}
```

两个方法加一个可选接口，这是启动期接入能用的最小形状。没有事件、没有钩子、没有配置面——需要什么，扩展自己在 `Register` 里做。

MCP 这边的扩展实现就是一个 server 一个：

```go
// pi/mcp/extension.go:35
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
```

`Register` 是连接加工具发现的合体，它的收尾规则值得逐行看：

```go
// pi/mcp/extension.go:83
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
```

连上之后自己关会话，这是它和非必需跳过那条路的分工：跳过（连不上）与列工具失败这两条路返回的是空列表，Runtime 那边看到的是"成功、零个工具"，收尾不会替它关——漏关就是连接与后台 goroutine 的泄漏，反复建 Agent 会累积。成功那条路把会话记在 `e.session` 上，此后归 `Close` 管；记在"交出去之前"是有原因的：登记那一步也可能失败，那时候要靠 `e.session` 收尾。

### 4. Runtime：登记、owner 与失败收尾

`Runtime` 不存业务状态——某个 server 跳过了什么、为什么跳，是扩展自己的事。它只管三件事：接入顺序、整批登记、收尾。

```go
// pi/extension/runtime.go:17
// Runtime 持有本轮接入的扩展，管接入与停机清理。它不存业务状态：某个扩展
// 跳过了什么、为什么跳，是扩展自己的事。
type Runtime struct {
	extensions []Extension // 调用方给的顺序就是接入顺序，也是逆序收尾的顺序
	started    int         // 接入成功的个数，也就是已启动的那批前缀

	closeOnce sync.Once // 停机只真关一次：CloseAll 可能被重复调用
	closeErr  error     // 第一次 CloseAll 的结果，重复调用原样返回
}
```

接入的循环短得很，但每一步的失败归属都定死了：

```go
// pi/extension/runtime.go:62
func (r *Runtime) Register(ctx context.Context, registry *tools.Registry) error {
	for _, extension := range r.extensions {
		items, err := extension.Register(ctx)
		if err == nil {
			err = registry.RegisterFor(extension.Name(), items)
		}
		if err != nil {
			return errors.Join(err, closeExtension(ctx, extension), r.closeStarted(ctx))
		}

		r.started++
	}

	return nil
}
```

两件事在这里定下来。

一是 owner。`RegisterFor` 的第一个参数是扩展名，而 Runtime 是它唯一的调用方——"工具归谁是结构保证的"这句话就是这么来的：扩展想给自己换个 owner 都没有接口。名字只用来判重与当 owner，不参与排序——接入顺序跟着配置里的书写顺序走，排过一次，后来撤了。

二是收尾。失败分两种：扩展产出工具时失败（远端的事），和登记时失败（比如工具名撞了），后一种失败扩展自己并不知道。两种都由 Runtime 兜：`closeExtension` 关掉刚失败的那个（它不在"已启动"那批里，`started` 只记成功的），`closeStarted` 逆序关已启动的那批。两条错误都 `errors.Join` 进返回值，收尾的错不能盖掉注册的错。

```go
// pi/extension/runtime.go:81
// closeExtension 关掉刚失败的那个扩展。它不在"已启动"那批里（started 只记成功
// 的），而它的 Register 可能已经连上了远端、起了子进程，漏掉就是连接与后台
// goroutine 的泄漏：反复建 Agent 会累积。没实现 Closer 的扩展无事可做。
func closeExtension(ctx context.Context, extension Extension) error {
	closer, ok := extension.(Closer)
	if !ok {
		return nil
	}

	return closer.Close(ctx)
}
```

`CloseAll` 走同一个 `closeStarted`，外面套一个 `sync.Once`：停机只真关一次，重复调用返回第一次的结果（结果存在字段上，否则第二次会返回 nil）。

注册表那侧补了两个方法，一个整批注册、一个整批摘除：

```go
// pi/tools/register.go:99
// RegisterFor 以 owner 名义整批注册；中途失败则把该 owner 本批已注册的
// 全部摘掉，注册表回到调用前的状态（不影响其他 owner 的工具）。
// 同一个 owner 只准注册一次：重复注册返回 ErrToolAlreadyRegistered。这条
// 前提是"回到调用前"能成立的原因——Rollback 按 owner 整批摘除，owner 复用
// 会把先前那批一起摘掉。冻结不在这里判：写入只有 registerLocked 一处，守卫
// 跟着写入点走，就没有绕过去的旁路。
func (r *Registry) RegisterFor(owner string, items []Tool) error {
	// 空 owner 会让 Rollback 变成"摘掉所有空 owner 的工具"，而这批东西再也分不出
	// 是谁的：宁可在入口拒掉。
	if strings.TrimSpace(owner) == "" {
		return pierrors.ErrToolDefinitionInvalid.Wrap(
			errors.New("tool owner must not be empty"))
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// 扫一遍而不是另存一份 owner 名册：注册只发生在装配期，这一次 O(n) 不
	// 值当引入一个新的字段与它的一致性负担。
	for _, registered := range r.tools {
		if registered.owner == owner {
			return pierrors.ErrToolAlreadyRegistered.Wrap(
				fmt.Errorf("owner %q has already registered tools", owner))
		}
	}

	added := make([]string, 0, len(items))
	for _, tool := range items {
		name, err := r.registerLocked(owner, tool)
		if err != nil {
			for _, name := range added {
				delete(r.tools, name)
			}
			return err
		}
		added = append(added, name)
	}

	return nil
}
```

两个细节：一个 owner 只准注册一次，这条把"扩展名唯一"从 Runtime 的校验升级成了注册表的约束；整批要么全进要么全退，所以 Runtime 的 `Register` 不必自己去回滚。冻结的守卫不在这个函数里，在 `registerLocked`（`pi/tools/register.go:178`）——所有写入都经过它，守一处就够，包级那个 `Register` 也是转到这条入口上来的。

### 5. 远端工具怎么变成 tools.Tool

远端工具的形态和本地工具差得不多：名字、描述、一份 JSON Schema。所以要写的只是一个代理，把 `Definition` 映射过来、把 `Execute` 转出去：

```go
// pi/mcp/tool_impl.go:40
// remoteTool 是远端工具在本地的代理，实现 pi/tools 的 Tool。
type remoteTool struct {
	definition schema.ToolDefinition // 本地定义：名字带前缀，Label/Description/schema 从远端映射来
	remoteName string                // 远端原名，tools/call 时用它
	server     string                // 只进错误文案
	session    *sdkmcp.ClientSession // 复用扩展那条会话
	timeout    time.Duration         // 来自配置，Execute 里套一层 ctx 超时
}

// newRemoteTool 把远端 Tool 包成本地工具，远端名字闸开在这里。
func newRemoteTool(
	remote *sdkmcp.Tool,
	session *sdkmcp.ClientSession,
	server, prefix string,
	timeout time.Duration,
) (*remoteTool, error) {
	if !remoteNamePattern.MatchString(remote.Name) {
		return nil, pierrors.ErrToolDefinitionInvalid.Wrap(fmt.Errorf(
			"server %q 的工具名 %q 必须匹配 ^[a-zA-Z0-9_-]{1,64}$", server, remote.Name))
	}

	return &remoteTool{
		definition: schema.ToolDefinition{
			Name:         toolNamePrefix(prefix) + "__" + server + "__" + remote.Name,
			Label:        displayName(remote),
			Description:  remote.Description,
			InputSchema:  remote.InputSchema,
			ParallelSafe: false,
		},
		remoteName: remote.Name,
		server:     server,
		session:    session,
		timeout:    timeout,
	}, nil
}
```

本地名形如 `mcp__exa__web_search_exa`：第一段是 `tool_prefix`（缺省 `mcp`），第二段是 server 名，第三段是远端原名。server 那一段是必须留的，多个 server 有同名工具时靠它分开——跨 server 的唯一性靠结构保证，不靠用户起名字的纪律。

`Label` 按 SDK 注释里写的优先级取：`title` > `annotations.title` > `name`。`ParallelSafe` 一律给假：远端的并发安全我们不知道，宁可同批调用串着走。

`Execute` 是一条直线，中间那三个判断各有原因：

```go
// pi/mcp/tool_impl.go:90
func (t *remoteTool) Execute(
	ctx context.Context, arguments json.RawMessage, _ *tools.UpdateEmitter,
) (*schema.ToolOutput, error) {
	// 超时套在传进来的 ctx 上：SDK 自己的重试也落在同一个窗口里。
	callCtx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()

	// 空的 RawMessage 不能直接进 CallToolParams：它会在 json.Marshal 那步报错。
	// 参数被省略时按空对象发。
	if len(arguments) == 0 {
		arguments = json.RawMessage("{}")
	}

	result, err := t.session.CallTool(callCtx, &sdkmcp.CallToolParams{
		Name:      t.remoteName,
		Arguments: arguments,
	})
	if err != nil {
		// 超时与取消原样返回，别套 81004：CodeOf 会先看到 MCP 码，调用方就分不出
		// "远端坏了"和"我这轮超时了"。
		if ctxErr := callCtx.Err(); ctxErr != nil {
			return nil, ctxErr
		}

		return nil, pierrors.ErrMCPToolCallFailed.Wrap(fmt.Errorf(
			"server %q 工具 %q: %w", t.server, t.remoteName, err))
	}
	if result.IsError {
		return nil, pierrors.ErrMCPToolCallFailed.Wrap(
			errors.New(errorText(result.Content)))
	}

	// 成功路径的长度兜底交给 pi/tools.LimitText：它截断时保证 UTF-8 完整、追加
	// 同一个标记，并在 Details 上记 truncated。它返回的是值，这里取地址再交出。
	return new(tools.LimitText(schema.ToolOutput{
		Content: contentBlocks(result.Content),
		Details: result.StructuredContent,
	}, maxRemoteTextBytes)), nil
}
```

超时那一条是全局规则：`pi/error.CodeOf` 先看错误链里的带码错误，再看 `context.Canceled` / `context.DeadlineExceeded`（`pi/error/errors.go:186-201`），所以 `ErrMCPToolCallFailed.Wrap(ctx.Err())` 的结果是 81004 而不是 40001，调用方就分不出"远端坏了"和"我这轮超时了"。规则是超时与取消原样返回，其余失败才套 MCP 码。启动期同理：因 ctx 超时而失败给 40001，不是 81002。

内容块的映射是这一层唯一有信息损耗的地方：

```go
// pi/mcp/tool_impl.go:131
// contentBlocks 把远端内容块映成本地内容块，顺序原样保持。
func contentBlocks(content []sdkmcp.Content) schema.ContentBlocks {
	blocks := make(schema.ContentBlocks, 0, len(content))
	for _, item := range content {
		switch typed := item.(type) {
		case *sdkmcp.TextContent:
			blocks = append(blocks, schema.TextBlock(typed.Text))
		case *sdkmcp.ImageContent:
			blocks = append(blocks, schema.ContentBlock{
				Type: schema.ContentTypeImage,
				Image: &schema.ImageContent{
					// SDK 的 Data 在反序列化时已经从 wire 上的 base64 还原成原始
					// 字节，本地协议要的是 base64 字符串，得重新编码。
					Data:     base64.StdEncoding.EncodeToString(typed.Data),
					MIMEType: typed.MIMEType,
				},
			})
		default:
			// audio / resource / resource_link 落到这里：写明类型与标识，不静默丢。
			blocks = append(blocks, schema.TextBlock(placeholderText(item)))
		}
	}

	return blocks
}
```

text 直通，image 做一次 base64 重编码（SDK 反序列化时已经把它还原成字节了，本地协议要的还是字符串），audio / resource / resource_link 落成一条占位文本——本地协议装不下，但不静默丢，模型至少知道"那边回了点别的东西"。

错误那条路是另一份拼法：

```go
// pi/mcp/tool_impl.go:177
// errorText 拼 isError 的错误文本：text 按原序拼，其余类型只出占位——image
// 绝不能把 base64 带进来（一张图几百 KB，日志与模型上下文都撑不住，而截图
// 本身可能带敏感信息）。拼完过一道长度兜底。
func errorText(content []sdkmcp.Content) string {
	var builder strings.Builder
	for _, item := range content {
		switch typed := item.(type) {
		case *sdkmcp.TextContent:
			builder.WriteString(typed.Text)
		case *sdkmcp.ImageContent:
			builder.WriteString("[图片]")
		default:
			builder.WriteString(placeholderText(typed))
		}
	}
	if builder.Len() == 0 {
		return "远端返回了空错误"
	}

	return limitText(builder.String())
}
```

`isError` 的内容要给模型看见——它得据此自己纠正参数，所以这一处不做脱敏，只受长度约束。但 image 是个例外：一张图几百 KB 的 base64 进了日志和上下文就是灾难，所以只出 `[图片]` 两个字。

长度兜底给的是 1 MiB（`maxRemoteTextBytes`，量级取自 `pi/prompt.go` 的 AGENTS.md 上限）：MCP 的 `tools/call` 没有 `offset` 这类续读手段，截掉的部分补不回来，所以它不是预算而是兜底——碰到了说明这个 server 在成批吐数据。成功路径复用 `pi/tools.LimitText`，错误路径要的是 error 不是 `ToolOutput`，走不到那个函数，所以复用同一个上限和同一个截断标记（`limitText`）。

### 6. 失败策略：required 两档加 stderr 尾部

一个 server 连不上，是致命还是"少一批工具"，这件事得能声明。开关就是配置里的 `required`：

```go
// pi/mcp/extension.go:214
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
```

`required: true` 的 server 失败会让整个 Agent 装配失败（远端是必需依赖，缺了不该装作没事）；默认为假，记一条 Warn、返回空列表，其余 server 与 Agent 照常起。配置写错不走这条路：配置类错误一律致命，与 `required` 无关——`required` 管的是远端可达性，配置写错是本地的事，混在一个开关下只会让"错在哪"变难查。

Warn 的字段固定成 `server` / `stage` / `err` 三个。凭据不进日志这条纪律就落在这里：`err` 里裹着的 SDK 错误是原样上抛的（可能带 endpoint 与对端响应体片段，剪掉就没诊断信息了），真正要做的是保证 endpoint 里没有凭据——`validate` 拒绝 URL 上的 userinfo 与 query，就是为这个。

stdio server 起不来时，协议层只看到一句 `calling "initialize": EOF`，能说明原因的东西全在子进程的 stderr 里。SDK 的 `CommandTransport` 只管 stdout/stdin（`Connect` 只做两个 pipe 与 `Start`，压根不碰 `Stderr`，`mcp/cmd.go:26-47`），而 `Stderr` 为 nil 时 Go 会把子进程 stderr 送进空设备。所以我们能下的手只有一处：`newTransport` 里把 `command.Stderr` 接到自己的收集器上。

```go
// pi/mcp/stderr.go
// stderrTail 收子进程 stderr 的尾部。stdio server 起不来时（包没装、Node 崩了、
// 版本不对）它吐的那几行往往是唯一能说明原因的东西，而协议层看到的只有
// `calling "initialize": EOF`。
//
// 三条取舍：
//   - 只留尾部。一直写就一直丢前面的，内存有上限；写满了也不阻断子进程。
//   - 只在 server 级失败（连接、工具发现）时读，正常接入时不读——健康的 server
//     也爱往 stderr 写 npm 警告之类，进日志只是噪声。
//   - 读的时候按已知凭据（配置里的头值与 env 值）过一遍，值换成占位文本：子进程
//     把自己环境里的 Key 打出来是很常见的事。
type stderrTail struct {
	mu      sync.Mutex
	buf     []byte
	dropped bool
	secrets []string
}
```

三条取舍里的第三条是安全和可读性的折中：凭据表在装配时定一次，取的是 `Headers` 的值与 `env` 的值——这两个正是我们交给子进程、它最可能打回 stderr 的东西。短于八个字符的值不参与替换（`1`、`dev`、PATH 片段这类换掉了只会把 stderr 切得读不懂）。替换是纯字符串相等替换，不猜格式、不做正则：认得的就隐掉，不假装能认出别的。

读出来的时候把最后八行拼成一行（换行会把一条日志切碎），前面还有内容被丢掉就加一个 `…` 前缀，别让人把这几行当成全部。

### 7. 装配：agent.go 里那几行

`pi.NewAgent` 这一段是全部接线：

```go
// pi/agent.go:119
	// 注册工具
	registry, err := tools.Register(opts.Tools)
	if err != nil {
		return nil, err
	}

	// 注入扩展
	runtime, err := extension.NewRuntime(opts.Extensions)
	if err != nil {
		return nil, err
	}
	if err := runtime.Register(ctx, registry); err != nil {
		return nil, err
	}
	registry.Freeze()
```

顺序是有意的：内置工具与外部传入的工具先进表，扩展的工具随后，全部就位才 `Freeze`。冻结之后注册表不再接受新增——所以远端工具的热重载（`notifications/tools/list_changed`）这轮不做，通知无处可施。

`NewAgent` 多收了一个 `ctx`，它是启动期预算：扩展拿它做连接（SDK 的 `Connect` 必须收到 ctx），调用方也能用它给整个启动期设一个总上限——一个连不上的 server 最长占满它自己的 `timeout`，多个加起来就是 N 倍，这道总账得有人兜。驱动里给的是 `-startup-timeout`（缺省 60 秒），装配一结束就 `cancel`，不留给运行期。

停机那侧只有两行，但顺序不能反：

```go
// pi/agent.go:173
func (a *Agent) Close(ctx context.Context) error {
	// 先标记、再关扩展：反过来的话，新 Run 会溜进一个正在关闭的会话，拿到的是
	// 莫名其妙的调用失败，而不是 ErrClosed。
	a.closed.Store(true)

	return a.runtime.CloseAll(ctx)
}
```

`Close` 不等正在跑的那一轮，也不取消它——`Run` 归调用方的 ctx 管。已经发出去的那次 MCP 调用会照常跑完（SDK 的会话关闭要等在途请求返回），此后同一轮里新起的调用会失败，`Run` 本身照常返回。

### 8. 驱动：cmd/mcptest

`cmd/mcptest` 是这层的验证端子，两种跑法。`-prompt ""` 不开模型：按配置列工具，再照远端的 `inputSchema` 给常见工具各调一次示例参数——这条路不需要平台配置，也不要 Key。

```go
// cmd/mcptest/main.go:341
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
	// …（后面是分组打印，再按示例表逐个调用）
```

省略的那几行是分组打印加逐个调用：`groupByServer` 把注册表里的工具按 server 分组（本地名按 `__<server>__` 那段切，不假定前缀是什么），`demoArgs` 那张表里的工具各调一次，表里没有的不猜参数，失败的都记下来一起报（`cmd/mcptest/main.go:378-404`）。

示例参数是照着远端的 `inputSchema` 写的，不是猜的：`web_fetch_exa` 要的是 `urls` 数组（写 `url` 会被对端以 -32602 打回来），`web_search_exa` 的必填是 `query` 与 `objective` 两个。表里没有的工具不猜参数，只提示怎么自己调。

另一种跑法把任务交给模型，模型自己挑工具：

```text
go run ./cmd/mcptest                          # 默认任务：查北京今天的天气
go run ./cmd/mcptest -prompt "查一下上海明天的天气"
```

工作区里必须有 `AGENTS.md`，agent 的定义从它读。默认用驱动自带的 `cmd/mcptest/testdata`，那份把身份写成"接入了 MCP 工具的 agent"，并写明工具名以 `mcp__` 开头——借别的工作区会让模型把任务当成本地能力问题，先去 grep 工作区、看技能目录，然后拒答。

## 跑起来看

装两个不需要 Key 的 server 就够看全程：deepwiki（http）与 fs（stdio，`npx -y @modelcontextprotocol/server-filesystem`）。配置是临时写的一份，只开了这两个。

```
$ go run ./cmd/mcptest -config /tmp/mcpdemo.json -prompt ""

=== 配置里 2 个 server，启用 2 个 ===

--- deepwiki（http：https://mcp.deepwiki.com/mcp）
    mcp__deepwiki__ask_wiki_question（ask_wiki_question） —— Ask any question about a GitHub repository's codebase and get an AI-powered answer …
    mcp__deepwiki__read_wiki_structure（read_wiki_structure） —— Get a list of documentation topics for a GitHub repository.

--- fs（stdio：npx -y @modelcontextprotocol/server-filesystem .）
    mcp__fs__create_directory（Create Directory） —— Create a new directory or ensure a directory exists. …
    mcp__fs__directory_tree（Directory Tree） —— Get a recursive tree view of files and directories as a JSON structure. …
    mcp__fs__edit_file（Edit File） —— Make line-based edits to a text file. …
    （中间 10 个略）
    mcp__fs__write_file（Write File） —— Create a new file or completely overwrite an existing file with new content. …
```

两个 server 一共列出 16 个工具，本地的名字全是 `mcp__<server>__<远端名>`，`（…）` 里是远端给的 `title`。stdio 这条真的起了一个子进程——`npx` 第一次跑要下包，冷启动那一趟算在它自己的 `timeout` 里，所以配置给的是 120 秒而不是缺省的 30。

接着按示例表各调一次，下面截了三个：

```
=== 调用 mcp__deepwiki__ask_wiki_question
参数: {"repoName":"modelcontextprotocol/go-sdk","question":"Streamable HTTP 传输是怎么实现的？"}
结果: 您正在询问 `modelcontextprotocol/go-sdk` 仓库中 Streamable HTTP 传输的实现方式。  Streamable HTTP 传输通过结合 HTTP POST 请求和 Server-Sent Events (SSE) 来实现客户端与服务器之间的通信，支持有状态和无状态部署，并可选地支持流恢复。 

## 架构概览

Streamable HTTP 传输由客户端和服务器端组件协同工作，提供双向 HTTP 通信。 

### 客户端实现

客户端通过 `StreamableClientTransport` 与服务器通信。

1.  **连接生命周期**: `StreamableClientTransport.Connect` 方法创建一个 `streamableClientConn` 实例。
2.  **初始化**: 客户端发送 `initialize/initialized` 消息，并从服务器获取 `Mcp-Session-Id` 头部信息。 之后，通过 `streamableClientConn.connectStandaloneSSE` 启动独立的 SSE 流。
3.  **发送消息**: `streamableClientConn.Write` 方法通过 HTTP POST 请求发送所有出站消息。
4.  **接收消息**: `stream…（截断）
结构化内容: map[result:您正在询问 `modelcontextprotocol/go-sdk` 仓库中 Streamable HTTP 传输的实现方式。 …]
```

`结果` 那几行是远端 server 自己生成的（deepwiki 拿它索引过的仓库回答），不是本地模型答的——`结构化内容` 那一行是 `structuredContent` 落进 `ToolOutput.Details` 的结果，它把同一段文本又重复了一遍，这里只留开头。句尾那个 `（截断）` 不是我们那层的标记（`pi/tools.LimitText` 的标记是 `[output truncated]`，上限 1 MiB，够不着），是驱动打印时按 600 字截的（`clip`，`cmd/mcptest/main.go:512`）——真 server 一次搜索回来几千字，终端里看个开头就够。同一个问句在两次运行里回来的措辞不完全一样，远端是模型在答。

```
=== 调用 mcp__fs__list_directory
参数: {"path":"."}
结果: [DIR] .claude
[DIR] .git
[FILE] .gitignore
[DIR] .idea
[DIR] .superpowers
[FILE] LICENSE
[FILE] Loop 篇.md
[FILE] README.md
[DIR] cmd
[FILE] config.example.json
[FILE] config.json
[DIR] docs
[FILE] go.mod
[FILE] go.sum
[DIR] pi
[DIR] testdata
[FILE] 开篇：从零手搓一个 Harness.md

=== 全部通过 ===
```

`list_directory` 回来的是真目录数据，`[FILE]` / `[DIR]` 是对端加的格式。这一条证明 stdio 那条路是通的：协议在 stdin/stdout 上，子进程的工作目录（`cwd: "."`）落在驱动进程的当前目录。

再看失败那两条。把 `command` 换成一个不存在的包名，`required: true`：

```
$ go run ./cmd/mcptest -config /tmp/mcpbroken.json -prompt ""
=== 配置里 1 个 server，启用 1 个 ===
失败: 81002|MCP server 连接或工具发现失败: server "broken": calling "initialize": EOF（子进程 stderr 最后几行: npm error code E404 / npm error 404 Not Found - GET https://registry.npmjs.org/@modelcontextprotocol%2fserver-nope-does-not-exist - Not found / npm error 404 / npm error 404  The requested resource '@modelcontextprotocol/server-nope-does-not-exist@*' could not be found or you do not have permission to access it. / npm error 404 / npm error 404 Note that you can also install from a / npm error 404 tarball, folder, http url, or git url. / npm error A complete log of this run can be found in: /Users/allen/.npm/_logs/2026-09-24T06_39_27_145Z-debug-0.log）
```

协议层那句话是光秃秃的 `calling "initialize": EOF`，后面括号里那一段才是能用的信息——`stderrTail` 抓下来的 npm 404 全文。同一个 server 改成 `required: false`，它就从致命错误变成一行 Warn，其余 server 照常起来：

```
=== 配置里 2 个 server，启用 2 个 ===
{"caller":"Warn[logrus.go:80]","err":"81002|MCP server 连接或工具发现失败: server \"broken\": calling \"initialize\": EOF（子进程 stderr 最后几行: npm error code E404 / … / npm error A complete log of this run can be found in: /Users/allen/.npm/_logs/2026-09-24T06_41_34_216Z-debug-0.log）","level":"warning","module":"demo","msg":"mcp server skipped","server":"broken","stage":"connect","time":"2026-09-24 14:41:35.421328"}

--- broken（stdio：npx -y @modelcontextprotocol/server-nope-does-not-exist）
    （没接进来：非必需 server 连不上，或白名单里的名字远端没有）

--- deepwiki（http：https://mcp.deepwiki.com/mcp）
    mcp__deepwiki__read_wiki_structure（read_wiki_structure） —— Get a list of documentation topics for a GitHub repository.
```

Warn 那行的字段就是 `server` / `stage` / `err`（外面那层是日志库加的）。三条跑完都没有残留子进程——`agent.Close` 或 `runtime.CloseAll` 把会话关掉，SDK 关 stdin、等进程退出。

## 与 pi.dev 差在哪

前几篇的参考实现是 pi.dev（[badlogic/pi-mono](https://github.com/badlogic/pi-mono)），这一层对不上：MCP 客户端在这一版里不是内建能力。仓库里"mcp"一共三处命中（`c7cdb46`），一处是测试夹具里那个包名（`npm:pi-mcp-adapter`，`packages/coding-agent/test/settings-manager-bug.test.ts:45`），一处是注释把 MCP 桥和截图工具并列算作"扩展"（`packages/coding-agent/src/utils/tool-result-images.ts:17`），第三处是 OAuth 权限串里的 `user:mcp_servers`（`packages/ai/src/auth/oauth/anthropic.ts:37`）——没有客户端实现，它把 MCP 放在扩展位，靠外部包接。

扩展位的形状两边是一样的，我们这一轮往它那边靠了一半，另一半反着走：

| 项 | pi.dev（`c7cdb46`） | go-harness |
|---|---|---|
| 扩展的注册面 | `ExtensionAPI`（`packages/coding-agent/src/core/extensions/types.ts:1261`），一个接口装了五六十个方法：事件订阅、注册工具、注册命令、快捷键、标志位…… | `Extension` 两个方法（`Name` / `Register`），外加可选的 `Closer` |
| 工具谁登记 | 扩展自己调 `registerTool(tool)`（`packages/coding-agent/src/core/extensions/loader.ts:273`，声明在 `types.ts:1333`），注册面直接交给扩展 | 扩展只产出 `[]tools.Tool`，登记、owner、整批回滚归 `Runtime` |
| 名字归谁 | 扩展给 `tool.name`，重名由 `extension.tools` 这个 map 判 | 本地名由 `<tool_prefix>__<server>__<远端名>` 拼出来，重名在 `Registry.RegisterFor` 那一步整批拒绝 |
| 失败粒度 | 扩展是包，加载失败按包的规矩处理 | 一个 server 一个扩展，`required` 决定连不上是致命还是跳过 |

第二行是这一轮改出来的方向。设想稿里的 `API` 就是 `ExtensionAPI` 的那种做法：Runtime 把注册面递给扩展，扩展回调进来登记。落下来之后发现扩展拿到的这个面只有一个方法，做的又是"把 owner 换成扩展名再转手给 `RegisterFor`"这一件事——于是改成"扩展产出工具、装配方登记"，`API` 与它那个只做转发和改 owner 的适配器一起删了。owner 从"靠适配器固定"变成"只有 Runtime 调 `RegisterFor`"，是结构上的固定，不是约定。

至于 pi.dev 那个宽接口，它不是错——它接的是 TUI 产品形态：扩展要加斜杠命令、加快捷键、挂 UI、接事件。我们这层只服务一件事（启动期产出工具），接口宽出来的每一条都得有实现、有测试、有文档，现在一条都不需要。

## 总结

回头看流程图，这一层是三个部分。

扩展契约（`pi/extension`）只有两个方法：说清自己叫什么，交出自己产出的工具。扩展不知道注册表长什么样，也不知道别处还有几个扩展——失败粒度、连接与关闭的时机全由 Runtime 统一处理。

运行时（`pi/extension/runtime.go`）管接入顺序、整批登记与收尾。顺序按调用方声明，收尾逆序、只真关一次；失败的那个扩展不在"已启动"那批里，得单独关，这条漏了不报错，只会累积泄漏，所以它单独有一个函数和一组测试。

适配层（`pi/mcp`）把两份外部形状翻译成本地的：配置翻译成 `ServerConfig`，远端 Tool 翻译成 `tools.Tool`。翻译完，模型视角下远端工具与内置工具没有任何区别——同样的 `ToolDefinition`、同样过 schema 参数校验、同样走 `Scheduler` 的执行链与事件。

下一篇打算写上下文压缩（compaction）——历史越过窗口线之后怎么压、压缩点与从盘上折回来的历史怎么对齐。感兴趣的话关注一下，防止走丢。
