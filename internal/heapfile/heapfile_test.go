package heapfile

import (
	"testing"

	"gauss2sql-go/internal/meta"
)

// TestNullVsEmptyString 验证 NULL 与空字符串 '' 严格区分：
// NULL -> NullMarker（导出为 NULL/\N）；空串 varlena -> ""（导出为 ''/空字段）
func TestNullVsEmptyString(t *testing.T) {
	tm := &meta.TableMeta{
		Columns: []*meta.Column{
			{Name: "a", Atttypid: 23, Attlen: 4, Attalign: "i", Attbyval: true},  // int4
			{Name: "b", Atttypid: 25, Attlen: -1, Attalign: "i", Attbyval: false}, // text
		},
	}
	// 行1：a=NULL, b=空串''（1B varlena 头 0x00，长度 0）
	fields := [][]byte{nil, {0x00}}
	vals := DecodeFields(fields, tm, nil)
	if len(vals) != 2 {
		t.Fatalf("vals len = %d, want 2", len(vals))
	}
	if vals[0] != NullMarker {
		t.Fatalf("NULL 应解码为 NullMarker, got %q", vals[0])
	}
	if vals[1] != "" {
		t.Fatalf("空串应解码为 \"\", got %q", vals[1])
	}
}

// TestNullMarkerNotConflicting 验证 NullMarker 与真实文本不冲突（以 \x00 开头，
// openGauss 文本类型不含裸 NUL）
func TestNullMarkerNotConflicting(t *testing.T) {
	if len(NullMarker) == 0 || NullMarker[0] != 0x00 {
		t.Fatalf("NullMarker 必须以 \\x00 开头: %q", NullMarker)
	}
}
