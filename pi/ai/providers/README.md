# pi/ai/providers

模型平台适配器与 Provider 配置。

- 平台适配器：基于各平台官方 SDK（OpenAI、Anthropic 等）实现 `pi/ai` 的统一 `Provider` 接口。
- Provider 配置：构造前完成规范化与必需字段校验；配置路径的加载、平台选择与计价（pricing）归上层业务，本包不读取配置文件、也不感知价格。

依赖方向：本包依赖 `pi/ai` 与 `pi/schema`。