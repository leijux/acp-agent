package tools

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// netWritePolicy 是允许写网络的策略（等价于 permissions=auto）。
func netWritePolicy(t *testing.T) Policy {
	t.Helper()
	return Policy{Root: t.TempDir(), AllowNetworkWrite: true}
}

// fetch 的成功路径：状态码、ok、header（小写）、contentType、text、json。
func TestJSTool_FetchGetJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("X-Trace", "abc123")
		_, _ = w.Write([]byte(`{"name":"ok","items":[1,2,3]}`))
	}))
	defer srv.Close()

	r := runJS(t, Args{
		"script": `const r = fetch(args.url);
[r.status, r.ok, r.headers["x-trace"], r.contentType, r.json.name, r.json.items.length, r.size > 0].join("|")`,
		"args": map[string]any{"url": srv.URL},
	})
	want := "200|true|abc123|application/json; charset=utf-8|ok|3|true"
	if got := r.Text(); !strings.Contains(got, want) {
		t.Fatalf("结果 = %q, 期望包含 %q", got, want)
	}
}

// POST + json 选项：body 被序列化、Content-Type 自动补上。
// 写网络需要 Policy.AllowNetworkWrite（只有 permissions=auto 打开）。
func TestJSTool_FetchPostJSON(t *testing.T) {
	type seen struct {
		method, ct, body string
	}
	got := make(chan seen, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 512)
		n, _ := r.Body.Read(buf)
		got <- seen{method: r.Method, ct: r.Header.Get("Content-Type"), body: string(buf[:n])}
		_, _ = w.Write([]byte(`{"echo":true}`))
	}))
	defer srv.Close()

	r := runJSWithPolicy(t, netWritePolicy(t), Args{
		"script": `const r = fetch(args.url, {method: "post", json: {a: 1, b: [2]}}); r.status + "|" + r.json.echo`,
		"args":   map[string]any{"url": srv.URL},
	})
	if txt := r.Text(); !strings.Contains(txt, "200|true") {
		t.Fatalf("结果 = %q", txt)
	}
	select {
	case s := <-got:
		if s.method != http.MethodPost {
			t.Errorf("method = %q, 期望 POST（应被归一成大写）", s.method)
		}
		if s.ct != "application/json" {
			t.Errorf("Content-Type = %q, 期望 application/json（json 选项应自动补）", s.ct)
		}
		if s.body != `{"a":1,"b":[2]}` {
			t.Errorf("body = %q", s.body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("服务端没有收到请求")
	}
}

// 非 2xx 不抛错：脚本自己判 ok/status。这是刻意的，写进注释与文档了。
func TestJSTool_FetchNon2xxIsNotAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("nope"))
	}))
	defer srv.Close()

	r := runJS(t, Args{
		"script": `const r = fetch(args.url); typeof r.json + "|" + r.status + "|" + r.ok + "|" + r.text`,
		"args":   map[string]any{"url": srv.URL},
	})
	if r.IsError {
		t.Fatalf("404 不应算工具错误: %q", r.Text())
	}
	if got := r.Text(); !strings.Contains(got, `undefined|404|false|nope`) {
		t.Fatalf("结果 = %q", got)
	}
}

// 只读模式：GET 放行（与 web_fetch 一致），写方法拒绝。
func TestJSTool_FetchReadOnlyPolicy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()
	p := Policy{Root: t.TempDir(), ReadOnly: true, AllowNetworkWrite: true}

	ok := runJSWithPolicy(t, p, Args{
		"script": `fetch(args.url).status`,
		"args":   map[string]any{"url": srv.URL},
	})
	if ok.IsError {
		t.Fatalf("只读模式下 GET 应放行: %q", ok.Text())
	}

	bad := runJSWithPolicy(t, p, Args{
		"script": `try { fetch(args.url, {method: "POST", body: "x"}); "NO-ERROR" } catch (e) { String(e) }`,
		"args":   map[string]any{"url": srv.URL},
	})
	txt := bad.Text()
	if strings.Contains(txt, "NO-ERROR") {
		t.Fatalf("只读模式应拒绝 POST: %q", txt)
	}
	if !strings.Contains(txt, "只读模式") {
		t.Fatalf("错误文案缺少可操作提示: %q", txt)
	}
}

// ask 模式（未开 AllowNetworkWrite）：写方法必须拒，且把模型引到 bash 那条会确认的路。
// js 的 Mutates() 是 false，放行就等于静默绕过确认。
func TestJSTool_FetchWriteNeedsAutoMode(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	r := runJS(t, Args{
		"script": `try { fetch(args.url, {method: "PUT", body: "x"}); "NO-ERROR" } catch (e) { String(e) }`,
		"args":   map[string]any{"url": srv.URL},
	})
	txt := r.Text()
	if strings.Contains(txt, "NO-ERROR") {
		t.Fatalf("未开 AllowNetworkWrite 时应拒绝 PUT: %q", txt)
	}
	if !strings.Contains(txt, "bash") || !strings.Contains(txt, "auto") {
		t.Fatalf("错误文案应指出 bash / auto: %q", txt)
	}
	if hits != 0 {
		t.Fatalf("被拒的请求不该真的发出，实际 %d 次", hits)
	}
}

// maxBytes：响应体被截断，并如实标记 truncated。
func TestJSTool_FetchMaxBytesTruncates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", 4096)))
	}))
	defer srv.Close()

	r := runJS(t, Args{
		"script": `const r = fetch(args.url, {maxBytes: 100}); r.size + "|" + r.truncated`,
		"args":   map[string]any{"url": srv.URL},
	})
	if got := r.Text(); !strings.Contains(got, "100|true") {
		t.Fatalf("结果 = %q, 期望 100|true", got)
	}
}

// 请求次数预算：循环调用不能变成无限出网。
func TestJSTool_FetchRequestBudget(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	r := runJS(t, Args{
		"script": `let i = 0, err = "";
for (; i < 40; i++) { try { fetch(args.url); } catch (e) { err = String(e); break; } }
i + "|" + err`,
		"args": map[string]any{"url": srv.URL},
	})
	txt := r.Text()
	if !strings.Contains(txt, "最多") {
		t.Fatalf("超出请求预算应报错: %q", txt)
	}
	if hits > jsHTTPMaxRequests {
		t.Fatalf("实际发出 %d 次请求，超过上限 %d", hits, jsHTTPMaxRequests)
	}
}

// URL 只收 http/https；ftp/file 之类在出网前就被拒。
func TestJSTool_FetchRejectsNonHTTPURL(t *testing.T) {
	r := runJS(t, Args{"script": `try { fetch("file:///etc/passwd"); "NO-ERROR" } catch (e) { String(e) }`})
	if txt := r.Text(); strings.Contains(txt, "NO-ERROR") || !strings.Contains(txt, "http/https") {
		t.Fatalf("应拒绝非 http URL: %q", txt)
	}
}

// 单请求超时：慢站点必须在 timeout 附近失败，而不是挂到脚本超时之后。
func TestJSTool_FetchTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()

	start := time.Now()
	r := runJS(t, Args{
		"script":  `try { fetch(args.url, {timeout: 80}); "NO-ERROR" } catch (e) { String(e) }`,
		"args":    map[string]any{"url": srv.URL},
		"timeout": 5000,
	})
	elapsed := time.Since(start)
	txt := r.Text()
	if strings.Contains(txt, "NO-ERROR") {
		t.Fatalf("应该超时: %q", txt)
	}
	if !strings.Contains(txt, "没有完成") {
		t.Fatalf("超时文案不够明确: %q", txt)
	}
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("耗时 %v，单请求 timeout 没有生效", elapsed)
	}
}

// 连不上时必须抛 JS 异常（可 try/catch），而不是返回半个对象。
func TestJSTool_FetchConnectError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // 立刻关掉，连接必然失败

	r := runJS(t, Args{
		"script": `try { fetch(args.url); "NO-ERROR" } catch (e) { "caught:" + String(e).length }`,
		"args":   map[string]any{"url": url},
	})
	if txt := r.Text(); strings.Contains(txt, "NO-ERROR") || !strings.Contains(txt, "caught:") {
		t.Fatalf("连接失败应抛异常: %q", txt)
	}
}

// fetch 是同步的：不是 Promise，也不能被 await（没有事件循环）。
func TestJSTool_FetchIsSynchronous(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"a":1}`))
	}))
	defer srv.Close()

	r := runJS(t, Args{
		"script": `const r = fetch(args.url); typeof r.then + "|" + r.json.a`,
		"args":   map[string]any{"url": srv.URL},
	})
	if got := r.Text(); !strings.Contains(got, "undefined|1") {
		t.Fatalf("fetch 应是同步返回的普通对象: %q", got)
	}
}
