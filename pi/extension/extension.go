package extension

import (
	"context"

	"github.com/PycMono/go-harness/pi/tools"
)

// Extension 是启动期接入的扩展。
type Extension interface {
	// Name 是扩展的唯一标识，也是它提供的工具的 owner：注册时由 Runtime 拿这个
	// 名字登记，扩展自己说不上别的。
	Name() string
	// Register 在启动期产出这个扩展提供的工具。ctx 是装配方给的启动期 ctx：
	// 扩展拿它做网络连接（SDK 的 Client.Connect 必须收到 ctx）与日志，装配方
	// 也能用它给整个启动期设上限。
	//
	// 它不碰注册表：产出与登记分开，登记（owner、整批回滚、冻结）归 Runtime 的
	// Register。返回空列表表示这次什么都不接（非必需的 server 连不上就是这条
	// 路），不算失败。
	Register(ctx context.Context) ([]tools.Tool, error)
}

// Closer 是扩展可选的停机清理。Register 失败的那个扩展也会收到 Close——它可能
// 已经占下连接与子进程，收尾是它自己的事；只读不占资源的扩展不必实现它。
type Closer interface {
	Close(context.Context) error
}
