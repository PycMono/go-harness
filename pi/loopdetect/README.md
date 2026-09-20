# pi/loopdetect

请求级的工具行为循环检测：只回答"当前工具行为是否呈现重复且无进展的模式"。

- `Detector` 是请求内策略对象：每次 Run 创建一个实例，主代理与每个子代理相互独立。
- 只在 Loop 的单线程控制流中按 `AdmitToolBatch → 工具执行 → RecordToolBatchOutcome` 的顺序调用，不保证并发安全。
- 不累计 Token、成本或轮次——那是 `pi/governor` 的职责；本包只输出可被 governor 识别为终止原因的 typed Error。

依赖方向固定：`governor` 可以 import 本包，本包不得 import `governor`。