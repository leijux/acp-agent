package segmenter

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/huichen/sego"

	"github.com/leijux/acp-agent/internal/kernel"
)

// Sego 用 huichen/sego 做分词。
//
// 与 GSE 的区别：sego **不自带词典**，必须提供词典文件（sego 的格式是
// 每行 `词 频率 词性`）。好处是体积小、词典可控（可加项目专有名词）；
// 坏处是要自己分发词典。
//
// 另外 sego 的 LoadDictionary 在文件打不开时直接 log.Fatalf（进程退出），
// 所以这里先 os.Stat 校验，绝不把坏路径交给它。
type Sego struct {
	seg sego.Segmenter
}

// NewSego 加载一个或多个 sego 词典文件（按顺序，靠前的优先）。
//
// 词典可从 sego 仓库的 data/dictionary.txt 获取；也可以用项目自己的词表
// 与通用词典拼接（`NewSego("project.txt", "general.txt")`）。
func NewSego(files ...string) (*Sego, error) {
	if len(files) == 0 {
		return nil, errors.New("segmenter: sego 需要至少一个词典文件（sego 不自带词典；" +
			"可从 https://github.com/huichen/sego 的 data/dictionary.txt 获取）")
	}
	for _, f := range files {
		if strings.TrimSpace(f) == "" {
			return nil, errors.New("segmenter: sego 词典路径不能为空")
		}
		st, err := os.Stat(f)
		if err != nil {
			return nil, fmt.Errorf("segmenter: 无法读取 sego 词典 %q: %w", f, err)
		}
		if st.IsDir() {
			return nil, fmt.Errorf("segmenter: sego 词典 %q 是目录，需要文件", f)
		}
	}

	var seg sego.Segmenter
	// 文件已确认存在，这里不会再触发 sego 内部的 log.Fatalf。
	seg.LoadDictionary(strings.Join(files, ","))
	return &Sego{seg: seg}, nil
}

// Segment 实现 kernel.Segmenter。
func (s *Sego) Segment(text string) []kernel.Segment {
	if text == "" {
		return nil
	}
	raw := s.seg.Segment([]byte(text))
	out := make([]kernel.Segment, 0, len(raw))
	idx := runeIndex{text: text}
	for i := range raw {
		seg := &raw[i]
		tok := seg.Token()
		if tok == nil {
			continue
		}
		w := tok.Text()
		if w == "" {
			continue
		}
		out = append(out, kernel.Segment{Text: w, Index: idx.at(seg.Start())})
	}
	return out
}

// String 便于日志/诊断。
func (s *Sego) String() string { return "sego" }
