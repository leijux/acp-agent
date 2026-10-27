# acp-agent

纯 Go 的终端编码 agent：Pi 风格的 agent loop + 工具集 + 上下文压缩 + OpenAI 兼容 Provider + ACP 协议服务。

底座是 [`billion-context/kernel`](https://github.com/ranxianglei/billion-context/tree/master/kernel)（`acp-kernel`，agent 上下文管理器），跑在**纯 Go 进程**里：goja 解释执行上游 JS，无 Node、无 cgo、可静态编译。压缩中的 `<acp>` 引用标签、tier 1/2/3 分块、BM25 检索都来自上游。

## 快速开始

```bash
export DEEPSEEK_API_KEY=sk-...        # 或 OPENAI_API_KEY / MOONSHOT_API_KEY …
task build                            # → bin/acp-agent（轻量版，约 20MB）

acp-agent                             # 交互式 REPL（Bubble Tea）
acp-agent -p "把 go.mod 的 Go 版本升到 1.28 并跑测试"   # 一次性执行
acp-agent --acp                       # 作为 ACP agent 服务（stdio JSON-RPC，供 Zed 等）
acp-agent --provider ollama --model qwen3-coder
acp-agent -c                          # 续接当前目录最近一次会话
acp-agent -endpoints                  # 内置端点与定价
acp-agent -mcp-status                 # 连 MCP server 并列出工具
```

配置优先级 **flag > 环境变量 > `config.json` > 内置默认**；配置文件在 `%AppData%\acp-agent\config.json`（Linux/macOS：`~/.config/acp-agent/config.json`），字段与全部 flag 见 [docs/cli.md](docs/cli.md)。

## 交互界面

- 提/答分块；**思考块可折叠**（`Ctrl-T` 或点它），一轮结束自动收起。
- **鼠标左键拖选，松开即复制**；`Ctrl-Y` 复制最近一条回复，`/copy [n]` 复制倒数第 n 条（纯文本，不含思考/工具）。
- `Ctrl-↑/↓` 在上一条/下一条问答间跳转；`PgUp/PgDn`/滚轮滚动。
- 顶部常驻「模型 / ctx 占用百分比 / 权限 / 工作目录」，右下角显示 `● 运行中 12s`。
- 不占 alt-screen：`/freeze` 把当前全文写进终端 scrollback，之后可用终端原生滚动/搜索回看。
- `-c`（或 `--session <文件>`）启动即回放历史（最近 160 段，旧思考默认收起）；`--no-tui` 退回旧行式 REPL。
- 正文边流边渲染 Markdown（代码围栏内不误判格式）；非 TTY / `--no-color` 退化为去标记纯文本。

REPL 命令：`/help /new /compact /context /model /tools /cost /copy /save /session /init /mouse /freeze /exit`。

## 关键行为

- `-p` 模式：**stdout 只有最终结果**，工具调用与用量日志走 stderr，可直接管道。
- 权限三档：`ask`（默认，改动前问）、`auto`、`readonly`（拒绝一切改动）。
- 会话自动落盘为 JSONL，按工作目录分区（`sessions/<sha1(root)[:12]>/`）；`-c` 只接本项目最近一次。
- 上下文压缩由 kernel 驱动（何时压、压哪一段由它的 nudge 给出），压缩事件实时上报。
- **MCP**：自动加载工作目录与配置目录下的 `.mcp.json`（后者覆盖同名 server），也可 `--mcp-config <文件>` / `--mcp-json <JSON>`（后者是叠加）；远程工具名为 `mcp__<server>__<tool>`，与内建工具共用一套权限门禁。
- **子代理**：`delegate` 用 ACP 拉起自身（自动加 `--permissions ask --disable-tools delegate --no-mcp`），子代理的工具确认回传主进程裁决；启动/工具调用/耗时/结论预览回显在主视图。
- 工具集：`read` `write` `edit` `ls` `glob` `grep`、`bash`/`bash_output`/`bash_kill`、`js`、`web_search`/`web_fetch`、`parallel`、`todo`、`delegate`，以及压缩用的 `context_check`/`context_apply`/`context_search`/`context_decompress`。`bash` 默认走内置 mvdan/sh 解释器，PATH 没 coreutils 时用内建命令回退。

## 作为库使用

```go
prov := provider.NewOpenAI(provider.OpenAIConfig{APIKey: key, BaseURL: "https://api.deepseek.com"})
reg, _ := tools.DefaultRegistry(tools.Options{Policy: tools.Policy{Root: "."}})
ag := agent.New(agent.Options{
	Provider: prov, Model: provider.ResolveModel("deepseek", "deepseek-flash"),
	Registry: reg, Session: agent.NewSession(),
})
res, err := ag.Run(ctx, "解释 internal/kernel 的压缩算法")
```

```go
k, _ := kernel.New()
sess, _ := k.NewSession(kernel.Config{ModelContextLimit: 200_000})
res, _ := sess.Turn(history, promptTokens)   // → res.Messages（已打 <acp> 标签）、res.Nudge
r := res.Nudge.CompressibleRanges[0]         // 推荐的压缩区间（已排除活跃块/受保护内容）
sess.ApplyCompression([]kernel.Range{{StartRef: r.StartRef, EndRef: r.EndRef, Topic: "…", Summary: "…"}})
sess.Persist(ctx, store, "user-42")          // store: NewFileStore(dir) / NewMemoryStore() / 自写 4 个方法
```

## 验证

```bash
task check          # 格式 + vet + 全量测试
task test:race      # 竞态检测
task verify:kernel  # 裸 goja 全链路验证
```

同一 workload 下 `messagesHash` / `blockNoTimeHash` 与 Node **逐字节一致**（`6a8561416f02cde1` / `0cbd1741d253e587`）。

## 已知限制

1. **中文分词精度依赖构建 tag**：goja 没有 `Intl.Segmenter`。`task build:full`（`-tags "segmenter ne"`，发布矩阵用它）默认走真词典 gse；轻量构建只有单字 + bigram 回退，召回够用但词级精度低于 Node 的 CLDR。`ne` 不能漏，否则 gse 自带的 31MB 词典会被链进来（20MB → 52MB）。只影响中文 BM25 召回，不影响压缩结果的正确性。
2. **`processTurn` 比 Node 慢且随消息数超线性增长**（200 条约 68ms，800 条约 727ms；Node 8.8ms）：上游算法要求全量历史 + goja 解释执行 + JS↔Go 序列化。对策是把活跃消息控制在 200–300 条并及时压缩。
3. goja 缺 `structuredClone` / `TextEncoder` / `queueMicrotask`（kernel 未使用）；async 只在脚本边界 drain。

细节见 [docs/kernel.md](docs/kernel.md)。

## 目录

```
internal/kernel/    goja 里的上下文引擎（Turn / ApplyCompression / Search / persist）
internal/agent/     Pi agent loop：entry 树会话、turn loop、压缩编排、事件
internal/tools/     Go 版 Pi 工具集（bash 内置解释器、并发调度、路径与命令策略）
internal/acp/       ACP v1：Agent 侧（stdio JSON-RPC）+ Client 侧子代理
internal/tui/       Bubble Tea 块视图（行 sink、布局/视口、拖选、自绘输入行）
internal/provider/  OpenAI 兼容流式客户端、SSE、重试、模型注册表（含峰谷定价）
internal/mcp/       MCP 客户端（官方 go-sdk，只用 tools 子集）
internal/web/       web_search / web_fetch 后端（duckduckgo/searxng/tavily/brave/exa）
internal/proc/      跨平台子进程树回收（Unix 进程组 / Windows Job Object）
cmd/acp-agent/      可执行程序（TUI REPL / -p / --acp / 子代理自举）
cmd/verify-kernel/  裸 goja 全链路验证
```

仓库里的 `.md` 只放 `docs/`：kernel · tools · agent（Pi 语义对照）· acp · mcp · cli。
