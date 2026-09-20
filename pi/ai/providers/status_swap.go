package providers

import (
	stderrors "errors"
	"io"
	"net"
	"net/http"

	errors "github.com/PycMono/go-harness/pi/error"
)

// HTTPStatusToCode 将 provider HTTP 状态码归入通用 AI 错误值。
func HTTPStatusToCode(status int) *errors.CodeError {
	switch {
	case status == http.StatusTooManyRequests:
		return errors.ErrAIRateLimited
	case status == http.StatusRequestTimeout,
		status == http.StatusConflict,
		status >= http.StatusInternalServerError:
		return errors.ErrAITransient
	case status == http.StatusUnauthorized,
		status == http.StatusForbidden:
		return errors.ErrAIUnauthorized
	case status == http.StatusBadRequest:
		return errors.ErrAIInvalidRequest
	default:
		return errors.ErrAIGeneration
	}
}

// IsTransientNetwork 判断是否为可重试的传输层错误（EOF / 网络错误）。
func IsTransientNetwork(err error) bool {
	if stderrors.Is(err, io.EOF) || stderrors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}

	var networkError net.Error
	return stderrors.As(err, &networkError)
}
