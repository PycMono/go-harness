package schema

// 本文件承载消息联合的只读读取入口。Role() 已经回答"这条消息是什么角色"，这里补上
// 它的内容对应物：展示、降级投影、日志这些不关心具体变体、只要拿到内容块的路径。

// ContentOf 返回任意消息变体的内容块。只读展示、降级投影和日志等不关心
// 具体变体的路径用它，避免每个调用点各写一遍类型分支。
func ContentOf(message Message) ContentBlocks {
	switch typed := message.(type) {
	case *SystemMessage:
		return typed.Content
	case *UserMessage:
		return typed.Content
	case *AssistantMessage:
		return typed.Content
	case *ToolResultMessage:
		return typed.Content
	default:
		// 接口为 nil 就是没有载荷（header entry 的空消息就是这样），没有内容可给。
		return nil
	}
}
