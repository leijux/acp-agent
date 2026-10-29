# internal/kernel

[`billion-context/kernel`](https://github.com/ranxianglei/billion-context/tree/master/kernel)（`acp-kernel`）的 Go 封装：**无 Node 进程**，纯 goja 解释执行，内置上游 CJS 产物（`go:embed kernel.dist.cjs`，约 400 KB）。

上游是什么：一个 agent 的上下文管理器——给消息打 `<acp>` 引用标签、预算裁剪、压缩分块（tier 1/2/3）、BM25 检索、持久化状态。

## 快速开始

```go
k, err := kernel.New()
if err != nil { return err }

sess, err := k.NewSession(kernel.Config{ModelContextLimit: 200_000})
if err != nil { return err }
defer sess.Close()

// 每轮：传入完整历史 + provider 报的 tokenCount
res, err := sess.Turn(history, promptTokens)
history = res.Messages        // 渲染后的视图：已裁剪、已打 <acp> 标签

// kernel 自己推荐的压缩区间（已排除活跃块/受保护工具/最近消息）
if n := res.Nudge; n != nil && len(n.CompressibleRanges) > 0 {
	r := n.CompressibleRanges[0]
	cres, err := sess.ApplyCompression([]kernel.Range{{
		StartRef: r.StartRef,
		EndRef:   r.EndRef,
		Topic:    "上下文预算讨论",        // 3–200 字符
		Summary:  "SUMMARY: …",          // ≥ 50 字符，否则被拒绝
	}})
	_ = cres
}
```

完整可运行示例见 [`docs/agent.md`](agent.md) 与 `internal/kernel/kernel_test.go`。

## 为什么用 `Nudge.CompressibleRanges` 而不是自己挑 ref

上游对 `applyCompression` 的输入做多层校验，自己算区间很容易被静默拒绝（返回错误而非 panic）：

- 区间里含已被活跃块覆盖的消息 → `DEAD ref` / `overlap`
- 区间含受保护内容（最近 N 条、首条 user 消息、system）→ 拒绝
- `chars < 5000` → 拒绝（不划算）
- `summary < 50` 字符 → 拒绝
- 区间横跨活跃块边界 → 拒绝

`nudge.compressibleRanges` 是上游自己算好的安全区间，直接用最稳。

## API 概览

| 方法 | 说明 |
|---|---|
| `New(opts ...Option) (*Kernel, error)` | 创建实例（bundle 懒编译一次，之后复用） |
| `(*Kernel).NewSession(Config) (*Session, error)` | 会话（state/VM 隔离，编译产物共享；~4.7ms） |
| `(*Kernel).Version() (string, error)` | 上游 kernel 版本 |
| `(*Kernel).Functions() ([]string, error)` | 上游导出符号清单（259 个） |
| `(*Kernel).Call(name string, args ...any) ([]byte, error)` | 逃生口：直接调上游任意导出函数 |
| `(*Session).Turn(msgs, tokenCount)` | 主路径：裁剪 + 打标 + nudge，返回完整视图 |
| `(*Session).TurnMeta(msgs, tokenCount)` | 轻量版：只回 nudge/refs，不序列化视图 |
| `(*Session).View()` | 按需把最近视图转成 `[]Message` |
| `(*Session).TurnBatch(ctx, []BatchTurn)` | 批量推进，遇 `ShouldInject` 自动停 |
| `kernel.EstimateTokens(msgs)` | Go 侧 token 估算，用作 `tokenCount` 兜底 |
| `kernel.CountTokens(text)` | 与上游 `defaultCountTokens` 逐字节等价的 Go 实现 |
| `(*Session).ApplyCompression([]Range)` | 应用压缩 |
| `(*Session).Decompress(blockID)` | 还原块（只返回摘要 + 直接消息 id） |
| `(*Session).Blocks() / BlocksJSON()` | 块列表 / 原始 JSON |
| `(*Session).Refs()` | id ↔ `mNNNNN` / `bN` 引用映射 |
| `(*Session).Status(tokenCount)` | contextUsage、breakdown、prep 明细 |
| `(*Session).Search(query) / SearchBlocks(q) / SearchBlocksWith(q, opts)` | 检索 |
| `(*Session).StateJSON() / LoadState([]byte)` | 持久化（纯 JSON，可入库） |
| `(*Session).Persist(ctx, Store, id) / Restore(ctx, Store, id)` | 存取到 Store |
| `kernel.NewFileStore(dir) / NewMemoryStore() / SummarizeState(id, state)` | 内置两个后端 + 元信息提取 |
| `(*Kernel).NewSessionFrom(ctx, Store, id, Config)` | 创建并恢复 |
| `(*Session).SetMessages(msgs)` | 热替换历史 |
| `(*Session).CallCore(name, args...)` | 逃生口：直接调上游 core 对象方法 |

`Options`：`WithLogger`、`WithSegmenter`、`WithFS`、`WithCountTokens`、`WithBundle`。

## 持久化

state 是纯 JSON，丢了就丢了全部压缩块与引用。内置两种 `Store`：

```go
import "github.com/leijux/acp-agent/internal/kernel"

// JSON 文件（零依赖；每会话一个文件，临时文件 + fsync + rename 原子写）
store, err := kernel.NewFileStore("/var/lib/agent/sessions")
info, err := sess.Persist(ctx, store, "user-42")   // 写入
list, err := store.List(ctx)                        // 巡检（UpdatedAt 降序）
store.CleanupTemp(time.Hour)                        // 清理被杀进程留下的 .tmp-

// 进程内（测试）
mem := kernel.NewMemoryStore()

// 一行恢复
sess2, err := k.NewSessionFrom(ctx, store, "user-42", kernel.Config{ModelContextLimit: 200_000})
```

磁盘格式是 envelope（`version` / `meta` / `state`）；裸 state 文件也能读（向前兼容）。`id` 必须是 `[A-Za-z0-9][A-Za-z0-9._-]{0,127}` 且不含 `..`。

`Store` 是 4 个方法（`Save` / `Load` / `Delete` / `List`），接 Redis、S3 或你已有的 DB 都只需几十行。
接 SQLite 推荐纯 Go 驱动 `modernc.org/sqlite`（无需 cgo）：

```go
type store struct{ db *sql.DB }

func (s *store) Save(ctx context.Context, id string, data []byte) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sessions(id, data, updated_at) VALUES(?,?,unixepoch())
		ON CONFLICT(id) DO UPDATE SET data=excluded.data, updated_at=excluded.updated_at`, id, data)
	return err
}
```

## 与主链路的关系

**`internal/agent` 的压缩决策全部由本包提供。** 链路：

```
Agent.Compact
  → kernelMessages(branch)                  // entry ID 即 kernel Message.ID
  → Session.Turn(msgs, tokenCount)          // 得到 nudge
  → nudge.CompressibleRanges[0]               // kernel 划定的区间
  → 模型生成摘要（agent 侧）
  → Session.ApplyCompression([{StartRef, EndRef, Summary}])
  → Block.DirectMessageIDs → Session.AppendCompactionBlock
  → Session.SetKernelState（随 JSONL 落盘）
```

所以 `cmd/acp-agent` 现在**依赖本包**（`go list -deps ./cmd/acp-agent | grep kernel` 非空）。
agent 自己只保留了廉价的阈值预判（`shouldCompact`），不再算切点。

另有两个单独用途：`cmd/verify-kernel` 复现与 Node 的逐字节 hash 一致性；
`Session.Persist` 提供跨进程共享会话的 JSON 文件存储。

## 性能与并发

全部数据来自 `bench_test.go`（`go test ./internal/kernel/ -run XXX -bench . -benchtime 10x`，
Windows / 8 核 / goja v0.0.0-20261024）。fixture 是混合对话：约 1/5 是 40 行的 grep 结果，
其余是中英混合说明文本。

### 三次优化

| 优化 | 做法 | 效果 |
|---|---|---|
| token 计数下移到 Go | kernel 默认 `countTokens` 是 JS 正则 `text.match(/[一-鿿...]/g)`，goja 里走 regexp2 回溯引擎（pprof：78% 分配、33% CPU）。用 `CountTokens` 精确复刻同一算法并默认注入 `ports.countTokens` | Turn 200 条：198ms → 68ms；分配 167MB → 19MB |
| bundle 预编译 | `goja.Compile` 一次，每个新 VM 用 `vm.RunProgram` 复用 `*goja.Program` | `NewSession` 35ms → 4.7ms |
| refs 直读 | `refsLocked` 不再对整个 `messageRefs` 做 stringify→parse→unmarshal，直接从 goja 对象读 | 每轮省一次全表 JSON 往返 |

`TurnMeta` 相对 `Turn` 省的是**输出侧**：200 条时 55ms vs 60ms（差距在噪声附近），
分配 15.9MB vs 19.0MB（-17%）。它真正的价值不在于毫秒，而在于
「每轮只想知道要不要压缩」时不必把 200 条消息搬进 Go 堆。

### 单会话（同一组消息反复 Turn）

| 消息数 | ns/op | B/op | allocs/op |
|---|---|---|---|
| 100 | 26.6 ms | 7.5 MB | 96 K |
| 200 | 68.1 ms | 18.9 MB | 247 K |
| 400 | 200 ms | 56.4 MB | 980 K |
| 800 | 727 ms | 187 MB | 3.97 M |

**注意这是超线性的（接近 O(N²)）**：消息数翻倍，耗时乘 2.6→2.9→3.6。
原因在上游 —— kernel 对每条消息要做多趟全表扫描（refs 分配、absorb、tier 计算），
而且每条消息的 token 计数都要重算。Go 侧无法把这部分并行化（VM 内是单线程解释执行）。

**实用结论：把活跃消息数控制在 200–300 条以内。** 超过就按 nudge 给的区间
`ApplyCompression` 折叠掉 —— 这本来就是 kernel 的设计意图（它自己也会在 tier 阈值上压）。
600 条以上单轮就过秒，别指望靠调参救。

对照 `TurnMeta` 在 1000 条时同样是 ~1.1s，说明瓶颈在输入侧与内核逻辑，不在输出序列化。

### 并发（每 goroutine 独立 Session）

| GOMAXPROCS | ns/op | 相对 1 核 |
|---|---|---|
| 1 | 88.1 ms | 1.00× |
| 2 | 46.5 ms | 1.89× |
| 4 | 31.9 ms | 2.76× |
| 8 | 28.7 ms | 3.07× |

**4 核之后基本饱和（8 核 3.07×）**。原因是每轮 Turn 分配近 20MB，
GC 是全局的 —— 8 个 VM 并行跑，GC 成了共同瓶颈。这也意味着：

- 需要高吞吐时，**把并发度压在 GOMAXPROCS 附近**，多开 goroutine 收益递减；
- 想再往上走只有两条路：降低每轮分配（受限于上游 JS 逻辑），或把会话分散到多个进程。

正确性由 `concurrency_test.go` 保证：12 个会话并发 Turn 的渲染视图 hash
与串行逐字节一致；`go test -race ./...` 干净。

## 三个必须知道的限制

1. **中文分词取决于构建 tag**。上游在**加载期**执行 `new Intl.Segmenter("zh")`（`src/search/tokenizer.ts`），goja 无 Intl。

   带 `segmenter ne` tag 的构建（`task build:full` 与发布矩阵）里**默认就是 gse 真词典**，
   不需要任何开关；轻量构建（`task build`，无 tag）里只有无依赖的 `RuneSegmenter`：
   在 CJK 边界切「单字 + 连续 ASCII 词」，连续 CJK run 再在 kernel 内做 bigram 回退
   —— 检索召回够用，但**词级切分质量低于 Node 的 CLDR**。

   | 运行时值 | 词表 | 加载 | 备注 |
   |---|---|---|---|
   | 空 / `gse`（默认） | 简体 10 万词（1.3MB） | ~50ms / 29MB | 只认简体；繁体退化为单字，由 bigram 兜底；轻量构建里静默降级为 rune |
   | `rune` | 无 | 0 | 单字 + bigram 回退；子代理就是用这个值 |

   命令行**没有** `-segmenter` / `-segmenter-expand` 开关，要改只能靠 `config.json` 或
   `ACP_AGENT_SEGMENTER` / `ACP_AGENT_SEGMENTER_EXPAND`（优先级：env > 配置文件）。

   ```bash
   task build:full          # -tags "segmenter ne" → 23MB，启动即用 gse
   task build               # 轻量构建（20MB，分词器退化为 rune）
   ACP_AGENT_SEGMENTER=rune acp-agent   # 显式降级
   ```

   **`ne` 不能漏**：没有它，gse 自己 embed 的简/繁/日词典与 idf 表（约 31MB）会被
   linker 全部保留，二进制从 23MB 变成 52MB。词表由 `task dict`（`go generate`）从
   gse 自带词典按词频裁剪，生成物在 `internal/segmenter/dict_zh_s.txt`。

   gse **默认带 `WithExpandBigram`**：kernel 的 `cjkRunTokens` 只要 run 内
   有多字词就只返回词、丢掉全部单字段（`segs.filter(w => w.length >= 2)`），直通
   分词结果会丢召回 —— `壓縮引擎` 只剩 `[引擎]`，查询 `缩引` BM25 命中 0 篇。
   补 bigram 后词给精度、bigram 保召回；`"segmenterExpand": "off"` 可关。
   代价：BM25 的 token 集合同时含词与 bigram（doc 长度变大），排序分数会整体重标定。

   作为库自己接：实现 `kernel.Segmenter` 后 `kernel.New(kernel.WithSegmenter(seg))`。
   `Segment.Index` 的契约是 **rune 偏移**（不是 JS 的 UTF-16 码元），kernel 自身只用 `Text`。
   gse 实例可跨会话共享（Segment 只读词典，`go test -race` 16 goroutine × 200 次干净）。

2. **`processTurn` 比 Node 慢，且随消息数超线性增长**。200 条消息约 68ms（优化前 198ms），
   800 条约 730ms —— 消息翻倍耗时乘 2.6~3.6（详见「性能与并发」）。
   这不是 Go 封装引入的：kernel 对每条消息做多趟全表扫描，而 goja 是单线程解释执行。
   **对策是控制活跃消息数在 200–300 条内并及时 `ApplyCompression`**，
   而不是指望在 Go 侧做增量 —— `processTurn` 的语义要求输入全量历史，跳不过去。

3. **async 只在脚本边界 drain**。内核里 `async` 函数用到的 Promise 在单次调用结束前是 pending 状态。kernel 的同步 API 不受影响；如果你用 `Call` 调了上游的异步导出，需要自己 pump（`goja.RunProgram` 后重新调用）。

## 其他事实

- 时间、随机数、locale **不受支持也不需要**：kernel 内部不用 `Date`/`Math.random`（否则 hash 不会与 Node 一致）。
- `structuredClone` / `TextEncoder` / `queueMicrotask` / RegExp `d` flag 在 goja 缺失，kernel 未使用。
- 已支持：`??=`、`Object.hasOwn`、`Array.at`、`findLast`、class fields、`#private`、static block、Proxy get、BigInt、lookbehind。
- `goja_nodejs/eventloop` **没有**用：kernel 的 fs/path 由 Go 侧 `WithFS` 注入，不需要事件循环。
- 产出与 Node **逐字节一致**：同一 workload 的 `messagesHash = 6a8561416f02cde1`、`blockNoTimeHash = 0cbd1741d253e587`。
  这条断言在三次性能优化之后依然通过 —— 因为 `CountTokens` 是与上游 `defaultCountTokens`
  逐字节等价的 Go 复刻（`TestCountTokensMatchesUpstream` 用 JS 版逐例对照）。

## 重新生成 bundle

当前 bundle 对应上游 `acp-kernel 0.0.169`（commit `1fad21d`）。

上一次更新（0.0.167 → 0.0.169）的实际变化很小，只多了一个函数：
`restampContentStore`（上游 #4733 —— CCR content store 在 `restampRefsToLedger`
把 messageRefs 重编号之后要按新的 `byRaw` re-key，否则 store 的 byRef 键会指向
另一个消息）。它在 bundle 里，Go 侧只读持久化结构里的 `byRef` 字段、没有复刻这段
逻辑，所以不需要跟着改。更新后 `messagesHash` / `blockNoTimeHash` 仍与 Node 一致。

上游发新版时：

```bash
git clone --depth 1 https://github.com/ranxianglei/billion-context /tmp/billion-context
cd /tmp/billion-context/kernel && npm ci && npm run build   # 产 ESM dist/
cd /path/to/acp-agent && ./build-cjs.sh                     # ESM -> CJS(ES2017)
go test ./internal/kernel
```

`build-cjs.sh` 做的三件事：`--format=cjs`（goja 无 ESM loader）、`--target=es2017`、`--define:import.meta.url`（上游 `src/tokenize.ts` 顶层 `createRequire` 会读它）。

## 测试

```bash
go test ./internal/kernel -v   # 含与 Node 的 hash 一致性断言
cd .. && task verify:kernel     # 裸 goja 全链路（不复用 internal/kernel）
```
