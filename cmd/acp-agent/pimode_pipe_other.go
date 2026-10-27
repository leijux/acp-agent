//go:build !windows

package main

// extraBrokenPipe 在非 Windows 上没有额外错误码要认：EPIPE / io.ErrClosedPipe
// 已经在 errBrokenPipe 里处理了。
func extraBrokenPipe(error) bool { return false }
