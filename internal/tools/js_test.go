package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func runJS(t *testing.T, args Args) Result {
	t.Helper()
	r, err := NewJSTool().Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute 返回 Go error: %v", err)
	}
	return r
}

func TestJSTool_Arithmetic(t *testing.T) {
	r := runJS(t, Args{"script": "1 + 2 * 3"})
	if got := r.Text(); !strings.HasPrefix(got, "=> 7") {
		t.Fatalf("结果 = %q, 期望以 => 7 开头", got)
	}
}

func TestJSTool_ConsoleLog(t *testing.T) {
	r := runJS(t, Args{"script": `console.log("a", 1); console.warn("w"); console.log({x:1}); "done"`})
	got := r.Text()
	for _, want := range []string{"a 1", "[warn] w", `{"x":1}`, "=> done"} {
		if !strings.Contains(got, want) {
			t.Fatalf("输出 %q 缺少 %q", got, want)
		}
	}
}

func TestJSTool_ArgsInjection(t *testing.T) {
	r := runJS(t, Args{
		"script": `args.items.map(s => s.toUpperCase()).join("-")`,
		"args":   map[string]any{"items": []any{"a", "b"}},
	})
	if got := r.Text(); !strings.Contains(got, "A-B") {
		t.Fatalf("结果 = %q, 期望包含 A-B", got)
	}
}

func TestJSTool_NoFilesystemAccess(t *testing.T) {
	r := runJS(t, Args{"script": `typeof require + "/" + typeof process + "/" + typeof print`})
	if got := r.Text(); !strings.Contains(got, "undefined/undefined/undefined") {
		t.Fatalf("沙箱泄漏了宿主对象: %q", got)
	}
}

func TestJSTool_SyntaxError(t *testing.T) {
	r := runJS(t, Args{"script": "function ("})
	if !r.IsError {
		t.Fatalf("语法错误应返回 IsError 结果, 实际 %q", r.Text())
	}
	if !strings.Contains(r.Text(), "JavaScript 错误") {
		t.Fatalf("错误文本 = %q", r.Text())
	}
}

func TestJSTool_Timeout(t *testing.T) {
	r := runJS(t, Args{"script": "while(true){}", "timeout": 100})
	if !r.IsError {
		t.Fatalf("死循环应超时, 实际 %q", r.Text())
	}
	if !strings.Contains(r.Text(), "超时") && !strings.Contains(r.Text(), "中断") {
		t.Fatalf("错误文本 = %q", r.Text())
	}
}

func TestJSTool_ContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r, err := NewJSTool().Execute(ctx, Args{"script": "while(true){}"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !r.IsError {
		t.Fatalf("已取消的 ctx 应中断脚本, 实际 %q", r.Text())
	}
}

func TestJSTool_MissingScript(t *testing.T) {
	r := runJS(t, Args{})
	if !r.IsError {
		t.Fatal("缺少 script 应返回错误结果")
	}
}

// 每个 runtime 独立：并发执行不能相互污染（变量/输出）。
func TestJSTool_ConcurrentIsolation(t *testing.T) {
	tool := NewJSTool()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			script := `var acc = ` + string(rune('0'+n)) + `; console.log("n", acc); acc * 10`
			r, err := tool.Execute(context.Background(), Args{"script": script})
			if err != nil {
				t.Errorf("Execute: %v", err)
				return
			}
			want := "n " + string(rune('0'+n))
			if !strings.Contains(r.Text(), want) {
				t.Errorf("输出 %q 不含 %q（runtime 被共享了）", r.Text(), want)
			}
		}(i)
	}
	wg.Wait()
}

func TestJSTool_ConcurrencyAndMutation(t *testing.T) {
	tool := NewJSTool()
	if !tool.ConcurrencySafe() {
		t.Error("js 应该并发安全")
	}
	if tool.Mutates() {
		t.Error("js 不应声明为改动型")
	}
}

// TestJSTool_CircularReference 回归测试：脚本返回（或 console.log）自引用对象时
// 不能让进程栈溢出。goja 的 Export 会把自引用对象转成真正的循环 map，
// 旧实现 fallback 到 fmt.Sprint 时无限递归，触发不可 recover 的
// "fatal error: stack overflow"（整个进程死亡，不是普通 panic）。
func TestJSTool_CircularReference(t *testing.T) {
	r := runJS(t, Args{"script": `const a = {name: "x"}; a.self = a; a.list = [a]; console.log(a); a`})
	got := r.Text()
	if !strings.Contains(got, "[循环]") {
		t.Fatalf("输出 %q 应包含循环标记", got)
	}
	if !strings.Contains(got, "x") {
		t.Fatalf("输出 %q 应保留非循环字段", got)
	}
}

// TestJSTool_DeepNesting 验证超深嵌套不会爆栈，输出仍受 Truncate 限制。
func TestJSTool_DeepNesting(t *testing.T) {
	// 5000 层无循环：json.Marshal 能序列化，长度由 Truncate 封顶。
	r := runJS(t, Args{"script": `let n = {v: 1}; for (let i = 0; i < 5000; i++) n = {next: n}; n`})
	got := r.Text()
	if got == "" {
		t.Fatal("输出为空")
	}
	if len(got) > 16*1024+1024 {
		t.Fatalf("输出应被 Truncate 截断，实际 %d 字节", len(got))
	}

	// 20000 层超过 encoding/json 的深度上限，会回退到 safeString：
	// 此时必须靠深度限制封顶，而不能靠 json。
	deep := runJS(t, Args{"script": `let n = {v: 1}; for (let i = 0; i < 20000; i++) n = {next: n}; n`})
	if d := deep.Text(); len(d) > 16*1024+1024 {
		t.Fatalf("超深嵌套输出 %d 字节", len(d))
	}
}

// ---------- 只读 fs 桥 ----------

func runJSWithPolicy(t *testing.T, p Policy, args Args) Result {
	t.Helper()
	r, err := NewJSToolWithPolicy(p).Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute 返回 Go error: %v", err)
	}
	return r
}

func writeTestFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return p
}

func TestJSTool_FSReadFile(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "a.txt", "hello")
	p := Policy{Root: root}
	r := runJSWithPolicy(t, p, Args{"script": `fs.readFile("a.txt").toUpperCase()`})
	if got := r.Text(); !strings.Contains(got, "HELLO") {
		t.Fatalf("结果 = %q，期望包含 HELLO", got)
	}
}

func TestJSTool_FSReadDirAndGlob(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "src/a.go", "package a")
	writeTestFile(t, root, "src/b.go", "package b")
	writeTestFile(t, root, "src/sub/c.go", "package c")
	writeTestFile(t, root, "README.md", "x")
	p := Policy{Root: root}

	r := runJSWithPolicy(t, p, Args{"script": `fs.glob("src/**/*.go").sort().join(",")`})
	if got := r.Text(); !strings.Contains(got, "src/a.go,src/b.go,src/sub/c.go") {
		t.Fatalf("glob 结果 = %q", got)
	}

	r = runJSWithPolicy(t, p, Args{"script": `fs.readDir("src").sort().join(",")`})
	if got := r.Text(); !strings.Contains(got, "a.go,b.go,sub/") {
		t.Fatalf("readDir 结果 = %q（目录应带斜杠）", got)
	}
}

// 扫一批文件做统计，这是加 fs 桥的核心动机。
func TestJSTool_FSScanAggregate(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "x.go", "package x\nfunc A() {}\nfunc B() {}\n")
	writeTestFile(t, root, "y.go", "package y\nfunc C() {}\n")
	p := Policy{Root: root}

	script := `
const files = fs.glob("*.go");
let total = 0;
for (const f of files) {
  total += (fs.readFile(f).match(/^func /gm) || []).length;
}
({ files: files.length, funcs: total })`
	r := runJSWithPolicy(t, p, Args{"script": script})
	if got := r.Text(); !strings.Contains(got, `"funcs":3`) || !strings.Contains(got, `"files":2`) {
		t.Fatalf("聚合结果 = %q", got)
	}
}

func TestJSTool_FSStatAndExists(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "a.txt", "12345")
	p := Policy{Root: root}
	r := runJSWithPolicy(t, p, Args{
		"script": `({ size: fs.stat("a.txt").size, has: fs.exists("a.txt"), no: fs.exists("nope") })`,
	})
	got := r.Text()
	for _, want := range []string{`"size":5`, `"has":true`, `"no":false`} {
		if !strings.Contains(got, want) {
			t.Fatalf("结果 %q 缺少 %q", got, want)
		}
	}
}

// 越界必须被拦：这是 fs 桥的安全底线。
func TestJSTool_FSEscapesRejected(t *testing.T) {
	root := t.TempDir()
	p := Policy{Root: root}
	// 绝对路径的用例要挑一个真实存在的"root 之外"路径：
	// Windows 上 "/etc/passwd" 会被当成相对路径（无盘符），拦不到也不算越界。
	outside := writeTestFile(t, t.TempDir(), "outside.txt", "secret")
	absOutside := strings.ReplaceAll(outside, `\`, `\\`) // 塞进 JS 字符串字面量
	for _, script := range []string{
		`fs.readFile("../outside.txt")`,
		`fs.readFile("` + absOutside + `")`,
		`fs.glob("../*.go")`,
		`fs.readDir("..")`,
		`fs.stat("../outside.txt")`,
	} {
		r := runJSWithPolicy(t, p, Args{"script": script})
		if !r.IsError {
			t.Fatalf("%s 应被拒绝，实际 %q", script, r.Text())
		}
		msg := r.Text()
		if !strings.Contains(msg, "超出允许目录") && !strings.Contains(msg, "不能包含") {
			t.Fatalf("%s 拒绝原因不对: %q", script, msg)
		}
	}
}

// 没有写接口：沙箱不该能改文件。
func TestJSTool_FSWriteNotAvailable(t *testing.T) {
	root := t.TempDir()
	p := Policy{Root: root}
	r := runJSWithPolicy(t, p, Args{
		"script": `[typeof fs.writeFile, typeof fs.appendFile, typeof fs.unlink, typeof fs.mkdir].join(",")`,
	})
	if got := r.Text(); !strings.Contains(got, "undefined,undefined,undefined,undefined") {
		t.Fatalf("沙箱里出现了写接口: %q", got)
	}
}

// 脚本可以 try/catch 住 fs 错误。
func TestJSTool_FSErrorCatchable(t *testing.T) {
	root := t.TempDir()
	p := Policy{Root: root}
	r := runJSWithPolicy(t, p, Args{
		"script": `try { fs.readFile("missing.txt"); "no-throw" } catch (e) { "caught:" + e }`,
	})
	if got := r.Text(); !strings.Contains(got, "caught:") || strings.Contains(got, "no-throw") {
		t.Fatalf("异常没有被 catch: %q", got)
	}
}

// args 必须是原生 JS 值：数组 push 的修改要留在同一个对象里。
func TestJSTool_ArgsMutationPersists(t *testing.T) {
	r := runJS(t, Args{
		"script": `const a = args.list; a.push(9); ({ same: args.list === a, len: a.length, got: args.list.length })`,
		"args":   map[string]any{"list": []any{1, 2}},
	})
	got := r.Text()
	if !strings.Contains(got, `"same":true`) || !strings.Contains(got, `"got":3`) {
		t.Fatalf("args 仍是 Go 容器的包装（写回丢失）: %q", got)
	}
}

// console 输出要有上限，别让死循环式的打印吃掉内存。
func TestJSTool_ConsoleCap(t *testing.T) {
	r := runJS(t, Args{
		"script": `for (let i = 0; i < 200000; i++) console.log("xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"); 0`,
	})
	if !r.IsError && !strings.Contains(r.Text(), "已丢弃") {
		t.Fatalf("console 输出没有上限标记: %.200q", r.Text())
	}
}

// ---------- 二进制读取与哈希 ----------

func TestJSTool_FSReadBytes(t *testing.T) {
	root := t.TempDir()
	// 含 NUL 与非 UTF-8 字节，证明不是按文本读的
	raw := []byte{0x00, 0x01, 0xff, 0x50, 0x4b, 0x03, 0x04}
	if err := os.WriteFile(filepath.Join(root, "a.bin"), raw, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	p := Policy{Root: root}
	r := runJSWithPolicy(t, p, Args{
		"script": `const b = new Uint8Array(fs.readFileBytes("a.bin")); [b.length, b[0], b[2], b[3]].join(",")`,
	})
	if got := r.Text(); !strings.Contains(got, "7,0,255,80") {
		t.Fatalf("字节读取结果 = %q，期望 7,0,255,80", got)
	}
}

func TestJSTool_FSReadBase64(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.bin"), []byte{0x00, 0xff, 0x10}, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	p := Policy{Root: root}
	r := runJSWithPolicy(t, p, Args{"script": `fs.readFileBase64("a.bin")`})
	if got := r.Text(); !strings.Contains(got, "AP8Q") {
		t.Fatalf("base64 = %q，期望 AP8Q", got)
	}
}

func TestJSTool_CryptoSha256(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("abc"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	p := Policy{Root: root}
	// 已知向量：sha256("abc") = ba7816bf...
	const want = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"

	r := runJSWithPolicy(t, p, Args{"script": `crypto.sha256("abc")`})
	if !strings.Contains(r.Text(), want) {
		t.Fatalf("字符串摘要 = %q", r.Text())
	}
	// ArrayBuffer 入参（包含 NUL 的文件也能算）
	r = runJSWithPolicy(t, p, Args{"script": `crypto.sha256(fs.readFileBytes("a.txt"))`})
	if !strings.Contains(r.Text(), want) {
		t.Fatalf("ArrayBuffer 摘要 = %q", r.Text())
	}
	// 非法入参要报错而不是静默算出空串摘要
	r = runJSWithPolicy(t, p, Args{"script": `crypto.sha256({})`})
	if !r.IsError || !strings.Contains(r.Text(), "参数必须是") {
		t.Fatalf("非法入参未报错: %q", r.Text())
	}
}

// 二进制读取同样受 size 上限约束。
func TestJSTool_FSReadBytesSizeLimit(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "big.bin"), make([]byte, 2048), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	p := Policy{Root: root, MaxReadBytes: 1024}
	r := runJSWithPolicy(t, p, Args{"script": `fs.readFileBytes("big.bin")`})
	if !r.IsError || !strings.Contains(r.Text(), "超过上限") {
		t.Fatalf("超限未报错: %q", r.Text())
	}
}
