# pi/schema

与模型平台无关的公共词汇包：消息、内容块、工具 Schema、用量与流事件。

- `message_variants.go` —— 消息的四元判别联合：`Message` 接口与 `SystemMessage` / `UserMessage` / `AssistantMessage` / `ToolResultMessage` 四个具体类型，各自的构造函数与本地校验。
- `message_json.go` —— 消息的 JSON 编解码（`MarshalJSON` / `DecodeMessage` / `Messages` 的编解码），线格式与旧结构体逐字节一致。
- `protocol.go` —— 消息序列与双协议转换
  - 角色与结束原因：`Role`、`FinishReason`。
  - 消息序列：`Messages`；`Validate` 在入口边界校验角色、tool 消息的 `ToolCallID` 与内容块；`ToOpenAIMessages` / `ToAnthropicMessages` 完成双协议转换。
  - 内容块：`ContentType`（text / image）、`ContentBlock` / `ContentBlocks`，含校验、克隆、文本投影与图片脱敏占位。
  - 用量与事件：`Usage`（Token、价格、成本、延迟，价格与成本由业务层填充）、`StreamEvent`。
- `tools.go` —— 工具侧
  - 工具描述：`ToolDefinition` / `ToolDefinitions`，与模型平台无关；`InputSchema` 使用 JSON Schema，构造前完成校验与归一化。
  - 调用参数：`ToolCall` / `ToolCalls`，模型发起的一次或一批工具调用请求，参数保持未解析状态，由具体工具负责解析。
  - 执行返回值：`ToolOutput`（一次执行的返回值）、`ToolUpdate`（执行中的增量返回值）；只约定形状，截断等策略归 `pi/tools`。
  - 双协议转换：`ToOpenAITools` / `ToAnthropicTools` 把统一描述翻译成各平台 SDK 的请求格式。

文本投影与图片规则：

- `ContentBlocks.Text()` 是严格的：遇到图片或未知块就报错，只给真正需要纯文本的调用方。
- 允许降级的路径（摘要、日志、终端显示）用 `WithImagePlaceholders()` 把图片块换成 `ImagePlaceholderText` 的脱敏占位（只留 scheme/host/path）。转换路径不用它：Anthropic 的 `tool_result` 原生携带图片成员；OpenAI 的 tool 消息只有文本位置，选词按 pi.dev 的三路规则——有文本用文本、无文本有图片用 `"(see attached image)"`、两者都没有用 `"(no tool output)"`。
- 两条图片规则不是同一条，不得混为一谈：输入 Prompt 的图片限制（`pi/runner.go` 的 `Message2AI`：只有 customer 输入可带图，且每条至多 `MaxImagesPerMessage` 张）是业务入口的策略；消息联合的图片规则（user 与 tool-result 消息可携带图片，assistant 与 system 不可，由各自 `Validate` 拒绝）是消息类型自身的合法性契约。

依赖方向：本包是 SDK 的底层词汇包，不依赖 `pi` 内任何其他包；`pi/ai`（Provider 接口）、`pi/tools` 与上层组装代码都依赖本包。
