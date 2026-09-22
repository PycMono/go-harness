# 消息协议 v2 重构方案

目标：一次性将 go-harness 的消息模型切换为接近 pi.dev 的四元判别联合，并同步调整 Provider、Loop、Session 和 Prompt 输入边界，不保留统一大结构体的过渡形态。

本方案是一次明确的破坏性升级。运行时使用 v2 消息协议；旧 JSONL 文件通过一次性迁移工具转换，或者由 v2 明确拒绝，不在正常读取路径中长期维护两套协议。

## 最终架构

消息类型：

    type Message interface {
        Role() Role
        Validate() error
        message()
    }

    type SystemMessage struct {
        Content ContentBlocks
    }

    type UserMessage struct {
        Content ContentBlocks
    }

    type AssistantMessage struct {
        Content      ContentBlocks
        Usage        *Usage
        FinishReason FinishReason
        ToolCalls    ToolCalls
    }

    type ToolResultMessage struct {
        Content    ContentBlocks
        ToolCallID string
        ToolName   string
        IsError    bool
    }

具体类型通过 Role 方法返回角色，不再保存同名 Role 字段。四种类型分别拥有自己的字段，非法跨角色字段无法通过构造函数和 Validate。

消息序列：

    type Messages []Message

Provider 只负责生成 AssistantMessage；UserMessage、SystemMessage 由上下文和输入边界生成；ToolResultMessage 由工具调度器生成。

## 破坏性变更

- schema.Messages 从 []*Message 改为 []Message。
- ai.Provider 的 Stream.Result 从返回 schema.Message 改为返回 *schema.AssistantMessage。
- providers/stream.go、OpenAI、Anthropic 和所有调用方同步迁移。
- Session Entry.Message 改为 Message 接口，并实现自定义 JSON 编解码。
- Session 文件格式版本升级到 v2。
- 旧 v1 文件不在正常打开路径中自动兼容；提供一次性迁移命令，或返回明确的“需要迁移”错误。
- 当前磁盘协议继续使用 role=tool，不能改成 Pi 内部使用的 toolResult。

## 任务一：定义四元判别联合

文件：

- 修改 pi/schema/protocol.go
- 新增 pi/schema/message_variants.go
- 新增 pi/schema/message_variants_test.go

步骤：

- [ ] 定义 Message 接口、SystemMessage、UserMessage、AssistantMessage、ToolResultMessage。
- [ ] 为四种类型实现 Role、Validate 和私有 message 方法。
- [ ] 增加 NewSystemMessage、NewUserMessage、NewAssistantMessage、NewToolResultMessage 构造函数。
- [ ] UserMessage 和 ToolResultMessage 允许图片；AssistantMessage 和 SystemMessage 不允许图片。
- [ ] ToolResultMessage 必须同时拥有 ToolCallID 和 ToolName。
- [ ] 删除统一 Message 结构体及其跨角色可选字段。
- [ ] 为四种类型增加单元测试，验证非法字段组合无法构造或通过校验。

## 任务二：实现 v2 JSON 编解码

文件：

- 新增 pi/schema/message_json.go
- 修改 pi/session/entry.go
- 修改 pi/session/file.go
- 新增 schema 和 session JSON 测试

固定接口：

    func (m Messages) MarshalJSON() ([]byte, error)
    func (m *Messages) UnmarshalJSON(data []byte) error
    func DecodeMessage(data []byte) (Message, error)
    func (e *Entry) UnmarshalJSON(data []byte) error

步骤：

- [ ] 每个具体消息类型实现 MarshalJSON，输出 role、content、usage、finish_reason、tool_calls、tool_call_id、tool_name、is_error 等 v2 字段。
- [ ] DecodeMessage 先读取 role，再解码为四种具体类型；未知 role 直接报错。
- [ ] Messages 实现数组编解码，不能让 encoding/json 直接尝试填充非空接口。
- [ ] Entry 实现自定义 UnmarshalJSON，将 message 字段交给 DecodeMessage。
- [ ] Entry.validate 改为检查 Message 非空并调用 Message.Validate。
- [ ] Entries.messagesOf 改为按 Message.Role 和具体类型读取，不再访问统一结构体字段。
- [ ] sessionVersion 升到 2，并在文件加载时校验版本。
- [ ] 增加 v1 → v2 一次性迁移工具或明确的迁移错误路径，禁止正常读取路径隐式兼容两种格式。

## 任务三：整理内容块和图片规则

文件：

- 修改 pi/schema/protocol.go
- 修改所有 Content.Text 调用点
- 修改 pi/schema/README.md

步骤：

- [ ] 保留 Content.Text 的严格纯文本语义：遇到图片时返回错误。
- [ ] 增加显式的图片占位文本投影函数，供摘要、日志和只接受文本的 Provider 路径使用。
- [ ] 逐一检查全仓 Content.Text 调用，区分必须纯文本和允许图片降级的场景。
- [ ] ToolResultMessage 保留图片内容，不因统一文本投影而丢失图片。
- [ ] 图片模型改为 Pi 风格的内联数据：ImageContent 至少包含 base64 Data 和 MIMEType；不把 Path、Name、AttachmentID 放入 schema 消息。
- [ ] 普通文件不作为消息附件直接传给模型，文件通过工作目录和工具读取。

## 任务四：迁移 Provider 接口和适配器

文件：

- 修改 pi/ai/provider.go
- 修改 pi/ai/providers/stream.go
- 修改 pi/ai/providers/openai.go
- 修改 pi/ai/providers/anthropic.go
- 修改相关 Provider 测试

步骤：

- [ ] 将 Stream.Result 改为返回 AssistantMessage，Provider 不再返回通用 Message。
- [ ] OpenAI 和 Anthropic 按具体消息类型转换 System、User、Assistant、ToolResult。
- [ ] Anthropic 的 tool_result 保留支持的文本和图片内容块。
- [ ] OpenAI 工具结果图片按 pi.dev 行为投影：tool 消息发送文本；纯图片使用“see attached image”占位；空结果使用“no tool output”；模型支持图片时追加合成 user 消息携带图片。
- [ ] 增加 OpenAI 角色顺序回归测试：assistant、tool、tool、user。
- [ ] Provider 不支持图片时使用明确的占位或合成消息策略，不能静默丢弃内容。
- [ ] 运行 go test ./pi/ai/... -count=1。

## 任务五：迁移 Loop、工具调度和 Agent

文件：

- 修改 pi/loop.go
- 修改 pi/tools/event.go
- 修改 pi/context.go
- 修改 pi/agent.go
- 修改 pi/runner.go
- 修改相关测试

步骤：

- [ ] Loop 接收 Messages，Provider 返回 AssistantMessage。
- [ ] Loop 将 AssistantMessage 追加到消息序列。
- [ ] 工具调度结果构造 ToolResultMessage，并保留调用 ID、工具名称和错误标记。
- [ ] 保持消息顺序：system → user → assistant(tool_calls) → tool → assistant(final)。
- [ ] Agent.history 保持现有先读历史、再追加当前输入的顺序，不重新设计 Session 流程。
- [ ] 输入边界保留现有规则：正文不能为空、sender 必须是 customer、单条最多四张图片、只有用户输入允许图片。
- [ ] 将原有 Message2AI 的规则迁移到新的 UserMessage 和 Prompt 输入构造路径。
- [ ] context.go、agent.go、loop.go 中所有消息字段访问改为 Role 方法、具体类型断言或辅助函数。

## 任务六：采用 Pi 风格的 RunInput

文件：

- 修改 pi/runner.go
- 修改 pi/agent.go
- 修改相关 Agent 测试

目标接口：

    type RunInput struct {
        Prompt string
        Images []schema.ImageContent
    }

说明：

- Prompt 是当前轮文本输入，由调用方准备。
- Images 是当前轮图片，使用 base64 Data 和 MIMEType；不引入通用 Attachment。
- schema.Messages 不进入 RunInput。
- 普通文件通过工作目录和工具读取。

步骤：

- [ ] 添加 Prompt、Images 的输入校验。
- [ ] 将 Prompt 和 Images 转换为单条 UserMessage。
- [ ] 复用现有 Agent.history，确保当前输入只追加一次。
- [ ] 删除旧的业务 Message、SenderType、Message2AI 入口，或将它们一次性迁移为 Prompt 构造辅助函数。
- [ ] 迁移所有调用方和命令行示例。

## 任务七：全仓迁移和清理

必须扫描：

- pi/schema
- pi/ai
- pi/session
- pi/agent.go、pi/context.go、pi/loop.go、pi/runner.go
- pi/tools
- cmd/harness/main.go
- cmd/skilltest/main.go
- cmd/provider/main.go
- cmd/sessiontest/main.go
- 所有测试文件

步骤：

- [ ] 搜索并迁移所有 Role、Content、Usage、FinishReason、ToolCalls、ToolCallID、ToolName、IsError 字段访问。
- [ ] 搜索并迁移 []*schema.Message、append([]*schema.Message 和旧统一结构体构造。
- [ ] 搜索四个具体类型的构造函数，确认所有消息创建都有明确角色边界。
- [ ] 更新 Session、Provider、命令行示例和测试中的类型签名。
- [ ] 删除旧统一 Message、旧 Message2AI 路径和不再使用的兼容代码。

## 验收标准

- 消息是四元真正判别联合，不是统一大结构体包装。
- Go 代码可以编译：没有字段与 Role 方法同名，也没有给接口挂 JSON 方法。
- Provider 只返回 AssistantMessage。
- Session 通过 DecodeMessage、Messages.UnmarshalJSON、Entry.UnmarshalJSON 正确处理接口消息。
- Session 文件版本升级到 v2，旧文件走显式迁移或明确拒绝。
- 工具结果允许图片；Anthropic 保留图片，OpenAI 按 Pi 规则降级。
- Content.Text 的严格语义没有被破坏。
- Agent.history 的先读后写顺序保持不变。
- Prompt 和图片是 RunInput 的独立输入，schema.Messages 不暴露给调用方。
- 普通文件不进入消息 schema，通过工具读取。
- 全仓测试通过：go test ./... -count=1。
