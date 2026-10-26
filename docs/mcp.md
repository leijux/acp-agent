# MCP 支持

`internal/mcp` 让 agent 使用 [Model Context Protocol](https://modelcontextprotocol.io) 服务器提供的工具。
协议与传输完全交给官方 Go SDK [`github.com/modelcontextprotocol/go-sdk`](https://github.com/modelcontextprotocol/go-sdk)，
本包只负责三件事：

1. **配置语义** —— 读 `mcp.json`（与 Claude Desktop / Pi 兼容），展开 `${ENV}`，合并多来源。
2. **进程生命周期** —— stdio 子进程启动前的进程组/Job Object 设置，退出时回收整棵进程树（复用 `internal/proc`）。
3. **工具适配** —— `*mcp.Tool` → `tools.Tool`，名字命名空间、schema 归一化、内容块映射、结果截断、权限语义。

```
mcp.json ──▶ Config ──▶ Manager ──┬── Client (SDK) ──▶ stdio / streamable-http
                                  └── remoteTool ×N ──▶ tools.Registry ──▶ 权限门禁 ──▶ Executor
```

## 配置

自动加载两个位置，**后者覆盖前者**的同名 server：

| 顺序 | 路径 | 用途 |
|---|---|---|
| 1 | `<工作目录>/.mcp.json` | 项目级，可随仓库分发 |
| 2 | `<配置目录>/.mcp.json` | 用户级，跨项目共享 |

「配置目录」= `$ACP_AGENT_CONFIG_DIR`，未设置时用系统用户配置目录：
Windows `%AppData%\acp-agent`、Linux/macOS `~/.config/acp-agent`。
该目录同时存放 `config.json` 与 `sessions/`。

用 `--mcp-config <path>` 可跳过自动加载，只读指定文件（不与其他文件合并）。
想复用于其他客户端的配置时用它 —— 例如 `--mcp-config ~/.pi/agent/mcp.json`。

### 命令行直接给配置

不想动文件时，两种内联写法：

```bash
# 1. 完全取代配置文件（与传路径语义一致，不读默认位置）
acp-agent --mcp-config '{"mcpServers":{"playwright":{"command":"npx","args":["-y","@playwright/mcp"]}}}'

# 2. 在已加载配置之上叠加，同名 server 覆盖（"临时注入" 的常规做法）
acp-agent --mcp-json '{"playwright":{"command":"npx","args":["-y","@playwright/mcp"]}}'
```

两个 flag 都认两种 JSON 形状：完整格式 `{"mcpServers":{...}}`，以及省略包装的
`{"<server 名>": {...}}`（后者少一层括号，适合手敲）。`${VAR}` 照常展开，
也能用环境变量 `ACP_AGENT_MCP_JSON` 传 —— 便于脚本/CI 里按需求注入。

`--mcp-config` 以 `{` 开头就按内联 JSON 处理（路径不会以 `{` 开头），
所以不需要为「路径还是字符串」换 flag。

两点限制：

- `--no-mcp` 优先：两者同时给时不连任何 server。
- 内联配置不会被 `delegate` 子代理继承 —— 子代理一律以 `--no-mcp` 启动；
  需要子代理也用某个 server 时，在 `subagent.args` 里显式加 `--mcp-json`。

```json
{
  "mcpServers": {
    "filesystem": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-filesystem", "."],
      "env": { "NODE_OPTIONS": "--max-old-space-size=512" },
      "timeout": 30
    },
    "docs": {
      "url": "https://example.com/mcp",
      "headers": { "Authorization": "Bearer ${DOCS_TOKEN}" }
    },
    "noisy": {
      "command": "some-server",
      "exposure": "hidden"
    }
  }
}
```

| 字段 | 说明 |
|---|---|
| `command` / `args` / `env` / `cwd` | stdio 传输 |
| `url` / `headers` | streamable HTTP 传输 |
| `type` | `stdio` \| `http` \| `streamable-http`；缺省由 `command`/`url` 推断 |
| `timeout` | 单次请求超时（秒），默认 60 |
| `enabled` | `false` 时跳过该 server |
| `exposure` | `direct`（默认，工具给模型）\| `hidden`（注册但不给模型）。其他取值按 `direct` 处理 —— 配置文件要能跨工具复用（Pi 用 `codemode`） |

`${VAR}` 与 `$VAR` 在 `command`、`args`、`url`、`headers`、`env`、`cwd` 中展开；变量缺失时替换为空串。

## 工具命名与权限

- 命名空间：`mcp__<server>__<tool>`，非 `[A-Za-z0-9_-]` 字符替换为 `_`。
  同名冲突时 `Manager.RegisterInto` 跳过而不是覆盖，内建工具优先。
- **权限**：MCP 工具在 `wrapRegistry` 之前注册，因此和内建工具走同一套门禁。
  只有服务端在 `annotations.readOnlyHint` 显式声明只读时才免确认；
  未声明即视为会改状态（`Mutates() == true`），`ask` 模式下要人工确认，`readonly` 模式下被拒。
- **并发**：`readOnlyHint` 工具标记为 `ConcurrencySafe`，可与其他只读工具并行；
  其余由 `Executor` 串行调度。

## 结果映射

| MCP 内容 | 工具结果 |
|---|---|
| `TextContent` | `KindText` |
| `ImageContent` | `KindImage`（base64 原样透传） |
| `EmbeddedResource`（文本） | `KindText`，加 `--- uri ---` 分隔 |
| `EmbeddedResource`（`image/*` blob） | `KindImage` |
| `EmbeddedResource`（其他 blob） | `KindText`，只报 uri/mime/大小 |
| `AudioContent` | `KindText` 占位说明（模型不吃音频） |
| `ResourceLink` | `KindText` 一行引用 |
| `structuredContent` | 进 `Result.Details`；无内容块时序列化为文本 |

单次结果超过 100 KiB 按字节截断（不切开 UTF-8 字符），并追加「已截断 N 字节」提示。
`isError: true` 映射为 `Result.IsError`，回传给模型让它自己重试，而不是当成 Go error。

## schema 归一化

上游 server 常写 `{"type":"object"}` 而省略 `properties`。直接透传会让模型请求里出现
`"properties": null`，OpenAI 以 `null is not of type "object"` 拒绝整轮对话。
`normalizeSchema` 统一补齐：`type` 缺省为 `object`，`properties` 缺省为空对象。

## 诊断

```bash
acp-agent -mcp-status
```

先打印实际读到的文件与搜索顺序，再连接所有配置的 server，
逐个打印状态、协议版本、工具清单，并标出哪些工具会触发确认。
排查「配置写对了但模型看不到工具」的最短路径。

## 关键设计取舍

- **连接失败不阻断启动**。远程 server 常因网络/凭据/依赖不可用；为此让 CLI 起不来是过度反应。
  错误聚合后打到 stderr，其余 server 照常工作。
- **不做自动重连**（`MaxRetries: -1`）。SDK 的重连会在后台维持长连接，
  对一次性 CLI 进程是资源浪费；需要时由用户重启。
- **`tools/list_changed` 同步**：SDK 回调 → `Client` 重拉列表 → `Manager.SyncRegistry`，
  移除已下线的工具，避免模型调用到不存在的工具。
- **不实现 resources / prompts / sampling**。当前只消费 tools —— 这是编码 agent 的实际需求面。
  elicitation 也暂不支持（远程 server 会在需要时收到 `method not found`）。
