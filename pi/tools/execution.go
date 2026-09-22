package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	pierrors "github.com/PycMono/go-harness/pi/error"
	"github.com/PycMono/go-harness/pi/schema"
)

// 本文件是 Tool 执行链的中间件契约，术语对齐 pi.dev：
// 链上的单元叫 Handler（pi 文档的 tool_call handlers），被拦截的一次
// Tool 执行叫 Execution（对应 tool_execution_start/end 事件），阻断
// 叫 Block（对应 {block: true, reason}）。链式结构即中间件模式：
// 切片顺序即执行顺序、前后置沿链折返、Block 短路。
//
// 两条语义铁律（均由 Execution 强制，违反即 Block）：
//
//  1. 短路必须 Block，不能只 return：handler 不调 Next 直接返回时，
//     Next 会检测到链没有前进，强制 Block，避免静默跳过真实工具执行。
//  2. panic 恢复后必须 Block：recover 后控制流回到外层 Next 循环，
//     不 Block 会继续执行后面的 handler。
//
// 契约归本包，内置 Handler 实现归 pi/middleware（依赖方向
// pi/middleware → pi/tools，本包不反向依赖）。Scheduler 是链的驱动方，
// 链尾固定接终端 handler 执行真实 Tool 调用，整条链有且只有一个。

// Handler 是 Tool 执行链上的一环，签名为 func(*Execution)。
type Handler func(*Execution)

// Execution 表示一次被拦截的 Tool 执行：前置字段由 Scheduler 填好，
// 后置字段（Err / Output）由链上的 handler 产出。
type Execution struct {
	Ctx          context.Context
	Call         schema.ToolCall
	Definition   schema.ToolDefinition
	Tool         Tool
	Emit         *UpdateEmitter
	ValidateArgs func(json.RawMessage) error

	Err    error
	Output schema.ToolOutput

	handlers []Handler
	index    int
}

// Run 以 handlers 为链驱动执行，链尾固定追加终端 handler 真正调工具。
// handlers 会被复制，调用方后续修改切片不影响本次执行。
func (e *Execution) Run(handlers []Handler) {
	e.handlers = append(slices.Clone(handlers), execute)
	e.index = -1

	e.Next()
}

// Next 执行链上的下一个 handler。Block 之后循环立即结束。
func (e *Execution) Next() {
	e.index++

	for e.index < len(e.handlers) {
		position := e.index
		e.handlers[position](e)
		// 终端 handler（链尾）的本职就是不调 Next 直接返回，豁免检查。
		if position < len(e.handlers)-1 && e.index == position && e.Err == nil {
			// 铁律 1 的强制执行：handler 既没有委托后续链（Next），也没有
			// 显式阻断（Block），继续跑下去会静默跳过真实工具执行。
			e.Block(pierrors.ErrToolRuntime.Wrap(fmt.Errorf(
				"middleware handler at index %d returned without calling Next or Block", position)))
		}
		e.index++
	}
}

// Block 记录失败原因并阻断链式执行：当前 handler 返回后，外层 Next 循环
// 不再继续。
func (e *Execution) Block(err error) {
	e.Err = err
	e.index = len(e.handlers)
}

// Index 返回当前 handler 在链上的位置：handler 函数体入口处调用得到的
// 就是自己的下标，供重试类 handler 记住位置。
func (e *Execution) Index() int { return e.index }

// Reset 清空上一轮执行结果并把链回退到 from，下一次 Next 会从 from 的
// 下一环重新执行。供重试类 handler 在 e.Next() 返回且决定重试后调用。
func (e *Execution) Reset(from int) {
	e.Err = nil
	e.Output = schema.ToolOutput{}
	e.index = from
}

// execute 链上唯一的终端 handler：真正调工具并产出 Output。panic 不在
// 这里恢复——由链上的 PanicRecovery 类 handler 兜住，链上没有时按 Go
// 默认行为上抛。
func execute(e *Execution) {
	output, err := e.Tool.Execute(e.Ctx, e.Call.Arguments, e.Emit)
	if err != nil {
		e.Err = err
		return
	}
	if output == nil {
		e.Err = pierrors.ErrToolPanic.Wrap(errors.New("tool returned nil output"))
		return
	}

	e.Output = *output
}
