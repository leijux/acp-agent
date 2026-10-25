package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// newTestClient 构造指向 httptest 的 client。
// base 为空时沿用 provider 的默认 host（测试里用不到网络）。
// newTestClient 构造用于测试的 Client。
//
// 测试都跑在 httptest（127.0.0.1）上，所以默认打开 AllowPrivate；
// 内网过滤本身由 ssrf_test.go 单独验证。显式设了 true 的用例保持不变。
func newTestClient(t *testing.T, cfg Config) *Client {
	t.Helper()
	cfg.Fetch.AllowPrivate = true
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestNewRejectsBadConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{"未知后端", Config{Search: SearchConfig{Provider: "bing"}}, "未知的搜索 provider"},
		{"tavily 缺 key", Config{Search: SearchConfig{Provider: "tavily"}}, "tavily 需要配置 apiKey"},
		{"brave 缺 key", Config{Search: SearchConfig{Provider: "brave"}}, "brave 需要配置 apiKey"},
		{"exa 缺 key", Config{Search: SearchConfig{Provider: "exa"}}, "exa 需要配置 apiKey"},
		{"searxng 缺 baseUrl", Config{Search: SearchConfig{Provider: "searxng"}}, "searxng 需要配置 baseUrl"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v，期望包含 %q", err, tc.want)
			}
		})
	}
}

func TestNewDefaults(t *testing.T) {
	// 空配置 → 无 key 的 DuckDuckGo。
	c := newTestClient(t, Config{})
	if c.Provider() != ProviderDuckDuckGo {
		t.Fatalf("provider = %q", c.Provider())
	}
	if c.fcfg.UserAgent == "" || c.fcfg.MaxBytes == 0 {
		t.Fatalf("fetch 默认值未生效：%+v", c.fcfg)
	}

	// 显式 off。
	c = newTestClient(t, Config{Search: SearchConfig{Provider: ProviderOff}})
	if c.Provider() != ProviderOff {
		t.Fatalf("provider = %q", c.Provider())
	}
	// off 时搜索直接报错而不是发请求。
	if _, err := c.Search(context.Background(), "x", 3); err == nil {
		t.Fatal("off 时 Search 应报错")
	}
	// fetch 不受 provider 影响。
	if _, err := c.Fetch(context.Background(), ""); err == nil {
		t.Fatal("空 URL 应报错")
	}
}

func TestSearchDuckDuckGo(t *testing.T) {
	const page = `<html><body>
<div class="result results_links">
  <a rel="nofollow" class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fgo.dev%2Fdoc%2F&amp;rut=abc">Go 文档</a>
  <a class="result__snippet" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fgo.dev%2Fdoc%2F">Go 语言官方文档，含<a href="/x">指南</a>与规范</a>
</div>
<div class="result results_links">
  <a class="result__a" href="https://example.com/direct">直链结果</a>
  <a class="result__snippet">直接给出的摘要</a>
</div>
<a class="result__a" href="javascript:void(0)">过滤掉</a>
</body></html>`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		if got := r.Header.Get("Content-Type"); !strings.Contains(got, "x-www-form-urlencoded") {
			t.Errorf("content-type = %q", got)
		}
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm: %v", err)
		}
		if r.PostForm.Get("q") != "go 文档" {
			t.Errorf("q = %q", r.PostForm.Get("q"))
		}
		fmt.Fprint(w, page)
	}))
	defer srv.Close()

	c := newTestClient(t, Config{Search: SearchConfig{BaseURL: srv.URL}})
	hits, err := c.Search(context.Background(), "go 文档", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("hits = %d，期望 2：%+v", len(hits), hits)
	}
	if hits[0].URL != "https://go.dev/doc/" {
		t.Errorf("uddg 未解包：%q", hits[0].URL)
	}
	if hits[0].Title != "Go 文档" {
		t.Errorf("title = %q", hits[0].Title)
	}
	if !strings.Contains(hits[0].Snippet, "官方文档") || !strings.Contains(hits[0].Snippet, "指南") {
		t.Errorf("snippet = %q", hits[0].Snippet)
	}
	// 摘要里的嵌套标签要去掉。
	if strings.Contains(hits[0].Snippet, "<a") {
		t.Errorf("snippet 残留标签：%q", hits[0].Snippet)
	}
	if hits[1].URL != "https://example.com/direct" {
		t.Errorf("直链被改写：%q", hits[1].URL)
	}

	// max 截断。
	hits, err = c.Search(context.Background(), "go 文档", 1)
	if err != nil || len(hits) != 1 {
		t.Fatalf("max 未生效：%v %d", err, len(hits))
	}
}

func TestSearchTavily(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tvly-k" {
			t.Errorf("auth = %q", got)
		}
		fmt.Fprint(w, `{"results":[
			{"title":"A","url":"https://a.example","content":"摘要 A"},
			{"title":"","url":"","content":"空 URL 被丢弃"}]}`)
	}))
	defer srv.Close()

	c := newTestClient(t, Config{Search: SearchConfig{
		Provider: ProviderTavily, APIKey: "tvly-k", BaseURL: srv.URL,
	}})
	hits, err := c.Search(context.Background(), "q", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 || hits[0].URL != "https://a.example" || hits[0].Snippet != "摘要 A" {
		t.Fatalf("hits = %+v", hits)
	}
}

func TestSearchBrave(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Subscription-Token"); got != "bk" {
			t.Errorf("token = %q", got)
		}
		fmt.Fprint(w, `{"web":{"results":[
			{"title":"Go &amp; Rust","url":"https://a.example","description":"包含 &lt;b&gt;标签&lt;/b&gt; 的描述"}]}}`)
	}))
	defer srv.Close()

	c := newTestClient(t, Config{Search: SearchConfig{
		Provider: ProviderBrave, APIKey: "bk", BaseURL: srv.URL,
	}})
	hits, err := c.Search(context.Background(), "q", 3)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("hits = %+v", hits)
	}
	if hits[0].Title != "Go & Rust" {
		t.Errorf("title = %q", hits[0].Title)
	}
	if hits[0].Snippet != "包含 标签 的描述" {
		t.Errorf("snippet 未清洗 = %q", hits[0].Snippet)
	}
}

func TestSearchExa(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("x-api-key"); got != "ek" {
			t.Errorf("key = %q", got)
		}
		fmt.Fprint(w, `{"results":[{"title":"T","url":"https://a.example","text":"正文片段"}]}`)
	}))
	defer srv.Close()

	c := newTestClient(t, Config{Search: SearchConfig{
		Provider: ProviderExa, APIKey: "ek", BaseURL: srv.URL,
	}})
	hits, err := c.Search(context.Background(), "q", 3)
	if err != nil || len(hits) != 1 || hits[0].Snippet != "正文片段" {
		t.Fatalf("hits = %+v err = %v", hits, err)
	}
}

func TestSearchSearxNG(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("format") != "json" {
			t.Errorf("format = %q", r.URL.Query().Get("format"))
		}
		fmt.Fprint(w, `{"results":[{"title":"T","url":"https://a.example","content":"摘要"}]}`)
	}))
	defer srv.Close()

	c := newTestClient(t, Config{Search: SearchConfig{
		Provider: ProviderSearxNG, BaseURL: srv.URL,
	}})
	hits, err := c.Search(context.Background(), "q", 3)
	if err != nil || len(hits) != 1 || hits[0].Snippet != "摘要" {
		t.Fatalf("hits = %+v err = %v", hits, err)
	}

	// 实例没开 JSON 输出时要给出可操作的提示。
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "<html>not json</html>")
	}))
	defer srv2.Close()
	c2 := newTestClient(t, Config{Search: SearchConfig{Provider: ProviderSearxNG, BaseURL: srv2.URL}})
	_, err = c2.Search(context.Background(), "q", 3)
	if err == nil || !strings.Contains(err.Error(), "format=json") {
		t.Fatalf("err = %v", err)
	}
}

func TestSearchHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, "rate limited")
	}))
	defer srv.Close()

	c := newTestClient(t, Config{Search: SearchConfig{Provider: ProviderTavily, APIKey: "k", BaseURL: srv.URL}})
	_, err := c.Search(context.Background(), "q", 3)
	if err == nil {
		t.Fatal("期望报错")
	}
	if !strings.Contains(err.Error(), "429") || !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("err = %v", err)
	}
}

func TestSearchContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		fmt.Fprint(w, "{}")
	}))
	defer srv.Close()

	c := newTestClient(t, Config{Search: SearchConfig{Provider: ProviderTavily, APIKey: "k", BaseURL: srv.URL}})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := c.Search(ctx, "q", 3)
	if err == nil {
		t.Fatal("期望超时错误")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "超时") {
		t.Fatalf("err = %v", err)
	}
}

func TestFetchHTML(t *testing.T) {
	const page = `<!doctype html><html><head><title>Go 并发模式</title>
<meta name="description" content="关于 goroutine 的文章"></head>
<body>
<script>var x = 1;</script>
<style>.a{color:red}</style>
<nav>菜单 首页 归档</nav>
<h1>Go 并发模式</h1>
<p>Go 的并发模型基于 CSP，goroutine 是最小的执行单元。</p>
<p>channel 用于在 goroutine 之间传递消息，而不是共享内存。</p>
<pre><code>go func() { fmt.Println("hi") }()</code></pre>
<ul><li>goroutine</li><li>channel</li></ul>
</body></html>`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("User-Agent"); got == "" {
			t.Error("缺少 User-Agent")
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, page)
	}))
	defer srv.Close()

	c := newTestClient(t, Config{})
	out, err := c.Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	for _, want := range []string{"Go 并发模式", "基于 CSP", "传递消息", `fmt.Println("hi")`, "- goroutine"} {
		if !strings.Contains(out, want) {
			t.Errorf("输出缺少 %q\n---\n%s", want, out)
		}
	}
	for _, bad := range []string{"var x = 1", "color:red", "菜单 首页", "<script", "<p>"} {
		if strings.Contains(out, bad) {
			t.Errorf("输出残留 %q\n---\n%s", bad, out)
		}
	}
	if !strings.HasPrefix(out, "[URL") {
		t.Errorf("缺少页头：%q", out)
	}
}

func TestFetchPlainTextAndJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"a":1}`)
	}))
	defer srv.Close()

	c := newTestClient(t, Config{})
	out, err := c.Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !strings.Contains(out, `{"a":1}`) {
		t.Fatalf("JSON 正文被破坏：%q", out)
	}
}

func TestFetchCharsets(t *testing.T) {
	// GB18030（超集 GBK）：常见中文站点。
	gbkSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=gbk")
		w.Write([]byte{0xD6, 0xD0, 0xCE, 0xC4}) // "中文"
	}))
	defer gbkSrv.Close()
	c := newTestClient(t, Config{})
	out, err := c.Fetch(context.Background(), gbkSrv.URL)
	if err != nil {
		t.Fatalf("Fetch gbk: %v", err)
	}
	if !strings.Contains(out, "中文") {
		t.Fatalf("GBK 未解码：%q", out)
	}

	// 正文是 UTF-16LE（带 BOM），Content-Type 不声明 charset。
	u16 := []byte{0xFF, 0xFE}
	for _, r := range "你好" {
		u16 = append(u16, byte(r), byte(r>>8))
	}
	u16Srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write(u16)
	}))
	defer u16Srv.Close()
	out, err = c.Fetch(context.Background(), u16Srv.URL)
	if err != nil {
		t.Fatalf("Fetch utf16: %v", err)
	}
	if !strings.Contains(out, "你好") {
		t.Fatalf("UTF-16 未解码：%q", out)
	}
}

func TestFetchGzipAndRedirect(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/go":
			http.Redirect(w, r, srv.URL+"/final", http.StatusFound)
		case "/final":
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, "重定向后的正文")
		case "/slow":
			http.Redirect(w, r, srv.URL+"/loop", http.StatusFound)
		case "/loop":
			http.Redirect(w, r, srv.URL+"/slow", http.StatusFound)
		}
	}))
	defer srv.Close()

	c := newTestClient(t, Config{})
	out, err := c.Fetch(context.Background(), srv.URL+"/go")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !strings.Contains(out, "重定向后的正文") {
		t.Fatalf("重定向失败：%q", out)
	}
	if !strings.Contains(out, "/final") {
		t.Errorf("页头应显示最终 URL：%q", out)
	}

	// 无限重定向要报错而不是挂住。
	if _, err := c.Fetch(context.Background(), srv.URL+"/slow"); err == nil {
		t.Fatal("期望重定向过多报错")
	}
}

func TestFetchTruncates(t *testing.T) {
	big := strings.Repeat("x", 200_000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, big)
	}))
	defer srv.Close()

	c := newTestClient(t, Config{Fetch: FetchConfig{MaxBytes: 50 << 10}})
	out, err := c.Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(out) > 50<<10+512 {
		t.Fatalf("未截断：%d 字节", len(out))
	}
	if !strings.Contains(out, "已截断") {
		t.Fatalf("缺少截断提示：%q", out[len(out)-200:])
	}
}

func TestFetchMaxRunes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, strings.Repeat("中", 5000))
	}))
	defer srv.Close()

	c := newTestClient(t, Config{Fetch: FetchConfig{MaxChars: 1000}})
	out, err := c.Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	// 页头 + 1000 字 + 截断提示。
	if n := utf8.RuneCountInString(out); n > 1300 {
		t.Fatalf("未按字符截断：%d", n)
	}
	if !strings.Contains(out, "已截断") {
		t.Fatal("缺少截断提示")
	}
}

func TestFetchRejectsNonHTTP(t *testing.T) {
	c := newTestClient(t, Config{})
	for _, u := range []string{"file:///etc/passwd", "ftp://x/y", "not-a-url"} {
		if _, err := c.Fetch(context.Background(), u); err == nil {
			t.Errorf("%q 应被拒绝", u)
		}
	}
}

func TestDuckDuckGoChallengePage(t *testing.T) {
	// DDG 限流时返回 200 + 一个验证页：必须报错而不是「没有结果」。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<html><body><div class="anomaly-modal">Unusual traffic detected</div></body></html>`)
	}))
	defer srv.Close()

	c := newTestClient(t, Config{Search: SearchConfig{BaseURL: srv.URL}})
	_, err := c.Search(context.Background(), "q", 3)
	if err == nil {
		t.Fatal("验证页应报错")
	}
	if !strings.Contains(err.Error(), "人机校验") {
		t.Fatalf("err = %v", err)
	}

	// 空页（真的没结果）不是错误。
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<html><body>No results found.</body></html>`)
	}))
	defer empty.Close()
	c2 := newTestClient(t, Config{Search: SearchConfig{BaseURL: empty.URL}})
	hits, err := c2.Search(context.Background(), "q", 3)
	if err != nil {
		t.Fatalf("空结果不应报错：%v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("hits = %+v", hits)
	}
}
