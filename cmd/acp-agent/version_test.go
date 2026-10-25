package main

import "testing"

// 构建时注入的值（-X main.version=...）必须优先于 build info。
func TestResolveVersionPrefersInjected(t *testing.T) {
	old := version
	t.Cleanup(func() { version = old })

	version = "v9.9.9-test"
	if got := resolveVersion(); got != "v9.9.9-test" {
		t.Fatalf("resolveVersion() = %q, 期望注入值 v9.9.9-test", got)
	}
}

// 没有 -ldflags 时（go run / go test），回退到默认值，不能返回空串。
func TestResolveVersionFallsBackToDefault(t *testing.T) {
	old := version
	t.Cleanup(func() { version = old })

	version = defaultVersion
	if got := resolveVersion(); got != defaultVersion {
		t.Fatalf("resolveVersion() = %q, 期望回退值 %q", got, defaultVersion)
	}
	if defaultVersion == "" {
		t.Fatal("defaultVersion 不能为空")
	}
}
