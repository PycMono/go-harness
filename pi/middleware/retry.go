package middleware

import (
	"errors"

	logsdk "github.com/PycMono/go-logger-sdk"
	retry "github.com/avast/retry-go/v4"

	pierrors "github.com/PycMono/go-harness/pi/error"
	"github.com/PycMono/go-harness/pi/tools"
)

// Retry 用 retry-go 重试链上剩余部分（含真实工具调用），最多尝试
// maxAttempts 次（含首次）。仅当结束错误命中 retryable 才继续；每次尝试
// 前用 Reset 清空上一轮结果，从自己位置的下一环重新执行。
//
// 位置约定：Reset 会把链上位于 Retry 之后的 handler 也重新跑一遍，所以
// Retry 应放在需要随每次尝试重新执行的 handler 靠链尾一侧。通用的 Retry
// 不进 Defaults——重试条件是调用方的策略；固定重试暂时性失败的快捷方式
// 是 RetryTransient，已含在 Defaults 里。
//
// 重试间隔沿用 retry-go 默认的退避 + 抖动，调用方需要自定义节奏时换用
// 自己的 Handler 而不是改这里。
func Retry(maxAttempts int, retryable func(error) bool) tools.Handler {
	// retry-go 的 Attempts(0) 表示无限重试，这里收口成至少一次。
	if maxAttempts < 1 {
		maxAttempts = 1
	}

	return func(e *tools.Execution) {
		from := e.Index()
		err := retry.Do(
			func() error {
				e.Reset(from)
				e.Next()
				return e.Err
			},
			retry.Context(e.Ctx),
			retry.Attempts(uint(maxAttempts)),
			retry.RetryIf(func(err error) bool { return err != nil && retryable(err) }),
			retry.LastErrorOnly(true),
			retry.OnRetry(func(_ uint, err error) {
				logsdk.Warn(e.Ctx, "tool retry",
					logsdk.Any("component", "tool_runtime"),
					logsdk.Any("tool", e.Definition.Name),
					logsdk.Any("error", err.Error()))
			}),
		)
		if err != nil {
			e.Err = err
		}
	}
}

// defaultRetryAttempts 是 RetryTransient 的默认尝试次数（含首次）。
const defaultRetryAttempts = 3

// RetryTransient 只重试暂时性失败的构造器：按错误链是否命中 ErrToolTimeout
// 判定，默认尝试 3 次（含首次）。注意工具可能不是幂等的，接入前确认重放
// 副作用可接受。
func RetryTransient() tools.Handler {
	return Retry(defaultRetryAttempts, func(err error) bool {
		return errors.Is(err, pierrors.ErrToolTimeout)
	})
}
