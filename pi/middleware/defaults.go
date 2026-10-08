package middleware

import "github.com/PycMono/go-harness/pi/tools"

// Defaults 返回默认中间件集合，切片顺序即执行顺序：
// Tracing 最外层包住整条链与真实工具调用，一个工具调用一个 Span（重试的
// 每一次尝试在同一个 Span 里，次数看 Logging 与 Retry 的日志）；Logging
// 记录开始与结束；RetryTransient 居中，重试只重跑链上更靠内的部分
// （PanicRecovery 与真实工具调用），结束日志记录的是最终状态；PanicRecovery
// 最靠内，兜住每次尝试中真实工具执行的 panic，恢复后 Retry 拿到 ErrToolPanic，
// 不命中 ErrToolTimeout，不会重试。
// 未注册的 Tool 在 pi/tools 的 Runtime 入口即返回，不经过本链。
func Defaults() []tools.Handler {
	return []tools.Handler{
		Tracing,
		Logging,
		RetryTransient(),
		PanicRecovery,
	}
}
