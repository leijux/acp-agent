package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/MoeForge/moejs"
)

// JSTool 在内置 JavaScript 引擎（moejs）里执行脚本。
//
// 用途：计算、JSON 变换、正则处理、日期换算这类「不值得起一个进程」的活；
// 另外提供**只读**的 fs 桥（readFile/readDir/stat/exists/glob），
// 让「扫一批文件再统计」不用先把内容读进 args；以及同步的 fetch（见 js_http.go）。
//
// 沙箱约束：没有 require、没有文件写入、没有 import；只有 JS 标准库 + console
// + args + 只读 fs + crypto + fetch（fetch 的次数/字节/超时都受预算约束，
// 只读模式只放行 GET/HEAD）。
// 并发安全：每次 Execute 新建一个 runtime，互不共享状态。
type JSTool struct {
	// Policy 限定 fs 能读的范围（零值时按 cwd）。
	Policy Policy
}

// NewJSTool 创建 js 工具（fs 以当前工作目录为根）。
func NewJSTool() *JSTool { return &JSTool{Policy: DefaultPolicy()} }

// NewJSToolWithPolicy 创建受指定 Policy 约束的 js 工具。
func NewJSToolWithPolicy(p Policy) *JSTool { return &JSTool{Policy: p} }

func (*JSTool) Name() string { return "js" }

func (*JSTool) Description() string {
	return "JavaScript 执行引擎，" +
		"适合计算、JSON 变换、字符串/正则处理、日期换算。" +
		"可用 console.log/info/warn/error 输出（会被收集）；脚本最后一个表达式的值作为结果返回。" +
		"只读文件系统可用：fs.readFile(path)、fs.readDir(path)、fs.stat(path)、fs.exists(path)、" +
		"fs.glob(pattern)（pattern 相对 fs.root）；permissions=auto 时可用 fs.writeFile(path, content) 落盘，" +
		"其余模式写盘请用 write/edit（那条路有确认与 diff 审计）。" +
		"网络请求用同步的 fetch(url, {method, headers, body|json, timeout, maxBytes})，" +
		"返回 {status, ok, url, headers, text, json, size, truncated}（不是 Promise，不能 await；" +
		"非 2xx 不抛错，看 ok/status）。非 GET/HEAD 请求需要 permissions=auto；" +
		"没有 require；仅支持同步代码；超时会被中断；单脚本 JS 堆上限 64MB（超限也会被中断，要拆小任务）。"
}

func (*JSTool) Parameters() map[string]any {
	return Schema(map[string]any{
		"script":  Prop("string", "要执行的 JavaScript 代码（同步）"),
		"args":    map[string]any{"type": "object", "description": "注入为全局变量 args 的对象（JSON）", "additionalProperties": true},
		"timeout": PropInt("最长执行毫秒数，默认 5000，上限 60000", 5000),
	}, "script")
}

// ConcurrencySafe：纯计算、无共享状态，可并行。
func (*JSTool) ConcurrencySafe() bool { return true }

// Mutates：能读文件，但不写文件、不碰进程，调度上仍可并发。
func (*JSTool) Mutates() bool { return false }

const (
	jsDefaultTimeout = 5000
	jsMaxTimeout     = 60000

	// jsLogCap 限制 console 累计输出：脚本里一个 for 循环打印十万行，
	// 之前的实现会把它们全攒在内存里再截断（真正超时的脚本能吃掉几百 MB）。
	jsLogCap = 1 << 20

	// jsMemoryLimit 是单个脚本能留住的 JS 堆上限（字节）。
	//
	// 换引擎前完全没有内存约束：`const a=[];for(;;)a.push(new Array(1<<16).fill(0))`
	// 会把整个进程吃到内核 OOM kill —— 连一条 error 都拿不到（工具层的 recover
	// 救不了 OOM）。moejs 的 MemoryLimit 在分配越过阈值时先测量「真正留住的量」
	// 再决定是否打断，所以「制造大量垃圾但只留一点」的脚本（逐行扫文件、反复
	// 拼临时串）不受影响。
	//
	// 64MB 的取法：工具自身的输入预算（fs 16MB、fetch 8MB、console 1MB）都远小于它，
	// 正常计算留足余量；同时能挡住 `new Array(1e9)` 这类一口气吃掉几个 GB 的写法。
	jsMemoryLimit = 64 << 20
)

func (t *JSTool) Execute(ctx context.Context, a Args) (Result, error) {
	script, err := a.Required("script")
	if err != nil {
		return ErrorResult("%v", err), nil
	}
	timeout := a.Int("timeout", jsDefaultTimeout)
	if timeout <= 0 {
		timeout = jsDefaultTimeout
	}
	if timeout > jsMaxTimeout {
		timeout = jsMaxTimeout
	}

	rt := moejs.NewRuntime(moejs.Options{MemoryLimit: jsMemoryLimit})
	var logs strings.Builder
	sink := &consoleSink{out: &logs, max: jsLogCap}
	if err := installConsole(rt, sink); err != nil {
		return ErrorResult("初始化 console 失败: %v", err), nil
	}
	if err := installFS(rt, t.Policy); err != nil {
		return ErrorResult("初始化 fs 失败: %v", err), nil
	}
	if err := installCrypto(rt); err != nil {
		return ErrorResult("初始化 crypto 失败: %v", err), nil
	}
	deadline := time.Duration(timeout) * time.Millisecond
	// fetch 需要知道脚本的截止时间：Interrupt 打不断阻塞在 socket 上的 Go
	// 调用，单请求超时只能由 http 层按「剩余时间」兜住（见 js_http.go）。
	scriptCtx, cancelScript := context.WithTimeout(ctx, deadline)
	defer cancelScript()
	if err := installFetch(rt, t.Policy, &jsHTTP{policy: t.Policy, ctx: scriptCtx}); err != nil {
		return ErrorResult("初始化 fetch 失败: %v", err), nil
	}
	// args 必须先变成真正的 JS 值：moejs 的 FromGo 给的是**惰性**的 Go 容器
	// 视图（JS 写入只改 JS 对象，Go 侧不动），`args.a.push(x)` 这类原地修改
	// 会在后续读取时对不上。走一趟 JSON.parse 之后就是普通的 JS 对象/数组。
	if raw, ok := a["args"].(map[string]any); ok && len(raw) > 0 {
		if encoded, err := json.Marshal(raw); err == nil {
			if v, err := rt.ParseJSONString(string(encoded)); err == nil {
				_ = rt.SetGlobal("args", v)
			}
		}
	}

	// 超时与取消都通过 Interrupt 打断解释器（moejs 支持跨 goroutine 调用）。
	timer := time.AfterFunc(deadline, func() { rt.Interrupt("timeout") })
	defer timer.Stop()
	stopWatch := make(chan struct{})
	defer close(stopWatch)
	go func() {
		select {
		case <-ctx.Done():
			rt.Interrupt("canceled")
		case <-stopWatch:
		}
	}()

	start := time.Now()
	value, runErr := runJSScript(rt, "js", script)
	elapsed := time.Since(start).Round(time.Millisecond)

	out := strings.TrimRight(logs.String(), "\n")
	summary := ""
	if runErr != nil {
		summary = jsErrorText(runErr, timeout)
		if out != "" {
			return ErrorResult("%s\n\n%s", out, summary), nil
		}
		return ErrorResult("%s", summary), nil
	}
	// 真正的 undefined/null 不打印（脚本返回字符串 "undefined" 时要照常打印）。
	if !value.IsNullish() {
		if res := jsValueString(rt, value); res != "" {
			if out != "" {
				out += "\n"
			}
			out += "=> " + res
		}
	}
	if out == "" {
		out = "(无输出)"
	}
	out += fmt.Sprintf("\n[%s]", elapsed)
	text, truncated := Truncate(out, 200, 16*1024)
	if truncated {
		text += Note(truncated, "把计算拆成多步，或先 console.log 汇总再返回")
	}
	return TextResult("%s", text), nil
}

// runJSScript 编译并执行一段经典脚本，返回它的完成值。
//
// 每次都重新编译：脚本来自模型、一次性使用，没有缓存价值；编译失败是
// *moejs.SyntaxError，和运行期错误走同一套错误文案。
func runJSScript(rt *moejs.Runtime, name, src string) (moejs.Value, error) {
	s, err := moejs.CompileScript(name, src)
	if err != nil {
		return moejs.Undefined(), err
	}
	return rt.RunScript(s)
}

// consoleSink 决定 console.* 各级别写到哪，并限制累计输出量。
//
// js 工具把所有级别并进同一块缓冲（模型看到一份完整日志）；shell 里的 goja
// 则把 warn/error 分到 stderr，符合命令行直觉。
type consoleSink struct {
	out    io.Writer
	errOut io.Writer // nil 表示并入 out
	max    int       // <=0 表示不限
	n      int
	cut    bool
}

// write 输出一行；超过 max 后只提示一次，之后丢弃。
func (s *consoleSink) write(prefix, line string, toErr bool) {
	w := s.out
	if toErr && s.errOut != nil {
		w = s.errOut
	}
	if s.max > 0 && s.n >= s.max {
		if !s.cut {
			s.cut = true
			fmt.Fprintf(s.out, "[console 输出超过 %d 字节，后续已丢弃]\n", s.max)
		}
		return
	}
	s.n += len(prefix) + len(line)
	fmt.Fprint(w, prefix+line)
}

// installConsole 注入 console.*。
func installConsole(rt *moejs.Runtime, sink *consoleSink) error {
	log := func(prefix string, toErr bool) moejs.NativeFunc {
		return func(_ *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
			parts := make([]string, 0, len(args))
			for _, arg := range args {
				parts = append(parts, jsValueString(rt, arg))
			}
			sink.write(prefix, strings.Join(parts, " ")+"\n", toErr)
			return moejs.Undefined(), nil
		}
	}
	return rt.SetGlobal("console", map[string]any{
		"log":   log("", false),
		"info":  log("", false),
		"debug": log("[debug] ", false),
		"warn":  log("[warn] ", true),
		"error": log("[error] ", true),
		"table": log("", false),
	})
}

// jsValueString 把引擎值转成稳定可读的字符串。
//
// 顺序有讲究：字符串原样（不套引号），其余先试 JSON.stringify（对象/数组/
// 数字/布尔都得到模型习惯的形态），失败（循环引用、BigInt、函数）才退到
// 有界渲染 —— 见 safeString 的注释，这里绝不能直接 fmt.Sprint。
func jsValueString(rt *moejs.Runtime, v moejs.Value) string {
	if v.IsNullish() {
		return "undefined"
	}
	if v.IsString() {
		return v.String()
	}
	if out, err := rt.AppendJSON(nil, v); err == nil && len(out) > 0 && string(out) != "undefined" {
		return string(out)
	}
	if x, err := rt.ToGo(v); err == nil {
		return safeString(x)
	}
	return v.String()
}

const (
	safeMaxDepth = 4
	safeMaxItems = 50
)

// safeString 把任意 Go 值渲染成有界字符串：限制深度/条目数，并检测循环引用。
//
// 注意：**绝不能用 fmt.Sprint 件任何可能自引用的值** —— Go 运行时的栈溢出
// 在 goroutine 里是 fatal error，连 recover 都拦不住，整个进程直接消亡。
func safeString(v any) string {
	var b strings.Builder
	safeWrite(&b, reflect.ValueOf(v), 0, make(map[uintptr]bool))
	return b.String()
}

func safeWrite(b *strings.Builder, rv reflect.Value, depth int, seen map[uintptr]bool) {
	if !rv.IsValid() {
		b.WriteString("null")
		return
	}
	if depth > safeMaxDepth {
		b.WriteString("…")
		return
	}

	switch rv.Kind() {
	case reflect.Interface, reflect.Pointer:
		if rv.IsNil() {
			b.WriteString("null")
			return
		}
		if rv.Kind() == reflect.Pointer {
			if seen[rv.Pointer()] {
				b.WriteString("[循环]")
				return
			}
			seen[rv.Pointer()] = true
			defer delete(seen, rv.Pointer())
		}
		safeWrite(b, rv.Elem(), depth, seen)

	case reflect.Map:
		if rv.IsNil() {
			b.WriteString("null")
			return
		}
		ptr := rv.Pointer()
		if seen[ptr] {
			b.WriteString("[循环]")
			return
		}
		seen[ptr] = true
		defer delete(seen, ptr)
		b.WriteString("{")
		iter := rv.MapRange()
		n := 0
		for iter.Next() {
			if n >= safeMaxItems {
				b.WriteString(", …")
				break
			}
			if n > 0 {
				b.WriteString(", ")
			}
			safeWrite(b, iter.Key(), depth+1, seen)
			b.WriteString(": ")
			safeWrite(b, iter.Value(), depth+1, seen)
			n++
		}
		b.WriteString("}")

	case reflect.Slice, reflect.Array:
		if rv.Kind() == reflect.Slice {
			if rv.IsNil() {
				b.WriteString("null")
				return
			}
			ptr := rv.Pointer()
			if seen[ptr] {
				b.WriteString("[循环]")
				return
			}
			seen[ptr] = true
			defer delete(seen, ptr)
		}
		b.WriteString("[")
		for i := 0; i < rv.Len(); i++ {
			if i >= safeMaxItems {
				b.WriteString(", …")
				break
			}
			if i > 0 {
				b.WriteString(", ")
			}
			safeWrite(b, rv.Index(i), depth+1, seen)
		}
		b.WriteString("]")

	case reflect.String:
		b.WriteString(strconv.Quote(rv.String()))
	case reflect.Bool:
		b.WriteString(strconv.FormatBool(rv.Bool()))
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		b.WriteString(strconv.FormatInt(rv.Int(), 10))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		b.WriteString(strconv.FormatUint(rv.Uint(), 10))
	case reflect.Float32, reflect.Float64:
		b.WriteString(strconv.FormatFloat(rv.Float(), 'f', -1, 64))
	case reflect.Func:
		b.WriteString("[function]")
	default:
		// ToGo 只会产出 map/slice/基础类型；其余类型只报类型名，
		// 不做递归 —— 递归就是栈溢出的来源。
		b.WriteString("<")
		b.WriteString(rv.Type().String())
		b.WriteString(">")
	}
}

// jsErrorText 把脚本错误整理成模型能看懂的一段文本。
func jsErrorText(err error, timeoutMs int64) string {
	// 内存超限在引擎里也是一次 interrupt，必须先区分开：两者的处置完全不同
	// （超时要查死循环，超限要拆小任务）。
	var mle *moejs.MemoryLimitError
	if errors.As(err, &mle) {
		return fmt.Sprintf(
			"脚本内存超限（上限 %dMB，本次已占用 %.1fMB）：把任务拆成多步，或者先在宿主侧过滤/截断数据再传进 args",
			mle.Limit>>20, float64(mle.Live+mle.Allocated)/(1<<20))
	}
	var ie *moejs.InterruptedError
	if errors.As(err, &ie) {
		if reason, ok := ie.Value.(string); ok && reason == "canceled" {
			return "脚本执行被取消"
		}
		return fmt.Sprintf("脚本被中断（超时 %dms 或请求已取消）：检查是否有死循环", timeoutMs)
	}
	var ex *moejs.Exception
	if errors.As(err, &ex) {
		return "JavaScript 错误: " + ex.String()
	}
	return "JavaScript 错误: " + err.Error()
}
