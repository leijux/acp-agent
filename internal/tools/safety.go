package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Policy 控制工具能触达的文件范围。
//
// 默认策略：所有路径必须落在 Root 内（含解析符号链接后的路径）。
// AllowOutside 时才允许 root 之外的绝对路径。
type Policy struct {
	Root string
	// ReadOnly 禁止写入类工具（write/edit/bash 写操作）；js 的 fetch 也只允许 GET/HEAD。
	ReadOnly bool
	// AllowOutside 允许访问 Root 之外的路径（谨慎）。
	AllowOutside bool
	// MaxReadBytes 单次读取上限（0 表示用默认值）。
	MaxReadBytes int64
	// AllowNetworkWrite 允许 js 的 fetch 发非 GET/HEAD 请求（即向外写）。
	//
	// 只有 `permissions=auto`（已全放行、不询问）打开。ask 必须关：js 不是
	// 改动型工具（Mutation=false），开了就直接绕过了确认弹窗——同样一件事走
	// bash 会弹窗，走 js 却不弹，那就是个后门。关掉时 fetch 的报错会把模型
	// 引到 bash 那条需要确认的路上。
	AllowNetworkWrite bool
	// AllowScriptWrite 允许 js 的 fs.writeFile 写盘。
	//
	// 和 AllowNetworkWrite 同理：js 不是改动型工具（Mutates=false），如果无条件
	// 让它写文件，就等于绕过了 write/edit 的确认弹窗与 diff 审计。只有
	// `permissions=auto`（已全放行）打开；关着时报错会把模型引到 write 工具那条
	// 需要确认的路上。写盘本身仍然复用 write 的路径锁（LockFile），同一文件并发
	// 写不会互相踩。
	AllowScriptWrite bool
}

// DefaultPolicy 以 cwd 为根。
func DefaultPolicy() Policy {
	wd, err := os.Getwd()
	if err != nil {
		wd = "."
	}
	return Policy{Root: wd, MaxReadBytes: 2 << 20}
}

// Resolve 把用户/模型给的路径规范化为绝对路径并做越界检查。
// 允许相对路径（相对 Root）与 ~ 开头路径。
func (p Policy) Resolve(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("path 不能为空")
	}
	pth := expandHome(strings.TrimSpace(path))

	root := p.Root
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

	if p.AllowOutside {
		return abs, nil
	}
	// root 自身也做符号链接规范化，否则 Windows 短路径/符号链接会让比较失败。
	if realRoot, err := filepath.EvalSymlinks(root); err == nil {
		root = realRoot
	}
	// 先按字面路径判断，再按符号链接真实路径判断（防止 root/link -> /etc）。
	if !withinRoot(root, abs) {
		return "", fmt.Errorf("path %q 超出允许目录 %q", path, root)
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		if !withinRoot(root, real) {
			return "", fmt.Errorf("path %q 解析符号链接后指向 %q，超出允许目录 %q", path, real, root)
		}
	}
	return abs, nil
}

func withinRoot(root, abs string) bool {
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func expandHome(p string) string {
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

// Rel 返回相对 Root 的展示路径（失败时返回绝对路径）。
func (p Policy) Rel(abs string) string {
	root := p.Root
	if root == "" {
		return abs
	}
	if r, err := filepath.Rel(root, abs); err == nil && !strings.HasPrefix(r, "..") {
		return r
	}
	return abs
}

// 危险命令检测：best effort，只拦明显破坏性操作（不是安全边界）。
var dangerousPatterns = []string{
	"rm -rf /",
	":(){:|:&};:",
	"mkfs",
	"dd if=/dev/zero of=/dev/",
	"> /dev/sda",
	"chmod -R 777 /",
	"shutdown",
	"reboot",
}

// CheckCommand 对 shell 命令做基本防护。返回非 nil 表示拒绝。
func (p Policy) CheckCommand(cmd string) error {
	lower := strings.ToLower(cmd)
	for _, pat := range dangerousPatterns {
		if strings.Contains(lower, pat) {
			return fmt.Errorf("命令被安全策略拒绝（匹配到 %q）", pat)
		}
	}
	return nil
}
