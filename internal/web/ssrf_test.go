package web

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestIsPublicIP 地址分类：内网/回环/链路本地必须判私有。
func TestIsPublicIP(t *testing.T) {
	private := []string{
		"127.0.0.1", "::1", "10.1.2.3", "172.16.0.1", "192.168.1.1",
		"169.254.169.254", // 云元数据
		"0.0.0.0", "::", "224.0.0.1", "fe80::1", "100.64.0.1",
	}
	for _, s := range private {
		if ip := parseIP(t, s); isPublicIP(ip) {
			t.Errorf("%s 应被判为不可抓取", s)
		}
	}
	public := []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"}
	for _, s := range public {
		if ip := parseIP(t, s); !isPublicIP(ip) {
			t.Errorf("%s 应被判为可抓取", s)
		}
	}
	if isPublicIP(nil) {
		t.Error("nil 应判为不可抓取")
	}
}

// TestFetchRejectsPrivateByDefault 默认拒绝 localhost / 内网。
func TestFetchRejectsPrivateByDefault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("should not be reachable"))
	}))
	defer srv.Close()

	c, err := New(Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = c.Fetch(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("默认应拒绝抓取回环地址")
	}
	if !strings.Contains(err.Error(), "内网") && !strings.Contains(err.Error(), "回环") {
		t.Fatalf("拒绝原因不明确: %v", err)
	}
}

// TestFetchAllowPrivateOptIn 配置 allowPrivate 后可以抓本机（自测/内网部署用）。
func TestFetchAllowPrivateOptIn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("hello-local"))
	}))
	defer srv.Close()

	c, err := New(Config{Fetch: FetchConfig{AllowPrivate: true}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	out, err := c.Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("allowPrivate=true 时应能抓取: %v", err)
	}
	if !strings.Contains(out, "hello-local") {
		t.Fatalf("输出 = %q", out)
	}
}

// TestGuardedTransportBlocksPrivateDial 即使绕过 checkPublicURL（例如重定向到内网），
// DialContext 也要拦住 —— 这是 DNS rebinding 的兜底。
func TestGuardedTransportBlocksPrivateDial(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("x"))
	}))
	defer srv.Close()

	tr := newGuardedTransport(false, 0)
	client := &http.Client{Transport: tr}
	if _, err := client.Get(srv.URL); err == nil {
		t.Fatal("DialContext 应拒绝回环地址")
	} else if !strings.Contains(err.Error(), "内网") && !strings.Contains(err.Error(), "回环") {
		t.Fatalf("拒绝原因不明确: %v", err)
	}
}

// TestSearchClientNotGuarded 搜索后端（自建 searxng 常在 localhost）不能读 fetch 的过滤。
func TestSearchClientNotGuarded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"title":"t","url":"https://e.com","content":"c"}]}`))
	}))
	defer srv.Close()

	c, err := New(Config{Search: SearchConfig{
		Provider:   ProviderSearxNG,
		BaseURL:    srv.URL,
		NumResults: 1,
	}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	hits, err := c.Search(context.Background(), "q", 1)
	if err != nil {
		t.Fatalf("搜索不应受 fetch 内网过滤影响: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("命中 %d 条，期望 1", len(hits))
	}
}

func parseIP(t *testing.T, s string) net.IP {
	t.Helper()
	ip := net.ParseIP(s)
	if ip == nil {
		t.Fatalf("无法解析 %s", s)
	}
	return ip
}
