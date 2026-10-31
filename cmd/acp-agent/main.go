// Command acp-agent 是一个完整的终端编码 agent：
//
//	acp-agent                    交互式 REPL
//	acp-agent -p "修复 TestFoo"   非交互，流式输出到 stdout
//	acp-agent --acp              作为 ACP agent 服务（供 Zed 等编辑器调用）
//
// 配置优先级：命令行 flag > 环境变量 > ~/.config/acp-agent/config.json。
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"

	xterm "golang.org/x/term"

	"github.com/leijux/acp-agent/internal/acp"
	"github.com/leijux/acp-agent/internal/agent"
	"github.com/leijux/acp-agent/internal/mcp"
	"github.com/leijux/acp-agent/internal/provider"
	"github.com/leijux/acp-agent/internal/skills"
	"github.com/leijux/acp-agent/internal/tools"
	"github.com/leijux/acp-agent/internal/tui"
)

// errFlagsReported 表示解析失败已经打印过一行提示，main 不要再重复。
var errFlagsReported = errors.New("flag 解析失败（已报告）")

// defaultVersion 是未被 -X 注入时的值，同时充当「是否已注入」的哨兵。
const defaultVersion = "0.1.0"

// version 由构建时注入（-X main.version=...），这里是 go run/源码直用的默认值。
var version = defaultVersion

// resolveVersion 返回展示用版本号。task build / goreleaser 之类会注入 version;
// `go install ...@vX.Y.Z` 没有 -ldflags，此时回退到 build info 里的模块版本。
func resolveVersion() string {
	if version != defaultVersion {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		if v := bi.Main.Version; v != "" && v != "(devel)" {
			return v
		}
	}
	return version
}

func main() {
	if err := run(); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		if errors.Is(err, errFlagsReported) {
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "错误: %v\n", err)
		os.Exit(1)
	}
}

type flags struct {
	print          string
	provider       string
	model          string
	apiKey         string
	baseURL        string
	permissions    string
	root           string
	session        string
	cont           bool
	maxTurns       int
	maxTurnsSet    bool // --max-turns 被显式给出（含 0 / 负数 = 不限制）
	thinking       string
	temperature    float64
	temperatureSet bool
	// mode 是输出模式："" 是人类交互，"json" 是 Pi 兼容的事件流（供编排器）。
	mode           string
	noSession      bool
	noContextFiles bool
	noSkills       bool
	// Pi 的 no-* 系列：我们没有这些资源可关，但编排器（Multica 等）会按 Pi 的
	// 参数原样传进来。不认的后果很严重 —— flag 解析失败会打印整段 usage，
	// 而调用方在 stdout 为空时会拿 stderr 当模型目录，于是 usage 的每一行都被
	// 切成 "acp-agent/[flags]" 这样的假模型（真实故障，见 #multica）。
	noPromptTemplates bool
	noThemes          bool
	noExtensions      bool
	noApprove         bool
	noTools           bool // 关闭全部工具（含 MCP）
	noBuiltinTools    bool // 关闭内建工具，保留 MCP
	// extensions 是 --extension/-e 给出的 Pi 扩展文件。我们不执行扩展 JS，只从
// 文件里提取 mcpServers（某些编排器用它下发受管 MCP 配置）。
	extensions    []string
	acpMode       bool
	listEndpoints bool
	mcpStatus     bool
	showVersion   bool
	noColor       bool
	mcpConfig     string
	mcpJSON       string
	disableTools  string
	noSubagent    bool
	subagentCmd   string
	noMCP         bool
	webSearch     string
	webSearchKey  string
	noWeb         bool
	noTUI         bool
}

// newFlagSet 注册全部 flag。
//
// 单独抽出来是为了让 normalizePiArgs 能查「下一个 token 是不是已知 flag」：
// 只有已知 flag 才说明那个 -p 是无值开关，否则它就是被当成了 prompt 的值。
func newFlagSet(f *flags) *flag.FlagSet {
	fs := flag.NewFlagSet("acp-agent", flag.ContinueOnError)
	// 解析出错时 flag 包默认会往 stderr 打「flag provided but not defined」**并调用
	// Usage 打印整段用法**。这里的 stderr 会被编排器当成数据（Multica 的模型目录
	// 探测在 stdout 为空时回退去读 stderr），usage 的每一行都会被切成
	// "acp-agent/[flags]" 这样的假模型。所以整个输出通道关掉，只留下面的 UseUsage。
	fs.SetOutput(io.Discard)
	fs.StringVar(&f.print, "p", "", "非交互模式：执行一次 prompt 后退出（值为 - 时从 stdin 读）")
	fs.StringVar(&f.print, "print", "", "非交互模式（同 -p）")
	fs.StringVar(&f.provider, "provider", "", "模型服务商（deepseek/openai/ollama/...）")
	fs.StringVar(&f.model, "m", "", "模型 ID")
	fs.StringVar(&f.model, "model", "", "模型 ID（同 -m）")
	fs.StringVar(&f.apiKey, "api-key", "", "API key（默认读环境变量）")
	fs.StringVar(&f.baseURL, "base-url", "", "覆盖服务端点 URL")
	fs.StringVar(&f.permissions, "permissions", "", "工具权限：ask/auto/readonly")
	fs.StringVar(&f.root, "root", "", "工作目录（工具可访问范围的根）")
	fs.StringVar(&f.session, "session", "", "会话文件路径（JSONL）")
	fs.BoolVar(&f.cont, "c", false, "继续最近一次会话")
	fs.BoolVar(&f.cont, "continue", false, "继续最近一次会话（同 -c）")
	fs.IntVar(&f.maxTurns, "max-turns", 0, "单次运行的最大轮数；0 或负数 = 不限制（默认 500）")
	fs.StringVar(&f.thinking, "thinking", "", "思考等级：off/minimal/low/medium/high/max（默认 medium）")
	fs.Float64Var(&f.temperature, "temperature", 0, "采样温度（推理模型自动忽略）")
	fs.BoolVar(&f.acpMode, "acp", false, "以 ACP agent 模式运行（stdio JSON-RPC）")
	fs.StringVar(&f.mode, "mode", "", "输出模式：json（逐行 JSON 事件流，供 Multica 等编排器）；rpc 不支持")
	fs.BoolVar(&f.noSession, "no-session", false, "不落盘会话文件（Pi 兼容）")
	fs.BoolVar(&f.noContextFiles, "no-context-files", false, "不注入 AGENTS.md/CLAUDE.md 等上下文文件（Pi 兼容）")
	// --no-skills / --no-prompt-templates：Pi 探测目录时会带。no-skills 现在是
	// 真的（关掉 .agents/skills 发现）；prompt 模板仍未实现。
	fs.BoolVar(&f.noSkills, "no-skills", false, "不加载 .agents/skills（Pi 兼容）")
	for _, name := range []string{"extension", "e"} {
		fs.Func(name, "Pi 扩展文件；只提取其中的 mcpServers 作为 MCP 配置（不执行扩展 JS），可重复", func(v string) error {
			f.extensions = append(f.extensions, v)
			return nil
		})
	}
	fs.BoolVar(&f.listEndpoints, "endpoints", false, "列出内置服务端点后退出")
	fs.BoolVar(&f.mcpStatus, "mcp-status", false, "连接 MCP server 并打印状态后退出")
	fs.BoolVar(&f.showVersion, "version", false, "打印版本后退出")
	fs.BoolVar(&f.noColor, "no-color", false, "禁用 ANSI 颜色")
	fs.StringVar(&f.mcpConfig, "mcp-config", "", "MCP server 配置文件路径；也可直接传 JSON 字符串（默认查 <工作目录>/.mcp.json 与 <配置目录>/.mcp.json）")
	fs.StringVar(&f.mcpJSON, "mcp-json", "", "内联 MCP 配置 JSON，叠加在配置文件之上（同名 server 覆盖），用于临时注入")
	fs.StringVar(&f.disableTools, "disable-tools", "", "不注册的工具名（逗号分隔）")
	fs.BoolVar(&f.noSubagent, "no-subagent", false, "禁用 delegate（不启动 ACP 子代理）")
	fs.StringVar(&f.subagentCmd, "subagent", "", "子代理可执行文件（默认 self：自身以 --acp 模式启动）")
	fs.BoolVar(&f.noMCP, "no-mcp", false, "不连接任何 MCP server")
	fs.StringVar(&f.webSearch, "web-search", "", "web_search 后端：duckduckgo/searxng/tavily/brave/exa/off")
	fs.StringVar(&f.webSearchKey, "web-search-key", "", "web_search 后端的 API key（searxng/duckduckgo 不需要）")
	fs.BoolVar(&f.noWeb, "no-web", false, "关闭 web_search/web_fetch")
	fs.BoolVar(&f.noTUI, "no-tui", false, "用旧版行式 REPL（不用 Bubble Tea 界面）")

	// Pi 的 no-* 系列：我们没有对应资源可关的接受并忽略；no-tools / no-builtin-tools
	// 是真语义（走 DisableTools 的 "*"）。必须认全：解析失败会打印整段 usage，
	// 而编排器（Multica 的模型目录探测）在 stdout 为空时会拿 stderr 当目录解析，
	// usage 的每一行都会被切成 "acp-agent/[flags]" 这样的假模型。
	fs.BoolVar(&f.noPromptTemplates, "no-prompt-templates", false, "（兼容 Pi）本实现没有 prompt templates，忽略")
	fs.BoolVar(&f.noPromptTemplates, "np", false, "（兼容 Pi）同 --no-prompt-templates")
	fs.BoolVar(&f.noThemes, "no-themes", false, "（兼容 Pi）本实现没有主题，忽略")
	fs.BoolVar(&f.noExtensions, "no-extensions", false, "（兼容 Pi）忽略（扩展只用来提取 MCP 配置）")
	fs.BoolVar(&f.noExtensions, "ne", false, "（兼容 Pi）同 --no-extensions")
	fs.BoolVar(&f.noApprove, "no-approve", false, "（兼容 Pi）忽略")
	fs.BoolVar(&f.noApprove, "na", false, "（兼容 Pi）同 --no-approve")
	fs.BoolVar(&f.noTools, "no-tools", false, "不注册任何工具（含 MCP）")
	fs.BoolVar(&f.noTools, "nt", false, "同 --no-tools")
	fs.BoolVar(&f.noBuiltinTools, "no-builtin-tools", false, "不注册内建工具，保留 MCP 工具")
	fs.BoolVar(&f.noBuiltinTools, "nbt", false, "同 --no-builtin-tools")

	// Usage 默认丢弃：解析失败时 flag 包会调它，而那段文本会被编排器当成数据。
	// 只有 --help（flag.ErrHelp）时 run 才显式打到 stderr。
	fs.Usage = func() { printUsage(io.Discard, fs) }
	return fs
}

// printUsage 打印完整用法。只在 --help 时调用：解析失败只给一行错误
// （见 run），避免整段 usage 落进 stderr 被编排器当成数据。
func printUsage(w io.Writer, fs *flag.FlagSet) {
	fmt.Fprintf(w, "acp-agent %s — 终端编码 agent\n\n", resolveVersion())
	fmt.Fprintf(w, "用法:\n  acp-agent [flags]            交互式 REPL\n")
	fmt.Fprintf(w, "  acp-agent -p \"任务\" [flags]   执行一次后退出\n")
	fmt.Fprintf(w, "  acp-agent \"任务\" [flags]      同上（位置参数等价于 -p）\n")
	fmt.Fprintf(w, "  acp-agent --acp [flags]      作为 ACP agent 服务\n")
	fmt.Fprintf(w, "  acp-agent -p --mode json     Pi 兼容：prompt 读 stdin，stdout 逐行 JSON 事件\n\n")
	fmt.Fprintf(w, "示例:\n")
	fmt.Fprintf(w, "  export DEEPSEEK_API_KEY=sk-...\n")
	fmt.Fprintf(w, "  acp-agent -p \"列出 internal/ 下所有包的用途\"\n")
	fmt.Fprintf(w, "  acp-agent -c \"接着上次的继续改\"\n")
	fmt.Fprintf(w, "  acp-agent --provider ollama --model qwen3-coder\n")
	fmt.Fprintf(w, "  acp-agent --web-search tavily --web-search-key tvly-... -p \"最新 Go 1.28 有什么变化\"\n")
	fmt.Fprintf(w, "  acp-agent --no-web -p \"离线看一下代码\"\n")
	fmt.Fprintf(w, "  echo \"任务\" | acp-agent -p --mode json --session /tmp/s.jsonl\n\n")
	fmt.Fprintf(w, "flags:\n")
	fs.SetOutput(w)
	fs.PrintDefaults()
	fs.SetOutput(io.Discard)
}

func parseFlags(args []string) (*flags, error) {
	f := &flags{}
	fs := newFlagSet(f)
	// Pi 那边把 -p 当无值开关用（prompt 走 stdin），先把这种形状归一化。
	args = normalizePiArgs(args, fs)

	// Go 的 flag 解析遇到第一个非 flag 参数就停止，`acp-agent -c "任务" --provider openai"
	// 会让后面的 flag 全部被静默丢弃。这里循环解析：每轮把首个位置参数收走，
	// 剩下的部分重新交给 Parse，直到没有位置参数为止。
	var positional []string
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			break
		}
		positional = append(positional, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	if len(positional) > 0 {
		// 位置参数不再被丢弃：当作 prompt（等价 -p），与 -p 同时给则报错。
		if f.print != "" {
			return nil, fmt.Errorf("参数冲突：-p 已给定，位置参数 %q 无处安放", strings.Join(positional, " "))
		}
		f.print = strings.Join(positional, " ")
	}

	f.temperatureSet = false
	f.maxTurnsSet = false
	fs.Visit(func(fl *flag.Flag) {
		switch fl.Name {
		case "temperature":
			f.temperatureSet = true
		case "max-turns":
			// 只看值区分不了「没给」与「显式不限」：两者都是 0 附近的值，
			// 而语义相反（一个用默认兜底、一个完全不管）。必须问 flag 包。
			f.maxTurnsSet = true
		}
	})
	return f, nil
}

func run() error {
	// 模型目录是纯查询命令，不跑 agent，也不用先解析 flag（Pi 兼容）。
	if handled, err := handlePiCatalog(os.Args[1:], os.Stdout); handled {
		return err
	}
	f, err := parseFlags(os.Args[1:])
	switch {
	case errors.Is(err, flag.ErrHelp):
		// 显式 --help：flag 包已经把 Usage 调到了 Discard，这里补打到 stderr。
		printUsage(os.Stderr, newFlagSet(&flags{}))
		return nil
	case err != nil:
		// 解析失败只给一行：整段 usage 一旦落进 stderr，编排器（如 Multica 的模型
		// 目录探测）会把它当数据，usage 的每一行都被切成 "acp-agent/[flags]" 这样的假模型。
		fmt.Fprintf(os.Stderr, "acp-agent: %v\n（--help 查看完整用法）\n", err)
		return errFlagsReported
	}
	if f.showVersion {
		fmt.Println(resolveVersion())
		return nil
	}
	if f.listEndpoints {
		printEndpoints()
		return nil
	}

	cfg, err := LoadConfig()
	if err != nil {
		return err
	}
	// 环境变量覆盖配置文件（flag 在后面覆盖它俩）
	cfg.applyEnv()
	// flag 覆盖配置（优先级最高）
	cfg.applyFlags(f)

	// 在补默认值之前处理 --model 的 "provider/id" 前缀，这样能分清
	// 「provider 是用户指定的」和「provider 只是 applyDefaults 填的」。
	applyModelSelector(&cfg)
	cfg.applyDefaults()

	// Pi 兼容模式（编排器驱动）。
	if err := applyRunMode(f, &cfg); err != nil {
		return err
	}
	// --extension 里带的是调用方下发的 MCP 配置，叠加到已经读到的配置上。
	mergePiExtensions(&cfg, f.extensions)

	if abs, err := filepath.Abs(cfg.Root); err == nil {
		cfg.Root = abs
	}

	if f.mcpStatus {
		// 不需要 provider，纯诊断 MCP 配置。
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return runMCPStatus(ctx, cfg)
	}

	apiKey := resolveAPIKey(cfg, f.apiKey)
	prov, model, err := buildProvider(cfg, apiKey)
	if err != nil {
		return err
	}

	in := bufio.NewReader(os.Stdin)
	termOut := newTerminal(os.Stdout, f.noColor || os.Getenv("NO_COLOR") != "")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 只有「真终端 + 非 ACP + 非 -p」才能用 stdin 问权限。
	// ACP 模式下 stdin 属于 JSON-RPC，拿它问 y/n 会卡死并搞乱协议流；
	// -p 模式是一次性非交互运行，同样不该停下来等人。
	interactive := !f.acpMode && f.print == "" && xterm.IsTerminal(int(os.Stdin.Fd()))

	// quiet 必须在 newApp 之前置位：guard、web 后端这些拿到的 writer 在
	// 构造时就固化了，晚了它们还会往 stdout 写人读内容（污染管道/事件流）。
	// -p 模式：正文只在结束时打印一次（stdout 是结果），工具/用量日志走 stderr。
	if f.print != "" {
		termOut.quiet = true
	}

	// 分词器必须在 kernel 首次初始化前装上（进程级共享，选项只生效一次）。
	installSegmenter(cfg)

	app, err := newApp(ctx, cfg, prov, model, in, termOut, f.cont, interactive)
	if err != nil {
		return err
	}
	defer app.close()
	if f.cont {
		app.reportContinue()
	}

	switch {
	case f.acpMode:
		return app.serveACP(ctx)
	case f.print != "":
		prompt := f.print
		if prompt == "-" {
			b, err := io.ReadAll(os.Stdin)
			if err != nil {
				return fmt.Errorf("读取 stdin: %w", err)
			}
			prompt = string(b)
		}
		runErr := app.runOnce(ctx, prompt)
		// 事件流没写完（磁盘满、stdout 出错）时调用方只看到半截流，
		// 不能还给它们一个 0。管道被对面关掉不算错，已在 emit 里滤掉。
		if serr := app.eventStreamErr(); serr != nil {
			if runErr != nil {
				return fmt.Errorf("事件流写入失败: %w（run 同时失败: %v）", serr, runErr)
			}
			return fmt.Errorf("事件流写入失败: %w", serr)
		}
		return runErr
	default:
		// 真终端默认走 Bubble Tea 界面；stdout 被重定向时退回行式 REPL，
		// 否则整屏重绘的 ANSI 会把管道/文件写花。
		if interactive && !f.noTUI && xterm.IsTerminal(int(os.Stdout.Fd())) {
			return app.tuiRepl(ctx, f.cont)
		}
		return app.repl(ctx, f.cont)
	}
}

func printEndpoints() {
	fmt.Println("内置 OpenAI 兼容端点：")
	for _, e := range provider.Endpoints {
		key := e.EnvKey
		if key == "" {
			key = "(无需 key)"
		}
		fmt.Printf("  %-12s %-45s key: %s\n", e.Name, e.BaseURL, key)
		fmt.Printf("  %-12s 默认模型: %s\n", "", e.DefaultModel)
	}
	fmt.Println("\n模型（含上下文窗口 / 输出上限 / 定价）：")
	for _, id := range []string{
		"deepseek-flash", "deepseek-v4-pro", "gpt-5", "gpt-4.1", "claude-sonnet-4-5",
		"qwen3-coder-plus", "kimi-k2", "glm-4.6",
	} {
		info, ok := provider.LookupModel(id)
		if !ok {
			continue
		}
		cur := info.Currency
		if cur == "" {
			cur = "USD"
		}
		in, out := fmt.Sprintf("%g", info.Input), fmt.Sprintf("%g", info.Output)
		if info.InputOffPeak > 0 {
			// 浮动定价：峰/谷分开列，别让人误以为只有一档。
			in = fmt.Sprintf("%g~%g", info.InputOffPeak, info.Input)
			out = fmt.Sprintf("%g~%g", info.OutputOffPeak, info.Output)
		}
		fmt.Printf("  %-22s ctx=%-9d max_out=%-7d %s/M  in=%-10s out=%-10s\n",
			info.ID, info.ContextWindow, info.MaxOutput, cur, in, out)
	}
}

// ---------- app ----------

type app struct {
	cfg      Config
	prov     agent.Provider
	agent    *agent.Agent
	reg      *tools.Registry
	rawReg   *tools.Registry // 未被本地 guard 包裹的原始 registry（ACP 模式用）
	mcp      *mcp.Manager
	guard    *guard
	// ACP 会话按 cwd 重建工具集时复用的进程级能力。
	search    tools.SearchProvider
	fetch     tools.FetchProvider
	webUsable bool
	sess     *agent.Session
	sessPath string
	store    sessionStore
	in       *bufio.Reader
	term     *terminal
	started  time.Time

	// acpCur 是当前活动 ACP 会话。非 ACP 模式下恒为 nil。
	// 子代理的授权请求靠它转给客户端（session/request_permission）。
	acpMu  sync.RWMutex
	acpCur *acp.ServerSession

	// subPerm 记住子代理授权里的「总是允许/总是拒绝」。
	subPerm subagentPermCache

	// keys 是 REPL 的按键层（仅终端可用）；为 nil 时退回行模式读取。
	keys *keyWatcher

	// ui 是 Bubble Tea 界面句柄；非 TUI 模式（--no-tui、非终端）下为 nil。
	ui *tui.UI

	// jsonSink 是 --mode json 的事件流出口；其它模式下为 nil。
	// run() 收尾时用它把「事件没写完」变成非 0 退出码。
	jsonSink *piEventSink
}

func newApp(ctx context.Context, cfg Config, prov agent.Provider, model agent.Model, in *bufio.Reader, term *terminal, cont bool, interactive bool) (*app, error) {
	mode, err := parsePermMode(cfg.Permissions)
	if err != nil {
		return nil, err
	}
	if cfg.JSONMode {
		// 事件流占用 stdout，人读的渲染一律走 stderr。
		term.quiet = true
	}

	g := newGuard(mode, in, term.humanOut())
	g.interactive = interactive

	// delegate 通过 ACP 驱动另一个 acp-agent 进程（默认就是自身）。
	// ap 在下面才构造，故用闭包延迟取。
	var ap *app
	var runner tools.SubagentRunner = func(ctx context.Context, role, task string) (string, error) {
		if ap == nil {
			return "", fmt.Errorf("子代理尚未就绪")
		}
		return ap.runSubagent(ctx, role, task)
	}

	// web 能力（web_search / web_fetch）由 internal/web 提供；
	// 构造失败（如 tavily 缺 key）时降级为关闭，而不是拦住整个启动。
	searchProvider, fetchProvider, webUsable := buildWebProviders(cfg, term.humanOut())

	// 技能只从规范的 .agents/skills 发现（项目级 + 用户级），
	// 不读 Pi 私有的 .pi/skills（见 docs/skills.md）。
	skillSet := skills.Discover(skills.Options{
		Root:       cfg.Root,
		ExtraPaths: cfg.Skills,
		Disabled:   cfg.DisableSkills,
	})

	reg, bashMgr := tools.DefaultRegistry(tools.Options{
		Policy: tools.Policy{
			Root: cfg.Root, MaxReadBytes: 2 << 20,
			// 写网络（fetch 非 GET/HEAD）只在 auto 下放行：js 不是改动型工具，
			// ask 下放行就等于绕过了 bash 那条确认路径。
			AllowNetworkWrite: mode == modeAuto,
		AllowScriptWrite:  mode == modeAuto,
		},
		// auto 模式已全部放行，root 边界只会让模型绕道 bash（bash 的命令
		// 本身不受 Root 约束）；ask/readonly 保留边界。
		AllowOutside: mode == modeAuto,
		MaxParallel:  8,
		Disable:      registryDisable(cfg, webUsable),
		Subagent:     runner,
		Search:       searchProvider,
		Fetch:        fetchProvider,
		Skills:       skillSet.Skills,
	})
	// 保证子进程在退出时被回收。
	_ = bashMgr

	// MCP 工具在 wrapRegistry **之前**注册，这样远程工具和内置工具一样
	// 受同一套权限门禁约束（远程工具默认 mutating，必须确认）。
	mgr, err := connectMCP(ctx, cfg, reg, term, false)
	if err != nil {
		return nil, err
	}

	guarded := wrapRegistry(reg, g)
	// 关键：Executor 必须持有被门禁包裹的那个 Registry，
	// 否则工具会绕过权限直接执行。
	//
	// 注意 context_* 四个工具由 agent.New 在 wrapRegistry **之后** 注册，
	// 所以它们不经过这层门禁 —— 这是有意的：它们只读写会话历史，
	// 不碰文件/进程，压缩又是模型自主的行为（对齐上游主动模式），
	// 每次弹确认只会碍事。需要人工介入时用 /compact 或关掉主动模式。
	exec := tools.NewExecutor(guarded, 8)

	sess, sessPath, err := loadOrNewSession(cfg, cont)
	if err != nil {
		return nil, err
	}

	contextFiles := agent.DiscoverContextFiles(cfg.Root)
	if cfg.NoContextFiles {
		contextFiles = nil
	}
	// temperature 用指针表达「是否设置」：0 值不发送，避免覆盖服务端默认。
	var temperature *float64
	if cfg.Temperature != 0 {
		t := cfg.Temperature
		temperature = &t
	}

	opts := agent.Options{
		Provider: prov,
		Model:    model,
		Session:  sess,
		Registry: guarded,
		Executor: exec,
		Context: agent.ContextOptions{
			ContextFiles: contextFiles,
			Skills:       skillSet.Skills,
			WorkingDir:   cfg.Root,
		},
		MaxTurns:      cfg.MaxTurns,
		ContextWindow: model.ContextWindow,
		ThinkingLevel: cfg.ThinkingLevel,
		Temperature:   temperature,
	}
	a, err := agent.New(opts)
	if err != nil {
		return nil, err
	}

	ap = &app{
		cfg:       cfg,
		prov:      prov,
		agent:     a,
		reg:       guarded,
		rawReg:    reg,
		mcp:       mgr,
		guard:     g,
		search:    searchProvider,
		fetch:     fetchProvider,
		webUsable: webUsable,
		sess:      sess,
		sessPath:  sessPath,
		store:     newSessionStore(cfg.Root),
		in:        in,
		term:      term,
		started:   time.Now(),
	}
	a.On(ap.renderHandler())
	if cfg.JSONMode {
		// stdout 只走事件流：人读的渲染仍由 renderHandler 写 stderr（term.quiet）。
		out := cfg.JSONOut
		if out == nil {
			out = os.Stdout
		}
		ap.jsonSink = newPiEventSink(out)
		a.On(piEventHandler(ap.jsonSink, term.errOut))
	}
	return ap, nil
}

// eventStreamErr 报告事件流有没有写完（只有 --mode json 会用到）。
func (a *app) eventStreamErr() error { return a.jsonSink.Err() }

// loadOrNewSession 决定本次启动用哪个会话：
//   - 显式指定的文件（--session）优先；
//   - `-c` 接着本项目目录下最近一次的会话（REPL 与 -p 行为一致）；
//   - 否则新建一个空会话，真正落盘要等到有消息时（见 app.close）。
func loadOrNewSession(cfg Config, cont bool) (*agent.Session, string, error) {
	if cfg.Session != "" {
		return loadOrResetSession(cfg.Session)
	}
	if cont {
		if p := newSessionStore(cfg.Root).latest(); p != "" {
			if s, err := agent.LoadSession(p); err == nil {
				return s, p, nil
			}
		}
	}
	return agent.NewSession(), "", nil
}

// loadOrResetSession 加载调用方指定的会话文件，把「文件坏了」降级成新会话。
//
// 编排列器每一轮都会把同一个路径拿回来。以前 LoadSession 一报错就让整个进程
// 起不来（exit 1），那个任务就永久钉死在这个坏文件上了；现在改为：备份坏文件、
// 当新会话跑、结果写回同一路径，下一轮又能 resume。
func loadOrResetSession(path string) (*agent.Session, string, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// 文件不存在是正常的：Save 会创建它。
			return agent.NewSession(), path, nil
		}
		fmt.Fprintf(os.Stderr, "警告: 访问会话文件 %s 失败（%v），本次自动分配会话文件\n", path, err)
		return agent.NewSession(), "", nil
	}
	if info.IsDir() {
		fmt.Fprintf(os.Stderr, "警告: --session %s 是个目录，本次自动分配会话文件\n", path)
		return agent.NewSession(), "", nil
	}
	s, err := agent.LoadSession(path)
	if err == nil {
		return s, path, nil
	}
	backup := fmt.Sprintf("%s.corrupt-%d", path, time.Now().Unix())
	if rerr := os.Rename(path, backup); rerr == nil {
		fmt.Fprintf(os.Stderr, "警告: 会话文件 %s 无法解析（%v），已备份为 %s，本次用新会话\n", path, err, backup)
	} else {
		fmt.Fprintf(os.Stderr, "警告: 会话文件 %s 无法解析（%v），备份失败（%v），本次用新会话并在结束时覆盖它\n", path, err, rerr)
	}
	return agent.NewSession(), path, nil
}

func (a *app) close() {
	// close 只在进程退出时调用一次；真正的内容落盘逻辑在 saveSession。
	a.saveSession()
	closeMCP(a.mcp)
}

// saveSession 在每轮之后落盘，保证异常退出不丢历史。
// 之前 sessPath 为空时直接 return，导致 -p 模式只有在进程正常退出时
// （close 里才分配路径）才写盘，中途被 kill 就丢历史。
func (a *app) saveSession() {
	if a.cfg.NoSession {
		return
	}
	if len(a.sess.Messages()) == 0 {
		return
	}
	if a.sessPath == "" {
		a.sessPath = a.store.newPath()
	}
	if err := a.sess.Save(a.sessPath); err != nil {
		fmt.Fprintf(os.Stderr, "警告: 保存会话失败: %v\n", err)
	}
}

// reportContinue 明确告诉用户 -c 到底续上了没有。
// 以前找不到历史会话时是静默新建，看起来就像「-c 没生效」。
func (a *app) reportContinue() {
	switch n := len(a.sess.Messages()); {
	case a.sessPath == "":
		a.term.logf("%s\n", a.term.paint(cDim, "没有可继续的历史会话，已新建"))
	case n == 0:
		a.term.logf("%s\n", a.term.paint(cDim, "继续会话 "+a.sessPath+"（空）"))
	default:
		a.term.logf("%s\n", a.term.paint(cDim,
			fmt.Sprintf("继续会话 %s（%d 条消息）", a.sessPath, n)))
	}
}

// runOnce 是非交互模式：只把模型文本写到 stdout，工具日志走 stderr。
func (a *app) runOnce(ctx context.Context, prompt string) error {
	if strings.TrimSpace(prompt) == "" {
		return errors.New("prompt 为空")
	}
	res, err := a.agent.Run(ctx, prompt)
	a.saveSession()
	if res != nil {
		// JSON 模式下文字已经通过 message_update 事件出去了，再打一遍会把
		// stdout 弄成非法 JSON 行（调用方虽会跳过，但没必要）。
		if !a.cfg.JSONMode {
			for _, m := range res.Messages {
				if am, ok := m.(*agent.AssistantMessage); ok {
					if t := am.Text(); t != "" {
						fmt.Fprintln(a.term.out, t)
					}
				}
			}
		}
		a.term.printUsage(res.Usage, time.Since(a.started))
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	if res != nil && res.Aborted {
		return context.Canceled
	}
	return nil
}

// serveACP 以 ACP agent 模式运行：stdout 只走协议帧。
func (a *app) serveACP(ctx context.Context) error {
	perm := acp.PermissionAllowAll
	switch a.cfg.Permissions {
	case permAsk:
		perm = acp.PermissionAsk
	case permReadonly:
		perm = acp.PermissionReadOnly
	}
	// 工具集必须按会话的 cwd 重建，不能用进程级 registry：工具在构造时就把
	// Policy.Root（相对路径解析 / bash 默认 cwd / js 的 fs 根）固化了，共享
	// 一份等于让所有会话都用进程启动目录（客户端切项目时 cwd 会变）。
	// 权限门禁仍由 ACP 层的 PermissionGuard 接管，不要再叠 cmd 层 guard：
	// 那会在 stdin 上问 y/n（ACP 模式下 stdin 是 JSON-RPC），请求永远挂住。
	handler, err := acp.NewAgentHandler(acp.AgentConfig{
		Provider:   a.prov,
		Model:      a.agent.Model(),
		SessionFor: a.newACPSession,
		MaxTurns:   a.cfg.MaxTurns,
		Permission: perm,
	})
	if err != nil {
		return err
	}
	conn := acp.NewConn(os.Stdin, os.Stdout, nil)
	srv, err := acp.NewServer(conn, acp.ServerOptions{
		// 包一层：把「当前会话」记在 app 上，供 delegate 子代理把
		// 授权请求转给客户端（否则子代理照样卡在本地 guard 上）。
		Handler: &acpSessionTracker{Handler: handler, onSession: a.setACPSession},
		Info:    acp.Implementation{Name: "acp-agent", Version: resolveVersion()},
	})
	if err != nil {
		return err
	}
	// 以 ACP 模式运行时，日志必须只进 stderr。
	return srv.Serve()
}
