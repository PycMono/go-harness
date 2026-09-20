# pi/error

SDK 全域唯一的错误码与错误值框架。

- 稳定错误值只定义在本包：`CodeError{code, msg}` + 各 `ErrX` 哨兵，配套 `CodeOf` 取码。
- 需要跨包识别的领域错误类型（如 loopdetect 的循环错误、governor 的限额错误）随各自领域包定义并实现 `Code() int`，不集中到本包（会引入包循环）。
- 业务/HTTP 层的业务错误码不属于本包，由上层业务自行定义。

引用别名统一为 `pierrors`。