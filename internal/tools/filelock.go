package tools

import (
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// per-file 写锁。
//
// 背景：Executor 原先只要遇到 Mutates()==true 的工具就把整批调用串行化，
// 「两个不同文件的 write」也只能一个一个来。真正需要互斥的只是**同一路径**
// 的读-改-写，所以把粒度下沉到文件：同路径串行、不同路径并行。
//
// 局限（已知且可接受）：
//   - 不做符号链接解析，两个指向同一文件的路径会拿到两把锁；
//   - Windows 短名（PROGRA~1）与长名不会归一到同一个 key；
//   - 只保护本进程内的写，管不到子进程（bash/子代理）的并发写。
type fileLockTable struct {
	mu    sync.Mutex
	locks map[string]*fileLockEntry
}

type fileLockEntry struct {
	mu   sync.Mutex
	refs int
}

var fileLocks = fileLockTable{locks: make(map[string]*fileLockEntry)}

// LockFile 获取 path 的独占锁，返回的函数必须调用（通常是 defer）。
//
// 同一路径的并发调用互相阻塞；不同路径立即返回。解锁时引用计数归零
// 会从表里摘掉条目，避免长期运行的工具集积累无限 key。
func LockFile(path string) func() {
	key := lockKey(path)
	fileLocks.mu.Lock()
	e := fileLocks.locks[key]
	if e == nil {
		e = &fileLockEntry{}
		fileLocks.locks[key] = e
	}
	e.refs++
	fileLocks.mu.Unlock()

	e.mu.Lock()
	var once sync.Once
	return func() {
		once.Do(func() {
			e.mu.Unlock()
			fileLocks.mu.Lock()
			e.refs--
			if e.refs <= 0 {
				delete(fileLocks.locks, key)
			}
			fileLocks.mu.Unlock()
		})
	}
}

// lockKey 把路径归一到锁 key。
//
// 大小写不敏感的文件系统（Windows、macOS 默认）上 `a.go` 与 `A.go`
// 是同一个文件，key 必须一起小写，否则同文件仍会并行写。
func lockKey(path string) string {
	p := path
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	p = filepath.Clean(p)
	switch runtime.GOOS {
	case "windows", "darwin":
		p = strings.ToLower(p)
	}
	return p
}
