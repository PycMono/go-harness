# pi/context

Run 上下文组装：把调用方输入、历史与工作区素材装配成一次模型调用所需的上下文。

- `ContextBlock`：调用方注入的上下文块，置于会话历史之前。
- System Prompt 组装：AGENTS.md 内容、Skills 概要、工具说明的拼接与排序。
- 历史剪枝（prune）与压缩（compaction）：按阈值比例裁剪旧消息、用摘要换回预算，内部默认值由单元测试断言，未证明需要调参前不暴露为配置项。

本包只依赖 `pi/ai`（以及 `pi/skills`），不做模型调用决策之外的事；Loop 编排归 Agent Core。