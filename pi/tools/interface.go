package tools

import (
	"context"
	"encoding/json"
	"reflect"

	"github.com/PycMono/go-harness/pi/schema"
)

type Tool interface {
	Definition() schema.ToolDefinition
	Execute(context.Context, json.RawMessage, *UpdateEmitter) (*schema.ToolOutput, error)
}

// 报告工具接口是否为空或装有一个类型化 nil 值。
func isNilTool(tool Tool) bool {
	if tool == nil {
		return true
	}
	value := reflect.ValueOf(tool)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}
