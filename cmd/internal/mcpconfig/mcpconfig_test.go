package mcpconfig

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	pierrors "github.com/PycMono/go-harness/pi/error"
	"github.com/PycMono/go-harness/pi/mcp"
)

// 本文件钉住读配置这一侧的契约：字段名写错要报出名字（错名字丢掉的是凭据），
// timeout 的两种写法都收，以及转换只搬值、不做校验——校验在 pi/mcp 装配时判。

// TestEntryRejectsUnknownField 钉住严格解码：认不得的字段当场报错并点出字段名。
// 这条是必要的：encoding/json 默认把不认识的键静默丢掉，配置多是从别的客户端抄来
// 的（`transport` 抄成 `type`），丢掉的往往是凭据。
func TestEntryRejectsUnknownField(t *testing.T) {
	cases := map[string]string{
		"抄了别的客户端的 type":  `{"name":"exa","type":"http","url":"https://example.com/mcp"}`,
		"字段名拼一半":         `{"name":"exa","transport":"http","allowed_tools":["web_search_exa"]}`,
		"把入口写成 endpoint": `{"name":"exa","transport":"http","endpoint":"https://example.com/mcp"}`,
	}

	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			var entry Entry

			err := json.Unmarshal([]byte(raw), &entry)
			if err == nil {
				t.Fatalf("解 %s 竟然成功，想要报错", raw)
			}
			if code := pierrors.CodeOf(err); code != 81000 {
				t.Fatalf("错误码 = %d，想要 81000", code)
			}
			// 错误文案要含认得的字段表，人照着改得动。
			if !strings.Contains(err.Error(), "header_env") {
				t.Fatalf("错误文案里没有认得的字段表: %v", err)
			}
		})
	}
}

// TestTimeout 钉住 timeout 的两种写法与"不写"：数字按秒、字符串走 ParseDuration、
// 不写就是 0（交给 pi/mcp 取默认值）；写了个非正数要报错，不能悄悄当成"没配"。
func TestTimeout(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want time.Duration
		code int // 非零表示想要这个错误码
	}{
		{name: "数字按秒", raw: `60`, want: 60 * time.Second},
		{name: "字符串", raw: `"1m"`, want: time.Minute},
		{name: "小数秒", raw: `1.5`, want: 1500 * time.Millisecond},
		{name: "null 表示没配", raw: `null`},
		{name: "空串表示没配", raw: `""`},
		{name: "不写表示没配", raw: ``},
		{name: "零要报错", raw: `0`, code: 81000},
		{name: "负数要报错", raw: `-5`, code: 81000},
		{name: "零秒字符串要报错", raw: `"0s"`, code: 81000},
		{name: "解不开的字符串要报错", raw: `"一会儿"`, code: 81000},
		{name: "布尔要报错", raw: `true`, code: 81000},
	}

	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			raw := `{"name":"exa","transport":"http"`
			if item.raw != "" {
				raw += `,"timeout":` + item.raw
			}
			raw += `}`

			var entry Entry
			err := json.Unmarshal([]byte(raw), &entry)
			if item.code != 0 {
				if err == nil {
					t.Fatalf("解 %s 竟然成功，想要 %d", raw, item.code)
				}
				if code := pierrors.CodeOf(err); code != item.code {
					t.Fatalf("错误码 = %d，想要 %d", code, item.code)
				}

				return
			}
			if err != nil {
				t.Fatalf("解 %s 出错: %v", raw, err)
			}
			if got := time.Duration(entry.Timeout); got != item.want {
				t.Fatalf("timeout = %s，想要 %s", got, item.want)
			}
		})
	}
}

// TestServerConfigs 钉住转换：值一一搬过去，不做校验也不解析 header_env——名字合不合规、
// transport 认不认得、环境变量缺没缺，都是 pi/mcp 装配时的事（那边与手写 ServerConfig
// 的调用方走同一条路），这里把坏值原样带过去，不假装自己判过了。
func TestServerConfigs(t *testing.T) {
	const raw = `{
		"mcp": {
			"servers": [
				{
					"name": "exa",
					"transport": "http",
					"url": "https://mcp.exa.ai/mcp",
					"header_env": {"X-Api-Key": "EXA_API_KEY"},
					"required": true,
					"allow_tools": ["web_search_exa", "web_fetch_exa"],
					"tool_prefix": "exa",
					"timeout": 60
				},
				{
					"name": "fs",
					"enabled": false,
					"transport": "stdio",
					"command": "npx",
					"args": ["-y", "@modelcontextprotocol/server-filesystem", "."]
				}
			]
		}
	}`

	var cfg struct {
		MCP struct {
			Servers []Entry `json:"servers"`
		} `json:"mcp"`
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("解配置出错: %v", err)
	}

	servers := ServerConfigs(cfg.MCP.Servers)
	if len(servers) != 2 {
		t.Fatalf("转出 %d 个 server，想要 2 个", len(servers))
	}

	exa := servers[0]
	if exa.Name != "exa" || exa.Transport != mcp.TransportHTTP {
		t.Fatalf("exa 的名字或传输不对: %+v", exa)
	}
	if exa.URL != "https://mcp.exa.ai/mcp" {
		t.Fatalf("exa 的 url = %q", exa.URL)
	}
	if !exa.IsEnabled() {
		t.Fatal("exa 没写 enabled，应该是启用")
	}
	if exa.Headers != nil {
		t.Fatalf("header_env 不该在这里解析成头值: %v", exa.Headers)
	}
	if exa.HeaderEnv["X-Api-Key"] != "EXA_API_KEY" {
		t.Fatalf("header_env 没带过去: %v", exa.HeaderEnv)
	}
	if !exa.Required || exa.ToolPrefix != "exa" || exa.Timeout != time.Minute {
		t.Fatalf("exa 的 required/tool_prefix/timeout 不对: %+v", exa)
	}
	if len(exa.AllowTools) != 2 || exa.AllowTools[0] != "web_search_exa" {
		t.Fatalf("exa 的 allow_tools 不对: %v", exa.AllowTools)
	}

	fs := servers[1]
	if fs.IsEnabled() {
		t.Fatal("fs 写了 enabled:false，应该是停用")
	}
	if fs.Transport != mcp.TransportStdio || fs.Command != "npx" || len(fs.Args) != 3 {
		t.Fatalf("fs 的 stdio 参数不对: %+v", fs)
	}
	if fs.Timeout != 0 {
		t.Fatalf("fs 没写 timeout，转换该原样留 0（默认值由 pi/mcp 补）: %s", fs.Timeout)
	}
}

// TestServerConfigsKeepsBadValues 钉住"转换不做校验"：坏值照搬，让 pi/mcp 那侧去报。
func TestServerConfigsKeepsBadValues(t *testing.T) {
	servers := ServerConfigs([]Entry{{
		Name:      "Exa 大写",
		Transport: "sse",
	}})

	if len(servers) != 1 {
		t.Fatalf("转出 %d 个 server，想要 1 个", len(servers))
	}
	if servers[0].Name != "Exa 大写" || servers[0].Transport != mcp.TransportType("sse") {
		t.Fatalf("坏值被改了: %+v", servers[0])
	}
}
