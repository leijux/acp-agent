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
| `(*Kernel).Version() (string, error)` | 嵌入 bundle 对应的上游版本（`kernel.BundleVersion`） |
| `(*Kernel).Functions() ([]string, error)` | 上游导出符号清单（266 个） |
| `(*Kernel).Call(name string, args ...any) ([]byte, error)` | 逃生口：直接调上游任意导出函数 |
| `(*Session).Turn(msgs, tokenCount)` | 主路径：裁剪 + 打标 + nudge，返回完整视图 |
| `(*Session).TurnSource(msgs, tokenCount, source)` | 同上，但声明 `tokenCount` 是 usage 还是估算（见下文「接线现状」） |
| `(*Session).TurnMeta(msgs, tokenCount)` | 轻量版：只回 nudge/refs，不序列化视图 |
| `(*Session).TurnMetaSource(msgs, tokenCount, source)` | 轻量版的带来源版本 |
| `(*Session).View()` | 按需把最近视图转成 `[]Message` |
| `(*Session).TurnBatch(ctx, []BatchTurn)` | 批量推进，遇 `ShouldInject` 自动停 |
| `kernel.EstimateTokens(msgs)` | Go 侧 token 估算，用作 `tokenCount` 兜底 |
| `kernel.CountTokens(text)` | 与上游 `defaultCountTokens` 逐字节等价的 Go 实现 |
| `(*Session).ApplyCompression([]Range)` | 应用压缩（重复区间返回 `CompressionResult.Noop`） |
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

- 时间、随机数、locale **不受支持也不需要**：默认路径里 kernel 不用 `Date`/`Math.random`（否则 hash 不会与 Node 一致）。
  唯一的例外是上游 0.0.171+ 的消息时间戳特性：开了 `Ports.timestamps` 才会在 stamp 节点里
  调 `Date.now()`，我们没开（见下面「上游新增但 Go 侧未接线」）。
- `structuredClone` / `TextEncoder` / `queueMicrotask` / RegExp `d` flag 在 goja 缺失，kernel 未使用。
- 已支持：`??=`、`Object.hasOwn`、`Array.at`、`findLast`、class fields、`#private`、static block、Proxy get、BigInt、lookbehind。
- `goja_nodejs/eventloop` **没有**用：kernel 的 fs/path 由 Go 侧 `WithFS` 注入，不需要事件循环。
- 产出与 Node **逐字节一致**：同一 workload 的 `messagesHash = 6a8561416f02cde1`、`blockNoTimeHash = 0cbd1741d253e587`。
  这条断言在三次性能优化、以及 0.0.177 → 0.0.193 的 bundle 更新之后依然通过 —— 因为
  `CountTokens` 是与上游 `defaultCountTokens` 逐字节等价的 Go 复刻
  （`TestCountTokensMatchesUpstream` 用 JS 版逐例对照）。

## 重新生成 bundle

当前 bundle 对应上游 `acp-kernel 0.0.193`（commit `e3220ad`，2026-11-11），435 KB。

上一次更新（0.0.177/0.0.178 → 0.0.193，2026-11-11）：kernel/src 改 24 个文件、+2184/−272，
全是 kernel 内部行为，**Go 封装一行没改**。要点：

- 压缩正确性：#5751 按 id 在任意距离折叠 tool-call/result 配对；#5789 tier 蒸馏合并能在
  历史中段块周围恢复；#5894 多区间压缩的摘要下限改成按批次合计判定（单个短 span 不再
  拖死整批）；#5531 被块覆盖的调用在 turn-integrity 里视为不可见。
- nudge：#5698 T1 收割停滞时允许质量达标的 T2/T3 先触发；#5860/#5865 幻压守卫
  （`nudge.requireUsageSampleForPressure` 默认 **true**）要求压力档拿到 usage 级采样才注入
  EMERGENCY/OVER-LIMIT，估值级读数最多只压一轮。
- media：#5620 引用式 media 可折叠，重新访问句柄跟着摘要走。
- 时间戳：#5608-B/C 的 high-water mark 与「诚实 NULL」铸造（都要 `Ports.timestamps` 才生效）。
- **删除**：上游 0.0.193 整体移除了 `compressAtFoldPoint` / `relocateCompressCarriers`
  （v1 #5471 + v2 #5949，2026-11-10 #6132 事故后回滚：重定位会把本轮答案文本裸留在
  尾部，小模型会把自己的话读成用户消息）。我们这份 0.0.177 里还带着它，本次更新一并消失。

> 注：本页此前的版本号（0.0.169 / `1fad21d`）是错的 —— 那份 bundle 实际已经是 0.0.177/0.0.178
> （含 `Ports.timestamps`、`absorbedDebt`、relocate v1，缺 `mediaRefs`），只是文档没跟着改。

### 上游新增能力：Go 侧接线现状

| 上游能力 | Go 侧 | 行为 |
|---|---|---|
| `processTurn({ tokenCountSource })`（0.0.184+） | `Session.TurnSource` / `TurnMetaSource` / `TurnBatchSource` + `TokenCountSource` | agent 一律传 `TokenCountEstimate`（它给的确是本地估算）；不传来源 = 旧行为，取值不在白名单里直接报错 |
| `ApplyCompressionResult.payload.noop`（0.0.172+） | `CompressionResult.Noop` | `blocksCreated=0 && errors=[]` 时上游置 true；`context_apply` 渲染成「空操作，不要重试」，`Compact` 归入 `ErrNothingToCompact` |
| `Ports.timestamps`（0.0.171+） | 未接（默认关） | 开了才会在 `<acp>` 标签里带 `time="…"` 并调 `Date.now()`，会改渲染字节、需重对 hash |
| 版本 + 产物指纹 | `BundleVersion` / `BundleDigest` / `BundleEsbuild`（`version_gen.go`，全由 `build-cjs.sh` 生成） | `Kernel.Version()` 返回常量（自定义 bundle 时返回空串）；`TestBundleDigest` 用 sha256 挡住「换了 bundle 没重跑脚本」 |

### 上游发新版时

```bash
git clone --depth 1 https://github.com/ranxianglei/billion-context /tmp/billion-context
cd /tmp/billion-context/kernel && npm ci && npm run build   # 产 ESM dist/
cd /path/to/acp-agent
KERNEL=/tmp/billion-context/kernel ./script/build-cjs.sh    # ESM -> CJS(ES2017)
go test ./internal/kernel/ -run TestGoldenHashes -v         # 与 Node 的逐字节 hash
go run ./cmd/verify-kernel                                  # 裸 goja 全链路（不复用 internal/kernel）
go test ./...                                               # agent/tools 侧回归
```

没有 bash 的机器（纯 Windows）可以直接调 esbuild 的 node 入口，参数与脚本一致：

```bash
cd <kernel> && node node_modules/esbuild/bin/esbuild dist/index.js \
  --bundle --format=cjs --platform=neutral --target=es2017 \
  --external:crypto --external:path --external:fs --external:module "--external:node:*" \
  "--define:import.meta.url=\"file:///kernel/index.js\"" \
  --outfile=/path/to/acp-agent/internal/kernel/kernel.dist.cjs
```

**但手工打完必须补一步**：`internal/kernel/version_gen.go` 里的 `BundleVersion` /
`BundleDigest` / `BundleEsbuild` 是脚本从上游 `package.json` 与产物算出来写的，只跑
esbuild 会留下过期的旧值（`TestBundleDigest` 会立刻红）。手工方案下请自己算：

```bash
node -e "const c=require('crypto'),f=require('fs');console.log(c.createHash('sha256').update(f.readFileSync('internal/kernel/kernel.dist.cjs')).digest('hex'))"
```

`build-cjs.sh` 做的四件事：`--format=cjs`（goja 无 ESM loader）、`--target=es2017`、
`--define:import.meta.url`（上游 `src/tokenize.ts` 顶层 `createRequire` 会读它）、
以及生成 `version_gen.go`（版本 + sha256 + esbuild 版本）。

## 测试

```bash
go test ./internal/kernel -v   # 含与 Node 的 hash 一致性断言
cd .. && task verify:kernel     # 裸 goja 全链路（不复用 internal/kernel）
```
