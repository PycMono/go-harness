package tools

import (
	"context"
	"errors"
	"fmt"
	"sync"

	pierrors "github.com/PycMono/go-harness/pi/error"
	"github.com/PycMono/go-harness/pi/schema"
)

/*
	负责工具执行和调度
	1、并行
	2、串行
*/

// Scheduler 工具调度器
type Scheduler struct {
	registry    *Registry
	maxParallel int
	emit        EventObserver
}

// NewScheduler 构造工具调度器。emit 接收执行过程中的生命周期事件（开始与增量），
// 可为 nil 表示丢弃；它在整个 Scheduler 生命周期内固定，且会被并发调用，实现须
// 自行保证并发安全。
func NewScheduler(registry *Registry, maxParallel int, emit EventObserver) *Scheduler {
	return &Scheduler{
		registry:    registry,
		maxParallel: maxParallel,
		emit:        emit,
	}
}

// Definitions 获取工具的快照
func (s *Scheduler) Definitions() []schema.ToolDefinition {
	return s.registry.Definitions()
}

// ExecuteBatch 执行一批工具调用，返回与 calls 下标一一对齐的结束事件。
// 并发粒度按「波次」切分：连续若干个可并发的工具合成一波并发执行，不可并发的
// 工具各自独占一波串行执行。是否为可并发由工具定义里的 ParallelSafe 声明，
// 判断见 Registry.ParallelSafe。
// 返回的 error 非 nil 时 results 只是部分有效（取消前已完成的那些调用）。
func (s *Scheduler) ExecuteBatch(ctx context.Context, calls schema.ToolCalls) ([]Event, error) {
	results := make([]Event, len(calls))

	for start := 0; start < len(calls); {
		end := start + 1
		if s.registry.ParallelSafe(calls[start].Name) {
			for end < len(calls) && s.registry.ParallelSafe(calls[end].Name) {
				end++
			}
		}

		if err := s.executeWave(ctx, calls, results, start, end); err != nil {
			return results, err
		}
		start = end
	}

	return results, nil
}

// Execute 执行单次工具调用。开始与增量事件经 Scheduler 的 emit 送出，返回值是
// 本次调用的结束事件；工具自身的失败以 IsError 结束事件表达，返回的 error 只用于
// 调度器无法执行的情况（上下文已取消）。
//
// 参数先按工具自己的 JSON Schema 校验，非法参数不进入工具实现。
func (s *Scheduler) Execute(ctx context.Context, call schema.ToolCall) (Event, error) {
	if err := ctx.Err(); err != nil {
		return Event{}, err
	}

	// 执行前先查找工具
	toolEntry, ok := s.registry.lookup(call.Name)
	if !ok {
		return NewRejectedEvent(
			call, pierrors.ErrToolResourceNotFound, fmt.Sprintf("tool %q is not registered", call.Name),
		), nil
	}

	if err := toolEntry.validateArgs(call.Arguments); err != nil {
		return NewRejectedEvent(call, pierrors.ErrToolInvalidArguments, err.Error()), nil
	}

	if s.emit != nil {
		s.emit(NewStartEvent(call))
	}

	// 工具执行期间的增量更新转成增量事件送出；emit 为 nil 时静默丢弃，
	// 但 emitter 始终非 nil，避免工具实现额外做空指针判断。
	output, err := toolEntry.tool.Execute(ctx, call.Arguments, new(UpdateEmitter(func(update schema.ToolUpdate) {
		if s.emit != nil {
			s.emit(NewUpdateEvent(call, update))
		}
	})))
	if err != nil {
		return NewErrorEvent(call, err), nil
	}
	if output == nil {
		return NewErrorEvent(call, pierrors.ErrToolPanic.Wrap(errors.New("tool returned nil output"))), nil
	}

	return NewEndEvent(call, *output, false, 0), nil
}

// executeWave 并发执行 calls[start:end)，结束事件按下标写回 results。
func (s *Scheduler) executeWave(
	ctx context.Context,
	calls schema.ToolCalls,
	results []Event,
	start int,
	end int,
) error {
	limit := s.maxParallel
	if limit <= 0 {
		limit = 1
	}
	if waveSize := end - start; limit > waveSize {
		limit = waveSize
	}

	semaphore := make(chan struct{}, limit)
	executionErrors := make([]error, end-start)
	var waitGroup sync.WaitGroup
	for index := start; index < end; index++ {
		call := calls[index]
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()

			select {
			case semaphore <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-semaphore }()
			if ctx.Err() != nil {
				return
			}

			results[index], executionErrors[index-start] = s.Execute(ctx, call)
		}()
	}
	waitGroup.Wait()

	for _, err := range executionErrors {
		if err != nil {
			return err
		}
	}

	return ctx.Err()
}
