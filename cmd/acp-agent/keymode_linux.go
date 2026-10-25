//go:build linux

package main

import (
	"golang.org/x/sys/unix"
)

// enterCbreak 关闭行缓冲与回显，并关掉 ISIG —— Ctrl+C 作为字节 0x03 交给
// keyWatcher，三个平台行为一致（详见 keyinput.go）。
//
// **OPOST 必须保留**：关掉它之后输出里的 \n 不再转 \r\n，流式 Markdown 会错乱。
// 这也是不能用 x/term.MakeRaw 的原因：它在 Unix 上会清 OPOST。
func enterCbreak(fd int) (func(), error) {
	t, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return nil, err
	}
	old := *t
	t.Lflag &^= unix.ICANON | unix.ECHO | unix.IEXTEN | unix.ISIG
	t.Cc[unix.VMIN] = 1
	t.Cc[unix.VTIME] = 0
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, t); err != nil {
		return nil, err
	}
	return func() { _ = unix.IoctlSetTermios(fd, unix.TCSETS, &old) }, nil
}
