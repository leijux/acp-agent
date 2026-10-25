# 文档索引

代码目录（`internal/`、`cmd/`）下不放 `.md`，说明文档统一收在这里；
项目总览与快速开始见根目录 [`README.md`](../README.md)。

| 文档 | 对应代码 | 内容 |
|---|---|---|
| [kernel.md](kernel.md) | `internal/kernel/` | goja 内嵌 billion-context 上下文引擎：API、Store、Intl 短板、重新打包 |
| [tools.md](tools.md) | `internal/tools/` | 工具契约与并发调度模型、写栅栏、截断预算、自定义工具 |
| [agent.md](agent.md) | `internal/agent/` | Pi agent loop 移植：entry 树、turn loop、steering、压缩切点、事件 |
| [acp.md](acp.md) | `internal/acp/` | ACP v1 agent 侧：方法表、session/update、权限门禁、通知顺序 |
| [mcp.md](mcp.md) | `internal/mcp/` | MCP 客户端：mcp.json、官方 SDK 接入、工具命名与权限、结果映射 |
| [cli.md](cli.md) | `cmd/acp-agent/` | 终端 agent：flag、配置文件、斜杠命令、权限模式、输出约定 |

`internal/provider/`（OpenAI 兼容客户端与 DeepSeek 适配）暂无独立文档，
端点与模型矩阵运行 `acp-agent -endpoints` 查看，设计说明见根 README「结构」一节。

其他：`script/build-cjs.sh`（重新生成 `kernel.dist.cjs`）、`Taskfile.yml`（常用任务）。
