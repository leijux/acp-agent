//go:build !shellnative

package tools

// 内建 cp：PATH 里没有真 cp 时顶上（纯 Windows 环境常见）。
//
// 支持 -r/-R（递归）、-f（覆盖）、-n（不覆盖）。与 GNU 的差异：
//
//   - 默认**跟随符号链接**（GNU cp 默认也是解引用，但 `cp -P` 保留链接；这里
//     没有 -P）；
//   - 不实现 `-p/-a`（保留 mtime/owner）与稀疏文件、硬链接的语义；
//   - 目标是已存在目录时按 GNU 规则拷成 `dst/<basename(src)>`；目标不存在且
//     只有一个源时，目标就是文件名（可改名）；
//   - 目录拷贝用 `filepath.WalkDir`，不跟随符号链接目录（避免环）。

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"mvdan.cc/sh/v3/interp"
)

func cmdCp(ctx context.Context, hc interp.HandlerContext, args []string) error {
	var (
		recursive bool
		force     bool
		noClobber bool
	)
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
				case 'r', 'R':
					recursive = true
				case 'f':
					force = true
				case 'n':
					noClobber = true
				case 'v':
					// 不打印每个文件的拷贝过程
				default:
					return usagef(hc, "cp: invalid option -- %q", string(c))
				}
			}
		}
	}
	if len(paths) < 2 {
		return usagef(hc, "cp: missing destination file operand after %q",
			strings.Join(paths, " "))
	}

	dstArg := paths[len(paths)-1]
	srcs := paths[:len(paths)-1]
	dstAbs := hcAbs(hc, dstArg)

	dstIsDir := false
	if st, err := os.Stat(dstAbs); err == nil {
		dstIsDir = st.IsDir()
	} else if len(srcs) > 1 {
		// 多源时目标必须是已存在目录（GNU 同）。
		return usagef(hc, "cp: target %q is not a directory", dstArg)
	}

	status := 0
	for _, src := range srcs {
		if err := ctx.Err(); err != nil {
			return err
		}
		srcAbs := hcAbs(hc, src)
		st, err := os.Stat(srcAbs)
		if err != nil {
			fmt.Fprintf(hc.Stderr, "cp: %s: %s\n", src, errText(err))
			status = 1
			continue
		}

		target := dstAbs
		if dstIsDir {
			target = filepath.Join(dstAbs, filepath.Base(srcAbs))
		}

		switch {
		case st.IsDir():
			if !recursive {
				fmt.Fprintf(hc.Stderr, "cp: %s: omitting directory\n", src)
				status = 1
				continue
			}
			// 别把目录拷进自己/子目录里（否则 WalkDir 会边拷边发现新文件）。
			if isSubPath(srcAbs, target) {
				fmt.Fprintf(hc.Stderr, "cp: cannot copy a directory, %q, into itself, %q\n", src, dstArg)
				status = 1
				continue
			}
			if err := copyDir(ctx, hc, srcAbs, target, force, noClobber); err != nil {
				fmt.Fprintf(hc.Stderr, "cp: %v\n", err)
				status = 1
			}
		default:
			if err := copyFile(srcAbs, target, st, force, noClobber); err != nil {
				fmt.Fprintf(hc.Stderr, "cp: %v\n", err)
				status = 1
			}
		}
	}
	if status != 0 {
		return exitStatus(status)
	}
	return nil
}

// copyFile 拷一个普通文件。dst 是完整目标路径。
func copyFile(src, dst string, st fs.FileInfo, force, noClobber bool) error {
	if dstSt, err := os.Stat(dst); err == nil {
		if dstSt.IsDir() {
			return fmt.Errorf("%s: is a directory", dst)
		}
		if noClobber {
			return nil
		}
		// -f 之外也允许覆盖：GNU 的交互确认在非 tty 下等于放弃覆盖，
		// 这里没有 tty 概念，默认覆盖（-n 才跳过）。
		_ = force
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}

	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("%s: %s", src, errText(err))
	}
	defer in.Close()

	mode := st.Mode().Perm()
	if mode == 0 {
		mode = 0o644
	}
	// 先写临时文件再 rename：中途失败不会留下半个文件。
	tmp := dst + ".cp-tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return fmt.Errorf("%s: %s", dst, errText(err))
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return fmt.Errorf("%s: %v", dst, err)
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("%s: %v", dst, err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		// Windows 上 rename 到已存在的文件会失败：退化成删了再 rename。
		if rmErr := os.Remove(dst); rmErr == nil {
			if err2 := os.Rename(tmp, dst); err2 == nil {
				return nil
			}
		}
		return fmt.Errorf("%s: %v", dst, err)
	}
	return nil
}

// copyDir 递归拷目录。
func copyDir(ctx context.Context, hc interp.HandlerContext, src, dst string, force, noClobber bool) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("%s: %s", p, errText(err))
		}
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		rel, rerr := filepath.Rel(src, p)
		if rerr != nil {
			return fmt.Errorf("%s: %v", p, rerr)
		}
		target := dst
		if rel != "." {
			target = filepath.Join(dst, rel)
		}
		if d.IsDir() {
			info, ierr := d.Info()
			mode := os.FileMode(0o755)
			if ierr == nil && info.Mode().Perm() != 0 {
				mode = info.Mode().Perm()
			}
			if err := os.MkdirAll(target, mode); err != nil {
				return fmt.Errorf("%s: %s", target, errText(err))
			}
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return fmt.Errorf("%s: %s", p, errText(err))
		}
		if err := copyFile(p, target, info, force, noClobber); err != nil {
			return err
		}
		return nil
	})
}

// isSubPath 报告 child 是否等于 parent 或位于其下（都必须是绝对/清理过的路径）。
func isSubPath(parent, child string) bool {
	parent = filepath.Clean(parent)
	child = filepath.Clean(child)
	if parent == child {
		return true
	}
	return strings.HasPrefix(child, parent+string(filepath.Separator))
}
