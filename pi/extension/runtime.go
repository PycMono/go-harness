package extension

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"

	pierrors "github.com/PycMono/go-harness/pi/error"
	"github.com/PycMono/go-harness/pi/tools"
)

// Runtime 持有本轮接入的扩展，管接入与停机清理。它不存业务状态：某个扩展
// 跳过了什么、为什么跳，是扩展自己的事。
type Runtime struct {
	extensions []Extension // 调用方给的顺序就是接入顺序，也是逆序收尾的顺序
	started    int         // 接入成功的个数，也就是已启动的那批前缀

	closeOnce sync.Once // 停机只真关一次：CloseAll 可能被重复调用
	closeErr  error     // 第一次 CloseAll 的结果，重复调用原样返回
}

// NewRuntime 校验扩展并按调用方给的顺序收好：空列表也返回可用的 Runtime，调用方
// 不必判 nil。不排序——顺序是调用方声明的，接进来与收尾都照它走，注册顺序因此与
// 配置里的书写顺序一致，人也看得出"哪个 server 先接"。
func NewRuntime(extensions []Extension) (*Runtime, error) {
	ordered := make([]Extension, 0, len(extensions))
	seen := make(map[string]struct{}, len(extensions))
	for _, v := range extensions {
		if isNilExtension(v) {
			return nil, pierrors.ErrInitialization.Wrap(
				errors.New("extension must not be nil"))
		}

		name := strings.TrimSpace(v.Name())
		if len(name) == 0 {
			return nil, pierrors.ErrInitialization.Wrap(
				errors.New("extension name must not be empty"))
		}

		if _, ok := seen[name]; ok {
			return nil, pierrors.ErrInitialization.Wrap(
				fmt.Errorf("duplicate extension name %q", name))
		}

		seen[name] = struct{}{}
		ordered = append(ordered, v)
	}

	return &Runtime{extensions: ordered}, nil
}

// Register 按扩展名排序逐个接入，ctx 原样传给每个扩展。每个扩展两步：先要它产出
// 工具（Extension.Register），再以它的名字为 owner 整批登记。任一步失败即收尾：
// 关掉这个扩展（它可能已经占下连接与子进程）与已启动的那批，错误 errors.Join 后
// 一起返回。登记失败不用单独回滚——RegisterFor 是整批原子的，失败时它自己已经
// 把这一批摘干净了。
//
// 它不冻结注册表——Freeze 归装配方（pi/agent.go），Runtime 只管接入与收尾。
func (r *Runtime) Register(ctx context.Context, registry *tools.Registry) error {
	for _, extension := range r.extensions {
		items, err := extension.Register(ctx)
		if err == nil {
			err = registry.RegisterFor(extension.Name(), items)
		}
		if err != nil {
			return errors.Join(err, closeExtension(ctx, extension), r.closeStarted(ctx))
		}

		r.started++
	}

	return nil
}

// closeExtension 关掉刚失败的那个扩展。它不在"已启动"那批里（started 只记成功
// 的），而它的 Register 可能已经连上了远端、起了子进程，漏掉就是连接与后台
// goroutine 的泄漏：反复建 Agent 会累积。没实现 Closer 的扩展无事可做。
func closeExtension(ctx context.Context, extension Extension) error {
	closer, ok := extension.(Closer)
	if !ok {
		return nil
	}

	return closer.Close(ctx)
}

// CloseAll 逆序关闭已启动的扩展。真正关一次，重复调用返回第一次的结果——
// 结果存在字段上而不是局部变量里，否则第二次调用会返回 nil。
func (r *Runtime) CloseAll(ctx context.Context) error {
	r.closeOnce.Do(func() {
		r.closeErr = r.closeStarted(ctx)
	})

	return r.closeErr
}

// closeStarted 逆序关闭前 started 个里实现了 Closer 的扩展，并把 started 归零
// ——关过的不再关第二次。装配失败与停机收尾共用它。
func (r *Runtime) closeStarted(ctx context.Context) error {
	var closeErr error
	for index := r.started - 1; index >= 0; index-- {
		closer, ok := r.extensions[index].(Closer)
		if !ok {
			continue
		}
		closeErr = errors.Join(closeErr, closer.Close(ctx))
	}
	r.started = 0

	return closeErr
}

func isNilExtension(e Extension) bool {
	if e == nil {
		return true
	}

	value := reflect.ValueOf(e)
	switch value.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Map,
		reflect.Slice, reflect.Chan, reflect.Func:
		return value.IsNil()
	default:
		return false
	}

}
