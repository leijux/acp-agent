package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
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
