// Package heapfile 移植自 gauss2sql/heapfile.py（Author: raysuen）
// openGauss 堆文件读取与导出：页面遍历、字段提取、TOAST 关联、SQL/CSV 生成。
package heapfile

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

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
	Attlen    int
	IsVarlena bool
	Attalign  string
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
				if offset+18 > nraw {
					return fields // varlena 越界（损坏数据）：截断返回，同 CalculateTupleSize 语义
				}
				fields = append(fields, append([]byte(nil), raw[offset:offset+18]...))
				pos += 18
			case binary.VARLENA1B, binary.VARLENA4B, binary.VARLENA4BComp:
				if offset+total > nraw {
					return fields
				}
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
// NullMarker 内部 NULL 哨兵：与空字符串 ” 严格区分。
// 以 \x00 开头——openGauss text/varchar/jsonb 等文本类型不允许裸 NUL 字节，
// 因此该标记不可能与真实数据冲突。
const NullMarker = "\x00__NULL__"

func DecodeFields(fields [][]byte, tm *meta.TableMeta, toast ToastFetcher) []string {
	var values []string
	for i, col := range tm.Columns {
		if col.Attdropped {
			continue
		}
		if i >= len(fields) {
			values = append(values, NullMarker)
			continue
		}
		raw := fields[i]
		if raw == nil {
			values = append(values, NullMarker)
			continue
		}
		isVar := col.Attlen == -1 || types.VarlenaTypes[col.Atttypid]
		if isVar && len(raw) > 0 {
			kind, _, _, _ := binary.VarlenaParse(raw, 0)
			if kind == binary.VARLENAExternal && toast != nil {
				ext := binary.ParseExternalPointer(raw, 0)
				if ext == nil {
					values = append(values, NullMarker)
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
	Path     string
	PageSize int
	Toast    ToastFetcher
	Parallel int // 页级并行工作数（>1 时 DumpRows 自动走并行路径）
	badPages []int
	pageSize int
}

// NewHeapFile 打开堆文件
func NewHeapFile(path string, pageSize int) *HeapFile {
	return &HeapFile{Path: path, PageSize: pageSize}
}

func (h *HeapFile) detectSize() int {
	if h.PageSize > 0 {
		return h.PageSize
	}
	// 仅读文件头 130 字节探测页大小（v0.2.13：避免整文件 ReadFile 造成内存峰值）
	f, err := os.Open(h.Path)
	if err == nil {
		buf := make([]byte, 130)
		n, _ := f.Read(buf)
		_ = f.Close()
		if n >= 130 {
			if ps := page.DetectPageSize(buf[:130]); ps > 0 {
				return ps
			}
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
	h.iterPagesRange(data, ps, 0, len(data)/ps, cb)
}

// iterPagesRange 指定页范围 [start, end) 逐页回调（共享整文件数据，供并行分片使用）
func (h *HeapFile) iterPagesRange(data []byte, ps, start, end int, cb func(pageno int, pg *page.Page) bool) {
	for pno := start; pno < end; pno++ {
		raw := data[pno*ps : (pno+1)*ps]
		pg := page.NewPage(pno, raw, ps)
		if !pg.HasValidLayout {
			h.badPages = append(h.badPages, pno) // 损坏页：记录供 Phase2 数据区扫描补漏
			continue
		}
		if !cb(pno, pg) {
			return
		}
	}
}

// iterTuplesRange 指定页范围标准 ItemId 遍历
func (h *HeapFile) iterTuplesRange(data []byte, ps, start, end int, includeDeleted bool, cb func(pageno, idx int, t *tuple.HeapTuple) bool) {
	h.iterPagesRange(data, ps, start, end, func(pageno int, pg *page.Page) bool {
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

// scanTuplesRange 指定页范围数据区扫描兜底
func (h *HeapFile) scanTuplesRange(data []byte, ps, start, end int, nExpected int, colLengths []ColLayout, cb func(pageno, pos int, t *tuple.HeapTuple) bool) {
	h.iterPagesRange(data, ps, start, end, func(pageno int, pg *page.Page) bool {
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

// iterTuples 标准 ItemId 遍历（串行）
func (h *HeapFile) iterTuples(includeDeleted bool, cb func(pageno, idx int, t *tuple.HeapTuple) bool) {
	ps := h.detectSize()
	h.pageSize = ps
	data, err := os.ReadFile(h.Path)
	if err != nil {
		return
	}
	h.iterTuplesRange(data, ps, 0, len(data)/ps, includeDeleted, cb)
}

// scanTuples 数据区扫描兜底（串行）
func (h *HeapFile) scanTuples(nExpected int, colLengths []ColLayout, cb func(pageno, pos int, t *tuple.HeapTuple) bool) {
	ps := h.detectSize()
	h.pageSize = ps
	data, err := os.ReadFile(h.Path)
	if err != nil {
		return
	}
	h.scanTuplesRange(data, ps, 0, len(data)/ps, nExpected, colLengths, cb)
}

// DumpRows 产出行（--parallel>1 且无 limit 时自动走页级并行）
func (h *HeapFile) DumpRows(tm *meta.TableMeta, includeDeleted, onlyDeleted bool, limit int) []Row {
	if onlyDeleted {
		includeDeleted = true
	}
	if h.Parallel > 1 && limit == 0 {
		return h.DumpRowsParallel(tm, includeDeleted, onlyDeleted, h.Parallel)
	}
	return h.dumpRowsSerial(tm, includeDeleted, onlyDeleted, limit)
}

// dumpRowsSerial 串行主体（两阶段：标准 ItemId 遍历 → 数据区扫描兜底）
func (h *HeapFile) dumpRowsSerial(tm *meta.TableMeta, includeDeleted, onlyDeleted bool, limit int) []Row {
	types.SetRoleNameMap(tm.RoleMap)
	colLengths := BuildColLengths(tm)
	nExpected := len(tm.Columns)

	// Phase 1
	var standard []Row
	foundStandard := false
	hasValid := false
	count := 0
	h.iterTuples(true, func(pageno, idx int, t *tuple.HeapTuple) bool {
		foundStandard = true
		r := h.buildRow(t, pageno, idx, tm, colLengths, includeDeleted, onlyDeleted)
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
		if len(h.badPages) > 0 {
			// 存在损坏页：对坏页做数据区扫描补漏（坏页在 Phase1 无行，追加不重复）
			ps := h.detectSize()
			if data, err := os.ReadFile(h.Path); err == nil {
				for _, pno := range h.badPages {
					h.scanTuplesRange(data, ps, pno, pno+1, nExpected, colLengths, func(pageno, pos int, t *tuple.HeapTuple) bool {
						r := h.buildRow(t, pageno, pos, tm, colLengths, includeDeleted, onlyDeleted)
						if r == nil {
							return true
						}
						standard = append(standard, *r)
						return limit <= 0 || len(standard) < limit
					})
					if limit > 0 && len(standard) >= limit {
						break
					}
				}
			}
		}
		return standard
	}
	// Phase 2
	h.badPages = nil
	var out []Row
	count = 0
	h.scanTuples(nExpected, colLengths, func(pageno, pos int, t *tuple.HeapTuple) bool {
		r := h.buildRow(t, pageno, pos, tm, colLengths, includeDeleted, onlyDeleted)
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

// buildRow 由 HeapTuple 构建行（含删除/存活过滤）
func (h *HeapFile) buildRow(t *tuple.HeapTuple, blk, off int, tm *meta.TableMeta, colLengths []ColLayout, includeDeleted, onlyDeleted bool) *Row {
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

func rowHasValid(r *Row) bool {
	for _, v := range r.Values {
		if v != "" && v != NullMarker && v != "__TOAST_MISSING__" && v != "__TOAST_CORRUPT__" {
			return true
		}
	}
	return false
}

// DumpRowsParallel 页级并行产出行（workers>1）。
// 语义与串行 DumpRows 完全一致：Phase1 标准 ItemId 遍历全并行 → 全局判定
// 是否使用标准结果；否则 Phase2 数据区扫描兜底全并行。段内按页序、段间按
// 页范围顺序合并，输出与串行逐字节一致。
func (h *HeapFile) DumpRowsParallel(tm *meta.TableMeta, includeDeleted, onlyDeleted bool, workers int) []Row {
	types.SetRoleNameMap(tm.RoleMap)
	colLengths := BuildColLengths(tm)
	nExpected := len(tm.Columns)

	ps := h.detectSize()
	data, err := os.ReadFile(h.Path)
	if err != nil {
		return nil
	}
	npages := len(data) / ps
	if npages < 2 {
		return h.dumpRowsSerial(tm, includeDeleted, onlyDeleted, 0)
	}
	if workers > npages {
		workers = npages
	}

	// 页范围分段（按页号连续切分，保证合并顺序）
	segs := make([][2]int, workers)
	base, rem := npages/workers, npages%workers
	cur := 0
	for i := 0; i < workers; i++ {
		n := base
		if i < rem {
			n++
		}
		segs[i] = [2]int{cur, cur + n}
		cur += n
	}

	type segResult struct {
		rows     []Row
		hasValid bool
		badPages []int
	}
	var wg sync.WaitGroup

	// Phase 1：标准 ItemId 遍历（全并行）
	results := make([]segResult, workers)
	for i, s := range segs {
		wg.Add(1)
		go func(i int, s [2]int) {
			defer wg.Done()
			var rows []Row
			hasValid := false
			// 段内内联遍历（避免共享 h.badPages 的并发写竞争）：坏页段内收集
			var bad []int
			for pno := s[0]; pno < s[1]; pno++ {
				raw := data[pno*ps : (pno+1)*ps]
				pg := page.NewPage(pno, raw, ps)
				if !pg.HasValidLayout {
					bad = append(bad, pno)
					continue
				}
				for _, it := range pg.Items {
					if it.Flags != page.ItemIDNormal {
						continue
					}
					t, err := tuple.New(pg.Raw[it.Off : it.Off+it.Len])
					if err != nil {
						continue
					}
					if !includeDeleted && !t.IsLive() {
						continue
					}
					r := h.buildRow(t, pno, it.Index, tm, colLengths, includeDeleted, onlyDeleted)
					if r == nil {
						continue
					}
					rows = append(rows, *r)
					if rowHasValid(r) {
						hasValid = true
					}
				}
			}
			results[i] = segResult{rows: rows, hasValid: hasValid, badPages: bad}
		}(i, s)
	}
	wg.Wait()

	globalValid := false
	for i := range results {
		if results[i].hasValid {
			globalValid = true
			break
		}
	}
	if globalValid {
		var out []Row
		for i := range results {
			out = append(out, results[i].rows...)
			for _, pno := range results[i].badPages {
				h.scanTuplesRange(data, ps, pno, pno+1, nExpected, colLengths, func(pageno, pos int, t *tuple.HeapTuple) bool {
					r := h.buildRow(t, pageno, pos, tm, colLengths, includeDeleted, onlyDeleted)
					if r == nil {
						return true
					}
					out = append(out, *r)
					return true
				})
			}
		}
		return out
	}

	// Phase 2：数据区扫描兜底（全并行）
	h.badPages = nil
	results2 := make([]segResult, workers)
	for i, s := range segs {
		wg.Add(1)
		go func(i int, s [2]int) {
			defer wg.Done()
			var rows []Row
			h.scanTuplesRange(data, ps, s[0], s[1], nExpected, colLengths, func(pageno, pos int, t *tuple.HeapTuple) bool {
				r := h.buildRow(t, pageno, pos, tm, colLengths, includeDeleted, onlyDeleted)
				if r == nil {
					return true
				}
				rows = append(rows, *r)
				return true
			})
			results2[i] = segResult{rows: rows}
		}(i, s)
	}
	wg.Wait()
	var out []Row
	for i := range results2 {
		out = append(out, results2[i].rows...)
	}
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
			if v == NullMarker || v == "__TOAST_MISSING__" || v == "__TOAST_CORRUPT__" {
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
	rows := 0
	if header {
		rows++
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
			if v == NullMarker || v == "__TOAST_MISSING__" || v == "__TOAST_CORRUPT__" {
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

// ==================== 流式导出（v0.2.13，A+B：分块读 + 块级并行解析/转义 + 按序流式输出） ====================
// 内存峰值 O(workers×块大小)（默认块 512 页=4MB），替代整文件 ReadFile + 全量文本拼接；
// 语义与 ToSQL/ToData/DumpRows 完全一致（坏页补漏、Phase2 兜底、limit/fields/deleted/header）。

const chunkPages = 512 // 每块页数（8KB 页 → 4MB 窗口）

// chunkOut 一块的处理结果（按页序的行文本 + 坏页 + 有效性判定）
type chunkOut struct {
	idx      int
	lines    []string
	nRows    int
	badPages []int
	hasValid bool
}

// lineBuilder 将一行 Row 转为输出行文本；ok=false 表示该行不输出（count 模式）
type lineBuilder func(r *Row) (line string, ok bool)

// processChunk 块内标准 ItemId 遍历（坏页记录、行经 lineBuilder 转文本）。
// base 为该段文件的全局起始页号（v0.2.13：支持 >1GB 段文件 .1/.2 的页号续接）。
func (h *HeapFile) processChunk(f *os.File, ps, s, e, base int, nExpected int, colLengths []ColLayout,
	tm *meta.TableMeta, includeDeleted, onlyDeleted bool, lb lineBuilder, idx int) chunkOut {
	co := chunkOut{idx: idx}
	buf := make([]byte, (e-s)*ps)
	n, _ := f.ReadAt(buf, int64(s*ps))
	data := buf[:n]
	for pno := s; pno < e; pno++ {
		off := (pno - s) * ps
		if off+ps > len(data) {
			break
		}
		raw := data[off : off+ps]
		pg := page.NewPage(base+pno, raw, ps)
		if !pg.HasValidLayout {
			co.badPages = append(co.badPages, pno)
			continue
		}
		for _, it := range pg.Items {
			if it.Flags != page.ItemIDNormal {
				continue
			}
			t, err := tuple.New(pg.Raw[it.Off : it.Off+it.Len])
			if err != nil {
				continue
			}
			if !includeDeleted && !t.IsLive() {
				continue
			}
			r := h.buildRow(t, pno, it.Index, tm, colLengths, includeDeleted, onlyDeleted)
			if r == nil {
				continue
			}
			if rowHasValid(r) {
				co.hasValid = true
			}
			if ln, ok := lb(r); ok {
				co.lines = append(co.lines, ln)
				co.nRows++
			}
		}
	}
	return co
}

// processChunkScan 块内数据区扫描（Phase2 兜底 / 坏页补漏用）
func (h *HeapFile) processChunkScan(f *os.File, ps, s, e, base int, nExpected int, colLengths []ColLayout,
	tm *meta.TableMeta, includeDeleted, onlyDeleted bool, lb lineBuilder, idx int) chunkOut {
	co := chunkOut{idx: idx}
	buf := make([]byte, (e-s)*ps)
	n, _ := f.ReadAt(buf, int64(s*ps))
	data := buf[:n]
	h.scanTuplesRangeSeg(data, ps, s, e, base, nExpected, colLengths, func(pageno, pos int, t *tuple.HeapTuple) bool {
		r := h.buildRow(t, pageno, pos, tm, colLengths, includeDeleted, onlyDeleted)
		if r == nil {
			return true
		}
		if ln, ok := lb(r); ok {
			co.lines = append(co.lines, ln)
			co.nRows++
		}
		return true
	})
	return co
}

// runChunked 并行处理块并按块序输出（pending 最多 workers 块）；limit>0 时输出截断。
// v0.2.14：并发信号量修复——旧版一次性扇出全部 nChunks 个 goroutine（每块 ≤4MB 读缓冲 + 行文本），
// workers 仅限制输出通道缓冲、不限制并发处理数，大表（数百块）串行模式亦瞬时并发 ~2.9GB 被 cgroup OOM。
// 现以 sem 限流在飞 process ≤ workers（≥1），串行模式内存峰值降至单块级（O(4MB×2)）。
func (h *HeapFile) runChunked(workers, nChunks int, limit int, process func(idx int) chunkOut, out func(string) error) (int, []int, bool, error) {
	if workers < 1 {
		workers = 1
	}
	// v0.2.14：串行模式（workers==1）直接同步顺序循环——零 goroutine、零 pending，
	// 内存峰值 = 单块（4MB 读缓冲 + 块内行文本 ≈ 十余 MB），无乱序积压。
	if workers == 1 {
		var total int
		var badPages []int
		globalHasValid := false
		for ci := 0; ci < nChunks; ci++ {
			co := process(ci)
			badPages = append(badPages, co.badPages...)
			if co.hasValid {
				globalHasValid = true
			}
			for _, ln := range co.lines {
				if limit > 0 && total >= limit {
					return total, badPages, globalHasValid, nil
				}
				if out != nil {
					if err := out(ln); err != nil {
						return total, badPages, globalHasValid, err
					}
				}
				total++
			}
		}
		return total, badPages, globalHasValid, nil
	}
	// v0.2.14：并行模式（workers>1）主循环驱动的有界窗口派发——
	// 任务按块序派发，窗口 = workers*2（在途未完成 ≤ window）；完成结果进 pend，
	// 主循环推进 next 时滑动窗口（next 推进一个、派发一个）。乱序完成不再导致
	// pending 无界积压（旧 sem 方案实测 parallel 峰值 1.8GB），内存峰值 = O(window×块)。
	window := workers * 2
	if window < 2 {
		window = 2
	}
	resCh := make(chan chunkOut, window)
	var total int
	next := 0
	sent := 0
	pend := map[int]chunkOut{}
	var badPages []int
	globalHasValid := false
	for next < nChunks {
		for sent < nChunks && (sent-next) < window {
			i := sent
			sent++
			go func(i int) {
				resCh <- process(i)
			}(i)
		}
		for {
			if co, ok := pend[next]; ok {
				for _, ln := range co.lines {
					if limit > 0 && total >= limit {
						return total, badPages, globalHasValid, nil
					}
					if out != nil {
						if err := out(ln); err != nil {
							return total, badPages, globalHasValid, err
						}
					}
					total++
				}
				badPages = append(badPages, co.badPages...)
				if co.hasValid {
					globalHasValid = true
				}
				delete(pend, next)
				next++
				continue
			}
			break
		}
		if next >= nChunks {
			break
		}
		co := <-resCh
		pend[co.idx] = co
	}
	return total, badPages, globalHasValid, nil
}

func chunkPlan(npages, pagesPer int) [][2]int {
	n := (npages + pagesPer - 1) / pagesPer
	out := make([][2]int, n)
	for i := 0; i < n; i++ {
		s := i * pagesPer
		e := s + pagesPer
		if e > npages {
			e = npages
		}
		out[i] = [2]int{s, e}
	}
	return out
}

// segInfo 段文件信息（openGauss 表文件 >1GiB 自动分段：主文件 + .1/.2...，页号全局续接）
type segInfo struct {
	path  string // 段文件路径
	start int    // 全局起始页号
	pages int    // 段内页数
}

// listSegments 返回主文件及其全部段文件（含页号偏移）
func (h *HeapFile) listSegments(ps int) ([]segInfo, int) {
	var segs []segInfo
	fi, err := os.Stat(h.Path)
	if err != nil {
		return nil, 0
	}
	start := 0
	pages := int(fi.Size() / int64(ps))
	segs = append(segs, segInfo{path: h.Path, start: 0, pages: pages})
	start += pages
	for i := 1; ; i++ {
		p := fmt.Sprintf("%s.%d", h.Path, i)
		f, err := os.Stat(p)
		if err != nil {
			break
		}
		sp := int(f.Size() / int64(ps))
		if sp <= 0 {
			break
		}
		segs = append(segs, segInfo{path: p, start: start, pages: sp})
		start += sp
	}
	return segs, start
}

// locateSeg 全局页号 → (段索引, 段内页号)
func locateSeg(segs []segInfo, globalPno int) (int, int) {
	for i := range segs {
		if globalPno < segs[i].start+segs[i].pages {
			return i, globalPno - segs[i].start
		}
	}
	last := len(segs) - 1
	return last, globalPno - segs[last].start
}

// scanTuplesRangeSeg 段窗口内的数据区扫描（pageno 回调整全局页号 = base + 段内页）
func (h *HeapFile) scanTuplesRangeSeg(data []byte, ps, start, end, base int, nExpected int, colLengths []ColLayout, cb func(pageno, pos int, t *tuple.HeapTuple) bool) {
	for pno := start; pno < end; pno++ {
		off := (pno - start) * ps
		if off+ps > len(data) {
			break
		}
		raw := data[off : off+ps]
		pg := page.NewPage(base+pno, raw, ps)
		if !pg.HasValidLayout {
			continue
		}
		pdUpper := pg.Upper
		pdSpecial := pg.Special
		if pdUpper < pg.HeaderSize || pdUpper >= pdSpecial {
			continue
		}
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
			if !cb(base+pno, pos, t) {
				return
			}
			actual := CalculateTupleSize(raw[pos:], int(t.HOff), t.GetNulls(), colLengths)
			nextPos := (actual + 7) &^ 7
			if nextPos < 8 {
				nextPos = 8
			}
			pos += nextPos
		}
	}
}

// streamRowsCore 统一流式行主体（SQL/CSV/count 共用；v0.2.13 支持多段文件）：
// Phase1 块级标准遍历 → 按块序输出 → 坏页补漏（有有效标准行时）/ Phase2 全表数据区扫描（无有效行时）。
func (h *HeapFile) streamRowsCore(tm *meta.TableMeta, includeDeleted, onlyDeleted bool, limit int,
	lb lineBuilder, out func(string) error) (int, error) {
	if onlyDeleted {
		includeDeleted = true
	}
	types.SetRoleNameMap(tm.RoleMap)
	colLengths := BuildColLengths(tm)
	nExpected := len(tm.Columns)

	ps := h.detectSize()
	segs, totalPages := h.listSegments(ps)
	if totalPages == 0 {
		return 0, nil
	}
	workers := h.Parallel
	if workers < 1 || limit > 0 {
		workers = 1 // 与现状语义一致：limit>0 走串行
	}
	if workers > totalPages {
		workers = totalPages
	}
	// 打开全部段文件（共享句柄，ReadAt 并发安全）
	fhs := make([]*os.File, len(segs))
	for i, sg := range segs {
		f, err := os.Open(sg.path)
		if err != nil {
			return 0, err
		}
		defer f.Close()
		fhs[i] = f
	}

	plan := chunkPlan(totalPages, chunkPages)
	// Phase1：标准 ItemId 遍历（块级并行）
	process := func(idx int) chunkOut {
		s, e := plan[idx][0], plan[idx][1]
		si, localS := locateSeg(segs, s)
		return h.processChunk(fhs[si], ps, localS, localS+(e-s), segs[si].start, nExpected, colLengths, tm, includeDeleted, onlyDeleted, lb, idx)
	}
	total, badPages, hasValid, err := h.runChunked(workers, len(plan), limit, process, out)
	if err != nil {
		return total, err
	}
	if limit > 0 && total >= limit {
		return total, nil
	}
	if hasValid {
		// 坏页补漏：对坏页单页数据区扫描（顺序追加，与串/并行现状一致）
		// v0.2.14 修复：旧版补漏按整块（512 页）扫描数据区，把 Phase1 已正常导出的
		// 同块其他页行重复输出（实测 50052 行表破坏一页页头后多出 958 行）；
		// 现仅读坏页 1 页并只扫描该页数据区，补漏不重复。
		for _, pno := range badPages {
			si, localPno := locateSeg(segs, pno)
			buf := make([]byte, ps)
			n, _ := fhs[si].ReadAt(buf, int64(localPno*ps))
			data := buf[:n]
			h.scanTuplesRangeSeg(data, ps, localPno, localPno+1, segs[si].start, nExpected, colLengths, func(pageno, pos int, t *tuple.HeapTuple) bool {
				r := h.buildRow(t, pageno, pos, tm, colLengths, includeDeleted, onlyDeleted)
				if r == nil {
					return true
				}
				if ln, ok := lb(r); ok {
					if limit > 0 && total >= limit {
						return false
					}
					if out != nil {
						if err := out(ln); err != nil {
							return false
						}
					}
					total++
				}
				return true
			})
		}
		return total, nil
	}
	// Phase2：全表数据区扫描兜底（块级并行）
	process2 := func(idx int) chunkOut {
		s, e := plan[idx][0], plan[idx][1]
		si, localS := locateSeg(segs, s)
		return h.processChunkScan(fhs[si], ps, localS, localS+(e-s), segs[si].start, nExpected, colLengths, tm, includeDeleted, onlyDeleted, lb, idx)
	}
	total2, _, _, err := h.runChunked(workers, len(plan), limit, process2, out)
	if err != nil {
		return total2, err
	}
	return total2, nil
}

// StreamSQL 流式生成 INSERT（A+B：分块读 + 块级并行解析/转义 + 按序输出）。
// 每条输出行含尾部换行，与 ToSQL 经 main 写出的字节完全一致。返回产出行数。
func (h *HeapFile) StreamSQL(tm *meta.TableMeta, includeDeleted, onlyDeleted bool, limit int,
	completeInsert, replace bool, fields map[string]bool, out func(string) error) (int, error) {
	verb := "INSERT INTO"
	if replace {
		verb = "REPLACE INTO"
	}
	liveCols, outIdx, colStr, target := buildOutCols(tm, completeInsert, fields)
	lb := func(r *Row) (string, bool) {
		var sqlVals []string
		for k, i := range outIdx {
			v := r.Values[i]
			if v == NullMarker || v == "__TOAST_MISSING__" || v == "__TOAST_CORRUPT__" {
				sqlVals = append(sqlVals, "NULL")
			} else {
				sqlVals = append(sqlVals, sqlQuoteValue(v, liveCols[outIdx[k]]))
			}
		}
		stmt := verb + " " + target + " " + colStr + " VALUES (" + strings.Join(sqlVals, ", ") + ");"
		if r.Deleted {
			stmt = "-- DELETED ctid=" + r.Ctid + "\n" + stmt
		}
		return stmt + "\n", true
	}
	return h.streamRowsCore(tm, includeDeleted, onlyDeleted, limit, lb, out)
}

// StreamToData 流式生成 CSV（header 由内部先行输出；每行含尾部换行；返回行数含 header）。
func (h *HeapFile) StreamToData(tm *meta.TableMeta, includeDeleted, onlyDeleted bool, limit int,
	delimiter string, fields map[string]bool, header bool, out func(string) error) (int, error) {
	liveCols, outIdx, _, _ := buildOutCols(tm, false, fields)
	if header {
		var hdr []string
		for _, i := range outIdx {
			hdr = append(hdr, csvFieldName(liveCols[i].Name, delimiter))
		}
		if err := out(strings.Join(hdr, delimiter) + "\n"); err != nil {
			return 0, err
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
	rows := 0
	if header {
		rows++
	}
	lb := func(r *Row) (string, bool) {
		var parts []string
		for _, i := range outIdx {
			v := r.Values[i]
			if v == NullMarker || v == "__TOAST_MISSING__" || v == "__TOAST_CORRUPT__" {
				parts = append(parts, `\N`)
			} else {
				parts = append(parts, csvField(v))
			}
		}
		return strings.Join(parts, delimiter) + "\n", true
	}
	n, err := h.streamRowsCore(tm, includeDeleted, onlyDeleted, limit, lb, out)
	return rows + n, err
}

// csvFieldName CSV 字段名转义（与 csvField 同规则，用于 header）
func csvFieldName(raw, delimiter string) string {
	needsQuote := strings.Contains(raw, delimiter) || strings.Contains(raw, "\n") ||
		strings.Contains(raw, "\r") || strings.Contains(raw, "\"") || raw == `\N`
	if needsQuote {
		return "\"" + strings.ReplaceAll(raw, "\"", "\"\"") + "\""
	}
	return raw
}

// StreamRowsCount 流式统计行数（不产生输出文本），与 DumpRows 语义一致。
func (h *HeapFile) StreamRowsCount(tm *meta.TableMeta, includeDeleted, onlyDeleted bool, limit int) (int, error) {
	lb := func(r *Row) (string, bool) { return "", true }
	total, err := h.streamRowsCore(tm, includeDeleted, onlyDeleted, limit, lb, nil)
	return total, err
}

// buildOutCols 提取输出列（与 ToSQL/ToData 一致）：非 dropped 列 + fields 过滤
func buildOutCols(tm *meta.TableMeta, completeInsert bool, fields map[string]bool) ([]*meta.Column, []int, string, string) {
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
	return liveCols, outIdx, colStr, target
}
