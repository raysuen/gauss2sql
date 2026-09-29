// Package tuple 移植自 gauss2sql/tuple.py（Author: raysuen）
// HeapTuple 解析：行头、NULL 位图、数据区（openGauss 专用）。
package tuple

import (
	"fmt"

	"gauss2sql-go/internal/binary"
)

// HeapTupleHeaderSize 固定头 23B
const HeapTupleHeaderSize = 23

// t_infomask 位
const (
	HeapHasNull        = 0x0001
	HeapHasVarwidth    = 0x0002
	HeapHasExternal    = 0x0004
	HeapHasOid         = 0x0008
	HeapXminCommitted  = 0x0100
	HeapXminInvalid    = 0x0200
	HeapXminFrozen     = 0x0300
	HeapXmaxCommitted  = 0x0400
	HeapXmaxInvalid    = 0x0800
	HeapXmaxIsMulti    = 0x1000
	HeapXmaxLockOnly   = 0x0080
)

// HeapNattmsk t_infomask2 低 11 位
const HeapNattMask = 0x07FF

// HeapTuple 堆元组
type HeapTuple struct {
	Raw      []byte
	Xmin     uint32
	Xmax     uint32
	Field3   uint32
	Ctid     [3]uint16
	Imask2   uint16
	Imask    uint16
	HOff     uint8
	Nattrs   int
	oid      int
	nulls    []bool
	nullsSet bool
}

// New 解析元组；过短 panic（调用方 recover）
func New(raw []byte) (*HeapTuple, error) {
	if len(raw) < HeapTupleHeaderSize {
		return nil, fmt.Errorf("tuple too short: %d", len(raw))
	}
	t := &HeapTuple{Raw: raw}
	t.Xmin = uint32(binary.U16(raw, 0)) | uint32(binary.U16(raw, 2))<<16
	t.Xmax = uint32(binary.U16(raw, 4)) | uint32(binary.U16(raw, 6))<<16
	t.Field3 = uint32(binary.U16(raw, 8)) | uint32(binary.U16(raw, 10))<<16
	t.Ctid[0] = binary.U16(raw, 12)
	t.Ctid[1] = binary.U16(raw, 14)
	t.Ctid[2] = binary.U16(raw, 16)
	t.Imask2 = binary.U16(raw, 18)
	t.Imask = binary.U16(raw, 20)
	t.HOff = raw[22]
	t.Nattrs = int(t.Imask2) & HeapNattMask
	t.oid = -1
	return t, nil
}

// IsHeaderConsistent t_hoff 一致性校验
func (t *HeapTuple) IsHeaderConsistent() bool {
	if t.HOff < 24 || int(t.HOff) > 256 {
		return false
	}
	bitmapLen := 0
	if t.Imask&HeapHasNull != 0 {
		bitmapLen = (t.Nattrs + 7) / 8
	}
	oidLen := 0
	if t.Imask&HeapHasOid != 0 {
		oidLen = 4
	}
	expected := (HeapTupleHeaderSize + bitmapLen + oidLen + 7) &^ 7
	return expected == int(t.HOff)
}

// IsInsertAborted 插入已回滚
func (t *HeapTuple) IsInsertAborted() bool {
	if t.Imask&HeapXminFrozen == HeapXminFrozen {
		return false
	}
	return t.Imask&HeapXminInvalid != 0 && t.Imask&HeapXminCommitted == 0
}

// IsDeleted 已删除
func (t *HeapTuple) IsDeleted() bool {
	if t.IsInsertAborted() {
		return false
	}
	if t.Imask&HeapXmaxInvalid != 0 {
		return false
	}
	if t.Imask&HeapXmaxLockOnly != 0 {
		return false
	}
	if t.Imask&HeapXmaxIsMulti != 0 {
		return false
	}
	if t.Imask&HeapXmaxCommitted != 0 {
		return true
	}
	if t.Xmax != 0 {
		return true
	}
	return false
}

// IsLive 可见性
func (t *HeapTuple) IsLive() bool {
	if t.IsInsertAborted() {
		return false
	}
	if t.IsDeleted() {
		return false
	}
	return true
}

// GetOid OID（t_hoff-4），无 OID 返回 -1
func (t *HeapTuple) GetOid() int {
	if t.oid != -1 {
		return t.oid
	}
	if t.Imask&HeapHasOid == 0 {
		t.oid = -1
		return t.oid
	}
	off := int(t.HOff) - 4
	if off < 0 || off+4 > len(t.Raw) {
		t.oid = -1
		return t.oid
	}
	t.oid = int(binary.U32(t.Raw, off))
	return t.oid
}

// GetNulls 返回每列是否 NULL（bit 置 1 = 非空）
func (t *HeapTuple) GetNulls() []bool {
	if t.nullsSet {
		return t.nulls
	}
	n := t.Nattrs
	if t.Imask&HeapHasNull == 0 {
		t.nulls = make([]bool, n)
		t.nullsSet = true
		return t.nulls
	}
	bitmapLen := (n + 7) / 8
	if HeapTupleHeaderSize+bitmapLen > len(t.Raw) {
		t.nulls = make([]bool, n)
		t.nullsSet = true
		return t.nulls
	}
	bitmap := t.Raw[HeapTupleHeaderSize : HeapTupleHeaderSize+bitmapLen]
	res := make([]bool, n)
	for i := 0; i < n; i++ {
		by := bitmap[i/8]
		bit := by&(1<<uint(i%8)) != 0
		res[i] = !bit // bit 置 1 = 非空 → is_null = not bit
	}
	t.nulls = res
	t.nullsSet = true
	return res
}
