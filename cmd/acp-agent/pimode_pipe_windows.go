//go:build windows

package main

import (
	"errors"
	"strings"
	"syscall"
)

// extraBrokenPipe 覆盖 Windows 特有的「对面把管道关了」错误码。
//
// Windows 上写一个已关闭的管道不会返回 EPIPE：可能是 ERROR_BROKEN_PIPE，
// 也可能是 ERROR_NO_DATA（Go 的 syscall 没导出后者的常量）。不认这两种，
// `cmd | head -1` 这种「读一行就走」的调用方会被误判成「事件流写入失败」。
func extraBrokenPipe(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.ERROR_BROKEN_PIPE) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "pipe is being closed") || strings.Contains(msg, "broken pipe")
}
