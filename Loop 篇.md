# 从零手搓 Harness 之主循环：一个 for 循环，让模型真的能干活

## 前言

这是从零开始搭建 Agent Harness（`go-harness`）的第三篇。开篇讲清楚了为什么自搓，第二篇把模型封装成了统一的流式接口——到这里我们只有一个"大脑接口"，模型还只能跟你打字聊天。这一篇把心脏装上：主循环（Main Loop），以及让模型能读文件、写文件、跑命令的工具执行域。

先交代一件事：第二篇发布后，仓库做过一次整理，消息模型、内容块、ToolCall 这些公共类型从 `pi/ai` 挪到了 `pi/schema`（`pi/ai` 回归只放 Provider 抽象）。本篇统一按新目录讲，对照老文章的读者注意对一下路径。

完整代码在 GitHub：[https://github.com/PycMono/go-harness](https://github.com/PycMono/go-harness)，本篇涉及的文件（`pi/loop.go`、`pi/tools/`、`pi/schema/`、`pi/agent.go`、`cmd/harness/`）都在仓库里可以直接翻。

## 循环全貌

先把整个循环的形状画出来，后面所有代码都在这张图里：

```text
用户输入 + 历史消息
      │
      ▼
┌─── 组装上下文（schema.Messages + ToolDefinitions）
│
├─── 调模型 provider.Stream() ──► 拿回一条 assistant 消息
│
├─── 消息里有 ToolCalls 吗？
│        │
│        ├─ 没有 ──► 循环结束，返回完整消息序列
│        │
│        └─ 有 ──► Scheduler.ExecuteBatch()
│                       │  注册表校验参数
│                       │  按 ParallelSafe 分波，波内并发
│                       ▼
│                  每个结果转成 RoleTool 消息回填
│                       │
└───────────────────────┘
```

主循环本身朴素到不像"引擎"：一个 `for` 循环，退出条件只有一个——模型不再请求调用工具。所有复杂度都在两件事上：**工具怎么执行、失败怎么处理**。

## 代码层级划分

```text
pi/
├── loop.go              # 主循环：模型调用与工具执行的编排
├── agent.go             # 装配入口：Provider + Registry + Scheduler + Loop
├── context.go           # 运行前上下文组装
├── schema/
│   ├── protocol.go      # 消息模型（第二篇那套，从 pi/ai 迁过来的）
│   └── tools.go         # ToolCall / ToolDefinition / ToolOutput
└── tools/
    ├── interface.go     # Tool 接口
    ├── register.go      # Registry：注册 + JSON Schema 编译
    ├── scheduler.go     # 调度：单发执行 + 分波并发
    ├── event.go         # 工具生命周期事件
    └── impl/            # 内置工具：read / write / edit / bash
```

分工对应流程图的三层：

| 层 | 职责 | 关键文件 |
|---|---|---|
| 循环层 | 决定"下一轮问谁、结果回哪"，不含任何业务逻辑 | `loop.go` |
| 调度层 | 工具查找、参数校验、并发执行、失败归一 | `scheduler.go` / `register.go` |
| 装配层 | 把 Provider、工具、循环拼成可运行的 Agent | `agent.go` / `cmd/harness` |

## 代码实战

### 1. 主循环：run()

全部代码在 `pi/loop.go`：

```go
const defaultMaxTurns = 100

type runState struct {
	messages       schema.Messages
	contextHistory schema.Messages
	availableTools schema.ToolDefinitions
}

func (l *Loop) run(ctx context.Context, runContext *Context) (schema.Messages, error) {
	state := &runState{
		contextHistory: append([]*schema.Message(nil), runContext.Messages...),
		availableTools: append(schema.ToolDefinitions(nil), runContext.Tools...),
	}
	state.messages = append(schema.Messages(nil), state.contextHistory...)

	for turn := 0; ; turn++ {
		if err := ctx.Err(); err != nil {
			return state.messages, pierrors.ErrCanceled.Wrap(fmt.Errorf("agent 运行已取消: %w", err))
		}
		if turn >= l.maxTurns {
			return state.messages, pierrors.ErrRunLimitExceeded.Wrap(
				fmt.Errorf("连续 %d 轮模型调用都要求执行工具，已终止运行", l.maxTurns))
		}

		message, err := l.complete(ctx, state)
		if err != nil {
			return state.messages, err
		}
		// 模型消息先入列：工具结果必须紧跟在发起调用的那条助手消息之后，
		// 两家协议都按这个顺序还原上下文。
		state.messages = append(state.messages, message)

		if len(message.ToolCalls) == 0 {
			return state.messages, nil
		}
		if l.scheduler == nil {
			return state.messages, pierrors.ErrInternal.Wrap(fmt.Errorf(
				"模型请求调用工具 %q，但本轮运行没有接入工具调度器", message.ToolCalls[0].Name))
		}

		results, err := l.scheduler.ExecuteBatch(ctx, message.ToolCalls)
		if err != nil {
			return state.messages, err
		}
		for index := range results {
			// 工具自身的失败同样作为一条 IsError 的工具消息回给模型，
			// 让模型自己决定重试还是换条路；只有调度层面的失败才中断。
			result := results[index].ResultMessage()
			state.messages = append(state.messages, &result)
		}
	}
}
```

这个循环里有几个决定值得单独说。

**退出条件只有两个：模型不调工具，或者保险丝烧了。** `maxTurns` 默认 100，撞到上限直接 `ErrRunLimitExceeded` 退出，而不是无限烧 token。有些观点认为工业级引擎不该设硬性步数上限，任务多复杂就该跑多久——我不这么看，不设上限的代价是模型一旦陷入自我循环就无限烧钱，而"连续 100 轮都在要求执行工具"这个状态本身已经说明模型出不来了，续命没有意义。真正的治理手段（预算准入、上下文压缩）在后面的篇章里，`maxTurns` 只是最后一道保险丝。

**消息顺序是协议硬约束。** 模型消息先入列，工具结果紧跟其后——第二篇讲过，OpenAI 和 Anthropic 都要求 tool 结果紧贴在发起调用的 assistant 消息后面，这个顺序一乱，历史消息回填时两家协议都会报错。

**出错也返回部分消息序列。** 看 `run` 的每个返回点，`state.messages` 都跟着一起回去。跑到第 7 轮挂在工具调用上，调用方拿到的就是前 6 轮的完整对话，出问题的时候能直接看到挂在哪一步，而不是只有一个孤立 error。

`runState` 里 `contextHistory` 保留了一份组装好的原始上下文副本，`messages` 是滚动增长的工作区。目前两者内容一致，但后面做压缩的时候，"原始上下文"和"实际发给模型的上下文"会分家，这里先把这个口留出来。

### 2. complete()：把第二篇的 Stream 接进来

```go
func (l *Loop) complete(ctx context.Context, state *runState) (*schema.Message, error) {
	stream := l.provider.Stream(ctx, state.messages, state.availableTools)
	defer stream.Close()

	for stream.Next() {
		event := stream.Current()
		if event.Type != schema.StreamEventTextDelta || l.onText == nil {
			continue
		}
		l.onText(event.TextDelta)
	}

	return stream.Result()
}
```

第二篇设计的拉取式流式接口在这里兑现：`Next()` 拉到流结束，文本增量实时交给 `TextObserver`（打字机效果就靠它），`Result()` 拿到完整的 assistant 消息。注意 `Result()` 要等流读到结束才有效，所以这里必须一直 `Next()` 到返回 false，不能中途 break。

还有一处刻意的设计：`WithScheduler` 不设置时，模型一旦请求调用工具，循环直接报 `ErrInternal` 退出。宁可明确失败，也不静默丢掉模型的工具调用——静默丢掉的后果是模型以为工具执行了，在错误的前提上继续推理，这种错误排查起来非常痛苦。

### 3. 工具接口：模型不认识你的工具，只认识 JSON Schema

工具的抽象就两个方法（`pi/tools/interface.go`）：

```go
type Tool interface {
	Definition() schema.ToolDefinition
	Execute(context.Context, json.RawMessage, *UpdateEmitter) (*schema.ToolOutput, error)
}
```

`ToolDefinition`（`pi/schema/tools.go`）是发给模型的那个"工具说明书"：

```go
type ToolDefinition struct {
	Name         string `json:"name"`
	Label        string `json:"label,omitempty"`
	Description  string `json:"description"`
	InputSchema  any    `json:"input_schema"`
	ParallelSafe bool   `json:"parallel_safe,omitempty"`
}
```

注意 `Execute` 的第二个参数：`json.RawMessage`。**主循环不解析工具参数，一个字节都不碰。** 模型传来的 JSON 原样传给具体工具，由工具自己解。这样写一个新工具不需要改循环的任何代码，参数结构完全是工具自己的事。

工具的返回值分两层：

```go
// ToolOutput 是工具一次执行的返回值。Content 直接作为消息内容写入上下文，
// Details 不进入模型上下文。
type ToolOutput struct {
	Content ContentBlocks `json:"content"`
	Details any           `json:"details,omitempty"`
}
```

`Content` 是模型能看到的，`Details`（比如 edit 的 diff 统计）是给人看的、走事件通道给前端展示用的。模型上下文里塞不进去的东西，都走 `Details`。

### 4. Registry：注册期把校验编译好

所有工具进 `Registry`（`pi/tools/register.go`）。注册的时候干一件重要的事：

```go
func (r *Registry) register(owner string, tool Tool) error {
	if isNilTool(tool) {
		return pierrors.ErrToolDefinitionInvalid.Wrap(errors.New("tool must not be nil"))
	}

	definition := tool.Definition()
	name := strings.TrimSpace(definition.Name)
	if name == "" {
		return pierrors.ErrToolDefinitionInvalid.Wrap(errors.New("tool definition name must not be empty"))
	}
	validateArgs, err := compileSchemaValidator(definition)
	if err != nil {
		return err
	}
	toolEntry := entry{definition: definition, tool: tool, validateArgs: validateArgs, owner: owner}
	...
}
```

`compileSchemaValidator` 用 `jsonschema` 库把 `InputSchema` 编译成一个校验函数，**在注册期完成，只做这一次**。之后模型每次调用工具，只需要跑一次编译好的 `validateArgs`，参数不合 Schema 的调用在进工具实现之前就被拦下。

为什么这么较真？因为模型传参数是会出错的——尤其是小参数量的工具，模型偶尔会幻觉出不存在的字段、把 string 传成 number。没有 Schema 校验的话，这些脏参数会直接进工具实现，炸出一些不知所云的运行时错误。校验挡在门口，模型收到的错误信息是"参数不匹配 schema"，它下一轮自己就改了。

另外两个小点：

- `Freeze()` 冻结注册表，装配完成之后拒绝一切新注册。运行期的注册表就该是只读的，把可变性直接关掉，而不是靠约定；
- `Definitions()` 返回的工具列表按名称排序。这个列表每一轮都会发给模型，顺序稳定对模型侧的 prompt cache 友好，缓存命中率直接影响成本——这个坑我们在下一篇讲上下文时细说。

### 5. Scheduler：一次工具调用的一生

单次执行在 `pi/tools/scheduler.go` 的 `Execute`：

```go
func (s *Scheduler) Execute(ctx context.Context, call schema.ToolCall) (Event, error) {
	if err := ctx.Err(); err != nil {
		return Event{}, err
	}

	// 执行前先查找工具
	toolEntry, ok := s.registry.lookup(call.Name)
	if !ok {
		return NewRejectedEvent(
			call, pierrors.ErrToolResourceNotFound, fmt.Sprintf("tool %q is not registered", call.Name),
		), nil
	}

	if err := toolEntry.validateArgs(call.Arguments); err != nil {
		return NewRejectedEvent(call, pierrors.ErrToolInvalidArguments, err.Error()), nil
	}

	if s.emit != nil {
		s.emit(NewStartEvent(call))
	}

	output, err := toolEntry.tool.Execute(ctx, call.Arguments, new(UpdateEmitter(func(update schema.ToolUpdate) {
		if s.emit != nil {
			s.emit(NewUpdateEvent(call, update))
		}
	})))
	if err != nil {
		return NewErrorEvent(call, err), nil
	}
	if output == nil {
		return NewErrorEvent(call, pierrors.ErrToolPanic.Wrap(errors.New("tool returned nil output"))), nil
	}

	return NewEndEvent(call, *output, false, 0), nil
}
```

一次调用的完整流程：查注册表 → Schema 校验 → 发 Start 事件 → 执行 → 发 Update 事件（长任务时工具可以边跑边吐增量）→ 包成 End 事件返回。

**最关键的设计在错误处理上**：`Execute` 的返回值 `error` 几乎永远是 nil。工具自身失败（包括执行报错、返回 nil）都被包成 `IsError` 的 End 事件，而不是 error。error 返回值只留给一种情况——调度层自己挂了（比如 ctx 已取消）。

为什么？回到主循环那段代码看：`IsError` 事件会转成一条正常的 `RoleTool` 消息回给模型。**工具失败不是异常，是给模型的输入。** 模型看到"Command exited with code 1"和输出内容，下一轮自己决定重试、换命令还是换条路。循环把"报错 → 崩溃"变成了"报错 → 重新规划"——开篇说过这是 Loop Agent 相对 DAG 最本质的弹性，落地就是这十几行。

拿内置的 `bash` 工具（`pi/tools/impl/bash.go`）验证一下这个设计：

```go
func (t *BashTool) Execute(ctx context.Context, args json.RawMessage, _ *tools.UpdateEmitter) (*schema.ToolOutput, error) {
	input, err := decodeArgs[bashArgs](args)
	if err != nil {
		return nil, err
	}

	runCtx := ctx
	if input.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, time.Duration(input.Timeout)*time.Second)
		defer cancel()
	}

	command := exec.CommandContext(runCtx, shellName(), "-c", input.Command)
	command.Dir = t.workDir
	output, runErr := command.CombinedOutput()
	text := strings.TrimRight(string(output), "\n")

	switch {
	case runErr == nil:
		if text == "" {
			return textOutput("(no output)"), nil
		}
		return textOutput(text), nil
	case runCtx.Err() != nil:
		return nil, pierrors.ErrToolTimeout.Wrap(
			fmt.Errorf("%s\n\nCommand timed out after %d seconds", text, input.Timeout))
	default:
		// 进程正常退出但退出码非零，输出照常返回给模型。
		status := "Command failed"
		if command.ProcessState != nil {
			status = fmt.Sprintf("Command exited with code %d", command.ProcessState.ExitCode())
		}

		return nil, pierrors.ErrToolRuntime.Wrap(fmt.Errorf("%s\n\n%s", text, status))
	}
}
```

命令非零退出时，返回的 error 里包着**完整的输出内容和退出码**。这些信息经 `NewErrorEvent` 变成一条 `IsError` 消息回到模型——也就是说，命令失败后，模型看到的是自己那条命令的真实输出，它完全有能力据此修正下一条命令。如果你在工具实现里把失败吞掉或者只返回一句 "command failed"，模型就瞎了。

`bash` 的定义里 `ParallelSafe: false`，而 `read` 是 `true`——读文件随便并发，写文件和跑命令不行。这个字段就是给下一节的调度器用的。

### 6. ExecuteBatch：分波并发

模型一次响应可以带多个 ToolCalls（并行调用），调度器按"波次"（wave）切分：

```go
func (s *Scheduler) ExecuteBatch(ctx context.Context, calls schema.ToolCalls) ([]Event, error) {
	results := make([]Event, len(calls))

	for start := 0; start < len(calls); {
		end := start + 1
		if s.registry.ParallelSafe(calls[start].Name) {
			for end < len(calls) && s.registry.ParallelSafe(calls[end].Name) {
				end++
			}
		}

		if err := s.executeWave(ctx, calls, results, start, end); err != nil {
			return results, err
		}
		start = end
	}

	return results, nil
}
```

规则：连续若干个 `ParallelSafe` 的工具合成一波并发跑；不可并发的各自独占一波，串行执行。波内用信号量限流（`maxParallel` 默认 4）：

```go
semaphore := make(chan struct{}, limit)
var waitGroup sync.WaitGroup
for index := start; index < end; index++ {
	call := calls[index]
	waitGroup.Add(1)
	go func() {
		defer waitGroup.Done()

		select {
		case semaphore <- struct{}{}:
		case <-ctx.Done():
			return
		}
		defer func() { <-semaphore }()
		if ctx.Err() != nil {
			return
		}

		results[index], executionErrors[index-start] = s.Execute(ctx, call)
	}()
}
waitGroup.Wait()
```

结束事件按原始下标写回 `results`，和 `calls` 一一对齐——这个对齐很重要，工具结果回填消息序列时靠的是下标对应，乱了就张冠李戴。

后面运行输出里正好有一个真实例子：模型在同一轮发起了 `write` 和 `bash` 两个调用。两者都不是 `ParallelSafe`，于是各占一波、按声明顺序执行——先写文件，再列目录。如果模型发的是"read 三个文件"，它们会在一波里并发跑完。

### 7. 事件：让循环看得见

工具执行全程通过 `EventObserver` 外发事件（`pi/tools/event.go`），三个阶段：

```go
type Event struct {
	Phase     EventPhase // EventStart / EventUpdate / EventEnd
	Call      schema.ToolCall
	Update    *schema.ToolUpdate
	Content   schema.ContentBlocks
	Details   any
	IsError   bool
	ErrorCode int
}
```

`Event.ResultMessage()` 把结束事件转成回填上下文的 `schema.Message`（复制内容，隔离后续修改）。观察者拿到的就是终端展示和审计要用的东西，`cmd/harness` 里就一个 switch：

```go
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
```

### 8. 装配：NewAgent 把所有件拼起来

`pi/agent.go` 是装配入口，装配关系就一屏代码：

```go
provider, err := providers.New(opts.ProviderOptions)

// 获取 tools 下面的默认工具
newTools := impl.NewDefaultTools(opts.WorkDir)
if len(newTools) > 0 {
	opts.Tools = append(opts.Tools, newTools...)
}

// 注册工具
registry, err := tools.Register(opts.Tools)
registry.Freeze()

loop := NewLoop(
	provider,
	WithScheduler(tools.NewScheduler(registry, maxParallel, opts.Observer)),
	WithTextObserver(opts.TextObserver),
	WithMaxTurns(opts.MaxTurns),
)

return &Agent{loop: loop, contextBuilder: NewContextBuilder(opts.WorkDir)}, nil
```

`Agent.Run` 做两件事：`prepareRunContext` 把业务消息转成模型消息、用 `ContextBuilder` 组装上下文（system 块、历史消息、本轮输入排好序），然后把组装结果交给 `loop.run`。工具定义交给上下文组装去排序，循环只消费组装结果——职责到此切干净。

`Run` 的返回值值得注意：运行失败时 `RunOutput` 一样有效，`Messages()` 返回出错前的完整消息序列，调用方能看到模型跑到哪一步才出的问题。

## 跑起来看

`cmd/harness` 是最小可运行示例，给它一个真实任务：

```bash
go run ./cmd/harness -workdir /tmp/go-harness-demo \
  -prompt "先用 bash 列出当前目录内容，然后把字符串 hello from go-harness 写进 demo.txt，最后读回来确认。"
```

真实运行输出（DeepSeek / deepseek-chat）：

```text
=== deepseek (openai / deepseek-chat) ===
workdir: /tmp/go-harness-demo

I'll list the directory, write the file, then read it back.
  → bash {"command": "ls -la"}
  ← bash ok
Empty directory. Now writing the file and reading it back.
  → write {"content": "hello from go-harness\n", "path": "demo.txt"}
  → bash {"command": "ls -la"}
  ← write ok
  ← bash ok

=== 消息序列 ===
[user] 先用 bash 列出当前目录内容，然后把字符串 hello from go-harness 写进 demo.txt…
[assistant] I'll list the directory, write the file, then read it back.
            └ call bash {"command": "ls -la"}
[tool:bash] result: total 0 …
[assistant] Empty directory. Now writing the file and reading it back.
            └ call write {"content": "hello from go-harness\n", "path": "demo.txt"}
            └ call bash {"command": "ls -la"}
[tool:write] result: Successfully wrote to demo.txt (22 bytes)
[tool:bash] result: total 8 …
[assistant] Now reading it back:
            └ call read {"path": "demo.txt"}
[tool:read] result: hello from go-harness
[assistant] 三步都完成了：…
```

对照流程图看这个 transcript，每一轮就是图里的一条竖线：模型说要调工具 → 调度器执行 → 结果回填 → 再问模型。第三轮里 `write` 和 `bash` 同时出现在一条 assistant 消息里，但执行是按波的——write 先落盘，bash 后列目录，顺序没有被并发打乱。

## 总结

回到开头的三个层。循环层（`loop.go`）只有一个 `for`，退出条件是模型不再调工具，`maxTurns` 是防烧钱的保险丝；模型消息先入列、工具结果紧随其后，协议顺序在这里被守住。

调度层（`tools/`）把"工具失败"从异常变成了给模型的输入：注册期编译好 Schema 校验，执行期失败包成 `IsError` 消息回填，模型自己决定下一步；只有调度层自身的失败才中断运行。`ParallelSafe` 声明 + 分波调度，让"能并发的并发、不能并发的保序"。

装配层（`agent.go`）把 Provider、Registry、Scheduler、Loop 拼成 `Agent`，业务侧一行 `agent.Run()` 跑完整个循环。

到这里，模型已经能真的改文件、跑命令了。但它跑得越久，上下文涨得越快——每轮的模型消息和工具输出都在 `messages` 里滚雪球。下一篇写上下文工程：`pi/context` 怎么组装 System Prompt、`LimitText` 这类输出限流怎么接线，以及上下文超限后的压缩策略。感兴趣的话关注一下，防止走丢。