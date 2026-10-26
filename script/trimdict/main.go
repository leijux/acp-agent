// Command trimdict 从 gse 自带词典裁剪出一份可控体积的高频词表。
//
// 背景：gse 的 go:embed 里同时塞了简体（4.9MB）、繁体（3.4MB）、日文（22.6MB）
// 和 idf（5.9MB）。只要 LoadDictEmbed 可达，linker 就得全留下 —— 我们只想要
// 简体词典，却要付 +31MB 的体积。解法是构建时加 gse 的 `ne` tag 关掉它的
// embed，改用这里生成的高频词表 + LoadDictStr 加载。
//
// 用法（也由 internal/segmenter 的 //go:generate 调用）：
//
//	go run ./script/trimdict -lang s -n 100000 -out internal/segmenter/dict_zh_s.txt
//
// 项目目前只用简体（-lang s）；-lang t 留作备用，输出不会入库。
//
// 裁剪规则：按词频降序取前 N 行，**原样输出整行**（`词 频率 词性`，单空格分隔）。
// 不要输出注释行、不要改分隔符、不要过滤单字词：
//   - gse 的 splitDictLine 按单个空格切三列，注释行会被当成词条加进词典；
//   - 单字词会被 gse 的 Size() 把频率强制设成 2，仍参与 DAG 选路，删了会改切分。
package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// dictFiles 是 gse 里两份中文词典的路径（相对模块根）。
var dictFiles = map[string]string{
	"s": "data/dict/zh/s_1.txt",
	"t": "data/dict/zh/t_1.txt",
}

func main() {
	var (
		lang    = flag.String("lang", "s", "词表语言：s=简体，t=繁体")
		n       = flag.Int("n", 100000, "保留的高频词条数")
		out     = flag.String("out", "", "输出文件路径（必填）")
		src     = flag.String("src", "", "源词典路径（默认从 gse 模块定位）")
		dry     = flag.Bool("dry", false, "只打印统计，不写文件")
		verbose = flag.Bool("v", false, "打印每条保留词条的频率下界")
	)
	flag.Parse()

	if *out == "" && !*dry {
		fatalf("-out 必填（或用 -dry 只看统计）")
	}
	rel, ok := dictFiles[*lang]
	if !ok {
		fatalf("-lang 只支持 s / t，收到 %q", *lang)
	}
	srcPath := *src
	if srcPath == "" {
		dir, err := gseModuleDir()
		if err != nil {
			fatalf("定位 gse 模块失败: %v（可用 -src 直接指定词典路径）", err)
		}
		srcPath = filepath.Join(dir, rel)
	}

	entries, err := readDict(srcPath)
	if err != nil {
		fatalf("%v", err)
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].freq > entries[j].freq })

	lim := *n
	if lim > len(entries) {
		lim = len(entries)
	}
	if lim <= 0 {
		fatalf("-n 必须为正数")
	}
	kept := entries[:lim]
	minFreq := kept[len(kept)-1].freq

	var buf bytes.Buffer
	for _, e := range kept {
		buf.WriteString(e.line)
		buf.WriteByte('\n')
	}

	fmt.Printf("源词典 %s：%d 行\n", srcPath, len(entries))
	fmt.Printf("保留 %d 行，频率下界 %d，输出 %.1f KB\n", len(kept), minFreq, float64(buf.Len())/1024)
	if *verbose {
		fmt.Printf("最高频：%s (%d)，最低频：%s (%d)\n",
			kept[0].line, kept[0].freq, kept[len(kept)-1].line, kept[len(kept)-1].freq)
	}
	if *dry {
		return
	}
	if err := os.WriteFile(*out, buf.Bytes(), 0o644); err != nil {
		fatalf("写 %s 失败: %v", *out, err)
	}
	fmt.Printf("已写出 %s\n", *out)
}

type entry struct {
	line string
	freq int
}

// readDict 读 gse 格式词典（`词 频率 [词性]`），保留原行文本。
func readDict(path string) ([]entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("打开 %s 失败: %w", path, err)
	}
	defer f.Close()

	var out []entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) < 2 {
			continue
		}
		freq, err := strconv.Atoi(fields[1])
		if err != nil || freq <= 0 {
			continue
		}
		// 词性列可能缺失；原样保留行内容（不去多余空格），gse 自己会 TrimSpace。
		out = append(out, entry{line: trimmed, freq: freq})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("读 %s 失败: %w", path, err)
	}
	return out, nil
}

// gseModuleDir 用 go list 拿 gse 在模块缓存里的绝对路径（不硬编码 GOMODCACHE）。
func gseModuleDir() (string, error) {
	cmd := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/go-ego/gse")
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	dir := strings.TrimSpace(string(out))
	if dir == "" {
		return "", fmt.Errorf("go list 返回空路径（gse 不在依赖里？）")
	}
	return dir, nil
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "trimdict: "+format+"\n", args...)
	os.Exit(1)
}
