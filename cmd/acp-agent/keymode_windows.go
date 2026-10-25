//go:build windows

package main

import (
	xterm "golang.org/x/term"
)

// enterCbreak 把终端切成「按键即时送达」模式，返回恢复函数。
//
// Windows 下 x/term.MakeRaw 只改 stdin 的 console mode（关闭行缓冲/回显/
// 处理后输入），不碰输出句柄，所以流式渲染里的 \n 仍然正常换行。
// 代价是 Ctrl+C 不再产生 SIGINT，会以字节 0x03 到达 —— 由 keyWatcher 处理。
func enterCbreak(fd int) (func(), error) {
	st, err := xterm.MakeRaw(fd)
	if err != nil {
		return nil, err
	}
	return func() { _ = xterm.Restore(fd, st) }, nil
}
