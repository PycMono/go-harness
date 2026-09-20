package tools

import (
	pierrors "github.com/PycMono/go-harness/pi/error"
	"github.com/PycMono/go-harness/pi/schema"
)

// Event 是 Tool 执行的生命周期事件
type Event struct {
	Phase     EventPhase
	Call      schema.ToolCall
	Update    *schema.ToolUpdate
	Content   schema.ContentBlocks
	Details   any
	IsError   bool
	ErrorCode int
}

func NewEndEvent(
	call schema.ToolCall,
	output schema.ToolOutput,
	isError bool,
	errorCode int,
) Event {
	return Event{
		Phase:     EventEnd,
		Call:      call,
		Content:   output.Content,
		Details:   output.Details,
		IsError:   isError,
		ErrorCode: errorCode,
	}
}

// NewStartEvent 开始事件
func NewStartEvent(call schema.ToolCall) Event {
	return Event{Phase: EventStart, Call: call}
}

func NewUpdateEvent(call schema.ToolCall, update schema.ToolUpdate) Event {
	return Event{Phase: EventUpdate, Call: call, Update: &update}
}

// NewErrorEvent 把工具返回的错误归一化成一条 IsError 结束事件：错误码取自
// 错误链（CodeOf），未分类的错误取 0，由调用方按 ErrToolRuntime 兜底。
func NewErrorEvent(call schema.ToolCall, err error) Event {
	return NewEndEvent(call, schema.ToolOutput{
		Content: []schema.ContentBlock{schema.TextBlock(err.Error())},
	}, true, pierrors.CodeOf(err))
}

// EventObserver 接收执行过程中产生的生命周期事件（开始与增量）。结束事件由
// 调用方从返回值取得；批量执行时会被多个 goroutine 并发调用，实现须自行保证
// 并发安全。
type EventObserver func(Event)

// ResultsMatchCalls 校验合并后的结束事件与原始调用的长度、ID、工具名
// 一一对齐。
func ResultsMatchCalls(calls schema.ToolCalls, events []Event) bool {
	if len(calls) != len(events) {
		return false
	}
	for index := range calls {
		if events[index].Phase != EventEnd ||
			calls[index].ID != events[index].Call.ID || calls[index].Name != events[index].Call.Name {
			return false
		}
	}
	return true
}

// NewRejectedEvent 构造一条确定性合成的 IsError 工具结束事件。
func NewRejectedEvent(call schema.ToolCall, codeErr *pierrors.CodeError, text string) Event {
	return NewEndEvent(call, schema.ToolOutput{
		Content: []schema.ContentBlock{schema.TextBlock(text)},
	}, true, codeErr.Code())
}

// ResultMessage 将工具结束事件转换为模型消息，复制内容以隔离后续修改。
func (event Event) ResultMessage() schema.Message {
	return schema.Message{
		Role:       schema.RoleTool,
		Content:    event.Content.Clone(),
		ToolCallID: event.Call.ID,
		ToolName:   event.Call.Name,
		IsError:    event.IsError,
	}
}
