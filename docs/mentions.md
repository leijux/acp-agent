# @文件 / @文件夹（设计规格）

> 状态：**设计，未实现**。落地时按此写；有分歧先改这份文档。
>
> 目标：在提示词里写 `@path` 就能把该文件（或目录清单）灌进这一轮的上下文，
> 与 Pi / Claude Code 的 `@` 语义对齐；同时给交互界面（Bubble Tea）加 `@` 路径补全。

## 1. 为什么是「展开」而不是「让模型自己读」

模型当然可以用 `read` 工具读文件，但：

- 多一次工具往返（延迟、多一轮 token），且模型未必会去读；
- 用户显式 `@` 的意思是「我确定要你看这个」——这是**授权**，不该降级成建议；
- 编排器（Multica）会把 argv 里的 `@file` 参数丢掉（`filterPiCustomArgs`），
  但 prompt 正文里的 `@` 会原样到 stdin / `prompt` 命令 —— 展开由我们做正好补回语义。

代价是**上下文预算**：所以下面有硬上限，并且超出时留占位符而不是静默丢弃。

## 2. 语法

```
@path/to/file.go            文件：注入内容
@path/to/dir                目录：注入一层清单
@path/to/dir/               同上（尾斜杠只是显式声明意图）
@"path with space.txt"      带空格的路径（单/双引号都行）
\@literal                   转义：输出 @literal，不展开
@src/agent.go:10-40         行号：v1 只标注，不裁剪（见 §9-d）
```

规则：

- **token 起点**：`@` 且它前面是行首、空白，或 `(` `[` `{` `"` `'` `` ` `` 之一。
  前面是字母/数字/`_`/`-`/`.`/`/` 时**不算**（`a@b.com`、`foo@bar` 不误判）。
- **token 结束**：空白、行尾，或这些分隔符：`"'` + `` ` `` + `()[]{}<>` +
  中英文标点 `,;:!?，。；：、！？）】」』`（中文里紧贴路径打逗号很常见）。
  路径里允许 `-` `_` `.` `/` `\` `~`。
- 行号后缀 `:<数字>[-<数字>]` 从路径里剥出来，单独记在附件元信息里；
  v1 不裁剪内容（裁剪要与 `read` 的 offset/limit 语义对齐，留 P3）。
- 大小写：交给文件系统，不做归一化。
- 不加引号且路径含空格 → 解析到空格为止（大概率是 two tokens 里的第一个），
  找不到就按「不存在」处理并警告 —— 文档里教用户加引号。

## 3. 注入格式

在**同一条 user 消息**的正文前插入一个 `<attachments>` 块，`@token` 原文保留：

```
<attachments>
<file path="internal/tools/builtin.go" bytes="12345">
package tools
...
</file>
<dir path="internal/tools" entries="34">
builtin.go
grep.go
shell/
...
</dir>
</attachments>

看下 @internal/tools/builtin.go 里 skipDirs 的用法
```

- **保留 token 原文**：删掉会让句子读不通（"看下 里的用法"），而且模型需要
  建立「文本里提到的地方 ↔ 附件」的对应。
- **同 path 只注入一次**（按首次出现顺序），正文里多次出现不影响。
- **目录只列一层**：子目录以 `/` 结尾；不递归内容（递归会把整棵树塞进上下文，
  且用户想要内容时应该 `@某个文件`）。
- **截断标记**：`<file path="x" bytes="3355443" truncated="262144">`，
  内容尾部再加一行 `…（已截断：共 3.2MB，只注入前 256KB）`。
- **预算跳过的附件**：`<file path="y" skipped="budget"/>` —— 让模型知道
  「还有一个你没看到的东西」，而不是假装它不存在。
- 二进制：`<file path="z" skipped="binary" bytes="1258291"/>`。

## 4. 预算（硬上限，改这里等于改行为）

| 项 | 值 | 理由 |
|---|---|---|
| 单文件注入 | `min(Policy.MaxReadBytes, 256KB)` | 复用 `read` 的上限口径，别造第二套 |
| 一次提交文件数 | 20 | 再多就该用 glob/grep 了，而不是逐个 @ |
| 一次提交总注入 | 512KB | 约等于 128k token 的一半，留足正文与历史 |
| 目录清单条目 | 200，超出写 `…（还有 N 项）` | 清单只用来定位，不用来通读 |
| 二进制判定 | 前 8KB 含 NUL | 与 git 的启发式一致，够用 |

超预算时**按出现顺序**注入，直到预算耗尽，其余留 `skipped="budget"` 占位，
并在 `-p` 的 stderr / TUI 状态行提示「N 个附件因预算未注入」。

## 5. 接线（4 个入口 → 1 个函数）

新包 `internal/mentions`，纯逻辑（只读文件系统，不依赖 TUI/agent）：

```go
type Options struct {
    Root          string
    MaxFileBytes  int64 // 默认 256<<10
    MaxFiles      int
    MaxTotalBytes int64
    AllowOutside  bool
    SkipDirs      map[string]bool // 复用 internal/tools 的 skipDirs（需导出/抽包）
}

type Attachment struct {
    Path      string // 相对 Root 的展示路径
    Kind      string // "file" | "dir"
    Bytes     int64
    Truncated int64
    LineRange string // v1 只透传
    Skipped   string // "" | "budget" | "binary" | "missing" | "outside"
}

type Result struct {
    Text        string       // 展开后的完整 prompt
    Attachments []Attachment
    Warnings    []string     // 给用户看的一行一条
}

func Parse(text string) []Token           // 只扫描，不碰磁盘（补全也用它）
func Expand(opts Options, text string) Result
```

调用点（都在 `agent.Run` **之前**，因此历史里存的是展开后的文本，resume 不依赖磁盘再展开）：

| 入口 | 位置 | 说明 |
|---|---|---|
| `-p` / `--mode json` | `cmd/acp-agent/main.go` 的 `runOnce` 开头 | 一处覆盖两种模式 |
| `--mode rpc` | `rpcmode.go` 的 `appRunner.Run` → 同一个 `runOnce` | 自动覆盖 |
| TUI 一轮 | `cmd/acp-agent/tui_repl.go` 的 `tuiTurn` 开头 | 界面上仍显示用户敲的原文 |
| TUI 插话 | `tui_repl.go` 的 `submitSteer` 分支 | 不展开的话跨轮行为不一致 |
| ACP | **不展开**（用户拍板） | ACP 客户端有原生 `resource_link` |

实现上收敛成一个 `app.expandMentions(text string) string`：展开 + 把
`Warnings` 打到 stderr（TUI 走 note 行）+ 记一行摘要
`附件：3 个文件 12.4KB（含 1 个截断）`。**stdout 只放正文/JSON**，摘要走 stderr。

关闭开关：`--no-mentions`（`Config.NoMentions`）。理由：prompt 里合法出现 `@`
的场景（贴邮件、贴代码里的装饰器）需要一个不误伤的出口。

## 6. 补全

### V1（已实现）：Tab 补全，无浮层

- 触发：光标紧跟在 `@token` 后面时按 Tab。
- 候选：**只列 token 的父目录**（`@src/to` 只列 `src/` 下以 `to` 开头的项），
  跳过 `skipDirs`，不扫全仓库 —— 大仓库里也必须瞬间出结果。
- 唯一匹配 → 直接替换 token（目录补 `/`）；多个 → 补到**最长公共前缀**，
  状态行显示 `@src/ 12 个候选`。
- 下钻：目录补 `/` 后再 Tab 就是下一层，逐层走，不需要浮层。
- 键位：Tab 已从「插入一个 `\t`」改成补全（`internal/tui/model.go` 的 `onKey`）；
  要字面 Tab 只能靠粘贴（`Ctrl+I` 与 Tab 同字节，终端分不开）。写进文档。
- 实现：`internal/tui/complete.go` 的 `completeInput()`，扫盘走 `mentions.Complete`。
- TUI 为唯一交互界面（行式 REPL 已删），补全只在这一处实现。

### V2（可选）：候选浮层

- `layout()`（`model.go:869`）在候选打开时预留 `min(n, 8)` 行，插在 viewport 与
  输入行之间，viewport 相应减高 —— 必须守住 `viewString()`（`model.go:973`）
  「行数严格等于 `m.height`」这条不变式，否则行内重绘错位。
- `↑/↓` 在浮层打开时优先移动候选（`onKey` 最前面加分支），`Esc` 关闭，
  `Tab`/`Enter` 接受；输入继续则重新过滤。
- 渲染复用现有 ansi 常量，选中行反显。

## 7. 安全与可见性

- 路径解析**复用** `tools.Resolve` 的语义（root 内相对路径、`~` 展开、越界需
  `AllowOutside`）。建议把它抽到 `internal/fsutil`，`tools` 与 `mentions` 共用，
  避免两套规则漂移。
- 越界直接拒绝（`skipped="outside"`），不读文件。
- 不额外屏蔽 `.env` / 私钥：显式 `@` 就是显式授权。但**必须回显附件清单**
  （TUI note / stderr 一行），让误 @ 能被立刻发现 —— 这是可见性要求，不是过滤。

## 8. 测试清单

- `Parse`：行首/空白后/`(` 后、`a@b.com` 不误判、`\@` 转义、`@"带 空格.txt"`、
  中文标点终止、`:10-40` 后缀、token 在行尾、连续 `@@`。
- `Expand`：单文件、目录清单、二进制跳过、超大截断、总预算截断（含占位符）、
  不存在、越界拒绝、重复去重、`~` 展开、CRLF 原样保留、`--no-mentions` 直通。
- 接线：`-p` 带 `@` 时 fake provider 收到的 user 消息含 `<file path=…>`；
  TUI 重复提交不重复注入。
- 补全：`completeAt(text, cur)` 纯函数（唯一/多候选/公共前缀/目录补 `/`/无匹配不动）；
  V2 再加 `viewString()` 行数不变式。
- 不变式：同一 prompt 两次 `Expand` 结果一致（幂等，便于重试）。

## 9. 待拍板（附建议）

| # | 问题 | 建议 |
|---|---|---|
| a | ACP 是否也展开 `@` | **默认关**，加 `--acp-mentions`：ACP 客户端有原生 `resource_link`，`@` 文本可能被客户端自己消费，双份注入比不注入更糟 |
| b | 目录清单层级 | 1 层（子目录带 `/`）；要内容就 `@具体文件` |
| c | 是否保留 `@token` 原文 | 保留（§3） |
| d | `@a.go:10-40` 是否裁剪 | v1 只标注；P3 再按 `read` 的 offset/limit 裁剪 |
| e | 补全是否支持模糊/段缩写（`@t/b` → `internal/tools/builtin.go`） | V1 字面前缀，V2 再加 |
| f | Tab 不再插入 `\t` | 接受（终端无法区分 Ctrl+I） |

## 10. 分期

- **P1**：`internal/mentions` + 6 处接线 + 预算/安全/日志 + 测试。约 1 天。
- **P2**：Tab 补全 V1（TUI + `keyinput.go`）。约半天。
- **P3**：候选浮层 V2、行号裁剪、段匹配。约半天。

## 11. 与 Multica 的边界

- Multica 的 `filterPiCustomArgs` 会丢弃 argv 里的 `@file`（它把 prompt 放 stdin），
  但正文里的 `@` 会原样到达 —— 正是我们展开的位置；`--mode rpc` 的 `prompt`
  命令同理，`steer` 里的 `@` 也照此处理。
- 上游若哪天自己展开 `@`，我们就会双份注入：届时用 `--no-mentions` 兜底，
  或按版本门关掉（与 `defaultVersion` 同一套思路）。
