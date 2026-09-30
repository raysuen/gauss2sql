// Package types 移植自 gauss2sql/types.py（Author: raysuen）
// openGauss 内置类型解码，输出与 PG *_out 一致。
package types

import (
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"gauss2sql-go/internal/binary"
)

// ---- 类型 OID ----
const (
	BoolOID      = 16
	ByteaOID     = 17
	CharOID      = 18
	NameOID      = 19
	Int8OID      = 20
	Int2OID      = 21
	Int2VecOID   = 22
	Int4OID      = 23
	TextOID      = 25
	OidOID       = 26
	TidOID       = 27
	XidOID       = 28
	CidOID       = 29
	OidVecOID    = 30
	JSONOID      = 114
	CidrOID      = 650
	Float4OID    = 700
	Float8OID    = 701
	UnknownOID   = 705
	MoneyOID     = 790
	MacaddrOID   = 829
	InetOID      = 869
	BpcharOID    = 1042
	VarcharOID   = 1043
	DateOID      = 1082
	TimeOID      = 1083
	TimestampOID = 1114
	TstzOID      = 1184
	IntervalOID  = 1186
	TimetzOID    = 1266
	BitOID       = 1560
	VarbitOID    = 1562
	NumericOID   = 1700
	UuidOID      = 2950
	JsonbOID     = 3802
	AclitemOID   = 1033
)

// TypeNames 内置类型名（pg_type 未命中时兜底）
var TypeNames = map[int]string{
	BoolOID: "bool", ByteaOID: "bytea", CharOID: "char", NameOID: "name",
	Int8OID: "bigint", Int2OID: "smallint", Int4OID: "integer", TextOID: "text",
	OidOID: "oid", TidOID: "tid", XidOID: "xid", CidOID: "cid", JSONOID: "json",
	Float4OID: "real", Float8OID: "double precision", MoneyOID: "money",
	MacaddrOID: "macaddr", InetOID: "inet", CidrOID: "cidr", BpcharOID: "character",
	VarcharOID: "character varying", DateOID: "date", TimeOID: "time",
	TimestampOID: "timestamp", TstzOID: "timestamptz", IntervalOID: "interval",
	TimetzOID: "timetz", BitOID: "bit", VarbitOID: "varbit", NumericOID: "numeric",
	UuidOID: "uuid", JsonbOID: "jsonb",
}

// VarlenaTypes 可能 TOAST 外联的类型
var VarlenaTypes = map[int]bool{
	TextOID: true, VarcharOID: true, BpcharOID: true, ByteaOID: true,
	JSONOID: true, JsonbOID: true, NumericOID: true, BitOID: true, VarbitOID: true,
	86: true, 88: true, 90: true, 3969: true, 33: true, 32: true,
	InetOID: true, CidrOID: true,
}

// 枚举映射 {类型oid: {成员oid: 标签}}，由 catalog 注入
var (
	enumLookup  = map[int]map[int]string{}
	enumLabels  = map[int][]string{}
	roleNameMap = map[int]string{}
)

// SetEnumMap 注入 pg_enum 映射（ordered 保持堆扫描顺序）
func SetEnumMap(m map[int]map[int]string) {
	enumLookup = m
	enumLabels = map[int][]string{}
}

// AddEnumMember 按堆扫描顺序追加成员
func AddEnumMember(typeOid, memberOid int, label string) {
	if enumLookup[typeOid] == nil {
		enumLookup[typeOid] = map[int]string{}
	}
	enumLookup[typeOid][memberOid] = label
	enumLabels[typeOid] = append(enumLabels[typeOid], label)
}

// SetRoleNameMap 注入角色名映射
func SetRoleNameMap(m map[int]string) {
	roleNameMap = map[int]string{}
	for k, v := range m {
		roleNameMap[k] = v
	}
}

// EnumLabels 返回某枚举类型的所有标签（保持 pg_enum 堆扫描顺序）
func EnumLabels(oid int) []string {
	return enumLabels[oid]
}

// EnumLookupAll 返回全部枚举成员映射 类型oid -> {成员oid -> 标签}
// （--export-meta 导出用；map 无序，仅用于解码查表）
func EnumLookupAll() map[int]map[int]string {
	out := map[int]map[int]string{}
	for oid, members := range enumLookup {
		cp := map[int]string{}
		for k, v := range members {
			cp[k] = v
		}
		out[oid] = cp
	}
	return out
}

// EnumLabelsAll 返回全部枚举有序标签 类型oid -> []标签
// （--export-meta 导出用；顺序即 pg_enum 堆扫描顺序，DDL 重建 CREATE TYPE 依赖）
func EnumLabelsAll() map[int][]string {
	out := map[int][]string{}
	for oid, ls := range enumLabels {
		out[oid] = append([]string{}, ls...)
	}
	return out
}

// SetEnumLabels 注入 类型oid -> 有序标签列表（--catalog-json 重建 DDL 用）
func SetEnumLabels(m map[int][]string) {
	enumLabels = map[int][]string{}
	for oid, ls := range m {
		enumLabels[oid] = append([]string{}, ls...)
	}
}

// ---- varlena payload 提取 ----

func varPayload(b []byte) (payload []byte, isExt bool, ext *binary.ExtPointer) {
	if len(b) == 0 {
		return nil, false, nil
	}
	kind, total, _, _ := binary.VarlenaParse(b, 0)
	switch kind {
	case binary.VARLENAExternal:
		e := binary.ParseExternalPointer(b, 0)
		if e != nil {
			return nil, true, e
		}
		return nil, false, nil
	case binary.VARLENA1B:
		end := 1 + max(total-1, 0)
		if end > len(b) {
			end = len(b)
		}
		return b[1:end], false, nil
	case binary.VARLENA4B:
		end := 4 + max(total-4, 0)
		if end > len(b) {
			end = len(b)
		}
		return b[4:end], false, nil
	case binary.VARLENA4BComp:
		if total > len(b) {
			return nil, false, nil
		}
		comp := b[4:total]
		if len(comp) >= 5 {
			rawlen := int(binary.U32(comp, 0))
			if rawlen > 0 && rawlen <= 1000000000 {
				if data := binary.PglzDecompress(comp[4:], rawlen); data != nil {
					return data, false, nil
				}
			}
		}
		return nil, false, nil
	}
	return nil, false, nil
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// ---- 浮点 ----

func fmtFloat(v float64) string {
	if math.IsNaN(v) {
		return "NaN"
	}
	if math.IsInf(v, 0) {
		if v > 0 {
			return "Infinity"
		}
		return "-Infinity"
	}
	return pythonRepr(v)
}

// pythonRepr 模拟 Python repr(float)：最短 round-trip，科学计数阈值 E<-4 or E>=16
func pythonRepr(v float64) string {
	if v == 0 {
		if math.Signbit(v) {
			return "-0.0"
		}
		return "0.0"
	}
	sci := strconv.FormatFloat(v, 'e', -1, 64)
	// sci 形如 "2.5e+10" / "1.5e-07" / "3.141590118408203e+00"
	ei := strings.IndexByte(sci, 'e')
	mantissa := sci[:ei]
	expStr := sci[ei+1:]
	sign := ""
	if mantissa[0] == '-' {
		sign = "-"
		mantissa = mantissa[1:]
	}
	exp, _ := strconv.Atoi(expStr)
	// mantissa digits: "2.5" → digits="25", decimal point after 1 digit
	digits := strings.Replace(mantissa, ".", "", 1)
	for strings.HasSuffix(digits, "0") {
		digits = digits[:len(digits)-1]
	}
	n := len(digits) // significant digits
	// E = exponent of leading digit
	if exp < -4 || exp >= 16 {
		// scientific: d.ddd e+XX
		out := sign + digits[:1]
		if n > 1 {
			out += "." + digits[1:]
		}
		out += "e"
		if exp >= 0 {
			out += "+"
		}
		out += strconv.Itoa(exp)
		return out
	}
	// fixed
	if exp >= n-1 {
		// digits followed by zeros
		out := sign + digits + strings.Repeat("0", exp-(n-1)) + ".0"
		return out
	}
	if exp >= 0 {
		out := sign + digits[:exp+1]
		if n > exp+1 {
			out += "." + digits[exp+1:]
		} else {
			out += ".0"
		}
		return out
	}
	// exp < 0
	out := sign + "0." + strings.Repeat("0", -exp-1) + digits
	return out
}

// ---- 基础解码 ----

func decodeBool(b []byte) string {
	if len(b) == 0 {
		return "false"
	}
	v := b[0]
	if v == 1 || v == 0x74 || v == 't' || v == 'T' {
		return "true"
	}
	return "false"
}

func decodeInt2(b []byte) string { return strconv.FormatInt(int64(int16(binary.U16(b, 0))), 10) }
func decodeInt4(b []byte) string { return strconv.FormatInt(int64(binary.I32(b, 0)), 10) }
func decodeInt8(b []byte) string { return strconv.FormatInt(binary.I64(b, 0), 10) }

func decodeFloat4(b []byte) string {
	return fmtFloat(float64(math.Float32frombits(binary.U32(b, 0))))
}
func decodeFloat8(b []byte) string {
	return fmtFloat(math.Float64frombits(binary.U64(b, 0)))
}

func decodeText(b []byte) string {
	p, _, _ := varPayload(b)
	return binary.DecodeBytes(p)
}
func decodeName(b []byte) string {
	end := 0
	for end < len(b) && b[end] != 0 {
		end++
	}
	return binary.DecodeBytes(b[:end])
}
func decodeBytea(b []byte) string {
	p, _, _ := varPayload(b)
	return `\x` + hex.EncodeToString(p)
}
func decodeOid(b []byte) string { return strconv.FormatUint(uint64(binary.U32(b, 0)), 10) }

func decodeXid(b []byte) string {
	if len(b) >= 8 {
		return strconv.FormatUint(binary.U64(b, 0), 10)
	}
	return strconv.FormatUint(uint64(binary.U32(b, 0)), 10)
}

func decodeTid(b []byte) string {
	hi := binary.U16(b, 0)
	lo := binary.U16(b, 2)
	off := binary.U16(b, 4)
	return fmt.Sprintf("(%d,%d)", uint32(hi)<<16|uint32(lo), off)
}

// ---- 时间 ----

var epoch2000 = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

func decodeDate(b []byte) string {
	days := binary.I32(b, 0)
	if days == math.MaxInt32 {
		return "infinity"
	}
	if days == math.MinInt32 {
		return "-infinity"
	}
	t := epoch2000.AddDate(0, 0, int(days))
	return t.Format("2006-01-02")
}

func timeFromUS(us int64) string {
	us %= 86400000000
	h := us / 3600000000
	us %= 3600000000
	m := us / 60000000
	us %= 60000000
	s := us / 1000000
	micro := us % 1000000
	if micro != 0 {
		return fmt.Sprintf("%02d:%02d:%02d.%06d", h, m, s, micro)
	}
	return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
}

func decodeTime(b []byte) string {
	us := binary.I64(b, 0)
	return timeFromUS(us)
}

func tsFormat(us int64, tz bool) string {
	if us == math.MaxInt64 {
		return "infinity"
	}
	if us == math.MinInt64 {
		return "-infinity"
	}
	t := epoch2000.Add(time.Duration(us) * time.Microsecond)
	s := t.Format("2006-01-02 15:04:05")
	micro := t.Nanosecond() / 1000
	if micro != 0 {
		s += fmt.Sprintf(".%06d", micro)
	}
	if tz {
		s += "+00"
	}
	return s
}

func decodeTimestamp(b []byte) string   { return tsFormat(binary.I64(b, 0), false) }
func decodeTstz(b []byte) string         { return tsFormat(binary.I64(b, 0), true) }
func decodeSmalldatetime(b []byte) string { return tsFormat(binary.I64(b, 0), false) }

func decodeTimetz(b []byte) string {
	us := binary.I64(b, 0)
	zone := binary.I32(b, 8)
	t := timeFromUS(us)
	sign := "-"
	if zone < 0 {
		sign = "+"
	}
	if zone < 0 {
		zone = -zone
	}
	return fmt.Sprintf("%s%s%02d:%02d", t, sign, zone/3600, (zone%3600)/60)
}

func decodeInterval(b []byte) string {
	timeUS := binary.I64(b, 0)
	day := binary.I32(b, 8)
	month := binary.I32(b, 12)
	year := int(month / 12)
	month -= int32(year * 12)
	var parts []string
	if year != 0 {
		if year == 1 {
			parts = append(parts, "1 year")
		} else {
			parts = append(parts, fmt.Sprintf("%d years", year))
		}
	}
	if month != 0 {
		if month == 1 {
			parts = append(parts, "1 mon")
		} else {
			parts = append(parts, fmt.Sprintf("%d mons", month))
		}
	}
	if day != 0 {
		if day < 0 {
			if day == -1 {
				parts = append(parts, "-1 day")
			} else {
				parts = append(parts, fmt.Sprintf("%d days", day))
			}
		} else {
			if day == 1 {
				parts = append(parts, "1 day")
			} else {
				parts = append(parts, fmt.Sprintf("%d days", day))
			}
		}
	}
	sign := ""
	if timeUS < 0 {
		sign = "-"
	}
	t := timeUS
	if t < 0 {
		t = -t
	}
	us := t % 1000000
	totalS := t / 1000000
	hh := totalS / 3600
	rem := totalS % 3600
	mm := rem / 60
	ss := rem % 60
	timeS := fmt.Sprintf("%s%02d:%02d:%02d", sign, hh, mm, ss)
	if us != 0 {
		timeS += strings.TrimSuffix(fmt.Sprintf(".%06d", us), "0")
	}
	if timeUS != 0 {
		parts = append(parts, timeS)
	}
	if len(parts) == 0 {
		return "00:00:00"
	}
	return strings.Join(parts, " ")
}

// ---- numeric ----

func numericDiskToStr(payload []byte) string {
	if len(payload) < 2 {
		return "0"
	}
	header := binary.U16(payload, 0)
	flags := header & 0xC000
	var neg bool
	var dscale int
	var weight int
	var digitsRaw []byte
	if flags == 0xC000 {
		if header == 0xD000 {
			return "Infinity"
		}
		if header == 0xF000 {
			return "-Infinity"
		}
		return "NaN"
	}
	if flags == 0x8000 {
		neg = header&0x2000 != 0
		dscale = int((header & 0x1F80) >> 7)
		if header&0x0040 != 0 {
			weight = int((^uint16(0x3F) | (header & 0x3F)) & 0xFFFF)
		} else {
			weight = int(header & 0x3F)
		}
		if weight&0x8000 != 0 {
			weight -= 0x10000
		}
		digitsRaw = payload[2:]
	} else {
		if len(payload) < 4 {
			return "0"
		}
		neg = header&0x4000 != 0
		dscale = int(header & 0x3FFF)
		weight = int(int16(binary.U16(payload, 2)))
		digitsRaw = payload[4:]
	}
	ndigits := len(digitsRaw) / 2
	digits := make([]int, ndigits)
	for i := 0; i < ndigits; i++ {
		digits[i] = int(binary.U16(digitsRaw, i*2))
	}
	var intStr string
	var fracStart int
	if weight < 0 {
		intStr = "0"
		fracStart = weight + 1
	} else {
		var sb strings.Builder
		for i := 0; i <= weight; i++ {
			var d int
			if i < ndigits {
				d = digits[i]
			}
			if i == 0 {
				sb.WriteString(strconv.Itoa(d))
			} else {
				sb.WriteString(fmt.Sprintf("%04d", d))
			}
		}
		intStr = strings.TrimLeft(sb.String(), "0")
		if intStr == "" {
			intStr = "0"
		}
		fracStart = weight + 1
	}
	var result string
	if dscale > 0 {
		ngroups := (dscale + 3) / 4
		var frac strings.Builder
		for g := 0; g < ngroups; g++ {
			di := fracStart + g
			var d int
			if di >= 0 && di < ndigits {
				d = digits[di]
			}
			frac.WriteString(fmt.Sprintf("%04d", d))
		}
		f := frac.String()
		if len(f) > dscale {
			f = f[:dscale]
		}
		result = intStr + "." + f
	} else {
		result = intStr
	}
	if neg {
		result = "-" + result
	}
	return result
}

func decodeNumeric(b []byte) string {
	p, _, _ := varPayload(b)
	return numericDiskToStr(p)
}

// ---- uuid ----

func decodeUuid(b []byte) string {
	if len(b) < 16 {
		return ""
	}
	return fmt.Sprintf("%02x%02x%02x%02x-%02x%02x-%02x%02x-%02x%02x-%02x%02x%02x%02x%02x%02x",
		b[0], b[1], b[2], b[3], b[4], b[5], b[6], b[7],
		b[8], b[9], b[10], b[11], b[12], b[13], b[14], b[15])
}

// ---- jsonb ----

const (
	jbFScalar    = 0x10000000
	jbFObject    = 0x20000000
	jbFArray     = 0x40000000
	jTypeMask    = 0x70000000
	jTypeNum     = 0x10000000
	jTypeCont    = 0x20000000
	jTypeFalse   = 0x30000000
	jTypeNull    = 0x40000000
	jTypeTrue    = 0x70000000
	jOffMask     = 0x0FFFFFFF
)

func jsonQuote(s string) string {
	var sb strings.Builder
	sb.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			sb.WriteString("\\\"")
		case '\\':
			sb.WriteString("\\\\")
		case '\n':
			sb.WriteString("\\n")
		case '\r':
			sb.WriteString("\\r")
		case '\t':
			sb.WriteString("\\t")
		default:
			if r < 0x20 || r == 0x7f {
				sb.WriteString(fmt.Sprintf("\\u%04x", r))
			} else {
				sb.WriteRune(r)
			}
		}
	}
	sb.WriteByte('"')
	return sb.String()
}

func jsonbScalarToText(je uint32, payload []byte, dataOff, s, e int) string {
	t := je & jTypeMask
	switch t {
	case 0:
		return jsonQuote(binary.DecodeBytes(payload[dataOff+s : dataOff+e]))
	case jTypeNum:
		off := (s + 3) &^ 3
		p, _, _ := varPayload(payload[dataOff+off : dataOff+e])
		return numericDiskToStr(p)
	case jTypeTrue:
		return "true"
	case jTypeFalse:
		return "false"
	case jTypeNull:
		return "null"
	case jTypeCont:
		off := (s + 3) &^ 3
		return jsonbContainerToText(payload[dataOff+off : dataOff+e])
	}
	return "null"
}

func jsonbContainerToText(payload []byte) string {
	if len(payload) < 4 {
		return "null"
	}
	header := binary.U32(payload, 0)
	flags := header & 0xF0000000
	count := int(header & jOffMask)
	scalar := flags&jbFScalar != 0
	isObj := flags&jbFObject != 0
	nEnt := count * 2
	if !isObj {
		nEnt = count
	}
	dataOff := 4 + 4*nEnt
	if dataOff > len(payload) {
		return "null"
	}
	ents := make([]uint32, nEnt)
	for i := 0; i < nEnt; i++ {
		ents[i] = binary.U32(payload, 4+4*i)
	}
	startOff := func(idx int) int {
		if idx == 0 {
			return 0
		}
		prevEnd := int(ents[idx-1] & jOffMask)
		if ents[idx]&jTypeMask == jTypeCont || ents[idx]&jTypeMask == jTypeNum {
			return (prevEnd + 3) &^ 3
		}
		return prevEnd
	}
	emit := func(idx int) string {
		s := startOff(idx)
		e := int(ents[idx] & jOffMask)
		return jsonbScalarToText(ents[idx], payload, dataOff, s, e)
	}
	if scalar {
		return emit(0)
	}
	if isObj {
		var pairs []string
		for i := 0; i < count; i++ {
			pairs = append(pairs, emit(2*i)+": "+emit(2*i+1))
		}
		return "{" + strings.Join(pairs, ", ") + "}"
	}
	var items []string
	for i := 0; i < count; i++ {
		items = append(items, emit(i))
	}
	return "[" + strings.Join(items, ", ") + "]"
}

func decodeJsonb(b []byte) string {
	p, _, _ := varPayload(b)
	return jsonbContainerToText(p)
}

// ---- inet / cidr ----

func decodeInet(b []byte, forceCidr bool) string {
	p, _, _ := varPayload(b)
	if len(p) < 2 {
		return ""
	}
	family := p[0]
	bits := p[1]
	if family != 2 && family != 3 {
		return ""
	}
	var s string
	if family == 2 {
		addr := p[2:6]
		s = fmt.Sprintf("%d.%d.%d.%d", addr[0], addr[1], addr[2], addr[3])
	} else {
		addr := p[2:18]
		var parts []string
		for i := 0; i < len(addr); i += 2 {
			parts = append(parts, fmt.Sprintf("%x", uint16(addr[i])<<8|uint16(addr[i+1])))
		}
		s = strings.Join(parts, ":")
	}
	defBits := 32
	if family == 3 {
		defBits = 128
	}
	if forceCidr || int(bits) != defBits {
		return fmt.Sprintf("%s/%d", s, bits)
	}
	return s
}

func decodeMacaddr(b []byte) string {
	if len(b) < 6 {
		return ""
	}
	return fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x", b[0], b[1], b[2], b[3], b[4], b[5])
}

// ---- bit ----

func decodeBit(b []byte) string {
	p, _, _ := varPayload(b)
	if len(p) < 4 {
		return ""
	}
	nbits := binary.I32(p, 0)
	data := p[4:]
	var sb strings.Builder
	for i := 0; i < int(nbits); i++ {
		by := data[i/8]
		if by&(1<<uint(7-(i%8))) != 0 {
			sb.WriteByte('1')
		} else {
			sb.WriteByte('0')
		}
	}
	return sb.String()
}

func decodeMoney(b []byte) string {
	v := binary.I64(b, 0)
	sign := ""
	if v < 0 {
		sign = "-"
		v = -v
	}
	return fmt.Sprintf("%s%d.%02d", sign, v/100, v%100)
}

// ---- char / text 变体 ----

func decodeChar(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return string(rune(b[0]))
}

// ---- 数组 ----

type elemInfo struct {
	alen  int
	align byte
	fn    func([]byte) string
}

var arrayElemInfo = map[int]elemInfo{
	BoolOID:      {1, 'c', decodeBool},
	ByteaOID:     {-1, 'i', nil},
	CharOID:      {1, 'c', decodeChar},
	NameOID:      {64, 'c', decodeName},
	Int8OID:      {8, 'd', decodeInt8},
	Int2OID:      {2, 's', decodeInt2},
	Int4OID:      {4, 'i', decodeInt4},
	TextOID:      {-1, 'i', nil},
	OidOID:       {4, 'i', decodeOid},
	Float4OID:    {4, 'i', decodeFloat4},
	Float8OID:    {8, 'd', decodeFloat8},
	UnknownOID:   {-1, 'i', nil},
	BpcharOID:    {-1, 'i', nil},
	VarcharOID:   {-1, 'i', nil},
	DateOID:      {4, 'i', decodeDate},
	TimeOID:      {8, 'd', decodeTime},
	TimestampOID: {8, 'd', decodeTimestamp},
	TstzOID:      {8, 'd', decodeTstz},
	TimetzOID:    {12, 'd', decodeTimetz},
	IntervalOID:  {16, 'd', decodeInterval},
	NumericOID:   {-1, 'i', nil},
	UuidOID:      {16, 'c', decodeUuid},
	JsonbOID:     {-1, 'i', decodeJsonb},
	XidOID:       {8, 'd', decodeXid},
	9003:         {8, 'd', decodeSmalldatetime},
	5545:         {1, 'c', func(b []byte) string { return strconv.FormatInt(int64(int8(b[0])), 10) }},
}

var alignSizes = map[byte]int{'c': 1, 's': 2, 'i': 4, 'd': 8}

func arrayElemText(payload []byte, pos int, elemOid int) (string, int) {
	info, ok := arrayElemInfo[elemOid]
	if !ok {
		first := payload[pos]
		if first&1 != 0 {
			total := int(first >> 1)
			return binary.DecodeBytes(payload[pos+1 : pos+total]), pos + total
		}
		total := int(binary.U32(payload, pos) >> 2)
		return binary.DecodeBytes(payload[pos+4 : pos+total]), pos + total
	}
	alen := info.alen
	if alen == -1 {
		first := payload[pos]
		if first&1 != 0 {
			total := int(first >> 1)
			seg := payload[pos+1 : pos+total]
			if info.fn != nil {
				return info.fn(seg), pos + total
			}
			return binary.DecodeBytes(seg), pos + total
		}
		a := alignSizes[info.align]
		if a > 1 && pos%a != 0 {
			pos = (pos + a - 1) &^ (a - 1)
		}
		total := int(binary.U32(payload, pos) >> 2)
		seg := payload[pos+4 : pos+total]
		if info.fn != nil {
			return info.fn(seg), pos + total
		}
		return binary.DecodeBytes(seg), pos + total
	}
	a := alignSizes[info.align]
	if a > 1 {
		pos = (pos + a - 1) &^ (a - 1)
	}
	seg := payload[pos : pos+alen]
	if info.fn != nil {
		return info.fn(seg), pos + alen
	}
	return binary.DecodeBytes(seg), pos + alen
}

func arrayQuote(s string) string {
	if s == "NULL" {
		return s
	}
	needs := s == ""
	if !needs {
		for _, ch := range s {
			if ch == ',' || ch == '{' || ch == '}' || ch == '"' || ch == '\\' {
				needs = true
				break
			}
		}
	}
	if !needs {
		r := []rune(s)
		if isWS(r[0]) || isWS(r[len(r)-1]) {
			needs = true
		}
	}
	if !needs {
		return s
	}
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\"", "\\\"")
	return "\"" + s + "\""
}

func isWS(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r'
}

func decodeArray(b []byte) string {
	p, _, _ := varPayload(b)
	if len(p) < 12 {
		return "{}"
	}
	ndim := int(binary.I32(p, 0))
	dataoffset := int(binary.I32(p, 4))
	if ndim <= 0 || ndim > 6 {
		return "{}"
	}
	dims := make([]int, ndim)
	nelems := 1
	for i := 0; i < ndim; i++ {
		d := int(binary.I32(p, 12+4*i))
		if d <= 0 {
			return "{}"
		}
		dims[i] = d
		nelems *= d
	}
	bodyOff := 12 + 8*ndim
	var nulls []bool
	var elemOff int
	if dataoffset > 0 {
		bmLen := (nelems + 7) / 8
		bm := p[bodyOff : bodyOff+bmLen]
		nulls = make([]bool, nelems)
		for i := 0; i < nelems; i++ {
			by := bm[i/8]
			bit := by&(1<<uint(i%8)) != 0
			nulls[i] = !bit
		}
		elemOff = dataoffset - 4
		elemOff = (elemOff + 3) &^ 3
	} else {
		elemOff = bodyOff
	}
	elemtype := int(binary.U32(p, 8))
	texts := make([]string, 0, nelems)
	pos := elemOff
	corrupt := false
	for i := 0; i < nelems; i++ {
		if nulls != nil && nulls[i] {
			texts = append(texts, "NULL")
			continue
		}
		t, np, err := safeElemText(p, pos, elemtype)
		if err != nil {
			texts = append(texts, "__ARRAY_CORRUPT__")
			corrupt = true
			break
		}
		texts = append(texts, t)
		pos = np
	}
	if corrupt {
		return "{" + strings.Join(texts, ",") + "}"
	}
	// 嵌套组装
	var nest func(items []string, dimIdx int) interface{}
	nest = func(items []string, dimIdx int) interface{} {
		d := dims[dimIdx]
		if dimIdx == ndim-1 {
			return items[:d]
		}
		var out []interface{}
		cnt := 0
		for j := 0; j < d; j++ {
			sub := nest(items[cnt:], dimIdx+1)
			out = append(out, sub)
			// count consumed
			cnt += countLeaves(sub)
		}
		return out
	}
	var out string
	if ndim == 1 {
		out = "{" + strings.Join(quoteList(texts), ",") + "}"
	} else {
		nested := nest(texts, 0)
		out = formatNested(nested)
	}
	return out
}

func countLeaves(v interface{}) int {
	switch x := v.(type) {
	case []string:
		return len(x)
	case []interface{}:
		n := 0
		for _, e := range x {
			n += countLeaves(e)
		}
		return n
	}
	return 0
}

func quoteList(items []string) []string {
	out := make([]string, len(items))
	for i, s := range items {
		out[i] = arrayQuote(s)
	}
	return out
}

func formatNested(v interface{}) string {
	switch x := v.(type) {
	case []string:
		return "{" + strings.Join(quoteList(x), ",") + "}"
	case []interface{}:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = formatNested(e)
		}
		return "{" + strings.Join(parts, ",") + "}"
	}
	return "{}"
}

func safeElemText(payload []byte, pos int, elemOid int) (string, int, error) {
	defer func() {
		recover()
	}()
	t, np := arrayElemText(payload, pos, elemOid)
	return t, np, nil
}

// ---- sql_string_literal ----

// SQLStringLiteral 把值转成安全 SQL 字符串字面量
func SQLStringLiteral(v string) string {
	needs := false
	for _, r := range v {
		o := int(r)
		if r == '\'' || r == '\\' || o < 32 || o == 127 {
			needs = true
			break
		}
	}
	if !needs {
		return "'" + v + "'"
	}
	var sb strings.Builder
	sb.WriteString("E'")
	for _, r := range v {
		o := int(r)
		switch r {
		case '\'':
			sb.WriteString("''")
		case '\\':
			sb.WriteString("\\\\")
		case '\n':
			sb.WriteString("\\n")
		case '\r':
			sb.WriteString("\\r")
		case '\t':
			sb.WriteString("\\t")
		default:
			if o < 32 || o == 127 {
				sb.WriteString(fmt.Sprintf("\\x%02X", o))
			} else {
				sb.WriteRune(r)
			}
		}
	}
	sb.WriteString("'")
	return sb.String()
}

// ---- decode_value 主入口 ----

// DecodeValue 按 OID 解码字段值（raw 为字段原始字节，含 varlena 头）
func DecodeValue(oid int, raw []byte) string {
	if raw == nil {
		return "NULL"
	}
	if em, ok := enumLookup[oid]; ok && len(raw) >= 4 {
		memberOid := binary.U32(raw, 0)
		if lbl, ok := em[int(memberOid)]; ok {
			return lbl
		}
		return strconv.FormatUint(uint64(memberOid), 10)
	}
	var s string
	switch oid {
	case BoolOID:
		s = decodeBool(raw)
	case Int2OID:
		s = decodeInt2(raw)
	case Int4OID, OidOID, CidOID:
		s = decodeInt4(raw)
	case Int8OID:
		s = decodeInt8(raw)
	case Float4OID:
		s = decodeFloat4(raw)
	case Float8OID:
		s = decodeFloat8(raw)
	case TextOID, VarcharOID, BpcharOID, JSONOID:
		s = decodeText(raw)
	case NameOID:
		s = decodeName(raw)
	case ByteaOID:
		s = decodeBytea(raw)
	case XidOID:
		s = decodeXid(raw)
	case TidOID:
		s = decodeTid(raw)
	case DateOID:
		s = decodeDate(raw)
	case TimeOID:
		s = decodeTime(raw)
	case TimestampOID:
		s = decodeTimestamp(raw)
	case TstzOID:
		s = decodeTstz(raw)
	case TimetzOID:
		s = decodeTimetz(raw)
	case IntervalOID:
		s = decodeInterval(raw)
	case NumericOID:
		s = decodeNumeric(raw)
	case UuidOID:
		s = decodeUuid(raw)
	case JsonbOID:
		s = decodeJsonb(raw)
	case InetOID:
		s = decodeInet(raw, false)
	case CidrOID:
		s = decodeInet(raw, true)
	case MacaddrOID:
		s = decodeMacaddr(raw)
	case BitOID, VarbitOID:
		s = decodeBit(raw)
	case MoneyOID:
		s = decodeMoney(raw)
	case CharOID:
		s = decodeChar(raw)
	case 9003:
		s = decodeSmalldatetime(raw)
	default:
		if _, isArr := arrayTypeSet[oid]; isArr {
			s = decodeArray(raw)
		} else {
			s = decodeText(raw)
		}
	}
	return s
}

// arrayTypeSet 数组类型 OID 集合
var arrayTypeSet = map[int]bool{
	1000: true, 1001: true, 1002: true, 1003: true, 1005: true, 1006: true,
	1007: true, 1008: true, 1009: true, 1010: true, 1011: true, 1012: true,
	1013: true, 1014: true, 1015: true, 1016: true, 1017: true, 1018: true,
	1019: true, 1020: true, 1021: true, 1022: true, 1023: true, 1024: true,
	1027: true, 1028: true, 1040: true, 1041: true, 1034: true, 1115: true,
	1182: true, 1183: true, 1185: true, 1187: true, 1231: true, 1263: true,
	1270: true, 1561: true, 1563: true, 2951: true, 3807: true, 1004: true,
	9005: true,
}

var _ = binary.U16
