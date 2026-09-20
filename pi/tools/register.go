package tools

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	pierrors "github.com/PycMono/go-harness/pi/error"
	"github.com/PycMono/go-harness/pi/schema"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

/*
	工具注册，所有工具均使用 Register() 函数注册，Registry 提供注册和查找函数，方便外部使用。

	Registry 可以理解成一个管理类，主要用于工具管理。

*/

// 工具实体定义
type entry struct {
	definition   schema.ToolDefinition
	tool         Tool
	validateArgs func(json.RawMessage) error
	owner        string
}

// Registry 持有已注册 Tool 的定义、实现与参数校验器。Freeze 之后拒绝
// 新的注册；扩展注册的工具可按 owner 整体 Rollback。
type Registry struct {
	mu     sync.RWMutex
	tools  map[string]entry
	frozen bool
}

// Register 注册所有工具
func Register(tools []Tool) (*Registry, error) {
	registry := &Registry{tools: make(map[string]entry, len(tools))}
	for _, tool := range tools {
		if err := registry.register(staticToolOwner, tool); err != nil {
			return nil, err
		}
	}
	return registry, nil
}

// Lookup 返回工具的定义与实现，供装配期校验（如 subagent 绑定）。
func (r *Registry) Lookup(name string) (schema.ToolDefinition, Tool, bool) {
	toolEntry, ok := r.lookup(name)

	return toolEntry.definition, toolEntry.tool, ok
}

// ParallelSafe 报告该工具能否与同批次的其他工具并发执行，取自工具定义里的
// 声明；未注册的工具无从判断，保守按不可并发处理。
func (r *Registry) ParallelSafe(name string) bool {
	toolEntry, ok := r.lookup(name)
	if !ok {
		return false
	}

	return toolEntry.definition.ParallelSafe
}

// Freeze 冻结注册表，后续 Register 一律失败。
func (r *Registry) Freeze() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.frozen = true
}

func (r *Registry) Definitions() []schema.ToolDefinition {
	r.mu.RLock()
	definitions := make([]schema.ToolDefinition, 0, len(r.tools))
	for _, toolEntry := range r.tools {
		definitions = append(definitions, toolEntry.definition)
	}
	r.mu.RUnlock()

	sort.Slice(definitions, func(i, j int) bool {
		return definitions[i].Name < definitions[j].Name
	})

	return definitions
}

func (r *Registry) register(owner string, tool Tool) error {
	if isNilTool(tool) {
		return pierrors.ErrToolDefinitionInvalid.Wrap(errors.New("tool must not be nil"))
	}

	definition := tool.Definition()
	name := strings.TrimSpace(definition.Name)
	if name == "" {
		return pierrors.ErrToolDefinitionInvalid.Wrap(errors.New("tool definition name must not be empty"))
	}
	if definition.Name != name {
		return pierrors.ErrToolDefinitionInvalid.Wrap(
			fmt.Errorf("tool definition name %q must not contain surrounding whitespace", definition.Name),
		)
	}
	validateArgs, err := compileSchemaValidator(definition)
	if err != nil {
		return err
	}
	toolEntry := entry{definition: definition, tool: tool, validateArgs: validateArgs, owner: owner}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.frozen {
		return pierrors.ErrToolRegistryFrozen
	}
	if _, exists := r.tools[name]; exists {
		return pierrors.ErrToolAlreadyRegistered.Wrap(fmt.Errorf("tool %q is already registered", name))
	}
	r.tools[name] = toolEntry

	return nil
}

func (r *Registry) lookup(name string) (entry, bool) {
	r.mu.RLock()
	toolEntry, ok := r.tools[name]
	r.mu.RUnlock()

	return toolEntry, ok
}

func compileSchemaValidator(definition schema.ToolDefinition) (func(json.RawMessage) error, error) {
	schemaJSON, err := json.Marshal(definition.InputSchema)
	if err != nil {
		return nil, pierrors.ErrToolDefinitionInvalid.Wrap(
			fmt.Errorf("marshal input schema for tool %q: %w", definition.Name, err),
		)
	}
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemaJSON))
	if err != nil {
		return nil, pierrors.ErrToolDefinitionInvalid.Wrap(
			fmt.Errorf("decode input schema for tool %q: %w", definition.Name, err),
		)
	}
	location := "urn:go-reagent:tool:" + definition.Name
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(location, document); err != nil {
		return nil, pierrors.ErrToolDefinitionInvalid.Wrap(
			fmt.Errorf("register input schema for tool %q: %w", definition.Name, err),
		)
	}
	compiled, err := compiler.Compile(location)
	if err != nil {
		return nil, pierrors.ErrToolDefinitionInvalid.Wrap(
			fmt.Errorf("compile input schema for tool %q: %w", definition.Name, err),
		)
	}

	return func(arguments json.RawMessage) error {
		decoder := json.NewDecoder(bytes.NewReader(arguments))
		decoder.UseNumber()
		var value any
		if err := decoder.Decode(&value); err != nil {
			return pierrors.ErrToolInvalidArguments.Wrap(
				fmt.Errorf("invalid arguments for tool %q: %w", definition.Name, err),
			)
		}
		var extra any
		if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
			if err != nil {
				return pierrors.ErrToolInvalidArguments.Wrap(
					fmt.Errorf("invalid trailing arguments for tool %q: %w", definition.Name, err),
				)
			}
			return pierrors.ErrToolInvalidArguments.Wrap(
				fmt.Errorf("invalid trailing arguments for tool %q", definition.Name),
			)
		}
		if err := compiled.Validate(value); err != nil {
			return pierrors.ErrToolInvalidArguments.Wrap(
				fmt.Errorf("arguments do not match schema for tool %q: %w", definition.Name, err),
			)
		}
		return nil
	}, nil
}
