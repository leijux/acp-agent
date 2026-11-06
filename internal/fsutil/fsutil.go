// Package fsutil 是跨包共用的文件系统小工具：路径解析（root 边界 / ~ 展开）
// 与「遍历时跳过哪些目录」这一张表。
//
// 为什么单独抽一个包：这两件事以前分别长在 internal/tools（read/write/grep 用）
// 和 skills 里，@ 文件注入又要用一遍。三份各写一遍就是三套会漂移的规则 ——
// 尤其是越界检查（安全相关），漂移一次就是一个洞。
package fsutil

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SkipDirs 是遍历时默认跳过的目录名（按 basename 匹配）。
//
// 这些目录里没有「用户想 @ 或想 grep 的源码」：构建产物、依赖缓存、VCS 内部。
var SkipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, "dist": true,
	"build": true, ".next": true, "target": true, "__pycache__": true,
	".venv": true, ".idea": true, ".cache": true,
}

// Skip 报告一个目录名是否该被跳过。
func Skip(name string) bool { return SkipDirs[name] }

// Resolve 把用户/模型给的路径规范化为绝对路径并做越界检查。
// 允许相对路径（相对 root）与 ~ 开头路径。
//
// allowOutside 为真时只做规范化，不做边界检查（对应 tools.Policy.AllowOutside）。
func Resolve(root string, allowOutside bool, path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("path 不能为空")
	}
	pth := ExpandHome(strings.TrimSpace(path))

	if root == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		root = wd
	}
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}

	abs := pth
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(root, abs)
	}
	abs = filepath.Clean(abs)

	if allowOutside {
		return abs, nil
	}
	// root 自身也做符号链接规范化，否则 Windows 短路径/符号链接会让比较失败。
	if realRoot, err := filepath.EvalSymlinks(root); err == nil {
		root = realRoot
	}
	// 先按字面路径判断，再按符号链接真实路径判断（防止 root/link -> /etc）。
	if !WithinRoot(root, abs) {
		return "", fmt.Errorf("path %q 超出允许目录 %q", path, root)
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		if !WithinRoot(root, real) {
			return "", fmt.Errorf("path %q 解析符号链接后指向 %q，超出允许目录 %q", path, real, root)
		}
	}
	return abs, nil
}

// WithinRoot 报告 abs 是否落在 root 之内（root 自身算「之内」）。
func WithinRoot(root, abs string) bool {
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// ExpandHome 把 `~` / `~/x` / `~\x` 展开成用户主目录。
func ExpandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil {
			return p
		}
		if p == "~" {
			return home
		}
		return filepath.Join(home, p[2:])
	}
	return p
}
