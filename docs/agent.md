# internal/agent —— Pi agent loop 的 Go 移植

对照 Pi 的三份核心文档实现：`how-pi-works.md`（agent loop / 会话 / 上下文）、
`message-types.md`（消息与内容块）、`compaction.md`（压缩触发/切点/摘要格式）。
```go
prov := myProvider()          // 实现 agent.Provider 即可
reg, mgr := tools.DefaultRegistry(tools.Options{Policy: tools.Policy{Root: "."}})
defer mgr.List()

a, _ := agent.New(agent.Options{
    Provider: prov,
    Model:    agent.Model{Provider: "openai", ID: "gpt-5", ContextWindow: 400_000},
    Registry: reg,
    Executor: tools.NewExecutor(reg, 8),
    Context:  agent.ContextOptions{WorkingDir: ".", ContextFiles: agent.DiscoverContextFiles(".")},
})
a.On(func(ev agent.Event) { render(ev) })

res, err := a.Run(ctx, "把 internal/tools 里的 TODO 都处理掉")
```

## 复刻了 Pi 的哪些语义

| Pi 语义 | 这里 |
|---|---|
| 一轮 = 模型响应 + 其中所有工具执行 + 结果记录 | `Run` 的 turn loop，`TurnEndEvent` 携带该轮的 `ToolResults` |
| 有 tool result 就要再请求一次，否则 run 结束 | `agent.go` 第 4 步：`len(calls)==0` 且无 follow-up 才 break |
| 会话是 entry 树，active branch 提供历史 | `session.go` 的 `Entry{ID, ParentID}` + `ActiveBranch()` |
| `steering` 插在当前 assistant turn 之后 | `Steer()`，在 turn 结束后立即进入下一轮上下文 |
| `follow-up` 等 agent 手上工作做完 | `FollowUp()`，只在没有工具调用时进入 |
| abort 停止 run，排队消息退回 | `Abort()` → `RunResult.Abandoned` |
| 压缩在「工具结果追加后、下一次响应前」检查 | `agent.go` 第 5 步，且仅当这批工具会继续下一轮 |
| 压缩区间由 kernel 划定（不在 toolResult 上切） | `kernel.go` + `Compact`，kernel 自带 protectedRanges |
| 压缩后原文 entry 仍保留 | `AppendCompactionBlock` 只追加 entry，不删旧 entry |
| 消息类型 / content block / usage / stopReason | `message.go`，`stopReason` 含 `pending`/`toolUse`/`length`/`aborted` |
| 摘要结构（Goal/Constraints/Progress/Decisions/Next Steps/Critical Context） | `compact.go` 的 `SummaryPrompt` |
| 摘要输入用角色标签序列化，tool result 截断 2000 字符 | `serializeConversation()` |

## 事件

```
agent_start → turn_start → message_start → message_update* → message_end
            → tool_execution_start → tool_execution_end
            → turn_end → … → agent_end
                                   session_before_compact → session_compact（需要时）
```

事件在调用 goroutine 上同步分发，handler 必须自己保证非阻塞（要写 UI 就丢进 channel）。

## Provider

```go
type Provider interface {
    Stream(ctx context.Context, req Request, sink Sink) (*AssistantMessage, error)
}
```

- `Stream` 必须尊重 `ctx` 取消，返回的 `StopReason` 必须是终态（不能是 `pending`）。
- 增量通过 `Sink` 上报（`Text`/`Thinking`/`ToolCall`），上层可据此渲染流式输出。
  传 `nil` sink 表示不需要流式。
- `req.Purpose == agent.PurposeSummarize` 时是压缩请求：**不要携带工具**，
  并应关闭 prompt-cache write（这类一次性 prompt 基本不会被复用）。
- `agent.ScriptedProvider` 用于测试与回放，`TestRun*` 全靠它做到确定性。

## 上下文组装

`BuildSystemPrompt` 的顺序固定：基础指令 → 工作目录 → 具名 section（按 key 排序，
保证 prompt 稳定命中缓存）→ 项目上下文文件 → 附加说明。

`DiscoverContextFiles` 从工作目录向上找 `AGENTS.md` / `CLAUDE.md` / `PI.md`，
每个名字取最近的一份，遇到 `.git` 停止。内容带 `<file path="…">` 标注，让模型知道来源。

`ProjectMessages` 是第一层转换（Pi 的 `convertToLlm`）：
`BashExecutionMessage` 与 `CustomMessage` 转成 user 文本，
`ExcludeFromContext` 的直接丢弃。

## 压缩：模型自主（主动模式）

压缩的**决策权交给模型**，对齐上游 billion-context 的主动模式：宿主把四个
`context_*` 工具注册进 Registry，模型自己决定何时压、压哪一段、摘要写什么。

| 工具 | 并发安全 | 作用 |
|---|:--:|---|
| `context_check` | ✅ | 看占用比例与 kernel 推荐的可压区间（带 `mNNNNN` 引用） |
| `context_apply` | ❌ | 提交压缩；摘要由**模型自己写** |
| `context_search` | ✅ | 在已压缩块里做 BM25 检索 |
| `context_decompress` | ✅ | 查看块详情（摘要 + 被折叠的消息 id） |

宿主只剩三层触发（`compact.go`）：

| 水位 | 行为 |
|---|---|
| < 45%（`SoftCompactUsage`） | 不干预 |
| ≥ 45% | 注入一条 `context-nudge`（`Display=false`），告知模型可压区间；按「上下文再涨 20%」节流，压缩后 `resetNudge` 重置 |
| ≥ 90%（`HardCompactUsage`） | 模型迟迟不动手时强行 `Compact`（安全网） |
| `/compact`（手动） | 一律执行，不走阈值判断 |

```
模型看到 nudge ──context_check──▶ kernel.Turn → nudge.CompressibleRanges
   区间 refs ──context_apply（模型写 summary）──▶ kernel.ApplyCompression
   Blocks ──mirrorBlocks──▶ compaction entry（BlockID + BlockedIDs）
```

`Compact`（宿主自动摘要）仍然保留，但它现在只是兜底路径：手动 `/compact` 或
占用撞到 90% 时才会走到「调模型生成摘要」那条老路。

### kernel 仍然负责「压哪一段」

无论谁触发，区间都由 `internal/kernel` 划定（`nudge.CompressibleRanges`），
宿主和模型都不自己算切点。对齐方式：喂给 kernel 的 `Message.ID` 直接用 entry ID，
所以 kernel 返回的 `Block.DirectMessageIDs` 可以原样当作 `Entry.BlockedIDs`。

kernel 的 **state 快照跟着 Session 一起落盘**（`Session.SetKernelState` → JSONL 的
`{"type":"kernel_state"}` 行），重开会话后压缩块仍然有效，不会重复压同一段历史。

本包不再自己算切点：`KeepRecentTokens` / `findCutPoint` / `validCutMessage` 已删除。

## 关键实现细节（踩过的坑）

**`DisableAutoCompact` / `DisableContextTools` 而不是肯定式命名。** 布尔零值必须
是「功能开启」，否则忘记设置字段就会静默失去能力 —— `AutoCompact` 默认为 false 的
问题在测试里真实出现过一次（上下文 139 条消息时没压缩）。

**`ShouldInject` 不是「该不该压」。** 上游的 `Nudge.ShouldInject` 是「是否把提示
文本注入对话」的节流开关（受 `frequency` / `iterationThreshold` / `growthFloor`
控制）。用它当压缩门禁，会在上下文已经很高、区间也够的时候因为节流窗口没到而
整轮跳过。判断依据必须是 `len(nudge.CompressibleRanges) == 0`。

**kernel 复合操作要自己加锁。** `Turn` + `ApplyCompression` 不是原子的，而
`context_check`（并发安全）可能与 `context_apply`（独占）同时运行。
`Agent.ctxMu` 串行化这一整段，不依赖 `kernel.Session` 内部的细粒度锁。

**`context_*` 工具不走权限门禁。** 它们在 `wrapRegistry` 之后由 `agent.New` 注册，
所以绕过了 `guard`。这是有意的：它们只读写会话历史，不动文件/进程，每次弹确认
只会碍事。要人工介入就用 `/compact`，要不想要就设 `DisableContextTools`。

**kernel 对摘要长度有硬下限（50 字符）。** 太短的摘要会让**整批**区间被拒：
`Summary too short (12 chars, min 50)`。`padSummary` 会在摘要过短时追加区间概要兵底。

**kernel 可以压中间段。** 旧投影规则「以最后一个 compaction entry 为界」在 kernel 下
不再成立（压缩块可以覆盖中段而保留两头）。投影改成块感知：
`compactionBlocksLocked` 把每块的摘要插在它首个被吃 entry 的位置
（`Entry.BlockedIDs` 非空时生效），旧格式仍走原逻辑当兵底。

**工具事件 hook 是临时挂载的。** `Executor.OnStart/OnDone` 是单字段，多个 Agent 共享
一个 Executor 会打架。`runTools` 里先保存旧 hook、挂上自己的、`defer` 恢复，
这样串行 run 下正确；**并发 run 同一个 Executor 不安全**（每个 Agent 用独立 Executor 即可）。

**`RunMessages` 而不是只有 `Run`。** 带图片的输入必须是 `[]Block`，
只接受 string 的 `Run` 会逼调用方自己拼消息。

## 测试

```bash
go test ./internal/agent/ -count=1        # 0.25s
go test ./internal/agent/ -count=1 -race  # 1.3s
```

覆盖的语义（都是断言，不是「跑通就行」）：

- `TestRunToolLoop` —— 有工具结果必须再请求一轮；结果顺序与 calls 一致；第二轮上下文含工具结果。
- `TestSteeringEntersAfterCurrentTurn` —— steering 出现在第二轮请求的**最后**。
- `TestFollowUpWaitsForIdle` —— follow-up 只在没有工具调用时才触发新一轮。
- `TestAbortReturnsQueuedMessages` / `TestAbortMethodStopsRun` —— abort 语义与排队消息退回。
- `TestToolCallsRunConcurrently` —— 4 个只读调用的 `Parallel` 标记必须全为真。
- `TestCompactionReducesContext` —— token 必须下降、投影首条是摘要、原始 entry 计数不变。
- `TestAutoCompactionDuringToolLoop` —— 压缩发生在工具轮之后（第二轮上下文 139 → 6 条）。
- `TestContextToolsRegistered` —— 四个 `context_*` 工具默认自动注册；`DisableContextTools` 时不注册。
- `TestContextCheckReportsRanges` —— nudge 区间必须带合法的 `mNNNNN` 引用与非零消息数。
- `TestContextApplyFoldsHistory` —— 模型自写摘要压缩后投影首条为摘要、token 下降、`nudgedAt` 重置。
- `TestContextApplyAutoRange` / `TestContextApplyShortSummaryPadded` —— 只给 summary 也能压；<50 字符被补足而非整批失败。
- `TestContextSearchAndDecompress` —— 压缩后能检索到块、能取出块详情。
- `TestCompactThresholdSemantics` —— 45% 软提示、90% 硬兜底、两者大小关系。
- `TestContextNudgeMessageThrottled` —— 提示带 `context_apply` 引导、不显示给用户、同水位不重复。
- `TestActiveCompactionThroughToolLoop` —— 端到端：模型在同一 run 内先 check 再 apply，压缩真正落地。
- `TestSessionSaveLoadRoundTrip` —— JSONL 往返保真（tool call、usage、leaf）。
- `TestBranchForkChangesProjection` —— fork 后旧分支消息不再进入上下文，但 entry 仍在。
- `TestProviderErrorRecordsAssistantMessage` —— provider 失败也留下一条 `stopReason=error` 的消息。
- `TestMaxTurnsLimit` —— 模型永远发工具调用时会被上限拦住。
