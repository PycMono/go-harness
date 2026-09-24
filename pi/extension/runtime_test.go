package extension

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	pierrors "github.com/PycMono/go-harness/pi/error"
	"github.com/PycMono/go-harness/pi/schema"
	"github.com/PycMono/go-harness/pi/tools"
)

// 本文件钉住 Runtime 的两件事：接入的顺序与失败时的收尾。顺序按调用方给的声明，
// 不排序；收尾要覆盖失败的那一个——它不在"已启动"那批里，而它可能已经连上远端、
// 起了子进程，漏关就是泄漏，而且这条漏关不会报错。

type stubTool struct {
	name string
}

func (t stubTool) Definition() schema.ToolDefinition {
	return schema.ToolDefinition{
		Name:        t.name,
		Description: "测试工具",
		InputSchema: map[string]any{"type": "object"},
	}
}

func (t stubTool) Execute(
	context.Context, json.RawMessage, *tools.UpdateEmitter,
) (*schema.ToolOutput, error) {
	return &schema.ToolOutput{Content: schema.ContentBlocks{schema.TextBlock("ok")}}, nil
}

// recorder 记下接入与关闭的次序。收尾的正确性一半在"关了没有"，一半在"按什么顺序
// 关"，只有把两件事记在同一串里才断言得清楚。
type recorder struct {
	events []string
}

func (r *recorder) add(event string) { r.events = append(r.events, event) }

// fakeExtension 是能产出工具、能失败、能记账收尾的扩展。
type fakeExtension struct {
	name     string
	items    []tools.Tool
	err      error // Register 的失败
	closeErr error // Close 的失败
	rec      *recorder
	ctxValue string // 收到的 ctx 值，用来钉"原样透传"
}

func (e *fakeExtension) Name() string { return e.name }

func (e *fakeExtension) Register(ctx context.Context) ([]tools.Tool, error) {
	e.rec.add("register:" + e.name)
	if e.ctxValue != "" {
		e.ctxValue = ctx.Value(ctxKey{}).(string)
	}
	if e.err != nil {
		return nil, e.err
	}

	return e.items, nil
}

func (e *fakeExtension) Close(context.Context) error {
	e.rec.add("close:" + e.name)

	return e.closeErr
}

type ctxKey struct{}

// fake 造一个只产出工具、不失败的扩展。
func fake(rec *recorder, name string, toolNames ...string) *fakeExtension {
	items := make([]tools.Tool, 0, len(toolNames))
	for _, toolName := range toolNames {
		items = append(items, stubTool{name: toolName})
	}

	return &fakeExtension{name: name, items: items, rec: rec}
}

// names 返回注册表里现有的全部工具名，供断言"进没进表"。
func names(registry *tools.Registry) []string {
	definitions := registry.Definitions()
	result := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		result = append(result, definition.Name)
	}

	return result
}

func TestNewRuntimeKeepsOrderAndRejectsBadExtensions(t *testing.T) {
	rec := &recorder{}
	// 名字按字母序是 a、b，这里故意倒着给：接入顺序要跟声明一致，不跟名字。
	runtime, err := NewRuntime([]Extension{fake(rec, "b"), fake(rec, "a")})
	if err != nil {
		t.Fatalf("NewRuntime() 出错: %v", err)
	}
	registry, err := tools.Register(nil)
	if err != nil {
		t.Fatalf("Register() 出错: %v", err)
	}
	if err := runtime.Register(context.Background(), registry); err != nil {
		t.Fatalf("Register() 出错: %v", err)
	}

	want := []string{"register:b", "register:a"}
	if len(rec.events) != len(want) || rec.events[0] != want[0] || rec.events[1] != want[1] {
		t.Fatalf("接入次序是 %v，想要 %v", rec.events, want)
	}
}

func TestNewRuntimeRejectsBadExtensions(t *testing.T) {
	rec := &recorder{}
	var typedNil *fakeExtension

	cases := []struct {
		name string
		list []Extension
	}{
		{"类型化 nil", []Extension{typedNil}},
		{"空名", []Extension{fake(rec, "")}},
		{"只有空格的名字", []Extension{fake(rec, "   ")}},
		{"重名", []Extension{fake(rec, "same"), fake(rec, "same")}},
		{"首尾空格之后重名", []Extension{fake(rec, "same"), fake(rec, " same ")}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := NewRuntime(testCase.list); pierrors.CodeOf(err) != 10001 {
				t.Fatalf("错误码 = %d，想要 10001（err: %v）", pierrors.CodeOf(err), err)
			}
		})
	}

	// 空列表也要给一个能用的 Runtime：调用方不必判 nil。
	runtime, err := NewRuntime(nil)
	if err != nil {
		t.Fatalf("NewRuntime(nil) 出错: %v", err)
	}
	if err := runtime.CloseAll(context.Background()); err != nil {
		t.Fatalf("空的 CloseAll() 出错: %v", err)
	}
}

// TestRegisterRegistersUnderExtensionName 钉住 owner 是扩展名：扩展自己说不上
// owner，它只产出工具，登记这一步归 Runtime。
func TestRegisterRegistersUnderExtensionName(t *testing.T) {
	rec := &recorder{}
	runtime, err := NewRuntime([]Extension{fake(rec, "ext:a", "one", "two")})
	if err != nil {
		t.Fatalf("NewRuntime() 出错: %v", err)
	}
	registry, err := tools.Register(nil)
	if err != nil {
		t.Fatalf("Register() 出错: %v", err)
	}
	if err := runtime.Register(context.Background(), registry); err != nil {
		t.Fatalf("Register() 出错: %v", err)
	}

	if got := names(registry); len(got) != 2 {
		t.Fatalf("注册表里有 %v，想要 one 与 two", got)
	}
	if removed := registry.Rollback("ext:a"); removed != 2 {
		t.Fatalf("Rollback(ext:a) = %d，想要 2：owner 不是扩展名", removed)
	}
}

// TestRegisterPassesCtxThrough 钉住 ctx 原样透传：扩展拿它连远端，装配方靠它设
// 启动期的总上限，中间被换掉的话那个上限就没了。
func TestRegisterPassesCtxThrough(t *testing.T) {
	rec := &recorder{}
	extension := fake(rec, "ext:a")
	extension.ctxValue = "marker"
	runtime, err := NewRuntime([]Extension{extension})
	if err != nil {
		t.Fatalf("NewRuntime() 出错: %v", err)
	}
	registry, err := tools.Register(nil)
	if err != nil {
		t.Fatalf("Register() 出错: %v", err)
	}

	ctx := context.WithValue(context.Background(), ctxKey{}, "marker")
	if err := runtime.Register(ctx, registry); err != nil {
		t.Fatalf("Register() 出错: %v", err)
	}
	if extension.ctxValue != "marker" {
		t.Fatalf("扩展收到的 ctx 值是 %q，想要 marker", extension.ctxValue)
	}
}

// TestRegisterClosesFailedExtension 钉住失败的那个也要收尾：它已经连上了远端
// （所以它的 Register 才会走到失败），不在"已启动"那批里，全靠 closeExtension 兜。
func TestRegisterClosesFailedExtension(t *testing.T) {
	t.Run("产出工具时失败", func(t *testing.T) {
		rec := &recorder{}
		failure := errors.New("连不上")
		bad := fake(rec, "ext:bad")
		bad.err = failure

		runtime, err := NewRuntime([]Extension{fake(rec, "ext:ok"), bad})
		if err != nil {
			t.Fatalf("NewRuntime() 出错: %v", err)
		}
		registry, err := tools.Register(nil)
		if err != nil {
			t.Fatalf("Register() 出错: %v", err)
		}

		err = runtime.Register(context.Background(), registry)
		if !errors.Is(err, failure) {
			t.Fatalf("Register() 的错误 = %v，想要 %v", err, failure)
		}
		want := []string{"register:ext:ok", "register:ext:bad", "close:ext:bad", "close:ext:ok"}
		if len(rec.events) != len(want) {
			t.Fatalf("事件是 %v，想要 %v", rec.events, want)
		}
		for index := range want {
			if rec.events[index] != want[index] {
				t.Fatalf("事件是 %v，想要 %v", rec.events, want)
			}
		}
	})

	t.Run("登记时失败", func(t *testing.T) {
		rec := &recorder{}
		// 名字撞车：同一个 owner 只准注册一次，所以第二个扩展的整批会被拒。
		// 这条失败出在 Runtime 自己手里，扩展那边只看到"产出成功"。
		runtime, err := NewRuntime([]Extension{
			fake(rec, "ext:a", "one"),
			fake(rec, "ext:b", "two"),
		})
		if err != nil {
			t.Fatalf("NewRuntime() 出错: %v", err)
		}
		registry, err := tools.Register([]tools.Tool{stubTool{name: "two"}})
		if err != nil {
			t.Fatalf("Register() 出错: %v", err)
		}

		err = runtime.Register(context.Background(), registry)
		if code := pierrors.CodeOf(err); code != 30009 {
			t.Fatalf("错误码 = %d，想要 30009（err: %v）", code, err)
		}
		if removed := registry.Rollback("ext:b"); removed != 0 {
			t.Fatalf("Rollback(ext:b) = %d，想要 0：被拒的一批不该留在表里", removed)
		}
		want := []string{"register:ext:a", "register:ext:b", "close:ext:b", "close:ext:a"}
		if len(rec.events) != len(want) {
			t.Fatalf("事件是 %v，想要 %v", rec.events, want)
		}
		for index := range want {
			if rec.events[index] != want[index] {
				t.Fatalf("事件是 %v，想要 %v", rec.events, want)
			}
		}
	})
}

// TestRegisterJoinsCloseErrors 钉住两条失败都留得住：收尾的错不能盖掉注册的错。
func TestRegisterJoinsCloseErrors(t *testing.T) {
	rec := &recorder{}
	registerErr := errors.New("连不上")
	closeErr := errors.New("关不掉")
	bad := fake(rec, "ext:bad")
	bad.err = registerErr
	bad.closeErr = closeErr

	runtime, err := NewRuntime([]Extension{bad})
	if err != nil {
		t.Fatalf("NewRuntime() 出错: %v", err)
	}
	registry, err := tools.Register(nil)
	if err != nil {
		t.Fatalf("Register() 出错: %v", err)
	}

	err = runtime.Register(context.Background(), registry)
	if !errors.Is(err, registerErr) || !errors.Is(err, closeErr) {
		t.Fatalf("Register() 的错误 = %v，两个都要在里面", err)
	}
}

// TestCloseAllClosesInReverseOnce 钉住停机收尾：逆序关、只关一次、重复调用返回
// 第一次的结果（结果存在字段上，否则第二次会返回 nil）。
func TestCloseAllClosesInReverseOnce(t *testing.T) {
	rec := &recorder{}
	closeErr := errors.New("关不掉")
	first := fake(rec, "ext:a")
	second := fake(rec, "ext:b")
	second.closeErr = closeErr

	runtime, err := NewRuntime([]Extension{first, second})
	if err != nil {
		t.Fatalf("NewRuntime() 出错: %v", err)
	}
	registry, err := tools.Register(nil)
	if err != nil {
		t.Fatalf("Register() 出错: %v", err)
	}
	if err := runtime.Register(context.Background(), registry); err != nil {
		t.Fatalf("Register() 出错: %v", err)
	}

	rec.events = nil
	ctx := context.Background()
	if err := runtime.CloseAll(ctx); !errors.Is(err, closeErr) {
		t.Fatalf("第一次 CloseAll() 的错误 = %v，想要 %v", err, closeErr)
	}
	if err := runtime.CloseAll(ctx); !errors.Is(err, closeErr) {
		t.Fatalf("第二次 CloseAll() 的错误 = %v，想要仍然是 %v", err, closeErr)
	}
	want := []string{"close:ext:b", "close:ext:a"}
	if len(rec.events) != len(want) {
		t.Fatalf("事件是 %v，想要 %v", rec.events, want)
	}
	for index := range want {
		if rec.events[index] != want[index] {
			t.Fatalf("事件是 %v，想要 %v", rec.events, want)
		}
	}
}

// TestRegisterSkipsCloseForNonCloser 钉住"没实现 Closer 的扩展不关"：跳过它而不是
// 让类型断言炸掉。
func TestRegisterSkipsCloseForNonCloser(t *testing.T) {
	registry, err := tools.Register(nil)
	if err != nil {
		t.Fatalf("Register() 出错: %v", err)
	}
	plain := plainExtension{}
	runtime, err := NewRuntime([]Extension{plain})
	if err != nil {
		t.Fatalf("NewRuntime() 出错: %v", err)
	}
	if err := runtime.Register(context.Background(), registry); err != nil {
		t.Fatalf("Register() 出错: %v", err)
	}
	if err := runtime.CloseAll(context.Background()); err != nil {
		t.Fatalf("CloseAll() 出错: %v", err)
	}
}

// plainExtension 只实现 Extension，不实现 Closer。
type plainExtension struct{}

func (plainExtension) Name() string { return "ext:plain" }

func (plainExtension) Register(context.Context) ([]tools.Tool, error) {
	return []tools.Tool{stubTool{name: "plain_one"}}, nil
}
