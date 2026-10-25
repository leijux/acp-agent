package tools

import (
	"errors"
	"io/fs"
)

// errText 把文件系统错误转成稳定的英文文案。
//
// 为什么不用 err.Error()：Windows 上的原文是 "The system cannot find the file
// specified."，而 POSIX 侧是 "no such file or directory"。模型在两个平台上
// 看到的措辞不同会影响它对「文件是否真的不存在」的判断，也不方便断言。
//
// 该函数不加 build tag：js_fs.go（无 tag）与内建命令层（!shellnative）都要用。
func errText(err error) string {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "No such file or directory"
	case errors.Is(err, fs.ErrPermission):
		return "Permission denied"
	case errors.Is(err, fs.ErrExist):
		return "File exists"
	}
	if pe, ok := err.(*fs.PathError); ok {
		return pe.Err.Error()
	}
	return err.Error()
}
