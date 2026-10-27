package main

// Pi 兼容层。
//
// 有些编排器（例如 Multica 的自托管守护进程）不实现自己的协议，而是按 Pi CLI
// 的既有契约驱动本机运行时：
//
//	acp-agent -p --mode json --session <path> [--model <selector>] \
//	           [--thinking <level>] [<custom args>] [--extension <mcp.mjs>]
//
//	- prompt 从 stdin 读到 EOF（不是 argv）；
//	- stdout 每行一个 JSON 事件，stderr 才是人读的日志;
//	- `--session` 指向的 JSONL 文件由调用方预先创建，后续用同一路径恢复会话;
//	- `--list-models` 打印模型目录（`models --json` 是 omp 的变体）。
//
// 本文件只做两件事：把这种 argv 翻译成我们自己的 flag，把 agent.Event 翻译成
// Pi 的事件流。没有引入新协议——internal/agent/events.go 里的事件名本来就是
// 照 Pi 抄的，字段名也对得上（Usage 的 json tag 就是 input/output/cacheRead/
// cacheWrite/totalTokens）。

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"syscall"

	"github.com/leijux/acp-agent/internal/agent"
	"github.com/leijux/acp-agent/internal/mcp"
	"github.com/leijux/acp-agent/internal/provider"
)

const (
	// piModeJSON 让 stdout 只输出事件流。
	piModeJSON = "json"
	// piModeRPC 是 Pi 的 JSON-RPC 模式（Multica 用它探测模型目录）。我们没实现，
	// 但要明确报错而不是挂住：探测方有超时预算，失败后会自动回退到 --list-models。
	piModeRPC = "rpc"

	// piStdinPrompt 是「prompt 从 stdin 读」的哨兵值，与 run() 里的约定一致。
	piStdinPrompt = "-"
)

// normalizePiArgs 处理 Pi 那边「无值开关」的写法。
//
// Pi 的 `-p` / `--print` 是**开关**：prompt 走 stdin，所以命令行里常常是
// `-p --mode json` 这种「flag 紧跟 flag」的形状。Go 的 flag 包把 `-p` 当字符串
// 选项，会直接吃掉后面的 `--mode`。这里把这种形状改成 `-p=-`（`-` 表示读 stdin）。
//
// 判定依据是「下一个 token 是不是已注册的 flag」，而不是「是不是以 - 开头」：
// prompt 本身可能以 - 开头（例如贴一段 markdown 列表），那些必须原样当值。
func normalizePiArgs(args []string, fs *flag.FlagSet) []string {
	known := func(tok string) bool {
		name := strings.TrimLeft(tok, "-")
		if i := strings.IndexByte(name, '='); i >= 0 {
			name = name[:i]
		}
		return name != "" && fs.Lookup(name) != nil
	}
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "-p", "--print":
			bare := i+1 >= len(args)
			if !bare {
				next := args[i+1]
				bare = strings.HasPrefix(next, "-") && known(next)
			}
			if bare {
				out = append(out, arg+"="+piStdinPrompt)
				continue
			}
			out = append(out, arg, args[i+1])
			i++
		default:
			out = append(out, arg)
		}
	}
	return out
}

// applyModelSelector 把 "provider/id" 形式的 --model 选择器拆成 provider + model。
//
// 只在「没指定 provider」或「前缀就是当前 provider」时才拆：
// openrouter 的模型 ID 天生带斜杠（openai/gpt-5、anthropic/claude-sonnet-4.5），
// 无脑拆会静默换端点、换计费。调用方需在 applyDefaults 之前调（那时
// Provider 为空才代表「用户没选」）。
func applyModelSelector(cfg *Config) {
	p, m, ok := splitModelSelector(cfg.Model)
	if !ok {
		return
	}
	if cfg.Provider != "" && !strings.EqualFold(cfg.Provider, p) {
		return
	}
	cfg.Provider, cfg.Model = p, m
}

// applyRunMode 校验 --mode 并把它翻译成配置。--mode rpc 是 Pi 的 JSON-RPC
// 探测面，我们没实现；它带着超时预算，失败后会回退到 --list-models，所以明确
// 报错比挂了强。
func applyRunMode(f *flags, cfg *Config) error {
	switch f.mode {
	case "":
	case piModeRPC:
		return errors.New("--mode rpc 未实现（模型目录请用 --list-models 或 models --json）")
	case piModeJSON:
		// 这个模式的前提是「一次运行、prompt 从 stdin 来」。漏了 -p 会掉进
		// REPL/TUI，人读界面和事件流就撞在同一个 stdout 上了。
		if f.acpMode {
			return errors.New("--acp 与 --mode json 不能同时使用")
		}
		if f.print == "" {
			return errors.New("--mode json 需要 -p（prompt 从 stdin 读）")
		}
		cfg.JSONMode = true
	default:
		return fmt.Errorf("未知 --mode %q（可选：json）", f.mode)
	}
	cfg.NoContextFiles = f.noContextFiles
	if f.noSession {
		cfg.Session = ""
		cfg.NoSession = true
	}
	// 编排列器不会有人回答权限询问：非交互时 ask 会把所有改动型工具直接拒掉。
	// 所以 JSON 模式下把 ask 提成 auto（显式 readonly 保留）。
	// 用 parsePermMode 判定而不是字符串比较："Ask" / " ask " 也是 ask。
	if cfg.JSONMode {
		if mode, err := parsePermMode(cfg.Permissions); err == nil && mode == modeAsk {
			cfg.Permissions = permAuto
		}
	}
	return nil
}

// ── 模型目录（Pi 的 --list-models / omp 的 models --json） ──

// piCatalogEntry 是模型目录的一行。Selector 是 provider/id，即写入 --model 的
// 形态：编排列出它，下一次运行再把它原样传回来。
type piCatalogEntry struct {
	Provider      string `json:"provider"`
	ID            string `json:"id"`
	Selector      string `json:"selector,omitempty"`
	Label         string `json:"name,omitempty"`
	ContextWindow int    `json:"contextWindow,omitempty"`
	MaxOutput     int    `json:"maxOutput,omitempty"`
}

// handlePiCatalog 识别并执行模型目录命令，返回是否已经处理完。
//
//	acp-agent --list-models        Pi 的表格格式（Multica 解析这个）
//	acp-agent models [--json]      对象格式 {"models":[…]}
//
// 只认第一个参数：prompt 里恰好出现 --list-models 这样的字符串时不能被当成目录命令。
func handlePiCatalog(args []string, stdout io.Writer) (bool, error) {
	if len(args) == 0 {
		return false, nil
	}
	asJSON := false
	switch args[0] {
	case "--list-models", "-list-models":
	case "models":
		// 后面跟着位置参数时按 prompt 处理，不当子命令。
		if len(args) > 1 && !strings.HasPrefix(args[1], "-") {
			return false, nil
		}
		asJSON = true
	default:
		return false, nil
	}
	for _, arg := range args[1:] {
		if arg == "--json" {
			asJSON = true
		}
	}
	rows := piCatalogRows(piCatalogEndpoints(piCatalogProviderArg(args)))
	if asJSON {
		return true, writePiCatalogJSON(stdout, rows)
	}
	return true, writePiCatalogTable(stdout, rows)
}

// piCatalogProviderArg 取 --provider 的值（没给时返回空，表示自动选）。
func piCatalogProviderArg(args []string) string {
	for i := 1; i < len(args); i++ {
		switch arg := args[i]; {
		case arg == "--provider" || arg == "-provider":
			if i+1 < len(args) {
				return args[i+1]
			}
		case strings.HasPrefix(arg, "--provider="):
			return strings.TrimPrefix(arg, "--provider=")
		}
	}
	return ""
}

// piCatalogEndpoints 决定目录里列哪些端点：显式指定的优先；否则「配置里的
// provider + 有凭据的其他端点」。一个 key 都没有时退回配置里的那个，至少让
// 下拉框有东西可选，而不是空着。
func piCatalogEndpoints(only string) []provider.Endpoint {
	if only != "" {
		if ep, ok := provider.LookupEndpoint(only); ok {
			return []provider.Endpoint{ep}
		}
		// 未知名字不能只回车头就退出：调用方会以为「这台机器一个模型都没有」。
		fmt.Fprintf(os.Stderr, "警告: 未知 provider %q，目录改用自动选择\n", only)
	}
	cfg, err := LoadConfig()
	if err == nil {
		cfg.applyEnv()
	}
	var eps []provider.Endpoint
	seen := map[string]bool{}
	add := func(ep provider.Endpoint) {
		if ep.Name == "" || seen[ep.Name] {
			return
		}
		seen[ep.Name] = true
		eps = append(eps, ep)
	}
	if ep, ok := provider.LookupEndpoint(cfg.Provider); ok {
		add(ep)
	}
	for _, ep := range provider.Endpoints {
		// 本地端点（EnvKey 为空）总能跑；远端端点只在 key 存在时列出来，
		// 免得把一个一选就 401 的模型摆到别人面前。
		if ep.EnvKey == "" || os.Getenv(ep.EnvKey) != "" {
			add(ep)
		}
	}
	if len(eps) == 0 {
		if ep, ok := provider.LookupEndpoint("deepseek"); ok {
			add(ep)
		}
	}
	return eps
}

// piCatalogRows 把端点展开成目录行。没写 Models 的端点用 DefaultModel 兜底。
func piCatalogRows(eps []provider.Endpoint) []piCatalogEntry {
	rows := make([]piCatalogEntry, 0, len(eps)*2)
	for _, ep := range eps {
		ids := ep.Models
		if len(ids) == 0 && ep.DefaultModel != "" {
			ids = []string{ep.DefaultModel}
		}
		for _, id := range ids {
			id = strings.TrimSpace(id)
			if id == "" {
				continue
			}
			selector := ep.Name + "/" + id
			info, _ := provider.LookupModel(id)
			rows = append(rows, piCatalogEntry{
				Provider:      ep.Name,
				ID:            id,
				Selector:      selector,
				Label:         selector,
				ContextWindow: info.ContextWindow,
				MaxOutput:     info.MaxOutput,
			})
		}
	}
	return rows
}

// writePiCatalogTable 输出 Pi 的多列表格。解析方只看前两列（provider、model），
// 首列是 "provider" 的行按表头跳过，所以这里保持这个列序。
func writePiCatalogTable(w io.Writer, rows []piCatalogEntry) error {
	bw := bufio.NewWriter(w)
	fmt.Fprintln(bw, "provider\tmodel\tcontext\tmax-output")
	for _, r := range rows {
		fmt.Fprintf(bw, "%s\t%s\t%d\t%d\n", r.Provider, r.ID, r.ContextWindow, r.MaxOutput)
	}
	return bw.Flush()
}

func writePiCatalogJSON(w io.Writer, rows []piCatalogEntry) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(map[string]any{"models": rows})
}

// splitModelSelector 把 "provider/id" 形式的模型选择器拆成 provider + model。
//
// 目录里的 selector 就是这种形态，编排放回来的 --model 也是。前缀不是已知
// provider 时原样保留（"anthropic/claude-sonnet-4.5" 这类模型 ID 里本来就有斜杠）。
func splitModelSelector(selector string) (providerName, modelID string, ok bool) {
	selector = strings.TrimSpace(selector)
	idx := strings.Index(selector, "/")
	if idx <= 0 || idx == len(selector)-1 {
		return "", "", false
	}
	if _, known := provider.LookupEndpoint(selector[:idx]); !known {
		return "", "", false
	}
	return selector[:idx], selector[idx+1:], true
}

// ── --extension：Multica 通过它下发受管 MCP 配置 ──

// piExtensionMCPServers 从 Pi 扩展文件里取出受管 MCP server。
//
// Multica 生成的 mcp-extension.mjs 形如：
//
//	const servers = JSON.parse("{\"mcpServers\":{…}}");
//	export default function (pi) { … pi.registerMcpServer(name, config) … }
//
// 我们不解 JS：那句 JSON.parse 后面的字符串字面量就是它自己 json.Marshal 出来
// 的配置，取出来反序列化即可。脚本里可能有多处 JSON.parse（有些扩展先解析
// 别的常量），所以逐个位置试，取第一个能解析出 mcpServers 的。
func piExtensionMCPServers(path string) (map[string]json.RawMessage, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	const marker = "JSON.parse("
	for off := 0; ; {
		idx := bytes.Index(data[off:], []byte(marker))
		if idx < 0 {
			return nil, nil
		}
		start := off + idx + len(marker)
		servers, next := piExtensionServersAt(data, start)
		if servers != nil {
			return servers, nil
		}
		off = start
		if next > off {
			off = next
		}
	}
}

// piExtensionServersAt 尝试把 data[i:] 开头的 "…" 字面量解析成 mcpServers。
// 第二个返回值是这个字面量之后的位置，供调用方继续往后找下一个 JSON.parse。
func piExtensionServersAt(data []byte, i int) (map[string]json.RawMessage, int) {
	rest := bytes.TrimLeft(data[i:], " \t\r\n")
	if len(rest) == 0 || rest[0] != '"' {
		return nil, i
	}
	for j := 1; j < len(rest); j++ {
		if rest[j] == '\\' {
			j++
			continue
		}
		if rest[j] != '"' {
			continue
		}
		var inner string
		if err := json.Unmarshal(rest[:j+1], &inner); err != nil {
			return nil, i // 这一段不是合法字面量：跳到下一个 JSON.parse 再试
		}
		var doc struct {
			MCPServers map[string]json.RawMessage `json:"mcpServers"`
		}
		if err := json.Unmarshal([]byte(inner), &doc); err != nil || len(doc.MCPServers) == 0 {
			return nil, i
		}
		return doc.MCPServers, i
	}
	return nil, i
}

// mergePiExtensions 把若干 Pi 扩展里的 MCP server 叠加进配置。
// 叠加而不是覆盖：--mcp-json / .mcp.json 里已有的 server 保留，同名以扩展为准
// （扩展是本次运行的调用方下发的，优先级更高）。
func mergePiExtensions(cfg *Config, paths []string) {
	if len(paths) == 0 || cfg.NoMCP {
		return
	}
	merged := map[string]json.RawMessage{}
	for _, path := range paths {
		servers, err := piExtensionMCPServers(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "警告: 读取 Pi 扩展 %s 的 MCP 配置失败: %v\n", path, err)
			continue
		}
		if len(servers) == 0 {
			// 调用方明确给了 --extension 却什么都没读到，不能静默。
			fmt.Fprintf(os.Stderr, "警告: Pi 扩展 %s 里没找到 mcpServers，本次不注入受管 MCP\n", path)
			continue
		}
		for name, raw := range servers {
			// 逐个隔离：一个写错的 server 不能把整轮运行拖下水（本地配置仍然严格）。
			var sc mcp.ServerConfig
			if err := json.Unmarshal(raw, &sc); err != nil {
				fmt.Fprintf(os.Stderr, "警告: MCP server %q 的配置无法解析，已跳过: %v\n", name, err)
				continue
			}
			if err := sc.Validate(name); err != nil {
				fmt.Fprintf(os.Stderr, "警告: MCP server %q 不可用，已跳过: %v\n", name, err)
				continue
			}
			merged[name] = raw
		}
	}
	if len(merged) == 0 {
		return
	}
	// 已有内联配置可能是两种形状：完整 {"mcpServers":{…}}，或省略包装的
	// {"<server 名>":{…}}（mcp.LoadJSON 两种都收）。后者如果只当 "mcpServers"
	// 读，会连原有的 server 一起丢掉。
	existing := map[string]json.RawMessage{}
	if strings.TrimSpace(cfg.MCPJSON) != "" {
		var doc map[string]json.RawMessage
		if err := json.Unmarshal([]byte(cfg.MCPJSON), &doc); err != nil {
			fmt.Fprintf(os.Stderr, "警告: 已有的 MCP 内联配置无法解析，本轮只用扩展里的配置: %v\n", err)
		} else if raw, ok := doc["mcpServers"]; ok && len(bytes.TrimSpace(raw)) > 0 {
			_ = json.Unmarshal(raw, &existing)
		} else {
			existing = doc
		}
	}
	if existing == nil {
		existing = map[string]json.RawMessage{}
	}
	for name, raw := range merged {
		existing[name] = raw
	}
	wrapped, err := json.Marshal(existing)
	if err != nil {
		return
	}
	out, err := json.Marshal(map[string]json.RawMessage{"mcpServers": wrapped})
	if err != nil {
		return
	}
	cfg.MCPJSON = string(out)
}

// ── JSON 事件流 ──

// piEventSink 串行化事件输出。agent 的 handler 可能来自工具并发协程，JSON 行
// 必须整行写出，不能交错。
//
// 写失败（磁盘满、管道坏）必须能被上层看到：静默吞掉会得到「退出码 0 +
// 半截事件流」，调用方会当成功。管道被对面关掉（EPIPE）不算错，那属于
// 「调用方不想要了」，但其余错误记下第一个，`run()` 收尾时改成非 0 退出。
type piEventSink struct {
	mu  sync.Mutex
	enc *json.Encoder
	err error
}

func newPiEventSink(w io.Writer) *piEventSink {
	enc := json.NewEncoder(w)
	// 别把中文和引号转成 \u003c 之类：事件流是给人看执行记录用的。
	enc.SetEscapeHTML(false)
	return &piEventSink{enc: enc}
}

// errBrokenPipe 判定「对面不读了」类错误，它们不应当作失败上报。
// Windows 的错误码在 extraBrokenPipe 里（需要 build tag）。
func errBrokenPipe(err error) bool {
	return errors.Is(err, syscall.EPIPE) || errors.Is(err, io.ErrClosedPipe) ||
		errors.Is(err, os.ErrClosed) || extraBrokenPipe(err)
}

func (s *piEventSink) emit(ev any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return // 已经写不出去了，别再堆积半截行
	}
	if err := s.enc.Encode(ev); err != nil && !errBrokenPipe(err) {
		s.err = err
	}
}

// Err 返回首个非管道类写错误（没有则 nil）。
func (s *piEventSink) Err() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// piAssistantDelta 对应 Pi 的 assistantMessageEvent。
type piAssistantDelta struct {
	Type  string `json:"type"`
	Delta string `json:"delta,omitempty"`
}

// piUsageJSON 与 Pi 的 usage 字段同名同义。
type piUsageJSON struct {
	Input       int `json:"input"`
	Output      int `json:"output"`
	CacheRead   int `json:"cacheRead"`
	CacheWrite  int `json:"cacheWrite"`
	TotalTokens int `json:"totalTokens"`
}

// piMessageJSON 是 Pi 消息里我们关心的字段。agent_end 只被用来判断最后一次
// assistant 回复是不是 error，所以其余内容不必逐字对齐。
type piMessageJSON struct {
	Role         string       `json:"role"`
	Model        string       `json:"model,omitempty"`
	Usage        *piUsageJSON `json:"usage,omitempty"`
	StopReason   string       `json:"stopReason,omitempty"`
	ErrorMessage string       `json:"errorMessage,omitempty"`
}

// piMessageOf 把内部消息翻成事件流里的消息。
func piMessageOf(m agent.Message) piMessageJSON {
	out := piMessageJSON{Role: string(m.Role())}
	am, ok := m.(*agent.AssistantMessage)
	if !ok {
		return out
	}
	out.Model = am.Model
	out.StopReason = string(am.StopReason)
	out.ErrorMessage = am.ErrorMessage
	out.Usage = &piUsageJSON{
		Input:       am.Usage.Input,
		Output:      am.Usage.Output,
		CacheRead:   am.Usage.CacheRead,
		CacheWrite:  am.Usage.CacheWrite,
		TotalTokens: am.Usage.TotalTokens,
	}
	return out
}

// piEventHandler 把 agent 事件翻成 Pi 的 JSONL 事件流。logw 收非致命告警（可为 nil）。
//
// 关键几条（调用方就是按这些判断成败的）：
//   - 纯文本只通过 message_update/text_delta 输出，调用方拼接它就是最终结果；
//   - tool_execution_start/end 成对，带 toolCallId；
//   - turn_end 带这一轮的 usage 与 stopReason；
//   - agent_end 带本次运行的全部消息，最后一条 assistant 的 stopReason=error
//     表示 provider 失败。
func piEventHandler(sink *piEventSink, logw io.Writer) agent.EventHandler {
	if sink == nil {
		sink = newPiEventSink(io.Discard)
	}
	return func(ev agent.Event) {
		switch e := ev.(type) {
		case agent.AgentStartEvent:
			sink.emit(map[string]any{"type": "agent_start"})

		case agent.TurnStartEvent:
			sink.emit(map[string]any{"type": "turn_start", "turn": e.Turn})

		case agent.MessageUpdateEvent:
			kind := ""
			switch e.Kind {
			case "text":
				kind = "text_delta"
			case "thinking":
				kind = "thinking_delta"
			}
			if kind == "" || e.Delta == "" {
				return
			}
			sink.emit(map[string]any{
				"type":                  "message_update",
				"assistantMessageEvent": piAssistantDelta{Type: kind, Delta: e.Delta},
			})

		case agent.ToolExecutionStartEvent:
			sink.emit(map[string]any{
				"type":       "tool_execution_start",
				"toolCallId": e.Call.ID,
				"toolName":   e.Call.Name,
				"args":       e.Call.Arguments,
			})

		case agent.ToolExecutionEndEvent:
			result := ""
			isError := false
			if e.Result != nil {
				result = blocksToText(e.Result.Content)
				isError = e.Result.IsError
			}
			sink.emit(map[string]any{
				"type":       "tool_execution_end",
				"toolCallId": e.Call.ID,
				"toolName":   e.Call.Name,
				"result":     result,
				"isError":    isError,
			})

		case agent.TurnEndEvent:
			msg := piMessageOf(e.Message)
			if msg.Role == "" {
				msg.Role = string(agent.RoleAssistant)
			}
			sink.emit(map[string]any{"type": "turn_end", "message": msg})

		case agent.MessageEndEvent:
			// provider 失败时 agent 只发 message_end（StopError）不发 turn_end，
			// 直接 break 到 agent_end。补一条 turn_end，否则「一轮的结束」永远
			// 不会到达。只补 error：正常轮次已有 TurnEndEvent，再补会重复计 usage。
			am, ok := e.Message.(*agent.AssistantMessage)
			if !ok || am.StopReason != agent.StopError {
				return
			}
			sink.emit(map[string]any{"type": "turn_end", "message": piMessageOf(am)})

		case agent.ErrorEvent:
			// 非致命（例如自动压缩失败）：不能发 "error" —— 调用方会把整个 run
			// 判成失败，而它其实还会继续。只能进 stderr 留个痕迹。
			if e.Err != nil && logw != nil {
				fmt.Fprintf(logw, "警告: %v\n", e.Err)
			}

		case agent.AgentEndEvent:
			// 先发 error 再发 agent_end：后者是「run 收尾」，调用方看到它就会把结果定稿，
			// 所以失败原因必须在那之前到达。
			if e.Err != nil && !errors.Is(e.Err, context.Canceled) {
				sink.emit(map[string]any{"type": "error", "message": e.Err.Error()})
			}
			msgs := make([]piMessageJSON, 0, len(e.Messages))
			for _, m := range e.Messages {
				if m != nil {
					msgs = append(msgs, piMessageOf(m))
				}
			}
			sink.emit(map[string]any{"type": "agent_end", "messages": msgs})
		}
	}
}
