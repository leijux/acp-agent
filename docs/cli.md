# cmd/acp-agent

一个可执行的终端编码 agent。三种入口共享同一套 `internal/agent` 会话与工具：

```
acp-agent                     交互式 REPL（流式渲染 + 斜杠命令）
acp-agent -p "任务"            非交互，stdout 只输出结果
acp-agent --acp               ACP agent 服务（stdio JSON-RPC）
```

## 配置

优先级：**命令行 flag > 环境变量 > `config.json`**。

```bash
acp-agent -endpoints          # 列出内置端点与默认模型
```

配置文件位置（`ConfigPath()`）：`$XDG_CONFIG_HOME/acp-agent/config.json`，
Windows 为 `%AppData%\acp-agent\config.json`；`ACP_AGENT_CONFIG` 可指定其它路径。

```json
{
  "provider": "deepseek",
  "model": "deepseek-flash",
  "permissions": "ask",
  "maxTurns": 100,
  "temperature": 0.7,
  "thinkingLevel": "medium",
  "root": "D:/golang/acp-agent",
  "disableTools": ["web_search"]
}
```

| 配置项 | 环境变量 | 说明 |
|---|---|---|
| `provider` | `ACP_AGENT_PROVIDER` | 默认 `deepseek` |
| `model` | `ACP_AGENT_MODEL` | 缺省用端点默认模型 |
| `apiKey` | `DEEPSEEK_API_KEY` / `OPENAI_API_KEY` / … | 也可 `--api-key`；本地端点（localhost）允许为空 |
| `baseURL` | `ACP_AGENT_BASE_URL` | 覆盖端点 |
| `permissions` | `ACP_AGENT_PERMISSIONS` | `ask` / `auto` / `readonly` |
| `root` | `ACP_AGENT_ROOT` | 工具可访问范围，默认 cwd，启动时转绝对路径 |
| `session` | `ACP_AGENT_SESSION` | 显式指定会话文件；不设则按项目目录自动分配（见下） |
| `maxTurns` | `ACP_AGENT_MAX_TURNS` | 单次 `Run` 的 assistant 轮数上限，默认 100 |
| `temperature` | `ACP_AGENT_TEMPERATURE` | 非法值静默忽略 |
| `thinkingLevel` | `ACP_AGENT_THINKING` | `off` / `minimal` / `low` / `medium` / `high` |
| `disableTools` | — | 不注册的工具名数组 |
| `noMCP` | — | `true` 时不连接任何 MCP server（子代理默认开启） |
| `subagent` | — | 子代理启动方式，见下表；缺省用自身 + `--acp` |
| `web` | `ACP_AGENT_WEB_SEARCH_PROVIDER` 等 | `web_search` / `web_fetch` 后端，见下 |
| — | `ACP_AGENT_DATA` | 数据目录（sessions 的父目录），默认同 `DataDir()` |
| — | `ACP_AGENT_API_KEY` | 兜底密钥，优先于端点自己的 `EnvKey` |

> 环境变量只在**对应 flag 未指定**时生效，它本身又会覆盖 `config.json` 里的值。

### web 后端

默认 DuckDuckGo，**不需要任何 key**：

```json
{
  "web": {
    "search": {
      "provider": "duckduckgo",
      "apiKey": "",
      "baseURL": "",
      "numResults": 5
    },
    "fetch": {
      "timeoutMs": 20000,
      "maxBytes": 5242880,
      "maxChars": 40000
    }
  }
}
```

| `search.provider` | 额外要求 |
|---|---|
| `duckduckgo`（默认） | 无。但国内网络需 `HTTPS_PROXY`，且易被限流 |
| `searxng` | `baseURL` 指向自建实例，且已开 `format=json` |
| `tavily` / `brave` / `exa` | `apiKey`（或 `TAVILY_API_KEY` / `BRAVE_API_KEY` / `EXA_API_KEY`） |
| `off` | 彻底关闭：两个工具会从注册表里摘掉（模型看不到） |

```bash
acp-agent --web-search tavily --web-search-key tvly-xxx -p "最新 Go 1.28 有什么变化"
acp-agent --no-web -p "离线看代码"                # 关掉 web
ACP_AGENT_WEB_SEARCH_PROVIDER=searxng \
  ACP_AGENT_WEB_SEARCH_BASE_URL=https://sx.example acp-agent
```

`--web-search` 只换 provider 名；`baseURL` 用 `web.search.baseURL`
或 `ACP_AGENT_WEB_SEARCH_BASE_URL` 配。代理走环境变量（Go 的默认 Transport
自动读 `HTTPS_PROXY`/`HTTP_PROXY`）。

### 子代理（`delegate` 工具）

`delegate` 默认**用 ACP 把本程序当作子代理拉起**（不是进程内调用）：

```json
{
  "subagent": {
    "command": "self",
    "model": "deepseek-flash",
    "permissions": "ask",
    "args": [],
    "env": []
  }
}
```

| 字段 | 说明 |
|---|---|
| `command` | `self`（默认）或任意 ACP agent 可执行文件，例如 `claude-code-acp` |
| `model` | 子代理模型；缺省用主会话模型 |
| `permissions` | 子代理权限档位；缺省 `ask`，让改动型工具上报回主进程裁决 |
| `args` | 追加在自动生成参数之后 |
| `env` | 追加环境变量 `KEY=VALUE` |

自动附加的参数：`--acp --no-color --root <主 root> --permissions ask --disable-tools delegate --no-mcp`。
最后两项是关键：禁用 delegate 防无限递归，禁用 MCP 避免子代理背着主进程的远程工具。

```bash
acp-agent --no-subagent -p "..."        # 关掉 delegate
acp-agent --subagent claude-code-acp    # 换成第三方 ACP agent
acp-agent --no-mcp -p "..."             # 自己不连 MCP
```

## 会话存储（按项目目录分区）

会话不是平铺的：`-c` 只接**当前项目**的会话，不会把 A 项目的对话接到 B 项目上。

```
<DataDir>/sessions/
  0ecc49192a0b/                 # sha1(绝对工作目录)[:12]
    .cwd                        # 原始路径（给人看的反查表）
    session-20261025-182627.232075700.jsonl
  7683b2fedb4a/
    .cwd
    session-<日期-时间>.jsonl
```

- 分区键是 `root`（`--root` / `ACP_AGENT_ROOT` / 配置的 `root`，默认 cwd）的绝对路径；
  Windows 上大小写折叠，尾部斜杠会被归一。
- 用哈希而非路径本身做目录名：Windows 路径可能超 260 字符，也避免泄漏用户名。
- 升级前平铺在 `sessions/` 下的旧文件仍然可用：本目录没有会话时 `-c` 会回退去扫旧目录。
- `--session <path>` 显式指定时完全绕过以上逻辑。

```bash
acp-agent -c                   # 接本项目最近一次会话（REPL 与 -p 行为一致）
acp-agent --session ./x.jsonl  # 指定文件
```

## 三个模式

### 1. REPL

- 正文流式输出，**边流边做 Markdown 渲染**；`[思考]` 段用暗色实时渲染。
- 工具调用显示为 `→ name args`，结束补 `✓/✗`、耗时、`[并行]` 标记。
- 压缩时打印 `⚕ 上下文已压缩：A → B token`。
- 运行中按 `Ctrl-C` 中断当前 turn（不是退出进程）；再按一次退出。

#### Markdown 渲染

渲染器在 `cmd/acp-agent/markdown.go`，**不是**先收完再渲，而是每个 chunk 追加到行缓冲，
遇到 `\n` 就渲染整行——因此 `**粗` / `体**` 被拆到两个 chunk 也能正确出粗体。

| Markdown | 终端效果 |
|---|---|
| `# 标题` / `### 标题` | 粗体（H1/H2 青色） |
| `**粗体**` `*斜体*` `~~删除~~` | 加粗 / 斜体 / 删除线 |
| `` `code` `` | 黄色；外层反引号去掉 |
| `[文本](url)` | 文本蓝色 + 暗色 `(url)` |
| `- item` / `1. item` | `• item` / `1. item`，保留缩进层级 |
| `> 引用` | 暗色 `│ ` 前缀 |
| ```` ```go ```` 围栏 | 内容缩进 2 格变暗，顶部标 `── go ──`；围栏内**不再解析** `*`/`_` |
| `---` | 暗色横线 |
| `\| a \| b \|` 表格 | 完整边框（`┌┬┐ ├┼┤ └┴┘`）＋ `│` 分列、表头加粗、自动列宽（CJK/emoji 算 2 列）、超宽按终端宽度截断为 `…`、`---` 分隔行不输出 |

细节：

- 代码围栏是**跨 chunk 状态机**，`` ``` `` 本身不输出；围栏内 `func f(*int)` 里的 `*` 不会被当成斜体。
- 超过 400 字符仍未遇到换行的段落会先原样刷出，避免「整段一行」的模型输出看起来卡住（表格行除外，
  半张表提前吐出会打乱排版）。
- 表格是**块级缓冲**：连续以 `|` 开头的行先攒着，遇到非表格行或 turn 结束才排版。
  列宽靠 `displayWidth`（CJK/全角/emoji 算 2 列）自己算，单元格走行内解析（`` `code` ``、`**粗**`）。
  `:--` / `:--:` / `--:` 控制左/中/右对齐；没有分隔行时把首行当表头。
- 表格不会被终端折行打断：宽度取自 `x/term.GetSize`，放不下就削最宽列（下限 6 列，内容超长补 `…`）；
  窄到连最小列宽都摆不下时退回原样输出（不画一张会折行的破表）。
- 每轮 turn 结束 `resetMarkdown()` 清掉未闭合的围栏与未落盘的表格，不会污染下一轮。
- 非 TTY（管道/重定向）或 `--no-color` 时自动退化为去标记的纯文本，仍保留 `•`、缩进和 `(url)`；
  表格也**不做排版重排**，原样输出——否则 `-p` 的 stdout 会被对齐空格塞满，不好被别的程序消费。
- `-p` 模式不渲染 Markdown：stdout 是给程序读的。

命令：`/help`、`/new`、`/compact`、`/context`、`/model [id]`、`/tools`、`/cost`、
`/save [path]`、`/session`、`/init`、`/exit`。

### 2. `-p` 模式（脚本/管道）

```bash
acp-agent -p "跑测试并修好失败的那两个" > result.txt
echo "解释这段代码" | acp-agent -p -   # 从 stdin 读 prompt
```

**stdout 只放最终结果**，工具调用与用量统计全部走 stderr。这样
`$(acp-agent -p …)` 拿到的就是干净的答案。实现上 `terminal.quiet` 关掉了
流式正文的重复输出（`runOnce` 结束时统一打印一次）。

### 3. `--acp` 模式

stdout 只走协议帧。permissions 映射到 `acp.PermissionAllowAll / Ask / ReadOnly`。
工具执行复用同一个 `tools.Registry`，因此 concurrency-safe 的工具仍然并行。

## 权限模型

`internal/tools` 用 `Mutation` 接口区分「只读」与「改动型」工具，另用
`PermissionMutation` 区分「调度上可并行、但权限上算改动」的工具（`bash` 就是）：

| 模式 | 只读工具 | 改动工具（write/edit/bash/patch…） |
|---|---|---|
| `ask`（默认） | 直接执行 | 先问：`y` 一次 / `a` 本会话内总是允许 / `N`（默认）/ `d` 本会话内永不 |
| `auto` | 直接执行 | 直接执行 |
| `readonly` | 直接执行 | 立即拒绝——**不是**提示后继续，而是把拒绝作为 `IsError` 结果交给模型 |

注意 `bash` 必须算改动型：它只声明了 `ConcurrencySafe()==true`（让只读命令能并行），
若门禁只看 `Mutation.Mutates()`，`readonly` 下 `bash: rm -rf` / `python -c 'open(...,"w")'`
就绕过了只读限制。所以它额外实现 `PermissionMutates() == true`：
调度层仍可并行，权限层按改动型拦。回归测试：`TestReadonlyModeDeniesBash`。

`ask` 的决策按工具名缓存，`a`/`d` 对本进程内后续所有同名调用生效。
非 TTY（管道、CI）下 `ask` 仍会读 stdin；自动化场景请显式给 `auto` 或 `readonly`。

拒绝的面板里会展示参数的键值摘要（`content` 等长字段截断），让你在被问之前
就知道要写什么。

## 装配顺序

```
LoadConfig → applyDefaults → 解析 key → buildProvider → newApp
newApp:
  tools.DefaultRegistry          14 个内置工具（read/write/edit/ls/glob/grep/
                                 bash/bash_output/bash_kill/todo/web_search/
                                 web_fetch/delegate/parallel）
  wrapRegistry(guard)            包一层权限闸门，保留 Mutates()/ConcurrencySafe()
  agent.New(...)                 Session + Provider + Registry + Executor(并发 8)
```

`wrapRegistry` 用局部类型（而非匿名 struct）实现 `Mutates()`，因为匿名 struct 的
方法集不会从被嵌入的接口「提升」上来——那会让 `write` 被误判为可并发。

## 渲染细节

- `truncRunes` 按 **rune** 截断，避免把中文切成半个字符（早先按字节截断会显示 `图�…`）。
- `summarizeArgs` 对 map 迭代结果排序，保证同一调用每次打印一致。
- 颜色仅在 stdout 是 TTY 时启用；`--no-color` 或 `NO_COLOR=1` 强制关闭。

## 测试

`main_test.go` 用带 `httptest` 的假模型驱动真实链路：

| 测试 | 覆盖 |
|---|---|
| `TestEndToEnd_ToolLoop` | 模型发起 `read` → 工具执行 → 第二轮出结论；会话落盘/恢复；累计 usage 与轮数 |
| `TestReadOnly_BlocksWriteTool` | `readonly` 下 `write` 被拒、文件未落盘、拒绝信息回传给模型并继续 turn |
| `TestGuard_AlwaysAndNever` | `ask` 的 a/d 记忆与只读直通 |
| `TestWrapRegistry_PreservesMutationAndOrder` | 包装后 `Mutates()/ConcurrencySafe()` 仍正确 |
| `TestBuildProvider_DeepSeekDefaults` | key 缺失报错、默认模型与上下文窗口、未知 provider 列出可选项 |

```bash
go test ./cmd/acp-agent/ -race -v
```

本地手工验证时可以用 `httptest` 以外的独立假端点（构造 `data: {...}\n\n` 的 SSE，
最后补 `data: [DONE]`），再用 `--base-url http://127.0.0.1:PORT` 指过去。
