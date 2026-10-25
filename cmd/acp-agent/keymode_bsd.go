//go:build darwin || freebsd || netbsd || openbsd || dragonfly

package main

import (
	"golang.org/x/sys/unix"
)

// enterCbreak 关闭行缓冲与回显，并关掉 ISIG（原因见 keymode_linux.go）。
//
// BSD 系（含 macOS）的 termios ioctl 是 TIOCGETA / TIOCSETA，与 Linux 不同。
func enterCbreak(fd int) (func(), error) {
	t, err := unix.IoctlGetTermios(fd, unix.TIOCGETA)
	if err != nil {
		return nil, err
	}
	old := *t
	t.Lflag &^= unix.ICANON | unix.ECHO | unix.IEXTEN | unix.ISIG
	t.Cc[unix.VMIN] = 1
	t.Cc[unix.VTIME] = 0
	if err := unix.IoctlSetTermios(fd, unix.TIOCSETA, t); err != nil {
		return nil, err
	}
	return func() { _ = unix.IoctlSetTermios(fd, unix.TIOCSETA, &old) }, nil
}
