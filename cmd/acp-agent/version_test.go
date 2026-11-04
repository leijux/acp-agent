package main

import (
	"regexp"
	"strconv"
	"testing"
)

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

// defaultVersion 会原样落进编排器的能力门，改它之前先让这条测试拦一下。
//
// Multica 的 pi backend 用 `v?(\d+)\.(\d+)\.(\d+)` 抽 semver（agent/version.go），
// 然后：
//   - `validatePiMcpVersion` 要求 ≥ 0.99.0，否则配了 MCP 的任务直接失败；
//   - `SupportsTaskSupplement` 要求 ≥ 1.4.0 才发 `--mode rpc`（prompt/steer）。
//
// 现在我们已经实现了 rpc 的 prompt/steer/agent_settled，所以 1.4.0 是正确目标：
// 低于它 steering 用不上，高于它等于宣称我们没实现的能力。
func TestDefaultVersionSatisfiesRuntimeGates(t *testing.T) {
	re := regexp.MustCompile(`v?(\d+)\.(\d+)\.(\d+)`)
	m := re.FindStringSubmatch(defaultVersion)
	if m == nil {
		t.Fatalf("defaultVersion = %q 不是编排器能解析的 semver（期望 v?N.N.N）", defaultVersion)
	}
	major, minor, patch := atoi(t, m[1]), atoi(t, m[2]), atoi(t, m[3])
	atLeast := func(wantMajor, wantMinor, wantPatch int) bool {
		if major != wantMajor {
			return major > wantMajor
		}
		if minor != wantMinor {
			return minor > wantMinor
		}
		return patch >= wantPatch
	}
	if !atLeast(0, 99, 0) {
		t.Fatalf("defaultVersion = %q 低于 0.99.0：Multica 会拒绝为 pi 运行时下发受管 MCP", defaultVersion)
	}
	if !atLeast(1, 4, 0) {
		t.Fatalf("defaultVersion = %q 低于 1.4.0：Multica 不会协商 task supplement，steering 用不上", defaultVersion)
	}
	if atLeast(1, 5, 0) {
		t.Fatalf("defaultVersion = %q 越过 1.4.x：那是在宣称我们没实现的能力（先看上游的能力门再改）", defaultVersion)
	}
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatalf("解析版本号 %q 失败: %v", s, err)
	}
	return n
}
