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
	"unicode/utf8"

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
	// openGauss 缺口类型（v0.2.6 补全，OID 与 PostgreSQL 同源）
	PointOID        = 600
	LsegOID         = 601
	PathOID         = 602
	BoxOID          = 603
	PolygonOID      = 604
	LineOID         = 628
	CircleOID       = 718
	Macaddr8OID     = 774
	XmlOID          = 142
	RegprocOID      = 24
	RegprocedureOID = 2202
	RegoperOID      = 2203
	RegoperatorOID  = 2204
	RegclassOID     = 2205
	RegtypeOID      = 2206
	RegconfigOID    = 3734
	RegdictionaryOID = 3769
	RegnamespaceOID = 4089
	RegroleOID      = 4096
	RegcollationOID = 4191
	PgLsnOID        = 3220
	TsvectorOID     = 3614
	TsqueryOID      = 3615
	Int4RangeOID    = 3904
	NumRangeOID     = 3906
	TsRangeOID      = 3908
	TstzRangeOID    = 3910
	DateRangeOID    = 3912
	Int8RangeOID    = 3926
	TxidSnapshotOID = 2970 // openGauss txid_snapshot（PG 为 5030，openGauss 为 2970）
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
	// openGauss timetz 磁盘 zone（秒）与 PG 符号相反（经 timetz_send 权威字节实测）：
	// '10:00:00-05' 磁盘存 +18000、'-05:30' 存 +19800、'+05' 存 -18000。
	// 显示：zone>0 → '-HH'，zone<0 → '+HH'；整点省略 ':00'（openGauss timetz_out 格式）。
	sign := "-"
	if zone < 0 {
		sign = "+"
		zone = -zone
	}
	h := zone / 3600
	m := (zone % 3600) / 60
	if m != 0 {
		return fmt.Sprintf("%s%s%02d:%02d", t, sign, h, m)
	}
	return fmt.Sprintf("%s%s%02d", t, sign, h)
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
	if int(nbits) > len(data)*8 || nbits < 0 {
		return "" // 损坏数据：位数超过实际字节，避免越界 panic
	}
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
	if v == math.MinInt64 {
		return "-92233720368547758.08" // MinInt64 取负溢出，特判（与 PG money_out 一致）
	}
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

// sqlNullMarker 数组 SQL NULL 元素哨兵（\x00 开头，SQL 文本不可能含 \x00，避免与字面量碰撞）
const sqlNullMarker = "\x00GN\x00"

func arrayQuote(s string) string {
	if s == sqlNullMarker {
		return "NULL" // SQL NULL 元素（无引号），区别于字符串 "NULL"（带引号）
	}
	// openGauss array_out 对字面量 "NULL" 加引号（{"NULL",...}）；裸 NULL 会被 array_in 解析为 SQL NULL 元素
	needs := s == "" || s == "NULL"
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
	if nelems <= 0 || nelems > 1<<24 {
		return "{}" // 维长乘积异常（损坏数据），避免位图/元素循环越界
	}
	var nulls []bool
	var elemOff int
	if dataoffset > 0 {
		bmLen := (nelems + 7) / 8
		if bodyOff+bmLen > len(p) {
			return "{}" // NULL 位图越界（损坏数据）
		}
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
			texts = append(texts, sqlNullMarker)
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

func safeElemText(payload []byte, pos int, elemOid int) (t string, np int, err error) {
	defer func() {
		if r := recover(); r != nil {
			t, np, err = "", 0, fmt.Errorf("array element corrupt at pos %d: %v", pos, r)
		}
	}()
	t, np = arrayElemText(payload, pos, elemOid)
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
	case RegprocOID, RegprocedureOID, RegoperOID, RegoperatorOID, RegclassOID,
		RegtypeOID, RegconfigOID, RegdictionaryOID, RegnamespaceOID, RegroleOID, RegcollationOID:
		s = decodeRegOid(raw)
	case PgLsnOID:
		s = decodePgLsn(raw)
	case TxidSnapshotOID:
		s = decodeTxidSnapshot(raw)
	case TsvectorOID:
		s = decodeTsvector(raw)
	case TsqueryOID:
		s = decodeTsquery(raw)
	case PointOID:
		s = decodePoint(raw)
	case LsegOID:
		s = decodeLseg(raw)
	case BoxOID:
		s = decodeBox(raw)
	case PathOID:
		s = decodePath(raw)
	case PolygonOID:
		s = decodePolygon(raw)
	case LineOID:
		s = decodeLine(raw)
	case CircleOID:
		s = decodeCircle(raw)
	case Macaddr8OID:
		s = decodeMacaddr8(raw)
	case XmlOID:
		s = decodeXml(raw)
	case Int4RangeOID, Int8RangeOID, NumRangeOID, DateRangeOID, TsRangeOID, TstzRangeOID:
		s = decodeRange(raw, oid)
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

// ---- openGauss 缺口类型：range / tsvector / tsquery / 几何 / pg_lsn / txid_snapshot / reg* / xml / macaddr8 ----
// 移植自 pg2sql-go types.go（Author: raysuen），磁盘格式与 PostgreSQL/openGauss 同源。

// ---- range 子类型 ----
type rangeSub struct {
	wid int                 // 定长子类型宽度（0=变长 varlena 内嵌）
	dec func([]byte) string // 子类型解码器（接收含 varlena 头的段）
}

var rangeSubtypes = map[int]rangeSub{
	Int4RangeOID: {4, decodeInt4},        // int4range
	Int8RangeOID: {8, decodeInt8},        // int8range
	NumRangeOID:  {0, decodeNumeric},     // numrange（numeric 为 varlena）
	DateRangeOID: {4, decodeDate},        // daterange
	TsRangeOID:   {8, decodeTimestamp},   // tsrange
	TstzRangeOID: {8, decodeTstz},        // tstzrange
}

// decodeRange：openGauss 范围类型 varlena（与 PG rangetypes.h 同源）。
// 磁盘：range 自身 oid(4B) + lower + upper + 1B flags（变长子类型带 varlena 长度前缀）。
// flags: 0x01 EMPTY 0x02 LB_INC 0x04 UB_INC 0x08 LB_INF 0x10 UB_INF 0x20 LB_NULL 0x40 UB_NULL。
// 输出与 range_out 一致：empty / [1,10) / (,10] / [1,) 等。
func decodeRange(b []byte, oid int) string {
	sub, ok := rangeSubtypes[oid]
	if !ok {
		return decodeText(b)
	}
	p, _, _ := varPayload(b)
	if len(p) < 5 {
		return decodeText(b)
	}
	p = p[4:] // 剥 range 自身 oid 头
	flags := p[len(p)-1]
	if flags&0x01 != 0 {
		return "empty"
	}
	var lo, hi []byte
	pos := 0
	if flags&0x08 != 0 {
		lo = nil
	} else if pos < len(p)-1 {
		if sub.wid > 0 {
			if pos+sub.wid > len(p)-1 {
				return decodeText(b) // 定长子类型越界（损坏数据）
			}
			lo = p[pos : pos+sub.wid]
			pos += sub.wid
		} else {
			kind, total, _, _ := binary.VarlenaParse(p, pos)
			if kind != "" && total > 0 && pos+total <= len(p)-1 {
				lo = p[pos : pos+total] // 含 varlena 头，decodeNumeric 内部剥
				pos += total
			}
		}
	}
	if flags&0x10 != 0 {
		hi = nil
	} else if pos < len(p)-1 {
		if sub.wid > 0 {
			if pos+sub.wid > len(p)-1 {
				return decodeText(b)
			}
			hi = p[pos : pos+sub.wid]
		} else {
			kind, total, _, _ := binary.VarlenaParse(p, pos)
			if kind != "" && total > 0 && pos+total <= len(p)-1 {
				hi = p[pos : pos+total]
			}
		}
	}
	var sb strings.Builder
	if flags&0x02 != 0 {
		sb.WriteByte('[')
	} else {
		sb.WriteByte('(')
	}
	if lo != nil {
		sb.WriteString(sub.dec(lo))
	}
	sb.WriteByte(',')
	if hi != nil {
		sb.WriteString(sub.dec(hi))
	}
	if flags&0x04 != 0 {
		sb.WriteByte(']')
	} else {
		sb.WriteByte(')')
	}
	return sb.String()
}

// decodeTsvector：tsvector varlena（与 PG tsvector.h 同源）。
// WordEntry 位打包：haspos(bit0)/len(11bit)/pos(20bit)；data 区连续词；WEP 权重 3→A/2→B/1→C/0 不显示。
func decodeTsvector(b []byte) string {
	p, _, _ := varPayload(b)
	b2 := p
	if len(b2) < 4 {
		return decodeText(b)
	}
	size := int32(binary.U32(b2, 0))
	if size < 0 || 4+int(size)*4 > len(b2) {
		return decodeText(b)
	}
	cur := 4 + int(size)*4
	var parts []string
	for i := 0; i < int(size); i++ {
		e := binary.U32(b2, 4+i*4)
		haspos := e & 1
		l := int((e >> 1) & 0x7FF)
		if cur+l > len(b2) {
			break
		}
		w := string(b2[cur : cur+l])
		if !utf8.ValidString(w) {
			w = "\\x" + hex.EncodeToString(b2[cur:cur+l]) // 非法 UTF-8 词：hex 标记，避免 � 静默失真
		}
		cur += l
		if haspos != 0 {
			if cur&1 != 0 {
				cur++
			}
			if cur+2 > len(b2) {
				break
			}
			np := int(binary.U16(b2, cur))
			cur += 2
			var poss []string
			for j := 0; j < np && cur+2 <= len(b2); j++ {
				p16 := binary.U16(b2, cur)
				cur += 2
				p := p16 & 0x3FFF
				wgt := p16 >> 14
				s := strconv.Itoa(int(p))
				switch wgt {
				case 1:
					s += "C"
				case 2:
					s += "B"
				case 3:
					s += "A"
				}
				poss = append(poss, s)
			}
			wEsc := strings.Replace(w, "'", "''", -1)
			parts = append(parts, "'"+wEsc+"':"+strings.Join(poss, ","))
		} else {
			wEsc := strings.Replace(w, "'", "''", -1)
			parts = append(parts, "'"+wEsc+"'")
		}
	}
	return strings.Join(parts, " ")
}

// decodeTsquery：tsquery varlena（与 PG tsquery.h 同源）。
// QueryItem 12B/个：QI_VAL(词) / QI_OPR(运算)；NOT/AND/OR/PHRASE 优先级与 PG infix 一致。
func decodeTsquery(b []byte) string {
	p, _, _ := varPayload(b)
	if len(p) < 8 {
		return decodeText(b)
	}
	size := int(int32(binary.U32(p, 0)))
	if size <= 0 {
		return ""
	}
	itemsStart := 4
	itemsEnd := itemsStart + 12*size
	if itemsEnd > len(p) {
		return decodeText(b)
	}
	operands := p[itemsEnd:]
	curPos := 0
	var sb strings.Builder
	var infix func(out *strings.Builder, parentPriority int, rightPhraseOp bool)
	infix = func(out *strings.Builder, parentPriority int, rightPhraseOp bool) {
		if curPos >= size {
			return
		}
		item := p[itemsStart+12*curPos : itemsStart+12*curPos+12]
		typ := item[0]
		switch typ {
		case 1: // QI_VAL
			weight := item[1]
			prefix := item[2]
			wordBits := binary.U32(item, 8)
			length := wordBits & 0xFFF
			distance := wordBits >> 12
			if int(distance)+int(length) > len(operands) {
				curPos++
				return
			}
			op := operands[distance : distance+length]
			out.WriteByte('\'')
			for _, ch := range op {
				if ch == '\'' {
					out.WriteByte(ch)
				}
				out.WriteByte(ch)
			}
			out.WriteByte('\'')
			if weight != 0 || prefix != 0 {
				out.WriteByte(':')
				if prefix != 0 {
					out.WriteByte('*')
				}
				if weight&8 != 0 {
					out.WriteByte('A')
				}
				if weight&4 != 0 {
					out.WriteByte('B')
				}
				if weight&2 != 0 {
					out.WriteByte('C')
				}
				if weight&1 != 0 {
					out.WriteByte('D')
				}
			}
			curPos++
		case 2: // QI_OPR
			oper := item[1]
			priority := 0
			switch oper {
			case 1: // OP_NOT
				priority = 4
			case 2: // OP_AND
				priority = 2
			case 3: // OP_OR
				priority = 1
			case 4: // OP_PHRASE
				priority = 3
			}
			distance := int(int16(binary.U16(item, 2)))
			needParen := priority < parentPriority || (oper == 4 && rightPhraseOp)
			if needParen {
				out.WriteString("( ")
			}
			curPos++
			if oper == 1 {
				out.WriteByte('!')
				infix(out, priority, false)
			} else {
				var rightBuf strings.Builder
				infix(&rightBuf, priority, oper == 4) // right
				infix(out, priority, false)           // left
				switch oper {
				case 2:
					out.WriteString(" & ")
				case 3:
					out.WriteString(" | ")
				case 4:
					if distance != 1 {
						out.WriteString(fmt.Sprintf(" <%d> ", distance))
					} else {
						out.WriteString(" <-> ")
					}
				}
				out.WriteString(rightBuf.String())
			}
			if needParen {
				out.WriteString(" )")
			}
		default:
			curPos++
		}
	}
	infix(&sb, -1, false)
	return sb.String()
}

// decodePgLsn：pg_lsn（oid 3220，8B 小端）。输出与 pg_lsn_out 一致：高 32 位/低 32 位大写 hex。
func decodePgLsn(b []byte) string {
	if len(b) < 8 {
		return decodeText(b)
	}
	v := binary.U64(b, 0)
	return fmt.Sprintf("%X/%X", uint32(v>>32), uint32(v))
}

// decodeTxidSnapshot：txid_snapshot（oid 2970，openGauss）varlena。
// 磁盘（openGauss，经 txid_snapshot_send 权威校准）：[nxip int32][xmin int64][xmax int64][extra int64恒0][xip int64...]，
// 总长 28+8n；与 PG 不同（PG 无 extra 8B）。输出固定 "xmin:xmax:"（尾冒号必须保留），xip 逗号分隔。
func decodeTxidSnapshot(b []byte) string {
	p, _, _ := varPayload(b)
	// openGauss txid_snapshot 布局（经 txid_snapshot_send 权威校准）：
	// [nxip int32][xmin int64][xmax int64][extra int64(恒0)][xip int64...]
	//（与 PG 不同：PG 为 [nxip][xmin8][xmax8][xip8] 且无 extra；openGauss xid 为 64 位）
	if len(p) < 28 {
		return decodeText(b)
	}
	nxip := binary.U32(p, 0)
	xmin := binary.U64(p, 4)
	xmax := binary.U64(p, 12)
	var sb strings.Builder
	sb.WriteString(strconv.FormatUint(xmin, 10))
	sb.WriteByte(':')
	sb.WriteString(strconv.FormatUint(xmax, 10))
	sb.WriteByte(':')
	for i := 0; i < int(nxip) && 28+8*i+8 <= len(p); i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(strconv.FormatUint(binary.U64(p, 28+8*i), 10))
	}
	return sb.String()
}

// decodeRegOid：reg* 系列（int4 byval，磁盘为 4B oid 小端）。输出 oid 数字（可逆导入）。
func decodeRegOid(b []byte) string {
	if len(b) < 4 {
		return decodeText(b)
	}
	return strconv.FormatUint(uint64(binary.U32(b, 0)), 10)
}

// decodeXml：xml varlena。PG 标准内容带 4B 类型标记，openGauss 同为 PG 布局。
func decodeXml(b []byte) string {
	p, _, _ := varPayload(b)
	if len(p) >= 5 && p[0] <= 1 && p[1] == 0 && p[2] == 0 && p[3] == 0 {
		p = p[4:]
	}
	if utf8.Valid(p) {
		return string(p)
	}
	return "\\x" + hex.EncodeToString(p)
}

// decodeMacaddr8：macaddr8（oid 774，8B）。输出 xx:xx:xx:xx:xx:xx:xx:xx。
func decodeMacaddr8(b []byte) string {
	if len(b) < 8 {
		return ""
	}
	parts := make([]string, 8)
	for i := 0; i < 8; i++ {
		parts[i] = fmt.Sprintf("%02x", b[i])
	}
	return strings.Join(parts, ":")
}

// ---- 几何类型（与 PG geo_decls.h 同源，全部小端 double）----
func geomPt(b []byte, off int) string {
	x := math.Float64frombits(binary.U64(b, off))
	y := math.Float64frombits(binary.U64(b, off+8))
	return fmt.Sprintf("(%s,%s)", fmtFloat(x), fmtFloat(y))
}

func decodePoint(b []byte) string {
	if len(b) < 16 {
		return ""
	}
	return geomPt(b, 0)
}

func decodeLseg(b []byte) string {
	if len(b) < 32 {
		return ""
	}
	return fmt.Sprintf("[%s,%s]", geomPt(b, 0), geomPt(b, 16))
}

func decodeBox(b []byte) string {
	if len(b) < 32 {
		return ""
	}
	return fmt.Sprintf("%s,%s", geomPt(b, 0), geomPt(b, 16))
}

func decodePath(b []byte) string {
	p, _, _ := varPayload(b)
	if len(p) < 12 {
		return ""
	}
	npts := int(int32(binary.U32(p, 0)))
	closed := int32(binary.U32(p, 4))
	if npts <= 0 || len(p) < 12+16*npts {
		return ""
	}
	pts := make([]string, npts)
	for i := 0; i < npts; i++ {
		pts[i] = geomPt(p, 12+16*i)
	}
	if closed != 0 {
		return "(" + strings.Join(pts, ",") + ")"
	}
	return "[" + strings.Join(pts, ",") + "]"
}

func decodePolygon(b []byte) string {
	p, _, _ := varPayload(b)
	if len(p) < 36 {
		return ""
	}
	npts := int(int32(binary.U32(p, 0)))
	if npts <= 0 || len(p) < 36+16*npts {
		return ""
	}
	pts := make([]string, npts)
	for i := 0; i < npts; i++ {
		pts[i] = geomPt(p, 36+16*i)
	}
	return "(" + strings.Join(pts, ",") + ")"
}

func decodeLine(b []byte) string {
	if len(b) < 24 {
		return ""
	}
	a := math.Float64frombits(binary.U64(b, 0))
	bb := math.Float64frombits(binary.U64(b, 8))
	c := math.Float64frombits(binary.U64(b, 16))
	return fmt.Sprintf("{%s,%s,%s}", fmtFloat(a), fmtFloat(bb), fmtFloat(c))
}

func decodeCircle(b []byte) string {
	if len(b) < 24 {
		return ""
	}
	x := math.Float64frombits(binary.U64(b, 0))
	y := math.Float64frombits(binary.U64(b, 8))
	r := math.Float64frombits(binary.U64(b, 16))
	return fmt.Sprintf("<(%s,%s),%s>", fmtFloat(x), fmtFloat(y), fmtFloat(r))
}
