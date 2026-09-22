package providers

import (
	"github.com/PycMono/go-harness/pi/ai"
	"github.com/PycMono/go-harness/pi/schema"
)

type streamState struct {
	current  schema.StreamEvent
	started  bool
	terminal bool
	result   schema.Message
	err      error
}

func (s *streamState) Current() schema.StreamEvent { return s.current }

// Result 返回流结束时产出的消息与错误。result 本身就是接口字段：没有产出时它的零值
// 就是 nil 接口，所以"没结果"与"有结果"、以及"没结果"与"失败"都区分得开。
// 两条纪律守住这个契约：错误路径只回 nil 消息；任何赋值都不得把
// (*schema.AssistantMessage)(nil) 这类带类型的 nil 指针放进 result——那样接口非 nil，
// 调用方的 message == nil 判定会静默走错分支。
func (s *streamState) Result() (schema.Message, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.result == nil {
		return nil, nil
	}
	return s.result, nil
}

type failedStream struct {
	streamState
}

func newFailedStream(err error) ai.Stream {
	return &failedStream{streamState: streamState{err: err}}
}

func (s *failedStream) Next() bool {
	if s.terminal {
		return false
	}
	if s.start() {
		return true
	}
	return s.fail(s.err)
}

func (s *failedStream) Close() error { return nil }
