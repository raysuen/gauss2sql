// Package heapfile 移植自 gauss2sql/heapfile.py（Author: raysuen）
// openGauss 堆文件读取与导出：页面遍历、字段提取、TOAST 关联、SQL/CSV 生成。
package heapfile

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gauss2sql-go/internal/binary"
	"gauss2sql-go/internal/meta"
	"gauss2sql-go/internal/page"
	"gauss2sql-go/internal/tuple"
	"gauss2sql-go/internal/types"
)

// ToastFetcher 外联 TOAST 重组接口
type ToastFetcher interface {
	Resolve(valueid int) []byte
}

// ColLayout 列布局 (attlen, is_varlena, attalign)
type ColLayout struct {
	Attlen     int
	IsVarlena  bool
	Attalign   string
}

var alignSizes = map[string]int{"c": 1, "s": 2, "i": 4, "d": 8}

var fixedAlignOverrides = map[int]string{
	19: "c", 18: "c", 2950: "c", 27: "s", 30: "i", 22: "i",
}

func colAlign(col *meta.Column) string {
	if a, ok := alignSizes[col.Attalign]; ok && a > 0 {
		return col.Attalign
	}
	if a, ok := fixedAlignOverrides[col.Atttypid]; ok {
		return a
	}
	alen := col.Attlen
	if alen <= 0 {
		return "i"
	}
	if alen >= 8 {
		return "d"
	}
	if alen >= 4 {
		return "i"
	}
	if alen >= 2 {
		return "s"
	}
	return "c"
}

func layoutAlign(attlen int, attalign string) int {
	if a, ok := alignSizes[attalign]; ok {
		return a
	}
	if attlen <= 0 {
		return 4
	}
	if attlen >= 8 {
		return 8
	}
	if attlen >= 4 {
		return 4
	}
	if attlen >= 2 {
		return 2
	}
	return 1
}

// ExtractFieldsDirect 顺序提取数据区字段
func ExtractFieldsDirect(raw []byte, tHoff int, nulls []bool, colLengths []ColLayout) [][]byte {
	pos := 0
	var fields [][]byte
	nraw := len(raw)
	for i, item := range colLengths {
		attlen := item.Attlen
		isVar := item.IsVarlena
		align := layoutAlign(attlen, item.Attalign)
		if i < len(nulls) && nulls[i] {
			fields = append(fields, nil)
			continue
		}
		if isVar {
			offset := tHoff + pos
			if offset >= nraw {
				fields = append(fields, nil)
				break
			}
			if raw[offset]&1 == 0 { // 4B 头错位对齐
				if align > 1 && (tHoff+pos)%align != 0 {
					pos = (pos + align - 1) &^ (align - 1)
					offset = tHoff + pos
					if offset >= nraw {
						fields = append(fields, nil)
						break
					}
				}
			}
			kind, total, _, _ := binary.VarlenaParse(raw, offset)
			switch kind {
			case binary.VARLENAExternal:
				fields = append(fields, append([]byte(nil), raw[offset:offset+18]...))
				pos += 18
			case binary.VARLENA1B, binary.VARLENA4B, binary.VARLENA4BComp:
				fields = append(fields, append([]byte(nil), raw[offset:offset+total]...))
				pos += total
			default:
				fields = append(fields, nil)
				break
			}
		} else {
			if align > 1 {
				pos = (pos + align - 1) &^ (align - 1)
			}
			offset := tHoff + pos
			if offset+attlen > nraw {
				fields = append(fields, nil)
				break
			}
			fields = append(fields, append([]byte(nil), raw[offset:offset+attlen]...))
			pos += attlen
		}
	}
	return fields
}

// BuildColLengths 从 TableMeta 构建列布局
func BuildColLengths(tm *meta.TableMeta) []ColLayout {
	var out []ColLayout
	for _, col := range tm.Columns {
		isVar := col.Attlen == -1 || types.VarlenaTypes[col.Atttypid]
		attlen := col.Attlen
		if attlen < 0 {
			attlen = 0
		}
		out = append(out, ColLayout{attlen, isVar, colAlign(col)})
	}
	return out
}

// CalculateTupleSize 元组实际字节大小
func CalculateTupleSize(raw []byte, tHoff int, nulls []bool, colLengths []ColLayout) int {
	dataSize := 0
	nraw := len(raw)
	for i, item := range colLengths {
		align := layoutAlign(item.Attlen, item.Attalign)
		if i < len(nulls) && nulls[i] {
			continue
		}
		if item.IsVarlena {
			offset := tHoff + dataSize
			if offset >= nraw {
				break
			}
			if raw[offset]&1 == 0 {
				if align > 1 && (tHoff+dataSize)%align != 0 {
					dataSize = (dataSize + align - 1) &^ (align - 1)
					offset = tHoff + dataSize
					if offset >= nraw {
						break
					}
				}
			}
			kind, total, _, _ := binary.VarlenaParse(raw, offset)
			if kind == binary.VARLENAExternal {
				dataSize += 18
			} else if kind != "" {
				dataSize += total
			} else {
				break
			}
		} else {
			if align > 1 {
				dataSize = (dataSize + align - 1) &^ (align - 1)
			}
			dataSize += item.Attlen
		}
	}
	return tHoff + dataSize
}

// DecodeFields 字段字节 → 可打印值列表（只含未 dropped 列）
func DecodeFields(fields [][]byte, tm *meta.TableMeta, toast ToastFetcher) []string {
	types.SetRoleNameMap(tm.RoleMap)
	var values []string
	for i, col := range tm.Columns {
		if col.Attdropped {
			continue
		}
		if i >= len(fields) {
			values = append(values, "")
			continue
		}
		raw := fields[i]
		if raw == nil {
			values = append(values, "")
			continue
		}
		isVar := col.Attlen == -1 || types.VarlenaTypes[col.Atttypid]
		if isVar && len(raw) > 0 {
			kind, _, _, _ := binary.VarlenaParse(raw, 0)
			if kind == binary.VARLENAExternal && toast != nil {
				ext := binary.ParseExternalPointer(raw, 0)
				if ext == nil {
					values = append(values, "")
					continue
				}
				payload := toast.Resolve(int(ext.Valueid))
				if payload == nil {
					values = append(values, "__TOAST_MISSING__")
					continue
				}
				expected := int(ext.Rawsize) - 4
				if ext.Compressed {
					data := binary.ToastDecompress(payload, expected, int(ext.Method))
					if data == nil {
						values = append(values, "__TOAST_CORRUPT__")
						continue
					}
					payload = data
				} else if len(payload) != expected {
					values = append(values, "__TOAST_MISSING__")
					continue
				}
				raw = binary.RebuildVarlena(payload)
			} else if kind == binary.VARLENAExternal && toast == nil {
				values = append(values, "__TOAST_MISSING__")
				continue
			}
		}
		values = append(values, types.DecodeValue(col.Atttypid, raw))
	}
	return values
}

// Row 一行
type Row struct {
	Ctid    string
	Values  []string
	Deleted bool
}

// HeapFile 堆文件读取器
type HeapFile struct {
	Path       string
	PageSize   int
	Toast      ToastFetcher
	badPages   []int
	pageSize   int
}

// NewHeapFile 打开堆文件
func NewHeapFile(path string, pageSize int) *HeapFile {
	return &HeapFile{Path: path, PageSize: pageSize}
}

func (h *HeapFile) detectSize() int {
	if h.PageSize > 0 {
		return h.PageSize
	}
	data, err := os.ReadFile(h.Path)
	if err == nil && len(data) >= 130 {
		if ps := page.DetectPageSize(data[:130]); ps > 0 {
			return ps
		}
	}
	fi, err := os.Stat(h.Path)
	if err == nil {
		sz := fi.Size()
		if sz > 0 && sz <= 32768 {
			return int(sz)
		}
	}
	return page.PageSize
}

// IterPages 逐页返回
func (h *HeapFile) IterPages(cb func(pageno int, pg *page.Page) bool) {
	ps := h.detectSize()
	h.pageSize = ps
	data, err := os.ReadFile(h.Path)
	if err != nil {
		return
	}
	npages := len(data) / ps
	for pno := 0; pno < npages; pno++ {
		raw := data[pno*ps : (pno+1)*ps]
		pg := page.NewPage(pno, raw, ps)
		if !pg.HasValidLayout {
			h.badPages = append(h.badPages, pno)
			continue
		}
		if !cb(pno, pg) {
			return
		}
	}
}

// iterTuples 标准 ItemId 遍历
func (h *HeapFile) iterTuples(includeDeleted bool, cb func(pageno, idx int, t *tuple.HeapTuple) bool) {
	h.IterPages(func(pageno int, pg *page.Page) bool {
		for _, it := range pg.Items {
			if it.Flags != page.ItemIDNormal {
				continue
			}
			data := pg.Raw[it.Off : it.Off+it.Len]
			t, err := tuple.New(data)
			if err != nil {
				continue
			}
			if !includeDeleted && !t.IsLive() {
				continue
			}
			if !cb(pageno, it.Index, t) {
				return false
			}
		}
		return true
	})
}

// scanTuples 数据区扫描兜底
func (h *HeapFile) scanTuples(nExpected int, colLengths []ColLayout, cb func(pageno, pos int, t *tuple.HeapTuple) bool) {
	h.IterPages(func(pageno int, pg *page.Page) bool {
		pdUpper := pg.Upper
		pdSpecial := pg.Special
		if pdUpper < pg.HeaderSize || pdUpper >= pdSpecial {
			return true
		}
		raw := pg.Raw
		pos := pdUpper
		for pos+tuple.HeapTupleHeaderSize <= pdSpecial {
			tXmin := binary.U32(raw, pos)
			tImask2 := binary.U16(raw, pos+18)
			tImask := binary.U16(raw, pos+20)
			tHoff := int(raw[pos+22])
			nattrs := int(tImask2) & tuple.HeapNattMask
			if tHoff < tuple.HeapTupleHeaderSize || tHoff > 256 ||
				nattrs == 0 || nattrs > 1600 ||
				(nExpected > 0 && int(nattrs) != nExpected) ||
				tXmin == 0 || tXmin > 0x7FFFFFFF ||
				(tImask&0xFF00) == 0 {
				pos += 4
				continue
			}
			t, err := tuple.New(raw[pos:pdSpecial])
			if err != nil {
				pos += 4
				continue
			}
			if int(t.HOff) != tHoff || t.Nattrs != int(nattrs) {
				pos += 4
				continue
			}
			if !t.IsHeaderConsistent() {
				pos += 4
				continue
			}
			if !cb(pageno, pos, t) {
				return false
			}
			actual := CalculateTupleSize(raw[pos:], int(t.HOff), t.GetNulls(), colLengths)
			nextPos := (actual + 7) &^ 7
			if nextPos < 8 {
				nextPos = 8
			}
			pos += nextPos
		}
		return true
	})
}

// DumpRows 产出行
func (h *HeapFile) DumpRows(tm *meta.TableMeta, includeDeleted, onlyDeleted bool, limit int) []Row {
	if onlyDeleted {
		includeDeleted = true
	}
	colLengths := BuildColLengths(tm)
	nExpected := len(tm.Columns)

	buildRow := func(t *tuple.HeapTuple, blk, off int) *Row {
		deleted := t.IsDeleted()
		if !t.IsLive() && !deleted {
			return nil
		}
		if !includeDeleted && deleted {
			return nil
		}
		if onlyDeleted && !deleted {
			return nil
		}
		nulls := t.GetNulls()
		fields := ExtractFieldsDirect(t.Raw, int(t.HOff), nulls, colLengths)
		values := DecodeFields(fields, tm, h.Toast)
		return &Row{Ctid: fmtCtid(blk, off), Values: values, Deleted: deleted}
	}
	rowHasValid := func(r *Row) bool {
		for _, v := range r.Values {
			if v != "" && v != "__TOAST_MISSING__" {
				return true
			}
		}
		return false
	}

	// Phase 1
	var standard []Row
	foundStandard := false
	hasValid := false
	count := 0
	h.iterTuples(true, func(pageno, idx int, t *tuple.HeapTuple) bool {
		foundStandard = true
		r := buildRow(t, pageno, idx)
		if r == nil {
			return true
		}
		standard = append(standard, *r)
		if rowHasValid(r) {
			hasValid = true
		}
		count++
		if limit > 0 && count >= limit {
			return false
		}
		return true
	})
	if foundStandard && hasValid {
		return standard
	}
	// Phase 2
	h.badPages = nil
	var out []Row
	count = 0
	h.scanTuples(nExpected, colLengths, func(pageno, pos int, t *tuple.HeapTuple) bool {
		r := buildRow(t, pageno, pos)
		if r == nil {
			return true
		}
		out = append(out, *r)
		count++
		if limit > 0 && count >= limit {
			return false
		}
		return true
	})
	return out
}

func fmtCtid(blk, off int) string {
	return "(" + strconv.Itoa(blk) + "," + strconv.Itoa(off) + ")"
}

var noQuoteOids = map[int]bool{
	16: true, 20: true, 21: true, 23: true, 26: true, 700: true, 701: true, 1700: true,
}

func sqlQuoteValue(v string, col *meta.Column) string {
	if noQuoteOids[col.Atttypid] {
		if v == "NaN" || v == "Infinity" || v == "-Infinity" {
			return types.SQLStringLiteral(v)
		}
		return v
	}
	return types.SQLStringLiteral(v)
}

// ToSQL 生成 INSERT 语句
func (h *HeapFile) ToSQL(tm *meta.TableMeta, includeDeleted, onlyDeleted bool, limit int, completeInsert, replace bool, fields map[string]bool) []string {
	verb := "INSERT INTO"
	if replace {
		verb = "REPLACE INTO"
	}
	var liveCols []*meta.Column
	for _, c := range tm.Columns {
		if !c.Attdropped {
			liveCols = append(liveCols, c)
		}
	}
	var outIdx []int
	if len(fields) > 0 {
		for i, c := range liveCols {
			if fields[c.Name] {
				outIdx = append(outIdx, i)
			}
		}
	} else {
		for i := range liveCols {
			outIdx = append(outIdx, i)
		}
	}
	var colNames []string
	for _, i := range outIdx {
		colNames = append(colNames, "\""+liveCols[i].Name+"\"")
	}
	colStr := ""
	if completeInsert {
		colStr = "(" + strings.Join(colNames, ", ") + ")"
	}
	target := "\"" + tm.Schema + "\".\"" + tm.Relname + "\""
	var stmts []string
	for _, row := range h.DumpRows(tm, includeDeleted, onlyDeleted, limit) {
		var sqlVals []string
		for k, i := range outIdx {
			v := row.Values[i]
			if v == "" || v == "__TOAST_MISSING__" {
				sqlVals = append(sqlVals, "NULL")
			} else {
				sqlVals = append(sqlVals, sqlQuoteValue(v, liveCols[outIdx[k]]))
			}
		}
		stmt := verb + " " + target + " " + colStr + " VALUES (" + strings.Join(sqlVals, ", ") + ");"
		if row.Deleted {
			stmt = "-- DELETED ctid=" + row.Ctid + "\n" + stmt
		}
		stmts = append(stmts, stmt)
	}
	return stmts
}

// ToData 生成 CSV 行
func (h *HeapFile) ToData(tm *meta.TableMeta, includeDeleted, onlyDeleted bool, limit int, delimiter string, fields map[string]bool, header bool) []string {
	var liveCols []*meta.Column
	for _, c := range tm.Columns {
		if !c.Attdropped {
			liveCols = append(liveCols, c)
		}
	}
	var outIdx []int
	if len(fields) > 0 {
		for i, c := range liveCols {
			if fields[c.Name] {
				outIdx = append(outIdx, i)
			}
		}
	} else {
		for i := range liveCols {
			outIdx = append(outIdx, i)
		}
	}
	csvField := func(raw string) string {
		needsQuote := strings.Contains(raw, delimiter) || strings.Contains(raw, "\n") ||
			strings.Contains(raw, "\r") || strings.Contains(raw, "\"") || raw == `\N`
		if needsQuote {
			return "\"" + strings.ReplaceAll(raw, "\"", "\"\"") + "\""
		}
		return raw
	}
	var lines []string
	if header {
		var hdr []string
		for _, i := range outIdx {
			hdr = append(hdr, csvField(liveCols[i].Name))
		}
		lines = append(lines, strings.Join(hdr, delimiter))
	}
	for _, row := range h.DumpRows(tm, includeDeleted, onlyDeleted, limit) {
		var parts []string
		for _, i := range outIdx {
			v := row.Values[i]
			if v == "" || v == "__TOAST_MISSING__" {
				parts = append(parts, `\N`)
			} else {
				parts = append(parts, csvField(v))
			}
		}
		lines = append(lines, strings.Join(parts, delimiter))
	}
	return lines
}

// _ import guard
var _ = filepath.Join
