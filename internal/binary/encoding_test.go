package binary

import (
	"testing"
)

// TestDecodeByCodec_GBK 验证 GBK 字节 → UTF-8 转码
func TestDecodeByCodec_GBK(t *testing.T) {
	// "中文测试" 的 GBK 字节
	gbk := []byte{0xD6, 0xD0, 0xCE, 0xC4, 0xB2, 0xE2, 0xCA, 0xD4}
	got, err := decodeByCodec("gbk", gbk)
	if err != nil {
		t.Fatalf("gbk decode error: %v", err)
	}
	want := "中文测试"
	if got != want {
		t.Fatalf("gbk decode = %q, want %q", got, want)
	}
}

// TestDecodeByCodec_GB18030 验证 GB18030 字节 → UTF-8 转码
func TestDecodeByCodec_GB18030(t *testing.T) {
	// "数据库" 的 GB18030 字节（与 GBK 双字节相同，但走 GB18030 解码器）
	gb18030 := []byte{0xCA, 0xFD, 0xBE, 0xDD, 0xBF, 0xE2}
	got, err := decodeByCodec("gb18030", gb18030)
	if err != nil {
		t.Fatalf("gb18030 decode error: %v", err)
	}
	want := "数据库"
	if got != want {
		t.Fatalf("gb18030 decode = %q, want %q", got, want)
	}
}

// TestDecodeByCodec_UTF8 验证 UTF-8 直通与非法回退
func TestDecodeByCodec_UTF8(t *testing.T) {
	utf8s := []byte("Hello 中文")
	if got, _ := decodeByCodec("utf-8", utf8s); got != "Hello 中文" {
		t.Fatalf("utf-8 decode = %q", got)
	}
	// 非法 UTF-8（0xFF）应 latin-1 兜底不报错
	bad := []byte{0xFF, 0x41}
	if got, err := decodeByCodec("utf-8", bad); err != nil || got != "\u00FFA" {
		t.Fatalf("utf-8 fallback = %q, err=%v", got, err)
	}
}

// TestDecodeByCodec_Latin1 验证 latin-1 逐字节映射
func TestDecodeByCodec_Latin1(t *testing.T) {
	raw := []byte{0xE9, 0x41}
	if got, _ := decodeByCodec("latin-1", raw); got != "\u00E9A" {
		t.Fatalf("latin-1 decode = %q", got)
	}
}

// TestDecodeByCodec_SQLASCII 验证 sql_ascii 字节直通（逐字节）
func TestDecodeByCodec_SQLASCII(t *testing.T) {
	raw := []byte{0xE9, 0x41}
	if got, _ := decodeByCodec("sql_ascii", raw); got != "\u00E9A" {
		t.Fatalf("sql_ascii decode = %q", got)
	}
}

// TestDecodeByCodec_Unknown 未识别编码 latin-1 兜底
func TestDecodeByCodec_Unknown(t *testing.T) {
	raw := []byte{0x80, 0x42}
	if got, _ := decodeByCodec("euc_jp", raw); got != "\u0080B" {
		t.Fatalf("unknown decode = %q", got)
	}
}
