---
name: go-env-report
description: 当用户询问 Go 环境信息、版本或构建环境配置时使用本技能。收集 Go 工具链信息并生成环境报告。
---

# Go 环境报告技能

按以下步骤收集环境信息：

1. 用 bash 执行 go version 获取工具链版本。
2. 用 bash 执行 go env GOPATH GOPROXY 获取关键环境变量。
3. 把版本与环境变量整理成一段不超过 5 行的报告直接输出，不写入任何文件。