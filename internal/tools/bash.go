package tools

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// BashTool 执行 shell 命令。长时间命令可转到后台，再用 bash_output / bash_kill 管理。
type BashTool struct {
	Policy  Policy
	Manager *BashManager
}

// NewBashTool 创建 bash 工具（自带进程管理器）。
func NewBashTool(p Policy) *BashTool {
	return &BashTool{Policy: p, Manager: NewBashManager(p)}
}

func (*BashTool) Name() string { return "bash" }
func (*BashTool) Description() string {
	return "执行 shell 命令并返回 stdout/stderr 与退出码。" +
		"命令超过 timeout 会被转到后台并返回 sessionId（用 bash_output 读输出、bash_kill 终止）。" +
		"同一轮里多个只读命令（如 git status、ls）可并行调用。" + shellEngineHint()
}
func (*BashTool) Parameters() map[string]any {
	return Schema(map[string]any{
		"command":    Prop("string", "要执行的命令"),
		"cwd":        Prop("string", "工作目录，默认工作目录"),
		"timeout":    PropInt("前台最长等待毫秒数，默认 120000；超时转后台", 120000),
		"background": map[string]any{"type": "boolean", "description": "直接后台运行（服务/长任务）"},
		"env":        map[string]any{"type": "object", "description": "额外环境变量", "additionalProperties": map[string]any{"type": "string"}},
	}, "command")
}

// ConcurrencySafe 声明为 true：只读命令可以并行（如 git status 与 ls）。
// 需要独占的命令由模型自行串行（上游 Pi 同样按命令语义判断）。
func (*BashTool) ConcurrencySafe() bool { return true }

// PermissionMutates 把 bash 归为「权限层面的改动型工具」。
//
// shell 能写文件、跑 git、删目录，只读模式（readonly / --permissions readonly）
// 必须拦住它，否则门禁形同虚设。调度层仍保留 ConcurrencySafe=true：
// 多条只读命令并行是刻意保留的能力（见 PermissionMutation 的注释）。
func (*BashTool) PermissionMutates() bool { return true }

func (t *BashTool) Execute(ctx context.Context, a Args) (Result, error) {
	if err := CtxError(ctx); err != nil {
		return Result{}, err
	}
	cmd, err := a.Required("command")
	if err != nil {
		return ErrorResult("%v", err), nil
	}
	if err := t.Policy.CheckCommand(cmd); err != nil {
		return ErrorResult("%v", err), nil
	}
	dir := t.Policy.Root
	if c, ok := a.String("cwd"); ok && strings.TrimSpace(c) != "" {
		resolved, err := t.Policy.Resolve(c)
		if err != nil {
			return ErrorResult("%v", err), nil
		}
		dir = resolved
	}
	timeoutMs := a.Int("timeout", 120000)
	if timeoutMs <= 0 {
		timeoutMs = 120000
	}
	if timeoutMs > 600000 {
		timeoutMs = 600000
	}
	env := map[string]string{}
	if m, ok := a["env"].(map[string]any); ok {
		for k, v := range m {
			if s, ok := v.(string); ok {
				env[k] = s
			}
		}
	}
	background := a.Bool("background", false)

	if background {
		id, err := t.Manager.Start(cmd, dir, env)
		if err != nil {
			return ErrorResult("启动后台命令失败：%v", err), nil
		}
		return Result{
			Content: []Block{{Kind: KindText, Text: fmt.Sprintf("已转入后台，sessionId=%s\n用 bash_output 读取输出" +
				"（wait=\"exit\" 可直接等到结束，wait=\"output\" 等到有新输出），bash_kill 终止。", id)}},
			Details: map[string]any{"sessionId": id, "command": cmd, "background": true},
		}, nil
	}

	timeout := time.Duration(timeoutMs) * time.Millisecond
	start := time.Now()
	out, st, timedOut, live, err := runForeground(ctx, t.Policy, cmd, dir, env, timeout)
	if err != nil && out == "" {
		return ErrorResult("执行失败：%v", err), nil
	}
	if timedOut {
		// 关键：不重新执行命令，只把仍在跑的进程接管成后台会话。
		// 以前这里用 Manager.Start(cmd, dir, env) 重起一遍，非幂等命令（npm install、
		// git commit、>> 追加写）会被执行两次，而第一次还停在被 kill 的半截状态。
		var b strings.Builder
		b.WriteString(out)
		id := ""
		if live != nil {
			id = t.Manager.Attach(live)
		}
		if id == "" {
			b.WriteString(fmt.Sprintf("\n[前台等待超时 %s，命令已终止]", timeout))
			return Result{
				Content:   []Block{{Kind: KindText, Text: b.String()}},
				Truncated: true,
				Details:   map[string]any{"timedOut": true, "command": cmd},
			}, nil
		}
		b.WriteString(fmt.Sprintf("\n[前台等待超时 %s，命令仍在运行，已接管为后台 sessionId=%s，用 bash_output 继续读取]", timeout, id))
		res := Result{
			Content:   []Block{{Kind: KindText, Text: b.String()}},
			Truncated: true,
			Details:   map[string]any{"sessionId": id, "timedOut": true, "command": cmd},
		}
		return res, nil
	}

	text, truncated := Truncate(strings.TrimRight(out, "\n"), 0, 0)
	if text == "" {
		text = "(无输出)"
	}
	header := fmt.Sprintf("[exit %d, %s]", st, time.Since(start).Round(time.Millisecond))
	final := text + "\n" + header
	return Result{
		Content:   []Block{{Kind: KindText, Text: final}},
		Truncated: truncated,
		Details: map[string]any{
			"command": cmd, "cwd": t.Policy.Rel(dir),
			"exitCode":   st,
			"durationMs": time.Since(start).Milliseconds(),
			"truncated":  truncated,
		},
	}, nil
}

// killGrace 是 Kill 之后等待进程真死的时间。
//
// 纯内建脚本（`while true; do :; done`）无法被中断：mvdan/sh 没有中止脚本的 API，
// 取消只会作用到外面命令。没有这个上限的话，<-done 会永久阻塞，
// 把整次工具调用（以及整个 turn）挂死，而不只是泄露一个 goroutine。
const killGrace = 2 * time.Second

// maxForegroundOutput 是前台/接管输出的内存上限（超出部分丢弃并标记）。
const maxForegroundOutput = 4 << 20

// maxForegroundLines 是前台输出的**行数**预算。
//
// 4MB 的字节上限只管内存，不管 token：`ls -R` 几千行、每行几十字节，体积远
// 没到 4MB，但一次就能把上下文吃光（实测在 Temp 里误敲一次 ls -a，几万 token
// 直接进上下文）。所以再加一道按行的闸。
const maxForegroundLines = 2000

var nlByte = []byte{'\n'}

// headLines 返回 p 中前 n 行（含每行结尾的 \n）；不足 n 行时原样返回。
func headLines(p []byte, n int) []byte {
	idx := 0
	for i := 0; i < n; i++ {
		j := bytes.IndexByte(p[idx:], '\n')
		if j < 0 {
			return p
		}
		idx += j + 1
	}
	return p[:idx]
}

// sharedOutput 是可被前台与后台共享的输出缓冲。
// 前台超时接管时，写入端（解释器）已经在往它写，后台会话只是接管读取，
// 因此不会丢掉已经产生、但尚未被前台取走的输出。
type sharedOutput struct {
	mu    sync.Mutex
	b     bytes.Buffer
	limit int
	lines int  // 已缓冲的完整行数（用于行数预算）
	drop  bool // 字节上限被突破
	dropLines bool // 行数预算被突破
	// onWrite 在每次写入后调用（锁外），用来唤醒被接管会话的等待者。
	onWrite func()
}

func newSharedOutput(limit int) *sharedOutput {
	if limit <= 0 {
		limit = maxForegroundOutput
	}
	return &sharedOutput{limit: limit}
}

// setOnWrite 设置写入回调（Attach 时接上会话的 signal）。
func (o *sharedOutput) setOnWrite(fn func()) {
	o.mu.Lock()
	o.onWrite = fn
	o.mu.Unlock()
}

func (o *sharedOutput) Write(p []byte) (int, error) {
	orig := len(p)
	o.mu.Lock()
	hook := o.onWrite
	// 超过上限后仍然返回 len(p)：写端不能因为缓冲区满了而阻塞或报错。
	if o.drop || o.dropLines {
		o.mu.Unlock()
		if hook != nil {
			hook()
		}
		return orig, nil
	}
	if nl := bytes.Count(p, nlByte); o.lines+nl > maxForegroundLines {
		keep := maxForegroundLines - o.lines
		if keep > 0 {
			head := headLines(p, keep)
			o.b.Write(head)
			o.lines += keep
		}
		o.dropLines = true
		o.mu.Unlock()
		if hook != nil {
			hook()
		}
		return orig, nil
	}
	o.lines += bytes.Count(p, nlByte)
	if room := o.limit - o.b.Len(); room < len(p) {
		o.drop = true
		if room > 0 {
			o.b.Write(p[:room])
		}
		o.mu.Unlock()
		if hook != nil {
			hook()
		}
		return len(p), nil
	}
	n, err := o.b.Write(p)
	o.mu.Unlock()
	if hook != nil {
		hook()
	}
	return n, err
}

func (o *sharedOutput) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	s := o.b.String()
	s += o.truncNote()
	return s
}

// Slice 返回从 from 开始的剩余输出（增量读用；不重复拷贝整块）。
func (o *sharedOutput) Slice(from int) string {
	o.mu.Lock()
	defer o.mu.Unlock()
	b := o.b.Bytes()
	if from >= len(b) {
		return ""
	}
	out := string(b[from:])
	out += o.truncNote()
	return out
}

// truncNote 是截断说明（调用方必须持锁）。
func (o *sharedOutput) truncNote() string {
	if o.dropLines {
		return fmt.Sprintf("\n[输出超过 %d 行，已截断：请用 bash_output 增量读，或给命令加 head/grep 过滤]", maxForegroundLines)
	}
	if o.drop {
		return "\n[输出超过上限，已截断]"
	}
	return ""
}

// Len 返回已缓冲的字节数。
func (o *sharedOutput) Len() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.b.Len()
}

// waitResult 是进程结束的结果。
type waitResult struct {
	code int
	err  error
}

// liveProc 是「前台等超时了、但还在跑」的进程，可被 BashManager 接管。
type liveProc struct {
	proc   shellProc
	out    *sharedOutput
	done   chan waitResult
	cancel context.CancelFunc
}

// runForeground 在前台运行命令。超时不杀进程、也不重跑，
// 而是把仍在运行的进程（liveProc）交给调用方接管为后台会话。
func runForeground(ctx context.Context, policy Policy, command, dir string, env map[string]string, timeout time.Duration) (string, int, bool, *liveProc, error) {
	out := newSharedOutput(maxForegroundOutput)

	// 进程的生命周期不能绑在本次 turn 上：超时后被接管为后台会话时，
	// 调用方的 ctx 往往随 turn 结束而取消，那会把后台命令一起杀掉。
	procCtx, cancel := context.WithCancel(context.Background())

	p, err := newShellProcess(procCtx, command, dir, env, out, out, policy)
	if err != nil {
		cancel()
		return "", -1, false, nil, err
	}
	if err := p.Start(); err != nil {
		cancel()
		return "", -1, false, nil, err
	}

	done := make(chan waitResult, 1)
	go func() {
		code, werr := p.Wait()
		done <- waitResult{code, werr}
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case r := <-done:
		cancel()
		if r.err != nil {
			if ctx.Err() != nil {
				return out.String(), -1, false, nil, ctx.Err()
			}
			return out.String(), -1, false, nil, r.err
		}
		return out.String(), r.code, false, nil, nil

	case <-timer.C:
		// 不 Kill：写端继续跑，后台会话保留 cancel 以支持 bash_kill。
		return out.String(), -1, true, &liveProc{proc: p, out: out, done: done, cancel: cancel}, nil

	case <-ctx.Done():
		p.Kill()
		cancel()
		select {
		case <-done:
		case <-time.After(killGrace):
			// 内建死循环拦不住：放弃等待，别把整个 turn 挂在这里。
		}
		return out.String(), -1, false, nil, ctx.Err()
	}
}

// ---------------------------------------------------------------------------
// 后台进程管理
// ---------------------------------------------------------------------------

// bashSession 是一个后台命令会话。
type bashSession struct {
	id   string
	proc shellProc
	mu   sync.Mutex
	buf  bytes.Buffer
	// src 非 nil 表示本会话是「前台超时后被接管」的：
	// 输出由前台已经在用的 sharedOutput 提供，buf 不参与。
	src     *sharedOutput
	done    bool
	exit    int
	started time.Time

	// notify 是「有新输出 / 已结束 / 被 kill」的唤醒信号（容量 1，非阻塞发送）。
	// 等待方用 select 挂在它上面，而不是反复轮询 bash_output。
	notify chan struct{}
	// waiting 表示已经有一个调用方在等这个会话：第二个等待者直接拿快照返回，
	// 否则 parallel 里几个 bash_output 会一起挂在 executor 的槽位上。
	waiting bool
}

// signal 唤醒等待者（不阻塞）。
func (s *bashSession) signal() {
	if s.notify == nil {
		return
	}
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

// textLocked 返回当前已捕获的输出（调用方需持 s.mu）。
func (s *bashSession) textLocked() string {
	if s.src != nil {
		return s.src.String()
	}
	return s.buf.String()
}

// isPipeClosed 判断读端错误是否只是「我们自己把管道关了」造成的。
//
// Wait 之后仍读不完时（孙进程持着管道）会主动 Close 读端：默认解释器引擎下是
// io.ErrClosedPipe，os/exec 引擎下是 os.ErrClosed。这属于正常收尾，不算读失败。
func isPipeClosed(err error) bool {
	return errors.Is(err, io.ErrClosedPipe) || errors.Is(err, os.ErrClosed)
}

// sizeLocked 返回已捕获输出的字节数（调用方需持 s.mu）。
func (s *bashSession) sizeLocked() int {
	if s.src != nil {
		return s.src.Len()
	}
	return s.buf.Len()
}

// BashManager 管理后台进程。
type BashManager struct {
	mu       sync.Mutex
	sessions map[string]*bashSession
	seq      int
	maxKeep  int
	// policy 传给内建解释器（`goja` 等 Go 实现的内建命令按同一套 Root 约束读文件）。
	policy Policy
}

// NewBashManager 创建管理器。policy 决定内建命令能访问的文件范围。
func NewBashManager(p Policy) *BashManager {
	return &BashManager{sessions: make(map[string]*bashSession), maxKeep: 32, policy: p}
}

// Start 启动后台命令。
func (m *BashManager) Start(command, dir string, env map[string]string) (string, error) {
	// 后台命令用独立 context：请求结束时不能连带杀掉它们。
	p, err := newShellProcess(context.Background(), command, dir, env, nil, nil, m.policy)
	if err != nil {
		return "", err
	}
	pipe, err := p.Output()
	if err != nil {
		return "", err
	}
	if err := p.Start(); err != nil {
		return "", err
	}
	m.mu.Lock()
	m.seq++
	id := fmt.Sprintf("bash-%d", m.seq)
	s := &bashSession{id: id, proc: p, started: time.Now(), exit: -1, notify: make(chan struct{}, 1)}
	m.sessions[id] = s
	// 清理最旧的已完成会话
	if len(m.sessions) > m.maxKeep {
		m.pruneLocked()
	}
	m.mu.Unlock()

	go func() {
		// 读输出与等待结束分开：孙进程可能长期持着管道，不能让 done 一直拖着。
		readerDone := make(chan struct{})
		go func() {
			defer close(readerDone)
			sc := bufio.NewScanner(pipe)
			sc.Buffer(make([]byte, 64*1024), 1<<20)
			for sc.Scan() {
				s.mu.Lock()
				if s.buf.Len() < 4<<20 {
					s.buf.WriteString(sc.Text())
					s.buf.WriteByte('\n')
				}
				s.mu.Unlock()
				s.signal()
				time.Sleep(time.Millisecond)
			}
			// Scan 返回 false 既可能是读完（EOF），也可能是出错：单行超过 1MB 的
			// 缓冲上限（bufio.ErrTooLong）、管道读失败等。以前把两种情况都当读完，
			// 后半段输出静默丢弃，且没人再消费管道 —— 仍在写输出的进程会卡死，
			// 会话永远停在 running。
			if err := sc.Err(); err != nil && !isPipeClosed(err) {
				s.mu.Lock()
				if s.buf.Len() < 4<<20 {
					fmt.Fprintf(&s.buf, "[读取输出中断：%v]\n", err)
				}
				s.mu.Unlock()
				// 继续丢掉剩余输出：写端写不进去就会一直阻塞，进程也就永远不结束。
				_, _ = io.Copy(io.Discard, pipe)
			}
		}()

		code, werr := p.Wait()
		select {
		case <-readerDone:
		case <-time.After(200 * time.Millisecond):
			_ = pipe.Close()
			<-readerDone
		}

		s.mu.Lock()
		s.done = true
		if werr == nil && code >= 0 {
			s.exit = code
		}
		s.mu.Unlock()
		s.signal()
	}()
	return id, nil
}

// Attach 把一个仍在运行的前台进程接进后台会话，返回 sessionId。
//
// 这是「前台超时」的正确做法：不重新执行命令（非幂等命令会被跑两遍），
// 而是沿用同一个进程与同一块输出缓冲，前台已经产生、但尚未被读取的输出也不会丢。
func (m *BashManager) Attach(lp *liveProc) string {
	if lp == nil || lp.proc == nil {
		return ""
	}
	m.mu.Lock()
	m.seq++
	id := fmt.Sprintf("bash-%d", m.seq)
	s := &bashSession{id: id, proc: lp.proc, started: time.Now(), exit: -1, src: lp.out, notify: make(chan struct{}, 1)}
	m.sessions[id] = s
	// 前台的 stdout 已经在往 lp.out 里写：接上唤醒回调，否则接管的会话
	// 在等待方眼里永远是「没有新输出」。
	lp.out.setOnWrite(s.signal)
	if len(m.sessions) > m.maxKeep {
		m.pruneLocked()
	}
	m.mu.Unlock()

	go func() {
		r := <-lp.done
		s.mu.Lock()
		s.done = true
		if r.err == nil && r.code >= 0 {
			s.exit = r.code
		}
		s.mu.Unlock()
		s.signal()
		// 进程已退出，释放为它单独开的 context。
		lp.cancel()
	}()
	return id
}

func (m *BashManager) pruneLocked() {
	var oldest *bashSession
	for _, s := range m.sessions {
		if oldest == nil || s.started.Before(oldest.started) {
			oldest = s
		}
	}
	if oldest != nil {
		delete(m.sessions, oldest.id)
	}
}

// WaitUntil 说明要等到什么程度才返回。
type WaitUntil int

const (
	// WaitNone 立即返回当前快照（旧行为）。
	WaitNone WaitUntil = iota
	// WaitOutput 等到出现新输出（或进程结束 / 超时）。
	WaitOutput
	// WaitExit 等到进程结束（或超时）。
	WaitExit
)

// ReadResult 是一次读取（可能带等待）的结果。
type ReadResult struct {
	Text   string
	Next   int // 下次读取用的 cursor
	Done   bool
	Exit   int // -1 表示未知
	Waited time.Duration
	// TimedOut 表示等到了超时但条件仍未满足（进程还在跑）。
	TimedOut bool
	// Canceled 表示 ctx 被取消（用户 Esc / turn 中断）—— 结果里带的是取消瞬间的快照。
	Canceled bool
}

// Read 读取后台输出。
//
// cursor 是上次返回的 Next（0 = 从头读）；until != WaitNone 时会阻塞到
// 「有新输出 / 进程结束 / 超时 / ctx 取消」。
//
// 为什么要能等：模型之前只能反复调 bash_output 或前台 sleep 死等，一次
// 两分钟的任务要耗掉好几轮模型往返和重复的全量输出。
func (m *BashManager) Read(ctx context.Context, id string, cursor int, filter string, until WaitUntil, timeout time.Duration) (ReadResult, error) {
	m.mu.Lock()
	s, ok := m.sessions[id]
	m.mu.Unlock()
	if !ok {
		return ReadResult{}, fmt.Errorf("未知 sessionId %q（可能已被清理）", id)
	}
	if cursor < 0 {
		cursor = 0
	}

	// 同一个会话只允许一个等待者：第二个直接拿快照返回，否则 parallel 里
	// 几个 bash_output 会一起挂在 executor 的槽位上。
	onlySnapshot := until == WaitNone
	if !onlySnapshot {
		s.mu.Lock()
		if s.waiting {
			onlySnapshot = true
			until = WaitNone
		} else {
			s.waiting = true
		}
		s.mu.Unlock()
		if !onlySnapshot {
			defer func() {
				s.mu.Lock()
				s.waiting = false
				s.mu.Unlock()
			}()
		}
	}

	start := time.Now()
	var deadline <-chan time.Time
	if !onlySnapshot && timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		deadline = t.C
	}

	for {
		res, notify := s.snapshot(cursor, filter)
		res.Waited = time.Since(start)
		if res.Done || onlySnapshot {
			return res, nil
		}
		if until == WaitOutput && res.Text != "" {
			return res, nil
		}
		select {
		case <-notify:
			// 有新输出或状态变化：重新取快照。
		case <-ctx.Done():
			res, _ = s.snapshot(cursor, filter)
			res.Waited = time.Since(start)
			res.Canceled = true
			return res, nil
		case <-deadline:
			res, _ = s.snapshot(cursor, filter)
			res.Waited = time.Since(start)
			res.TimedOut = true
			return res, nil
		}
	}
}

// snapshot 取从 cursor 开始的输出与唤醒通道。
//
// Next 只推进到**最后一个完整行的末尾**：写了一半的行留在游标后面，下次
// 连同剩余部分一起发，既不重复也不丢。整块都没有换行时（进度条、超长
// 单行）照常推进，否则永远读不到。
func (s *bashSession) snapshot(cursor int, filter string) (ReadResult, <-chan struct{}) {
	s.mu.Lock()
	size := s.sizeLocked()
	if cursor > size {
		// 缓冲区被换过（理论上的兜底）：从头读。
		cursor = 0
	}
	chunk := s.sliceLocked(cursor)
	done, exit := s.done, s.exit
	notify := s.notify
	s.mu.Unlock()

	next := cursor + len(chunk)
	if i := strings.LastIndexByte(chunk, '\n'); i >= 0 {
		next = cursor + i + 1
	}
	text := applyLineFilter(chunk[:next-cursor], filter)
	return ReadResult{Text: text, Next: next, Done: done, Exit: exit}, notify
}

// sliceLocked 返回从 from 开始的剩余输出（调用方需持 s.mu）。
func (s *bashSession) sliceLocked(from int) string {
	if s.src != nil {
		return s.src.Slice(from)
	}
	b := s.buf.Bytes()
	if from >= len(b) {
		return ""
	}
	return string(b[from:])
}

// applyLineFilter 按正则只保留匹配的行；filter 为空时原样返回。
func applyLineFilter(text, filter string) string {
	if filter == "" || text == "" {
		return text
	}
	re, err := regexp.Compile(filter)
	if err != nil {
		return text
	}
	var b strings.Builder
	for _, ln := range strings.Split(text, "\n") {
		if re.MatchString(ln) {
			b.WriteString(ln)
			b.WriteString("\n")
		}
	}
	return b.String()
}

// Kill 终止后台会话。
func (m *BashManager) Kill(id string) error {
	m.mu.Lock()
	s, ok := m.sessions[id]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("未知 sessionId %q", id)
	}
	s.proc.Kill()
	s.signal()
	return nil
}

// KillAll 终止所有后台会话并清空管理器。
//
// 会话关闭时调用。以前 bash manager 是进程级单例，后台进程跟着 agent 进程
// 一起被 OS 收掉；按会话隔离后每个会话有自己的 manager，不主动清就会一直
// 挂到 agent 退出 —— 对长期开着的 ACP 客户端是明确的泄漏。
func (m *BashManager) KillAll() {
	if m == nil {
		return
	}
	m.mu.Lock()
	list := make([]*bashSession, 0, len(m.sessions))
	for _, s := range m.sessions {
		list = append(list, s)
	}
	m.sessions = make(map[string]*bashSession)
	m.mu.Unlock()
	for _, s := range list {
		s.proc.Kill()
	}
}

// List 列出后台会话。
func (m *BashManager) List() []map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]map[string]any, 0, len(m.sessions))
	for _, s := range m.sessions {
		s.mu.Lock()
		out = append(out, map[string]any{
			"id": s.id, "done": s.done, "exitCode": s.exit,
			"bytes": s.sizeLocked(), "elapsedMs": time.Since(s.started).Milliseconds(),
		})
		s.mu.Unlock()
	}
	return out
}

// ---------------------------------------------------------------------------
// bash_output / bash_kill
// ---------------------------------------------------------------------------

// BashOutputTool 读取后台输出。
type BashOutputTool struct{ Manager *BashManager }

func NewBashOutputTool(m *BashManager) *BashOutputTool { return &BashOutputTool{Manager: m} }

func (*BashOutputTool) Name() string { return "bash_output" }
func (*BashOutputTool) Description() string {
	return "读取后台命令的输出。默认立即返回当前快照；wait=output/exit 会阻塞到" +
		"有新输出/命令结束（比反复轮询或前台 sleep 省 token）。" +
		"配合 cursor 只取上次之后的新输出。"
}
func (*BashOutputTool) Parameters() map[string]any {
	return Schema(map[string]any{
		"sessionId": Prop("string", "bash 返回的 sessionId"),
		"filter":    Prop("string", "可选：只返回匹配该正则的行"),
		"cursor":    PropInt("从上次返回的 [cursor N] 之后开始读；0 = 从头", 0),
		"wait": Prop("string", "none（默认，立即返回）| output（等到有新输出）| "+
			"exit（等到命令结束）"),
		"timeoutMs": PropInt("等待超时毫秒（output 默认 5000，exit 默认 60000，上限 300000）", 0),
	}, "sessionId")
}
func (*BashOutputTool) ConcurrencySafe() bool { return true }
func (*BashOutputTool) Mutates() bool         { return false }

func (t *BashOutputTool) Execute(ctx context.Context, a Args) (Result, error) {
	id, err := a.Required("sessionId")
	if err != nil {
		return ErrorResult("%v", err), nil
	}
	until, err := parseWaitUntil(a.Str("wait", ""))
	if err != nil {
		return ErrorResult("%v", err), nil
	}
	timeoutMs := a.Int("timeoutMs", 0)
	if timeoutMs <= 0 {
		switch until {
		case WaitOutput:
			timeoutMs = 5000
		case WaitExit:
			timeoutMs = 60000
		}
	}
	if timeoutMs > maxWaitTimeoutMs {
		timeoutMs = maxWaitTimeoutMs
	}

	res, err := t.Manager.Read(ctx, id, int(a.Int("cursor", 0)), a.Str("filter", ""),
		until, time.Duration(timeoutMs)*time.Millisecond)
	if err != nil {
		return ErrorResult("%v", err), nil
	}

	raw := strings.TrimRight(res.Text, "\n")
	// 后台缓冲能到 4MB，一次全量读出来同样能炸上下文（比如 sleep 后的目录树）。
	lineNote := ""
	if nl := strings.Count(raw, "\n") + 1; nl > maxForegroundLines {
		raw = string(headLines([]byte(raw), maxForegroundLines))
		lineNote = fmt.Sprintf("\n[本次只返回前 %d 行（共 %d 行）：用 cursor 继续读，或加 filter 过滤]",
			maxForegroundLines, nl)
	}
	text, truncated := Truncate(raw, 0, 0)
	if lineNote != "" {
		text = strings.TrimRight(text, "\n") + lineNote
		truncated = true
	}
	if text == "" {
		switch {
		case res.Canceled:
			text = "(等待被取消，暂无新输出)"
		case res.TimedOut:
			text = "(等待超时，暂无新输出)"
		default:
			text = "(暂无输出)"
		}
	}
	// 状态行保持旧格式 [running] / [done(exit N)]，只在需要时加后缀，
	// 免得既有的调用方（和模型）重新学一遍。
	status := "running"
	switch {
	case res.Done:
		status = fmt.Sprintf("done(exit %d)", res.Exit)
	case res.Canceled:
		status = "running, 等待已取消"
	case res.TimedOut:
		status = "running, 等待超时"
	}
	final := fmt.Sprintf("%s [%s] [cursor %d]", text, status, res.Next)
	return Result{
		Content:   []Block{{Kind: KindText, Text: final}},
		Truncated: truncated,
		Details: map[string]any{
			"sessionId": id, "done": res.Done, "exitCode": res.Exit,
			"cursor": res.Next, "timedOut": res.TimedOut, "canceled": res.Canceled,
		},
	}, nil
}

// maxWaitTimeoutMs 是 bash_output 等待的上限：无限等会让 turn 永远不结束，
// 用户只能 Ctrl-C 才能拿回控制权。
const maxWaitTimeoutMs = 300000

// parseWaitUntil 解析 wait 参数。空串等于 none（保持老行为）。
func parseWaitUntil(v string) (WaitUntil, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "none", "false", "no":
		return WaitNone, nil
	case "output", "new", "any":
		return WaitOutput, nil
	case "exit", "done", "finish":
		return WaitExit, nil
	}
	return WaitNone, fmt.Errorf("wait 只支持 none / output / exit，收到 %q", v)
}

// BashKillTool 终止后台命令。
type BashKillTool struct{ Manager *BashManager }

func NewBashKillTool(m *BashManager) *BashKillTool { return &BashKillTool{Manager: m} }

func (*BashKillTool) Name() string { return "bash_kill" }
func (*BashKillTool) Description() string {
	return "终止一个后台命令（bash 转入后台或 background=true 启动的）。"
}
func (*BashKillTool) Parameters() map[string]any {
	return Schema(map[string]any{
		"sessionId": Prop("string", "要终止的 sessionId"),
	}, "sessionId")
}
func (*BashKillTool) ConcurrencySafe() bool { return false }
func (*BashKillTool) Mutates() bool         { return true }

func (t *BashKillTool) Execute(ctx context.Context, a Args) (Result, error) {
	id, err := a.Required("sessionId")
	if err != nil {
		return ErrorResult("%v", err), nil
	}
	if err := t.Manager.Kill(id); err != nil {
		return ErrorResult("%v", err), nil
	}
	return Result{
		Content: []Block{{Kind: KindText, Text: fmt.Sprintf("已终止 %s", id)}},
		Details: map[string]any{"sessionId": id},
	}, nil
}
