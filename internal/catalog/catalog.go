// Package catalog 移植自 gauss2sql/catalog.py（Author: raysuen）
// 表结构元数据：离线解析 pg_class/pg_attribute/pg_type/pg_partition。
package catalog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gauss2sql-go/internal/binary"
	"gauss2sql-go/internal/heapfile"
	"gauss2sql-go/internal/meta"
	"gauss2sql-go/internal/page"
	"gauss2sql-go/internal/tuple"
	"gauss2sql-go/internal/types"
)

const (
	pgClassOID       = 1259
	pgAttrOID        = 1249
	pgTypeOID        = 1247
	pgNSOID          = 2615
	pgAttrdefOID     = 2604 // openGauss/PostgreSQL 实际 OID（2600 是 pg_aggregate）
	pgConstraintOID  = 2606
	pgDescriptionOID = 2609
	pgIndexOID       = 2610 // openGauss pg_index 目录 OID（与 PG 2654 不同，经 pg_class 实测）
	pgEnumRelfile    = 3501
	pgDBRelfile      = 1262
)

// pgEncodingToCodec openGauss 服务端编码枚举 → 解码 codec 名。
// 枚举值取自 openGauss 源码 src/include/mb/pg_wchar.h（5.0.0 起全版本一致，
// 与标准 PostgreSQL 不同：GBK=6、UTF8=7、LATIN1=9、GB18030=36）。
var pgEncodingToCodec = map[int]string{
	0:  "sql_ascii", // 无转换，字节直通
	1:  "euc_jp",
	2:  "euc_cn",
	3:  "euc_kr",
	4:  "euc_tw",
	5:  "euc_jis_2004",
	6:  "gbk",    // 简体中文（GBK）
	7:  "utf-8",  // UTF-8
	8:  "mule_internal",
	9:  "latin-1",
	10: "latin-2",
	11: "latin-3",
	12: "latin-4",
	13: "latin-5",
	14: "latin-6",
	15: "latin-7",
	16: "latin-8",
	17: "latin-9",
	18: "latin-10",
	19: "win1256",
	20: "win1258",
	21: "win866",
	22: "win874",
	23: "koi8r",
	24: "win1251",
	25: "win1252",
	26: "iso_8859_5",
	27: "iso_8859_6",
	28: "iso_8859_7",
	29: "iso_8859_8",
	30: "win1250",
	31: "win1253",
	32: "win1254",
	33: "win1255",
	34: "win1257",
	35: "koi8u",
	36: "gb18030", // 简体中文（GB18030）
}

// colItem 布局
type colItem = heapfile.ColLayout

// pg_class 用户列布局
var ogClassCols = []colItem{
	{64, false, "c"}, {4, false, "i"}, {4, false, "i"},
	{4, false, "i"}, {4, false, "i"}, {4, false, "i"},
	{4, false, "i"}, {4, false, "i"}, {8, false, "d"},
	{8, false, "d"}, {4, false, "i"}, {4, false, "i"},
	{4, false, "i"}, {4, false, "i"}, {4, false, "i"},
	{4, false, "i"}, {4, false, "i"}, {1, false, "c"},
	{1, false, "c"}, {1, false, "c"}, {1, false, "c"},
}

// pg_attribute 固定 20 列 + 4 varlena + 1 char
var ogAttrLayout = func() []colItem {
	fixed := []colItem{
		{4, false, "i"}, {64, false, "c"}, {4, false, "i"},
		{4, false, "i"}, {2, false, "s"}, {2, false, "s"},
		{4, false, "i"}, {4, false, "i"}, {4, false, "i"},
		{1, false, "c"}, {1, false, "c"}, {1, false, "c"},
		{1, false, "c"}, {1, false, "c"}, {1, false, "c"},
		{1, false, "c"}, {1, false, "c"}, {4, false, "i"},
		{4, false, "i"},
	}
	out := append([]colItem{}, fixed...)
	for i := 0; i < 4; i++ {
		out = append(out, colItem{-1, true, "i"})
	}
	out = append(out, colItem{1, false, "c"})
	return out
}()

var ogNSCols = []colItem{{64, false, "c"}, {4, false, "i"}}

var ogPartitionCols = []colItem{
	{64, false, "c"}, {1, false, "c"}, {4, false, "i"},
	{4, false, "i"}, {4, false, "i"}, {1, false, "c"}, {4, false, "i"},
}

// pg_attrdef (OID 2600): adrelid oid, adnum int2, adbin pg_node_tree, adsrc text
var ogAttrdefCols = []colItem{
	{4, false, "i"}, {2, false, "s"},
	{-1, true, "i"}, {-1, true, "i"},
}

// pg_constraint (OID 2606): conname name, connamespace oid, contype char,
//   condeferrable/condeferred/convalidated bool, conrelid oid, contypid oid,
//   conindid oid, confrelid oid, confupdtype/confdeltype/confmatchtype char,
//   conislocal bool, coninhcount int4, connoinherit/consoft/conopt bool,
//   conkey int2[], confkey int2[], conpfeqop/conppeqop/conffeqop/conexclop oid[],
//   conbin pg_node_tree, consrc text
var ogConstraintCols = []colItem{
	{64, false, "c"}, {4, false, "i"}, {1, false, "c"},
	{1, false, "c"}, {1, false, "c"}, {1, false, "c"},
	{4, false, "i"}, {4, false, "i"}, {4, false, "i"},
	{4, false, "i"},
	{1, false, "c"}, {1, false, "c"}, {1, false, "c"},
	{1, false, "c"}, {4, false, "i"}, {1, false, "c"},
	{1, false, "c"}, {1, false, "c"},
	{-1, true, "i"}, {-1, true, "i"}, {-1, true, "i"},
	{-1, true, "i"}, {-1, true, "i"}, {-1, true, "i"},
	{-1, true, "i"}, {-1, true, "i"},
}

// pg_description (OID 2609): objoid oid, classoid oid, objsubid int4, description text
var ogDescriptionCols = []colItem{
	{4, false, "i"}, {4, false, "i"}, {4, false, "i"}, {-1, true, "i"},
}

// ---- relmapper ----

func loadRelmapper(dir string) map[int]int {
	m := map[int]int{}
	data, err := os.ReadFile(filepath.Join(dir, "pg_filenode.map"))
	if err != nil {
		return m
	}
	for i := 0; i+8 <= len(data); i += 8 {
		oid := binary.U32(data, i)
		fn := binary.U32(data, i+4)
		if oid == 0 && fn == 0 {
			break
		}
		m[int(oid)] = int(fn)
	}
	return m
}

// rfnClassCache 系统目录 OID → 实时 relfilenode 的进程内缓存（dbDir+oid 键），
// 避免批量导出每表多次全扫 pg_class。仅 relmapper/std 未命中路径填充，正常库无额外开销。
var rfnClassCache = map[string]int{}

// lookupRfnByClass 通过 pg_class 回查系统目录对象的实时 relfilenode（磁盘文件存在才返回）。
// openGauss 运行中 relmapper（pg_filenode.map）刷盘可能滞后，以 pg_class.relfilenode 为准。
func lookupRfnByClass(dbDir string, standardOid int) int {
	if standardOid == pgClassOID {
		return 0 // pg_class 自身走 std/relmapper，避免递归
	}
	key := dbDir + ":" + strconv.Itoa(standardOid)
	if v, ok := rfnClassCache[key]; ok {
		return v
	}
	pc := SysFilePath(dbDir, pgClassOID)
	if pc == "" {
		return 0
	}
	found := 0
	iterateTuples(pc, 0, func(pno, idx int, t *tuple.HeapTuple) bool {
		row := classFields(t)
		if row == nil {
			return true
		}
		if row.oid == standardOid && row.relfilenode != 0 && row.relfilenode != standardOid {
			if _, err := os.Stat(filepath.Join(dbDir, strconv.Itoa(row.relfilenode))); err == nil {
				found = row.relfilenode
				return false
			}
		}
		return true
	})
	rfnClassCache[key] = found
	return found
}

// SysFilePath 定位系统目录物理文件
func SysFilePath(dbDir string, standardOid int) string {
	std := filepath.Join(dbDir, strconv.Itoa(standardOid))
	if _, err := os.Stat(std); err == nil {
		return std
	}
	fn := loadRelmapper(dbDir)[standardOid]
	// relmapper 命中后仍以 pg_class 实时 rfn 为准（运行中集群 VACUUM FULL 等场景 relmapper 滞后）
	if fresh := lookupRfnByClass(dbDir, standardOid); fresh != 0 {
		if _, err := os.Stat(filepath.Join(dbDir, strconv.Itoa(fresh))); err == nil {
			return filepath.Join(dbDir, strconv.Itoa(fresh))
		}
	}
	if fn != 0 {
		p := filepath.Join(dbDir, strconv.Itoa(fn))
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// iterateTuples 遍历堆文件 live 元组
func iterateTuples(path string, pageSize int, cb func(pageno, idx int, t *tuple.HeapTuple) bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	ps := pageSize
	if ps == 0 {
		if len(data) >= 130 {
			ps = page.DetectPageSize(data[:130])
		}
		if ps == 0 {
			ps = page.PageSize
		}
	}
	npages := len(data) / ps
	for pno := 0; pno < npages; pno++ {
		raw := data[pno*ps : (pno+1)*ps]
		pg := page.NewPage(pno, raw, ps)
		if !pg.HasValidLayout {
			continue
		}
		for _, it := range pg.Items {
			if it.Flags != page.ItemIDNormal {
				continue
			}
			t, err := tuple.New(raw[it.Off : it.Off+it.Len])
			if err != nil {
				continue
			}
			if !t.IsLive() {
				continue
			}
			if !cb(pno, it.Index, t) {
				return
			}
		}
	}
}

func cstring(b []byte) string {
	end := 0
	for end < len(b) && b[end] != 0 {
		end++
	}
	return binary.DecodeBytes(b[:end])
}

// classRow pg_class 一行
type classRow struct {
	oid         int
	relname     string
	relns       int
	relfilenode int
	relkind     string
	toastrelid  int
}

// classFields 解析 pg_class 行；失败返回 nil
func classFields(t *tuple.HeapTuple) *classRow {
	fields := heapfile.ExtractFieldsDirect(t.Raw, int(t.HOff), t.GetNulls(), ogClassCols)
	if len(fields) < 7 {
		return nil
	}
	relnameF := fields[0]
	if relnameF == nil {
		return nil
	}
	oid := t.GetOid()
	relname := cstring(relnameF)
	var relns, rfn uint32
	if fields[1] != nil && len(fields[1]) >= 4 {
		relns = binary.U32(fields[1], 0)
	}
	if fields[6] != nil && len(fields[6]) >= 4 {
		rfn = binary.U32(fields[6], 0)
	}
	relfilenode := int(rfn)
	if relfilenode == 0 {
		relfilenode = oid
	}
	relkind := "r"
	if len(fields) > 20 && fields[20] != nil && len(fields[20]) >= 1 {
		relkind = string(rune(fields[20][0]))
	}
	toastrelid := 0
	if len(fields) > 11 && fields[11] != nil && len(fields[11]) >= 4 {
		toastrelid = int(binary.U32(fields[11], 0))
	}
	return &classRow{oid, relname, int(relns), relfilenode, relkind, toastrelid}
}

// AttrRow pg_attribute 行
type AttrRow struct {
	Attrelid int
	Attname  string
	Atttypid int
	Attlen   int
	Attnum   int
	Typmod   int
	Notnull  bool
	Dropped  bool
	Attalign string
	Attbyval bool
	Attstorage string
}

func attrFields(t *tuple.HeapTuple) *AttrRow {
	defer func() { recover() }()
	fields := heapfile.ExtractFieldsDirect(t.Raw, int(t.HOff), t.GetNulls(), ogAttrLayout)
	if len(fields) < 9 {
		return nil
	}
	aalign := fields[11]
	astorage := fields[10]
	if aalign == nil || !strings.ContainsRune("csi d", rune(aalign[0])) {
		return nil
	}
	if aalign == nil || (aalign[0] != 'c' && aalign[0] != 's' && aalign[0] != 'i' && aalign[0] != 'd') {
		return nil
	}
	if astorage == nil || (astorage[0] != 'p' && astorage[0] != 'e' && astorage[0] != 'm' && astorage[0] != 'x') {
		return nil
	}
	u32f := func(i int) int {
		if fields[i] != nil && len(fields[i]) >= 4 {
			return int(binary.U32(fields[i], 0))
		}
		return 0
	}
	i16f := func(i int) int {
		if fields[i] != nil && len(fields[i]) >= 2 {
			return int(int16(binary.U16(fields[i], 0)))
		}
		return 0
	}
	i32f := func(i int) int {
		if fields[i] != nil && len(fields[i]) >= 4 {
			return int(binary.I32(fields[i], 0))
		}
		return -1
	}
	boolf := func(i int) bool {
		if fields[i] == nil || len(fields[i]) == 0 {
			return false
		}
		v := fields[i][0]
		return v == 1 || v == 't'
	}
	name := ""
	if fields[1] != nil {
		end := 0
		for end < len(fields[1]) && fields[1][end] != 0 {
			end++
		}
		name = binary.DecodeBytes(fields[1][:end])
	}
	return &AttrRow{
		Attrelid: u32f(0), Attname: name, Atttypid: u32f(2),
		Attlen: i16f(4), Attnum: i16f(5), Typmod: i32f(8),
		Notnull: boolf(12), Dropped: boolf(14),
		Attalign: string(rune(aalign[0])), Attbyval: boolf(9),
		Attstorage: string(rune(astorage[0])),
	}
}

func nsFields(t *tuple.HeapTuple) (int, string) {
	defer func() { recover() }()
	fields := heapfile.ExtractFieldsDirect(t.Raw, int(t.HOff), t.GetNulls(), ogNSCols)
	if len(fields) == 0 || fields[0] == nil {
		return 0, ""
	}
	return t.GetOid(), cstring(fields[0])
}

func partitionFields(t *tuple.HeapTuple) (relname, parttype string, parentid, rfn int) {
	defer func() { recover() }()
	fields := heapfile.ExtractFieldsDirect(t.Raw, int(t.HOff), t.GetNulls(), ogPartitionCols)
	if len(fields) < 7 || fields[0] == nil {
		return "", "", 0, 0
	}
	relname = cstring(fields[0])
	parttype = ""
	if fields[1] != nil && len(fields[1]) >= 1 {
		parttype = string(rune(fields[1][0]))
	}
	parentid = 0
	if fields[2] != nil && len(fields[2]) >= 4 {
		parentid = int(binary.U32(fields[2], 0))
	}
	rfn = 0
	if fields[6] != nil && len(fields[6]) >= 4 {
		rfn = int(binary.U32(fields[6], 0))
	}
	return
}

// ---- 默认值 / 约束 / 注释 ----

// ConstraintInfo 表级约束（主键/唯一/CHECK）
type ConstraintInfo struct {
	Name     string
	Type     byte  // 'p' 主键, 'u' 唯一, 'c' CHECK
	Cols     []int // conkey（列号，主键/唯一）
	Src      string // consrc（CHECK 表达式文本）
	Conindid int   // conindid：约束 backing 索引 OID（p/u 约束内联建表时隐式创建的同名索引）
}

func attrdefFields(t *tuple.HeapTuple) (relid, attnum int, adsrc string) {
	defer func() { recover() }()
	fields := heapfile.ExtractFieldsDirect(t.Raw, int(t.HOff), t.GetNulls(), ogAttrdefCols)
	if len(fields) < 4 || fields[0] == nil {
		return 0, 0, ""
	}
	relid = int(binary.U32(fields[0], 0))
	if fields[1] != nil && len(fields[1]) >= 2 {
		attnum = int(int16(binary.U16(fields[1], 0)))
	}
	if fields[3] != nil {
		adsrc = varlenaText(fields[3])
	}
	return
}

func constraintFields(t *tuple.HeapTuple) (name string, ctype byte, relid int, conindid int, key []int, src string) {
	defer func() { recover() }()
	fields := heapfile.ExtractFieldsDirect(t.Raw, int(t.HOff), t.GetNulls(), ogConstraintCols)
	if len(fields) < 26 || fields[0] == nil {
		return "", 0, 0, 0, nil, ""
	}
	end := 0
	for end < len(fields[0]) && fields[0][end] != 0 {
		end++
	}
	name = binary.DecodeBytes(fields[0][:end])
	if fields[2] != nil && len(fields[2]) >= 1 {
		ctype = fields[2][0]
	}
	if fields[6] != nil && len(fields[6]) >= 4 {
		relid = int(binary.U32(fields[6], 0))
	}
	// conindid（布局字段 8）：p/u 约束 backing 索引 OID，CHECK('c') 恒为 0
	if fields[8] != nil && len(fields[8]) >= 4 {
		conindid = int(binary.U32(fields[8], 0))
	}
	if fields[18] != nil {
		key = decodeInt2Array(fields[18])
	}
	if fields[25] != nil {
		src = varlenaText(fields[25])
	}
	return
}

// decodeInt2Array 解析 int2[] 数组（conkey/confkey 等），返回元素值列表
func decodeInt2Array(raw []byte) []int {
	if len(raw) < 16 {
		return nil
	}
	// 剥掉 varlena 头（兼容 1B 短头 / 4B 头）
	kind, _, poff, plen := binary.VarlenaParse(raw, 0)
	if kind == "" || poff+plen > len(raw) {
		return nil
	}
	payload := raw[poff : poff+plen]
	if len(payload) < 12 {
		return nil
	}
	dims := int(binary.U32(payload, 0))
	if dims != 1 {
		return nil
	}
	if elemtype := binary.U32(payload, 8); elemtype != 21 { // int2
		return nil
	}
	// 数组头：ndim(4)+dataoffset(4)+elemtype(4) + 每维 nelems(4)+lowerbound(4)
	nelems := int(binary.U32(payload, 12))
	dataOff := int(binary.U32(payload, 4))
	body := payload[20:] // 12 基础头 + 8 维头
	if dataOff > 0 && dataOff <= len(body) {
		body = body[dataOff:] // NULL 位图
	}
	if nelems < 0 || nelems*2 > len(body) {
		return nil
	}
	out := make([]int, 0, nelems)
	for i := 0; i < nelems; i++ {
		out = append(out, int(int16(binary.U16(body, i*2))))
	}
	return out
}

// varlenaText 从 ExtractFieldsDirect 返回的整段 varlena（含头）中取出文本负载。
func varlenaText(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	kind, _, poff, plen := binary.VarlenaParse(b, 0)
	if kind == "" || poff+plen > len(b) {
		return string(b)
	}
	return string(b[poff : poff+plen])
}

// LoadAttrDefaults 读 pg_attrdef：reloid → {attnum: adsrc}
func LoadAttrDefaults(dbDir string, relOid, pageSize int) map[int]string {
	out := map[int]string{}
	path := SysFilePath(dbDir, pgAttrdefOID)
	if path == "" {
		return out
	}
	iterateTuples(path, pageSize, func(pno, idx int, t *tuple.HeapTuple) bool {
		relid, attnum, adsrc := attrdefFields(t)
		if relid == relOid && attnum > 0 && adsrc != "" {
			out[attnum] = adsrc
		}
		return true
	})
	return out
}

// LoadConstraints 读 pg_constraint：reloid → 约束列表（contype p/u/c，跳过外键/检查未验证）
func LoadConstraints(dbDir string, relOid, pageSize int) []ConstraintInfo {
	var out []ConstraintInfo
	path := SysFilePath(dbDir, pgConstraintOID)
	if path == "" {
		return out
	}
	iterateTuples(path, pageSize, func(pno, idx int, t *tuple.HeapTuple) bool {
		name, ctype, relid, conindid, key, src := constraintFields(t)
		if relid != relOid {
			return true
		}
		switch ctype {
		case 'p', 'u':
			if len(key) > 0 {
				out = append(out, ConstraintInfo{Name: name, Type: ctype, Cols: key, Conindid: conindid})
			}
		case 'c':
			if src != "" {
				out = append(out, ConstraintInfo{Name: name, Type: ctype, Src: src})
			}
		}
		return true
	})
	return out
}

// LoadDescriptions 读 pg_description：reloid → {attnum: 描述}，attnum=0 表示表注释
func LoadDescriptions(dbDir string, relOid, pageSize int) map[int]string {
	out := map[int]string{}
	path := SysFilePath(dbDir, pgDescriptionOID)
	if path == "" {
		return out
	}
	iterateTuples(path, pageSize, func(pno, idx int, t *tuple.HeapTuple) bool {
		defer func() { recover() }()
		fields := heapfile.ExtractFieldsDirect(t.Raw, int(t.HOff), t.GetNulls(), ogDescriptionCols)
		if len(fields) < 4 || fields[0] == nil || fields[2] == nil || fields[3] == nil {
			return true
		}
		if int(binary.U32(fields[0], 0)) == relOid {
			out[int(binary.U32(fields[2], 0))] = varlenaText(fields[3])
		}
		return true
	})
	return out
}

// ---- 加载枚举映射 ----

// LoadEnumMap 读 pg_enum
func LoadEnumMap(dbDir string) {
	path := SysFilePath(dbDir, pgEnumRelfile)
	if path == "" {
		types.SetEnumMap(map[int]map[int]string{})
		return
	}
	layout := []colItem{{4, false, "i"}, {4, false, "i"}, {64, false, "c"}}
	iterateTuples(path, 0, func(pno, idx int, t *tuple.HeapTuple) bool {
		fields := heapfile.ExtractFieldsDirect(t.Raw, int(t.HOff), t.GetNulls(), layout)
		if len(fields) < 3 || fields[0] == nil || fields[2] == nil {
			return true
		}
		enumTypid := int(binary.U32(fields[0], 0))
		label := cstring(fields[2])
		if label == "" {
			return true
		}
		types.AddEnumMember(enumTypid, t.GetOid(), label)
		return true
	})
}

// BuildTypeNameMap pg_type oid→typname
func BuildTypeNameMap(dbDir string) map[int]string {
	tm := map[int]string{}
	path := SysFilePath(dbDir, pgTypeOID)
	if path == "" {
		return tm
	}
	layout := []colItem{{64, false, "c"}}
	iterateTuples(path, 0, func(pno, idx int, t *tuple.HeapTuple) bool {
		fields := heapfile.ExtractFieldsDirect(t.Raw, int(t.HOff), t.GetNulls(), layout)
		if len(fields) == 0 || fields[0] == nil {
			return true
		}
		oid := t.GetOid()
		name := cstring(fields[0])
		if oid != 0 && name != "" {
			tm[oid] = name
		}
		return true
	})
	return tm
}

// DetectDatabaseEncoding 探测库编码
func DetectDatabaseEncoding(datadir string, dbOid int) string {
	if datadir == "" || dbOid == 0 {
		return ""
	}
	f := filepath.Join(datadir, "global", strconv.Itoa(pgDBRelfile))
	if _, err := os.Stat(f); err != nil {
		f = SysFilePath(filepath.Join(datadir, "global"), pgDBRelfile)
	}
	if f == "" {
		return ""
	}
	layout := []colItem{{64, false, "c"}, {4, false, "i"}, {4, false, "i"}}
	enc := 0
	iterateTuples(f, 0, func(pno, idx int, t *tuple.HeapTuple) bool {
		if t.GetOid() != dbOid {
			return true
		}
		fields := heapfile.ExtractFieldsDirect(t.Raw, int(t.HOff), t.GetNulls(), layout)
		if len(fields) < 3 || fields[2] == nil || len(fields[2]) < 4 {
			return false
		}
		enc = int(binary.I32(fields[2], 0))
		return false
	})
	if c, ok := pgEncodingToCodec[enc]; ok {
		return c
	}
	return ""
}

// ---- 列构建 ----

func loadAttrs(dbDir string, pgAttrPath string) map[int][]*meta.Column {
	out := map[int][]*meta.Column{}
	iterateTuples(pgAttrPath, 0, func(pno, idx int, t *tuple.HeapTuple) bool {
		ar := attrFields(t)
		if ar == nil || ar.Attnum <= 0 {
			return true
		}
		out[ar.Attrelid] = append(out[ar.Attrelid], &meta.Column{
			Name: ar.Attname, Atttypid: ar.Atttypid, Attlen: ar.Attlen,
			Attnum: ar.Attnum, Typmod: ar.Typmod, Notnull: ar.Notnull,
			Attdropped: ar.Dropped, Attalign: ar.Attalign, Attbyval: ar.Attbyval,
			Attstorage: ar.Attstorage,
		})
		return true
	})
	for k := range out {
		sort.Slice(out[k], func(i, j int) bool { return out[k][i].Attnum < out[k][j].Attnum })
	}
	return out
}

func loadNSMap(dbDir string) map[int]string {
	nsPath := SysFilePath(dbDir, pgNSOID)
	out := map[int]string{}
	if nsPath == "" {
		return out
	}
	iterateTuples(nsPath, 0, func(pno, idx int, t *tuple.HeapTuple) bool {
		oid, name := nsFields(t)
		if name != "" {
			out[oid] = name
		}
		return true
	})
	return out
}

// AutoDiscoverMeta 从单个数据文件发现表结构
func AutoDiscoverMeta(dataFile string, pageSize int) *meta.TableMeta {
	abs, _ := filepath.Abs(dataFile)
	filename := filepath.Base(abs)
	dbDir := filepath.Dir(abs)
	targetRF, _ := strconv.Atoi(filename)

	pgClassPath := SysFilePath(dbDir, pgClassOID)
	if pgClassPath == "" {
		return nil
	}
	nsMap := loadNSMap(dbDir)

	var targetOid int
	var targetRelname string
	var targetRelkind string
	var targetNSInt int
	var targetToastrelid int
	iterateTuples(pgClassPath, pageSize, func(pno, idx int, t *tuple.HeapTuple) bool {
		row := classFields(t)
		if row == nil {
			return true
		}
		if row.relfilenode == targetRF {
			targetOid = row.oid
			targetRelname = row.relname
			targetNSInt = row.relns
			targetRelkind = row.relkind
			targetToastrelid = row.toastrelid
			return false
		}
		return true
	})

	if targetOid == 0 {
		// 分区子分区回查
		if tm := tryPartitionFallback(dbDir, targetRF, pageSize); tm != nil {
			return tm
		}
		return nil
	}

	schema := nsMap[targetNSInt]
	if schema == "" {
		schema = "public"
	}
	pgAttrPath := SysFilePath(dbDir, pgAttrOID)
	if pgAttrPath == "" {
		return nil
	}
	var cols []*meta.Column
	iterateTuples(pgAttrPath, pageSize, func(pno, idx int, t *tuple.HeapTuple) bool {
		ar := attrFields(t)
		if ar == nil || ar.Attrelid != targetOid || ar.Attnum <= 0 {
			return true
		}
		cols = append(cols, &meta.Column{
			Name: ar.Attname, Atttypid: ar.Atttypid, Attlen: ar.Attlen,
			Attnum: ar.Attnum, Typmod: ar.Typmod, Notnull: ar.Notnull,
			Attdropped: ar.Dropped, Attalign: ar.Attalign, Attbyval: ar.Attbyval,
			Attstorage: ar.Attstorage,
		})
		return true
	})
	if len(cols) == 0 {
		return nil
	}
	sort.Slice(cols, func(i, j int) bool { return cols[i].Attnum < cols[j].Attnum })
	tm := &meta.TableMeta{
		DBName: filepath.Base(dbDir), Schema: schema, Relname: targetRelname,
		Relfilenode: targetRF, Reloid: targetOid, Columns: cols, Relkind: targetRelkind,
		Toastrelid: targetToastrelid,
		TypeNames:  BuildTypeNameMap(dbDir),
	}
	tm.PrimaryKey = primaryKeyCols(LoadConstraints(dbDir, targetOid, pageSize), cols)
	return tm
}

func tryPartitionFallback(dbDir string, targetRF, pageSize int) *meta.TableMeta {
	pgClassPath := SysFilePath(dbDir, pgClassOID)
	if pgClassPath == "" {
		return nil
	}
	classByOid := map[int]*classRow{}
	var partitionRF int
	iterateTuples(pgClassPath, pageSize, func(pno, idx int, t *tuple.HeapTuple) bool {
		row := classFields(t)
		if row == nil {
			return true
		}
		classByOid[row.oid] = row
		if row.relname == "pg_partition" {
			partitionRF = row.relfilenode
		}
		return true
	})
	if partitionRF == 0 {
		return nil
	}
	partPath := filepath.Join(dbDir, strconv.Itoa(partitionRF))
	if _, err := os.Stat(partPath); err != nil {
		return nil
	}
	var parentOid int
	iterateTuples(partPath, pageSize, func(pno, idx int, t *tuple.HeapTuple) bool {
		_, parttype, parentid, rfn := partitionFields(t)
		if rfn == targetRF && parttype == "p" {
			parentOid = parentid
			return false
		}
		return true
	})
	if parentOid == 0 {
		return nil
	}
	pinfo, ok := classByOid[parentOid]
	if !ok {
		return nil
	}
	parentRelname := pinfo.relname
	parentNS := pinfo.relns
	parentRelkind := pinfo.relkind
	nsMap := loadNSMap(dbDir)
	schema := nsMap[parentNS]
	if schema == "" {
		schema = "public"
	}
	pgAttrPath := SysFilePath(dbDir, pgAttrOID)
	if pgAttrPath == "" {
		return nil
	}
	var cols []*meta.Column
	iterateTuples(pgAttrPath, pageSize, func(pno, idx int, t *tuple.HeapTuple) bool {
		ar := attrFields(t)
		if ar == nil || ar.Attrelid != parentOid || ar.Attnum <= 0 {
			return true
		}
		cols = append(cols, &meta.Column{
			Name: ar.Attname, Atttypid: ar.Atttypid, Attlen: ar.Attlen,
			Attnum: ar.Attnum, Typmod: ar.Typmod, Notnull: ar.Notnull,
			Attdropped: ar.Dropped, Attalign: ar.Attalign, Attbyval: ar.Attbyval,
			Attstorage: ar.Attstorage,
		})
		return true
	})
	if len(cols) == 0 {
		return nil
	}
	sort.Slice(cols, func(i, j int) bool { return cols[i].Attnum < cols[j].Attnum })
	return &meta.TableMeta{
		DBName: filepath.Base(dbDir), Schema: schema, Relname: parentRelname,
		Relfilenode: targetRF, Reloid: parentOid, Columns: cols, Relkind: parentRelkind,
		TypeNames: BuildTypeNameMap(dbDir),
	}
}

// primaryKeyCols 从约束中提取主键列名（contype='p'）
func primaryKeyCols(cs []ConstraintInfo, cols []*meta.Column) []string {
	nameByNum := map[int]string{}
	for _, c := range cols {
		if !c.Attdropped {
			nameByNum[c.Attnum] = c.Name
		}
	}
	for _, ci := range cs {
		if ci.Type != 'p' {
			continue
		}
		var out []string
		for _, n := range ci.Cols {
			if name, ok := nameByNum[n]; ok {
				out = append(out, name)
			}
		}
		return out
	}
	return nil
}

// FindPhysicalFileByOid 按表 OID 查 pg_class 取物理 relfilenode 文件名
func FindPhysicalFileByOid(dbDir string, tableOid int) string {
	pgClassPath := SysFilePath(dbDir, pgClassOID)
	if pgClassPath == "" {
		return ""
	}
	found := 0
	iterateTuples(pgClassPath, 0, func(pno, idx int, t *tuple.HeapTuple) bool {
		row := classFields(t)
		if row != nil && row.oid == tableOid {
			found = row.relfilenode
			return false
		}
		return true
	})
	if found == 0 {
		return ""
	}
	p := filepath.Join(dbDir, strconv.Itoa(found))
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return ""
}

// SysFilePathPublic 公开系统目录文件定位
func SysFilePathPublic(dbDir string, standardOid int) string {
	return SysFilePath(dbDir, standardOid)
}

// IterateClass 遍历 pg_class 回调 (oid, relname, relkind, relfilenode)
func IterateClass(dbDir string, pageSize int, cb func(oid int, relname, relkind string, rfn int)) {
	pgClassPath := SysFilePath(dbDir, pgClassOID)
	if pgClassPath == "" {
		return
	}
	iterateTuples(pgClassPath, pageSize, func(pno, idx int, t *tuple.HeapTuple) bool {
		if row := classFields(t); row != nil {
			cb(row.oid, row.relname, row.relkind, row.relfilenode)
		}
		return true
	})
}

// 注：pg_partition 定位不依赖固定 OID（openGauss 中其 OID 非 8190 且不在 relmapper），
// 由 partitionFilePath 从 pg_class 按 relname 实时取 relfilenode。

// FindTableMetaByName 按表名（"schema.table" 或裸表名）从数据库目录定位表元数据（v0.2.12）。
// 返回 (表元数据, 物理文件路径)；分区父表（relfilenode==0）时物理文件路径为空串，
// 调用方须用 ListPartitionFiles 展开子分区文件。
// 裸表名在多 schema 下同名时报错，提示用 schema.table 指定。
func FindTableMetaByName(dbDir, name string) (*meta.TableMeta, string, error) {
	abs, _ := filepath.Abs(dbDir)
	nsMap := loadNSMap(abs)
	pgAttrPath := SysFilePath(abs, pgAttrOID)
	var attByRel map[int][]*meta.Column
	if pgAttrPath != "" {
		attByRel = loadAttrs(abs, pgAttrPath)
	}
	var schema, relname string
	if i := strings.Index(name, "."); i >= 0 {
		schema, relname = name[:i], name[i+1:]
	} else {
		relname = name
	}
	var matches []*classRow
	pgClassPath := SysFilePath(abs, pgClassOID)
	if pgClassPath != "" {
		iterateTuples(pgClassPath, 0, func(pno, idx int, t *tuple.HeapTuple) bool {
			if row := classFields(t); row != nil && row.relname == relname {
				if schema == "" || nsMap[row.relns] == schema {
					matches = append(matches, row)
				}
			}
			return true
		})
	}
	if len(matches) == 0 {
		return nil, "", fmt.Errorf("未找到表 %q（检查 schema.table 或数据库目录是否正确）", name)
	}
	if schema == "" && len(matches) > 1 {
		var names []string
		for _, m := range matches {
			ns := nsMap[m.relns]
			if ns == "" {
				ns = "public"
			}
			names = append(names, ns+"."+m.relname)
		}
		return nil, "", fmt.Errorf("表 %q 在多个 schema 下存在: %s，请用 schema.table 指定", name, strings.Join(names, ", "))
	}
	row := matches[0]
	ns := nsMap[row.relns]
	if ns == "" {
		ns = "public"
	}
	cols := attByRel[row.oid]
	if len(cols) == 0 {
		return nil, "", fmt.Errorf("表 %s.%s 无列定义（pg_attribute 解析失败）", ns, row.relname)
	}
	tm := &meta.TableMeta{
		DBName:      filepath.Base(abs),
		Schema:      ns,
		Relname:     row.relname,
		Relfilenode: row.relfilenode,
		Reloid:      row.oid,
		Columns:     cols,
		Relkind:     row.relkind,
		Toastrelid:  row.toastrelid,
		TypeNames:   BuildTypeNameMap(abs),
	}
	filePath := ""
	if row.relfilenode != 0 {
		filePath = FindPhysicalFileByOid(abs, row.oid)
		if filePath == "" {
			filePath = filepath.Join(abs, strconv.Itoa(row.relfilenode))
		}
		if _, err := os.Stat(filePath); err != nil {
			filePath = "" // 物理文件不存在：分区父表（relfilenode 兜底为 oid 但无独立文件）
		}
	}
	if filePath == "" {
		// 分区父表判定：pg_partition 中存在 parentid=本表 oid 的子分区
		if parts := ListPartitionFiles(abs, row.oid); len(parts) > 0 {
			tm.Relfilenode = 0 // 父表无独立物理文件，由调用方展开子分区
		}
	}
	return tm, filePath, nil
}

// ListPartitionFiles 列出分区父表（parentOid）的全部子分区物理数据文件（v0.2.12）。
// openGauss 分区父表 relfilenode=0，子分区元数据在 pg_partition（parentid→父表 OID）。
func ListPartitionFiles(dbDir string, parentOid int) []string {
	abs, _ := filepath.Abs(dbDir)
	partPath := partitionFilePath(abs)
	var files []string
	if partPath == "" {
		return files
	}
	iterateTuples(partPath, 0, func(pno, idx int, t *tuple.HeapTuple) bool {
		relname, _, parentid, rfn := partitionFields(t)
		if relname == "" || parentid != parentOid || rfn == 0 {
			return true
		}
		p := FindPhysicalFileByOid(abs, rfn)
		if p == "" {
			p = filepath.Join(abs, strconv.Itoa(rfn))
		}
		if _, err := os.Stat(p); err == nil {
			files = append(files, p)
		}
		return true
	})
	return files
}

// partitionFilePath 定位 pg_partition 物理文件。
// 不依赖固定 OID/relmapper（openGauss pg_partition OID 非 8190 且不在 relmapper），
// 从 pg_class 取 relname='pg_partition' 的实时 relfilenode（与 tryPartitionFallback 同法）。
func partitionFilePath(dbDir string) string {
	pgClassPath := SysFilePath(dbDir, pgClassOID)
	if pgClassPath == "" {
		return ""
	}
	rf := 0
	iterateTuples(pgClassPath, 0, func(pno, idx int, t *tuple.HeapTuple) bool {
		row := classFields(t)
		if row != nil && row.relname == "pg_partition" && row.relfilenode != 0 {
			rf = row.relfilenode
			return false
		}
		return true
	})
	if rf == 0 {
		return ""
	}
	p := filepath.Join(dbDir, strconv.Itoa(rf))
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}

// LoadDBNames 解析 <pgdata>/global/pg_database (OID 1262) → {oid: datname}
func LoadDBNames(pgdata string) map[int]string {
	out := map[int]string{}
	globalDir := filepath.Join(pgdata, "global")
	path := SysFilePath(globalDir, pgDBRelfile)
	if path == "" {
		return out
	}
	layout := []colItem{{64, false, "c"}}
	iterateTuples(path, 0, func(pno, idx int, t *tuple.HeapTuple) bool {
		fields := heapfile.ExtractFieldsDirect(t.Raw, int(t.HOff), t.GetNulls(), layout)
		if len(fields) == 0 || fields[0] == nil || len(fields[0]) == 0 {
			return true
		}
		name := cstring(fields[0])
		if name == "" {
			return true
		}
		oid := t.GetOid()
		if oid > 0 {
			out[oid] = name
		}
		return true
	})
	return out
}

// AutoDiscoverAllTables 发现库内所有表
func AutoDiscoverAllTables(dbDir string, pageSize int) map[string]*meta.TableMeta {
	abs, _ := filepath.Abs(dbDir)
	pgClassPath := SysFilePath(abs, pgClassOID)
	nsMap := loadNSMap(abs)
	var classEntries []*classRow
	if pgClassPath != "" {
		iterateTuples(pgClassPath, pageSize, func(pno, idx int, t *tuple.HeapTuple) bool {
			if row := classFields(t); row != nil {
				classEntries = append(classEntries, row)
			}
			return true
		})
	}
	pgAttrPath := SysFilePath(abs, pgAttrOID)
	attByRel := map[int][]*meta.Column{}
	if pgAttrPath != "" {
		attByRel = loadAttrs(abs, pgAttrPath)
	}
	out := map[string]*meta.TableMeta{}
	for _, row := range classEntries {
		oid := row.oid
		relname := row.relname
		ns := row.relns
		rfn := row.relfilenode
		relkind := row.relkind
		cols := attByRel[oid]
		if len(cols) == 0 {
			continue
		}
		schema := nsMap[ns]
		if schema == "" {
			schema = "public"
		}
		tm := &meta.TableMeta{
			DBName: filepath.Base(abs), Schema: schema, Relname: relname,
			Relfilenode: rfn, Columns: cols, Relkind: relkind,
			TypeNames: BuildTypeNameMap(abs),
		}
		out[tm.FullName()] = tm
		key := strconv.Itoa(rfn)
		if _, exists := out[key]; !exists && rfn != 0 {
			out[key] = tm
		}
	}
	return out
}

// ExportMetaTable 导出 JSON 用表
type ExportMetaTable struct {
	Schema      string            `json:"schema"`
	Table       string            `json:"table"`
	Oid         int               `json:"oid"`
	Relkind     string            `json:"relkind"`
	Relfilenode int               `json:"relfilenode"`
	Toastrelid  int               `json:"toastrelid"`
	PrimaryKey  []string          `json:"primary_key"`
	TypeNames   map[int]string    `json:"type_names"`
	Columns     []ExportCol       `json:"columns"`
}

// ExportCol 导出 JSON 列
type ExportCol struct {
	Name      string `json:"name"`
	TypeOid   int    `json:"type_oid"`
	Len       int    `json:"len"`
	Attnum    int    `json:"attnum"`
	Typmod    int    `json:"typmod"`
	Notnull   bool   `json:"notnull"`
	Dropped   bool   `json:"dropped"`
	Attalign  string `json:"attalign"`
	Attbyval  bool   `json:"attbyval"`
	Attstorage string `json:"attstorage"`
}

// ExportMetaJSON export-meta 输出
type ExportMetaJSON struct {
	Database   string             `json:"database"`
	PgVersion  int                `json:"pg_version"`
	Enums      map[int]map[int]string `json:"enums"`       // 枚举成员映射（数据解码用）
	EnumLabels map[int][]string   `json:"enum_labels"`     // 枚举有序标签（DDL 重建用）
	Tables     []ExportMetaTable  `json:"tables"`
}

// AutoDiscoverAllTablesOrdered 按 pg_class 插入顺序返回去重后的表列表
func AutoDiscoverAllTablesOrdered(dbDir string, pageSize int) []*meta.TableMeta {
	abs, _ := filepath.Abs(dbDir)
	pgClassPath := SysFilePath(abs, pgClassOID)
	nsMap := loadNSMap(abs)
	var classEntries []*classRow
	if pgClassPath != "" {
		iterateTuples(pgClassPath, pageSize, func(pno, idx int, t *tuple.HeapTuple) bool {
			if row := classFields(t); row != nil {
				classEntries = append(classEntries, row)
			}
			return true
		})
	}
	pgAttrPath := SysFilePath(abs, pgAttrOID)
	attByRel := map[int][]*meta.Column{}
	if pgAttrPath != "" {
		attByRel = loadAttrs(abs, pgAttrPath)
	}
	// 约束按 reloid 预分组（避免每表全扫 pg_constraint）
	constraintByRel := map[int][]ConstraintInfo{}
	if cPath := SysFilePath(abs, pgConstraintOID); cPath != "" {
		iterateTuples(cPath, pageSize, func(pno, idx int, t *tuple.HeapTuple) bool {
			name, ctype, relid, conindid, key, src := constraintFields(t)
			if relid == 0 {
				return true
			}
			switch ctype {
			case 'p', 'u':
				if len(key) > 0 {
					constraintByRel[relid] = append(constraintByRel[relid], ConstraintInfo{Name: name, Type: ctype, Cols: key, Conindid: conindid})
				}
			case 'c':
				if src != "" {
					constraintByRel[relid] = append(constraintByRel[relid], ConstraintInfo{Name: name, Type: ctype, Src: src})
				}
			}
			return true
		})
	}
	type entry struct{ tm *meta.TableMeta }
	pos := map[string]int{}
	var order []*meta.TableMeta
	for _, row := range classEntries {
		cols := attByRel[row.oid]
		if len(cols) == 0 {
			continue
		}
		schema := nsMap[row.relns]
		if schema == "" {
			schema = "public"
		}
		tm := &meta.TableMeta{
			DBName: filepath.Base(abs), Schema: schema, Relname: row.relname,
			Relfilenode: row.relfilenode, Reloid: row.oid, Columns: cols, Relkind: row.relkind,
			Toastrelid: row.toastrelid,
			TypeNames:  BuildTypeNameMap(abs),
		}
		tm.PrimaryKey = primaryKeyCols(constraintByRel[row.oid], cols)
		// 每个 relfilenode 只返回一次：名字键与 relfilenode 键登记到同一位置（此前重复 append 致调用方双份输出）
		if _, ok := pos[tm.FullName()]; !ok {
			pos[tm.FullName()] = len(order)
			if row.relfilenode != 0 {
				pos[strconv.Itoa(row.relfilenode)] = len(order)
			}
			order = append(order, tm)
		}
	}
	return order
}

// ExportMetaToJSON 离线导出元数据
func ExportMetaToJSON(dbDir string, pageSize int) ExportMetaJSON {
	// 修复 v0.2.1：--export-meta 分支在 main.go 提前 return，走不到直连路径的
	// catalog.LoadEnumMap(dbDir)（main.go:157），导致此处 EnumLookupAll/EnumLabelsAll
	// 取到空映射、meta.json 的 enums/enum_labels 恒为空。此处补一次加载，与直连路径一致。
	LoadEnumMap(dbDir)
	ordered := AutoDiscoverAllTablesOrdered(dbDir, pageSize)
	seen := map[string]bool{}
	var out []ExportMetaTable
	for _, tm := range ordered {
		unique := tm.Schema + "." + tm.Relname
		if seen[unique] {
			continue
		}
		seen[unique] = true
		cols := []ExportCol{}
		for _, c := range tm.Columns {
			if c.Attdropped {
				continue
			}
			attalign := c.Attalign
			if attalign == "" {
				attalign = "c"
			}
			attstorage := c.Attstorage
			if attstorage == "" {
				attstorage = "x"
			}
			cols = append(cols, ExportCol{
				Name: c.Name, TypeOid: c.Atttypid, Len: c.Attlen, Attnum: c.Attnum,
				Typmod: c.Typmod, Notnull: c.Notnull, Dropped: c.Attdropped,
				Attalign: attalign, Attbyval: c.Attbyval, Attstorage: attstorage,
			})
		}
		pk := tm.PrimaryKey
		if pk == nil {
			pk = []string{}
		}
		tn := tm.TypeNames
		if tn == nil {
			tn = map[int]string{}
		}
		out = append(out, ExportMetaTable{
			Schema: tm.Schema, Table: tm.Relname, Oid: tm.Reloid, Relkind: tm.Relkind,
			Relfilenode: tm.Relfilenode, Toastrelid: tm.Toastrelid,
			PrimaryKey: pk, TypeNames: tn, Columns: cols,
		})
	}
	return ExportMetaJSON{
		Database: filepath.Base(dbDir), PgVersion: 92004,
		Enums: types.EnumLookupAll(), EnumLabels: types.EnumLabelsAll(),
		Tables: out,
	}
}

// MarshalIndent JSON 输出（2 空格缩进，非 ASCII 原样）
func MarshalIndent(v interface{}) ([]byte, error) {
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	s := buf.String()
	s = strings.TrimSuffix(s, "\n")
	return []byte(s), nil
}

// ---- pg_index 宽松解析（v0.2.6，移植自 pg2sql-go varlena 定位法，兼容不同布局）----
// IndexRow pg_index 行关键字段
type IndexRow struct {
	IndexRelID int
	RelID      int
	IsUnique   bool
	IsPrimary  bool
	Keys       []int
}

// locateIndkey 在数据区 [18,34) 范围定位第一个合法 varlena 头（indkey 起始偏移，相对 t_hoff）。
// bool 区（值 0/1）与 padding（0x00）不会产生合法 varlena 头，首个合法头即 indkey。
func locateIndkey(raw []byte, hoff int) int {
	start := hoff + 18
	end := hoff + 34
	if end > len(raw) {
		end = len(raw)
	}
	for pos := start; pos+4 <= end; pos++ {
		if raw[pos] == 0 {
			continue
		}
		kind, total, _, _ := binary.VarlenaParse(raw, pos)
		if kind != "" && total >= 4 {
			return pos - hoff
		}
	}
	return -1
}

// indexFields 解析 pg_index 行：indexrelid@0 indrelid@4 indisunique@10 indisprimary@11
//（openGauss 列序与 PG 不同，经 pg_class 实测校准），indkey 用 varlena 定位。
func indexFields(t *tuple.HeapTuple) *IndexRow {
	raw := t.Raw
	hoff := int(t.HOff)
	if hoff+15 > len(raw) {
		return nil
	}
	ir := &IndexRow{
		IndexRelID: int(binary.U32(raw, hoff)),
		RelID:      int(binary.U32(raw, hoff+4)),
		IsUnique:   raw[hoff+10] != 0, // openGauss 列4 indisunique
		IsPrimary:  raw[hoff+11] != 0, // openGauss 列5 indisprimary
	}
	pos := locateIndkey(raw, hoff)
	if pos >= 0 && hoff+pos+4 <= len(raw) {
		ir.Keys = decodeInt2Array(raw[hoff+pos:])
	}
	return ir
}

// IndexInfo 非主键索引导出信息
type IndexInfo struct {
	Name   string
	Unique bool
	Keys   []int
}

// loadBackingIndexOids 返回 reloid 上被 contype in ('p','u') 约束 conindid 引用的索引 OID 集合。
// 这些索引由 CREATE TABLE 内联 PRIMARY KEY / UNIQUE 约束隐式创建（与约束同名），
// 导出独立 CREATE [UNIQUE] INDEX 会与内联约束重复建索引，严格导入时报 relation already exists。
func loadBackingIndexOids(dbDir string, reloid, pageSize int) map[int]bool {
	skip := map[int]bool{}
	if reloid == 0 {
		return skip
	}
	path := SysFilePath(dbDir, pgConstraintOID)
	if path == "" {
		return skip
	}
	iterateTuples(path, pageSize, func(pno, idx int, t *tuple.HeapTuple) bool {
		_, ctype, relid, conindid, _, _ := constraintFields(t)
		if relid == reloid && (ctype == 'p' || ctype == 'u') && conindid != 0 {
			skip[conindid] = true
		}
		return true
	})
	return skip
}

// LoadIndexes 返回目标表（reloid）的非主键索引（含唯一索引）；表达式/部分索引跳过。
// v0.2.7：除 indisprimary 外，额外跳过被 contype in ('p','u') 约束 conindid 引用的 backing 索引
//（PK/UNIQUE 约束已内联进 CREATE TABLE，其同名隐式索引不再重复 emit 独立 CREATE INDEX）。
func LoadIndexes(dbDir string, reloid int, pageSize int) []IndexInfo {
	var out []IndexInfo
	if reloid == 0 {
		return out
	}
	// indexrelid → 索引名（pg_class），同时取 pg_index 的实时 relfilenode
	//（openGauss 运行中 relmapper 刷盘可能滞后，以 pg_class.relfilenode 为准）
	idxName := map[int]string{}
	idxRfn := 0
	classPath := SysFilePath(dbDir, pgClassOID)
	if classPath == "" {
		return out
	}
	iterateTuples(classPath, pageSize, func(pno, idx int, t *tuple.HeapTuple) bool {
		if row := classFields(t); row != nil && row.oid != 0 {
			idxName[row.oid] = row.relname
			if row.oid == pgIndexOID && row.relfilenode != 0 {
				idxRfn = row.relfilenode
			}
		}
		return true
	})
	if idxRfn == 0 {
		return out
	}
	// 约束（PK/UNIQUE）backing 索引 OID 集合：内联约束已隐式建同名索引，不再独立导出
	backing := loadBackingIndexOids(dbDir, reloid, pageSize)
	p := filepath.Join(dbDir, strconv.Itoa(idxRfn))
	iterateTuples(p, pageSize, func(pno, idx int, t *tuple.HeapTuple) bool {
		ir := indexFields(t)
		if ir == nil || ir.RelID != reloid || ir.IsPrimary {
			return true
		}
		if backing[ir.IndexRelID] {
			return true // p/u 约束 backing 索引（已内联进 CREATE TABLE），跳过独立 CREATE INDEX
		}
		if len(ir.Keys) == 0 {
			return true
		}
		name := idxName[ir.IndexRelID]
		if name == "" {
			return true
		}
		out = append(out, IndexInfo{Name: name, Unique: ir.IsUnique, Keys: ir.Keys})
		return true
	})
	return out
}

// SequenceInfo 序列对象
type SequenceInfo struct {
	Schema string
	Name   string
}

// LoadSequences 返回本库全部序列（pg_class relkind='S'），OID → (schema, name)。
func LoadSequences(dbDir string, pageSize int) map[int]SequenceInfo {
	out := map[int]SequenceInfo{}
	nsMap := loadNSMap(dbDir)
	iterateTuples(SysFilePath(dbDir, pgClassOID), pageSize, func(pno, idx int, t *tuple.HeapTuple) bool {
		if row := classFields(t); row != nil && row.relkind == "S" {
			schema := nsMap[row.relns]
			if schema == "" {
				schema = "public"
			}
			out[row.oid] = SequenceInfo{Schema: schema, Name: row.relname}
		}
		return true
	})
	return out
}
