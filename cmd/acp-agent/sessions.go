package main

import (
	"bufio"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/leijux/acp-agent/internal/agent"
)

// sessionStore 把会话文件按工作目录分区存放。
//
// 为什么要分区：会话里全是「在这个项目里干了什么」的上下文，混在一个平铺目录里，
// `-c`（继续最近一次）会把 A 项目的对话接到 B 项目的请求上。
//
// 布局：
//
//	<DataDir>/sessions/<sha1(root)[:12]>/.cwd          # 原始路径，方便人肉排查
//	<DataDir>/sessions/<sha1(root)[:12]>/session-*.jsonl
//
// 升级前的平铺文件（<DataDir>/sessions/session-*.jsonl）会在本目录没有会话时
// 作为回退来源，保证老用户 -c 仍能接上。
type sessionStore struct {
	root string // 已绝对化的工作目录
}

func newSessionStore(root string) sessionStore {
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	return sessionStore{root: filepath.Clean(root)}
}

// key 把工作目录映射成稳定的 12 位十六进制目录名。
// 不用路径本身做目录名：Windows 路径可能超过 260 字符，且会泄漏用户名。
func (s sessionStore) key() string {
	p := s.root
	if runtime.GOOS == "windows" {
		p = strings.ToLower(p) // D:\Proj 与 d:\proj 是同一个目录
	}
	sum := sha1.Sum([]byte(p))
	return hex.EncodeToString(sum[:])[:12]
}

// legacyDir 是升级前的共享会话目录。
func legacyDir() string { return filepath.Join(DataDir(), "sessions") }

// dir 返回本项目专属的会话目录。
func (s sessionStore) dir() string { return filepath.Join(legacyDir(), s.key()) }

// newPath 分配一个新的会话文件路径，并确保目录与 .cwd 标记存在。
func (s sessionStore) newPath() string {
	dir := s.dir()
	_ = os.MkdirAll(dir, 0o755)
	// 记录原始路径，纯给人看：反查哈希目录属于哪个项目。
	cwdFile := filepath.Join(dir, ".cwd")
	if _, err := os.Stat(cwdFile); err != nil {
		_ = os.WriteFile(cwdFile, []byte(s.root+"\n"), 0o644)
	}
	return filepath.Join(dir, "session-"+time.Now().Format("20060102-150405.000000000")+".jsonl")
}

// latest 找本项目最近修改的会话文件；本目录为空时回退到旧共享目录。
func (s sessionStore) latest() string {
	if p := latestIn(s.dir()); p != "" {
		return p
	}
	return latestIn(legacyDir())
}

// latestIn 返回 dir 下（不含子目录）最近修改的 .jsonl 文件。
func latestIn(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var best string
	var bestTime time.Time
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(bestTime) {
			bestTime = info.ModTime()
			best = filepath.Join(dir, e.Name())
		}
	}
	return best
}

// ---------- 列表与选择（-r） ----------

// maxSessionList 是列表条数上限：会话目录可能攒了几百个，
// 一次全渲染会把真正要的那些顶出屏幕，逐个读 JSONL 也白花时间。
const maxSessionList = 20

// sessionInfo 是会话列表里的一行（-r 的选择菜单用）。
type sessionInfo struct {
	Path     string
	ModTime  time.Time
	Bytes    int64
	Messages int
	// First 是首条用户消息的首行 60 字，用来帮人认出「哪个才是要的那个」。
	First string
	// Err 非空表示文件在但解析不了：仍然列出来，让人知道它坏了而不是凭空消失。
	Err error
}

// summary 是一行「几条消息 / 开头是什么 / 坏了」。
func (si sessionInfo) summary() string {
	if si.Err != nil {
		return "（无法解析: " + firstLine(si.Err.Error(), 40) + "）"
	}
	s := fmt.Sprintf("%d 条", si.Messages)
	if si.First != "" {
		s += "  " + si.First
	}
	return s
}

// list 列出本项目的历史会话（最近修改在前）；本目录为空时回退旧共享目录。
//
// 与 latest 同一套回退规则：升级前的平铺文件不能因为新旧目录都存在就消失。
func (s sessionStore) list() []sessionInfo {
	if infos := listIn(s.dir()); len(infos) > 0 {
		return infos
	}
	return listIn(legacyDir())
}

func listIn(dir string) []sessionInfo {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	infos := make([]sessionInfo, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		infos = append(infos, sessionInfo{
			Path:    filepath.Join(dir, e.Name()),
			ModTime: info.ModTime(),
			Bytes:   info.Size(),
		})
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].ModTime.After(infos[j].ModTime) })
	if len(infos) > maxSessionList {
		infos = infos[:maxSessionList]
	}
	for i := range infos {
		describeSession(&infos[i])
	}
	return infos
}

// describeSession 补上「几条消息 / 开头是什么」。
//
// 解析失败不算致命：标成损坏照旧列出（选中后仍然会走 loadOrResetSession
// 那套备份 + 新建）——静默跳过反而让人以为会话丢了。
func describeSession(si *sessionInfo) {
	sess, err := agent.LoadSession(si.Path)
	if err != nil {
		si.Err = err
		return
	}
	msgs := sess.Messages()
	si.Messages = len(msgs)
	for _, m := range msgs {
		if _, ok := m.(*agent.UserMessage); ok {
			si.First = firstLine(agent.TextOf(m), 60)
			break
		}
	}
}

// resolveResume 把 -r 的选择器解析成一个会话文件路径。
//
//	"2"    → 本项目第 2 条（1 = 最近，与列表序号一致）
//	"path" → 原样返回，之后走 --session 那套加载（文件不存在就新建）
//	""     → 打印列表后从 in 读一行：回车 = 最近一条，q = 不继续（返回空 = 新会话）
//
// in 为 nil 表示没有交互通道（-p / --acp / --mode rpc、stdin 不是终端）：
// 这种情况只能报错，不能去抢 stdin。
func resolveResume(sel string, store sessionStore, in *bufio.Reader, out io.Writer) (string, error) {
	if trimmed := strings.TrimSpace(sel); trimmed != "" {
		if n, err := strconv.Atoi(trimmed); err == nil {
			infos := store.list()
			if n < 1 || n > len(infos) {
				return "", fmt.Errorf("-r %s 超出范围（本项目有 %d 个会话，1 = 最近）", trimmed, len(infos))
			}
			return infos[n-1].Path, nil
		}
		return sel, nil
	}
	if in == nil {
		return "", errors.New("裸 -r 要交互终端才能选：请给编号（-r 2）或路径（-r ./x.jsonl）")
	}

	infos := store.list()
	if len(infos) == 0 {
		fmt.Fprintf(out, "本项目还没有历史会话，直接开新会话\n")
		return "", nil
	}
	fmt.Fprintf(out, "本项目的历史会话（%d 个，1 = 最近）：\n", len(infos))
	for i, si := range infos {
		fmt.Fprintf(out, "  %2d  %s  %s\n", i+1, si.ModTime.Format("2006-01-02 15:04"), si.summary())
	}
	fmt.Fprintf(out, "选择编号 [1]（q 取消）: ")

	line, err := in.ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("读取选择失败: %w", err)
	}
	switch line = strings.TrimSpace(line); line {
	case "q", "Q":
		fmt.Fprintln(out, "已取消，开新会话")
		return "", nil
	case "":
		return infos[0].Path, nil
	}
	n, cerr := strconv.Atoi(line)
	if cerr != nil || n < 1 || n > len(infos) {
		return "", fmt.Errorf("无效的选择 %q（1..%d，或 q 取消）", line, len(infos))
	}
	return infos[n-1].Path, nil
}
