# pi/middleware

Tool 执行链的中间件机制与内置 Handler。

- `Handler`：围绕单次工具调用的洋葱式中间件契约；`Defaults()` 返回默认集合，切片顺序即执行顺序。
- 内置 Handler：Tracing（最外层包住整条链）、PanicRecovery、SchemaValidation、Logging、事件转发、权限拦截。
- 未注册的 Tool 在 `pi/tools` 的 Runtime 入口即返回，不经过本链，也不创建执行 Span。

自定义横切逻辑（限流、审计、重试等）通过实现 `Handler` 插入执行链。