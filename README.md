# go-harness

Pi 风格的 Agent Core/Harness SDK：`pi/schema` 提供与平台无关的消息词汇，`pi/ai` 在其上提供统一 Provider 抽象，其余目录按域拆分为工具执行、Run 治理、会话持久化、沙箱与观测等能力，由上层业务组合成完整的 Agent 运行时。

## 目录结构

```text
pi/
├── ai             统一 Provider 抽象
│   └── providers  模型平台适配器（OpenAI、Anthropic 等）与 Provider 配置
├── error          全域稳定错误码与错误值框架
├── extension      扩展契约：启动期注册 Tool，参与停机清理
├── governor       Run 治理：资源上限、预算准入、调用计量与终止分类
├── loopdetect     请求级工具行为循环检测
├── mcp            经 MCP 协议接入外部工具
├── middleware     Tool 执行链中间件机制与内置 Handler
├── observability  追踪、用量计量与语义约定
├── sandbox        本地命令执行边界与隔离后端
├── schema         与平台无关的消息、内容块、工具 Schema 与 Usage 词汇
├── session        会话存储（文件 / 内存）与模型上下文重建
├── skills         SKILL.md 发现、解析与 Prompt 渲染
└── tools          内置默认工具与工具执行域：Registry、Runtime、edit、exec 等
```

Agent Core（唯一 Agent 类型、Loop、Run 契约、EventListener/Notifier、子代理与装配入口）是 `pi` 包根下的代码，不单独成目录。

## 依赖方向

- `pi/ai`（及其 `providers` 子包）是最底层公共包，不依赖其他任何包。
- 域包只依赖 `pi/ai` 和自己的子包（`loopdetect` 额外允许被 `governor` 引用），不反向依赖 `pi` 包根的 Agent Core。`pi/session` 只依赖 `pi/schema` + `pi/error`，不认识 loop、agent、provider。
- Agent Core 依赖各域包做最终装配；上层业务从自己的配置中选择平台与工作目录，经装配入口组合成 `Runner`。