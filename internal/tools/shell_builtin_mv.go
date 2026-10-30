//go:build !shellnative

package tools

// 内建 mv：PATH 里没有真 mv 时的回退（纯 Windows 环境常见）。
//
// 语义照 GNU 常用面：mv [-f] [-n] src... dst。目标存在且是目录时拷成
// dst/base(src)；多源时目标必须是已存在目录。
//
// 跨盘/跨卷时 os.Rename 会失败（Windows 的 ERROR_NOT_SAME_DEVICE、Unix 的
// EXDEV），这时退化成「复制 + 删除源」，复用 cp 的实现（copyFile / copyDir）。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"mvdan.cc/sh/v3/interp"
)

func cmdMv(ctx context.Context, hc interp.HandlerContext, args []string) error {
	var force, noClobber bool
	var paths []string
	flagsDone := false
	for _, a := range args {
		switch {
		case flagsDone || a == "-" || !strings.HasPrefix(a, "-"):
			paths = append(paths, a)
		case a == "--":
			flagsDone = true
		default:
			for _, c := range a[1:] {
				switch c {
				case 'f':
					force = true
				case 'n':
					noClobber = true
				case 'v':
					// 不打印每个文件的移动过程
				default:
					return usagef(hc, "mv: invalid option -- %q", string(c))
				}
			}
		}
	}
	if len(paths) < 2 {
		return usagef(hc, "mv: missing destination file operand")
	}

	dstArg := paths[len(paths)-1]
	srcs := paths[:len(paths)-1]
	dstAbs := hcAbs(hc, dstArg)

	dstIsDir := false
	if st, err := os.Stat(dstAbs); err == nil {
		dstIsDir = st.IsDir()
	} else if len(srcs) > 1 {
		return usagef(hc, "mv: target %q is not a directory", dstArg)
	}

	status := 0
	for _, src := range srcs {
		if err := ctx.Err(); err != nil {
			return err
		}
		srcAbs := hcAbs(hc, src)
		target := dstAbs
		if dstIsDir {
			target = filepath.Join(dstAbs, filepath.Base(srcAbs))
		}
		if target == srcAbs {
			// mv a a：GNU 报 "are the same file"，但不改退出码之外的任何东西。
			if !force {
				fmt.Fprintf(hc.Stderr, "mv: %q and %q are the same file\n", src, dstArg)
				status = 1
			}
			continue
		}
		if isSubPath(srcAbs, target) {
			// 目录移进自身子目录：会边移边发现自己，必须先拒。
			fmt.Fprintf(hc.Stderr, "mv: cannot move %q into itself\n", src)
			status = 1
			continue
		}
		if err := moveOne(ctx, hc, srcAbs, target, force, noClobber); err != nil {
			fmt.Fprintf(hc.Stderr, "mv: %s: %s\n", src, errText(err))
			status = 1
		}
	}
	if status != 0 {
		return exitStatus(status)
	}
	return nil
}

// moveOne 移动一个条目：同卷直接 rename，跨卷退化成复制 + 删除。
func moveOne(ctx context.Context, hc interp.HandlerContext, src, dst string, force, noClobber bool) error {
	st, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(dst); err == nil {
		if noClobber {
			return nil
		}
		dstSt, serr := os.Stat(dst)
		if serr == nil && dstSt.IsDir() && !st.IsDir() {
			return fmt.Errorf("cannot overwrite directory")
		}
	}
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	// 目标可能是空目录（Windows 上 rename 到已存在目录会失败）：删掉再试一次。
	if dstSt, serr := os.Stat(dst); serr == nil && dstSt.IsDir() && st.IsDir() {
		if entries, rerr := os.ReadDir(dst); rerr == nil && len(entries) == 0 {
			if os.Remove(dst) == nil {
				if err := os.Rename(src, dst); err == nil {
					return nil
				}
			}
		}
	}

	// 跨卷（EXDEV / ERROR_NOT_SAME_DEVICE）：复制后删除源。复制失败时源保持不动。
	if st.IsDir() {
		if err := copyDir(ctx, hc, src, dst, force, noClobber); err != nil {
			return err
		}
		return os.RemoveAll(src)
	}
	if err := copyFile(src, dst, st, force, noClobber); err != nil {
		return err
	}
	return os.Remove(src)
}
