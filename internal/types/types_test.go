package types

import (
	"testing"

	"gauss2sql-go/internal/binary"
)

// withEncoding 临时切换全局文本编码，测完恢复 utf-8。
func withEncoding(enc string, fn func()) {
	prev := binary.GetTextEncoding()
	binary.SetTextEncoding(enc)
	defer binary.SetTextEncoding(prev)
	fn()
}

// TestSQLStringLiteral_DecodedRunes 确认 SQL 转义发生在解码之后：
// 输入已是解码完成的 UTF-8 rune 串，转义应按 rune 进行（' → ''，\ → \\），
// 中文字符不得被截断/误转义。v0.2.21 C2。
func TestSQLStringLiteral_DecodedRunes(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"普通中文", "'普通中文'"},
		{"it's", "E'it''s'"},
		{"a\\b", "E'a\\\\b'"},
		{"中文'quote\\反斜", "E'中文''quote\\\\反斜'"},
		{"line\nbreak", "E'line\\nbreak'"},
		{"tab\there", "E'tab\\there'"},
	}
	for _, c := range cases {
		if got := SQLStringLiteral(c.in); got != c.want {
			t.Errorf("SQLStringLiteral(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestDecodeValue_GBKTextThenEscape 端到端确认顺序：
// GBK 库编码下，文本 varlena 字节先被 DecodeValue 按 GBK 解码成 UTF-8，
// 再交给 SQLStringLiteral 转义——中文正确、内嵌引号正确翻倍。
// 若顺序反了（先按字节转义再解码），GBK 双字节 trail 会被误判。
func TestDecodeValue_GBKTextThenEscape(t *testing.T) {
	withEncoding("gbk", func() {
		// "O'Reilly中文" 的 GBK 字节
		raw := []byte("O'Reilly")
		raw = append(raw, 0xD6, 0xD0, 0xCE, 0xC4) // 中文
		vl := binary.RebuildVarlena(raw)

		decoded := DecodeValue(TextOID, vl)
		if decoded != "O'Reilly中文" {
			t.Fatalf("DecodeValue GBK text = %q, want %q", decoded, "O'Reilly中文")
		}
		lit := SQLStringLiteral(decoded)
		want := "E'O''Reilly中文'"
		if lit != want {
			t.Fatalf("SQLStringLiteral = %q, want %q", lit, want)
		}
	})
}

// jsonbScalarContainer 构造最小 jsonb 标量字符串容器（用于单测）：
// header=jbFScalar|count=1，一个 entry（t=0 字符串，结束偏移=len(data)），
// 后跟 data 段原始字节。
func jsonbScalarContainer(data []byte) []byte {
	payload := make([]byte, 0, 8+len(data))
	payload = append(payload, 0x01, 0x00, 0x00, 0x10) // jbFScalar
	payload = append(payload, byte(len(data)), 0, 0, 0)
	payload = append(payload, data...)
	return payload
}

// TestDecodeValue_JSONB_GBKTranscode 确认 C1 修正：
// GBK 库上 jsonb 容器内字符串 leaf 以 GBK 字节存储，必须走全局 codec
// （GBK→UTF-8），与普通 text 列一致；不得强制 UTF-8（否则 latin-1 乱码）。
func TestDecodeValue_JSONB_GBKTranscode(t *testing.T) {
	withEncoding("gbk", func() {
		// "你好" 的 GBK 磁盘字节：你=C4 E3，好=BA C3
		gbk := []byte{0xC4, 0xE3, 0xBA, 0xC3}
		vl := binary.RebuildVarlena(jsonbScalarContainer(gbk))

		got := DecodeValue(JsonbOID, vl)
		want := "\"你好\""
		if got != want {
			t.Fatalf("jsonb GBK leaf = %q, want %q", got, want)
		}
	})
}

// TestDecodeValue_JSONB_UTF8Baseline 确认 UTF-8 库上 jsonb 输出直通
// （回归保护：UTF-8 数据 md5 不得因 C1 改动漂移）。
func TestDecodeValue_JSONB_UTF8Baseline(t *testing.T) {
	withEncoding("utf-8", func() {
		s := []byte("中文abc")
		got := DecodeValue(JsonbOID, binary.RebuildVarlena(jsonbScalarContainer(s)))
		want := "\"中文abc\""
		if got != want {
			t.Fatalf("jsonb UTF-8 = %q, want %q", got, want)
		}
	})
}
