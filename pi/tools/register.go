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

// Register 建一个新注册表，把内置工具整批登记进去，owner 是 staticToolOwner。
// 它与扩展那条路走同一个入口：整批要么全进要么全退，不必另写一遍逐个注册。
func Register(items []Tool) (*Registry, error) {
	registry := &Registry{tools: make(map[string]entry, len(items))}
	if err := registry.RegisterFor(staticToolOwner, items); err != nil {
		return nil, err
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

// RegisterFor 以 owner 名义整批注册；中途失败则把该 owner 本批已注册的
// 全部摘掉，注册表回到调用前的状态（不影响其他 owner 的工具）。
// 同一个 owner 只准注册一次：重复注册返回 ErrToolAlreadyRegistered。这条
// 前提是"回到调用前"能成立的原因——Rollback 按 owner 整批摘除，owner 复用
// 会把先前那批一起摘掉。冻结不在这里判：写入只有 registerLocked 一处，守卫
// 跟着写入点走，就没有绕过去的旁路。
func (r *Registry) RegisterFor(owner string, items []Tool) error {
	// 空 owner 会让 Rollback 变成"摘掉所有空 owner 的工具"，而这批东西再也分不出
	// 是谁的：宁可在入口拒掉。
	if strings.TrimSpace(owner) == "" {
		return pierrors.ErrToolDefinitionInvalid.Wrap(
			errors.New("tool owner must not be empty"))
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// 扫一遍而不是另存一份 owner 名册：注册只发生在装配期，这一次 O(n) 不
	// 值当引入一个新的字段与它的一致性负担。
	for _, registered := range r.tools {
		if registered.owner == owner {
			return pierrors.ErrToolAlreadyRegistered.Wrap(
				fmt.Errorf("owner %q has already registered tools", owner))
		}
	}

	added := make([]string, 0, len(items))
	for _, tool := range items {
		name, err := r.registerLocked(owner, tool)
		if err != nil {
			for _, name := range added {
				delete(r.tools, name)
			}
			return err
		}
		added = append(added, name)
	}

	return nil
}

// Rollback 摘除该 owner 名下的全部工具，返回摘除数量。Freeze 只挡新增注册，
// 不挡摘除：冻结之后仍然允许回滚。
func (r *Registry) Rollback(owner string) int {
	r.mu.Lock()
	defer r.mu.Unlock()

	matched := make([]string, 0, len(r.tools))
	for name, registered := range r.tools {
		if registered.owner == owner {
			matched = append(matched, name)
		}
	}
	for _, name := range matched {
		delete(r.tools, name)
	}

	return len(matched)
}

// registerLocked 是注册的实现，假定调用方已持有写锁；返回登记的工具名，
// RegisterFor 靠它做整批回滚。校验与 schema 编译都在锁内：注册只发生在装配
// 期，一次编译的代价换来"锁内状态自洽"这一条。冻结也在这里判——写入只有这一
// 处，守在这里就不必指望每个调用方都记得。
func (r *Registry) registerLocked(owner string, tool Tool) (string, error) {
	if isNilTool(tool) {
		return "", pierrors.ErrToolDefinitionInvalid.Wrap(errors.New("tool must not be nil"))
	}

	definition := tool.Definition()
	name := strings.TrimSpace(definition.Name)
	if name == "" {
		return "", pierrors.ErrToolDefinitionInvalid.Wrap(errors.New("tool definition name must not be empty"))
	}
	if definition.Name != name {
		return "", pierrors.ErrToolDefinitionInvalid.Wrap(
			fmt.Errorf("tool definition name %q must not contain surrounding whitespace", definition.Name),
		)
	}
	validateArgs, err := compileSchemaValidator(definition)
	if err != nil {
		return "", err
	}
	toolEntry := entry{definition: definition, tool: tool, validateArgs: validateArgs, owner: owner}

	if r.frozen {
		return "", pierrors.ErrToolRegistryFrozen
	}
	if _, exists := r.tools[name]; exists {
		return "", pierrors.ErrToolAlreadyRegistered.Wrap(fmt.Errorf("tool %q is already registered", name))
	}
	r.tools[name] = toolEntry

	return name, nil
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
