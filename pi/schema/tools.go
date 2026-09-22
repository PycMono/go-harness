package schema

import (
	"encoding/json"
	"errors"
	"fmt"

	anthropicsdk "github.com/anthropics/anthropic-sdk-go"
	openaisdk "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/shared"
)

// ToolCall 表示模型发起的一次工具调用请求。
type ToolCall struct {
	// ID 是工具调用的唯一标识。
	ID string `json:"id"`
	// Name 是模型请求调用的工具名称。
	Name string `json:"name"`
	// Arguments 保存未经解析的 JSON 参数，由具体工具负责解析。
	Arguments json.RawMessage `json:"arguments"`
}

// ToolCalls 是一次响应中的一批工具调用请求。
type ToolCalls []ToolCall

// ToolDefinition 描述一个可供模型调用的工具。
type ToolDefinition struct {
	// Name 是工具的唯一名称。
	Name string `json:"name"`
	// Label 是用于展示的工具名称。
	Label string `json:"label,omitempty"`
	// Description 说明工具的用途。
	Description string `json:"description"`
	// InputSchema 使用 JSON Schema 描述工具的输入参数。
	InputSchema any `json:"input_schema"`
	// ParallelSafe 表示运行框架能否在同一批次中并发执行该工具，默认值为 false。
	ParallelSafe bool `json:"parallel_safe,omitempty"`
}

// InputSchemaObject returns the tool input schema as a JSON object while
func (t *ToolDefinition) InputSchemaObject() (map[string]any, error) {
	wrap := func(err error) (map[string]any, error) {
		return nil, fmt.Errorf("tool %q input schema: %w", t.Name, err)
	}

	value := t.InputSchema
	if value == nil {
		return nil, nil
	}

	object, ok := value.(map[string]any)
	if !ok {
		encoded, err := json.Marshal(value)
		if err != nil {
			return wrap(err)
		}
		if err = json.Unmarshal(encoded, &object); err != nil {
			return wrap(err)
		}
		if object == nil {
			return wrap(errors.New("JSON schema must be an object"))
		}
	}

	normalized, err := normalizeToolSchemaNumbers(object)
	if err != nil {
		return wrap(err)
	}
	object = normalized.(map[string]any)
	if schemaType, exists := object["type"]; exists && schemaType != "object" {
		return nil, fmt.Errorf("tool %q input schema type must be object", t.Name)
	}

	return object, nil
}

type ToolDefinitions []*ToolDefinition

func (t ToolDefinitions) ToOpenAITools() ([]openaisdk.ChatCompletionToolUnionParam, error) {
	result := make([]openaisdk.ChatCompletionToolUnionParam, 0, len(t))
	for _, definition := range t {
		inputSchema, err := definition.InputSchemaObject()
		if err != nil {
			return nil, err
		}

		result = append(result, openaisdk.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
			Name: definition.Name, Description: openaisdk.String(definition.Description), Parameters: inputSchema,
		}))
	}

	return result, nil
}

func (t ToolDefinitions) ToAnthropicTools() ([]anthropicsdk.ToolUnionParam, error) {
	result := make([]anthropicsdk.ToolUnionParam, 0, len(t))
	for _, definition := range t {
		inputSchema, err := definition.InputSchemaObject()
		if err != nil {
			return nil, err
		}
		var properties any
		var required []string
		extraFields := make(map[string]any)
		for key, value := range inputSchema {
			switch key {
			case "type":
			case "properties":
				properties = value
			case "required":
				required, err = schemaStringValues(value)
				if err != nil {
					return nil, fmt.Errorf("tool %q required: %w", definition.Name, err)
				}
			default:
				extraFields[key] = value
			}
		}
		tool := anthropicsdk.ToolParam{
			Name: definition.Name, Description: anthropicsdk.String(definition.Description),
			InputSchema: anthropicsdk.ToolInputSchemaParam{Properties: properties, Required: required, ExtraFields: extraFields},
		}
		result = append(result, anthropicsdk.ToolUnionParam{OfTool: &tool})
	}
	return result, nil
}

// ToolOutput 是工具一次执行的返回值。执行域据此构造 RoleTool 消息，
// Content 直接作为消息内容块写入上下文。
type ToolOutput struct {
	// Content 是工具返回的内容块。
	Content ContentBlocks `json:"content"`
	// Details 保存工具自定义的结构化附加信息，不进入模型上下文。
	Details any `json:"details,omitempty"`
}

// UpdateEmitter receives incremental updates from a running tool.
type UpdateEmitter func(ToolUpdate)

// ToolUpdate 是工具执行过程中的一次增量返回值。
type ToolUpdate struct {
	// Content 是本次增量返回的内容块。
	Content ContentBlocks `json:"content"`
	// Details 保存本次增量自定义的结构化附加信息。
	Details any `json:"details,omitempty"`
}

func normalizeToolSchemaNumbers(value any) (any, error) {
	switch typed := value.(type) {
	case json.Number:
		if integer, err := typed.Int64(); err == nil {
			return integer, nil
		}
		number, err := typed.Float64()
		if err != nil {
			return nil, fmt.Errorf("invalid JSON schema number %q: %w", typed, err)
		}
		return number, nil
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, child := range typed {
			normalized, err := normalizeToolSchemaNumbers(child)
			if err != nil {
				return nil, err
			}
			result[key] = normalized
		}
		return result, nil
	case []any:
		result := make([]any, len(typed))
		for index, child := range typed {
			normalized, err := normalizeToolSchemaNumbers(child)
			if err != nil {
				return nil, err
			}
			result[index] = normalized
		}
		return result, nil
	default:
		return value, nil
	}
}

func schemaStringValues(value any) ([]string, error) {
	switch values := value.(type) {
	case nil:
		return nil, nil
	case []string:
		return values, nil
	case []any:
		result := make([]string, 0, len(values))
		for _, value := range values {
			text, ok := value.(string)
			if !ok {
				return nil, errors.New("must contain only strings")
			}
			result = append(result, text)
		}
		return result, nil
	default:
		return nil, errors.New("must be an array of strings")
	}
}
