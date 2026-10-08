package observability

import (
	"strconv"

	contexttracing "github.com/PycMono/go-context-sdk/tracing"
	pierrors "github.com/PycMono/go-harness/pi/error"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// ClassifyError 是 contexttracing.WithSpan 的错误分类器：把错误映射成
// pi/error 的稳定错误码。绝不返回错误正文——正文里可能有用户数据，
// 而且基数不受控，前端按它分组会炸。
func ClassifyError(err error) string {
	if err == nil {
		return ""
	}

	return strconv.Itoa(pierrors.CodeOf(err))
}

// ErrorFields 返回失败操作的 error.type / pi.error.code 属性。未分类的错误
// （CodeOf 返回 0，比如 stdlib 的错误）只留 OTel 标准那一项，值为 "unknown"
// ——写 0 会与"没分类"混在同一个桶里。
func ErrorFields(err error) []contexttracing.Field {
	if err == nil {
		return nil
	}
	code := pierrors.CodeOf(err)
	if code == 0 {
		return []contexttracing.Field{contexttracing.KV(AttrErrorType, "unknown")}
	}

	return []contexttracing.Field{
		contexttracing.KV(AttrErrorType, strconv.Itoa(code)),
		contexttracing.KV(AttrErrorCode, strconv.Itoa(code)),
	}
}

// spanError 收尾一个失败的手工生命周期 Span。目前只有 chat Span 走这条路：
// 它的生命周期跨 Stream 的多次调用，包不进一个函数。函数作用域的 Span 一律
// 走 contexttracing.WithSpan + ClassifyError，不用这个。
//
// 不导出：它是本包内部给"包不进函数的那种 Span"用的收尾动作，唯一的调用者在
// provider.go。导出的代价是 `trace.Span` 这个 otel 类型进入本包的公开 API；
// 真要给外部用，包一层只收 error 的形状而不是把这个签名递出去。
func spanError(span trace.Span, err error) {
	if err == nil {
		return
	}
	span.SetStatus(codes.Error, ClassifyError(err))
	span.SetAttributes(ErrorFields(err)...)
}
