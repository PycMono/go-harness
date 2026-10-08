# pi/observability

Run 与工具执行的观测：追踪、用量计量与 Span 命名/属性键的统一定义。

## 三层装饰

装配顺序固定，只有 `Wrap` 一处写：

```
Loop → TracingProvider → UsageMeter → 原始 Provider
```

- **TracingProvider**：给每次物理模型请求开一个 `chat {model}` Span。它套在计量层
  外面，Span 上的 token 属性来自内层补好的 Usage——顺序反了就读不到。
- **UsageMeter**：为每次模型调用固化 Token 计量、TTFT 与延迟，并强制 Usage 存在、
  token 非负。价格与成本换算归业务层。

没装 OTel Provider 时两层都是透传，代价是两次函数调用。

## Span 分层

一次运行在追踪后端上是一棵树：

```
pi.run                      整次 Run：准备上下文 → 每轮 → 会话写入判定
└── pi.turn                 一轮：交付插话 → 压缩 → 调模型 → 执行工具 → 循环检测
    ├── chat {model}        一次物理模型请求（压缩摘要也是它）
    └── execute_tool {name} 一次真实工具执行
pi.compact_context          一次上下文压缩，挂在触发它的那一轮底下
```

`pi.run` 从 `prepareRunContext` 之前就开，到会话写入失败的判定为止；闭包的返回值
与 `Run` 的返回值是同一件事——`Run` 返回错误 ⇔ 这个 Span 是红的。三个入口校验
（`closed`、`input == nil`、`Input.Validate`）在 Span 之外：调用方写错了参数，
这一次运行根本没开始。

`pi.turn` 只括住真正跑起来的那一轮，轮首的两处退出判定（ctx 取消、maxTurns）落在
轮与轮之间、不属于任何一轮。

`pi.compact_context` 开在 `compactBeforeTurn` 的两个提前返回之后——每轮都会调它，
第一个检查 `OverLine` 在绝大多数轮次上直接返回，开在函数顶上等于每轮留一个空 Span。
压不动时它照样标红，而这一轮照常跑下去：压不动不算运行失败（历史还是完整的，只是
继续贴着窗口跑，真溢出了让 provider 报 20003），但 Span 必须红，否则它永远是绿的，
而它存在的全部意义正是让人看见"压缩没生效"。

## 不做的两件事

- **不记正文**。提示词、模型输出、工具参数与输出的正文都不上 Span——它们可能很长，
  也可能带用户数据。工具调用只记参数与输出的字节数，模型请求只记事件的条数。
- **不碰金额**。`CostUSD` 与 `*PriceUSDPerMillionTokens` 在 Span 上一个都不出现，
  换算归业务层。

错误一律以 `pi/error` 的稳定整数码上 Span（`error.type` 与 `pi.error.code`），不带
错误正文。

Span 名称与属性键只在 `semantics.go` 里写一次，业务代码不手写字符串。

## 接入

业务可替换/扩展 provider 实现以对接自己的观测后端。接后端（OTLP exporter）是
`cmd/harness` 里一个 main 函数级的装配，不在本包内。
