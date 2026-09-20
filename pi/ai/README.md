# pi/ai

Provider 抽象：屏蔽不同模型平台的差异，SDK 最底层的接口包。

- 统一 `Provider` / `Stream` 接口：`Provider.Stream` 接收 `schema.Messages` 与 `schema.ToolDefinitions`，返回按顺序消费的 `Stream`；`providers/` 子包承载具体平台（如 OpenAI、Anthropic）的官方 SDK 适配器与配置。
- 消息模型、内容块、工具定义、`Usage` 与流事件的公共表示由 `pi/schema` 承载，本包只做接口，不定义数据。

依赖方向：`pi/schema` 是 SDK 的底层词汇包，不依赖任何内部包；本包只依赖 `pi/schema`，不依赖任何模型 SDK 内部结构。`providers/` 与上层组装代码依赖本包。
