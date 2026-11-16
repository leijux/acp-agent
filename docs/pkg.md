# pkg/ —— 给外部用户的 Go SDK（设计稿）

目标一句话：**让模块外的 Go 程序用 3 行代码拿到一个能读写文件、能跑命令、会自动压缩上下文的 AI**，
而把 `internal/` 留给「机制」，`pkg/` 只负责「接线」。

---

## 1. 为什么现在做不到

`internal/` 按 Go 规则模块外不可导入，所以 README「作为库使用」那段示例（README.md:87-95）
在别人的项目里编不过。更实际的问题是：**接线本身有 7 个必选项、4 个坑，全都藏在实现里。**

真正的接线成本看 `cmd/acp-agent/main.go:545-656` 的 `newApp`：

| 步骤 | 做什么 | 忘了会怎样 |
|---|---|---|
| 1 | `provider.LookupEndpoint` + `Endpoint.ModelFor`（internal/provider/deepseek.go:195） | 上下文窗口猜错 → 压缩水位算错 |
| 2 | `provider.NewOpenAI` + `provider.Retry(4)`（main.go:408/424） | 429/5xx 直接失败给调用方 |
| 3 | `skills.Discover`（internal/skills/skills.go:99） | 技能工具不注册，模型没有 skill 能力 |
| 4 | `tools.DefaultRegistry`（internal/tools/agent.go:257） | 工具集、bash manager、cleanup 全没有 |
| 5 | 权限包装 + `tools.NewExecutor(guarded, 8)`（main.go:611-619） | **Executor 拿错 Registry 就绕过权限** |
| 6 | `agent.DiscoverContextFiles` + `agent.New`（main.go:626/653） | AGENTS.md 不注入；context_* 工具不注册 |
| 7 | `Registry.Close()` | 后台 bash 与子代理变孤儿进程 |

这四个坑是设计必须吃掉的（不是文档补一句就行的）：

1. **Executor 必须持有被门禁包裹的 Registry**（main.go:612-619 的注释已经写明）。
2. **不 Close 就漏进程**：cleanup 登记在 `Registry`（internal/tools/registry.go:35-62），
   而调用方通常只拿到 `*agent.Agent`。
3. **`Policy.AllowNetworkWrite` / `AllowScriptWrite` 只在 `permissions=auto` 打开**
   （internal/tools/safety.go:24-38）：`js` 不是改动型工具，ask 下放行等于绕开 bash 的确认弹窗。
4. **`context_*` 在 wrap 之后注册**（agent.go:172 `registerContextTools`），所以它们不受门禁 ——
   这是有意的，门面里别「顺手补一下」。

---

## 2. 目标 / 非目标

**目标**

- G1 三行起一个能跑工具的 agent（含自动压缩、技能、权限、会话）。
- G2 覆盖四种真实用法：① 嵌到自己的服务里（聊天 + 工具）② 写 CLI ③ 只要多模型客户端
  ④ 只要 agent loop（自带 Provider / 自带工具）。
- G3 可扩展：自定义工具、自定义 Provider、自定义事件消费。
- G4 不锁死内部：`pkg/` 承诺门面，`internal/` 保留自由重构；需要下钻时有逃生口。
- G5 默认安全：不显式声明权限时**不会**静默 `auto`。

**非目标（本期不做）**

- 不封装 TUI（internal/tui）、不封装 ACP server（internal/acp）、不做 HTTP server。
- 不做 MCP 生命周期管理（`mcp.Manager` 需要多会话/断线重连语义，二期给 `pkg/mcp`）。
- 不复刻 CLI 的配置优先级（flag > env > config.json）：SDK 用显式 Config，环境变量只用于 API key。

---

## 3. 分层与目录

```
pkg/
  ai/            ← 门面：ai.New(ai.Config) → *ai.Chat（90% 用户只需要它）
  provider/      ← 多模型客户端，可单独当 SDK 用（OpenAI 兼容 + 端点预设 + 重试 + 定价）
  aitest/        ← 测试替身（ScriptedProvider / 假时钟），让用户能无网测自己的 agent
```

三个包，不做更细的拆分。判据（写进每个 doc.go）：

> **接线 → pkg；机制 → internal。**
> pkg 里不出现算法，只出现「谁按什么顺序装配、默认值是什么、生命周期归谁」。

`pkg/ai` 单包而不是 `pkg/ai/{tool,session,...}`：Go 的扩展点靠接口，
`ai.Tool` 一个别名就够；拆子包只会让用户多写两行 import。

---

## 4. `pkg/ai` 核心 API

### 4.1 最小用法

```go
c, err := ai.New(ai.Config{Name: "deepseek", WorkDir: "."}) // APIKey 从 DEEPSEEK_API_KEY 取
defer c.Close()                                            // 必调：回收后台 bash/子代理
out, err := c.Ask(ctx, "这个仓库的压缩是怎么触发的？")
```

`Ask` = `Run` + 取最后一条 assistant 文本，覆盖「问答式」调用。

### 4.2 Config

```go
type Config struct {
	// ---- 模型：三选一 ----
	Name    string // 内置端点：deepseek/openai/moonshot/dashscope/zhipu/minimax/openrouter/ollama
	APIKey  string // 空 → 读端点预设的 EnvKey（internal/provider/deepseek.go:31）
	Model   string // 空 → 端点 DefaultModel；窗口未知时保守 128K
	BaseURL string // 覆盖端点地址（自建网关 / vLLM）
	// Provider 非 nil 时忽略上面四项，直接用调用方的实现。
	Provider Provider

	// ---- 环境 ----
	WorkDir      string     // 工具根目录，默认 cwd
	Permission   Permission // 默认 PermReadOnly，见 §5
	OnPermission func(context.Context, PermissionRequest) Decision

	// ---- 上下文 ----
	SystemPrompt  string   // 空 → agent.DefaultSystemPrompt
	ExtraPrompt   string   // 追加在末尾
	ContextFiles  []string // 空 → 自动发现 AGENTS.md 等（agent.DiscoverContextFiles）
	NoContextFile bool

	// ---- 能力 ----
	Skills       SkillOptions // {Enable, ExtraPaths, Files, Disabled}
	Tools        []Tool       // 追加自定义工具（重名报错，不静默覆盖）
	DisableTools []string     // 或 "*"（= 一个内建工具都不要，context_* 仍注册）
	NoSubagent   bool
	Web          WebOptions // {Search, Fetch}；零值 = 关

	// ---- 行为 ----
	MaxTurns    int     // 0 = 不限制（Pi 语义）
	Thinking    string  // off/minimal/low/medium/high/max；默认 "medium"
	Temperature *float64
	Compact     CompactOptions // {Disable, Model}

	// ---- 观测 ----
	Hub     Hub                    // 分类回调（§6），nil 也不要紧
	OnEvent func(Event)            // 原始事件兜底
	Logf    func(string, ...any)   // 空 = 静默
}
```

默认值一律「安全 + 够用」：`WorkDir`=cwd，`Permission`=readonly，`Thinking`=medium，
`MaxTurns`=0，`Compact`=开（含 context_* 主动压缩）。

**与 CLI 的差异要写进 godoc**：`cmd/acp-agent` 默认 `permissions=ask`（有人盯着终端），
SDK 默认 `readonly`（不能假设有 TTY）。这是刻意的不一致，不是遗漏。

### 4.3 Chat

```go
func New(cfg Config) (*Chat, error)

func (c *Chat) Ask(ctx context.Context, prompt string) (string, error)
func (c *Chat) Run(ctx context.Context, prompt string) (*Result, error)
func (c *Chat) Stream(ctx context.Context, prompt string) iter.Seq2[Event, error] // range-over-func，go.mod 已是 1.27.3

func (c *Chat) Messages() []Message
func (c *Chat) Usage() Usage          // 跨轮累计（含 Cost，按价格表估算）
func (c *Chat) Reset() error          // /new：新会话，保留配置
func (c *Chat) Close() error          // 幂等；跑 Registry.Close + 会话落盘

// 逃生口：别名类型，外部可命名、可传参
func (c *Chat) Agent() *Agent            // ≡ *agent.Agent
func (c *Chat) Session() *Session         // ≡ *agent.Session
func (c *Chat) Registry() *ToolRegistry   // 被门禁包裹的那一个（= Executor 持有的同一个）
func (c *Chat) Provider() Provider
type Result = agent.RunResult
```

并发语义照抄 internal：同一 `*Chat` 串行 Run，跨实例可并发；`Steer`/`FollowUp`/`Abort` 从其它
goroutine 调（`Chat` 上直接转发，不额外加锁）。

### 4.4 类型别名（这一层要克制）

别名只给「**必须互相传递**的值/接口 + 逃生口」，不给纯实现细节：

```go
type Message = agent.Message        // 及 UserMessage/AssistantMessage/ToolResultMessage/...
type Block   = agent.Block          // 及 TextBlock/ThinkingBlock/ToolCallBlock/ImageBlock
type Usage   = agent.Usage
type Cost    = agent.Cost
type Model   = agent.Model
type Event   = agent.Event          // 及全部 XxxEvent
type Agent   = agent.Agent
type Session = agent.Session
type Tool    = tools.Tool           // 用户要能实现它
type ToolResult = tools.Result
type Policy  = tools.Policy         // WorkDir/ReadOnly/AllowOutside 的语义在这里
type ToolRegistry = tools.Registry
```

**代价要说清**：别名会让 godoc 显示 `internal/...` 类型，等于把内部分支变成事实上的公共契约。
缓解三条：
1. 别名清单在白名单文件 `pkg/ai/alias.go` 里，一行一个，review 时只审这张表；
2. 非别名（`CompactOptions`、`Hub`、`Permission`…）都是 pkg 自有类型，内部改字段不会漏出去；
3. `internal/` 里被别名的类型**只许加字段，不许改语义**（写进 CONTRIBUTING 或 doc 注释）。

---

## 5. 权限

```go
type Permission string
const (
	PermReadOnly Permission = "readonly" // 默认：拒绝一切改动
	PermAsk      Permission = "ask"      // 每次改动问 OnPermission；没给回调 = 一律拒绝
	PermAuto     Permission = "auto"     // 全放行
)

type PermissionRequest struct {
	Tool    string
	Args    map[string]any
	Summary string // 人可读：`write internal/a.go (12 行)` / `bash: rm -rf build`
	Diff    string // write/edit 的 diff 预览（其它工具为空）
}

type Decision int // DecisionDeny / DecisionAllow / DecisionAlways
```

规则（照 internal 的语义抄，不改）：

| 档位 | 路径边界 | js 的 fetch 写/写盘 | 改动型工具 |
|---|---|---|---|
| readonly | 锁 WorkDir | 关 | 拒 |
| ask | 锁 WorkDir | 关（否则是 bash 确认的后门） | 请求 OnPermission |
| auto | 放开（`AllowOutside`） | 开 | 直通 |

`DecisionAlways` 记到本次 Chat 的 in-memory 白名单（按工具名），`Reset` 不清（同进程生命周期）。

---

## 6. 事件

两种消费方式并存：

```go
type Hub struct {
	OnText     func(delta string)
	OnThinking func(delta string)
	OnToolStart func(call ToolCall)
	OnToolEnd   func(call ToolCall, out ToolOutput, d time.Duration, err error)
	OnUsage     func(Usage)
	OnCompact   func(before, after int)
	OnError     func(error)     // 非致命
	OnRewind    func()          // 网络重连导致增量被撤回（默认关闭 Rewind，见下）
}
```

- Hub 由 `pkg/ai` 把 `agent.Event` 归一化后回调，用户不必 type switch（90% 场景）。
- `OnEvent` 拿原始事件，ACP/RPC 那种需要逐事件转发的场景走它。
- handler 在 agent 的调用 goroutine 上同步执行，pkg 不替用户做 channel（否则要么丢事件、要么卡住 run）。
  godoc 里给一段「丢进 buffered channel」的示范。
- `SetStreamRewind` 默认 **false**（对齐 ACP 语义：增量无法撤回）；`Config.Rewind = true` 时才开。

---

## 7. 自定义工具（pkg 相对 internal 的新增能力）

internal 的 `Tool` 接口要 5 个方法，写一个小工具太啰嗦。pkg 提供函数式构造器：

```go
ai.NewTool("git_status", "显示工作区状态", nil,
	func(ctx context.Context, args ai.Args) (ai.ToolResult, error) { ... })

ai.NewJSONTool("query_db", "查订单",
	ai.Schema(ai.Prop("id", "string", "订单号")),
	func(ctx context.Context, args ai.Args) (any, error) { ... }) // 返回值自动 JSON 化
```

选项：`WithReadOnly()`（并发安全 + `PermissionMutates=false`）、`WithMutating()`、
`WithPathScoped()`。默认保守：**只读**。这样「忘了声明」不会让一个改动型工具并发跑进只读批次。

`Config.Tools` 里的重名 = `New` 直接报错（internal 的 `Register` 就是报错语义，别在门面上改成覆盖）。

---

## 8. `pkg/provider`（可独立使用的多模型客户端）

`internal/provider` 已经是一份完整实现（OpenAI 兼容流式 + SSE + 重试 + 模型/定价表），
外面只缺可导入的门面：

```go
c, err := provider.New(provider.Options{Name: "deepseek"}) // 或 BaseURL/APIKey/HTTP/Headers
msg, err := c.Chat(ctx, provider.ChatRequest{Model: "deepseek-flash", Messages: []provider.Message{{Role:"user", Content:"hi"}}})
// 流式：
err = c.ChatStream(ctx, req, func(delta provider.Delta) { ... })

provider.Endpoints        // 内置端点表（8 个）
provider.LookupModel("gpt-5")     // 窗口/输出上限/价格
provider.ResolveModel("openai", "gpt-5") // → ai.Model
```

刻意不做的：不重写 HTTP 层、不发明第二套 message 类型。`ChatRequest.Messages` 用 `[]ai.Message`，
和 `pkg/ai` 共用一套 = 用户不必在两层之间转换（这也是 §4.4 别名的真实理由）。

---

## 9. 内部映射表（实现时的对照清单）

| `ai.New` 的动作 | 内部调用 | 备注 |
|---|---|---|
| 端点/模型 | `provider.LookupEndpoint` + `Endpoint.ModelFor` | 语义化模型名（`deepseek-flash`）自动换算窗口 |
| Provider | `provider.NewOpenAI(OpenAIConfig{Kind, Headers, NoStreamUsage})` → `provider.Retry(4)` | 两个都从 Endpoint 透传，漏 `Kind` 会丢思考模式 |
| 技能 | `skills.Discover(Options{Root, ExtraPaths, Files, Disabled})` | `Files` 非空时不再扫 `.pi/skills`（内部语义） |
| 工具 | `tools.DefaultRegistry(Options{Policy, MaxParallel:8, Disable, Skills, Subagent, Search, Fetch})` | 返回的 `*BashManager` 可丢弃：cleanup 已登记在 Registry |
| 门禁 | 按 §5 包一层 → `tools.NewExecutor(guarded, 8)` | Executor 与 `Chat.Registry()` 必须是同一个 |
| 上下文文件 | `agent.DiscoverContextFiles(WorkDir)` | `NoContextFile` 时传 nil |
| Agent | `agent.New(Options{Provider, Model, Session, Registry: guarded, Executor, Context, MaxTurns, ThinkingLevel, Temperature, ContextWindow})` | `agent.New` 自己注册 context_* 并追加压缩 prompt |
| 回收 | `Registry.Close()` ← `Chat.Close()` | 幂等，可以 defer |

---

## 10. 示例（四种用法各一段，作为 doc 的验收标准）

**① 嵌进服务：聊天 + 工具 + 流式推 SSE**

```go
c, _ := ai.New(ai.Config{Name: "deepseek", WorkDir: "/srv/repo",
	Permission: ai.PermAsk, OnPermission: approveViaWebUI,
	Hub: ai.Hub{OnText: func(d string) { sse.Send(d) }}})
defer c.Close()
```

**② CLI**

```go
c, _ := ai.New(ai.Config{Name: "deepseek", Permission: ai.PermAsk,
	OnPermission: promptOnTTY, MaxTurns: 500})
res, err := c.Run(ctx, os.Args[1])
fmt.Print(res.LastAssistant().Text())
```

**③ 只要多模型客户端**

```go
cl, _ := provider.New(provider.Options{Name: "moonshot"})
fmt.Println(cl.Chat(ctx, req))
```

**④ 只要 loop（自带 Provider / 自带工具）**

```go
c, _ := ai.New(ai.Config{Provider: myProvider, Model: "my-model",
	DisableTools: []string{"*"}, Tools: []ai.Tool{myTool}})
```

---

## 11. 测试策略（pkg 不做逻辑，只测量接线）

不变量测试，全部无网：

1. `New` 之后 `registry.Names()` 含 read/write/edit/ls/glob/grep/bash/web_search/delegate/context_check…；
   `DisableTools:{"*"}` 之后只剩 `context_*`（证明「* 只关内建」的语义没被门面改掉）。
2. 自定义 `Tool` 出现在 specs 里；且**经过门禁**：用 `PermReadOnly` 跑它 → 被拒（证明 Executor 拿的是 guarded）。
3. `Close()` 之后：起一个 `bash: sleep 60`，Close，断言进程已死（证明 cleanup 没丢）。
4. `PermReadOnly` 下 `write` 返回拒绝文案；`PermAuto` 下同一调用成功（Policy 三个开关的传递）。
5. 压缩：用 `ai.CompactOptions{}` 默认值 + 小窗口（`Model` 覆盖 8K）跑多轮，断言出现 `session_compact` 事件。
6. 测试替身：`aitest.Scripted("hi", toolCall("read", ...), "done")` → 2 轮 Run 结果确定。
   （内部已有 `agent.NewScriptedProvider`，internal/agent/provider.go:136，pkg 只包一层。）

---

## 12. 分期

- **P0（能用）**：`pkg/ai`（Config/New/Ask/Run/Close/权限/别名）+ `pkg/provider`（New/Chat/ChatStream）
  + 上述不变量测试 + `docs/pkg.md` 定稿。
- **P1（好用）**：`Stream` 迭代器、`Hub`、`ai.NewTool` 函数式工具、会话落盘/续接（`SessionOptions.Path`）、
  `pkg/aitest`。
- **P2（生态）**：`pkg/acp`（被 Zed/编排器拉起）、`pkg/mcp`、预算控制（`MaxCost` / `MaxTokens` 熔断）。

---

## 13. 风险与对策

| 风险 | 对策 |
|---|---|
| 别名把 internal 变成事实公共契约 | 白名单文件 + 只加不改；语义变更走 pkg 自有类型 |
| `cmd/` 与 `pkg/` 两套接线漂移 | 让 `newApp` 复用 `pkg/ai`（吃自己的狗粮）；先在 `cmd` 里加一条「接线一致性」测试对比工具名单 |
| 默认值分歧（CLI ask vs SDK readonly） | godoc 与 README 都写明理由，别当成 bug 修 |
| 二进制体积（kernel bundle + moejs） | 压缩是惰性初始化（internal/agent/kernel.go:86），`Compact.Disable` 时不编译 bundle；`-tags "segmenter ne"` 只影响中文召回 |
| 用户忘 `Close()` | `Close` 幂等 + runtime finalizer 兜底不可靠，改为：`New` 在 `Logf` 缺省时于 GC 前不保证；文档 + 示例每处 `defer` |
