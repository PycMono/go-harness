package mcp

import "time"

type TransportType string

const (
	TransportHTTP  TransportType = "http"  // Streamable HTTP
	TransportStdio TransportType = "stdio" // 子进程，走 stdin/stdout
)

const (
	// defaultClientName 是 clientName 没传时 initialize 报给远端的名字。
	defaultClientName = "go-harness"
	// clientVersion 写死，不配置：仓库里没有版本来源（`version.txt` 已经删了，
	// 它存的是 go 版本、而且是测试数据）。将来引入版本号，改这一处。
	clientVersion = "pro"
)

// defaultTimeout 是单次远端操作超时（一次连接、一次工具发现、一次工具调用各算一次）
// 在 Timeout 没配时的缺省值：Timeout 为 0 就是没配。
const defaultTimeout = 30 * time.Second

// defaultToolPrefix 是本地工具名的第一段：mcp__<server>__<远端名>。
const defaultToolPrefix = "mcp"

// toolNamePrefix 是配置里的 tool_prefix 落地的地方：写了就用它换掉第一段。
func toolNamePrefix(prefix string) string {
	if prefix == "" {
		return defaultToolPrefix
	}

	return prefix
}
