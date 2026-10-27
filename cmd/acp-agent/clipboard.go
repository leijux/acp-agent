package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	osc52 "github.com/aymanbagabas/go-osc52/v2"
)

// 复制到系统剪贴板。两条路，先本地命令、再 OSC 52：
//
//   - 本地命令（clip / pbcopy / wl-copy / xclip）：本机终端里最可靠，
//     一条命令就能确认成功与否；
//   - OSC 52：让终端自己写剪贴板，SSH、tmux、容器里只有它能用
//     （前提是终端支持 + 没被 tmux/screen 吞掉）。
//
// osc52 是否允许由调用方决定：TUI 模式下 stdout 由界面渲染器独占，
// 直接写转义序列会破坏“单一写入者”的约束，而且终端不支持时还会假报成功，
// 所以交互界面里干脆不用它，只报“本机剪贴板命令不可用”。
const clipboardTimeout = 5 * time.Second

// copyToClipboard 把 text 放进系统剪贴板，返回用的哪条路（给提示行看）。
// allowOSC 为 false 时不会退到 OSC 52。
func copyToClipboard(text string, allowOSC bool) (string, error) {
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("没有可复制的内容")
	}
	how, nativeErr := clipboardNative(text)
	if nativeErr == nil {
		return how, nil
	}
	if !allowOSC {
		return "", fmt.Errorf("本机剪贴板命令不可用：%v", nativeErr)
	}
	// 本地命令不可用时退到 OSC 52；stdout 不是终端就没人读这个序列。
	if !isTTY(os.Stdout) {
		return "", fmt.Errorf("剪贴板不可用：%v（且 stdout 不是终端，无法用 OSC 52）", nativeErr)
	}
	if _, err := osc52Seq(text).WriteTo(os.Stdout); err != nil {
		return "", fmt.Errorf("剪贴板不可用：%v；OSC 52 也失败：%w", nativeErr, err)
	}
	return "OSC 52", nil
}

// osc52Seq 生成复制用序列。tmux 里要额外包一层 DCS 透传，
// 否则序列会被 tmux 当成自己的 OSC 吃掉。
func osc52Seq(text string) osc52.Sequence {
	seq := osc52.New(text)
	if os.Getenv("TMUX") != "" {
		seq = seq.Tmux()
	}
	return seq
}

// clipboardNative 调用平台自带命令写剪贴板。
func clipboardNative(text string) (string, error) {
	switch runtime.GOOS {
	case "windows":
		return clipboardWindows(text)
	case "darwin":
		return runStdin(text, "pbcopy")
	default:
		// 显示服务器可能缺；挨个试，全不在就交给 OSC 52。
		err := fmt.Errorf("没有可用的剪贴板命令")
		for _, c := range [][]string{
			{"wl-copy"},
			{"xclip", "-selection", "clipboard"},
			{"xsel", "--clipboard", "--input"},
		} {
			if _, lookErr := exec.LookPath(c[0]); lookErr != nil {
				continue
			}
			if how, runErr := runStdin(text, c[0], c[1:]...); runErr == nil {
				return how, nil
			} else {
				err = runErr
			}
		}
		return "", err
	}
}

// psClipboardScript 拼出写剪贴板的 PowerShell 命令。
//
// 单独抽出来是为了能测：路径里的单引号在 PowerShell 单引号串里要写两遍，
// 拼错会让整个复制失败（而且失败信息还被 PowerShell 吞掉）。
func psClipboardScript(path string) string {
	q := strings.ReplaceAll(path, "'", "''")
	return fmt.Sprintf(
		"Set-Clipboard -Value ([IO.File]::ReadAllText('%s', [Text.Encoding]::UTF8))", q)
}

// clipboardWindows 走 PowerShell 的 Set-Clipboard。
//
// 不用 clip.exe：它按控制台代码页读 stdin（中文系统是 GBK），
// 我们喂的是 UTF-8，中文会变乱码。Set-Clipboard 用临时文件传文本，
// 既避开编码问题也避开引号转义问题。
func clipboardWindows(text string) (string, error) {
	ps, err := lookPath([]string{"powershell", "pwsh"})
	if err != nil {
		return "", fmt.Errorf("找不到 powershell：%w", err)
	}
	f, err := os.CreateTemp("", "acp-agent-clip-*.txt")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(text); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}

	return runCmd(nil, ps, "-NoProfile", "-NonInteractive", "-Command",
		psClipboardScript(f.Name()))
}

// runStdin 用 stdin 喂文本（macOS / Linux 的剪贴板命令都这么用）。
func runStdin(text string, name string, args ...string) (string, error) {
	bin, err := exec.LookPath(name)
	if err != nil {
		return "", err
	}
	return runCmd(strings.NewReader(text), bin, args...)
}

// runCmd 带超时执行：剪贴板命令卡住也不该把 REPL 拖死。
func runCmd(stdin *strings.Reader, bin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), clipboardTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	if stdin != nil {
		cmd.Stdin = stdin
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			err = fmt.Errorf("%v: %s", err, firstLine(msg, 200))
		}
		return "", err
	}
	return filepath.Base(bin), nil
}

// lookPath 返回第一个装了的候选命令。
func lookPath(names []string) (string, error) {
	for _, n := range names {
		if p, err := exec.LookPath(n); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s 都不在 PATH 里", strings.Join(names, "/"))
}
