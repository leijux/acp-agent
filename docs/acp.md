# internal/acp — Agent Client Protocol（Agent 侧）

ACP v1 的 **Agent 侧**实现：newline-delimited JSON-RPC 2.0 over stdio，
外加一个把 `internal/agent` 与 `internal/tools` 接进来的参考后端。

```
浏览器/编辑器 (ACP client)
        │  initialize → session/new → session/prompt
        ▼
   ┌──────────────┐   session/update (notification)   ┌──────────────┐
   │ acp.Server   │ ─────────────────────────────────► │   Client     │
   │  (Handler)   │ ◄─── session/request_permission ── │  (Zed 等)    │
   └──────┬───────┘          fs/*, terminal/*          └──────────────┘
          │
   ┌──────▼───────┐   ┌──────────────┐
   │ internal/    │──►│ internal/    │
   │    agent     │   │    tools     │
   └──────────────┘   └──────────────┘
```

## 快速开始

```go
conn := acp.NewConn(os.Stdin, os.Stdout, nil)
handler, _ := acp.NewAgentHandler(acp.AgentConfig{
    Provider:   myProvider,
    Model:      agent.Model{Provider: "openai", ID: "gpt-5", ContextWindow: 200_000},
    Registry:   registry,
    Executor:   tools.NewExecutor(registry, 0),
    Permission: acp.PermissionAsk,           // 走 session/request_permission
    AutoApprove: []string{"read", "glob", "grep"},
})
srv, _ := acp.NewServer(conn, acp.ServerOptions{
    Handler: handler,
    Info:    acp.Implementation{Name: "my-agent", Version: "0.1.0"},
})
srv.Serve() // 阻塞到连接关闭
```

跑一遍自检（`cmd/acp-agent-demo` 已被移除；用库自带的 httptest 假模型验证）：

```bash
go test ./internal/acp/ -v      # net.Pipe 全链路，不需要 API key
```

作为真实 agent 进程启动（stdout 只走协议，日志进 stderr）：

```bash
go run ./cmd/acp-agent --acp -root . -permissions ask
```

## 文件分工

| 文件 | 职责 |
|---|---|
| `jsonrpc.go` | JSON-RPC 2.0 连接：读循环、pending 请求表、并发写、双向调用 |
| `schema.go` | 协议类型与常量（content block、session/update 变体、方法名） |
| `server.go` | 方法注册与分发、会话表、取消、错误码映射 |
| `session.go` | `internal/agent` 参考后端：会话生命周期、prompt、stopReason 映射 |
| `update.go` | `agent.Event` → `session/update` 翻译 + toolCallId 登记 |
| `client.go` | Agent 主动调用客户端（permission / fs / terminal） |
| `permission.go` | 把权限请求接进 `tools.Executor` 的调度 |
| `subagent.go` | **Client 侧**：拉起 ACP 子代理（`SpawnSubagent` / `Prompt`），用于 `delegate` 工具 |

## 三个容易踩的设计点

### 1. 权限门禁必须包到 Executor 用的那个 Registry

`tools.Executor` 自己持有 `Registry` 指针。如果门禁包了一个新 Registry、
却把 Executor 留在旧 Registry 上，工具会**照常执行**，客户端一条
`request_permission` 都收不到 —— 静默失效，且测试里若只断言「turn 成功」
就完全看不出来。

`AgentHandler.executorFor` 因此会在包装后重建/重绑 Executor。对应回归测试：
`TestToolCallEmitsUpdatesAndAsksPermission`、`TestGuardBlocksExecution`。

### 2. 包装工具必须继续实现 `tools.Mutation`

`Executor` 用 `Mutation.Mutates()` 决定能否并行。`guardedTool` 若不转发这个
接口，`write` / `edit` / `bash` 会被当成只读工具排进同一并发批次。
回归测试：`TestWrapRegistryPreservesMutation`。

但 `bash` 是个特例：它**故意**没有 `Mutation`（只读命令并行是保留能力），
只实现 `tools.PermissionMutation.PermissionMutates() == true`。
`DenyMutating` 三个条件都要查（ConcurrencySafe / Mutates / PermissionMutates），
只看前两个会让只读审查模式放行 shell。回归测试：`TestDenyMutatingDeniesBash`。

### 3. 通知必须串行处理

`session/update` 的到达顺序就是客户端渲染顺序。早期实现给每条通知起
goroutine，实测 `tool_call_update(completed)` 会晚于 `agent_message_chunk`
到达，UI 出现错序流式输出。现在 `Conn.serveNotification` 同步执行，
代价是通知 handler 不得阻塞。回归测试：`TestNotificationOrderPreserved`。

## toolCallId 的配对

权限请求要求带上与 `tool_call` 更新一致的 `toolCallId`：

```
agent.MessageEndEvent(assistant)      → 为每个 ToolCallBlock 发 tool_call(pending)
                                        并登记 name → [toolCallId]
agent.ToolExecutionStartEvent         → tool_call_update(in_progress)
   └─ tools.Execute → guard.Authorize → session/request_permission(toolCallId)
agent.ToolExecutionEndEvent           → tool_call_update(completed|failed)
```

`updates.takeToolID(name)` 按工具名弹出队首 ID，因此同一响应里重复调用同一
工具（两个 `read`）也能各自拿到正确的 ID。

## 权限拒绝 ≠ turn 失败

`Guard` 返回错误时，`guardedTool` 把它变成**工具结果**（`IsError`），
模型能看到「用户拒绝」并换策略；只有 `ErrPermissionCancelled`（turn 被取消）
才真正中断。这样客户端点「拒绝」不会让整个会话崩掉。

## 子代理：用 ACP 调自己

`delegate` 工具不跑在进程内，而是把本程序以 `--acp` 再拉起一个进程，用 ACP 协议驱动：

```
主进程 (REPL)  ──spawn──▶  acp-agent --acp（stdio JSON-RPC）
     │ client 侧                     │ agent 侧
     ├──── initialize / session/new ─▶│
     ├──── session/prompt ───────────▶│
     │◀─── session/update ────────────┤
     │◀─── session/request_permission ┤  ← 子代理的改动上报回主进程
```

为什么走进程而不是进程内：

- **上下文天然隔离**：子代理看不到主会话历史，不会被长对话污染；
- **崩溃隔离**：子代理 panic / 被杀不会带走主进程；
- **可换实现**：`SubagentSpec.Command` 指向任何 ACP agent 都行。

代价是每次 delegate 起一个进程（kernel 的 goja VM 初始化 ~35ms），相对一次
LLM 往返可以忽略；实测本地假 provider 下 delegate 往返 **19ms**。

安全约束（缺一不可）：

1. 子进程固定 `--permissions ask`，把 `edit/delete/move/execute` 上报回主进程，
   由主进程的 `guard` 按自己的模式（auto/ask/readonly）最终裁决 —— 否则
   delegate 就是绕过确认的后门；
2. 子进程固定 `--disable-tools delegate`，防无限递归；
3. 子进程固定 `--no-mcp`，不背主进程的 MCP 工具表；
4. 子进程 `--root` 继承主 root，不能在任意目录动手。

`subagentKindMutates` 把未知的 `toolKind` 一律当改动型处理（宁可多问一次）。

## 已知限制

- **MCP 服务器未接入**：`session/new` 的 `mcpServers` 会被记录在
  `ServerSession.MCPServers`，但没有拉起子进程。
- **`fs/*` 与 `terminal/*` 只做了客户端调用封装**，内置工具仍用本地文件系统；
  想走客户端能力需要在 `tools.Policy` 层做适配。
- **图片输入已直通**：`toAgentMessage` 把带 `data` 的 image 块保留为
  `agent.ImageBlock`，`internal/provider/openai.go` 再转成 `image_url`
  （`data:<mime>;base64,<data>`）。客户端只给 `uri` 不给 `data` 时才降级为
  文本占位。
- **`parallel` 工具的嵌套调用绕过权限门禁**：它内部持有原始 Registry。
  需要严格门禁时把 `parallel` 放进 `AgentConfig.OnlyTools` 之外（或用
  `tools.Options.Disable` 关掉）。
- **单会话单 prompt**：并发 `session/prompt` 会被拒绝（`-32600`），
  而不是排队 —— 两个 turn 交错会写坏同一条消息历史。

## 测试

```bash
go test ./internal/acp/ -race -v
```

用 `net.Pipe()` 拉真实客户端/服务端两个 `Conn`，覆盖：能力协商、版本拒绝、
流式 chunk、未知会话/方法、工具调用全链路（pending→in_progress→completed）、
权限允许/拒绝、`allow-always` 记忆、cancel、并发 prompt 拒绝、通知顺序。

## 子代理（delegate）

`delegate` 通过 ACP 协议驱动一个**独立进程**（默认是本程序自身 `--acp`），
所以子代理有独立的上下文、kernel 会话与工具状态，崩了也不会带走主进程。

关键约束：

- 子代理固定以 `--permissions ask` 启动，改动型工具会上报回主进程，
  由主进程的权限守卫最终裁决 —— 否则 delegate 就是绕过确认的后门；
- 启动参数里带 `--disable-tools delegate`，防止无限递归委派；
- 握手（`initialize` + `session/new`）受 `HandshakeTimeout`（默认 30s）约束，
  子代理不响应时报「子代理握手超时」而不是无限挂住；
- `Prompt` 取消后先发 `session/cancel`，等 `PromptGrace`（默认 5s），
  仍不结束就杀整棵进程树；
- 子代理 stderr 按**字节**转发并按行加 `[role] ` 前缀（单行上限 4KB），
  中文不会因为管道切分变成 `�`。
