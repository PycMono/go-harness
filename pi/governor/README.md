# pi/governor

Run 治理域：约束一次 Run 消耗的资源，并在跨过红线时给出可分类的终止。

- `Limits`：资源上限声明（轮次、Token）。
- `Governor`：预算累计与准入——每次模型调用/工具批次前判定是否放行。
- `Termination`：终止分类，区分正常完成、上限触达与检测到异常循环。
- `Invocation`：模型调用计量（Thinking/Compaction/Action 各类调用的 Usage 与耗时）。
- ctx 管道件：在父子 Run（子代理）之间传递这些治理原语。

本包可以 import `pi/loopdetect` 以识别循环类 typed Error；反向依赖禁止。