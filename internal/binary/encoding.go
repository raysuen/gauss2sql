package binary

import (
	"strings"
	"unicode/utf8"
)

// decodeByCodec 按 codec 名解码。utf-8 严格校验后失败回退 latin-1；
// 其它 codec 在标准库无实现时按 latin-1 逐字节映射（与 Python 兜底语义一致）。
func decodeByCodec(codec string, raw []byte) (string, error) {
	c := strings.ToLower(codec)
	switch c {
	case "utf-8", "utf8", "":
		if utf8.Valid(raw) {
			return string(raw), nil
		}
		// 无法整体解码：逐 rune 容错（等价 Python errors 兜底前的尝试——
		// Python 直接抛 UnicodeDecodeError 后整段 latin-1；这里我们保持与
		// "解码失败即 latin-1 逐字节" 一致：非法 UTF-8 字节逐字节映射。）
		fallthrough
	default:
		// latin-1 逐字节可逆映射
		var sb strings.Builder
		sb.Grow(len(raw))
		for _, b := range raw {
			sb.WriteRune(rune(b))
		}
		return sb.String(), nil
	}
}
