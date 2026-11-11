package tools

// 后台子代理任务管理器。
//
// 设计对齐 BashManager：delegate 不再阻塞主代理的 turn —— 它在后台起任务，
// 立刻返回 jobId；主代理继续干活，需要结果时再用 subagent_output 取
// （wait=exit 可以等到结束，比反复轮询省 token），用 subagent_kill 终止。
//
// 两个关键点：
//  1. **生命周期与 turn 解耦**。任务跑在独立 ctx 上（context.WithCancel(
//     context.Background())），所以「这一轮结束 / 用户按 Esc」不会连带杀掉
//     子代理；只有 Kill、会话关闭（KillAll）或进程退出才回收。
//  2. **结果不丢**。任务结束后结论文本留在 job 里，主代理任意后续轮次都能
//     取到；结论文本只在渲染时截断，不进上下文就不会污染。

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// ErrSubagentNotFound 表示没有该 id 的后台子代理任务。
var ErrSubagentNotFound = errors.New("tools: 找不到该子代理任务（id 可能已过期或被回收）")

// SubagentJob 是一个后台子代理任务的快照。
type SubagentJob struct {
	ID   string `json:"id"`
	Role string `json:"role"`
	Task string `json:"task"`
	// Running 为 true 表示仍在跑；为 false 时看 Cancelled / Err / Result 判断结局。
	Running bool `json:"running"`
	// Err 是失败原因（空串表示成功）。
	Err string `json:"err,omitempty"`
	// Cancelled 表示是被 subagent_kill 主动终止的。
	Cancelled bool `json:"cancelled,omitempty"`
	// Result 是子代理的结论文本（成功时有意义）。
	Result   string    `json:"result,omitempty"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished,omitempty"`
}

// Elapsed 返回已耗时（运行中按当前时间算）。
func (j SubagentJob) Elapsed() time.Duration {
	if j.Finished.IsZero() {
		return time.Since(j.Started)
	}
	return j.Finished.Sub(j.Started)
}

// subagentJob 是内部状态：快照 + 唤醒信号 + 取消。
type subagentJob struct {
	SubagentJob
	done   chan struct{}
	cancel context.CancelFunc
}

// SubagentManager 管理后台子代理任务。并发安全。
type SubagentManager struct {
	runner SubagentRunner

	mu      sync.Mutex
	seq     int
	jobs    map[string]*subagentJob
	maxKeep int
}

// maxSubagentResult 是单个任务保留的结论文本上限。
//
// 4MB 和 bash 的后台缓冲一个量级：子代理偶尔会把整份文件贴回来，
// 不设上限就是无界内存；渲染给模型时还会再截到 24KB。
const maxSubagentResult = 4 << 20

// NewSubagentManager 创建管理器。runner 为 nil 时 Start 会直接报错。
func NewSubagentManager(r SubagentRunner) *SubagentManager {
	return &SubagentManager{runner: r, jobs: make(map[string]*subagentJob), maxKeep: 32}
}

// Start 起一个后台子代理任务并立刻返回快照。
func (m *SubagentManager) Start(role, task string) (SubagentJob, error) {
	if m == nil || m.runner == nil {
		return SubagentJob{}, errors.New("子代理未配置 Runner（宿主需注入 tools.SubagentRunner）")
	}
	// 关键：不要用调用方的 ctx —— 那是「这一轮」的 ctx，turn 结束就取消，
	// 子代理会被连坐杀掉。后台任务的回收只能靠 Kill / KillAll。
	ctx, cancel := context.WithCancel(context.Background())

	m.mu.Lock()
	m.seq++
	j := &subagentJob{
		SubagentJob: SubagentJob{
			ID:      fmt.Sprintf("sub-%d", m.seq),
			Role:    role,
			Task:    task,
			Running: true,
			Started: time.Now(),
		},
		done:   make(chan struct{}),
		cancel: cancel,
	}
	m.jobs[j.ID] = j
	m.pruneLocked()
	snap := j.SubagentJob
	m.mu.Unlock()

	go func() {
		text, err := m.runner(ctx, role, task)

		m.mu.Lock()
		defer m.mu.Unlock()
		j.Running = false
		j.Finished = time.Now()
		j.Cancelled = ctx.Err() != nil
		if err != nil && !j.Cancelled {
			j.Err = err.Error()
		}
		if len(text) > maxSubagentResult {
			text = text[:maxSubagentResult] + "\n…（子代理结论过长，已截断）"
		}
		j.Result = text
		cancel()
		close(j.done)
	}()
	return snap, nil
}

// Get 返回快照。
func (m *SubagentManager) Get(id string) (SubagentJob, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return SubagentJob{}, false
	}
	return j.SubagentJob, true
}

// List 返回全部任务（按 id 升序，便于稳定展示）。
func (m *SubagentManager) List() []SubagentJob {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]SubagentJob, 0, len(m.jobs))
	for _, j := range m.jobs {
		out = append(out, j.SubagentJob)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out
}

// Wait 读取任务状态，until != WaitNone 时最多阻塞 timeout。
//
// 子代理没有流式输出（过程行直接打到主视图），所以 WaitOutput 与 WaitExit
// 等价：都等到结束。返回的 bool 表示是否等到了结束。
func (m *SubagentManager) Wait(ctx context.Context, id string, until WaitUntil, timeout time.Duration) (SubagentJob, bool, error) {
	m.mu.Lock()
	j, ok := m.jobs[id]
	if !ok {
		m.mu.Unlock()
		return SubagentJob{}, false, fmt.Errorf("%w: %s", ErrSubagentNotFound, id)
	}
	snap := j.SubagentJob
	m.mu.Unlock()

	if until == WaitNone || !snap.Running {
		return snap, !snap.Running, nil
	}
	if timeout <= 0 {
		timeout = defaultSubagentWait
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-j.done:
		got, _ := m.Get(id)
		return got, true, nil
	case <-timer.C:
		got, _ := m.Get(id)
		return got, false, nil
	case <-ctx.Done():
		got, _ := m.Get(id)
		return got, false, ctx.Err()
	}
}

// defaultSubagentWait 是 wait=exit 的默认等待上限。
const defaultSubagentWait = 120 * time.Second

// Kill 终止一个仍在跑的任务（已结束的返回 nil）。
func (m *SubagentManager) Kill(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrSubagentNotFound, id)
	}
	if j.Running {
		j.Cancelled = true
		j.cancel()
	}
	return nil
}

// KillAll 终止所有任务（会话关闭时调用）。
func (m *SubagentManager) KillAll() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, j := range m.jobs {
		if j.Running {
			j.Cancelled = true
			j.cancel()
		}
	}
}

// pruneLocked 回收最旧的已完成任务，避免长会话越攒越多。运行中的永不回收。
func (m *SubagentManager) pruneLocked() {
	if len(m.jobs) <= m.maxKeep {
		return
	}
	finished := make([]*subagentJob, 0, len(m.jobs))
	for _, j := range m.jobs {
		if !j.Running {
			finished = append(finished, j)
		}
	}
	sort.Slice(finished, func(a, b int) bool { return finished[a].Finished.Before(finished[b].Finished) })
	for _, j := range finished {
		if len(m.jobs) <= m.maxKeep {
			return
		}
		delete(m.jobs, j.ID)
	}
}

// renderSubagentJob 把任务快照渲染成给模型看的文本。
func renderSubagentJob(j SubagentJob, maxChars int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s [%s] %s · %s\n", j.ID, j.Role, statusText(j), fmtDuration(j.Elapsed()))
	switch {
	case j.Running:
		b.WriteString("（仍在运行，用 subagent_output wait=exit 等待，或 subagent_kill 终止）\n")
	case j.Cancelled:
		b.WriteString("（已被终止）\n")
	case j.Err != "":
		fmt.Fprintf(&b, "失败: %s\n", j.Err)
	default:
		body := strings.TrimSpace(j.Result)
		if body == "" {
			body = "（子代理没有返回任何文本）"
		}
		body, _ = Truncate(body, 0, maxChars)
		b.WriteString(body)
		b.WriteString("\n")
	}
	return b.String()
}

// statusText 是状态短标签（与 bash_output 的风格一致）。
func statusText(j SubagentJob) string {
	switch {
	case j.Running:
		return "running"
	case j.Cancelled:
		return "killed"
	case j.Err != "":
		return "failed"
	default:
		return "done"
	}
}

// fmtDuration 把耗时写成 3s / 1m05s。
func fmtDuration(d time.Duration) string {
	d = d.Round(100 * time.Millisecond)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}
