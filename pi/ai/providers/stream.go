package providers

import (
	"github.com/PycMono/go-harness/pi/ai"
	"github.com/PycMono/go-harness/pi/schema"
)

type streamState struct {
	current  schema.StreamEvent
	started  bool
	terminal bool
	result   *schema.Message
	err      error
}

func (s *streamState) Current() schema.StreamEvent { return s.current }

func (s *streamState) Result() (*schema.Message, error) { return s.result, s.err }

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
