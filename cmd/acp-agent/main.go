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
	"github.com/leijux/acp-agent/internal/tools"
)

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
	thinking       string
	temperature    float64
	temperatureSet bool
	acpMode        bool
	listEndpoints  bool
	mcpStatus      bool
	showVersion    bool
	noColor        bool
	mcpConfig      string
	disableTools   string
	noSubagent     bool
	subagentCmd    string
	noMCP          bool
	webSearch      string
	webSearchKey   string
	noWeb          bool
	segmenter      string
	segmenterDict  string
}

func parseFlags(args []string) (*flags, error) {
	f := &flags{}
	fs := flag.NewFlagSet("acp-agent", flag.ContinueOnError)
	fs.StringVar(&f.print, "p", "", "非交互模式：执行一次 prompt 后退出")
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
	fs.IntVar(&f.maxTurns, "max-turns", 0, "单次运行的最大轮数")
	fs.StringVar(&f.thinking, "thinking", "", "思考等级：off/minimal/low/medium/high")
	fs.Float64Var(&f.temperature, "temperature", 0, "采样温度（推理模型自动忽略）")
	fs.BoolVar(&f.acpMode, "acp", false, "以 ACP agent 模式运行（stdio JSON-RPC）")
	fs.BoolVar(&f.listEndpoints, "endpoints", false, "列出内置服务端点后退出")
	fs.BoolVar(&f.mcpStatus, "mcp-status", false, "连接 MCP server 并打印状态后退出")
	fs.BoolVar(&f.showVersion, "version", false, "打印版本后退出")
	fs.BoolVar(&f.noColor, "no-color", false, "禁用 ANSI 颜色")
	fs.StringVar(&f.mcpConfig, "mcp-config", "", "MCP server 配置文件路径（默认查 <工作目录>/.mcp.json 与 <配置目录>/.mcp.json）")
	fs.StringVar(&f.disableTools, "disable-tools", "", "不注册的工具名（逗号分隔）")
	fs.BoolVar(&f.noSubagent, "no-subagent", false, "禁用 delegate（不启动 ACP 子代理）")
	fs.StringVar(&f.subagentCmd, "subagent", "", "子代理可执行文件（默认 self：自身以 --acp 模式启动）")
	fs.BoolVar(&f.noMCP, "no-mcp", false, "不连接任何 MCP server")
	fs.StringVar(&f.webSearch, "web-search", "", "web_search 后端：duckduckgo/searxng/tavily/brave/exa/off")
	fs.StringVar(&f.webSearchKey, "web-search-key", "", "web_search 后端的 API key（searxng/duckduckgo 不需要）")
	fs.BoolVar(&f.noWeb, "no-web", false, "关闭 web_search/web_fetch")
	fs.StringVar(&f.segmenter, "segmenter", "", "中文分词器：rune（默认，单字+bigram）/gse/sego")
	fs.StringVar(&f.segmenterDict, "segmenter-dict", "", "sego 的词典文件路径（多个用逗号分隔）")

	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "acp-agent %s — 终端编码 agent\n\n", resolveVersion())
		fmt.Fprintf(os.Stderr, "用法:\n  acp-agent [flags]            交互式 REPL\n")
		fmt.Fprintf(os.Stderr, "  acp-agent -p \"任务\" [flags]   执行一次后退出\n")
		fmt.Fprintf(os.Stderr, "  acp-agent \"任务\" [flags]      同上（位置参数等价于 -p）\n")
		fmt.Fprintf(os.Stderr, "  acp-agent --acp [flags]      作为 ACP agent 服务\n\n")
		fmt.Fprintf(os.Stderr, "示例:\n")
		fmt.Fprintf(os.Stderr, "  export DEEPSEEK_API_KEY=sk-...\n")
		fmt.Fprintf(os.Stderr, "  acp-agent -p \"列出 internal/ 下所有包的用途\"\n")
		fmt.Fprintf(os.Stderr, "  acp-agent -c \"接着上次的继续改\"\n")
		fmt.Fprintf(os.Stderr, "  acp-agent --provider ollama --model qwen3-coder\n")
		fmt.Fprintf(os.Stderr, "  acp-agent --web-search tavily --web-search-key tvly-... -p \"最新 Go 1.28 有什么变化\"\n")
		fmt.Fprintf(os.Stderr, "  acp-agent --no-web -p \"离线看一下代码\"\n\n")
		fmt.Fprintf(os.Stderr, "flags:\n")
		fs.PrintDefaults()
	}

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
	fs.Visit(func(fl *flag.Flag) {
		if fl.Name == "temperature" {
			f.temperatureSet = true
		}
	})
	return f, nil
}

func run() error {
	f, err := parseFlags(os.Args[1:])
	if err != nil {
		return err
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
	cfg.applyDefaults()

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
	// -p 模式：正文只在结束时打印一次（stdout 是结果），
	// 工具/用量日志走 stderr，便于管道处理。
	if f.print != "" {
		termOut.quiet = true
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
		return app.runOnce(ctx, prompt)
	default:
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
}

func newApp(ctx context.Context, cfg Config, prov agent.Provider, model agent.Model, in *bufio.Reader, term *terminal, cont bool, interactive bool) (*app, error) {
	mode, err := parsePermMode(cfg.Permissions)
	if err != nil {
		return nil, err
	}

	g := newGuard(mode, in, term.out)
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
	searchProvider, fetchProvider, webUsable := buildWebProviders(cfg, term.out)

	reg, bashMgr := tools.DefaultRegistry(tools.Options{
		Policy:      tools.Policy{Root: cfg.Root, MaxReadBytes: 2 << 20},
		MaxParallel: 8,
		Disable:     registryDisable(cfg, webUsable),
		Subagent:    runner,
		Search:      searchProvider,
		Fetch:       fetchProvider,
	})
	// 保证子进程在退出时被回收。
	_ = bashMgr

	// MCP 工具在 wrapRegistry **之前**注册，这样远程工具和内置工具一样
	// 受同一套权限门禁约束（远程工具默认 mutating，必须确认）。
	mgr, err := connectMCP(ctx, cfg, reg, term)
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
		cfg:      cfg,
		prov:     prov,
		agent:    a,
		reg:      guarded,
		rawReg:   reg,
		mcp:      mgr,
		guard:    g,
		sess:     sess,
		sessPath: sessPath,
		store:    newSessionStore(cfg.Root),
		in:       in,
		term:     term,
		started:  time.Now(),
	}
	a.On(ap.renderHandler())
	return ap, nil
}

// loadOrNewSession 决定本次启动用哪个会话：
//   - 显式指定的文件（--session）优先；
//   - `-c` 接着本项目目录下最近一次的会话（REPL 与 -p 行为一致）；
//   - 否则新建一个空会话，真正落盘要等到有消息时（见 app.close）。
func loadOrNewSession(cfg Config, cont bool) (*agent.Session, string, error) {
	if cfg.Session != "" {
		if _, err := os.Stat(cfg.Session); err == nil {
			s, err := agent.LoadSession(cfg.Session)
			if err != nil {
				return nil, "", fmt.Errorf("加载会话 %s: %w", cfg.Session, err)
			}
			return s, cfg.Session, nil
		}
		return agent.NewSession(), cfg.Session, nil
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

func (a *app) close() {
	// close 只在进程退出时调用一次；真正的内容落盘逻辑在 saveSession。
	a.saveSession()
	closeMCP(a.mcp)
}

// saveSession 在每轮之后落盘，保证异常退出不丢历史。
// 之前 sessPath 为空时直接 return，导致 -p 模式只有在进程正常退出时
// （close 里才分配路径）才写盘，中途被 kill 就丢历史。
func (a *app) saveSession() {
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
		for _, m := range res.Messages {
			if am, ok := m.(*agent.AssistantMessage); ok {
				if t := am.Text(); t != "" {
					fmt.Fprintln(a.term.out, t)
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
	// 关键：用原始 registry，不要用已经包了本地 guard 的 a.reg。
	// ACP 模式下授权由客户端的 session/request_permission 裁决；再叠一层
	// cmd 层 guard 的话，它会在 stdin 上问 y/n（没有人答）——
	// 请求永远挂在半路，客户端弹窗还没机会出现。
	reg := a.rawReg
	if reg == nil {
		reg = a.reg
	}
	handler, err := acp.NewAgentHandler(acp.AgentConfig{
		Provider:   a.prov,
		Model:      a.agent.Model(),
		Registry:   reg,
		Executor:   tools.NewExecutor(reg, 8),
		Context:    agent.ContextOptions{WorkingDir: a.cfg.Root},
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
