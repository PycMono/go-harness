# pi/tools

内置默认工具集与工具执行域。

- 工具注册生命周期：`Registry` 管理工具的注册与查找。
- 执行：`Scheduler` 负责单次调用与批量调度，参数先按工具自己的 JSON Schema 校验；`Event` 表达执行生命周期。中间件链（`pi/middleware`）尚未实现。
- 内置工具（`pi/tools/impl`）：`read`、`write`、`edit`、`bash`。待做：`ls`、`apply_patch`（含补丁解析器）。
- 输出控制：`LimitText` 在 `schema.ToolOutput` 上做截断，策略（提示文案、额度语义、`truncated` 详情）留在本包，`pi/schema` 只约定返回值形状。当前没有调用方，内置工具尚未接上统一的字节预算。

扩展与 MCP 经 `pi/extension` 把自定义工具注册进这里的 `Registry`。

依赖方向：本包依赖 `pi/schema`（消息内容块、Tool Schema 与调用参数）、`pi/sandbox` 与 `pi/error`。