package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/leijux/acp-agent/internal/web"
)

func TestBuildWebProviders(t *testing.T) {
	// 默认 DuckDuckGo：无需 key，直接可用。
	cfg := Config{Web: &web.Config{Search: web.SearchConfig{Provider: web.ProviderDuckDuckGo}}}
	search, fetch, ok := buildWebProviders(cfg, &bytes.Buffer{})
	if !ok || search == nil || fetch == nil {
		t.Fatalf("duckduckgo 应可用：ok=%v", ok)
	}
	if got := registryDisable(cfg, ok); containsStr(got, "web_search") {
		t.Fatalf("可用时不该禁用工具：%v", got)
	}

	// off：两个工具都摘掉。
	cfg = Config{Web: &web.Config{Search: web.SearchConfig{Provider: web.ProviderOff}}}
	_, _, ok = buildWebProviders(cfg, &bytes.Buffer{})
	if ok {
		t.Fatal("off 时不该可用")
	}
	got := registryDisable(cfg, ok)
	if !containsStr(got, "web_search") || !containsStr(got, "web_fetch") {
		t.Fatalf("off 时应禁用两个工具：%v", got)
	}

	// 配置非法（tavily 缺 key）：降级为关闭，并把原因打到输出里。
	var buf bytes.Buffer
	cfg = Config{Web: &web.Config{Search: web.SearchConfig{Provider: web.ProviderTavily}}}
	_, _, ok = buildWebProviders(cfg, &buf)
	if ok {
		t.Fatal("缺 key 时不该可用")
	}
	if !strings.Contains(buf.String(), "web 能力已关闭") {
		t.Fatalf("缺少提示：%q", buf.String())
	}

	// 已有的 disableTools 不能被覆盖。
	cfg = Config{DisableTools: []string{"bash"}}
	if got := registryDisable(cfg, false); !containsStr(got, "bash") {
		t.Fatalf("disableTools 丢了：%v", got)
	}
}

// 注入的 provider 与 internal/web 的返回值类型能对上。
func TestWebProvidersAdapt(t *testing.T) {
	var buf bytes.Buffer
	cfg := Config{Web: &web.Config{Search: web.SearchConfig{Provider: web.ProviderDuckDuckGo}}}
	search, fetch, ok := buildWebProviders(cfg, &buf)
	if !ok {
		t.Fatal("应可用")
	}
	if search == nil || fetch == nil {
		t.Fatal("provider 不应为 nil")
	}
	// 不去真连网：只验证 fetch 的空 URL 校验能透出来。
	if _, err := fetch(context.Background(), ""); err == nil {
		t.Fatal("空 URL 应报错")
	}
}

func containsStr(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
