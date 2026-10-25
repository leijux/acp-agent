package kernel_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/leijux/acp-agent/internal/kernel"
)

// benchMessages 生成 n 条混合消息：约 1/5 是工具结果（真实 agent 的 token 大头）。
func benchMessages(n int) []kernel.Message {
	msgs := make([]kernel.Message, 0, n)
	for i := 0; i < n; i++ {
		m := kernel.Message{ID: fmt.Sprintf("bmsg_%05d", i), Role: kernel.RoleUser, ContentType: kernel.ContentText}
		switch {
		case i%2 == 1:
			m.Role = kernel.RoleAssistant
			m.Text = fmt.Sprintf("turn %d 分析：上下文主要被工具输出占据。", i) +
				strings.Repeat("建议按 tier 折叠历史摘要并对 reasoning 单独计费。", 20)
		case i%5 == 3:
			m.Role = kernel.RoleTool
			m.ContentType = kernel.ContentToolResult
			m.ToolName = "grep"
			m.ToolCallID = fmt.Sprintf("call_%d", i)
			var b strings.Builder
			fmt.Fprintf(&b, "grep -rn contextUsage src/ (turn %d)\n", i)
			for j := 0; j < 40; j++ {
				fmt.Fprintf(&b, "src/ctx/usage_%02d.ts:%d:  const contextUsage = tokens / limit; // 压缩与缓存命中率 %d\n", j, 100+j*7, i+j)
			}
			m.Text = b.String()
		default:
			m.Text = fmt.Sprintf("第 %d 轮：帮我看看上下文占用为什么涨得快，", i) +
				strings.Repeat("顺便评估一下压缩分级策略和缓存命中率的影响，", 12)
		}
		msgs = append(msgs, m)
	}
	return msgs
}

func benchSession(b *testing.B, k *kernel.Kernel, limit int) *kernel.Session {
	b.Helper()
	sess, err := k.NewSession(kernel.Config{ModelContextLimit: limit})
	if err != nil {
		b.Fatalf("NewSession: %v", err)
	}
	return sess
}

// BenchmarkNewSession 量化每个会话的初始化成本（bundle 解析 + createCore）。
func BenchmarkNewSession(b *testing.B) {
	k, err := kernel.New()
	if err != nil {
		b.Fatalf("kernel.New: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sess, err := k.NewSession(kernel.Config{ModelContextLimit: 200_000})
		if err != nil {
			b.Fatalf("NewSession: %v", err)
		}
		_ = sess.Close()
	}
}

// BenchmarkTurn 每次重开会话（保证 state 干净），NewSession 成本被 StopTimer 排除。
func BenchmarkTurn(b *testing.B) {
	k, err := kernel.New()
	if err != nil {
		b.Fatalf("kernel.New: %v", err)
	}
	for _, n := range []int{40, 200, 1000} {
		msgs := benchMessages(n)
		b.Run(fmt.Sprintf("msgs=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				sess := benchSession(b, k, 200_000)
				b.StartTimer()
				if _, err := sess.Turn(msgs, 90_000); err != nil {
					b.Fatalf("Turn: %v", err)
				}
				b.StopTimer()
				_ = sess.Close()
				b.StartTimer()
			}
		})
	}
}

// BenchmarkTurnSteady 同一会话反复 Turn 同一组消息（稳态；refs 按 message id 去重不增长）。
func BenchmarkTurnSteady(b *testing.B) {
	k, err := kernel.New()
	if err != nil {
		b.Fatalf("kernel.New: %v", err)
	}
	// 覆盖 100..800，用来看 Turn 随消息数的增长曲线（实测是超线性的）。
	for _, n := range []int{100, 200, 400, 800} {
		msgs := benchMessages(n)
		b.Run(fmt.Sprintf("msgs=%d", n), func(b *testing.B) {
			sess := benchSession(b, k, 200_000)
			defer sess.Close()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := sess.Turn(msgs, 90_000); err != nil {
					b.Fatalf("Turn: %v", err)
				}
			}
		})
	}
}

// BenchmarkTurnFolded 先压掉前 40%，再测稳态 Turn（模拟长会话的真实形状）。
func BenchmarkTurnFolded(b *testing.B) {
	k, err := kernel.New()
	if err != nil {
		b.Fatalf("kernel.New: %v", err)
	}
	msgs := benchMessages(200)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		sess := benchSession(b, k, 200_000)
		b.StartTimer()
		res, err := sess.Turn(msgs, 90_000)
		if err != nil {
			b.Fatalf("Turn: %v", err)
		}
		if len(res.Nudge.CompressibleRanges) > 0 {
			r := res.Nudge.CompressibleRanges[0]
			if _, err := sess.ApplyCompression([]kernel.Range{{
				StartRef: r.StartRef, EndRef: r.EndRef, Topic: "t", Summary: longSummary,
			}}); err != nil {
				b.Fatalf("ApplyCompression: %v", err)
			}
			if _, err := sess.Turn(msgs, 90_000); err != nil {
				b.Fatalf("Turn#2: %v", err)
			}
		}
		b.StopTimer()
		_ = sess.Close()
		b.StartTimer()
	}
}

// BenchmarkTurnParallel 每 goroutine 一个会话（独立 VM）——测真实并发吞吐。
// 配合 -cpu=1,2,4,8 可看出 VM/GC 争用导致的扩展性上限。
func BenchmarkTurnParallel(b *testing.B) {
	k, err := kernel.New()
	if err != nil {
		b.Fatalf("kernel.New: %v", err)
	}
	msgs := benchMessages(200)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		sess := benchSession(b, k, 200_000)
		defer sess.Close()
		for pb.Next() {
			if _, err := sess.Turn(msgs, 90_000); err != nil {
				b.Fatalf("Turn: %v", err)
			}
		}
	})
}

// BenchmarkTurnSharedVM 所有 goroutine 共用同一个 Session（VM 内串行化）——
// 证明共享 VM 的吞吐上限，用于对比池化方案。
func BenchmarkTurnSharedVM(b *testing.B) {
	k, err := kernel.New()
	if err != nil {
		b.Fatalf("kernel.New: %v", err)
	}
	msgs := benchMessages(200)
	sess := benchSession(b, k, 200_000)
	defer sess.Close()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := sess.Turn(msgs, 90_000); err != nil {
				b.Fatalf("Turn: %v", err)
			}
		}
	})
}

func BenchmarkVersion(b *testing.B) {
	k, err := kernel.New()
	if err != nil {
		b.Fatalf("kernel.New: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := k.Version(); err != nil {
			b.Fatalf("Version: %v", err)
		}
	}
}

func BenchmarkStatus(b *testing.B) {
	k, err := kernel.New()
	if err != nil {
		b.Fatalf("kernel.New: %v", err)
	}
	sess := benchSession(b, k, 200_000)
	defer sess.Close()
	if _, err := sess.Turn(benchMessages(200), 90_000); err != nil {
		b.Fatalf("Turn: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := sess.Status(90_000); err != nil {
			b.Fatalf("Status: %v", err)
		}
	}
}

// BenchmarkEndToEndSession 一个会话从创建到 20 轮对话的完整生命周期。
func BenchmarkEndToEndSession(b *testing.B) {
	k, err := kernel.New()
	if err != nil {
		b.Fatalf("kernel.New: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		sess := benchSession(b, k, 200_000)
		b.StartTimer()
		var history []kernel.Message
		tokens := 90_000
		for turn := 1; turn <= 20; turn++ {
			history = append(history, benchMessages(turn * 10)[turn*10-10:]...)
			res, err := sess.Turn(history, tokens)
			if err != nil {
				b.Fatalf("Turn %d: %v", turn, err)
			}
			history = append([]kernel.Message(nil), res.Messages...)
			tokens += 500
		}
		b.StopTimer()
		_ = sess.Close()
		b.StartTimer()
	}
}

// 保证 bench 里用到的辅助函数不会因为未使用而编译失败。
var _ = context.Background

// BenchmarkTurnMeta 对比 Turn：只推进状态 + 取 nudge，不序列化消息视图。
func BenchmarkTurnMeta(b *testing.B) {
	k, err := kernel.New()
	if err != nil {
		b.Fatalf("kernel.New: %v", err)
	}
	for _, n := range []int{200, 1000} {
		msgs := benchMessages(n)
		b.Run(fmt.Sprintf("msgs=%d", n), func(b *testing.B) {
			sess := benchSession(b, k, 200_000)
			defer sess.Close()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := sess.TurnMeta(msgs, 90_000); err != nil {
					b.Fatalf("TurnMeta: %v", err)
				}
			}
		})
	}
}

// BenchmarkEstimateTokens 纯 Go 侧估算成本（作为 Turn 的 tokenCount 兜底）。
func BenchmarkEstimateTokens(b *testing.B) {
	msgs := benchMessages(1000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if kernel.EstimateTokens(msgs) <= 0 {
			b.Fatal("zero estimate")
		}
	}
}
