# pi/mcp

经 MCP（Model Context Protocol）接入外部工具。

- 以 `pi/extension` 的 `Extension` 形态接入：启动期连接 MCP server，把远端工具经 `API` 注册进 SDK。
- 支持的传输方式：stdio、HTTP（SSE/Streamable）等。
- 连接生命周期由本包管理，工具调用转发到对应 server 并回传结构化结果。

本包依赖 `pi/extension`、`pi/ai`，不依赖上层组装代码。