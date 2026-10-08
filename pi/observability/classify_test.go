package observability

import (
	"errors"
	"testing"

	contexttracing "github.com/PycMono/go-context-sdk/tracing"
	pierrors "github.com/PycMono/go-harness/pi/error"
	"go.opentelemetry.io/otel/attribute"
)

// 本文件钉住错误分类这一层：Span 上只许出现稳定码，不许出现错误正文。
// 两个函数都是纯函数，直接比字符串，不装 Span 导出器。

func TestClassifyErrorReturnsStableCode(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil 错误", nil, ""},
		{"带码的错误", pierrors.ErrAIGeneration, "20000"},
		{"包了一层的带码错误", pierrors.ErrCompactionFailed.Wrap(errors.New("摘要为空")), "50002"},
		{"没有码的错误", errors.New("boom"), "0"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := ClassifyError(testCase.err); got != testCase.want {
				t.Fatalf("ClassifyError() = %q, want %q", got, testCase.want)
			}
		})
	}
}

// TestClassifyErrorNeverLeaksMessage 是这一层的红线：正文不进 Span。
// 用一个只可能出现在正文里的字符串当哨兵，任何一条路径带上它就是失败。
func TestClassifyErrorNeverLeaksMessage(t *testing.T) {
	const sentinel = "用户手机号 13800000000"
	err := pierrors.ErrAIGeneration.Wrap(errors.New(sentinel))

	for _, got := range []string{ClassifyError(err), ClassifyError(errors.New(sentinel))} {
		if got == sentinel {
			t.Fatalf("分类结果带上了错误正文：%q", got)
		}
	}
}

func TestErrorFields(t *testing.T) {
	t.Run("nil 错误没有属性", func(t *testing.T) {
		if fields := ErrorFields(nil); fields != nil {
			t.Fatalf("ErrorFields(nil) = %v, want nil", fields)
		}
	})

	t.Run("带码的错误两项都有", func(t *testing.T) {
		fields := ErrorFields(pierrors.ErrCompactionFailed.Wrap(errors.New("摘要为空")))
		got := attributeMap(fields)
		if got[AttrErrorType].AsString() != "50002" {
			t.Errorf("%s = %q, want %q", AttrErrorType, got[AttrErrorType].AsString(), "50002")
		}
		if got[AttrErrorCode].AsString() != "50002" {
			t.Errorf("%s = %q, want %q", AttrErrorCode, got[AttrErrorCode].AsString(), "50002")
		}
	})

	t.Run("无码的错误只留 error.type=unknown", func(t *testing.T) {
		fields := ErrorFields(errors.New("boom"))
		if len(fields) != 1 {
			t.Fatalf("属性数 = %d, want 1", len(fields))
		}
		got := attributeMap(fields)
		if got[AttrErrorType].AsString() != "unknown" {
			t.Errorf("%q = %q, want %q", AttrErrorType, got[AttrErrorType].AsString(), "unknown")
		}
		if _, ok := got[AttrErrorCode]; ok {
			t.Errorf("无码的错误不该有 %s：写 0 会与\"没分类\"混在同一个桶里", AttrErrorCode)
		}
	})
}

// attributeMap 把属性切片折成 key → value，断言时按名字取。
//
// 收 []contexttracing.Field 就是收 []attribute.KeyValue——那边是类型别名
// （go-context-sdk/tracing/preset.go:17），所以 Span 上取下来的 Attributes()
// 也能直接递进来，不必再写第二份。
func attributeMap(fields []contexttracing.Field) map[string]attribute.Value {
	attrs := make(map[string]attribute.Value, len(fields))
	for _, field := range fields {
		attrs[string(field.Key)] = field.Value
	}

	return attrs
}
