# Skills（`.agents/skills`）

**状态：设计已定，代码未实现。本文是实现规格，不是现状描述。**

当前 acp-agent 不读任何 skill：`--no-skills` 是空壳（`cmd/acp-agent/main.go:131` 里
`fs.Bool("no-skills", false, "兼容 Pi：忽略（无 skill 机制）")`），`docs/cli.md` 也写着
「skill 注入位置 `.pi/skills/` 我们不读，属于静默失效」。

规范依据：[Agent Skills specification](https://agentskills.io/specification)。
语义对齐上游 Pi（`pi.exe` bundle 里的 `loadSkills` / `formatSkillsForPrompt` /
`validateName`），**位置只取规范的 `.agents`**，不读 Pi 私有的 `.pi`。

---

## 1. 为什么要有

| 现状 | 后果 |
|---|---|
| 模型只有 system prompt + AGENTS.md 两层文本上下文 | 团队沉淀的「怎么做某类任务」只能塞进 AGENTS.md，越长越占上下文，且对所有任务都生效 |
| skill 是**按需加载**的：prompt 里只放 name/description/location，正文在模型判断匹配时才读 | 几十个 skill 常驻上下文只花几百 token |
| 第三方（Claude Code、Antigravity、Pi 等）已按 `.agents/skills` 发放资源 | 读同一份目录即可复用，不需要转换格式 |

---

## 2. 范围

### 支持

- 项目级 `<dir>/.agents/skills/**/SKILL.md`
- 用户级 `~/.agents/skills/**/SKILL.md`
- 配置项 `skills: ["<path>", …]` 追加自定义目录
- 递归发现（任意层级，只认文件名恰好 `SKILL.md`）
- frontmatter：`name`、`description`、`disable-model-invocation`（其余字段忽略但保留原样）
- system prompt 注入 skill 名单
- `skill` 工具按需加载正文与 bundled 文件

### 明确不做

| 不做 | 原因 |
|---|---|
| `~/.pi/agent/skills/`、`<root>/.pi/skills/` | 只用规范位置；Pi 私有布局不复制（见 §8 的取舍后果） |
| `/skill:name` 斜杠命令 | 要动 REPL 命令表与「参数追加到指令后」的语义，本期不做。加载走 `skill` 工具 |
| `allowed-tools` 的强制 | 规范里是实验字段；渲染进 prompt 反而诱导模型跳过权限门禁 |
| npm/git 包分发（Pi packages） | 与 skill 发现是两件事，先有本地目录 |
| `.gitignore` 感知 | 只扫 `SKILL.md`，误扫面很小；引 `ignore` 依赖不划算 |
| 散装单文件 skill（`xxx.md`） | 规范的便携形态是「目录 + SKILL.md」 |
| 热重载（`/reload`） | 没有对应命令；重启即重扫 |

---

## 3. 发现规则

按优先级从高到低，**同名保留先发现的**并输出一条 warning（Pi 行为）：

| 优先级 | 位置 | 扫描方式 |
|---|---|---|
| 1 | `<cwd>/.agents/skills/` | 从 cwd **逐层向祖先**找 `.agents/skills`，遇到 `.git` 目录那一层扫完即停（没有 `.git` 则到盘根） |
| 2 | `~/.agents/skills/` | 单次 |
| 3 | `settings.skills` 里的每个路径 | 单次，按配置顺序 |

每个目录内递归（`filepath.WalkDir`），命中条件：文件名大小写敏感地等于 `SKILL.md`。
符号链接**文件**跟随；符号链接**目录**不跟随（避免环）。

项目级向上扫描的意义：在 `<repo>/pkg/foo` 里启动也能用到 `<repo>/.agents/skills`，
与 Pi「project `.agents/skills` 从 cwd 到仓库根」一致。

---

## 4. frontmatter

只解析 YAML 的一个最小子集，**不引入 yaml 依赖**（`go.mod` 现在没有）：

- 文件必须以 `---\n` 开头，到下一个 `---` 行为止是 frontmatter；
- 只认顶层 `key: value`；
- 值支持：裸串、单/双引号串、`true`/`false`、`|` 块标量（`description: |` 后按缩进取行）；
- 嵌套映射、列表、锚点等一律忽略（不报错）。

| 字段 | 用途 |
|---|---|
| `name` | 工具参数里的名字；缺省取父目录名（与 Pi 一致） |
| `description` | 决定模型「什么时候该加载」；**必填** |
| `disable-model-invocation` | `true` 时不进 prompt 名单，但仍可用 `skill` 工具显式加载 |

校验（照 Pi，全部是 warning，不阻断加载）：

| 规则 | 违反时 |
|---|---|
| `^[a-z0-9-]+$` | warning，仍加载 |
| 长度 ≤ 64 | warning，仍加载 |
| 不以 `-` 开头/结尾、不含 `--` | warning，仍加载 |
| 有 `description` | **没有 → 整个 skill 跳过**（Pi 行为） |

frontmatter 与正文都读进内存：正文在「加载」时才用，发现阶段不预读全文以外的处理
（`SKILL.md` 通常几 KB，一次性读入可接受）。

---

## 5. system prompt 注入

插在 `## 项目上下文`（AGENTS.md）**之前**，格式照抄 Pi（社区模型见过这个结构）：

```
The following skills provide specialized instructions for specific tasks.
Use the skill tool (or the read tool) to load a skill's file when the task matches its description.
When a skill file references a relative path, resolve it against the skill directory
(parent of SKILL.md / dirname of the path) and use that absolute path in tool commands.

<available_skills>
  <skill>
    <name>pdf-tools</name>
    <description>Extract text and tables from PDF files. Use when reading, converting, or inspecting PDFs.</description>
    <location>/home/u/.agents/skills/pdf-tools/SKILL.md</location>
  </skill>
</available_skills>
```

- 只输出 `name` / `description` / `location`，**不放正文**；
- 值做 XML 转义；
- `disable-model-invocation: true` 的条目不出现；
- 上限 `MaxSkills = 64`（按发现顺序取），超出时末尾追加一行 `[另有 N 个 skill 未列出]`；
- `description` 截到 1024 字符（规范上限）；
- 一个 skill 都没有时**整段不输出**（不留空 section）。

---

## 6. `skill` 工具

```go
type SkillTool struct{ Skills []skills.Skill }   // 构造时注入发现结果

Name()        "skill"
Mutates()     false
ConcurrencySafe() true
```

| 参数 | 行为 |
|---|---|
| （无） | 列出全部 skill：`name` + `description` + 目录；一行一个 |
| `name` | 返回该 skill 的 `SKILL.md` 全文，前面加一行 `[skill dir: <baseDir>]`，后面附同目录其它文件的清单（`scripts/`、`references/` 等），提示相对路径以该目录为基准 |
| `name` + `file` | 返回 `<baseDir>/<file>` 的内容（只读，路径越出 baseDir 直接拒绝）；用于读 bundled 脚本/参考文档 |

约束：

- 名字未知 → 返回 `IsError` 结果，附可用名单（一次性纠正，不要让模型猜）；
- 单次返回内容按工具层既有预算截断（`Truncate`），提示用 `file` 参数分段读；
- 不过权限门禁（只读、无副作用），与 `read` 同级；`permissions=readonly` 下可用。

### 与 kernel 的契约（不能改名）

`internal/kernel/kernel.dist.cjs` 的 `toolPathOf()` 已经把
`toolName == "skill"` 的调用归一成 `skill/<name>`，并从 args 的 `skill` / `name` /
`command` 取名字；它还会把提到 `…/SKILL.md` 的调用锚定成 `skill/<name>`。

所以：**工具名必须是 `skill`，参数名必须是 `name`**，否则压缩/检索层的锚定失效
（表现为压缩后 skill 相关内容失去 `skill/<name>` 标签、BM25 命中变差）。

---

## 7. 开关与接线

| 开关 | 语义 |
|---|---|
| `--no-skills` | 现有 flag 变成真的：不发现、不注入、不注册 `skill` 工具 |
| `disableSkills: true` | 同上，从 config.json 关 |
| `skills: ["<path>", …]` | 追加自定义发现目录（字段名与 Pi 兼容） |

| 入口 | 发现时机 |
|---|---|
| REPL / `-p` / `--mode json` | 启动时按 `cfg.Root` 发现一次（`cmd/acp-agent/main.go` 里 `DiscoverContextFiles` 旁边） |
| ACP（`--acp`） | `newACPSession(cwd)` 里按会话 cwd 重新发现，与工具集隔离方式保持一致 |
| `delegate` 子代理 | 默认继承（子代理 `--root` 相同，自然发现同一批）；要更窄就在 `subagent.args` 加 `--no-skills` |

---

## 8. 与外部系统的落差

| 系统 | 行为 | 后果 |
|---|---|---|
| Multica | 给「pi 协议族」运行时注入的是 `{workDir}/.pi/skills/`（`server/internal/daemon/execenv/context.go:386`） | **不生效**。要生效只有两条路：Multica 把注入路径改成 `.agents/skills/`（它已为 antigravity 那样写过），或我们在部署时建软链接。本期不加 `.pi` 兼容层 |
| Pi 本体 | 用 `read` 工具加载 SKILL.md | 我们用 `skill` 工具（原因：用户级 skill 在 `Policy.Root` 之外，`ask/readonly` 下 `read` 会被拒；且 kernel 已按 `skill` 名字做锚定） |
| Claude Code / Antigravity | 读 `.agents/skills`（规范位置） | 资源可直接复用 |

---

## 9. 文件分工

| 文件 | 职责 |
|---|---|
| `internal/skills/skills.go` | `Skill` 结构、`Discover`（祖先扫描 + 优先级 + 冲突）、frontmatter 解析、校验、`PromptBlock` 渲染 |
| `internal/skills/skills_test.go` | tempdir 造技能：发现/递归/祖先/优先级/冲突/坏 frontmatter/缺 description/disable-model-invocation/块标量/CRLF |
| `internal/tools/skill.go` | `skill` 工具（list / load / file），随附文件限在技能目录内 |
| `internal/tools/skill_test.go` | 列表、加载、file 越界拒绝、未知名字给名单、工具契约（名字/参数/只读）、注册开关 |
| `internal/agent/context.go` | `ContextOptions.Skills` + 在「项目上下文」前渲染 `## Skills` |
| `cmd/acp-agent/main.go`、`acp_sessions.go`、`config.go`、`pimode.go` | 发现、接线，以及 `--no-skills` / `disableSkills` / `skills` 三个开关 |

---

## 10. 测试覆盖（落地时按此写）

下表已全部落到 `internal/skills/skills_test.go`、`internal/tools/skill_test.go`、
`internal/agent/agent_test.go` 与 `cmd/acp-agent/*_test.go`。

| 用例 | 断言 |
|---|---|
| 项目级发现 | `<root>/.agents/skills/a/SKILL.md` 被发现，name 默认取父目录名 |
| 祖先扫描 | 在 `<root>/pkg/x` 为 cwd 时仍能发现 `<root>/.agents/skills`，且 `.git` 之上不扫 |
| 递归 | `team/review/SKILL.md` 被发现 |
| 优先级 | 项目与用户同名 → 保留项目，产 warning |
| 缺 description | 该 skill 不出现 |
| 坏 name | 仍加载，产生 warning |
| `disable-model-invocation` | prompt 块里没有它，但 `skill` 工具能加载 |
| prompt 渲染 | 转义 `<`/`&`；无 skill 时整段不输出；超 `MaxSkills` 追加省略行 |
| 工具 | 无参列表；`name` 返回正文 + baseDir；`file` 越界拒绝；未知名字带可用名单 |
| `--no-skills` | 注册表里没有 `skill` 工具，prompt 无该段 |

---

## 11. 实施顺序（已按此落地）

1. `internal/skills`（发现 + 解析 + 校验 + 渲染）+ 单测
2. `internal/tools/skill.go` + 单测
3. `internal/agent/context.go` 注入 + 单测
4. `cmd/acp-agent` 接线（`--no-skills` / `disableSkills` / `skills`）+ ACP 会话
5. 文档（`docs/tools.md`、`docs/cli.md`）

每步独立可测；第 3 步（prompt 注入）之前不影响任何现有行为。
