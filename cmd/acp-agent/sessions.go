package main

import (
	"crypto/sha1"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
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
