# acp-agent

一个纯 Go 的终端编码 agent：Pi 风格的 agent loop + 工具集 + 上下文压缩 + OpenAI 兼容 Provider + ACP 协议服务。

底座是 [`billion-context/kernel`](https://github.com/ranxianglei/billion-context/tree/master/kernel)（`acp-kernel`，一个 agent 上下文管理器），跑在**纯 Go 进程**里：goja 解释执行上游 JS，无 Node、无 cgo、可静态编译。上游提供消息 `<acp>` 引用标签、预算裁剪、tier 1/2/3 压缩分块、BM25 检索、可序列化 state。

## 结构

```
internal/kernel/               包本体（零外部依赖，除了 goja）
  kernel.go                    New / NewSession / Options
  session.go                   Turn / ApplyCompression / Search / Status / Blocks / Refs / CallCore
  persist.go                   Store 接口 + FileStore（原子写）+ MemoryStore
  types.go                     Message / Config / TurnResult / Nudge / Block / RefMap
  runtime.go                   goja VM + node:crypto/path/fs/module shim
  intl.go                      Segmenter 接口 + RuneSegmenter（替代缺失的 Intl.Segmenter）
  kernel.dist.cjs              go:embed 的上游产物（约 400 KB）

internal/tools/                 Go 版 Pi 工具集（bash 默认用 mvdan/sh 内置解释器，零外部依赖，PATH 缺 coreutils 时用内建命令回退）
  tool.go                      Tool / Mutation 接口、Result/Block、Args、JSON Schema 辅助
  registry.go                  Registry + Executor（按 ConcurrencySafe 自动分批并发）
  builtin.go                   read / write / edit / ls / glob / grep
  bash.go                      bash / bash_output / bash_kill + 后台进程管理
  search.go                    glob 匹配与正则缓存
  builtin_ext.go               parallel 元工具 / todo
  js.go                        js（内置 goja，零依赖沙箱计算）
  web.go                       web_search / web_fetch（网络后端由 internal/web 注入）
  agent.go                     delegate + DefaultRegistry
  safety.go / truncate.go      路径策略、命令拦截、截断预算

internal/agent/                Pi agent loop 的 Go 移植
  message.go                   AgentMessage 联合、content block、Usage、stopReason
  session.go                   会话 entry 树 + active branch 投影 + JSONL 持久化
  provider.go                  Provider / Request / Sink / ScriptedProvider / token 估算
  context.go                   system prompt 组装、上下文文件发现、convertToLlm
  agent.go                     turn loop、steering / follow-up / abort、工具桥接
  compact.go                   压缩：kernel 划定区间 → 摘要 → 压缩块
  kernel.go                    kernel 桥接：消息转换、nudge 驱动、块镜像
  events.go                    生命周期事件与 Emitter

internal/acp/                  ACP v1 Agent 侧（stdio JSON-RPC）+ Client 侧子代理
  jsonrpc.go                   双向 JSON-RPC：Call / Notify / Handle、pending 表
  schema.go                    协议类型、session/update 常量、方法名、错误码
  server.go                    Server / Handler / ServerSession
  session.go                   AgentHandler、PermissionMode、stopReason 与 Usage 映射
  update.go                    agent.Event → session/update 翻译、toolCallId 配对
  permission.go                Guard 与 WrapRegistry（权限门禁）
  client.go                    Client 侧：req_permission、fs 读写、terminal

internal/provider/             OpenAI 兼容 Provider（零依赖）
  sse.go                       SSE 事件解析 / 编码
  openai.go                    Chat Completions 客户端：流式、工具调用、usage
  retry.go                     指数退避 + Retry-After
  models.go / deepseek.go      模型注册表（含峰谷定价）与 DeepSeek 适配

internal/mcp/                  MCP 客户端（官方 go-sdk，只用 tools 子集）
  config.go                    mcp.json 解析、${ENV} 展开、多来源合并
  client.go                    单 server：SDK 会话、stdio/HTTP 传输、进程树回收
  manager.go                    多 server 编排、工具聚合、list_changed 同步
  tool.go                      远程工具 → tools.Tool，schema 归一化与内容块映射

internal/web/                  web_search / web_fetch 的后端（duckduckgo/searxng/tavily/brave/exa）
  web.go                       Client、Config、Fetch 包装（字节+字符双上限）
  search.go                    各家搜索后端与 DDG HTML 解析（含验证页识别）
  html.go                      HTML → 纯文本（去 nav/footer/script，<pre> 保留围栏）
  charset.go                   BOM / charset / meta 猜编码，回退 GB18030

internal/proc/                 跨平台子进程树：Unix 进程组 / Windows Job Object

cmd/acp-agent/                 可执行的终端 agent（REPL / -p / --acp）
  main.go config.go            配置、flag、Provider 装配
  permission.go                工具权限闸门（ask/auto/readonly）
  subagent.go                  delegate 的 ACP 自举（buildSpec / 权限上报 / 结果格式化）
  web.go                       web 后端接线（配置 → tools.SearchProvider/FetchProvider）
  repl.go                      终端渲染与斜杠命令
  main_test.go                 端到端测试（假模型 + 工具闭环 + 会话恢复）
  markdown.go                  流式 Markdown → ANSI 渲染（跨 chunk 的围栏状态机）
  markdown_test.go             分段流式与整块渲染等价性
cmd/verify-kernel/             裸 goja 全链路验证（不复用包）

script/build-cjs.sh            上游 ESM → goja 可执行 CJS(ES2017)

内部的 .md 一律不放，文档统一在：
docs/                          文档索引
  kernel.md                    kernel 包：用法、限制、重新打包
  tools.md                     并发调度模型、自定义工具、可切换的 shell 引擎
  agent.md                     Pi 语义对照表与实现细节
  acp.md                       ACP 协议实现、权限门禁、通知顺序
  mcp.md                       MCP 客户端：配置、工具命名与权限、结果映射
  cli.md                       acp-agent 命令：flag、配置、斜杠命令
```

## 快速开始（CLI）

```bash
export DEEPSEEK_API_KEY=sk-...      # 或 OPENAI_API_KEY / MOONSHOT_API_KEY …

acp-agent                            # 交互式 REPL
go run ./cmd/acp-agent -p "把 go.mod 的 Go 版本升到 1.28 并跑测试"
acp-agent --acp                      # 作为 ACP agent 服务（stdio JSON-RPC，供 Zed 等）
acp-agent --provider ollama --model qwen3-coder
acp-agent -endpoints                 # 内置端点与默认模型
acp-agent -mcp-status                # 连接 MCP server 并列出工具
acp-agent --mcp-json '{"fs":{"command":"npx","args":["-y","@modelcontextprotocol/server-filesystem","."]}}'  # 临时注入 MCP server
```

MCP 工具从工作目录的 `.mcp.json` 与 acp-agent 配置目录（`%AppData%\acp-agent` 或 `~/.config/acp-agent`）
下的 `.mcp.json` 自动加载（Claude Desktop 兼容格式，后者覆盖前者同名 server），
也可以用 `--mcp-config` 指定单个文件，或直接传一段 JSON 字符串；
`--mcp-json` 则是在已加载配置之上叠加（同名 server 覆盖）—— 不改任何文件就能临时多挂一个 server。
远程工具统一以 `mcp__<server>__<tool>` 命名，
并和内建工具共享同一套权限门禁 —— 只有声明了 `readOnlyHint` 的才免确认。
详见 [docs/mcp.md](docs/mcp.md)。

REPL 命令：`/help /new /compact /context /model /tools /cost /save /session /init /exit`。

- REPL 边流边渲染 Markdown（标题/粗体/代码块/列表/引用/链接），围栏内不误判格式；
  非 TTY 或 `--no-color` 自动退化为去标记纯文本，`-p` 模式不渲染。

- `-p` 模式：**stdout 只有最终结果**，工具调用与用量日志走 stderr，可直接管道。
- 权限三档：`ask`（默认，改动前问）、`auto`（不问）、`readonly`（拒绝一切改动）。
- 会话自动落盘为 JSONL，按工作目录分区（`sessions/<sha1(root)[:12]>/`）；
  `-c` 只接本项目最近一次，REPL 与 `-p` 行为一致；`/save` 指定路径。
- 上下文压缩由 kernel 驱动（nudge 划定区间，模型生成摘要），压缩事件实时上报。

### 作为库使用

```go
prov := provider.NewOpenAI(provider.OpenAIConfig{APIKey: key, BaseURL: "https://api.deepseek.com"})
reg, _ := tools.DefaultRegistry(tools.Options{Policy: tools.Policy{Root: "."}})
ag := agent.New(agent.Options{
	Provider: prov, Model: provider.ResolveModel("deepseek", "deepseek-flash"),
	Registry: reg, Session: agent.NewSession(),
})
res, err := ag.Run(ctx, "解释 internal/kernel 的压缩算法")
```

## 快速开始（kernel）

```go
k, _ := kernel.New()
sess, _ := k.NewSession(kernel.Config{ModelContextLimit: 200_000})
defer sess.Close()

res, _ := sess.Turn(history, promptTokens)  // → res.Messages（已打 <acp> 标签）、res.Nudge
r := res.Nudge.CompressibleRanges[0]        // kernel 推荐的压缩区间（已排除活跃块/受保护内容）
cres, _ := sess.ApplyCompression([]kernel.Range{{
	StartRef: r.StartRef, EndRef: r.EndRef, Topic: "上下文预算", Summary: "SUMMARY: …",
}})

info, _ := sess.Persist(ctx, store, "user-42")  // store 见下文
sess2, _ := k.NewSessionFrom(ctx, store, "user-42", cfg)
```

持久化后端：`kernel.NewFileStore(dir)`（JSON 文件，零依赖）或 `kernel.NewMemoryStore()`（测试）。
`Store` 只有 4 个方法（`Save`/`Load`/`Delete`/`List`），要接 Redis / SQLite / 对象存储，自写几十行即可，
参见 [`docs/kernel.md`](docs/kernel.md#sqlite)。

> `internal/kernel` 不只是验证成果包：**`cmd/acp-agent` 的压缩决策完全依赖它**
> （何时压、压哪一段由 kernel 的 nudge 给出）。`internal/agent` 只保留阈值预判与摘要生成。

## 验证

```bash
task check                       # 格式 + vet + 全量测试
task test:race                   # 竞态检测
task verify:kernel               # 裸 goja 全链路验证
```

等效的原始命令：

```bash
go test ./...                    # 含与 Node 的 hash 一致性断言
go run ./cmd/verify-kernel
go run ./cmd/acp-agent -endpoints
```

同一 workload 下 `messagesHash` 与 `blockNoTimeHash` 与 Node **逐字节一致**（`6a8561416f02cde1` / `0cbd1741d253e587`）。

## 已知限制

1. **中文分词依赖构建 tag**：goja 无 `Intl.Segmenter`。带 `segmenter ne` tag 构建（`task build:full`，发布矩阵用的就是它）时
   **默认就用真词典 gse**；轻量构建（`task build`，无 tag）里只有 `RuneSegmenter`（「单字 + 连续 ASCII 词」切分，
   CJK 段在 kernel 内做 bigram 回退），检索召回够用但词级精度低于 Node 的 CLDR。
   `gse` 自带高频**简体**词表（10 万条，1.3MB），纯 Go、零外部文件，加载约 50ms / 29MB 分配。
   只认简体：繁体文本退化成单字，靠 bigram 兜底。

   默认会**额外输出 CJK bigram**：kernel 的 `cjkRunTokens` 只要 run 内有多字词就会丢掉全部单字段，
   直通分词结果会让 `壓縮引擎` 只剩 `[引擎]`、查询 `缩引` 命中 0 —— 补上 bigram 后词给精度、
   bigram 保召回。（要关掉只能用 config.json 的 `"segmenterExpand": "off"`，命令行没有开关。）

   体积：轻量版 20MB → full 版 22MB（`task build:full`，即 `-tags "segmenter ne"`）。
   `ne` 不能漏：没有它 gse 自己 embed 的简/繁/日词典（~31MB）会被 linker 保留，
   体积直接飙到 52MB。要回退到 `rune`（例如子代理这种不做检索的场景），
   用 config.json / `ACP_AGENT_SEGMENTER=rune` —— 分词器没有命令行开关。
   只影响中文 BM25 召回，**不影响压缩结果的正确性**（hash 仍与 Node 一致）。

2. **`processTurn` 比 Node 慢，且随消息数超线性增长**：200 条消息约 68ms（三次优化前 198ms），
   400 条 200ms，800 条约 727ms；Node 同 workload 约 8.8ms。原因三层：上游算法对每条消息做多趟全表扫描（O(N²)）、
   goja 是解释执行、JS↔Go 边界要序列化消息。**对策是把活跃消息控制在 200–300 条并及时压缩**，
   而不是指望 Go 侧做增量 —— `processTurn` 的语义要求全量历史。详见 `docs/kernel.md`「性能与并发」。
3. goja 无 `structuredClone` / `TextEncoder` / `queueMicrotask`，kernel 未使用；async 只在脚本边界 drain（kernel 的同步 API 不受影响）。

详见 [`docs/kernel.md`](docs/kernel.md)。
