package binary

import (
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
)

// decodeByCodec 按 codec 名解码，输出统一为 UTF-8。
// 支持：utf-8（严格校验）、gbk、gb18030（简体中文族，x/text 官方实现）；
// sql_ascii / latin-1 及未识别编码按 latin-1 逐字节可逆映射（与 Python 版兜底语义一致）。
// 任何解码失败均不报错，回退 latin-1 逐字节，保证字节可逆、绝不丢失。
func decodeByCodec(codec string, raw []byte) (string, error) {
	c := strings.ToLower(codec)
	switch c {
	case "utf-8", "utf8", "":
		if utf8.Valid(raw) {
			return string(raw), nil
		}
		// 非法 UTF-8：逐 rune 容错 → latin-1 逐字节映射
		return latin1(raw), nil
	case "gbk":
		if s, err := simplifiedchinese.GBK.NewDecoder().Bytes(raw); err == nil {
			return string(s), nil
		}
		return latin1(raw), nil
	case "gb18030":
		if s, err := simplifiedchinese.GB18030.NewDecoder().Bytes(raw); err == nil {
			return string(s), nil
		}
		return latin1(raw), nil
	default:
		// sql_ascii 与其它单字节/未识别编码：逐字节映射
		return latin1(raw), nil
	}
}

// latin1 latin-1 逐字节可逆映射（1 字节 → 1 codepoint）
func latin1(raw []byte) string {
	var sb strings.Builder
	sb.Grow(len(raw))
	for _, b := range raw {
		sb.WriteRune(rune(b))
	}
	return sb.String()
}
