# pi/extension

Agent 扩展契约与启动期注册运行时。

- `Extension`：扩展入口，在启动期经 `API` 注册 Tool。
- `API`：暴露给扩展的最小注册面，扩展只能通过它把工具接入 SDK。
- `Closer`：参与停机清理的可选接口。

依赖方向：本包只依赖 `pi/ai` 与 `pi/tools` 的执行域；`pi/mcp` 依赖本包而非上层组装代码。