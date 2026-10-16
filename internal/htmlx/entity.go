package htmlx

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// namedEntities 覆盖网页正文/标题里最高频的命名实体。
// 刻意保持精简：完整 HTML5 实体表有 2000+ 条，对抓取场景收益极低。
var namedEntities = map[string]rune{
	"amp": '&', "lt": '<', "gt": '>', "quot": '"', "apos": '\'',
	"nbsp": '\u00a0', "ensp": '\u2002', "emsp": '\u2003', "thinsp": '\u2009',
	"copy": '©', "reg": '®', "trade": '™', "deg": '°', "sect": '§',
	"para": '¶', "middot": '·', "bull": '•', "hellip": '…', "permil": '‰',
	"times": '×', "divide": '÷', "plusmn": '±', "minus": '−', "ne": '≠',
	"le": '≤', "ge": '≥', "infin": '∞', "radic": '√', "sum": '∑',
	"mdash": '—', "ndash": '–', "horbar": '―',
	"lsquo": '‘', "rsquo": '’', "ldquo": '“', "rdquo": '”',
	"laquo": '«', "raquo": '»', "sbquo": '‚', "bdquo": '„',
	"larr": '←', "uarr": '↑', "rarr": '→', "darr": '↓', "harr": '↔',
	"euro": '€', "pound": '£', "yen": '¥', "cent": '¢', "curren": '¤',
	"sup2": '²', "sup3": '³', "frac12": '½', "frac14": '¼', "frac34": '¾',
	"alpha": 'α', "beta": 'β', "gamma": 'γ', "delta": 'δ', "pi": 'π',
	"micro": 'µ', "prime": '′', "Prime": '″',
}

// DecodeEntities 解码 HTML 实体（数字实体 + namedEntities）。
// 无法识别的实体原样保留。无 '&' 时直接返回入参，避免无谓分配。
func DecodeEntities(s string) string {
	if !strings.ContainsRune(s, '&') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] != '&' {
			b.WriteByte(s[i])
			i++
			continue
		}
		// 实体长度上限 32，避免把 "a & b & c" 这类文本里的一段吞掉。
		semi := strings.IndexByte(s[i:], ';')
		if semi <= 0 || semi > 32 {
			b.WriteByte('&')
			i++
			continue
		}
		if r, ok := decodeEntity(s[i+1 : i+semi]); ok {
			b.WriteRune(r)
			i += semi + 1
			continue
		}
		b.WriteByte('&')
		i++
	}
	return b.String()
}

// decodeEntity 解码单个实体名（不含 '&' 与 ';'）。
func decodeEntity(name string) (rune, bool) {
	if name == "" {
		return 0, false
	}
	if name[0] == '#' {
		digits := name[1:]
		base := 10
		if len(digits) > 0 && (digits[0] == 'x' || digits[0] == 'X') {
			base = 16
			digits = digits[1:]
		}
		if digits == "" {
			return 0, false
		}
		code, err := strconv.ParseInt(digits, base, 32)
		if err != nil || code <= 0 || code > utf8.MaxRune {
			return 0, false
		}
		// 代理区码点不是合法字符，拒绝以免产生非法 UTF-8。
		if code >= 0xD800 && code <= 0xDFFF {
			return 0, false
		}
		return rune(code), true
	}
	r, ok := namedEntities[name]
	return r, ok
}
