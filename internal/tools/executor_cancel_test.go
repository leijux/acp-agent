package tools

import (
	"context"
	"testing"
	"time"
)

// TestExecutorCancelDuringParallelDoesNotHang 回归：批内取消不能让 Execute 永久阻塞。
//
// 旧实现里 runParallel 的两个 select 分支共用一个 `defer <-sem`：
// ctx 在 goroutine 拿到 token 之前就取消时，defer 会去接收一个永远不会到来的
// token，wg.Wait() 于是永久阻塞，整个 turn 卡死。
func TestExecutorCancelDuringParallelDoesNotHang(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Register(&fakeTool{name: "slow", safe: true, delay: 200 * time.Millisecond}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	ex := NewExecutor(reg, 1) // 并发度 1：必然有 goroutine 在排 sem

	ctx, cancel := context.WithCancel(context.Background())
	calls := make([]Call, 0, 8)
	for i := 0; i < 8; i++ {
		calls = append(calls, Call{ID: string(rune('a' + i)), Name: "slow"})
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		ex.Execute(ctx, calls) // 结果不关心，只关心它一定会返回
	}()

	// 让部分 goroutine 卡在 sem 上，然后取消。
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ctx 取消后 Executor.Execute 没有返回（信号量死锁）")
	}
}

// TestExecutorCanceledBatchReturnsPlaceholders 取消时每个调用都要有结果占位。
func TestExecutorCanceledBatchReturnsPlaceholders(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Register(&fakeTool{name: "slow", safe: true, delay: 100 * time.Millisecond}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	ex := NewExecutor(reg, 1)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan []ExecResult, 1)
	go func() {
		done <- ex.Execute(ctx, []Call{
			{ID: "1", Name: "slow"},
			{ID: "2", Name: "slow"},
			{ID: "3", Name: "slow"},
		})
	}()
	time.Sleep(10 * time.Millisecond)
	cancel()

	select {
	case results := <-done:
		if len(results) != 3 {
			t.Fatalf("结果数 %d，期望 3", len(results))
		}
		for i, r := range results {
			if r.Call.Name != "slow" {
				t.Errorf("results[%d].Call 丢了: %+v", i, r.Call)
			}
			// 要么执行了（有 Result），要么被取消（有占位错误），不能两者皆空。
			if r.Result.Content == nil && r.Err == nil {
				t.Errorf("results[%d] 既无结果也无错误", i)
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Execute 未返回")
	}
}
