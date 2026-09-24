// Package mcpconfig 读配置文件里 mcp.servers 那一段。
//
// 文件长什么样（字段名怎么叫、timeout 的两种写法、认不得的字段要报出名字）定在这里；
// 交给 pi/mcp 的是另一份结构，两边各一份，装配时用 ServerConfigs 转过去。这么分是因为
// 文件格式会跟着别的客户端变，而 pi/mcp 那侧只该认"接一个 server 需要知道哪些值"。
package mcpconfig

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	pierrors "github.com/PycMono/go-harness/pi/error"
	"github.com/PycMono/go-harness/pi/mcp"
)

// Entry 是配置文件里 mcp.servers 的一项。字段名跟生态里的客户端配置对齐
// （transport / url / header_env / allow_tools / tool_prefix），抄来的配置不至于
// 因为字段名对不上而静默失效。
//
// 这里只负责把文件读进来；名字合不合规、transport 认不认得、header_env 缺没缺，
// 都由 pi/mcp 装配时判定（见那边 ServerConfig 的说明），错误从 NewExtension 出来。
type Entry struct {
	// Name 是本地标识，用作工具名前缀与 owner。
	Name string `json:"name"`
	// Enabled 为假时这个 server 完全不接：不连、不注册。缺省视为启用。
	Enabled *bool `json:"enabled,omitempty"`
	// Transport 是传输类型，必填判别字段：http 或 stdio。
	Transport string `json:"transport"`
	// URL 是 Streamable HTTP 的单一入口。仅 http 用。
	URL string `json:"url,omitempty"`
	// Headers 是每个请求附带的固定头。仅 http 用。
	Headers map[string]string `json:"headers,omitempty"`
	// HeaderEnv 也是请求头，只是值写成环境变量的名字：{"X-Api-Key":
	// "EXA_API_KEY"} 表示这个头的值从环境里取。凭据不进配置文件，也就不进
	// 版本库。仅 http 用。
	HeaderEnv map[string]string `json:"header_env,omitempty"`
	// Command/Args/Env/Cwd 是 stdio 的启动参数。仅 stdio 用。
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Cwd     string            `json:"cwd,omitempty"`
	// Required 为真时，这个 server 连接或工具发现失败会让 Agent 装配失败；
	// 为假（默认）时只记一条 Warn，其余 server 与 Agent 照常起。
	Required bool `json:"required,omitempty"`
	// AllowTools 非空时只接入列出的远端工具；空表示接入全部。
	AllowTools []string `json:"allow_tools,omitempty"`
	// ToolPrefix 换掉本地工具名的第一段（默认 mcp）。空表示用默认值。
	ToolPrefix string `json:"tool_prefix,omitempty"`
	// Timeout 是单次远端操作的超时：一次连接、一次工具发现、一次工具调用各
	// 算一次。写数字按秒（60 就是 60 秒），写字符串按 time.ParseDuration
	// （"1m"）。不写表示交给 pi/mcp 取默认值（30s）。
	Timeout Duration `json:"timeout,omitempty"`
}

// recognizedFields 只进错误文案：字段名写错时，把认得的字段列出来比让人去翻
// 代码快。
const recognizedFields = "name/enabled/transport/url/headers/header_env/" +
	"command/args/env/cwd/required/allow_tools/tool_prefix/timeout"

// UnmarshalJSON 解码一个 server 条目，拒收不认识的字段。这个严格是必要的：
// encoding/json 默认把不认识的键静默丢掉，而配置多半是从别的客户端抄来的
// （`transport` 抄成 `type`、`header_env` 抄成 `headers`），错名字丢掉的是
// 凭据——server 照常起来，只是每个请求都不带 Key。宁可在这里报出字段名。
func (e *Entry) UnmarshalJSON(data []byte) error {
	// 别名类型：去掉方法集，否则这里会递归调回自己。
	type plain Entry

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()

	var decoded plain
	if err := decoder.Decode(&decoded); err != nil {
		return pierrors.ErrMCPConfigInvalid.Wrap(fmt.Errorf(
			"server 配置解不开（字段名对不上或类型不对，认得的字段: %s）: %w",
			recognizedFields, err))
	}
	*e = Entry(decoded)

	return nil
}

// Duration 承接配置里的超时。两种写法都收：数字按秒（60 就是 60 秒），字符串走
// time.ParseDuration（"1m"）。生态里的客户端配置两种都有，只认一种就会把抄来的
// 配置挡在门外。
type Duration time.Duration

func (d *Duration) UnmarshalJSON(data []byte) error {
	// 数字走这一支；显式 null 也落在这里，按"没配"处理。
	if trimmed := bytes.TrimSpace(data); len(trimmed) > 0 && trimmed[0] != '"' {
		var seconds *float64
		if err := json.Unmarshal(data, &seconds); err != nil {
			return pierrors.ErrMCPConfigInvalid.Wrap(fmt.Errorf(
				`timeout 只能是秒数或 "30s" 这样的字符串: %w`, err))
		}
		if seconds == nil {
			return nil
		}

		return d.set(time.Duration(*seconds*float64(time.Second)), fmt.Sprintf("%v 秒", *seconds))
	}

	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return pierrors.ErrMCPConfigInvalid.Wrap(fmt.Errorf(
			`timeout 只能是秒数或 "30s" 这样的字符串: %w`, err))
	}
	if text == "" {
		return nil
	}

	parsed, err := time.ParseDuration(text)
	if err != nil {
		return pierrors.ErrMCPConfigInvalid.Wrap(fmt.Errorf("timeout %q: %w", text, err))
	}

	return d.set(parsed, text)
}

func (d *Duration) set(parsed time.Duration, written string) error {
	if parsed <= 0 {
		return pierrors.ErrMCPConfigInvalid.Wrap(fmt.Errorf("timeout 必须为正，收到 %s", written))
	}
	*d = Duration(parsed)

	return nil
}

// ServerConfigs 把读进来的一项项转成运行期结构。只搬值，不做校验：那份结构上该有的
// 规矩（名字、transport、header_env、http 与 stdio 的字段）在 pi/mcp 装配时判，
// 手写 ServerConfig 的调用方与读文件的走同一条路。
func ServerConfigs(entries []Entry) []mcp.ServerConfig {
	configs := make([]mcp.ServerConfig, 0, len(entries))
	for _, entry := range entries {
		configs = append(configs, mcp.ServerConfig{
			Name:       entry.Name,
			Enabled:    entry.Enabled,
			Transport:  mcp.TransportType(entry.Transport),
			URL:        entry.URL,
			Headers:    entry.Headers,
			HeaderEnv:  entry.HeaderEnv,
			Command:    entry.Command,
			Args:       entry.Args,
			Env:        entry.Env,
			Cwd:        entry.Cwd,
			Required:   entry.Required,
			AllowTools: entry.AllowTools,
			ToolPrefix: entry.ToolPrefix,
			Timeout:    time.Duration(entry.Timeout),
		})
	}

	return configs
}
