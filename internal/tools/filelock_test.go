package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestLockFileSamePathSerializes 同路径的临界区不允许重叠。
func TestLockFileSamePathSerializes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.txt")

	var mu sync.Mutex
	inside, maxInside := 0, 0
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			unlock := LockFile(path)
			mu.Lock()
			inside++
			if inside > maxInside {
				maxInside = inside
			}
			mu.Unlock()

			time.Sleep(time.Millisecond)

			mu.Lock()
			inside--
			mu.Unlock()
			unlock()
		}()
	}
	wg.Wait()

	if maxInside != 1 {
		t.Fatalf("同路径临界区重叠：maxInside=%d，期望 1", maxInside)
	}
}

// TestLockFileSamePathCaseInsensitive 大小写不同的路径在 Windows/macOS 上是同一文件。
func TestLockFileSamePathCaseInsensitive(t *testing.T) {
	dir := t.TempDir()
	upper := filepath.Join(dir, "A.TXT")
	lower := filepath.Join(dir, "a.txt")

	unlock := LockFile(upper)
	acquired := make(chan struct{})
	go func() {
		unlock2 := LockFile(lower)
		close(acquired)
		unlock2()
	}()

	select {
	case <-acquired:
		if lockKey(upper) == lockKey(lower) {
			t.Fatal("大小写不同但归一后相同，第二个 lockFile 不该立即拿到锁")
		}
	case <-time.After(100 * time.Millisecond):
		// 期望：阻塞到上层解锁。
	}
	unlock()

	select {
	case <-acquired:
	case <-time.After(2 * time.Second):
		t.Fatal("解锁后第二个等待者没有被唤醒")
	}
}

// TestLockFileDifferentPathsParallel 不同路径必须能同时进入临界区。
func TestLockFileDifferentPathsParallel(t *testing.T) {
	dir := t.TempDir()
	var mu sync.Mutex
	inside, maxInside := 0, 0
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			unlock := LockFile(filepath.Join(dir, fmt.Sprintf("f%d.txt", i)))
			mu.Lock()
			inside++
			if inside > maxInside {
				maxInside = inside
			}
			mu.Unlock()

			time.Sleep(5 * time.Millisecond)

			mu.Lock()
			inside--
			mu.Unlock()
			unlock()
		}(i)
	}
	wg.Wait()

	if maxInside < 2 {
		t.Fatalf("不同路径被串行化了：maxInside=%d，期望并发", maxInside)
	}
}

// TestLockFileRefCountCleanup 引用计数归零后表项要被摘掉，否则长期运行会泄漏。
func TestLockFileRefCountCleanup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.txt")
	unlock := LockFile(path)

	fileLocks.mu.Lock()
	n := len(fileLocks.locks)
	fileLocks.mu.Unlock()
	if n == 0 {
		t.Fatal("持锁期间表项丢失")
	}

	unlock()
	fileLocks.mu.Lock()
	n = len(fileLocks.locks)
	fileLocks.mu.Unlock()
	if n != 0 {
		t.Fatalf("解锁后仍残留 %d 个表项", n)
	}

	// 重复调用解锁必须安全（once 保护）。
	unlock()
}

// TestConcurrentWriteSameFileNotTorn 20 个并发 write 同一文件，文件内容必须
// 是某一次写入的完整内容，不能是两次写入交错的碎片。
func TestConcurrentWriteSameFileNotTorn(t *testing.T) {
	dir := t.TempDir()
	pol := Policy{Root: dir, MaxReadBytes: 1 << 20}
	tool := NewWriteTool(pol)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := strings.Repeat(fmt.Sprintf("%02d", i), 4000)
			if _, err := tool.Execute(context.Background(), Args{
				"path": "out.txt", "content": body,
			}); err != nil {
				t.Errorf("write %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	data, err := os.ReadFile(filepath.Join(dir, "out.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 8000 {
		t.Fatalf("文件长度 %d，期望 8000（内容被撕裂）", len(data))
	}
	prefix := string(data[:2])
	if strings.Repeat(prefix, 4000) != string(data) {
		t.Fatalf("文件内容混合了多次写入：前缀 %q", prefix)
	}
}

// TestConcurrentEditSameFileAllApplied 并发 edit 同一文件的不同行时，
// 每一处修改都必须落地 —— 读-改-写没有原子化时会丢修改。
func TestConcurrentEditSameFileAllApplied(t *testing.T) {
	dir := t.TempDir()
	pol := Policy{Root: dir, MaxReadBytes: 1 << 20}
	tool := NewEditTool(pol)

	const n = 12
	var init strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&init, "line %d\n", i)
	}
	path := filepath.Join(dir, "many.txt")
	if err := os.WriteFile(path, []byte(init.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := tool.Execute(context.Background(), Args{
				"path": "many.txt",
				"edits": []any{map[string]any{
					"oldText": fmt.Sprintf("line %d\n", i),
					"newText": fmt.Sprintf("LINE %d\n", i),
				}},
			})
			if err != nil {
				t.Errorf("edit %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for i := 0; i < n; i++ {
		if !strings.Contains(got, fmt.Sprintf("LINE %d\n", i)) {
			t.Fatalf("第 %d 处修改丢失，当前内容：\n%s", i, got)
		}
		if strings.Contains(got, fmt.Sprintf("line %d\n", i)) {
			t.Fatalf("第 %d 处原文还在，修改被覆盖：\n%s", i, got)
		}
	}
}

// TestExecutorParallelWritesDifferentFiles write 自带路径级互斥，应能与
// 其它调用并行调度（不再独占整批）。
func TestExecutorParallelWritesDifferentFiles(t *testing.T) {
	dir := t.TempDir()
	reg := NewRegistry()
	if err := reg.Register(NewWriteTool(Policy{Root: dir})); err != nil {
		t.Fatal(err)
	}
	tool, ok := reg.Get("write")
	if !ok || !IsPathScoped(tool) {
		t.Fatal("write 应实现 PathScoped")
	}

	e := NewExecutor(reg, 8)
	var mu sync.Mutex
	done := map[string]bool{}
	calls := []Call{
		{ID: "1", Name: "write", Args: Args{"path": "a.txt", "content": "a"}},
		{ID: "2", Name: "write", Args: Args{"path": "b.txt", "content": "b"}},
		{ID: "3", Name: "write", Args: Args{"path": "c.txt", "content": "c"}},
	}
	res := e.Execute(context.Background(), calls)
	for _, r := range res {
		if r.Err != nil {
			t.Fatalf("%s: %v", r.Call.ID, r.Err)
		}
		if r.Parallel != true {
			t.Fatalf("%s 没有走并行路径", r.Call.ID)
		}
		mu.Lock()
		done[r.Call.ID] = true
		mu.Unlock()
	}
	for _, f := range []string{"a.txt", "b.txt", "c.txt"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Fatalf("%s 未生成：%v", f, err)
		}
	}
}

// exclusiveTool 是「改动型但不自带路径锁」的对照工具：
// 即使 ConcurrencySafe 返回 true，也不该被并行调度。
type exclusiveTool struct{}

func (*exclusiveTool) Name() string               { return "excl" }
func (*exclusiveTool) Description() string        { return "test" }
func (*exclusiveTool) Parameters() map[string]any { return Schema(nil) }
func (*exclusiveTool) ConcurrencySafe() bool      { return true }
func (*exclusiveTool) Mutates() bool              { return true }
func (*exclusiveTool) Execute(context.Context, Args) (Result, error) {
	time.Sleep(5 * time.Millisecond)
	return TextResult("ok"), nil
}

// TestExecutorExclusiveMutationStillSerializes 保证放开 write/edit 不会顺带
// 放开所有改动型工具：没有 PathScoped 的仍然独占整批。
func TestExecutorExclusiveMutationStillSerializes(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Register(&exclusiveTool{}); err != nil {
		t.Fatal(err)
	}
	e := NewExecutor(reg, 8)
	res := e.Execute(context.Background(), []Call{
		{ID: "1", Name: "excl"}, {ID: "2", Name: "excl"},
	})
	for _, r := range res {
		if r.Parallel {
			t.Fatalf("%s 不应被并行调度", r.Call.ID)
		}
	}
}
