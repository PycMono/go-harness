# pi/middleware

Tool 执行链的内置 Handler 实现。

- 契约（`Handler` / `Execution` / `Block` 语义与两条铁律的强制校验）定义在 `pi/tools`（`execution.go`），本包只放实现，依赖方向 pi/middleware → pi/tools。
- `Defaults()` 返回默认集合，切片顺序即执行顺序：`Logging`（最外层，记录开始与结束）→ `RetryTransient`（居中，超时重试 3 次，只重跑更靠内的部分）→ `PanicRecovery`（最内层，兜住每次尝试的工具 panic 并 Block；`ErrToolPanic` 不命中重试条件）。
- `Retry(maxAttempts, retryable)` 是按需接入的构造器（`RetryTransient` 只重试工具超时），基于 [retry-go/v4](https://github.com/avast/retry-go)（默认退避 + 抖动间隔，`LastErrorOnly` 保证结束错误仍是原始错误链），不进 Defaults——是否重试是调用方的策略。
- 待做：Tracing（最外层包住整条链）、SchemaValidation、事件转发（`UpdateObserver` 已预留）、权限拦截。
- 未注册的 Tool 在 `pi/tools` 的 Runtime 入口即返回，不经过本链，也不创建执行 Span。

自定义横切逻辑（限流、审计等）通过实现 `tools.Handler` 插入执行链。链由装配处（`pi/agent.go`）注入 Scheduler。