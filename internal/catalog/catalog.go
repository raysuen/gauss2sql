// Package catalog 移植自 gauss2sql/catalog.py（Author: raysuen）
// 表结构元数据：离线解析 pg_class/pg_attribute/pg_type/pg_partition。
package catalog

import (
	"encoding/json"
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
	pgClassOID    = 1259
	pgAttrOID     = 1249
	pgTypeOID     = 1247
	pgNSOID       = 2615
	pgEnumRelfile = 3501
	pgDBRelfile   = 1262
)

var pgEncodingToCodec = map[int]string{
	0: "latin-1", 6: "utf-8",
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

// SysFilePath 定位系统目录物理文件
func SysFilePath(dbDir string, standardOid int) string {
	std := filepath.Join(dbDir, strconv.Itoa(standardOid))
	if _, err := os.Stat(std); err == nil {
		return std
	}
	fn := loadRelmapper(dbDir)[standardOid]
	if fn != 0 {
		p := filepath.Join(dbDir, strconv.Itoa(fn))
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if standardOid != pgClassOID {
		pc := SysFilePath(dbDir, pgClassOID)
		if pc != "" {
			iterateTuples(pc, 0, func(pno, idx int, t *tuple.HeapTuple) bool {
				row := classFields(t)
				if row == nil {
					return true
				}
				oid, rfn := row.oid, row.relfilenode
				if oid == standardOid && rfn != 0 && rfn != standardOid {
					p := filepath.Join(dbDir, strconv.Itoa(rfn))
					if _, err := os.Stat(p); err == nil {
						pc = p
						return false
					}
				}
				return true
			})
			if pc != "" && pc != filepath.Join(dbDir, strconv.Itoa(pgClassOID)) {
				return pc
			}
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
		if fields[i] == nil {
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
		Relfilenode: targetRF, Columns: cols, Relkind: targetRelkind,
		Toastrelid: targetToastrelid,
		TypeNames:   BuildTypeNameMap(dbDir),
	}
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
		Relfilenode: targetRF, Columns: cols, Relkind: parentRelkind,
		TypeNames: BuildTypeNameMap(dbDir),
	}
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
	Schema     string         `json:"schema"`
	Table      string         `json:"table"`
	Relfilenode int           `json:"relfilenode"`
	Toastrelid int            `json:"toastrelid"`
	PrimaryKey []string       `json:"primary_key"`
	Columns    []ExportCol    `json:"columns"`
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
	Database string            `json:"database"`
	PgVersion int               `json:"pg_version"`
	Tables   []ExportMetaTable `json:"tables"`
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
			Relfilenode: row.relfilenode, Columns: cols, Relkind: row.relkind,
			TypeNames: BuildTypeNameMap(abs),
		}
		if _, ok := pos[tm.FullName()]; !ok {
			pos[tm.FullName()] = len(order)
			order = append(order, tm)
		}
		if row.relfilenode != 0 {
			k := strconv.Itoa(row.relfilenode)
			if _, ok := pos[k]; !ok {
				pos[k] = len(order)
				order = append(order, tm)
			}
		}
	}
	return order
}

// ExportMetaToJSON 离线导出元数据
func ExportMetaToJSON(dbDir string, pageSize int) ExportMetaJSON {
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
		out = append(out, ExportMetaTable{
			Schema: tm.Schema, Table: tm.Relname, Relfilenode: tm.Relfilenode,
			Toastrelid: tm.Toastrelid, PrimaryKey: pk, Columns: cols,
		})
	}
	return ExportMetaJSON{Database: filepath.Base(dbDir), PgVersion: 92004, Tables: out}
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
