package kernel

import (
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/MoeForge/moejs"
)

// Segment 是一个分词结果。Index 是 rune 偏移。
type Segment struct {
	Text  string
	Index int
}

// Segmenter 是内核宿主的可插拔分词器（上游用 `Intl.Segmenter`）。
//
// 默认路径不注入：引擎自带的 Segmenter（UAX#29、无词典，汉字逐字成段）
// 与 RuneSegmenter 在检索语义上等价。传入自定义实现（例如 gse 词典）时
// 会被注入为 Intl.Segmenter 的替身，见 runtime.injectIntl。
//
// kernel 只在 src/search/tokenizer.ts 里用它切分 CJK 文本：把连续 CJK 段
// 聚成 run，再在 run 内做 OOV bigram 回退。因此“每个汉字一段”的实现与
// Node + CLDR 的结果在检索语义上等价（实测 messagesHash / blocksHash 一致）。
//
// 接入真正的分词器（例如 go-ego/gse）能进一步改善中文检索召回，只需实现
// 本接口并通过 WithSegmenter 注入。
type Segmenter interface {
	Segment(text string) []Segment
}

// RuneSegmenter 是零依赖的默认实现：
//   - CJK（汉字/假名/谚文）逐字成段
//   - ASCII 字母数字下划线连续成段
//   - 空白连续成段
//   - 其余字符逐字符成段
type RuneSegmenter struct{}

func (RuneSegmenter) Segment(input string) []Segment {
	out := make([]Segment, 0, len(input)/2)
	i, ri := 0, 0
	for i < len(input) {
		r, size := utf8.DecodeRuneInString(input[i:])
		switch {
		case isCJK(r):
			out = append(out, Segment{Text: input[i : i+size], Index: ri})
			i += size
			ri++
		case isWordRune(r):
			j := i + size
			for j < len(input) {
				r2, s2 := utf8.DecodeRuneInString(input[j:])
				if !isWordRune(r2) {
					break
				}
				j += s2
			}
			out = append(out, Segment{Text: input[i:j], Index: ri})
			ri += utf8.RuneCountInString(input[i:j])
			i = j
		case unicode.IsSpace(r):
			j := i + size
			for j < len(input) {
				r2, s2 := utf8.DecodeRuneInString(input[j:])
				if !unicode.IsSpace(r2) {
					break
				}
				j += s2
			}
			out = append(out, Segment{Text: input[i:j], Index: ri})
			ri += utf8.RuneCountInString(input[i:j])
			i = j
		default:
			// 标点等：合并同类连续字符，便于调试时观察。
			j := i + size
			for j < len(input) {
				r2, s2 := utf8.DecodeRuneInString(input[j:])
				if isCJK(r2) || isWordRune(r2) || unicode.IsSpace(r2) {
					break
				}
				j += s2
			}
			out = append(out, Segment{Text: input[i:j], Index: ri})
			ri += utf8.RuneCountInString(input[i:j])
			i = j
		}
	}
	return out
}

func isWordRune(r rune) bool {
	return r < utf8.RuneSelf && (r == '_' ||
		(r >= '0' && r <= '9') ||
		(r >= 'a' && r <= 'z') ||
		(r >= 'A' && r <= 'Z'))
}

func isCJK(r rune) bool {
	switch {
	case r >= 0x3400 && r <= 0x4DBF, // CJK Ext A
		r >= 0x4E00 && r <= 0x9FFF,   // CJK Unified
		r >= 0xF900 && r <= 0xFAFF,   // CJK Compatibility
		r >= 0x3040 && r <= 0x30FF,   // Hiragana + Katakana
		r >= 0xAC00 && r <= 0xD7AF,   // Hangul
		r >= 0x20000 && r <= 0x2FA1F: // CJK Ext B..F + Compatibility Supplement
		return true
	}
	return false
}

// injectIntl 只在宿主显式传了分词器时才替换 Intl.Segmenter。
//
// 默认路径什么都不做：moejs 自带 Intl.Segmenter（UAX#29、无词典，汉字逐字
// 成段），与默认的 RuneSegmenter 在检索语义上等价。不动它也就保住了 moejs
// 的「进程级共享冻结内建」—— 写内建需要 Options.MutableIntrinsics，而那会
// 让每个 runtime 复制一份内建（内存与启动成本都上去）。
//
// 传了 WithSegmenter（例如 gse 词典分词）时才注入；newRuntime 已经据
// opts.Segmenter 打开了 MutableIntrinsics，否则这里写 Intl 会抛 TypeError。
func (r *runtime) injectIntl() error {
	seg := r.opts.Segmenter
	if seg == nil {
		return nil
	}
	if err := r.rt.SetGlobal("__acpSegment", moejs.NativeFunc(
		func(_ *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
			input := jsString(args, 0)
			segs := seg.Segment(input)
			out := make([]any, 0, len(segs))
			for _, s := range segs {
				out = append(out, []any{s.Text, s.Index})
			}
			return r.rt.FromGo(out)
		})); err != nil {
		return err
	}
	s, err := intlShim()
	if err != nil {
		return err
	}
	_, err = r.rt.RunScript(s)
	return err
}

// intlSegmenterShim 是替换 Intl.Segmenter 的 JS 代码。
//
// 用 JS（而不是从 Go 造一个构造器）是因为 new Intl.Segmenter() 需要
// [[Construct]]，而 class 写一行就有了；返回的对象数组也在 JS 里拼，
// 不必来回跨引擎边界。Script 不可变、可被多个 runtime 共享。
const intlSegmenterShim = `(function() {
	"use strict";
	class Segmenter {
		constructor(locale, opts) {
			this._granularity = (opts && opts.granularity) || "word";
		}
		resolvedOptions() { return { granularity: this._granularity }; }
		segment(input) {
			const raw = __acpSegment(input);
			const out = new Array(raw.length);
			for (let i = 0; i < raw.length; i++) {
				out[i] = { segment: raw[i][0], index: raw[i][1], input: input };
			}
			return out;
		}
	}
	Object.defineProperty(Intl, "Segmenter", {
		value: Segmenter, writable: true, configurable: true, enumerable: false,
	});
})()`

var (
	intlShimOnce sync.Once
	intlShimProg *moejs.Script
	intlShimErr  error
)

// intlShim 返回（进程内只编译一次的）Segmenter 替换脚本。
func intlShim() (*moejs.Script, error) {
	intlShimOnce.Do(func() {
		intlShimProg, intlShimErr = moejs.CompileScript("acp-kernel.intl.js", intlSegmenterShim)
	})
	return intlShimProg, intlShimErr
}
