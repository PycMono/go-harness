# 类型化消息联合重构方案

目标：按照 pi.dev 的真实消息模型，将用户消息、助手消息、工具结果消息拆成真正的判别联合类型，而不是在统一大结构体外包一层构造函数；同时保持现有 JSONL 会话文件、Provider 转换和 Loop 行为兼容。

本方案只处理消息联合类型。Prompt 字符串入口、附件、文件路径和 base64 图片输入另开第二个方案，不在本轮混合改造。

## 目标模型

消息对外使用统一接口，具体值必须是以下类型之一：

    type Message interface {
        Role() Role
        Validate() error
        message()
    }

    type UserMessage struct {
        Role      Role
        Content   ContentBlocks
        Timestamp time.Time
    }

    type AssistantMessage struct {
        Role         Role
        Content      ContentBlocks
        Usage        *Usage
        FinishReason FinishReason
        ToolCalls    ToolCalls
        Timestamp    time.Time
    }

    type ToolResultMessage struct {
        Role       Role
        Content    ContentBlocks
        ToolCallID string
        ToolName   string
        IsError    bool
        Timestamp  time.Time
    }

Role 仍然是唯一判别字段。当前项目暂时不引入 Pi 的 provider、api、responseId、thinking、details 等字段，避免把本次重构扩展成 Provider 元数据改造。

## 全局约束

- 现有 JSONL 中的 role、content、usage、finish_reason、tool_calls、tool_call_id、tool_name、is_error 字段保持可读。
- 旧消息 JSON 不要求迁移；读取时通过 role 恢复具体消息类型。
- UserMessage、AssistantMessage、ToolResultMessage 都允许文本内容。
- UserMessage 和 ToolResultMessage 允许图片内容；AssistantMessage 不允许图片，保持当前模型输出约束。
- ToolResultMessage 必须同时拥有 ToolCallID 和 ToolName。
- Messages.Validate 负责对从 JSONL 或 Provider 边界进入的完整序列做最终校验，不能只依赖构造函数。
- Agent.history 的“先读历史、再追加当前输入”顺序保持不变。
- 本方案不新增 Attachment、Path、Data 字段，也不改变当前 ImageContent 的 URL 结构。

## 任务一：定义真正的消息联合类型

文件：

- 修改 pi/schema/protocol.go
- 新增 pi/schema/message_variants.go
- 新增 pi/schema/message_variants_test.go

步骤：

- [ ] 写测试，验证三个具体类型的 Role、字段范围和 Validate 行为。
- [ ] 写测试，验证 UserMessage 允许图片，ToolResultMessage 允许图片，AssistantMessage 拒绝图片。
- [ ] 写测试，验证 ToolResultMessage 缺少 ToolCallID 或 ToolName 时失败。
- [ ] 定义 Message 接口和三个具体消息类型。具体类型不再包含其他角色的可选字段。
- [ ] 提供 NewUserMessage、NewAssistantMessage、NewToolResultMessage 构造函数，构造函数只负责创建对应具体类型并执行一次本地校验。
- [ ] 将 Messages 改为保存 Message 接口值，而不是 []*Message 统一结构体。
- [ ] 运行 go test ./pi/schema -run 'Test(UserMessage|AssistantMessage|ToolResult|Message)'，确认专项测试通过。

## 任务二：实现兼容的 JSON 编解码

文件：

- 新增 pi/schema/message_json.go
- 修改 pi/schema/protocol.go
- 新增或修改 pi/schema/message_json_test.go

步骤：

- [ ] 为 Message 实现 MarshalJSON：按具体类型输出现有字段名和 role 值；工具结果的 role 继续使用当前磁盘协议的 tool，不改成 Pi 的 toolResult。
- [ ] 为 Message 实现 UnmarshalJSON：先读取 role，再解码为 UserMessage、AssistantMessage 或 ToolResultMessage。
- [ ] 对 system 消息做兼容处理：如果当前代码仍需要 system 消息，保留 SystemMessage 作为第四种具体类型；它不参与本轮三个角色的业务输入改造。
- [ ] 增加旧 JSONL 样例的往返测试：用户文本、用户图片、助手文本、助手工具调用、工具结果文本、工具结果图片。
- [ ] 增加脏数据测试：未知 role、工具结果缺少 ID、工具结果缺少名称、助手携带图片、错误的 tool_calls 字段组合。
- [ ] 保证旧文件读取失败时返回明确校验错误，不静默丢弃角色字段。
- [ ] 运行 go test ./pi/schema -count=1。

## 任务三：修正内容块和序列校验

文件：

- 修改 pi/schema/protocol.go
- 修改 pi/schema/README.md
- 测试 pi/schema/message_json_test.go

步骤：

- [ ] 将 ContentBlocks.ValidateForRole 的图片规则改为：UserMessage 和 ToolResultMessage 允许图片，AssistantMessage 和 SystemMessage 不允许图片。
- [ ] Messages.Validate 改为遍历具体 Message，调用每条消息的 Validate，并额外检查序列级关系。
- [ ] Messages.Validate 必须检查 ToolResultMessage 的 ToolCallID 和 ToolName。
- [ ] 保留当前 role、content、tool call 参数的通用校验。
- [ ] 文档明确：工具结果图片是合法消息内容；Prompt 入参的图片限制不能误写成所有消息的统一限制。
- [ ] 运行 go test ./pi/schema ./pi/ai/providers -count=1。

## 任务四：迁移 Provider 适配器

文件：

- 修改 pi/ai/providers/openai.go
- 修改 pi/ai/providers/anthropic.go
- 修改相关 Provider 测试

步骤：

- [ ] 将 Provider 中按 message.Role 和可选字段读取的逻辑，改为按具体消息类型或统一访问方法读取。
- [ ] 保持 UserMessage 到 OpenAI/Anthropic 用户消息的转换不变。
- [ ] 保持 AssistantMessage 的文本、Usage、FinishReason、ToolCalls 转换不变。
- [ ] ToolResultMessage 的图片内容按 Provider 能力转换；如果目标 Provider 不接受图片工具结果，返回明确错误，不静默丢图。
- [ ] 运行 OpenAI 和 Anthropic 的完整 Provider 测试。
- [ ] 运行 go test ./pi/ai/... -count=1。

## 任务五：迁移 Loop、工具事件和 Session

文件：

- 修改 pi/loop.go
- 修改 pi/tools/event.go
- 修改 pi/session/entry.go
- 修改 pi/session/manager.go
- 修改 pi/runner.go 中现有消息转换边界
- 修改相关 Loop、工具、Session 测试

步骤：

- [ ] 将模型响应构造改为 AssistantMessage。
- [ ] 将工具执行结果构造改为 ToolResultMessage，并保留调用 ID、工具名称和错误标记。
- [ ] 保持消息顺序：user → assistant(tool_calls) → tool_result → assistant(final)。
- [ ] Session Entry 改为保存 Message 接口值，并通过任务二的 JSON 编解码保持 JSONL 兼容。
- [ ] 保持 Manager.BuildMessages、Append、pathToRoot 的行为不变。
- [ ] 不重写 Agent.history；它已经实现先读历史再追加当前输入的顺序。
- [ ] 只在输入边界替换现有 Message2AI 的返回类型，并搬迁已有规则：正文不能为空、输入发送者必须为 customer、单条最多四张图片、只有用户输入允许图片。
- [ ] 运行 go test ./pi/... -count=1。

## 任务六：清理直接构造点并决定兼容边界

检查清单：

- pi/runner.go:85
- pi/context.go:44、63
- pi/tools/event.go:81
- pi/ai/providers/openai.go:165
- pi/ai/providers/anthropic.go:126
- cmd/sessiontest/main.go:249

步骤：

- [ ] 逐个判断这些构造点属于协议解码、上下文组装、工具结果、Provider 映射还是测试工具。
- [ ] 协议边界和测试工具使用角色专用构造函数；JSON 解码统一走 Message.UnmarshalJSON。
- [ ] 删除不再允许的直接跨角色字段组合。
- [ ] 保留磁盘协议中的 role=tool，不把它改成 Pi 内部的 toolResult，避免破坏已有文件。
- [ ] 运行 rg -n 'schema\\.Message\\s*\\{' pi，确保剩余调用点都有明确边界说明。
- [ ] 运行 gofmt、go test ./... -count=1、git diff --check。

## 第二个独立方案：Prompt、附件和 base64 图片

本轮明确不实现以下内容：

- RunInput.Prompt 替换现有业务 Message 的完整迁移。
- Attachment 结构体。
- Path、Name、Data 混合进入 schema。
- ImageContent 从 URL 扩展为 base64 + mimeType。
- 普通文件如何物化到工作目录。

这些内容应另开方案，先决定是否把 ImageContent 扩展为同时支持 URL 和 base64，再设计 PromptOptions 和附件生命周期。

## 验收标准

- Message 是真正的判别联合，而不是统一大结构体加构造函数包装。
- 三个具体角色类型的非法字段组合在编译或 Validate 边界被拦截。
- 工具结果允许图片，并能在 Provider 不支持时明确报错。
- ToolCallID 和 ToolName 都是工具结果的必填字段。
- 旧 JSONL 文件无需迁移即可读取。
- OpenAI、Anthropic、Loop、Tool Scheduler、Session 的现有行为不回归。
- Agent.history 的先读后写语义保持不变。
- 现有 customer、图片数量上限和输入角色校验全部保留。
- 本轮不引入 Attachment、Path、Data 或 base64 图片改造。
- go test ./... -count=1 通过。
