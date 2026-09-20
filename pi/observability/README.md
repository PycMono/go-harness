# pi/observability

Run 与工具执行的观测：追踪、用量计量与语义约定。

- Tracing provider：接入 OpenTelemetry，产出 Run / 模型调用 / 工具执行各层的 Span。
- 用量计量：为每次模型调用固化 Token 计量、TTFT 与延迟，并强制 Usage 存在、token 非负（价格与成本换算归业务层）。
- 语义约定（semantics/hint）：Span 命名、属性键与提示语义的统一定义，避免各处手写字符串。

业务可替换/扩展 provider 实现以对接自己的观测后端。