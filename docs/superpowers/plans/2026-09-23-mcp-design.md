# MCP 接入方案

## 一句话

`pi/extension` 落地成最小扩展契约与启动期运行时，`pi/mcp` 作为第一个扩展：启动期连远端 MCP server、把远端工具以固定前缀注册进 `tools.Registry`，调用时转发并映射回 `schema.ContentBlocks`。协议与传输用官方 `modelcontextprotocol/go-sdk`，本仓库只写配置、装配与适配。两种传输都做：Streamable HTTP 与 stdio。

## 决策一览（先看这个）

| # | 决策 | 选择 | 备选与代价 |
|---|---|---|---|
| 1 | 协议与传输 | 用官方 `modelcontextprotocol/go-sdk` | 手写约 700 行 JSON-RPC + SSE + session；见「传输与协议」的三条理由 |
| 2 | 传输范围 | Streamable HTTP 与 stdio 都做，配置里 `transport` 判别 | 只做 http：能拿到的 MCP server 配置大半是 stdio 形态（`npx`/`uvx` 起进程），只做一半等于大半用不了 |
| 3 | 扩展粒度 | 一个 server 一个扩展 | 一个扩展接多个 server：owner 无法按 server 隔离回滚 |
| 4 | 握手时机 | 启动期，冻结注册表之前 | 首次调用时懒连：`Freeze` 不允许工具表后变，做不了 |
| 5 | 失败策略 | 每 server 一个 `required` 声明 | 一律降级 + 失败回调 / 诊断接口：多一个 API 面，表达力没多 |
| 6 | 工具名 | `<tool_prefix>__<server>__<tool>`，`tool_prefix` 缺省是 `mcp` | 前缀完全交给用户：server 那一段一去掉，跨 server 的唯一性就从结构保证退化成用户的纪律 |
| 7 | 头配置 | `header_env`：头值写环境变量名，装载时取值；`headers` 同时收字面值 | 只收字面值：凭据跟着配置文件进版本库、进备份，与仓库既有做法（`providers.Options.apiKey` 就写在配置里）一致但更危险 |
| 8 | 错误码 | 81000 段 5 个 | 不新增码，全归 `ErrToolRuntime`：远端故障与本地工具故障混成一类 |
| 9 | 配置字段命名 | 跟生态：`transport` / `url` / `header_env` / `allow_tools` / `tool_prefix` / `enabled`，容器是 `mcp.servers` 数组；这份文件形状住在读配置那侧（`cmd/internal/mcpconfig` 的 `Entry`），与 `pi/mcp` 的运行期 `ServerConfig` 分开两份 | 跟仓库内部（`providers.Options` 的 `protocol`/`baseURL`）：仓库内自洽，但从别的客户端抄来的配置不能直接用；数组而不是 map：名字是显式字段，顺序由配置决定，不随 map 遍历随机化；一份结构兼作两者：`pi/mcp` 就得背上 json 标签与解码规矩 |
| 10 | 远端内容上限 | 成功与失败两条路各过一道 1 MiB 兜底（复用 `pi/tools.LimitText` 的标记） | 不设：远端能塞进任意大的文本；只给 `isError` 设：成功路径同样进上下文，不自洽；按 `read` 的 50 KiB 设：MCP 没有续读手段，截掉补不回来 |
| 11 | client identity | `clientName` 走 `NewExtension` 的第二个参数（空串取 `go-harness`），版本写死成包内常量 | 放 `ServerConfig` 字段：identity 是 client 级的，同一个 harness 对每个 server 报不同名字，远端拿它做统计与兼容分支就没有意义；名字与版本都可配：版本没有来源（`version.txt` 已删），配了也只是把 `dev`/`pro` 这类占位搬进配置文件 |
| 12 | 注册权归谁 | 扩展只产出工具（`Register(ctx) ([]tools.Tool, error)`），登记、owner、整批回滚、收尾归 Runtime | 设想稿的 `API` 回调面（Runtime 把注册面塞给扩展，扩展回调进来登记）：多一层接口与一个只做转发与改 owner 的适配器，而扩展在那一步没有任何决定要下；owner 还得靠适配器固定，改成"只有 Runtime 调 `RegisterFor`"之后是结构上固定的 |

## 目标与非目标

目标：

- 启动期经 `pi/extension` 把远端 MCP 工具接进 `tools.Registry`，模型视角下与内置工具没有区别——同样的 `schema.ToolDefinition`、同样过 `jsonschema` 参数校验、同样走执行链与事件。
- 远端故障可声明：一个 server 连不上，可以是致命错误，也可以只是少一批工具。
- 远端结果尽量不丢信息：文本、图片、结构化结果各归其位。

非目标（明确不做，附理由）：

- **stdio 子进程的 stderr 全量进日志**。传输与进程收尾交给 SDK 的 `CommandTransport`（`Close` 关 stdin、超时后 SIGTERM）；子进程往 stderr 写的东西只在 server 级失败时读尾部几行、贴进那一条错误文案（见「传输与协议」），不做全量收集、不转结构化日志——它可能混着凭据与远端数据，全量要先进一道"哪些能进日志"的规则，是独立的一块。
- **sampling / roots / elicitation / prompts / resources**。我们只做 client 侧的工具消费。SDK 支持这些，但每一个都要往 harness 里接一条反向通道，与本轮目标无关。
- **`notifications/tools/list_changed` 热重载**。工具表启动期冻结（`registry.Freeze()`，`pi/agent.go:111`），通知无处可施。
- **多 server 并发握手**。串行，一个 server 一次。连得上的 server 成本在百毫秒量级，不值得引入并发与它的取消传播；连不上的各自占满自己的 `Timeout`，这道总账由调用方的启动 ctx 兜（见「pi 包根的装配」）。

## 归属：每个名字住在哪

| 名字 | 形态 | 位置 | 为什么在这 |
|---|---|---|---|
| `Registry.RegisterFor` / `Registry.Rollback` | 方法 | `pi/tools/register.go` | 扩展注册与回滚是注册表自己的事，owner 字段已经在这儿了（`register.go:30`） |
| `Extension` | 接口 | `pi/extension/extension.go` | 扩展入口：名字 + 启动期产出工具 |
| `Closer` | 接口 | `pi/extension/extension.go` | 可选停机清理；注册失败的扩展也会收到它 |
| `Runtime` | 结构 | `pi/extension/runtime.go` | 按声明顺序接入、整批登记、失败收尾、逆序关闭 |
| `ServerConfig` / `TransportType` | 结构 / 字符串 | `pi/mcp/config.go`、`pi/mcp/constant.go` | 运行期那份 server 配置与校验；传输常量、超时与工具名前缀的默认值在 `constant.go`。不带 json tag：读文件不在这层 |
| `Entry` / `Duration` / `ServerConfigs` | 结构 / 时长 / 函数 | `cmd/internal/mcpconfig/mcpconfig.go` | 配置文件里那一项的形状（字段名对齐生态）、两种写法的超时、以及转成 `[]mcp.ServerConfig` |
| `NewExtension` | 函数 | `pi/mcp/extension.go` | 配置 → `[]extension.Extension`（一个 server 一个），第二个参数是 client identity |
| 远端工具代理 | 结构（包内） | `pi/mcp/tool.go` | 实现 `tools.Tool`，只由扩展构造 |
| `stderrTail` / `redact` | 结构 / 函数（包内） | `pi/mcp/stderr.go` | stdio 子进程 stderr 的尾部采集与凭据隐去：只由扩展构造、只在 server 级失败时读 |
| `Options.Extensions` | 字段 | `pi/agent.go` | 装配点 |
| `Agent.Close` | 方法 | `pi/agent.go` | 停机清理的对外入口 |

`pi/mcp` 的对外面只有三个名字：`ServerConfig`、`TransportType`（两个常量）、`NewExtension`。代理工具、扩展实现、校验函数全部包内。理由：使用方唯一的注入点是配置，传输由 `ServerConfig.Transport` 选而不是由使用方实现，不导出就不必承诺形状，将来改它们不是破坏性变更。

`ServerConfig` 是运行期那一份，不带 json tag——它描述"怎么连一个 server"，不描述"配置文件长什么样"。文件那一份（字段名、超时怎么写、不认识的字段怎么报）住在 `cmd/internal/mcpconfig`，也就是真读 `config.json` 的地方：那里解出来的是 `Entry`，装配前用 `ServerConfigs` 一对一搬成 `[]mcp.ServerConfig`。两份分开是刻意的：文件形状跟生态里的客户端配置对齐（`transport` / `url` / `header_env`），改动频繁且只对读配置的人有意义；运行期结构是 `pi/mcp` 与装配方之间的契约，手写配置的调用方（测试、别的驱动器）不必被迫经过 json。`pi/mcp` 因此仍然只有一份结构、一条校验路径。

文件夹不新增：`pi/extension/` 与 `pi/mcp/` 已经在仓库里，这轮往里加文件。`pi/mcp` 五个 `.go` 平铺（`config` / `constant` / `extension` / `stderr` / `tool`），不建子目录——不像 `pi/ai/providers` 那种"一个契约多个实现由调用方选"的形态。

## 文件清单（这轮动了哪些）

`pi/extension/extension.go` 的三个契约（`Extension` / `API` / `Closer`）不是这轮凭空定的：提交前 `pi/extension/` 下只有一份未实现的 `README.md`，里面已经写了这三个名字与"扩展只能通过它把工具接入 SDK"这句，代码是照着它落下来的。

`API` 后来撤了：那三个名字是设想稿，而设想稿里没有"注册权归谁"这一条。落下来之后发现扩展拿到的这个面只有一个方法，做的又是把 owner 换成扩展名再转手给 `RegisterFor` 这一件事——于是改成"扩展产出工具、装配方登记"，`API` 与它的适配器一起没了（`extension.go` 现在只有 `Extension` 与 `Closer`）。除了这一份，其余"新增"的都是新文件。

```
go.mod / go.sum            改：加 modelcontextprotocol/go-sdk v1.3.1
config.example.json        改：加一段 mcp.servers 示例

pi/
├── agent.go               改：Options.Extensions、NewAgent 加 ctx、Close、Run 的关闭检查
├── agent_test.go          新增
├── error/
│   └── errors.go          改：81000 段五个码
├── tools/
│   ├── register.go        改：RegisterFor、Rollback
│   ├── register_test.go   新增
│   ├── constant.go        改：toolOutputTruncationMarker → OutputTruncationMarker
│   └── output_test.go     新增：LimitText 的基础用例（这轮它才有调用方）
├── extension/
│   ├── extension.go       新增：Extension / Closer 两个契约
│   ├── runtime.go         新增：Runtime、Register、CloseAll、closeExtension
│   └── runtime_test.go    新增
└── mcp/                   新增包，五个 .go 平铺
    ├── constant.go        TransportType、超时与名字前缀的默认值
    ├── config.go          ServerConfig / invalid / checkToolName / validate / header_env / validateHTTP / validateStdio
    ├── config_test.go
    ├── tool.go            远端 Tool → tools.Tool：代理、内容映射、名字闸
    ├── tool_test.go
    ├── extension.go       NewExtension、Register、Close、newTransport
    ├── extension_test.go
    ├── stderr.go          stderrTail：stdio 子进程 stderr 的尾部采集与凭据隐去
    └── stderr_test.go

cmd/
├── internal/mcpconfig/    新增包：配置文件里 mcp.servers 那一项的形状
│   ├── mcpconfig.go       Entry / Duration / ServerConfigs / 严格解码
│   └── mcpconfig_test.go
├── harness/
│   ├── main.go            改：mcp.servers 配置、mcpServers 转换、NewExtension、NewAgent 的 ctx、Close
│   └── main_test.go       改：既有测试跟着 NewAgent 的 ctx 走
└── mcptest/               新增：读配置连真 server 的驱动
    ├── main.go
    └── testdata/
        └── AGENTS.md      驱动自带的工作区：把身份写成"接入了 MCP 工具的 agent"
```

`cmd/internal/mcpconfig` 放在 `cmd/internal` 下：它只有 `cmd/*` 用（`pi/mcp` 不认它），Go 的 internal 规则正好把这条边界钉死——`pi` 包想 import 也 import 不到。

三条说明：

- `pi/mcp` 的文件与「pi/mcp 的配置」「工具适配」「扩展与失败策略」三节一一对应（`stderr.go` 对应「传输与协议」里那段 stdio），实现与测试同目录同名。
- `cmd/mcptest` 只有 `main.go`：它是驱动，跑起来看的是真 server 回来的真数据，断言写在 `pi/mcp` 的测试里，不另起一份假 server。它有两种跑法，共用一份配置读取：默认把任务交给 `pi.NewAgent`（模型自己挑工具，比如默认那个查天气的任务会让它调 exa 的 `web_search_exa`），`-prompt ""` 则不开模型，只列工具并按示例参数各调一次。
- 不补 `README.md`。`pi/mcp` 与 `pi/extension` 先前的两份是未实现的设想稿，已经删掉；等代码落地后按实际行为写，否则又是一份会和代码脱节的文档。

## pi/tools 的补齐

`register.go:34` 的注释已经承诺了"扩展注册的工具可按 owner 整体 Rollback"，但只有未导出的 `register`（`register.go:93`，本轮拆成 `registerLocked`）和 `Freeze`，没有摘除、也没有对外的注册入口。补两个方法：

```go
// RegisterFor 以 owner 名义整批注册；中途失败则把该 owner 本批已注册的
// 全部摘掉，注册表回到调用前的状态（不影响其他 owner 的工具）。
// 同一个 owner 只准注册一次：重复注册返回 ErrToolAlreadyRegistered。这条
// 前提是"回到调用前"能成立的原因——Rollback 按 owner 整批摘除，owner 复用
// 会把先前那批一起摘掉。冻结不在这里判：写入只有 registerLocked 一处，守卫
// 跟着写入点走，就没有绕过去的旁路。
func (r *Registry) RegisterFor(owner string, items []Tool) error {
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

// Rollback 摘除该 owner 名下的全部工具，返回摘除数量。Freeze 只挡新增注册，
// 不挡摘除：冻结之后仍然允许回滚。
func (r *Registry) Rollback(owner string) int {
	r.mu.Lock()
	defer r.mu.Unlock()

	matched := make([]string, 0, len(r.tools))
	for name, registered := range r.tools {
		if registered.owner == owner {
			matched = append(matched, name)
		}
	}
	for _, name := range matched {
		delete(r.tools, name)
	}

	return len(matched)
}
```

这要顺手把现有的 `register` 拆一刀：`registerLocked(owner, tool) (string, error)` 假定锁已持有、返回登记的名字，供 `RegisterFor` 做整批回滚。

拆完把包级的 `Register` 也并到这条路上，而不是留两层壳：

```go
// Register 建一个新注册表，把内置工具整批登记进去，owner 是 staticToolOwner。
// 它与扩展那条路走同一个入口：整批要么全进要么全退，不必另写一遍逐个注册。
func Register(items []Tool) (*Registry, error) {
	registry := &Registry{tools: make(map[string]entry, len(items))}
	if err := registry.RegisterFor(staticToolOwner, items); err != nil {
		return nil, err
	}

	return registry, nil
}
```

原来的 `register` 就此删掉：它只做"持锁 + 转手 `registerLocked`"，而 `RegisterFor` 是它唯一的调用方。留着的代价是冻结判两遍——先前 `RegisterFor` 入口判一次、`registerLocked` 里再判一次，两处守卫迟早会分叉。现在只剩 `registerLocked` 一处，写入只有这一个口子，冻结也就守得住。

还要顺手导出截断标记（`constant.go:4`），给 `pi/mcp` 的错误路径共用——它自己截文本，得跟 `LimitText` 用同一个标记，理由见「工具适配」那条上限：

```go
// constant.go
// OutputTruncationMarker 是文本被截断时追加的标记。LimitText 用它，pi/mcp 的
// 错误路径也用（那条路返回 error，过不了 LimitText）。
const OutputTruncationMarker = "\n[output truncated]"
```

两个都要用：整批原子是扩展想要的语义（半批工具注册进去比没有更糟——模型会看到一半能力的 server）；`Rollback` 是 Runtime 在某个扩展失败后清场的唯一手段。

owner 用精确匹配，不做层级前缀推导。Runtime 记账它实际见过的 owner 字符串，不去猜命名约定。"owner 单次使用"这条前提由 Runtime 保证——构造期已经拒绝重名扩展（见下），所以 `pi/mcp` 不必自己记账。

## pi/extension 契约

```go
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

契约里没有"注册面"这个东西，这是这版跟设想稿最大的差别。设想稿（提交前的 `README.md`）写的是"扩展只能通过它把工具接入 SDK"，落下来先照做成了 `API`，但那个面只有一个方法，做的又是把 owner 换成扩展名再转手给 `RegisterFor`——扩展在这中间没有任何决定要下。改成扩展产出、装配方登记之后，owner 这件事不用再靠适配器固定：`RegisterFor` 只有 Runtime 一个调用方，扩展根本够不着 owner 这个参数。

依赖方向：`pi/extension` 只依赖 `pi/tools`。它不依赖 `pi/ai`——工具类型在 `pi/tools` 与 `pi/schema`（`pi/tools/interface.go:11-14`），这轮没有任何东西要求它碰模型层。

`runtime.go`：

```go
// Runtime 持有本轮接入的扩展，管接入与停机清理。它不存业务状态：某个扩展
// 跳过了什么、为什么跳，是扩展自己的事（见下）。
type Runtime struct {
	extensions []Extension // 调用方给的顺序就是接入顺序，也是逆序收尾的顺序
	started    int         // 接入成功的个数，也就是已启动的那批前缀

	closeOnce sync.Once // 停机只真关一次：Close 可能被重复调用
	closeErr  error     // 第一次 CloseAll 的结果，重复调用原样返回
}

// NewRuntime 校验扩展并按调用方给的顺序收好。空列表也返回可用的 Runtime，调用方
// 不必判 nil。不排序——顺序是调用方声明的，接进来与收尾都照它走。
func NewRuntime(extensions []Extension) (*Runtime, error) {
	ordered := make([]Extension, 0, len(extensions))
	seen := make(map[string]struct{}, len(extensions))
	for _, item := range extensions {
		if isNilExtension(item) {
			return nil, pierrors.ErrInitialization.Wrap(
				errors.New("extension must not be nil"))
		}
		name := strings.TrimSpace(item.Name())
		if name == "" {
			return nil, pierrors.ErrInitialization.Wrap(
				errors.New("extension name must not be empty"))
		}
		if _, exists := seen[name]; exists {
			return nil, pierrors.ErrInitialization.Wrap(
				fmt.Errorf("duplicate extension name %q", name))
		}
		seen[name] = struct{}{}
		ordered = append(ordered, item)
	}

	return &Runtime{extensions: ordered}, nil
}

// isNilExtension 报告扩展接口是否为空或装有一个类型化 nil 值——类型化 nil
// 不挡的话，后面每个调用点都得再判一次。
func isNilExtension(item Extension) bool {
	if item == nil {
		return true
	}
	value := reflect.ValueOf(item)
	switch value.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Map,
		reflect.Slice, reflect.Chan, reflect.Func:
		return value.IsNil()
	default:
		return false
	}
}

// Register 按声明顺序逐个接入，ctx 原样传给每个扩展。每个扩展两步：先要它产出
// 工具，再以它的名字为 owner 整批登记。任一步失败即收尾：关掉这个扩展（它可能
// 已经占下连接与子进程）与已启动的那批，错误 errors.Join 后一起返回。登记失败
// 不用单独回滚——RegisterFor 是整批原子的，失败时它自己已经把这一批摘干净了。
func (r *Runtime) Register(ctx context.Context, registry *tools.Registry) error {
	for _, item := range r.extensions {
		items, err := item.Register(ctx)
		if err == nil {
			err = registry.RegisterFor(item.Name(), items)
		}
		if err != nil {
			return errors.Join(err, closeExtension(ctx, item), r.closeStarted(ctx))
		}
		r.started++
	}

	return nil
}

// closeExtension 关掉刚失败的那个扩展。它不在"已启动"那批里（started 只记成功
// 的），而它的 Register 可能已经连上了远端、起了子进程，漏掉就是连接与后台
// goroutine 的泄漏。没实现 Closer 的扩展无事可做。
func closeExtension(ctx context.Context, item Extension) error {
	closer, ok := item.(Closer)
	if !ok {
		return nil
	}

	return closer.Close(ctx)
}

// closeStarted 逆序关闭前 started 个里实现了 Closer 的扩展，并把 started
// 归零——关过的不再关第二次。装配失败与停机收尾共用它。
func (r *Runtime) closeStarted(ctx context.Context) error {
	var closeErr error
	for index := r.started - 1; index >= 0; index-- {
		closer, ok := r.extensions[index].(Closer)
		if !ok {
			continue
		}
		closeErr = errors.Join(closeErr, closer.Close(ctx))
	}
	r.started = 0

	return closeErr
}

// CloseAll 逆序关闭前 started 个里实现了 Closer 的扩展，错误 errors.Join；
// 只真关一次，重复调用返回第一次的结果，关完 started 归零。
func (r *Runtime) CloseAll(ctx context.Context) error {
	r.closeOnce.Do(func() {
		r.closeErr = r.closeStarted(ctx)
	})

	return r.closeErr
}
```

三个字段各有理由：

- `extensions` 按调用方给的顺序收着：接入顺序就是配置里的书写顺序，人也看得出"哪个 server 先接"；逆序收尾因此是"后接的先关"。
- `started` 而不是第二个切片：接入顺序就是 `extensions` 的顺序，所以"已启动的那批"永远是它的前缀，一个下标就够，不必再记一份名单。`Register` 中途失败时关掉前缀里的扩展，`started` 归零，之后谁来 `CloseAll` 都是空转。
- `closeOnce` / `closeErr`：唯一可能的并发在停机侧——`Agent.Close` 可能被重复调用，也可能与在途的 `Run` 并发。`started` 在装配期写完之后只读，装配与停机之间的先后由 `NewAgent` 的返回来建立，不需要额外的锁。

构造期校验：不为 nil、名字去空格后非空、不重名。名字只用来判重与当 owner，不参与排序——排过一次，后来撤了：声明顺序是调用方的东西，按名字重排会让人对着配置找不到"为什么这个 server 先连"。

**失败粒度到扩展，不往下传。** 某个扩展内部如何容忍自己的部分失败，是它自己的事——`pi/mcp` 知道 `required` 这个字段，Runtime 不需要知道。这一条让 `ExtensionError` / `Agent.Failures()` 这类类型不必存在，也让 `pi/extension` 不必引日志依赖：降级的那条 Warn 由扩展自己记（`pi/mcp` 知道为什么要跳、跳过了谁），Runtime 只负责把错误往上抛。

同理，`Register` 失败时不跨扩展全局回滚：那一批由 `RegisterFor` 自己整批退干净，装配失败会让 `newAgent` 直接返回 `nil, err`，注册表当场被丢弃，没有观察者能看到那半批工具；真正必须做的是关闭扩展——失败的那个与已启动的那些都已经占了连接与后台会话，那才是带副作用的部分。

## pi 包根的装配

`pi/agent.go`：

```go
type Options struct {
	// ...现有字段
	// Extensions 是启动期接入的扩展，按给的顺序接入，停机时逆序关闭。
	Extensions []extension.Extension
}
```

装配顺序（`newAgent` 内，`pi/agent.go:105-111` 之间）：

```go
registry, err := tools.Register(opts.Tools)
if err != nil {
	return nil, err
}

runtime, err := extension.NewRuntime(opts.Extensions)
if err != nil {
	return nil, err
}
if err := runtime.Register(ctx, registry); err != nil {
	return nil, err
}
registry.Freeze()

// 紧接着的 `agent := &Agent{...}` 里把这个 runtime 放进去（runtime: runtime），
// Close 才有东西可关。
```

`NewAgent` / `newAgent` 要加一个 ctx 参数（`func NewAgent(ctx context.Context, opts *Options) (*Agent, error)`）：`Client.Connect` 必须拿到 ctx，而装配期本来没有 ctx 可用。这是本轮唯一的破坏性签名变更，三个调用点（`cmd/harness`、`cmd/sessiontest`、`cmd/skilltest`）与测试跟着改。加了之后启动期的总耗时上限由调用方给——`cmd/harness` 拿启动 ctx 套一层 `context.WithTimeout`，一个连不上的 server 不会把启动拖成 N × Timeout。

`Agent` 增加两个字段与方法：

```go
type Agent struct {
	// ...现有字段
	runtime *extension.Runtime // 启动期接入的扩展，Close 时逆序关
	closed  atomic.Bool        // 关闭标记，Run 入口查一次
}

// Close 关闭全部扩展并标记 Agent 已关闭。幂等：真正关一次，重复调用返回第一次
// 的结果；多个扩展的关闭错误用 errors.Join 聚合。语义上不等正在跑的 Run、也不
// 取消它——Run 归调用方的 ctx 管，Close 只做标记与关扩展。已经发出去的那次 MCP
// 调用会照常跑完（见下），其后同一轮里新起的调用会失败（事件 IsError），Run
// 本身照常返回。
func (a *Agent) Close(ctx context.Context) error {
	// 先标记、再关扩展：反过来的话，新 Run 会溜进一个正在关闭的会话，拿到的是
	// 莫名其妙的调用失败，而不是 ErrClosed。
	a.closed.Store(true)

	return a.runtime.CloseAll(ctx)
}
```

`Run` 的入口因此多一行检查。只查这一处：一轮里扩展被关掉的表现已经是工具调用失败，轮次中间反复检查不增加信息（`pi/agent.go:146`）：

```go
func (a *Agent) Run(ctx context.Context, input *RunInput) (*RunOutput, error) {
	if a.closed.Load() {
		return nil, pierrors.ErrClosed
	}
	if input == nil {
		return nil, pierrors.ErrRequestInvalid.Wrap(errors.New("run input must not be nil"))
	}
	// ...其余不变
```

字段存 `*extension.Runtime` 而不是 `[]extension.Closer`：关闭顺序（后注册的先关）与"只关一次"都在 Runtime 里，Agent 只转发，不自己记账。装配片段里那个局部变量 `runtime` 随之进 `&Agent{...}` 字面量。空列表也返回可用的 Runtime，所以 `Close` 不必判 nil。

关闭标记就是唯一的同步点，顺序是先标记、再关扩展：`Close` 先把 `closed` 置真，然后调 `Runtime.CloseAll`。反过来的顺序会让新 `Run` 溜进一个正在关闭的会话，拿到的是莫名其妙的调用失败而不是 `ErrClosed`；先标记则晚到的 `Run` 在入口就被拦下，已经过了检查的 `Run` 继续跑。用 `Store` 就够，不需要 CAS——谁先谁后不影响结果，"只关一次"由 Runtime 的 `closeOnce` 保证，重复调用收敛到同一个错误值。也不需要 `open → closing → closed` 三态机：`Close` 不等着 `Run` 结束、也不取消它（它等的是会话，见下一条），没有第三个状态要表达。测试用 `go test -race` 跑 `Run` 与 `Close` 并发。

**`Close` 会等一等，这是核 SDK 时订正的一条。** `ClientSession.Close` 的注释写明它"preventing new requests from being handled, and waiting for ongoing requests to return"（`mcp/client.go:309-313`），而且它不接 ctx。所以 `Agent.Close` 最坏会阻塞到在途的 `tools/call` 自己超时为止——上限就是我们给它套的 `config.Timeout`，不会无限等。两个推论：`cmd/harness` 那个 `closeTimeout` ctx 只能约束我们自己的循环，管不到 SDK 这一层；"关掉 Agent 会让在途调用立刻失败"是错的，按注释它会正常跑完。造这个场景的测试要让假 server 拖住响应，断言的是"在途那次照常返回 + 之后的调用失败"，不是"调用被中断"。

`ErrClosed` 已存在（`pi/error/errors.go:112`，60000），目前零引用——这轮它拿到第一个使用者。

顺序上有一条硬约束：注册必须在 `Freeze` 之前，而 `Freeze` 必须在本轮第一次 `Run` 之前。`loop.definitions()` 每轮从 Scheduler 现读（`pi/loop.go:145-148`），工具表冻结后不再变，所以 MCP 的 `tools/list` 只能在启动期做。

## pi/mcp 的配置

`pi/mcp` 认的是一份运行期配置：把要接的 server 收拾成一个 `ServerConfig` 交给 `NewExtension`。它不带 json 标签，也不认识"配置文件"这件事——文件长什么样、从哪读，是读配置那一侧的事（见「文件形状与读法」一节），读出来转成这一份。

```go
type TransportType string

const (
	TransportHTTP  TransportType = "http"  // Streamable HTTP
	TransportStdio TransportType = "stdio" // 子进程，走 stdin/stdout
)

// defaultTimeout 是 Timeout 缺省时的单次远端操作超时。
const defaultTimeout = 30 * time.Second

// defaultToolPrefix 是本地工具名的第一段：mcp__<server>__<远端名>。
const defaultToolPrefix = "mcp"

// ServerConfig 是一个 MCP server 的运行期接入配置。
type ServerConfig struct {
	// Name 是本地标识，用作工具名前缀与 owner。必须匹配 ^[a-z0-9][a-z0-9_-]*$：
	// 它会进工具名，而工具名要经得起两家 Provider 的口径。
	Name string
	// Enabled 为假时这个 server 完全不接：不连、不注册。缺省视为启用。
	Enabled *bool
	// Transport 是传输类型，必填判别字段：http 或 stdio。
	Transport TransportType
	// URL 是 Streamable HTTP 的单一入口。仅 http 用。
	URL string
	// Headers 是每个请求附带的固定头。仅 http 用。
	Headers map[string]string
	// HeaderEnv 也是请求头，只是值写成环境变量的名字：{"X-Api-Key":
	// "EXA_API_KEY"} 表示这个头的值从环境里取。凭据不进配置文件，也就不进
	// 版本库。仅 http 用。
	HeaderEnv map[string]string
	// Command/Args/Env/Cwd 是 stdio 的启动参数。仅 stdio 用。
	Command string
	Args    []string
	Env     map[string]string
	Cwd     string
	// Required 为真时，这个 server 连接或工具发现失败会让 Agent 装配失败；
	// 为假（默认）时只记一条 Warn，其余 server 与 Agent 照常起。
	Required bool
	// AllowTools 非空时只接入列出的远端工具；空表示接入全部。
	AllowTools []string
	// ToolPrefix 换掉本地工具名的第一段（默认 mcp）：本地名形如
	// <tool_prefix>__<server>__<远端名>，server 那一段始终在，多个 server 的
	// 同名工具不会撞在一起。空表示用默认值。
	ToolPrefix string
	// Timeout 是单次远端操作的超时：一次连接、一次工具发现、一次工具调用各
	// 算一次。0 表示取默认值。
	Timeout time.Duration
}

// IsEnabled 报告这个 server 要不要接：缺省是接。装配时按它跳过停用的条目；
// 装配方想按同一条规则显示"哪些没接"，也用它，不在外面重写一遍。
func (c *ServerConfig) IsEnabled() bool {
	return c.Enabled == nil || *c.Enabled
}
```

两处形状是刻意的。`Timeout` 用 `time.Duration` 而不是自定义的时长类型：两种写法（数字按秒、字符串走 `time.ParseDuration`）是**文件格式**的事，跟运行期无关，自定义类型留在读文件那一侧，这边只剩一个数。`IsEnabled` 导出，是因为"跳不跳停用的 server"这条规则有两个使用方——`NewExtension` 与驱动的打印——写在两处迟早会散。

这份结构只有一条校验路径：`NewExtension` 里的 `validate`。文件那一侧只搬值，不判名字合不合规、`transport` 认不认得、`header_env` 缺没缺——否则手写配置的调用方（测试、别的驱动器）与读文件的调用方就会走出两套规矩。

容器是一个数组，每项自己带 `name`：

```json
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
      "timeout": 30
    }
  ]
}
```

数组而不是 map：名字是显式字段（不再靠 key 兼职），顺序由配置决定，多个 server 时不会随 map 的遍历顺序随机化。`pi/mcp` 只认 `[]ServerConfig`，**怎么读配置文件不在这层**——`cmd/harness` 与 `cmd/mcptest` 各自读自己那份文件（`harnessConfig.MCP.Servers` / `config.MCP.Servers`），跟读平台配置是同一件事。文件里那些字段名对应到 `ServerConfig` 的哪个字段，由 `cmd/internal/mcpconfig` 的 `Entry` 定（见下一节）。

判别分支（`ServerConfig.validate` 里 switch；它是未导出的，`NewExtension` 是唯一入口，所以对外名字不因此多一个）：

| `transport` | 行为 |
|---|---|
| `"http"` | `validateHTTP`：url 形状、被控头、不许出现 stdio 那几个字段 |
| `"stdio"` | `validateStdio`：必须有 command、不许出现 url/headers/header_env |
| 其他 / 空 | `ErrMCPTransportUnsupported`（81001），文案列出 `http, stdio` |

两种传输的字段是互斥的，串了行就报错：`transport` 写 http 却给了 `command`，或者写 stdio 却给了 `url`，都是抄配置时常见的错法，早报比连不上再猜省事。

其余校验与判别一起写在 `validate` 里，每条判定占一行——错误统一由 `invalid` 造：

```go
var serverNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// blockedHeaders 是传输自己在管的头，用户覆盖会把协议搞坏。比对前两侧都过
// http.CanonicalHeaderKey，否则写 host 或 HOST 就绕过去了。
var blockedHeaders = map[string]struct{}{
	"Host": {}, "Content-Length": {}, "Mcp-Session-Id": {},
	"Content-Type": {}, "Accept": {},
}

// invalid 造一个配置类错误：判定有十来条，各自铺一遍 Wrap 与 fmt.Errorf 会
// 把这页撑成两页。
func (c *ServerConfig) invalid(format string, args ...any) error {
	return pierrors.ErrMCPConfigInvalid.Wrap(fmt.Errorf(format, args...))
}

// checkToolName 校验会进工具名的那几个字段。宽松一点就会在远端以难查的方式失败。
func (c *ServerConfig) checkToolName(what, value string) error {
	if serverNamePattern.MatchString(value) {
		return nil
	}

	return c.invalid("server %q 的 %s %q 必须匹配 ^[a-z0-9][a-z0-9_-]*$（它会进工具名）",
		c.Name, what, value)
}

// validate 校验并补齐配置：它会把缺省的 Timeout 填成默认值、把 header_env 换成
// 真的头值，所以收指针。
func (c *ServerConfig) validate() error {
	if err := c.checkToolName("名字", c.Name); err != nil {
		return err
	}
	if c.ToolPrefix != "" {
		if err := c.checkToolName("tool_prefix", c.ToolPrefix); err != nil {
			return err
		}
	}
	// 非正数一律当"没配"取默认值：显式写的非正数在读文件那侧已经被拒了，走到
	// 这里的多半是手写 ServerConfig 的调用方。
	if c.Timeout <= 0 {
		c.Timeout = defaultTimeout
	}
	if err := c.resolveHeaderEnv(); err != nil {
		return err
	}

	switch c.Transport {
	case TransportHTTP:
		return c.validateHTTP()
	case TransportStdio:
		return c.validateStdio()
	default:
		return pierrors.ErrMCPTransportUnsupported.Wrap(fmt.Errorf(
			"server %q transport %q 不支持，可选值: %s, %s",
			c.Name, c.Transport, TransportHTTP, TransportStdio))
	}
}

// resolveHeaderEnv 把 header_env 里的环境变量名换成值并进 Headers。环境变量没
// 设置时报错并点出变量名：凭据没装上，连接与每次调用都会失败，早说比晚说省事。
// 值本身不进错误文案也不进日志，这里只说变量名。
func (c *ServerConfig) resolveHeaderEnv() error {
	// 按变量名排序：缺好几个时错误文案是稳定的。
	for _, name := range slices.Sorted(maps.Keys(c.HeaderEnv)) {
		envName := c.HeaderEnv[name]
		value, exists := os.LookupEnv(envName)
		if !exists || value == "" {
			return c.invalid("server %q header %q 要的环境变量 %s 没有设置",
				c.Name, name, envName)
		}
		if c.Headers == nil {
			c.Headers = make(map[string]string, len(c.HeaderEnv))
		}
		c.Headers[name] = value
	}

	return nil
}

func (c *ServerConfig) validateHTTP() error {
	if c.Command != "" || len(c.Args) > 0 || len(c.Env) > 0 || c.Cwd != "" {
		return c.invalid("server %q transport 是 http，command/args/env/cwd 只给 stdio 用",
			c.Name)
	}

	parsed, err := url.Parse(c.URL)
	if err != nil {
		return c.invalid("server %q url %q: %w", c.Name, c.URL, err)
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return c.invalid("server %q url %q 必须是 http 或 https 且带 host", c.Name, c.URL)
	}
	// userinfo 与 query 里常放 token，而 endpoint 会进错误文案与日志（见方案
	// 「凭据」那条）：凭据一律走 headers，URL 保持干净。
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return c.invalid("server %q url 不能带 userinfo、query 或 fragment", c.Name)
	}
	for name := range c.Headers {
		if _, blocked := blockedHeaders[http.CanonicalHeaderKey(name)]; blocked {
			return c.invalid("server %q header %q 由传输自己管理，不能覆盖", c.Name, name)
		}
	}

	return nil
}

func (c *ServerConfig) validateStdio() error {
	if c.URL != "" || len(c.Headers) > 0 || len(c.HeaderEnv) > 0 {
		return c.invalid("server %q transport 是 stdio，url/headers/header_env 只给 http 用",
			c.Name)
	}
	if strings.TrimSpace(c.Command) == "" {
		return c.invalid("server %q transport 是 stdio，必须给 command", c.Name)
	}

	return nil
}
```

两条判定被合并掉，各留一条：

- `timeout` 只有"非正数取默认值"这一条，不再单独报"必须为正"。显式写非正数的错在读文件那侧已经报过（`cmd/internal/mcpconfig` 的 `Duration.set`），走到 `validate` 的负值只能来自手写的 `ServerConfig`，按"没配"处理比多一条判定省事。
- `allow_tools` 不再清洗：名字按原样用，不 trim 也不去重。写错了会在装配时报「白名单里的工具远端没有」并点出那个名字（多出来的空格也跟着打出来），比在 `validate` 里另设一条"空名字"判定少一条路。

配置类错误一律致命，与 `required` 无关：`required` 管的是远端可达性与工具发现这类运行期的事，配置写错是本地的事，两者混在一个开关下会让"配置错在哪"变得难查。

**`enabled: false` 的条目不校验。** 停用是个明确的选择（Key 还没拿到、远端在维护），不该因为它而让整套装配失败——`NewExtension` 直接跳过它，不连接、不注册。缺省（不写这个字段）是启用。

`header_env` 的键是头名、值是环境变量名，装配时（`validate` 里的 `resolveHeaderEnv`）取值填进 `Headers`，之后两者走同一条路：被控头（`Host` / `Content-Length` / `Mcp-Session-Id` / `Content-Type` / `Accept`，大小写归一后比）一律拒收，`host` 或 `HOST` 都别想绕过去。凭据从环境来、不从配置文件来，是这条设计里唯一一处与仓库既有做法（`providers.Options.apiKey` 直接写在配置里）不同、而且是刻意不同的地方。

### 文件形状与读法：`cmd/internal/mcpconfig`

字段名跟生态里的客户端配置对齐（`transport` / `url` / `header_env` / `allow_tools` / `tool_prefix`），抄来的配置不至于因为字段名对不上而静默失效。这份形状住在真读 `config.json` 的地方：

```go
// cmd/internal/mcpconfig/mcpconfig.go

// Entry 是配置文件里 mcp.servers 的一项。只负责把文件读进来：名字合不合规、
// transport 认不认得、header_env 缺没缺，都由 pi/mcp 装配时判定。
type Entry struct {
	Name       string            `json:"name"`
	Enabled    *bool             `json:"enabled,omitempty"`
	Transport  string            `json:"transport"`
	URL        string            `json:"url,omitempty"`
	Headers    map[string]string `json:"headers,omitempty"`
	HeaderEnv  map[string]string `json:"header_env,omitempty"`
	Command    string            `json:"command,omitempty"`
	Args       []string          `json:"args,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	Cwd        string            `json:"cwd,omitempty"`
	Required   bool              `json:"required,omitempty"`
	AllowTools []string          `json:"allow_tools,omitempty"`
	ToolPrefix string            `json:"tool_prefix,omitempty"`
	Timeout    Duration          `json:"timeout,omitempty"`
}

// ServerConfigs 把读进来的一项项转成运行期结构。只搬值，不做校验。
func ServerConfigs(entries []Entry) []mcp.ServerConfig { /* 逐个字段搬 */ }
```

`Transport` 在这边是 `string` 而不是 `TransportType`：文件里写什么就带过去，认不认得由 `pi/mcp` 的 `validate` 判（写 `sse` 报 81001、文案列出可选值），判断只有一处。

**不认识的字段直接报错。** 配置多半是从别的客户端抄来的，`encoding/json` 默认把不认识的键静默丢掉——`header_env` 一旦抄成 `headers`，丢掉的就是凭据：server 照常起来、连接也成功，只是每个请求都不带 Key，远端回一句 401，人还得从头猜。`Entry.UnmarshalJSON` 用 `DisallowUnknownFields` 把这类错误挡在装载这一步，错误文案里列出认得的字段名：

```go
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
```

`timeout` 的两种写法也归这边（`Duration` 与它的 `UnmarshalJSON`：数字按秒、字符串走 `time.ParseDuration`）。不写、`null`、空串都留成 0，交给 `pi/mcp` 补默认 30s；写了非正数当场报 81000——"显式写了 0"与"没写"是两件事，不能悄悄当成一样。

`ServerConfigs` 只搬值，坏值原样带过去让 `pi/mcp` 报。这不是洁癖：手写 `ServerConfig` 的调用方与读文件的调用方走的是同一条 `validate`，规矩只有一份，报错只有一处。转换出来的切片就是打印、分组、装配共用的那一份——停用与否、入口怎么显示也只有一条规则。

`cmd/internal` 这个位置是有意的：它只给 `cmd/*` 用，Go 的 internal 规则让 `pi` 包想 import 都 import 不到，文件形状就钉在读配置的那一侧。

配置来源是 `cmd/harness` 的 `harnessConfig`（`cmd/harness/main.go:26`），`pi` 根包不认识 mcp 配置类型，只认识 `extension.Extension`：

```go
// cmd/harness/main.go

type harnessConfig struct {
	CurrentPlatform string               `json:"currentPlatform"`
	Platforms       []*providers.Options `json:"platforms"`
	// MCP 是 MCP server 配置，servers 里一项一个 server。文件形状由
	// cmd/internal/mcpconfig 定（字段名对齐生态里的客户端配置），装配时用
	// mcpServers 转成 pi/mcp 认的运行期结构。
	MCP struct {
		Servers []mcpconfig.Entry `json:"servers"`
	} `json:"mcp"`
}

// mcpServers 把配置里读进来的 server 转成 pi/mcp 认的那一份。
func (cfg *harnessConfig) mcpServers() []mcp.ServerConfig {
	return mcpconfig.ServerConfigs(cfg.MCP.Servers)
}
```

读文件、把这一段解出来是 `cmd/harness` 自己的事（`loadConfig`，与平台配置同一处）；`mcp.NewExtension` 只收切片。`cmd/mcptest` 用同一个包读同一段配置（`config.servers()` 一个方法，与 `mcpServers()` 同一形状）——它走 agent 时还要 `currentPlatform` 与 `platforms` 两段，读法与挑法都照 `cmd/harness` 那份来。两边都不往 `pi/mcp` 里塞文件路径。

`main` 里的装配与收尾：

```go
	// 启动期另给一个上限，-startupTimeout 默认 60s：它是这几个 server 加起来
	// 的总预算，单个 server 自己的 timeout 在它之下，两者取先到的那个。
	startupCtx, cancelStartup := context.WithTimeout(context.Background(), *startupTimeout)
	defer cancelStartup()

	extensions, err := mcp.NewExtension(cfg.mcpServers())
	if err != nil {
		fail(err)
	}

	agent, err := pi.NewAgent(startupCtx, &pi.Options{
		// ...现有字段
		Extensions: extensions,
	})
	if err != nil {
		fail(err)
	}
	// 装配结束就把启动期预算放掉，别把它留给运行期。
	cancelStartup()

	// 停机期另起一个 ctx：运行期那个此时可能已经超时或被取消，拿它关会话，
	// 关闭请求本身会立刻失败。
	defer func() {
		closeCtx, cancelClose := context.WithTimeout(context.Background(), closeTimeout)
		defer cancelClose()
		if err := agent.Close(closeCtx); err != nil {
			// 不改退出码：已经跑完那一轮的结果比关闭失败重要。
			fmt.Fprintf(os.Stderr, "关闭扩展失败: %v\n", err)
		}
	}()
```

`closeTimeout` 是常量（5s），不另开 flag。有一条要知道的：`fail` 走的是 `os.Exit(1)`（`cmd/harness/main.go:257`），`os.Exit` 不跑 defer，所以出错退出时这个 Close 不会执行。这不影响正确性——进程一退，连接与后台 goroutine 都随它结束——但哪天 harness 要在一个进程里跑多轮，这条 defer 得改成显式调用。

## 传输与协议：交给官方 SDK

用 `github.com/modelcontextprotocol/go-sdk/mcp`（本机 module cache 里有 v1.3.1），`pi/mcp` 只写装配。

三条理由：

1. **本仓库既有做法就是这样。** 线协议一律交给官方 SDK，自己只留契约：`pi/ai/providers` 用 `openai-go/v3` 与 `anthropic-sdk-go` 包住，对外只出 `ai.Provider`（见 `go.mod:5-13` 与 `pi/ai/providers/options.go:61`）。MCP 是同一类东西——外部线协议，不是 harness 的内部机理。
2. **SDK 把我们打算手写的东西做完，而且比手写多。** `Client.Connect` 自己完成 `initialize` 握手与版本协商（`mcp/client.go:228-248`）；`ClientSession.Tools` 是自动翻页的迭代器（`mcp/client.go:1013`）；`StreamableClientTransport` 带重连与可选的 standalone SSE 长连接（`mcp/streamable.go:1401-1430`）；`CommandTransport` 就是 stdio（`mcp/cmd.go:20-45`）。
3. **stdio 从"将来加一个实现"变成"换一个 transport 值"。** 决策 2 留的那道缝，SDK 那侧已经存在。

代价两条，都写在这儿：新增一个依赖；SDK 的会话是长期对象（握手后维持 session，可能挂着后台 SSE），要映射到我们"启动期建、停机期关"的模型上——`Closer` 就是干这个的。

一个前提要你先定：**如果专栏打算写"自己实现 MCP 协议"这个题材，这条要反过来**——手写 JSON-RPC 与 SSE 分帧，约 700 行，SDK 只当参照。这是取材问题，不是技术问题，我按"用 SDK"往下写。

装配：

```go
// newTransport 装配两种传输。transport 的判别归 validate：它已经把取值收敛到
// http 与 stdio，走到这里 default 那一支只兜底。stderr 只给 stdio 用，是子进程
// 的收尾线索（见下面那段）。
func newTransport(config ServerConfig, stderr io.Writer) (mcp.Transport, error) {
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

		return &mcp.CommandTransport{Command: command}, nil
	}

	return &mcp.StreamableClientTransport{
		Endpoint: config.URL,
		// 不设 http.Client.Timeout，理由见下。
		HTTPClient: &http.Client{Transport: headerRoundTripper{
			base:    http.DefaultTransport,
			headers: config.Headers,
		}},
	}, nil
}

// headerRoundTripper 把配置里的固定头加在 SDK 发出的每个请求上——SDK 只收
// *http.Client，没有别的注入口。
type headerRoundTripper struct {
	base    http.RoundTripper
	headers map[string]string
}

func (t headerRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	// Clone 会连 Header 一起深拷，改副本不动调用方那一份。
	cloned := request.Clone(request.Context())
	for name, value := range t.headers {
		// Set 而不是 Add：配置里写的值是"这个头的全部内容"，追加会变成
		// 用户以为在覆盖、实际拼出重复头。
		cloned.Header.Set(name, value)
	}
	return t.base.RoundTrip(cloned)
}
```

用户头不可能盖到 SDK 自己的头（`Host` / `Content-Length` / `Mcp-Session-Id` / `Content-Type` / `Accept`）：`validate` 已经把这几个拒了，所以这里不必再挡一次。

**stdio 的进程收尾交给 SDK。** `CommandTransport` 的 `Connect` 起进程、把 `stdout`/`stdin` 接成双向通道，`Close` 先关 stdin 等进程退出、超时（`TerminateDuration`，缺省 5s）后 SIGTERM（`mcp/cmd.go:20-90`）。我们不自己 `Wait`、不自己杀进程：会话的关与扩展的 `Close` 是同一条路径，跟 http 那条完全一致。env 给全不给全会影响子进程能不能起来，所以配了 `env` 就按"父进程环境 + 覆盖"给（`environ`，按变量名排序，同名以后出现的为准），没配就给 nil 让它继承。

**子进程的 stderr 只在失败时读尾巴。** stdio server 起不来时（包没装、Node 崩了、版本不对），协议层只看到 `calling "initialize": EOF`，能说明原因的东西全在子进程的 stderr 里——而 `CommandTransport` 只管 stdout/stdin：`Connect` 只做 `StdoutPipe` / `StdinPipe` 与 `Start`（`mcp/cmd.go:29-47`），压根不碰 `Stderr`；`Stderr` 为 nil 时 Go 的 `os/exec` 把子进程 stderr 送进空设备。所以我们能下的手就一处：`newTransport` 把 `command.Stderr` 接到 `stderrTail`（`pi/mcp/extension.go:248`，`pi/mcp/stderr.go`）。那个文件的注释写着三条取舍——只留尾部（最后 4 KiB，写满就丢前面的，不阻断子进程）、只在 server 级失败（连接、工具发现）的出口读、读的时候按已知凭据替换。凭据表在装配时定一次，取的是 `Headers` 的值与 `env` 的值：这两个正是我们交给子进程、它最可能打回 stderr 的东西。短于 8 个字符的值不参与替换（`1`、`dev`、PATH 片段这类换掉了只会把 stderr 切得读不懂）；替换是纯字符串相等替换，不猜格式、不做正则——认得的就隐掉，不假装能认出别的。读出来的东西只进那一条错误文案与 Warn 的 `err` 字段（`skipOrFail`，见「扩展与失败策略」）：健康的 server 也爱往 stderr 写 npm 警告，那不该进日志。

**`cwd` 相对的是进程目录，不是 `-workdir`。** stdio server 的 `cwd` 直接给 `exec.Cmd.Dir`（`pi/mcp/extension.go:247`），所以它按驱动进程自己的工作目录解析——与 `-sessions` 同一条口径。想让它跟着工作区走就写相对路径（示例里的 `.` 落在进程目录），别以为它会跟着 agent 的 `-workdir` 走。

实测（2026-09-24，`npx -y @modelcontextprotocol/server-filesystem`，把 fs 那条开成 `enabled: true`）：

```
go run ./cmd/mcptest -prompt ""     # 不开模型：列工具，再按示例表各调一次
go run ./cmd/mcptest                # agent 路：模型自己挑 mcp__fs__* 工具
```

第一条列出 14 个远端工具，`list_directory` 回来的是真目录数据；第二条模型确实调了 `mcp__fs__*` 的工具；两条跑完都没有残留子进程。把 `command` 换成一个不存在的包名，错误文案从光秃秃的 `EOF` 变成 npx 的 404 全文——这就是上面那块要解决的问题。`npx` 冷启动（要下包）算在 `config.Timeout` 里，所以示例给 60 秒而不是 30。

**超时统一走 ctx，`http.Client` 不设 `Timeout`。** 设了就是两套账：`http.Client.Timeout` 按每次尝试算，而 SDK 自己的重试会绕着它从头再来一次，谁也说不清哪个先到。所有超时都写成 ctx——启动期的 `Connect` 与 `tools/list` 套 `config.Timeout`，调用期的 `tools/call` 同样（见「工具适配」），外层再是启动 ctx 或调用方的 ctx。ctx 嵌套天然取先到的那个，不需要额外的优先级规则。粒度按操作算：一次 `Connect` 一个 `config.Timeout`，一次工具发现一个（整轮 `session.Tools` 迭代算一次，不按 SDK 内部翻的页各算一次——迭代器只收我们给的那一个 ctx，翻页都在它底下），一次 `tools/call` 一个。

**`Headers` 的值是凭据，不进日志、不进错误。** 这条管的是**我们自己拼的文案**：只允许出现 server 名、远端工具名与阶段，header 值一个字都不能有；Warn 的字段固定为 `server` / `stage` / `err`。远端给的内容是另一回事——`isError` 的 text 按设计要让模型看见（它要据此自纠），成功路径的内容块同理，这两处不做"脱敏"，只受「工具适配」那条长度兜底的约束。仓库里已有同一纪律的先例：`schema.ImagePlaceholderText`（`pi/schema/message_content.go:146-159`）刻意只留 scheme/host/path，为的就是不让签名与临时 Token 漏进模型上下文。

有一条要说清楚，否则这条纪律看着比实际严：`err` 里裹着的 SDK 错误是原样上抛的，它可能带 endpoint 与端点的响应体片段，我们剪不掉（剪了就没了诊断信息）。所以真正要做的是**保证 endpoint 里没有凭据**——`validate` 拒绝 URL 上的 userinfo 与 query，就是为这个；鉴权一律走 headers，它的值只存在于 `config.Headers` 与请求头里，不进任何一处文案。

## 工具适配

代理工具（包内），实现 `pi/tools` 的 `Tool`（`pi/tools/interface.go:11-14`）：

```go
const (
	toolNamePrefix = "mcp__"
	// maxRemoteTextBytes 是单个远端结果里文本总量的兜底上限，成功与失败两条路
	// 共用。它不是预算而是兜底：MCP 的 tools/call 没有 offset 这类续读手段，
	// 截掉的部分补不回来，所以阈值要远高于正常结果——碰到了说明这个 server
	// 在成批吐数据，模型看到的截断标记就是提醒。量级取自
	// pi/prompt.go:19 的 maxAgentsFileBytes（1 MiB）。
	maxRemoteTextBytes = 1024 * 1024
)

var remoteNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

type remoteTool struct {
	definition schema.ToolDefinition // 本地定义：名字带前缀，Label/Description/schema 从远端映射来
	remoteName string                // 远端原名，tools/call 时用它
	server     string                // 只进错误文案
	session    *mcp.ClientSession    // 复用扩展那条会话
	timeout    time.Duration         // 来自配置，Execute 里套一层 ctx 超时
}

// newRemoteTool 把远端 Tool 包成本地工具，远端名字闸开在这里。
func newRemoteTool(
	remote *mcp.Tool, session *mcp.ClientSession, server string, timeout time.Duration,
) (*remoteTool, error) {
	if !remoteNamePattern.MatchString(remote.Name) {
		return nil, pierrors.ErrToolDefinitionInvalid.Wrap(fmt.Errorf(
			"server %q 的工具名 %q 必须匹配 ^[a-zA-Z0-9_-]{1,64}$", server, remote.Name))
	}
	return &remoteTool{
		definition: schema.ToolDefinition{
			Name:         toolNamePrefix + server + "__" + remote.Name,
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

// displayName 按 SDK 写下的优先级取展示名（mcp/protocol.go:1051 那段注释）：
// title > annotations.title > name。
func displayName(remote *mcp.Tool) string {
	if remote.Title != "" {
		return remote.Title
	}
	if remote.Annotations != nil && remote.Annotations.Title != "" {
		return remote.Annotations.Title
	}
	return remote.Name
}

func (t *remoteTool) Definition() schema.ToolDefinition { return t.definition }

func (t *remoteTool) Execute(
	ctx context.Context, arguments json.RawMessage, _ *tools.UpdateEmitter,
) (*schema.ToolOutput, error) {
	// 超时套在传进来的 ctx 上，SDK 自己的重试也落在同一个窗口里。
	callCtx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()

	// 空的 RawMessage 不能直接进 CallToolParams：它会在 json.Marshal 那步
	// 报错。参数被省略时按空对象发。
	if len(arguments) == 0 {
		arguments = json.RawMessage("{}")
	}

	result, err := t.session.CallTool(callCtx, &mcp.CallToolParams{
		Name:      t.remoteName,
		Arguments: arguments,
	})
	if err != nil {
		// 超时与取消原样返回，别套 81004：CodeOf 会先看到 MCP 码，
		// 调用方就分不出"远端坏了"和"我这轮超时了"。
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
	// 成功路径的上限交给 pi/tools.LimitText（pi/tools/output.go:13）：它截断
	// 时保证 UTF-8 完整，追加固定的截断标记，并在 Details 上记 truncated。
	// 它返回的是值，这里取地址再交出去。
	output := tools.LimitText(schema.ToolOutput{
		Content: contentBlocks(result.Content),
		Details: result.StructuredContent,
	}, maxRemoteTextBytes)
	return &output, nil
}

// contentBlocks 把远端内容块映成本地内容块，顺序原样保持。
func contentBlocks(content []mcp.Content) schema.ContentBlocks {
	blocks := make(schema.ContentBlocks, 0, len(content))
	for _, item := range content {
		switch typed := item.(type) {
		case *mcp.TextContent:
			blocks = append(blocks, schema.TextBlock(typed.Text))
		case *mcp.ImageContent:
			blocks = append(blocks, schema.ContentBlock{
				Type: schema.ContentTypeImage,
				Image: &schema.ImageContent{
					// SDK 的 Data 在反序列化时已经从 wire 上的 base64 还原成
					// 原始字节，本地协议要的是 base64 字符串，得重新编码。
					Data:     base64.StdEncoding.EncodeToString(typed.Data),
					MIMEType: typed.MIMEType,
				},
			})
		default:
			// audio / resource / resource_link 落到这里：写明类型与标识，
			// 不静默丢。
			blocks = append(blocks, schema.TextBlock(placeholderText(item)))
		}
	}
	return blocks
}

// placeholderText 给本地协议装不下的内容块生成一条文本占位。
func placeholderText(item mcp.Content) string {
	switch typed := item.(type) {
	case *mcp.AudioContent:
		return fmt.Sprintf("[音频: %s]", typed.MIMEType)
	case *mcp.ResourceLink:
		return fmt.Sprintf("[资源链接: %s]", typed.URI)
	case *mcp.EmbeddedResource:
		if typed.Resource != nil {
			return fmt.Sprintf("[内嵌资源: %s]", typed.Resource.URI)
		}
		return "[内嵌资源]"
	default:
		return fmt.Sprintf("[不支持的内容块: %T]", item)
	}
}

// errorText 拼 isError 的错误文本：text 按原序拼，其余类型只出占位——
// image 绝不能把 base64 带进来（一张图几百 KB，日志与模型上下文都撑不住，
// 而截图本身可能带敏感信息）。拼完过一道长度兜底。
func errorText(content []mcp.Content) string {
	var builder strings.Builder
	for _, item := range content {
		switch typed := item.(type) {
		case *mcp.TextContent:
			builder.WriteString(typed.Text)
		case *mcp.ImageContent:
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

// limitText 兜住远端文本的量级，保证 UTF-8 完整。成功路径用 pi/tools.LimitText
// 做同一件事；错误路径返回的是 error 而不是 ToolOutput，走不到那个函数，所以
// 这里复用同一个上限与同一个标记（标记由 pi/tools 导出，见「pi/tools 的补齐」）。
func limitText(text string) string {
	if len(text) <= maxRemoteTextBytes {
		return text
	}
	cut := maxRemoteTextBytes
	for cut > 0 && !utf8.ValidString(text[:cut]) {
		cut--
	}
	return text[:cut] + tools.OutputTruncationMarker
}
```

`remoteName` 与 `definition.Name` 必须分开存：进 Provider 请求的是本地名，`tools/call` 发出去的是远端原名，混用一个字段以后改前缀会把请求打歪。单次超时在这里落地（`context.WithTimeout` 套在传入 ctx 上），不放在 `http.Client.Timeout` 上——放 ctx 才能把 SDK 自己的重试一起圈进去。

远端 `Tool`（`mcp/protocol.go:1045` 起）到 `schema.ToolDefinition`：

| 目标字段 | 来源 |
|---|---|
| `Name` | `mcp__<server>__<远端名>` |
| `Label` | 远端的展示名，优先级 title > annotations.title > name（`mcp/protocol.go:1051` 的注释就是这么写的，`displayName` 照它实现） |
| `Description` | `Tool.Description` |
| `InputSchema` | `Tool.InputSchema`（client 侧是 `map[string]any`） |
| `ParallelSafe` | 固定 `false` |

`ParallelSafe=false` 的理由：远端 server 是不是并发安全的，我们无从判断，而 MCP 也没有任何字段声明它。按"不可并发"保守处理，代价是同一批里的 MCP 调用串行。

远端结果到 `schema.ContentBlocks` + `Details`：

| 远端内容 | 落到哪 |
|---|---|
| `text` | `schema.TextBlock(text)`，多块按序拼接 |
| `image` | `schema.ImageContent{Data: base64.StdEncoding.EncodeToString(远端 Data), MIMEType: 远端 MIMEType}`（`pi/schema/message_content.go:37-44`）；SDK 的 `Data` 是 `[]byte`（`mcp/content.go:56`，wire 上的 base64 在反序列化时已经还原成原始字节），所以要重新编码，不能直接塞字符串 |
| `audio` / `resource` / `resource_link` | 一条占位文本，写明类型与它的标识——不静默丢 |
| `structuredContent` | `schema.ToolOutput.Details`（不进模型上下文，`pi/schema/tools.go:133`） |
| `isError: true` | 返回 error（`ErrMCPToolCallFailed`），内容按下面那条规则拼成错误文本 |

SDK 的 `Content` 是接口（`mcp/content.go:18-21`，`fromWire` 未导出，包外无法自己实现），所以上面的适配只能按 `*mcp.TextContent` / `*mcp.ImageContent` / `*mcp.AudioContent` / `*mcp.ResourceLink` / `*mcp.EmbeddedResource` 做类型 switch，没有别的入口。

**远端名字要过闸。** 远端工具名会拼进本地工具名，并原样进两家 Provider 的请求，而 `pi/tools` 只校验非空与首尾空白（`pi/tools/register.go:163-171`）——远端给个 `search.web`、带空格的、或超长的名字都会一路走到 Provider 那一步才炸。包裹时按 `^[a-zA-Z0-9_-]{1,64}$` 校验收到的远端名（两家 Provider 的共同口径），过不了返回 `ErrToolDefinitionInvalid`（30008，工具段的现有码，不为它扩 MCP 码表）。按整批原子的口径，一个非法名字会让该 server 整体失败，再由 `required` 决定致命还是跳过。

`isError` 走 error 返回而不是压成文本，是因为执行链已经接住了这个语义：`pi/tools/scheduler.go:116-117` 把非 nil error 转成 `NewErrorEvent`（`IsError=true` + `CodeOf` 取码），而成功路径的 `NewEndEvent` 把 isError 写死为 false（`scheduler.go:120`）。`tools.Tool.Execute` 没有 isError 通道（`pi/tools/interface.go:11-14`），返回 error 是唯一能让事件如实标记的路径。代价：多块内容里夹一个 `isError` 时会被展平成一条错误文本——这个失真记在这儿，修它要动 `schema.ToolOutput`。

错误文本怎么拼，规则钉死：text 块按原顺序拼接；`image` / `audio` / `resource` / `resource_link` 各自转成一行占位——`image` 只写 `[图片]`，绝不把 base64 塞进去（一张图几百 KB，进错误文本就会撑爆日志与模型上下文，而截图本身可能带敏感信息）；一块都没有时用固定文案 `远端返回了空错误`。拼好的文本交给 `ErrMCPToolCallFailed.Wrap`，`Error()` 就是它，`CodeOf` 取到 81004。`structuredContent` 不参与错误文本，也不会出现在事件里——`NewErrorEvent` 只构造一条文本块（`pi/tools/event.go:46-50`）。

**远端内容有一个兜底上限，成功与失败两条路都过。** 远端返回多少文本我们控制不了，而两种结果都会进模型上下文——只给 `isError` 加上限是不自洽的，一次搜索或 `resource` 同样能塞进几十 MB。所以上限 `maxRemoteTextBytes`（1 MiB）两条路共用，走法不同：成功路径把产出的 `ToolOutput` 直接过一遍 `pi/tools.LimitText`（`pi/tools/output.go:13`），标记与 `Details.truncated` 由它给；错误路径返回的是 error，到不了那个函数，`errorText` 自己截并追加同一个标记。标记这轮从包内常量改为导出（`pi/tools/constant.go:4` 的 `toolOutputTruncationMarker` → `OutputTruncationMarker`），两边共用一个字符串，免得以后改一处漏一处。错误路径还有一处不对称：事件的 Details 到不了——`NewErrorEvent` 只带一条文本块（`pi/tools/event.go:46-50`）——所以"这次被截断了"只能靠文本尾部的标记传达。

上限取 1 MiB 而不是 `pi/tools/impl/read.go:23` 那个 50 KiB，是因为 read 有 offset、模型可以接着读，而 MCP 的 `tools/call` 没有续读手段，截掉的信息补不回来：这里只当防病态输出的兜底，不当预算。另外，`pi/tools.LimitText` 在这轮之前是零调用方（仓库内 grep 只有它自己的定义，也没有测试），MCP 是它第一个使用者，所以这轮顺手给它补上基础用例。

工具名前缀固定、不提供配置项：唯一性由结构保证（`mcp__` 开头不可能撞内置工具，server 名做中段），不靠用户自觉；两个 server 同名工具也不会撞。想让模型看到更短的名字，改 `ServerConfig.Name` 就行。

## 扩展与失败策略

```go
// pi/mcp/extension.go

const (
	// defaultClientName 是 clientName 没传时 initialize 报给远端的名字。
	defaultClientName = "go-harness"
	// clientVersion 写死，不配置：仓库里没有版本来源（`version.txt` 已经删了，
	// 它存的是 go 版本、而且是测试数据）。将来引入版本号，改这一处。
	clientVersion = "pro"
)

// clientName 是 initialize 时报给远端的身份，空串取 defaultClientName。它是
// client 级、不是 server 级的：同一个 harness 对每个 server 报同一个名字，所以
// 它进这个参数而不是进 ServerConfig。
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

一个 `ServerConfig` 一个扩展，扩展名 `mcp:<server>`，`Name()` 同时就是 owner。返回切片而不是单个扩展，`cmd/harness` 直接 append 进 `Options.Extensions`；按 server 拆分的循环关在 `pi/mcp` 里，`pi` 根包不认识 server 这个概念。

`clientName` 走参数而不是 `ServerConfig` 的字段：identity 是 client 级的，同一个 harness 对每个 server 报同一个名字。`cmd/harness` 传空串（取缺省 `go-harness`），`cmd/mcptest` 传 `go-harness-mcptest`——两个驱动接同一个远端时，那边看得出是谁在连。版本不在此列：它是写死的常量（`clientVersion`），没有可配的东西。

扩展本身（包内）：

```go
type serverExtension struct {
	config ServerConfig
	// clientName 是 initialize 时报给远端的名字，NewExtension 已经填过缺省，
	// 所有扩展共一份。
	clientName string
	// stderr 收 stdio 子进程的 stderr 尾部，只在 server 级失败时读出来（见
	// stderr.go 的三条取舍）。http 那条路上它一直是空的。
	stderr  *stderrTail
	session *mcp.ClientSession // Register 连上后拿到；Close 关它
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
func (e *serverExtension) connect(ctx context.Context) (*mcp.ClientSession, error) {
	transport, err := newTransport(e.config, e.stderr)
	if err != nil {
		return nil, err
	}
	connectCtx, cancel := context.WithTimeout(ctx, e.config.Timeout)
	defer cancel()

	client := mcp.NewClient(&mcp.Implementation{
		Name:    e.clientName,
		Version: clientVersion,
	}, nil)
	session, err := client.Connect(connectCtx, transport, nil)
	if err != nil {
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
	ctx context.Context, session *mcp.ClientSession,
) ([]tools.Tool, error) {
	listCtx, cancel := context.WithTimeout(ctx, e.config.Timeout)
	defer cancel()

	remote := make(map[string]*mcp.Tool)
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
			remote[name], session, e.config.Name, e.config.Timeout)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

// selected 要注册的远端工具名，按名字排序：这个次序决定工具在 Provider 请求里
// 的次序，不排的话每次装配都可能不一样。
func (e *serverExtension) selected(remote map[string]*mcp.Tool) []string {
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
func (e *serverExtension) missingAllowed(remote map[string]*mcp.Tool) []string {
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
```

子进程 stderr 的尾部贴在这一个出口，而不是分别贴在致命与跳过两条路上：两条路都要它（致命那条更要——`required: true` 的 server 起不来是装配失败，日志里不会再有一条 Warn 兜底），贴两处早晚会漏一处。用 `%w` 包住原错误，`CodeOf` 拿到的还是 81002，不会因为加了一截文案就换成别的码。

`Close` 只关会话：`ClientSession.Close` 自己会收传输与后台 SSE，SDK 写的注释是"is idempotent and concurrency safe"（`mcp/client.go:309-313`），所以不必再加一层保护。跳过的 server 分两种——从没连上的 `session` 是 nil，连上后才跳过的已经在自己的收尾里关掉了——所以 `Close` 要挡 nil，重复关则不用管。（同一条注释还说它会等在途请求返回，对 `Agent.Close` 的语义有影响，见「pi 包根的装配」那一节。）

`Register(ctx)` 的流程（每个 server 一份）：

1. `Client.Connect(ctx, …)` → 失败：`Required` 为真则返回错误，否则记一条 Warn 并返回空列表（这个 server 不接任何工具）。
2. `session.Tools` 拉全量工具（SDK 自动翻页）→ 失败同上。
3. 非空 `AllowTools` 时逐项核对远端有没有；缺项报 `ErrMCPToolNotFound`，`Required` 为真则致命。
4. 逐项 wrap 成代理工具（含上节的远端名字闸），把这一批连同会话交出去——登记由 Runtime 做。

**关会话这件事分成两半，按"谁知道"分。** `Connect` 成功之后的失败有两种：一种是这个 server 自己判定的（列工具失败、白名单缺项），非必需时空列表跳过——Runtime 那边看着是"成功、零个工具"，收尾不会替它关，所以扩展自己关（一条路，`discover` 失败那一处）。另一种是登记被拒（重名、冻结、坏 schema）：决定权在 Runtime，扩展只看到"产出成功"，所以由 Runtime 关掉这个扩展——它可能已经连上了远端，而它不在"已启动"那批里（`started` 只记成功的）。两条各归各的，都是"占下资源的那一方收尾"。

非致命跳过的那条 Warn 由扩展自己记：`logsdk.Warn(ctx, "mcp server skipped", logsdk.Any("server", name), logsdk.Any("stage", stage), logsdk.Any("err", err))`（`go-logger-sdk`，`pi/middleware/logging.go` 已在用这套）。它落在扩展这一层而不是 Runtime 那层，是因为只有扩展知道跳过的原因与 server 名；Runtime 把错误原样上抛就够了。stdio server 失败时 `err` 里还带着子进程 stderr 的最后几行（凭据已隐去，见「传输与协议」）。

失败策略汇总：

| 时机 | 情况 | 行为 |
|---|---|---|
| 装配前 | 配置非法（`type` / `name` / `url` / `headers` / `timeout`） | 一律致命（`ErrMCPConfigInvalid`），与 `required` 无关 |
| 启动期 | server 连不上 / 握手失败 / 列工具失败 | `Required` 真：装配失败（`ErrMCPConnectFailed`）；假：跳过并关会话，记 Warn。因 ctx 超时/取消而失败的按「错误码」那节给 40000/40001 |
| 启动期 | 白名单里的工具远端没有 | 同上，错误码 `ErrMCPToolNotFound` |
| 启动期 | 远端工具名过不了名字闸 | 同上，错误码 `ErrToolDefinitionInvalid`（30008） |
| 启动期 | 工具名与已注册工具冲突 | `pi/tools` 自己的 `ErrToolAlreadyRegistered` 冒上来，扩展整体失败。这条失败由 Runtime 判：它登记时才知道撞了，所以收尾也归它——失败的扩展不留在"已启动"里，得单独关 |
| 运行期 | `tools/call` 失败或 `isError` | 该次调用事件 `IsError=true`（`ErrMCPToolCallFailed`），循环继续，模型看到错误文本 |
| 运行期 | 会话失效（SDK 重连失败） | 同上，表现为调用失败 |
| 停机期 | `CloseAll` | 逆序关闭；`Agent.Close` 把各扩展的关闭错误 `errors.Join` 后返回，`cmd/harness` 打到 stderr、不改退出码；在途的那次调用会先跑完（SDK 的 `Close` 要等它，见「pi 包根的装配」） |

运行期失败刻意不中断整轮：工具调错了模型可以改，一轮里一次调用失败就掀桌子没有必要。

## 错误码

`pi/error/errors.go` 现有分段到 80000（会话），MCP 取 81000。90000 不能占：它已经是 `ErrInternal`（`pi/error/errors.go:174`，`pi/loop.go` 与 `pi/session/manager.go` 在用）。

| 码 | 名字 | 什么时候 |
|---|---|---|
| 81000 | `ErrMCPConfigInvalid` | 配置非法：名字白名单、`transport` 字段与传输不匹配、`url`、`headers`/`header_env`、`allow_tools`、`tool_prefix`、`timeout` |
| 81001 | `ErrMCPTransportUnsupported` | `transport` 写了 `http`/`stdio` 以外的值 |
| 81002 | `ErrMCPConnectFailed` | 连接、握手、列工具失败 |
| 81003 | `ErrMCPToolNotFound` | 白名单里的工具远端没有 |
| 81004 | `ErrMCPToolCallFailed` | 运行期 `tools/call` 失败或 `isError` |

五个码对应五种不同的处置动作：改配置 / 换客户端或等实现 / 查远端可达性与凭据 / 改白名单 / 查工具本身。

超时与取消不新码，但有一条约束：**不要给 `context` 错误套 MCP 码。** `CodeOf` 先看错误链里的带码错误，再看 `context.Canceled` / `context.DeadlineExceeded`（`pi/error/errors.go:190-196` 就是这个顺序），所以 `ErrMCPToolCallFailed.Wrap(ctx.Err())` 的结果是 81004 而不是 40001——调用方就分不出"远端坏了"和"我这轮超时了"。规则：`pi/mcp` 在超时与取消时原样返回 ctx 错误，不 Wrap，其余失败才套 81002 / 81004。这对启动期同样成立：启动期因 ctx 超时而失败时给 40001，而不是 81002——超时就是超时，别用业务码盖住它。这条要有测试钉住，否则以后有人顺手加个 Wrap 就退化了。

## 配置示例

```json
{
  "currentPlatform": "anthropic",
  "platforms": [ ... ],
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

`timeout` 写成 `60` 就是 60 秒（写 `"1m"` 也一样）；`header_env` 要求 `EXA_API_KEY` 在环境里，没设就在装配时报错并点出变量名（`validate` 取环境变量，见「pi/mcp 的配置」；读文件那一步只搬值，不查环境）。stdio 那条的 `timeout` 给 60 而不是缺省的 30：`npx` 第一次跑要下包，冷启动算在这一个 `timeout` 里。`cwd` 相对的是驱动进程的工作目录（见「传输与协议」），`args` 里那个 `.` 才是给 filesystem server 自己的工作根。

## 测试

全部离线，用 `httptest.Server` 起假 MCP server，不碰公网。

一个体例问题要先说：这轮之前 `pi/` 下一个 `_test.go` 都没有，测试全在 `cmd/*/main_test.go`。下面的表把单元测试放在实现旁边（Go 的常规做法，注册表回滚、配置校验这类纯逻辑在那儿测得最省），`cmd/mcptest` 则接现有的"一篇一个驱动"体例。如果专栏的体例是"测试一律进 cmd 驱动"，`pi/` 下那几个 `_test.go` 可以退掉，逻辑转由 `cmd/mcptest` 覆盖——但那样 `pi/tools` 的 owner 回滚就只能间接测，这是取舍。

现状（2026-09-24）：仓库里有 `pi/tools/register_test.go`、`pi/tools/output_test.go`、`pi/extension/runtime_test.go`、`pi/mcp/extension_test.go` 与 `cmd/internal/mcpconfig/mcpconfig_test.go`。表里其余几份当前不在——那几块覆盖现在是空的，落不落回来由专栏体例定。

| 文件 | 覆盖 |
|---|---|
| `pi/tools/register_test.go` | `RegisterFor` 整批成功 / 中途失败后回到调用前 / 不影响其他 owner / 重复 owner 被拒（`ErrToolAlreadyRegistered`）/ 空 owner 被拒；`Rollback` 精确匹配、数量、Freeze 后仍可摘除；Freeze 之后每条入口都挡新增（守卫在 `registerLocked`，静态那条入口每次都发新注册表，所以只能走 `RegisterFor` 测），空批不报错；包级 `Register` 与扩展走同一条入口，坏定义与坏 schema 一样在注册期被拒 |
| `pi/tools/output_test.go` | `LimitText`：不超限时原样返回（不碰 Details）、超限时截断并追加标记、切点在多字节字符中间时保证 UTF-8 完整、非文本块不占额度、Details 的 nil / map / 其他三种形态都带上 `truncated` |
| `cmd/internal/mcpconfig/mcpconfig_test.go` | 文件那一份的规矩：不认识的字段被拒且文案列出认得的名字（`type` / `allowed_tools` / `endpoint` 三种抄错的写法）、`timeout` 的数字按秒与字符串与小数、不写 / `null` / 空串留成 0、零 / 负值 / 非法串 / 布尔报 81000、`ServerConfigs` 一比一搬值、`header_env` 不在这一步解析、坏值（名字、`transport`）原样带过去 |
| `pi/extension/runtime_test.go` | 按声明顺序接入（不按名字重排）、工具以扩展名为 owner 进表、重名与空名与类型化 nil 拒绝、空列表给出可用的 Runtime、ctx 原样透传给每个扩展、失败的那一个与已启动的都收尾（失败出在产出工具时与出在登记时各一条，两条都断言事件次序）、`errors.Join` 里注册错与收尾错都在、没实现 `Closer` 的扩展直接跳过、`CloseAll` 逆序且只真关一次 |
| `pi/mcp/config_test.go` | 运行期那份的规矩：名字与 `tool_prefix` 的字符集（含缺省）、`transport` 三个分支（http / stdio / 其他）、两种传输的字段互斥（http 不许给 command、stdio 不许给 url）、`url` 的 scheme / host / userinfo・query・fragment、`timeout` 的 0 与非正数都补默认（两种写法的解析与显式非正数的错在 `cmd/internal/mcpconfig` 那侧测）、`headers` 与 `header_env` 被控头的归一（`host`、`HOST` 都要拦）、`header_env` 取到值 / 变量缺失报错且点出变量名（值不出现在错误里）、不认识的字段在装载时被拒（在 `cmd/internal/mcpconfig` 那侧测）、`enabled: false` 跳过装配且不校验 |
| `pi/mcp/tool_test.go` | 定义映射（名字前缀、Label、ParallelSafe）、text 多块、image（`[]byte` → base64 重编码）、占位文本、structuredContent 进 Details、isError → error、isError 的错误文本（多块按序拼、图片只出 `[图片]` 不带 base64、空内容用固定文案）、非法远端名 → `ErrToolDefinitionInvalid`、超时与取消原样返回 ctx 错误（`CodeOf` 得到 40001/40000，不是 81004）、成功路径超限被截断（文本以标记结尾、`Details.truncated` 为真）、isError 的超长文本也被截断（标记在尾部、切点处 UTF-8 完整、文本里没有 base64） |
| `pi/mcp/extension_test.go` | 假 server：正常接入、白名单缺项、必需/非必需 server 失败时的两种行为、失败与跳过路径都不漏关会话（假 server 侧断言连接被关）、`config.Timeout` 生效（拖住不回的 server 被中断，`CodeOf` 得 40001）、Close 幂等、关闭时在途的调用先跑完再关（假 server 拖住响应，断言 `Close` 阻塞到那次调用返回，不是把它打断）；stdio 那条路真起一个子进程——对端是测试二进制自己（`-test.run` + 环境变量换个身份当 stdio server），所以不依赖 `npx`，CI 里也跑得起来；起不来的子进程把 stderr 尾部带进错误文案（致命与跳过两条路都带、`CodeOf` 仍是 81002），配置里的凭据值在这段文案里已换成占位文本；`clientName` 缺省与传入（假 server 侧断言 initialize 收到的 `clientInfo.name`，以及每个 server 收到的是同一个名字）。假 server 提供 `echo` 一个工具，所以接入成功那条路能走真 Runtime 断言：本地名、owner、与收尾一起验。当前仓库里这份落了三条：stdio 子进程起不来（必需致命 / 非必需跳过两条路、stderr 尾部进错误文案、`CodeOf` 是 81002）、列工具失败仍关会话（子进程在连接关掉后写标记文件，测试读它作证据）、经真 Runtime 的接入成功与停机收尾。还缺：`config.Timeout` 生效、Close 幂等、在途调用先跑完、`clientName` 缺省与传入 |
| `pi/mcp/stderr_test.go` | `stderrTail` 的三条取舍：只留最后 4 KiB（写满不阻断）、只贴最后 8 行、超行数与超字节都带 "…" 前缀（别让人把这几行当成全部）、空输入返回空串、凭据值换成占位文本而短于 8 个字符的值不动、`-race` 下并发 `Write` 干净 |
| `pi/agent_test.go` | 装配顺序：扩展注册的工具在 `Freeze` 之前进表；装配失败时不返回 Agent；装配 ctx 超时会中止启动（`CodeOf` 得到 40001）；`Close` 后 `Run` 返回 `ErrClosed`；`Close` 不取消在途的 `Run`；多次 `Close` 的错误聚合；`Close` 先标记后关（用一个 `Close` 里阻塞的假扩展制造窗口，其间新起的 `Run` 必须拿到 `ErrClosed` 而不是调用失败）；`go test -race` 下 `Run` 与 `Close` 并发 |
| `cmd/mcptest` | 不设测试文件：它是驱动，跑起来连真 server、看真数据（`mcp.servers` 里几个不需要 Key 的公共 server 就够）。默认这条走 `pi.NewAgent`：任务交给模型，模型自己挑工具，验收方式是人看一眼答复是不是工具回的——默认任务是查天气，凭记忆答出来的温度立不住，正好当这个验收。工作区（`-workdir`）里必须有 AGENTS.md，agent 的定义从它读，缺了在开跑前报 10006；默认用驱动自带的 `cmd/mcptest/testdata`，那份 AGENTS.md 把身份写成"接入了 MCP 工具的 agent"、写明工具名以 `mcp__` 开头。借别的工作区（比如 skilltest 那份定义成"测试 agent，验证资源加载与技能发现流程"的）会让模型把任务当成本地能力问题：grep 工作区、看技能目录、然后拒答，并且谎报自己的工具清单（"只有 `bash`、`read`、`write`、`edit`"）。实测过：同一份 `config.json`、同一句任务，只换这份 AGENTS.md 就会去调 `mcp__exa__web_search_exa`。会话落盘而不只是内存：`-sessions`（默认 `testdata/sessions`，与 sessiontest 同一处）下按 `-key`（默认 `mcp-001`）取文件，带同一个 key 再跑就是接着聊，历史从会话重建；装配前会把这个路径打出来。`-prompt ""` 那条不开模型：按配置列工具，再照远端的 `inputSchema` 给常见工具各调一次示例参数（表写在驱动里：`web_fetch_exa` 要的是 `urls` 数组、`web_search_exa` 要 `query` 与 `objective`，写错会被对端以 -32602 打回）；停用的 server 单列一行跳过，一次调用失败不挡后面的调用，收尾一起报。stdio 那条的验收命令就是这两条（把 `fs` 开成 `enabled: true`，见「传输与协议」的实测段）。适配逻辑与失败路径的断言在 `pi/mcp` 那三组测试里（`extension_test.go` 已在仓库，另两组见上表），这里不另起一份假 server |

## 实施顺序

每步独立可测，前一步绿了再走下一步。

1. `go get github.com/modelcontextprotocol/go-sdk@v1.3.1`，`go.mod` / `go.sum` 落锁——现在只有本机 module cache 里有，仓库还没引。
2. `pi/tools`：`RegisterFor` + `Rollback` + 测试；顺带把 `register` 拆成 `registerLocked`，包级 `Register` 并到 `RegisterFor` 这条入口（冻结判只在 `registerLocked` 一处）。
3. `pi/extension`：`Runtime` + 测试；契约 `extension.go` 同期定成"扩展产出工具"（`API` 撤掉），`Runtime` 承担登记与失败收尾。
4. `pi/agent.go`：`Options.Extensions`、`NewAgent` 加 ctx（三个调用点与测试跟着改）、装配顺序、`Close`、`Run` 的 `ErrClosed` + 测试。
5. `pi/error`：81000 段五个码。
6. `pi/mcp/config.go`：运行期配置与校验 + 测试；文件形状那一份（`Entry` / `Duration` / `ServerConfigs` / 严格解码）同期落在 `cmd/internal/mcpconfig` 里 + 测试。
7. `pi/mcp/tool.go` + `extension.go` + `stderr.go`：装配 SDK、代理工具、扩展、stdio 子进程 stderr 的尾部采集 + 测试。
8. `cmd/mcptest` 驱动：先落"列工具 + 示例调用"那条不开模型的路，再接 `pi.NewAgent` 的 agent 路；`cmd/harness` 与 `config.example.json` 加 `mcp.servers` 配置，装配处加启动 ctx 超时。

## 待验证项（动手前先钉）

只剩一条，而且是运行期语义，不影响现在动手：

1. 会话失效后 SDK 的重连是否重跑 `initialize`，以及它与"每次调用自带的 ctx 超时"如何相互作用。SDK 的设计文档写了 streamable client 对用户透明地重连（`design/design.md:204-206`，另有 `ReconnectOptions`），所以这不是缺口，但它是运行期语义，写重连测试之前先钉。

原先列在这里的两条已经核掉，记下位置备查：展示名优先级是 SDK 写在 `Tool.Annotations` 字段上的 `title, annotations.title, then name`（`mcp/protocol.go:1051`），`displayName` 照它写；内容块字段是 `Data []byte` 与 `MIMEType`（`mcp/content.go:53-58`），所以 image 要做一次 base64 重编码。

## 附：外部参照

这份设计的两个信息来源。

**官方 SDK 的传输抽象**（`modelcontextprotocol/go-sdk@v1.3.1`，本机）：它的 `Transport` 是 `Connect(ctx) (Connection, error)`，`Connection` 是双向的 `Read`/`Write`/`Close`（`mcp/transport.go:33-60`）——比"请求-响应"更一般，因为它要容纳服务端主动发起的消息（通知、sampling）。这是"用 SDK"这条决策的信息来源：我们的工具调用只是它能力的一个子集。它的设计文档（`design/design.md`，自称 canonical）把这条选择的理由写明了：JSON-RPC 本身就是双向连接，而"带 sendNotification / makeCall 的高层传输"是连接之上的操作，低层接口更容易实现自定义传输（`design/design.md:84-92`）。

**协议版本**：SDK 默认协商 `2025-06-18`（`mcp/shared.go:37`），并支持与 `2025-03-26`、`2024-11-05` 协商降级（`mcp/shared.go:45-48`）。我们不固定某一版。

**MCP 规范本身的形状**：`tools/list` 带游标分页、`tools/call` 返回内容块数组 + `structuredContent` + `isError`、Streamable HTTP 单 endpoint 与 `Mcp-Session-Id`。这些决定了工具适配与失败策略那两节的写法。

## 附：这一轮之后自然会长出来的东西

都不是现在要做，只是确认这轮的选择不会挡路：

- stdio 子进程 stderr 的全量与结构化（见「目标与非目标」）：尾部几行已经用上了（`pi/mcp/stderr.go`），要出更多得先把"哪些能进日志"定下来。
- 运行期失败的重试与降级：现在只按 `required` 分"致命/跳过"两档，没有"连不上时退到上一次的工具表"这类中间态。
- 更多扩展（skill 打包、外部知识库）：`pi/extension` 的契约与 `Runtime` 已经是通用的，不必为 MCP 特化。
