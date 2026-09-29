// Package binary 移植自 gauss2sql/binary.py（Author: raysuen）
// 二进制基础工具：小端读取、varlena 解析（openGauss 磁盘格式）、TOAST 压缩解压。
package binary

import (
	"encoding/binary"
	"encoding/hex"
	"sync"
)

// Version 模块版本
const Version = "0.1.0"

// ---- 库文本编码（由 main 按库探测/--encoding 设置）----
var (
	encMu    sync.RWMutex
	textEnc  = "utf-8"
)

// SetTextEncoding 设置全库文本解码编码
func SetTextEncoding(enc string) {
	encMu.Lock()
	defer encMu.Unlock()
	if enc == "" {
		enc = "utf-8"
	}
	textEnc = enc
}

// GetTextEncoding 当前文本编码
func GetTextEncoding() string {
	encMu.RLock()
	defer encMu.RUnlock()
	return textEnc
}

// DecodeBytes 按库编码解码字节；失败时 latin-1 逐字节兜底
// （字节可逆，绝不丢失）。输出统一为 UTF-8。
func DecodeBytes(raw []byte) string {
	enc := GetTextEncoding()
	if s, err := decodeByCodec(enc, raw); err == nil {
		return s
	}
	// latin-1 逐字节映射
	out := make([]rune, len(raw))
	for i, b := range raw {
		out[i] = rune(b)
	}
	return string(out)
}

// ---- 小端读取 ----

// U16 小端 uint16
func U16(b []byte, off int) uint16 { return binary.LittleEndian.Uint16(b[off:]) }

// U32 小端 uint32
func U32(b []byte, off int) uint32 { return binary.LittleEndian.Uint32(b[off:]) }

// I32 小端 int32
func I32(b []byte, off int) int32 { return int32(binary.LittleEndian.Uint32(b[off:])) }

// U64 小端 uint64
func U64(b []byte, off int) uint64 { return binary.LittleEndian.Uint64(b[off:]) }

// I64 小端 int64
func I64(b []byte, off int) int64 { return int64(binary.LittleEndian.Uint64(b[off:])) }

// Cstring 读取 C 风格 NUL 结尾字符串（按库编码解码，失败 latin-1 兜底）
func Cstring(b []byte, off int) string {
	end := off
	for end < len(b) && b[end] != 0 {
		end++
	}
	return DecodeBytes(b[off:end])
}

// Hexstr 小写 hex
func Hexstr(b []byte) string { return hex.EncodeToString(b) }

// Align4 n 向上 4 对齐
func Align4(n int) int { return (n + 3) &^ 3 }

// ---- varlena 核心 ----

// VARTAG_ONDISK = 18 (0x12)
const VARTAG_ONDISK = 18

// varlena kind
const (
	VARLENAExternal = "ext" // 18B 外联指针
	VARLENA1B       = "1b"  // 1 字节短头
	VARLENA4B       = "4b"  // 4 字节头未压缩
	VARLENA4BComp   = "4bc" // 4 字节头内联压缩
)

// VarlenaParse 解析 varlena 头。
// 返回 (kind, totalSize, payloadOff, payloadLen)；无法识别返回 ("",0,0,0)。
func VarlenaParse(b []byte, off int) (kind string, total int, poff int, plen int) {
	if off >= len(b) {
		return "", 0, 0, 0
	}
	first := b[off]
	if first == 0x01 {
		if off+2 <= len(b) && b[off+1] == VARTAG_ONDISK {
			return VARLENAExternal, 18, off + 2, 16
		}
		return "", 0, 0, 0
	}
	if first&0x01 != 0 {
		total := int(first >> 1)
		if total == 0 {
			return "", 0, 0, 0
		}
		return VARLENA1B, total, off + 1, total - 1
	}
	if off+4 > len(b) {
		return "", 0, 0, 0
	}
	word := U32(b, off)
	tot := int((word >> 2) & 0x3FFFFFFF)
	if tot < 4 {
		return "", 0, 0, 0
	}
	if first&0x03 == 0x02 {
		return VARLENA4BComp, tot, off + 4, tot - 4
	}
	return VARLENA4B, tot, off + 4, tot - 4
}

// ExtPointer 外联指针解析结果
type ExtPointer struct {
	Rawsize    uint32
	Extsize    uint32
	Valueid    uint32
	Toastrelid uint32
	Method     uint32
	Compressed bool
}

// ParseExternalPointer 解析 18B TOAST 外联指针；不合理返回 nil
func ParseExternalPointer(b []byte, off int) *ExtPointer {
	if off+18 > len(b) {
		return nil
	}
	if b[off] != 0x01 || b[off+1] != VARTAG_ONDISK {
		return nil
	}
	f1 := U32(b, off+2)
	f2 := U32(b, off+6)
	valueid := U32(b, off+10)
	toastrelid := U32(b, off+14)
	if !(100 <= valueid && valueid <= 100000000) {
		return nil
	}
	if !(100 <= toastrelid && toastrelid <= 100000000) {
		return nil
	}
	f1v := f1 & 0x3FFFFFFF
	f2v := f2 & 0x3FFFFFFF
	var rawsize, extinfo uint32
	if f1v >= f2v {
		rawsize, extinfo = f1, f2
	} else {
		rawsize, extinfo = f2, f1
	}
	if !(1 <= rawsize && rawsize <= 100000000) {
		return nil
	}
	extsize := extinfo & 0x3FFFFFFF
	method := (extinfo >> 30) & 0x03
	if !(1 <= extsize && extsize <= 100000000) {
		return nil
	}
	return &ExtPointer{
		Rawsize:    rawsize,
		Extsize:    extsize,
		Valueid:    valueid,
		Toastrelid: toastrelid,
		Method:     method,
		Compressed: extsize < rawsize-4,
	}
}

// TOAST 压缩方法
const (
	ToastCompressPglz = 0
	ToastCompressLz4  = 1
)

// PglzDecompress PGLZ 解压；失败返回 nil
func PglzDecompress(data []byte, expectedSize int) []byte {
	out := make([]byte, 0, expectedSize)
	sp, n := 0, len(data)
	destEnd := expectedSize
	for sp < n && len(out) < destEnd {
		ctrl := data[sp]
		sp++
		for i := 0; i < 8; i++ {
			if sp >= n || len(out) >= destEnd {
				break
			}
			if ctrl&1 != 0 {
				// match
				if sp+2 > n {
					return nil
				}
				b1 := data[sp]
				b2 := data[sp+1]
				sp += 2
				length := int(b1&0x0F) + 3
				off := int((uint16(b1&0xF0) << 4)) | int(b2)
				if length == 18 {
					if sp >= n {
						return nil
					}
					length += int(data[sp])
					sp++
				}
				if off == 0 {
					return nil
				}
				if len(out) < off {
					return nil
				}
				remaining := length
				if destEnd-len(out) < remaining {
					remaining = destEnd - len(out)
				}
				src := len(out) - off
				for k := 0; k < remaining; k++ {
					out = append(out, out[src+k])
				}
			} else {
				out = append(out, data[sp])
				sp++
			}
			ctrl >>= 1
		}
	}
	if len(out) != expectedSize {
		return nil
	}
	return out
}

// Lz4BlockDecompress LZ4 block 解压；失败返回 nil
func Lz4BlockDecompress(data []byte, expectedSize int) []byte {
	out := make([]byte, 0, expectedSize)
	sp, n := 0, len(data)
	for sp < n {
		token := data[sp]
		sp++
		litLen := int(token >> 4)
		if litLen == 15 {
			for {
				if sp >= n {
					return nil
				}
				b := data[sp]
				sp++
				litLen += int(b)
				if b != 255 {
					break
				}
			}
		}
		if sp+litLen > n {
			return nil
		}
		out = append(out, data[sp:sp+litLen]...)
		sp += litLen
		if sp >= n {
			break
		}
		if sp+2 > n {
			return nil
		}
		offset := int(data[sp]) | int(data[sp+1])<<8
		sp += 2
		if offset == 0 {
			return nil
		}
		matchLen := int(token&0x0F) + 4
		if token&0x0F == 15 {
			for {
				if sp >= n {
					return nil
				}
				b := data[sp]
				sp++
				matchLen += int(b)
				if b != 255 {
					break
				}
			}
		}
		if len(out) < offset {
			return nil
		}
		src := len(out) - offset
		for k := 0; k < matchLen; k++ {
			out = append(out, out[src+k])
		}
	}
	if len(out) != expectedSize {
		return nil
	}
	return out
}

// ToastDecompress 解压 TOAST 压缩数据；失败返回 nil
func ToastDecompress(payload []byte, expectedSize int, method int) []byte {
	if len(payload) < 5 {
		return nil
	}
	tcinfo := U32(payload, 0)
	rawsize := int(tcinfo & 0x3FFFFFFF)
	tcMethod := (tcinfo >> 30) & 0x03
	if rawsize != expectedSize {
		return nil
	}
	body := payload[4:]
	if tcMethod == ToastCompressLz4 {
		return Lz4BlockDecompress(body, rawsize)
	}
	if tcMethod == ToastCompressPglz {
		return PglzDecompress(body, rawsize)
	}
	return nil
}

// RebuildVarlena 将纯 payload 重建为合法 varlena（含头）
func RebuildVarlena(payload []byte) []byte {
	total := 4 + len(payload)
	if total < 128 {
		hdr := byte(byte(total<<1) | 1)
		out := make([]byte, 0, total)
		out = append(out, hdr)
		out = append(out, payload...)
		return out
	}
	var hdr [4]byte
	binary.LittleEndian.PutUint32(hdr[:], uint32(total<<2))
	out := make([]byte, 0, total)
	out = append(out, hdr[:]...)
	out = append(out, payload...)
	return out
}
