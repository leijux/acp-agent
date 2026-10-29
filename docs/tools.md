# internal/tools —— Go 版 Pi 基础工具集

把 Pi 的工具契约搬到 Go：**每个工具声明自己是否可并发**，调度器据此分批执行，
写入类工具自动形成栅栏（fence）。零外部依赖，`go test -race` 干净。

```go
reg, bashMgr := tools.DefaultRegistry(tools.Options{
    Policy:      tools.Policy{Root: "/repo", MaxReadBytes: 2 << 20},
    MaxParallel: 8,
})
defer bashMgr.List()

ex := tools.NewExecutor(reg, 8)
results := ex.Execute(ctx, []tools.Call{
    {ID: "1", Name: "grep", Args: map[string]any{"pattern": "TODO"}},
    {ID: "2", Name: "glob", Args: map[string]any{"pattern": "**/*.go"}},
})
// results 顺序与输入一致；results[0].Parallel == true
```

## 工具清单

| 工具 | 并发安全 | 说明 |
|---|:--:|---|
| `read` | ✅ | 读文件，带行号 / offset+limit 分页 / 图片返回 image 块 / 截断提示 |
| `write` | ✅* | 创建或覆盖，自动建父目录。自带路径级互斥（同文件串行、异文件并行） |
| `edit` | ✅* | 精确替换，要求 `oldText` 唯一；一次可带多个互不重叠的 edits。**读-改-写全程持文件锁**。容错见下方「edit 的文本容错」 |
| `ls` | ✅ | 列目录，支持 `-a` / `-l` / 递归深度，默认跳过 `node_modules`、`.git` |
| `glob` | ✅ | `**` 通配，按 mtime 倒序（找最近改动）。`limit` 截断后保证是**全库最新的 N 个**；候选超过 20000 时如实标注「可能不完整」。pattern 含 `/` 时按相对路径匹配（`*` 不跨目录），不含时按 basename 递归 |
| `grep` | ✅ | Go 正则，输出 `文件:行号:内容`（上下文行用 `-` 分隔），支持 `glob` 过滤 / `context` 行 / `limit`。内部 4 worker 并发扫描，见下方「grep 的扫描策略」 |
| `bash` | ✅ | 执行命令；超时或 `background:true` 转后台，返回 `sessionId` |
| `bash_output` | ✅ | 读后台输出：正则 `filter`、`cursor` 增量读、`wait=output/exit` 阻塞等待（见下方「后台任务的等待」） |
| `bash_kill` | ❌ | 杀整个进程组 / 进程树 |
| `js` | ✅ | 内置 goja 执行 JavaScript：纯计算、JSON 变换、字符串/正则处理，只读 `fs`，同步 `fetch`（出网受超时/次数/字节预算约束） |
| `skill` | ✅ | 加载 `.agents/skills` 里的技能：无参列清单、`name` 读 SKILL.md 正文、`name`+`file` 读随附脚本/文档。**未发现技能时不注册**（见下方「Skills」） |
| `todo` | ❌ | 内存态任务清单（replace/add/update/list） |
| `web_search` | ✅ | 多 query 并发检索；后端在 `internal/web`（duckduckgo 免 key / searxng / tavily / brave / exa） |
| `web_fetch` | ✅ | 抓正文：HTML→纯文本（去 nav/footer/script，代码块保留围栏）、GB18030/UTF-16 自动解码、字节+字符双上限 |
| `delegate` | ❌ | 子代理委派：通过 **ACP** 拉起子进程（默认自身），`tasks` 数组内部再并行 |
| `parallel` | ❌ | 元工具：模型一次提交多个只读调用，本地并发执行 |
| `context_check` | ✅ | 看上下文占用与 kernel 推荐的可压区间（后端由 agent 注入） |
| `context_apply` | ❌ | 提交压缩；摘要由模型自己写（后端由 agent 注入） |
| `context_search` | ✅ | 在已压缩块里 BM25 检索（后端由 agent 注入） |
| `context_decompress` | ✅ | 查看块详情（后端由 agent 注入） |

`bash` 声明为并发安全，与 Pi 一致：`git status` / `ls` / `grep` 这类命令可以并行，
是否独占由模型的命令语义决定，而不是靠工具白名单。

`✅*` 标记的是**自带路径级互斥**的改动型工具（`write`/`edit`）：权限层仍按改动处理，
但调度上允许并行 —— 同一路径的读-改-写由 `LockFile` 串行化，不同路径真正并行。

### glob 的截断语义（`internal/tools/search.go`）

`glob` 承诺「按修改时间倒序」，所以截断必须按**全量候选的 mtime** 挑，
不能用「遍历到的前 N 个里最新的几个」冒充。旧实现在 `limit*4` 处直接
`SkipAll`，实测：1000 个文件、`limit=5` 时返回 `f0020.go`（遍历序靠前的旧文件）
而不是 `f0999.go`（真正最新）—— 且没有任何提示。

现在的做法：小顶堆（size = `limit`）在遍历过程中维护「最新的 N 个」，
遍历结束再排序输出。代价是每个匹配文件都要 `d.Info()`（stat），
全命中场景 20.6ms → ~33ms；半命中/不命中基本持平（去掉了一次重复匹配）。

另外两个行为变化：

- **候选硬上限**：匹配文件超过 20000 个时停止遍历，并在输出里写明
  「不保证是全库最新的 N 个」—— 让不完整**可见**，而不是静默给错。
- **pattern 含 `/` 时不再拿 basename 兜底**。`pkg/*.go` 只匹配 `pkg` 这一层
  （旧实现会把 `pkg/sub/b.go` 也算进来）；不含 `/` 的 `*_test.go` 仍是
  basename 递归匹配。

`splitStaticPrefix`（把 `src/**/*.go` 的固定前缀下推成遍历根）与
`globToRegexp` 的正则缓存保持不变。

### Skills：按需加载的指令（`internal/tools/skill.go`）

技能是「目录 + `SKILL.md`」，位置只认规范里的 `.agents/skills`（项目级从 cwd
向祖先找到 `.git` 根，加上用户级 `~/.agents/skills`）。system prompt 里只放
name / description / location，**正文按需加载** —— 几十个技能常驻上下文也只花几百 token。

| 调用 | 行为 |
|---|---|
| `skill`（无参） | 列出全部技能：名字 + 描述 + 目录 |
| `skill{name}` | 返回 SKILL.md 正文，前缀 `[skill dir: …]`，并附同目录的随附文件清单 |
| `skill{name, file}` | 读技能目录下的随附文件（`scripts/`、`references/` 等），路径越出技能目录直接拒绝 |

几个刻意的选择：

- **没有技能时不注册这个工具**。一段只会回答「没有技能」的描述白占 prompt。
- **工具名与参数名不能改**：`internal/kernel/kernel.dist.cjs` 的 `toolPathOf` 已经把
  `toolName=="skill"` 的调用归一成 `skill/<name>`，并从 args 的 `name` 取技能名。
  改名会让压缩/检索段的锚定失效（压缩后相关内容丢掉 `skill/<name>` 标签、BM25 变差）。
- **让宿主读文件而不是让模型用 read**：用户级技能在 `Policy.Root` 之外，`ask/readonly`
  下 `read` 会被拒 —— prompt 里给一个读不到的路径等于骗模型。
- 随附文件只允许读技能目录内的相对路径（技能是外部内容，不能变成任意路径读取器）。

发现规则、frontmatter 字段与校验（缺 `description` 直接跳过、坏 name 只 warning）、
`--no-skills` / `disableSkills` / `skills` 开关与「为什么不读 `.pi/skills`」都见
[skills.md](skills.md)。

### 后台任务的等待（`bash_output`）

后台命令（`background:true` 或前台超时被接管）以前只能靠模型反复轮询
`bash_output`，或者前台 `sleep N` 死等：一次两分钟的任务要耗掉好几轮模型往返，
而且每次返回的是**从头开始的全部输出**（截断到 16KB），token 成倍浪费。

现在 `bash_output` 有三件事：

| 参数 | 作用 |
|---|---|
| `cursor` | 从上次返回的 `[cursor N]` 之后开始读，只要新增内容 |
| `wait: "output"` | 阻塞到**出现新输出**（默认超时 5s）——判断命令有没有起来 |
| `wait: "exit"` | 阻塞到**进程结束**（默认超时 60s，上限 300s）——跑完再叫我 |

实现要点：

- 唤醒用 `bashSession.notify`（容量 1 的 channel，非阻塞发送）：reader 每写一行、
  `done` 置位、`Kill` 都 signal 一次。等待方 `select` 在 notify / ctx.Done / 定时器上。
- **ctx 取消必须能打断等待**：用户按 Esc 取消 turn 时，等待立刻返回当前快照并标
  `[running, 等待已取消]`，不能把 turn 挂住。
- **同一会话只允许一个等待者**：第二个调用拿快照立即返回（否则 `parallel` 里几个
  `bash_output` 会一起占住 executor 的槽位）。
- **cursor 只推进到最后一个完整行的末尾**：写了一半的行留在游标后面，下次连剩余
  部分一起发，不重复也不丢；整块没有换行时（进度条、超长单行）照常推进，否则永远
  读不到。
- 前台超时被接管的会话走 `sharedOutput`，它的写入也接了 signal，两种来源行为一致。
- 结果状态行保持旧格式 `[running]` / `[done(exit N)]`，只在等待超时/取消时加后缀。

### grep 的扫描策略（`internal/tools/grep.go`）

模型最常调的工具之一，所以它是唯一做了并发与预筛的搜索工具。四条约束，改之前先读：

1. **结果顺序稳定**。worker 只写自己的下标，主 goroutine 收齐后按文件序号归并 ——
   不做「谁先完成谁先输出」。同一查询两次跑出不同顺序会污染 prompt 缓存，
   也让「为什么这次没这条」无法解释。
2. **`limit` 不早停**。早停意味着「哪些文件被扫」取决于调度顺序。代价是多扫一些，
   换来可复现；截断信息靠 `grepFileResult.truncated` 如实上报。
3. **worker 数固定 4，不与工具层 `MaxParallel` 相乘**。实测 8 个比 4 个更慢
   （2.54s vs 2.20s）—— Windows 上 4 线程读盘已饱和，多开只是增加调度竞争。
   文件数 < 64 时直接串行（goroutine + channel 的固定开销大于并行收益）。
4. **字面量预筛**。`regexp.LiteralPrefix()` 非空时先用 `bytes.Contains` 过整个文件，
   命中稀疏时（典型代码库）能省掉绝大部分逐行正则；`(?i)` 开头时前缀为空，
   天然不会误判。

其它细节：行切分只切切片头、不复制内容（旧实现 `strings.Split(string(data))` 会复制两遍，
内存约为文件的 3 倍）；上下文行用 `-` 分隔且相邻命中的重叠区段去重；
超过 64MB 的文件跳过并在结尾提示（全读进内存是 OOM 风险，流式实现又撑不起上下文语义）。

基准（`grep_bench_test.go`，`i7-1255U`，优化前后）：

| 场景 | 优化前 | 优化后 |
|---|---|---|
| 2000 文件 / 1% 命中 | 5.02 s | **2.20 s** |
| 500 文件 / 全命中 | 0.735 s | **0.278 s** |
| 8MB 单文件 | 20.0 ms | **13.7 ms** |

## web 能力：后端可换

`internal/tools/web.go` 只有工具壳，真正的网络实现在 `internal/web`，
通过 `Options.Search` / `Options.Fetch` 注入 —— tools 包本身零网络依赖。

| 后端 | 需要 key | 说明 |
|---|---|---|
| `duckduckgo`（默认） | 否 | 抓 `html.duckduckgo.com` 的结果页并解析。**国内网络需代理**，且易被限流；命中验证页时报「人机校验」而不是返回空结果 |
| `searxng` | 否 | 需 `base_url` 指向自建实例，且该实例开启了 `format=json` |
| `tavily` | 是 | 质量好，`max_results` 直通 |
| `brave` | 是 | 用 `X-Subscription-Token` |
| `exa` | 是 | 返回较长正文片段 |

`web_fetch` 与搜索后端无关，任何模式下都可用：

- HTML → 纯文本：丢 `script/style/nav/footer`，`<pre>` 保留为 ``` 围栏（不折叠缩进），`<li>` 加 `- `
- 编码：优先 BOM（UTF-16），再看 `Content-Type`，再猜 `<meta charset>`，最后回退 GB18030
- 双上限：响应体 `maxBytes`（默认 5MiB）+ 提取后正文 `maxChars`（默认 40000 字符）
  只限字节不够 —— 5MiB 的 HTML 展开成纯文本能到十几万字，直接吞掉上下文
- 只用 `http/https`，重定向最多 5 次，结果页头显示**重定向后的最终 URL**

## 执行引擎（默认内置，可切回系统 shell）

`bash` 工具的脚本执行有一个抽象层 `shellProc`，**默认用 mvdan/sh 的纯 Go 解释器**，
不需要任何外部 shell。加 `-tags shellnative` 才编译系统 shell 实现。

| | 默认（`shell_interp.go`） | `-tags shellnative`（`shell_exec.go`） |
|---|---|---|
| 引擎 | mvdan/sh v3（纯 Go 解释器） | 系统 bash / sh / cmd.exe |
| 内建命令开销 | **~0–1ms** | 每次 ~20–25ms（起一个 bash 进程） |
| 外部命令（`ls`/`git`/`awk`） | **~17–21ms**（少一层 shell） | ~20–70ms |
| 依赖 | 无，Windows 原生可用（含常见 Unix 命令回退，见下） | 需要 bash（Windows 上即 Git Bash） |
| `$HOME` | Windows 原生 `C:\Users\x` | Git Bash 风格 `/c/Users/x` |
| 语法错误 | `Start()` 直接返回 `1:1: ...` 错误 | 退出码 2 + bash 的报错文本 |
| 中断 | 取消 ctx，`exec.CommandContext` 连带杀外部命令 | Job Object 杀整棵进程树 |

实测（Windows，同一台机器）：

```
echo hello                                   默认 0ms    native 27ms
for i in 1 2 3; do echo -n "$i "; done; echo 默认 1ms    native 27ms
echo hi | tr a-z A-Z                         默认 20ms   native 62ms
go env GOMOD                                 默认 50ms   native 71ms
```

为什么内建命令快这么多：系统 shell 版每条命令都要 `fork/exec` 一个 bash.exe；
解释器版 `echo`/`for`/`$(…)`/`[[ ]]`/函数/`case` 全在 Go 里执行，只有真正的外部程序才 exec。

14 条常用命令（赋值、管道、`$?`、heredoc 风格 printf、`case`、函数、`set -e`、`[[ ]]`、`awk`、
`{ …; } | wc -l`）在两个引擎上输出完全一致（这些命令两边都有）。注意 `ls`/`rm`/`head`
这类走了内建回退的命令输出格式与 GNU 版本略有差异（如目录名带 `/` 后缀），
但语义一致；脚本里解析 `ls` 输出时不要依赖固定列宽。

### 内建命令回退（PATH 里没有 coreutils 时）

内置解释器只实现 shell 内建（`cd`/`echo`/`test`/`printf`/…）。`sleep`、`ls`、`cat` 在 POSIX
里本来就是外部程序，解释器会去 exec 它们。纯 Windows 环境往往一个 coreutils 都没有，
`ls`、`sleep 1` 直接 `exit 127`，模型基本没法用 bash 工具干活。

`shell_builtin.go` 用 `interp.ExecHandlers` 中间件补了这层：**PATH 里能找到同名可执行文件时
一律用真程序**（装了 Git Bash / MSYS2 / scoop coreutils 的用户行为不变），找不到才回退：

| 命令 | 选项 | 备注 |
|---|---|---|
| `sleep` | 多参数累加、`s/m/h/d` 后缀、小数 | 响应 ctx，能被 `bash_kill`/前台超时中断 |
| `ls` | `-a -l -1 -d` | 目录名带 `/` 后缀；非 tty 行为即一行一个 |
| `cat` | 无参/`-` 读 stdin、多文件 | 缺文件时继续处理其余文件，最后退出码 1 |
| `head` / `tail` | `-n N`、`-nN`、`-N`、`-q`、`-v` | 单行 >64KB 也完整输出（用 `bufio.Reader` 而非 `Scanner`） |
| `pwd` | `-L` / `-P` | 解释器的当前 cwd（脚本里 `cd` 之后正确） |
| `mkdir` | `-p` | |
| `rm` | `-r/-R/-f/-v` | 危险命令仍由 `Policy.CheckCommand` 在入口拦 |
| `cp` | `-r/-R -f -n` | 默认跟随符号链接；不实现 `-p/-a`；目标是已存在目录时拷成 `dst/<basename>` |
| `grep` | `-i -v -n -c -l -L -o -q -w -x -F -E -r/-R -h -H -e` | 见下「内建 grep 的取舍」 |

不在表里的命令（`sed`、`awk`、`wc`、`tr`、`sort`、`which`…）仍然要求系统里有真实
可执行文件。Windows 上想用它们，要么装 Git Bash / MSYS2 / scoop coreutils，要么改用
`read`/`grep`/`glob` 工具。

#### 内建 grep 的取舍（`shell_builtin_grep.go`）

退出码与 GNU 一致：`0` 有匹配 / `1` 无匹配 / `2` 出错（打不开文件、选项非法、正则无效），
所以 `grep x f || echo none` 这种写法靠得住。与真 grep 的差异：

| 项 | 内建行为 | 原因 |
|---|---|---|
| `-A/-B/-C/-m` | **明确报错退出 2** | 静默当普通匹配更坏：调用方会以为拿到了上下文 |
| 正则方言 | 默认按扩展正则（ERE，Go RE2） | GNU 默认 BRE 里 `\+`/`\?`/`\{n,m\}` 才特殊，这里反过来；模型写 ERE 远多于 BRE |
| 二进制文件 | 按字面行扫，不报 `Binary file … matches` | 省一次预扫；日志/代码场景不依赖它 |
| 递归时符号链接目录 | 不跟随 | `filepath.WalkDir` 的默认语义，避免环 |
| 递归输出的路径分隔符 | 统一 `/` | 输出要能直接喂回 sh 里的其他命令 |

几个实现细节：用 `bufio.Reader.ReadString` 而不是 `Scanner`（单行 >64KB 时 Scanner 会误判
EOF）；CRLF 行尾原样保留（`grep x f > out` 不会把文件悄悄改成 LF）；`-l` 命中第一个就停；
`-F` 且不带 `-i` 时走 `strings.Index`（顺便避开 `ToLower` 改变字节长度导致区间错位）。

已知限制（默认的 interp 引擎）：

- **纯内建死循环（`while true; do :; done`）无法中断** —— mvdan/sh 没有“中止脚本”的 API，
  取消只会让下一次外部命令失败。会真实执行的外部命令都能被及时杀掉。
- `[[ … ]]`、数组、`(( ))` 走 `syntax.LangBash` 解析；纯 POSIX 脚本没有问题，
  但 bash 的一些边角语法（`${a[@]:1:2}` 的极端组合、extglob）可能不被支持。
- `$HOME` 等环境变量是进程原生值，与 Git Bash 的模拟路径不同 —— 脚本里不要假设 `/c/...`。
- 解释器把 stdout/stderr 合并成同一管道，与系统 shell 的行为一致。

切换方式：

```bash
# 默认就是内置解释器，build 不需要任何 tag
task build

# 需要系统 bash 的行为（Git Bash 路径、别名、bash 特有内建）时：
go build -tags shellnative ./cmd/acp-agent
task test:native        # 用系统 shell 跑同一套 tools 测试
```

`shellEngineHint()` 会把当前引擎写进工具描述，模型能看见自己在哪个引擎下执行。

## 并发调度模型

```
输入:  [read] [grep] [write(a)] [write(b)] [read] [bash_kill]
分批:  └── 批1 并发 ──┘ └── 批2 并发 ──┘ └─批3─┘
                                   独占
```

规则见 `registry.go` 的 `Executor.Execute`：

1. **连续的可并发工具合成一批**，批内用信号量限制并发度（`MaxParallel`）。
2. **`ConcurrencySafe()==false` 的工具独占一批**，前后形成栅栏。
3. **改动型工具（`Mutates()==true`）默认也独占**；但实现了 `PathScoped()==true` 的
   例外 —— `write`/`edit` 用 `internal/tools/filelock.go` 的 `lockFile` 把**同一路径**的
   读-改-写持锁串行化，所以不同文件真正并行、同一文件安全互斥。
   没实现 `PathScoped` 的改动型工具（`todo`/`bash_kill`/`context_apply`/MCP 写工具等）仍独占。
4. **结果顺序永远等于输入顺序**，与模型消息一一对齐，不看谁先完成。
5. **panic 被恢复**并转成该调用自己的错误结果，不影响同批其它调用。
6. **未知工具只让该调用失败**，不中断整批。
7. `Mode` 可切 `ModeSequential`（全串行）/ `ModeAuto`（默认）/ `ModeParallel`。

`PathScoped` 的锁只管本进程：子进程（`bash`、子代理）的并发写不在保护范围内。
锁 key 用 `filepath.Abs` + `Clean`，Windows/macOS 上再 `ToLower`；不做符号链接解析。

`parallel` 元工具额外做**静态校验**：批里只要有一个 `Mutates()==true` 的工具就整体拒绝，
而不是「部分执行后发现不该并行」。

实测（4 个 300ms 命令，Windows/Git Bash）：

```
─ bash -lc（login shell）  709 ms
─ bash -c             365 ms
串行参考                    1200 ms
```

## 上下文管理工具（context_*）

把压缩的决定权交给模型（对齐上游 billion-context 的主动模式）。四个工具在
`context.go`，只定义契约；真正的 kernel 操作由 `internal/agent` 通过
`tools.ContextBackend` 注入 —— 所以本包不依赖 agent，也不依赖 kernel。

```go
type ContextBackend interface {
	ContextStatus(ctx) (ContextStatus, error)
	ContextApply(ctx, []ContextRange) (ContextApplyResult, error)
	ContextSearch(ctx, query string, limit int) ([]ContextHit, error)
	ContextDecompress(ctx, blockID string) (ContextBlock, error)
}
```

注册是自动的：`agent.New` 在 `Registry` 非空且未设 `DisableContextTools` 时
调 `tools.ContextTools(backend)` 注册四个工具（已存在同名工具则跳过）。

注意：`context_apply` 实现了 `Mutation.Mutates() == true` —— 目的是让 Executor 串行调度
它（不能与其它工具并发改会话），而不是让权限门禁拦它。这些工具在
`wrapRegistry` 之后注册，因此不经过权限确认（只读写会话历史，不碰文件/进程）。

## 零参数工具的 schema 坑

`schema` 返回的 `properties` 必须是**空对象**而不是 `null`。早期 `Schema(nil)` 直接
把 nil map 塞进去，序列化成 `{"properties": null}`，OpenAI 会直接回：

```
HTTP 400: Invalid schema for function 'context_check': null is not of type "object"
```

现在 `tools.Schema` 对 nil 做了归一化（`internal/tools/tool.go:95`），并且
`internal/tools/schema_test.go` 会遍历注册表里每个工具，断言序列化后的
`properties` 是对象 —— 新增零参数工具时这个测试会先拦住。

## JavaScript 工具（js）

模型需要算数（`86243 秒是几小时几分`）、改一段 JSON、写个正则验证时，调 `bash` + `python`
要么没装、要么起进程 20ms 起步、要么跨平台不一致。`js` 直接跑在进程内的 goja 上：
**零外部依赖、跨平台一致、启动 <1ms、无写权限**。

```json
{"script": "args.rows.filter(r => r.n > 3).map(r => r.name).join(',')", "args": {"rows": [{"name":"a","n":1},{"name":"b","n":5}]}}
```

| 参数 | 说明 |
|---|---|
| `script` | **必填**。整段脚本当作表达式求值；写了 `return` 则按函数体执行 |
| `args` | 任意 JSON，脚本内以 `args` 全局变量访问（走 JSON.parse 注入，是原生 JS 值） |
| `timeout` | 毫秒，默认 5000。超时会 `Interrupt` 整个 vm |

输出格式与 Chrome DevTools 的控制台体验一致：

```
$ acp-agent（模型调用）
js(script="({a:1,b:[1,2]}).b.length", )

=> 2
```

实现要点（`internal/tools/js.go`）：

- **每次调用新建 `goja.Runtime`** —— 变量不会跨调用泄漏，天然并发安全（`ConcurrencySafe() == true`）；
- 注入 `console.{log,info,warn,error,debug}`，对象用 `JSON.stringify` 打印，多余参数空格分隔；
- 返回值打印 `=> <值>`：`undefined` 不打印，对象/数组走 JSON，BigInt/Symbol 走 String；
- **只读 fs 桥**（`internal/tools/js_fs.go`）：`fs.readFile(path)` / `fs.readFileBytes(path)` /
  `fs.readFileBase64(path)` / `fs.readDir(path)` / `fs.stat(path)` / `fs.exists(path)` /
  `fs.glob(pattern)`，另有 `fs.root`。
  - `readFileBytes` 返回 **ArrayBuffer**（`new Uint8Array(b)` 即可按字节处理，含 NUL）；
    `readFileBase64` 返回 base64 串，适合只关心大小/校验值的场景；
  - **`crypto.sha256(字符串或 ArrayBuffer)`** → 十六进制摘要（`internal/tools/js_crypto.go`）。
    goja 没有 SubtleCrypto/Buffer，读二进制最常见的用途就是算校验值，所以这层接 Go 标准库；
  - 所有路径过 `Policy.Resolve`：越界（含符号链接指向 root 外）直接抛 JS 异常，
    脚本可以 `try/catch` 接住；
  - `fs.glob` 支持 `**`（`filepath.Glob` 不认识，内部走 WalkDir + 正则），
    结果统一用 `/` 分隔、相对 `fs.root`；`pattern` 不能是绝对路径、不能含 `..`；
  - 单次执行有预算：最多 512 个文件 / 16MB，单文件上限取 `Policy.MaxReadBytes`；
  - **没有写接口**（`writeFile`/`unlink`/`mkdir` 都是 `undefined`）——写盘继续走
    `write`/`edit`，那两条路有权限门禁和 diff；
- **同步 `fetch`**（`internal/tools/js_http.go`）：`fetch(url[, options])` →
  `{status, ok, url, contentType, headers, size, truncated, text, json}`。
  - options：`method`（默认 GET，自动大写）、`headers`（对象）、`body`（字符串）、
    `json`（任意值 → 自动 `JSON.stringify` 并补 `Content-Type: application/json`）、
    `timeout`（毫秒，默认 15s、上限 60s）、`maxBytes`（响应体上限，默认 1MB、上限 8MB）；
  - **不是 Promise**：没有事件循环，`r.json` 是属性而不是方法，`await` 不存在；
  - 响应体是合法 JSON 时才有 `json` 字段，否则是 `undefined`；`headers` 的键统一小写；
  - **非 2xx 不抛错**（HTTP 语义）：脚本自己判 `ok`/`status`；抛错的只有传输层失败、
    URL 非法、超预算、超时；
  - 预算：单次脚本最多 **16 次请求 / 累计 8MB**。单请求上限挡不住
    `for (const id of ids) fetch(...)`，所以次数单独记账；
  - 超时取 `min(options.timeout, 脚本剩余时间)`。`vm.Interrupt` 打不断阻塞在 socket 上的
    Go 调用，所以这个超时必须由 http 层自己兜住，不能指望解释器；
  - **写网络需要 `permissions=auto`**：GET/HEAD 任何时候都放行（与 `web_fetch`
    一致），非 GET/HEAD 需要 `Policy.AllowNetworkWrite`（只有 `auto` 打开）。
    理由：`js` 的 `Mutates()` 是 `false`（模型正常拿它算数），放行写就等于绕过了
    确认弹窗——同一件事走 `bash` 会弹、走 `js` 不弹，那就是后门。被拒时的报错
    会把模型引到 `bash` 那条需要确认的路上；`readonly` 另外全部拦掉；
  - **不做 SSRF 过滤**（不拦 localhost/内网）：`bash` 本来就能 `curl`，在 js 上单独
    设域名白名单只会把模型赶回 shell，是「看着安全」的假边界；
- **没有 `require`/`process`**，也没有 `import`；除只读 fs + crypto + fetch 外不存在宿主能力；
- `args` 经 `JSON.parse` 注入：`args.list.push(x)` 能写回（早先直接塞 Go map，
  `args.a === args.a` 会是 `false`，修改静默丢失）；
- `console` 累计输出上限 1MB，超出后追加一行提示并丢弃，避免死循环式打印吃内存；
- 未捕获异常（含语法错误）返回 `IsError` 结果而不是 Go error，模型能看到错误并自行修正；
- `timeout` 或 ctx 取消都通过 `vm.Interrupt` 打断，死循环不会挂住进程（实测 100ms 超时对 `while(true){}` 生效）。

## 内建命令 `goja`（shell 里执行 JS）

内建解释器里还有一个 `goja` 命令，可以在管道里直接做纯计算，跨平台且不依赖
任何外部程序（不需要装 python/node）：

```sh
goja -e '1 + 2'                                  # => 3
printf '6 * 7' | goja                            # 从 stdin 读脚本
goja script.js a b c                             # argv = ["a","b","c"]
ls | goja -e 'argv.length' -- x y                # -- 之后是 argv
goja -t 200ms -e 'while(true){}' || echo TIMEOUT # 超时打断
```

| 选项 | 说明 |
|---|---|
| `-e, --eval <code>` | 直接执行代码（带 `-e` 时不用 stdin） |
| `-t, --timeout <时长>` | 执行上限，支持 `ms/s/m/h/d`，默认 `30s` |
| `-q, --quiet` | 不打印最后一个表达式的值 |
| `--` | 之后全部当作脚本参数 |
| `-h, --help` | 用法 |

脚本内可用能力基本与 `js` 工具**一致**：`console.*`、只读 `fs.*`（`readFile` /
`readFileBytes` / `readFileBase64` / `readDir` / `stat` / `exists` / `glob`）、
`crypto.sha256`，另有 `argv`。差异：

- **没有 `fetch`**：这条路径定位是管道里的纯计算；出网直接用 `curl` 更自然，
  也不用再为它维护一套超时/预算语义；
- 输出直接写终端：`console.log/info/debug` → stdout，`warn/error` → stderr；
- `fs` 同样受 `Policy.Root` 约束（越界直接抛异常，非零退出）；
- 未捕获异常 → stderr + 退出码 1（不打印 `=> ` 前缀，方便接管道）。

实现要点（`internal/tools/shell_goja.go`）：走 `interp.ExecHandlers` 中间件，
和 `ls/cat/sleep` 同一层；**PATH 里有真的 `goja` 时让真程序跑**（和别的内建
命令一致）。`Policy` 由 `newShellProcess(..., policy)` 一路传进来，所以 shell
里的 JS 看到的工作区范围和 `read`/`write` 工具相同。

## 自定义工具

```go
type MyTool struct{ tools.Policy }

func (*MyTool) Name() string            { return "mytool" }
func (*MyTool) Description() string     { return "……" }
func (*MyTool) Parameters() map[string]any {
    return tools.Schema(map[string]any{
        "path": tools.Prop("string", "文件路径"),
    }, "path")
}
func (*MyTool) ConcurrencySafe() bool   { return true }   // 只读
func (t *MyTool) Execute(ctx context.Context, a tools.Args) (tools.Result, error) {
    p, err := a.Required("path")
    if err != nil { return tools.ErrorResult("%v", err), nil }
    abs, err := t.Policy.Resolve(p)
    ...
    return tools.TextResult("ok: %s", abs), nil
}
```

`Execute` 返回 `(Result, error)` 的语义区分：

- `error != nil` → 宿主机层面的失败（会被包装成模型可见错误）；
- `Result{IsError:true}` → 工具逻辑失败，模型看到后可以自己重试（推荐用它表达「参数不对/文件不存在」）。

写文件的工具如果需要参与并发，除了 `ConcurrencySafe()==true` 还要实现
`PathScoped() bool`，并在 `Execute` 里用 `lockFile(abs)` 包住读-改-写：

```go
func (*MyTool) Mutates() bool     { return true }   // 权限层仍按改动处理
func (*MyTool) PathScoped() bool  { return true }   // 调度层允许并行

func (t *MyTool) Execute(ctx context.Context, a tools.Args) (tools.Result, error) {
	abs, err := t.Policy.Resolve(p)
	...
	unlock := tools.LockFile(abs)   // 同路径串行，异路径真并行
	defer unlock()
	...
}
```

## 安全策略

`Policy.Resolve` 做三件事：

1. 空路径 / `~` 展开 / 相对路径拼到 `Root`；
2. `AllowOutside=false` 时拒绝逃逸 `Root` 的路径（`../` 与绝对路径都检查）；
3. **对解析符号链接后的真实路径再检查一次** —— `root/link -> /etc` 这种绕过会被拦住。

`ReadOnly=true` 时 `write`/`edit` 直接返回错误；`js` 的 `fetch` 也只放行
GET/HEAD（GET 算读网，与 `web_fetch` 一致）。`CheckCommand` 拦 `rm -rf /`、`mkfs`
等明显破坏性命令，这是 best effort 的防手滑，**不是安全边界**。

`--permissions auto` 下整个 `Policy` 都置 `AllowOutside=true`
（`tools.Options.AllowOutside`，作用于 read/write/edit/ls/glob/grep/js 与 bash 的 `cwd`）。
理由：`auto` 已经放行所有改动型工具，而与 `bash` 相比 root 从来不是真边界
（`bash` 的命令与内建 `cat/ls/rm` 都不查路径，只查 `cwd`）；守住它只会逼模型绕道 shell。
`ask`/`readonly` 不放开——否则一次不经询问的越界读写就成了绕过确认的捷径。

## 截断预算

大输出不静默丢弃，而是保留头尾 + 明确标记（`truncate.go`）：

- 默认 `2000 行 / 50KB`，先到先算；
- 按行截断保留 `head:maxLines/2` + `tail:maxLines/2`；
- 按字节截断切点回退到 **UTF-8 rune 边界**（中文/emoji 不会切出半个字符）；
- `Result.Truncated` 让上层（如 ACP / kernel）知道该提示模型「内容不全，用 offset 继续」。

`read` 的分页提示是显式的：`[还有 N 行未显示，用 offset=M 继续]`。

## 后台进程

`bash` 前台等待有上限（默认 120s，硬上限 600s）；超时**不销毁输出**、也**不重跑命令**，而是：

1. 把已收到的输出连同提示一起返回；
2. 把**同一个进程**接管为后台会话（`BashManager.Attach`），返回 `sessionId`；
3. 模型用 `bash_output` 继续轮询，或 `bash_kill` 终止。

第 2 步早先的实现是 `Manager.Start(cmd, dir, env)` —— 重新执行整条命令。
对非幂等操作（`npm install`、`git commit`、`>>` 追加写）这是两遍副作用，
而且第一遍还停在被 kill 的半截状态。现在沿用同一个进程和同一块输出缓冲，
前台已产生但尚未取走的输出也一起带过去（`sharedOutput`）。

被接管进程的 context 从 `context.Background()` 派生（不是 turn 的 ctx）：
否则 turn 结束一取消，后台命令就被连带杀掉。`Kill` 会同时 cancel 它。

另外 `runForeground` 在取消路径上等进程退出有 `killGrace = 2s` 上限：
纯内建脚本（`while true; do :; done`）mvdan/sh 无法中断，没有这个上限的话
`<-done` 会永久阻塞，把整次工具调用和整个 turn 挂死。

进程终止分三层：

1. **Unix**：`Setpgid` 建独立进程组，`kill(-pid, SIGKILL)` 杀整组。
2. **Windows**：启动后把子进程放进 **Job Object**，`TerminateJobObject` 一次杀掉整棵树
   （含 Git Bash 再 fork 的 coreutils 子进程）。
3. **兵底**：`taskkill /F /T` + `Process.Kill()`。`taskkill` 依赖 ParentProcessId 链，
   实测杀不掉孙进程，所以只在 Job Object 创建失败时作为回退。

`execProc.Kill()` 拿到作业对象时**只调 `TerminateJobObject`**，不再 fork 一个 `taskkill`
（多一次进程创建约 100ms）。这条是让 acp-agent 自己审代码时挑出来的：原实现在
`job.terminate()` 后又无条件调了 `killTree`。

修好这两处后，超时/取消/`bash_kill` 的实测表现：

| 场景 | 修前 | 修后 |
|---|---|---|
| `bash -c "echo before; sleep 3; echo after"`，timeout=300ms | 2042ms | **350ms** |
| `bash_kill` 后 `bash_output` 看到 `done` | 永远 `running` | **100ms** |
| `sleep 30` 被 kill 后的 `sleep.exe` 残留 | 活到自然结束 | **立即消失** |
| `echo hit >> f && sleep 5`，timeout=200ms | `f` 里两行（重跑） | **一行**（接管） |

另外 `exec.Cmd.WaitDelay = 300ms` 被显式设置：即使孙进程意外拖住输出管道，
`Wait` 也不会无限阻塞（返回的 `exec.ErrWaitDelay` 被当作正常退出处理）。

## 网络安全（web_fetch）

`web_fetch` 的 URL 来自模型，而模型可能被网页里的提示注入操纵，所以默认
**拒绝抓取回环/私网/链路本地地址**（`127.0.0.1`、`10/8`、`192.168/16`、
`169.254.169.254` 云元数据、`100.64/10` CGNAT 等）。

两道关，缺一不可：

1. 发起请求前解析 URL 并判 IP —— 给模型一个明确原因；
2. `DialContext` 里对**实际解析出的 IP** 再判一次 —— 域名可以先解析到公网、
   连接时再解析到内网（DNS rebinding），只查 URL 拦不住。

`web_search` 用的是**另一个**不受限的 client：自建 searxng 常常就在 localhost。
要允许抓内网（自测、内网部署）显式配置 `web.fetch.allowPrivate: true`。

## edit 的文本容错

两个层次的宽容度（对齐上游 Pi 的 `edit-diff.ts`）：

**1. 编码/行尾归一**

- 匹配前剥掉 UTF-8 BOM（模型不会把不可见字符写进 `oldText`），写回时补上；
- 全文归一成 LF 再匹配，写回时还原**原始**行尾。所以 CRLF 文件 + LF 的
  `oldText`（以及反过来）都能命中，且不会把整个文件改成 LF。

**2. 精确匹配失败才降级到模糊匹配**

归一内容：NFKC（全角 ASCII→半角）、每行去行尾空白、智能引号→ASCII、
各式破折号/减号→`-`、NBSP/全角空格等→普通空格。**行首缩进不动**——缩进是语义。

- 模糊匹配只在「精确匹配 0 处」时尝试；命中后结果里会注明
  「其中 N 处为模糊匹配」，让调用方知道发生了什么；
- 模糊匹配同样要求唯一（多处命中报「不唯一」，请扩大上下文）；
- 被触及的**行**按归一后的形态重写，未触及的行逐字节保留原始内容 ——
  不会把整个文件"格式化"掉。

**3. 重叠预检**

增量语义下「第二个 `oldText` 落在第一个 `newText` 内部」会被静默命中，产出模型
根本没想要的结果：

```
原文  value = 10
edits [{old:"value = 10", new:"value = 100"}, {old:"= 10", new:"= 20"}]
增量执行 → value = 200   ← 模型想要的是两处独立修改，且不报错
```

所以在应用之前先做一次预检：**用原文**给每个 `oldText` 定位区间（精确优先，
失败再走模糊归一），两两相交就报

```
edits[0] 与 edits[1] 在原文中重叠，结果会取决于执行顺序。请合并成一个 edit，或改成互不相交的区域
```

两个刻意的边界：

- **链式写法继续可用**：某个 `oldText` 在原文里根本不存在、但增量匹配能命中
  （`x = 1` → `x = 2` 再 `x = 2` → `x = 3`），预检直接跳过它，不算重叠；
- 首尾相邻（`[0,3)` 与 `[3,6)`）不算重叠，只有真正相交才报。

**4. 参数形态容错**

`edits` 接受：数组（正常）、JSON 字符串（部分模型会序列化整个数组）、
单个对象（没包成数组）、以及旧的顶层 `oldText`/`newText`（与 `edits` 同时给出时并入末尾）。


## 测试

```bash
go test ./internal/tools/ -count=1          # 9s
go test ./internal/tools/ -count=1 -race    # 8s
```

调度正确性靠可观测断言，不靠时序碰运气：

- `TestExecutorRunsSafeToolsInParallel` — 4×80ms 只读工具总耗时 < 250ms；
- `TestExecutorFencesMutatingTools` — 写工具执行期间只读工具**从未**重叠（原子计数器）；
- `TestExecutorMaxParallel` — 并发峰值 `>=2 且 <=3`；
- `TestExecutorPreservesOrder` — 逆序完成的 10 个调用结果仍按输入排列；
- `TestParallelToolRejectsMutations` — `parallel` 里混入 `write` 被整体拒绝；
- `TestBashConcurrent` — 4×400ms 命令 < 1300ms。
