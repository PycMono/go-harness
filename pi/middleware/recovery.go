package middleware

import (
	"fmt"

	pierrors "github.com/PycMono/go-harness/pi/error"
	"github.com/PycMono/go-harness/pi/tools"
)

// PanicRecovery 兜住链内 panic（真实工具实现的 panic 是最常见来源），
// 恢复后按铁律 2 必须 Block，否则外层 Next 循环会继续执行后续 handler。
// 放在链上尽量靠内的位置：恢复后 Logging 的折返段还能看到 Err，
// 结束日志的 status 是 error 而不是缺失。
func PanicRecovery(e *tools.Execution) {
	defer func() {
		if recovered := recover(); recovered != nil {
			e.Block(pierrors.ErrToolPanic.Wrap(fmt.Errorf("tool panicked: %v", recovered)))
		}
	}()

	e.Next()
}
