package kernel

import (
	"unicode"
	"unicode/utf8"

	"github.com/dop251/goja"
)

// Segment 是一个分词结果。Index 是 rune 偏移。
type Segment struct {
	Text  string
	Index int
}

// Segmenter 是 goja 缺失的 Intl.Segmenter 的替换实现。
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

// injectIntl 注册最小可用的 Intl.Segmenter。
func (r *runtime) injectIntl() error {
	seg := r.opts.Segmenter
	if seg == nil {
		seg = RuneSegmenter{}
	}
	vm := r.vm

	ctor := vm.ToValue(func(call goja.ConstructorCall) *goja.Object {
		obj := vm.NewObject()
		_ = obj.Set("resolvedOptions", vm.ToValue(func(goja.FunctionCall) goja.Value {
			o := vm.NewObject()
			_ = o.Set("granularity", "word")
			return o
		}))
		_ = obj.Set("segment", vm.ToValue(func(fc goja.FunctionCall) goja.Value {
			input := fc.Argument(0).String()
			segs := seg.Segment(input)
			arr := vm.NewArray()
			for i, s := range segs {
				item := vm.NewObject()
				_ = item.Set("segment", s.Text)
				_ = item.Set("index", s.Index)
				_ = item.Set("input", input)
				_ = arr.Set(intToKey(i), item)
			}
			return arr
		}))
		return obj
	})

	intlObj := vm.NewObject()
	_ = intlObj.Set("Segmenter", ctor)
	// 只有缺 Intl 时才需要注入；显式覆盖可以保证 bundle 在任何宿主下行为一致。
	v := vm.Get("Intl")
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return vm.Set("Intl", intlObj)
	}
	existing := v.ToObject(vm)
	_ = existing.Set("Segmenter", ctor)
	return nil
}

func intToKey(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}
