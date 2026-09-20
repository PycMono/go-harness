# pi/schema

与模型平台无关的公共词汇包：消息、内容块、工具 Schema、用量与流事件。

- `protocol.go` —— 消息侧
  - 角色与结束原因：`Role`、`FinishReason`。
  - 消息：`Message` / `Messages`；`Validate` 在入口边界校验角色、tool 消息的 `ToolCallID` 与内容块；`ToOpenAIMessages` / `ToAnthropicMessages` 完成双协议转换。
  - 内容块：`ContentType`（text / image）、`ContentBlock` / `ContentBlocks`，含校验、克隆、图片脱敏占位。
  - 用量与事件：`Usage`（Token、价格、成本、延迟，价格与成本由业务层填充）、`StreamEvent`。
- `tools.go` —— 工具侧
  - 工具描述：`ToolDefinition` / `ToolDefinitions`，与模型平台无关；`InputSchema` 使用 JSON Schema，构造前完成校验与归一化。
  - 调用参数：`ToolCall` / `ToolCalls`，模型发起的一次或一批工具调用请求，参数保持未解析状态，由具体工具负责解析。
  - 执行返回值：`ToolOutput`（一次执行的返回值）、`ToolUpdate`（执行中的增量返回值）；只约定形状，截断等策略归 `pi/tools`。
  - 双协议转换：`ToOpenAITools` / `ToAnthropicTools` 把统一描述翻译成各平台 SDK 的请求格式。

依赖方向：本包是 SDK 的底层词汇包，不依赖 `pi` 内任何其他包；`pi/ai`（Provider 接口）、`pi/tools` 与上层组装代码都依赖本包。
