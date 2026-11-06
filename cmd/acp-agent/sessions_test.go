package main

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/leijux/acp-agent/internal/agent"
)

// useTempData 把 DataDir 指向临时目录，避免污染真实会话。
func useTempData(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("ACP_AGENT_DATA", dir)
	return dir
}

func TestSessionStoreIsolatesByRoot(t *testing.T) {
	useTempData(t)
	a := newSessionStore(filepath.Join(t.TempDir(), "projA"))
	b := newSessionStore(filepath.Join(t.TempDir(), "projB"))

	if a.key() == b.key() {
		t.Fatalf("不同目录得到相同 key: %s", a.key())
	}
	if a.key() == newSessionStore("/definitely/other").key() {
		t.Fatal("key 冲突")
	}

	pa, pb := a.newPath(), b.newPath()
	if filepath.Dir(pa) == filepath.Dir(pb) {
		t.Fatalf("两个项目写进了同一个目录: %s", filepath.Dir(pa))
	}
	if !strings.HasSuffix(pa, ".jsonl") {
		t.Fatalf("会话文件后缀不对: %s", pa)
	}
}

func TestSessionStoreWritesCwdMarker(t *testing.T) {
	useTempData(t)
	root := filepath.Join(t.TempDir(), "myproject")
	s := newSessionStore(root)
	s.newPath()

	b, err := os.ReadFile(filepath.Join(s.dir(), ".cwd"))
	if err != nil {
		t.Fatalf("读 .cwd: %v", err)
	}
	if strings.TrimSpace(string(b)) != s.root {
		t.Fatalf(".cwd = %q, 期望 %q", strings.TrimSpace(string(b)), s.root)
	}
}

func TestSessionStoreNormalizesTrailingSeparator(t *testing.T) {
	useTempData(t)
	dir := t.TempDir()
	a := newSessionStore(dir)
	b := newSessionStore(dir + string(filepath.Separator))
	if a.key() != b.key() {
		t.Fatalf("尾部分隔符未归一: %s vs %s", a.key(), b.key())
	}
}

func TestSessionStoreKeyCaseFoldedOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("仅 Windows 大小写不敏感")
	}
	useTempData(t)
	if newSessionStore(`D:\Proj`).key() != newSessionStore(`d:\proj`).key() {
		t.Fatal("Windows 上大小写不同应视为同一目录")
	}
}

// latest 只能看到本项目的会话，不能被别的目录「串味」。
func TestSessionStoreLatestIsScoped(t *testing.T) {
	useTempData(t)
	dirA, dirB := filepath.Join(t.TempDir(), "a"), filepath.Join(t.TempDir(), "b")
	a, b := newSessionStore(dirA), newSessionStore(dirB)

	pa := a.newPath()
	if err := os.WriteFile(pa, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// B 的会话更新 -> 不能影响 A 的 latest
	pb := b.newPath()
	if err := os.WriteFile(pb, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(pa, old, old); err != nil {
		t.Fatal(err)
	}

	if got := a.latest(); got != pa {
		t.Fatalf("a.latest() = %q, 期望 %q", got, pa)
	}
	if got := b.latest(); got != pb {
		t.Fatalf("b.latest() = %q, 期望 %q", got, pb)
	}
	// 第三个目录从没建过会话，也不该看到别人的
	if got := newSessionStore(filepath.Join(t.TempDir(), "c")).latest(); got != "" {
		t.Fatalf("空项目 latest() = %q, 期望空", got)
	}
}

func TestSessionStoreLatestPicksNewestInDir(t *testing.T) {
	useTempData(t)
	s := newSessionStore(filepath.Join(t.TempDir(), "proj"))
	names := []string{"session-old.jsonl", "session-new.jsonl"}
	times := []time.Time{time.Now().Add(-time.Hour), time.Now()}
	for i, n := range names {
		p := filepath.Join(s.dir(), n)
		if err := os.MkdirAll(s.dir(), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, times[i], times[i]); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := s.latest(), filepath.Join(s.dir(), names[1]); got != want {
		t.Fatalf("latest() = %q, 期望 %q", got, want)
	}
}

// 升级兼容：旧版本把会话平铺在 sessions/ 下，本目录为空时应能接上。
func TestSessionStoreFallsBackToLegacyLayout(t *testing.T) {
	useTempData(t)
	legacy := legacyDir()
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(legacy, "session-1792923310601741500.jsonl")
	if err := os.WriteFile(old, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := newSessionStore(filepath.Join(t.TempDir(), "newproj"))
	if got := s.latest(); got != old {
		t.Fatalf("未回退到旧布局: got %q want %q", got, old)
	}

	// 本目录有了自己的会话后，就不再回退。
	fresh := s.newPath()
	if err := os.WriteFile(fresh, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := s.latest(); got != fresh {
		t.Fatalf("新布局存在时仍回退旧布局: got %q", got)
	}
}

func TestSessionStoreIgnoresSubdirectories(t *testing.T) {
	useTempData(t)
	s := newSessionStore(filepath.Join(t.TempDir(), "proj"))
	if err := os.MkdirAll(filepath.Join(legacyDir(), "deadbeef1234"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := s.latest(); got != "" {
		t.Fatalf("子目录被当成会话文件: %q", got)
	}
}

// writeSession 落一个真会话文件（内容是若干条用户消息）。
func writeSession(t *testing.T, path string, texts ...string) {
	t.Helper()
	sess := agent.NewSession()
	for _, txt := range texts {
		if _, err := sess.Append(agent.TextMessage(txt)); err != nil {
			t.Fatal(err)
		}
	}
	if err := sess.Save(path); err != nil {
		t.Fatal(err)
	}
}

// twoSessions 在本项目目录里放两个会话，返回 (较新的, 较旧的)。
func twoSessions(t *testing.T, s sessionStore) (newest, older string) {
	t.Helper()
	newest = s.newPath()
	writeSession(t, newest, "先看看 go.mod")
	older = s.newPath()
	writeSession(t, older, "跑一下测试", "再修一下失败的那个")
	if err := os.Chtimes(older, time.Now().Add(-time.Hour), time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	return newest, older
}

// list 必须最近修改在前，并带上「几条消息 / 首条提问」，否则选择菜单没用。
func TestSessionStoreListNewestFirst(t *testing.T) {
	useTempData(t)
	s := newSessionStore(filepath.Join(t.TempDir(), "proj"))
	newest, older := twoSessions(t, s)

	infos := s.list()
	if len(infos) != 2 {
		t.Fatalf("list 返回 %d 条，期望 2", len(infos))
	}
	if infos[0].Path != newest || infos[1].Path != older {
		t.Fatalf("排序不是最近优先: %q, %q", infos[0].Path, infos[1].Path)
	}
	if infos[0].Messages != 1 || infos[1].Messages != 2 {
		t.Fatalf("消息数不对: %d, %d", infos[0].Messages, infos[1].Messages)
	}
	if got := infos[0].First; got != "先看看 go.mod" {
		t.Fatalf("首条提问 = %q", got)
	}
	if infos[0].Err != nil {
		t.Fatalf("正常会话被标成损坏: %v", infos[0].Err)
	}
}

// 坏文件仍然列出（标成损坏），不能静默消失。
func TestSessionStoreListMarksCorrupt(t *testing.T) {
	useTempData(t)
	s := newSessionStore(filepath.Join(t.TempDir(), "proj"))
	if err := os.MkdirAll(s.dir(), 0o755); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(s.dir(), "session-broken.jsonl")
	if err := os.WriteFile(bad, []byte("这不是 JSON\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	infos := s.list()
	if len(infos) != 1 {
		t.Fatalf("list 返回 %d 条，期望 1", len(infos))
	}
	if infos[0].Err == nil {
		t.Fatal("坏文件没有被标成损坏")
	}
	if !strings.Contains(infos[0].summary(), "无法解析") {
		t.Fatalf("摘要没提示损坏: %q", infos[0].summary())
	}
}

func TestResolveResumeByIndex(t *testing.T) {
	useTempData(t)
	s := newSessionStore(filepath.Join(t.TempDir(), "proj"))
	newest, older := twoSessions(t, s)

	if got, err := resolveResume("1", s, nil, &bytes.Buffer{}); err != nil || got != newest {
		t.Fatalf("-r 1 = %q, %v；期望 %q", got, err, newest)
	}
	if got, err := resolveResume(" 2 ", s, nil, &bytes.Buffer{}); err != nil || got != older {
		t.Fatalf("-r 2 = %q, %v；期望 %q", got, err, older)
	}
}

func TestResolveResumeOutOfRange(t *testing.T) {
	useTempData(t)
	s := newSessionStore(filepath.Join(t.TempDir(), "proj"))
	twoSessions(t, s)
	for _, sel := range []string{"0", "3", "-1"} {
		if _, err := resolveResume(sel, s, nil, &bytes.Buffer{}); err == nil {
			t.Fatalf("-r %s 应该报错", sel)
		}
	}
}

// 非数字的选择器原样当路径：与 --session 走同一条加载逻辑。
func TestResolveResumePathPassthrough(t *testing.T) {
	useTempData(t)
	s := newSessionStore(filepath.Join(t.TempDir(), "proj"))
	p := filepath.Join(t.TempDir(), "x.jsonl")
	if got, err := resolveResume(p, s, nil, &bytes.Buffer{}); err != nil || got != p {
		t.Fatalf("路径直传 = %q, %v", got, err)
	}
}

// 裸 -r 没有交互通道时只能报错：绝不能去抢 stdin。
func TestResolveResumeBareNeedsTerminal(t *testing.T) {
	useTempData(t)
	s := newSessionStore(filepath.Join(t.TempDir(), "proj"))
	twoSessions(t, s)
	if _, err := resolveResume("", s, nil, &bytes.Buffer{}); err == nil {
		t.Fatal("裸 -r 在无交互通道时应该报错")
	}
}

func TestResolveResumePicker(t *testing.T) {
	useTempData(t)
	s := newSessionStore(filepath.Join(t.TempDir(), "proj"))
	newest, older := twoSessions(t, s)

	cases := []struct {
		input string
		want  string
	}{
		{"\n", newest}, // 直接回车 = 最近一条
		{"2\n", older},
		{"q\n", ""}, // 取消 = 新会话
	}
	for _, c := range cases {
		var out bytes.Buffer
		got, err := resolveResume("", s, bufio.NewReader(strings.NewReader(c.input)), &out)
		if err != nil {
			t.Fatalf("输入 %q: %v", c.input, err)
		}
		if got != c.want {
			t.Fatalf("输入 %q 得到 %q，期望 %q", c.input, got, c.want)
		}
		if !strings.Contains(out.String(), "历史会话") {
			t.Fatalf("没有打印列表: %q", out.String())
		}
	}

	if _, err := resolveResume("", s, bufio.NewReader(strings.NewReader("9\n")), &bytes.Buffer{}); err == nil {
		t.Fatal("无效编号应该报错")
	}
}

// 本项目没有会话时，裸 -r 不该报错：直接开新会话。
func TestResolveResumePickerNoSessions(t *testing.T) {
	useTempData(t)
	s := newSessionStore(filepath.Join(t.TempDir(), "proj"))
	var out bytes.Buffer
	got, err := resolveResume("", s, bufio.NewReader(strings.NewReader("\n")), &out)
	if err != nil || got != "" {
		t.Fatalf("空项目 = %q, %v；期望空路径 + 无错误", got, err)
	}
	if !strings.Contains(out.String(), "还没有历史会话") {
		t.Fatalf("提示不对: %q", out.String())
	}
}
