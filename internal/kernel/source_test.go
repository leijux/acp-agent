package kernel_test

import (
	"testing"

	"github.com/leijux/acp-agent/internal/kernel"
)

// 170_000 / 200_000 = 85%，超过上游 maxContextLimitPct（0.75）→ 压力档。
const pressureTokens = 170_000

// pressureSession 建一个把两个关键默认值钉死的会话：
//   - maxContextLimitPct：让 85% 确定落在压力档；
//   - maxEstimateHoldRounds：押几轮是测试的前提。
//
// 这样上游调默认值时不会让「接线回归」的测试误报红。
func pressureSession(t *testing.T) *kernel.Session {
	t.Helper()
	k, err := kernel.New()
	if err != nil {
		t.Fatalf("kernel.New: %v", err)
	}
	s, err := k.NewSession(kernel.Config{
		ModelContextLimit: 200_000,
		Overrides: map[string]any{"nudge": map[string]any{
			"maxContextLimitPct":    0.75,
			"maxEstimateHoldRounds": 2,
			"requireUsageSampleForPressure": true,
		}},
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestTurnSourcePressureGuard 计数来源要真的传到上游：
// 估值级读数触发幻压守卫（压力档最多压 maxEstimateHoldRounds 轮），
// usage 级读数直接放行，不声明来源则维持旧行为。
//
// 回归点：`processTurn({ tokenCountSource })` 是 0.0.184+ 的入参，忘了接线
// 时这个测试会在「estimate 应被押住」那一步挂掉。
func TestTurnSourcePressureGuard(t *testing.T) {
	t.Run("estimate", func(t *testing.T) {
		s := pressureSession(t)
		// 上游的 streak 只在非 estimate 调用时归零，默认上限 2：
		// 前两轮押住，第三轮按估值照常注入（之后一直照常 —— #5865 的防饿死设计）。
		for round := 1; round <= 3; round++ {
			res, err := s.TurnSource(fixture(), pressureTokens, kernel.TokenCountEstimate)
			if err != nil {
				t.Fatalf("TurnSource#%d: %v", round, err)
			}
			if res.Nudge == nil {
				t.Fatalf("第 %d 轮没有 nudge（压力档即使被押住也该给出说明）", round)
			}
			deferred := res.Nudge.PressureDeferredNoSample()
			if round <= 2 {
				if !deferred || res.Nudge.ShouldInject {
					t.Fatalf("第 %d 轮估值级读数应被押住：inject=%v deferred=%v reason=%q",
						round, res.Nudge.ShouldInject, deferred, res.Nudge.Reason)
				}
				continue
			}
			if deferred || !res.Nudge.ShouldInject {
				t.Fatalf("第 %d 轮应越过后卫照常注入：inject=%v deferred=%v reason=%q",
					round, res.Nudge.ShouldInject, deferred, res.Nudge.Reason)
			}
		}
	})

	t.Run("usage", func(t *testing.T) {
		s := pressureSession(t)
		res, err := s.TurnSource(fixture(), pressureTokens, kernel.TokenCountUsage)
		if err != nil {
			t.Fatalf("TurnSource: %v", err)
		}
		if res.Nudge == nil {
			t.Fatal("usage 级读数在 85% 占用下应该给出压力档 nudge")
		}
		if res.Nudge.PressureDeferredNoSample() {
			t.Fatalf("usage 级读数不该被押住: %q", res.Nudge.Reason)
		}
		if !res.Nudge.ShouldInject {
			t.Fatalf("usage 级读数应注入: %q", res.Nudge.Reason)
		}
	})

	t.Run("unspecified", func(t *testing.T) {
		// 不声明来源 = 旧行为（守卫不生效），Turn 与 TurnMeta 都要保持一致。
		s := pressureSession(t)
		res, err := s.Turn(fixture(), pressureTokens)
		if err != nil {
			t.Fatalf("Turn: %v", err)
		}
		if res.Nudge == nil || !res.Nudge.ShouldInject {
			t.Fatalf("不声明来源时应按旧行为注入: %+v", res.Nudge)
		}
		if res.Nudge.PressureDeferredNoSample() {
			t.Fatal("不声明来源时守卫不该生效")
		}

		s2 := newSession(t, 200_000)
		meta, err := s2.TurnMeta(fixture(), pressureTokens)
		if err != nil {
			t.Fatalf("TurnMeta: %v", err)
		}
		if meta.Nudge == nil || !meta.Nudge.ShouldInject {
			t.Fatalf("TurnMeta 不声明来源时应按旧行为注入: %+v", meta.Nudge)
		}
	})

	t.Run("meta_estimate", func(t *testing.T) {
		s := pressureSession(t)
		meta, err := s.TurnMetaSource(fixture(), pressureTokens, kernel.TokenCountEstimate)
		if err != nil {
			t.Fatalf("TurnMetaSource: %v", err)
		}
		if meta.Nudge == nil || !meta.Nudge.PressureDeferredNoSample() || meta.Nudge.ShouldInject {
			t.Fatalf("TurnMetaSource(estimate) 应被押住: %+v", meta.Nudge)
		}
	})
}

// TestCompressionNoop 重复提交同一区间是幂等空操作，不是失败（上游 #4521）。
//
// 回归点：`blocksCreated == 0 && errors.length == 0` 的结果以前看起来就跟
// 失败一样，模型会反复重试同一段。
func TestCompressionNoop(t *testing.T) {
	s := newSession(t, 200_000)
	if _, err := s.Turn(fixture(), 90_000); err != nil {
		t.Fatalf("Turn: %v", err)
	}
	rng := kernel.Range{StartRef: "m00005", EndRef: "m00020", Summary: longSummary}

	first, err := s.ApplyCompression([]kernel.Range{rng})
	if err != nil {
		t.Fatalf("ApplyCompression#1: %v", err)
	}
	if first.BlocksCreated == 0 || first.Noop {
		t.Fatalf("首次压缩应真建块: %+v", first)
	}

	second, err := s.ApplyCompression([]kernel.Range{rng})
	if err != nil {
		t.Fatalf("ApplyCompression#2: %v", err)
	}
	if !second.Noop {
		t.Fatalf("重复压缩同一区间应标记 noop: %+v", second)
	}
	if second.BlocksCreated != 0 {
		t.Fatalf("noop 不应建块，实际 %d", second.BlocksCreated)
	}
	if second.TokensCompressed != 0 {
		t.Fatalf("noop 不应省下 token，实际 %d", second.TokensCompressed)
	}
	if len(second.Errors) != 0 {
		t.Fatalf("noop 不是错误，实际 errors=%v", second.Errors)
	}
}

// TestTurnSourceRejectsUnknown 非法来源必须在入口被拦住。
//
// 上游对未知值不报错（任何非 "estimate" 都被当 usage 级），所以拼错一个字母
// 会静默失去保护 —— 这里靠 Go 侧白名单避免。
func TestTurnSourceRejectsUnknown(t *testing.T) {
	s := newSession(t, 200_000)
	if _, err := s.TurnSource(fixture(), 1_000, kernel.TokenCountSource("Usage")); err == nil {
		t.Fatal("TurnSource 应拒绝拼写错误的 source")
	}
	if _, err := s.TurnMetaSource(fixture(), 1_000, kernel.TokenCountSource("est")); err == nil {
		t.Fatal("TurnMetaSource 应拒绝拼写错误的 source")
	}
	// 合法值不受影响。
	if _, err := s.TurnSource(fixture(), 1_000, kernel.TokenCountUsage); err != nil {
		t.Fatalf("TurnSource(usage): %v", err)
	}
}

// TestBundleVersion 版本号来自重打包脚本生成的常量，不能凭空返回空串。
func TestBundleVersion(t *testing.T) {
	k, err := kernel.New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := k.Version()
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if kernel.BundleVersion == "" {
		t.Fatal("BundleVersion 为空：version_gen.go 没生成？")
	}
	if got != kernel.BundleVersion {
		t.Fatalf("Version() = %q, BundleVersion = %q", got, kernel.BundleVersion)
	}
}
