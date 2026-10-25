//go:build !windows && !linux && !darwin && !freebsd && !netbsd && !openbsd && !dragonfly

package main

import "errors"

// enterCbreak 在该平台上没有实现：REPL 会退回行模式（Esc 不可用），
// 其余功能不受影响。
func enterCbreak(int) (func(), error) {
	return nil, errors.New("该平台不支持 REPL 按键监听")
}
