package tools

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestLockFileMutualExclusionUnderChurn 压力验证「同路径互斥」在
// 「频繁加锁/解锁导致锁条目被摘除重建」的场景下依然成立。
//
// 背景：审阅时曾怀疑存在「条目被摘除后重建，导致同路径出现两把锁」的窗口。
// 逐行复核后判定不成立（refs 在阻塞前自增、在释放后自减，删条目只会发生在
// 零使用者时），但这条不变量值得用测试钉住 —— 谁改动引用计数都会撞到它。
func TestLockFileMutualExclusionUnderChurn(t *testing.T) {
	const (
		// 参数压到「足够暴露竞态但不拖慢测试套件」：-race 下约 1s。
		goroutines = 32
		rounds     = 60
	)
	path := "concurrent/same/file.go"

	var inside atomic.Int32
	var overlaps atomic.Int32
	var mu sync.Mutex
	seen := 0

	unlockShared := LockFile(path)
	unlockShared() // 先制造一次「建立即摘除」，让后续重建更频繁

	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				unlock := LockFile(path)
				if n := inside.Add(1); n != 1 {
					overlaps.Add(1)
				}
				// 临界区里做点非原子的读改写，放大竞态的可观测窗口。
				mu.Lock()
				seen++
				mu.Unlock()
				time.Sleep(time.Microsecond)
				inside.Add(-1)
				unlock()
			}
		}(i)
	}
	wg.Wait()

	if n := overlaps.Load(); n != 0 {
		t.Fatalf("同一路径出现 %d 次并发持锁（互斥失效）", n)
	}
	if want := goroutines * rounds; seen != want {
		t.Fatalf("临界区执行次数 %d，期望 %d", seen, want)
	}
}

// TestLockFileDistinctPathsDoNotBlock 不同路径必须互不阻塞。
func TestLockFileDistinctPathsDoNotBlock(t *testing.T) {
	a := LockFile("dir/a.go")
	defer a()

	done := make(chan struct{})
	go func() {
		u := LockFile("dir/b.go")
		u()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("不同路径之间被错误地互斥了")
	}
}

// TestLockFileEntryReclaimed 全部释放后条目应从表中摘除，避免无限积累。
func TestLockFileEntryReclaimed(t *testing.T) {
	key := lockKey("tmp/reclaim.go")
	LockFile("tmp/reclaim.go")()

	fileLocks.mu.Lock()
	_, alive := fileLocks.locks[key]
	n := len(fileLocks.locks)
	fileLocks.mu.Unlock()
	if alive {
		t.Errorf("解锁后条目未摘除: %v（当前 %d 项）", key, n)
	}
}

// TestLockFileUnlockIsIdempotent 重复 unlock 不能把引用计数打成负数。
func TestLockFileUnlockIsIdempotent(t *testing.T) {
	u := LockFile("tmp/idem.go")
	u()
	u()
	u()

	// 再锁一次必须立刻成功（说明计数没被破坏成阻塞态）。
	done := make(chan struct{})
	go func() {
		LockFile("tmp/idem.go")()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("重复 unlock 破坏了引用计数，后续加锁死锁")
	}
}
