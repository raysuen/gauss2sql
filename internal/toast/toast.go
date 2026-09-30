// Package toast 移植自 gauss2sql/toast.py（Author: raysuen）
// TOAST 表解析与重组。
package toast

import (
	"os"
	"sort"

	"gauss2sql-go/internal/binary"
	"gauss2sql-go/internal/page"
	"gauss2sql-go/internal/tuple"
)

var toastColLayout = []heapCol{
	{4, false}, {4, false}, {0, true},
}

type heapCol struct {
	attlen    int
	isVarlena bool
}

// ToastFile TOAST 表文件
type ToastFile struct {
	path     string
	pageSize int
	posIdx   map[int][]posEntry // valueid -> [(seq, pageno, offset)]
	data     []byte             // 整文件读缓存（BuildIndex 时加载，Resolve 只读复用）
}

type posEntry struct {
	seq    int
	pageno int
	off    int
}

// New 打开
func New(path string, pageSize int) *ToastFile {
	return &ToastFile{path: path, pageSize: pageSize}
}

// BuildIndex 建立位置索引
func (t *ToastFile) BuildIndex() {
	t.posIdx = map[int][]posEntry{}
	data, err := os.ReadFile(t.path)
	if err != nil {
		return
	}
	ps := t.pageSize
	if ps == 0 {
		ps = page.DetectPageSize(data[:min(130, len(data))])
		if ps == 0 {
			ps = page.PageSize
		}
	}
	t.pageSize = ps
	t.data = data
	npages := len(data) / ps
	for pno := 0; pno < npages; pno++ {
		raw := data[pno*ps : (pno+1)*ps]
		if allZero(raw[:64]) {
			continue
		}
		for _, off := range extractChunks(raw, pno, ps) {
			cid, seq := off.chunkID, off.chunkSeq
			t.posIdx[cid] = append(t.posIdx[cid], posEntry{seq, pno, off.off})
		}
	}
	for vid := range t.posIdx {
		sort.Slice(t.posIdx[vid], func(i, j int) bool {
			return t.posIdx[vid][i].seq < t.posIdx[vid][j].seq
		})
	}
}

type chunkLoc struct {
	off      int
	chunkID  int
	chunkSeq int
	payload  []byte
}

func extractChunks(raw []byte, pageno, ps int) []chunkLoc {
	pg := page.NewPage(pageno, raw, ps)
	var out []chunkLoc
	if pg.HasValidLayout {
		for _, it := range pg.Items {
			if it.Flags != page.ItemIDNormal {
				continue
			}
			data := raw[it.Off : it.Off+it.Len]
			t, err := tuple.New(data)
			if err != nil {
				continue
			}
			if t.Nattrs != 3 {
				continue
			}
			if cid, seq, payload := fieldsToChunk(t); cid > 0 {
				out = append(out, chunkLoc{it.Off, cid, seq, payload})
			}
		}
	}
	return out
}

func fieldsToChunk(t *tuple.HeapTuple) (cid, seq int, payload []byte) {
	nulls := t.GetNulls()
	layout := []struct{ attlen int; isVar bool }{
		{4, false}, {4, false}, {0, true},
	}
	fields := extractFields(t.Raw, int(t.HOff), nulls, layout)
	if len(fields) < 3 || fields[0] == nil || fields[1] == nil {
		return 0, 0, nil
	}
	if len(fields[0]) < 4 || len(fields[1]) < 4 {
		return 0, 0, nil
	}
	cid = int(binary.U32(fields[0], 0))
	seq = int(binary.I32(fields[1], 0))
	if !(1 <= cid && cid <= 100000000) || !(0 <= seq && seq <= 1000000) {
		return 0, 0, nil
	}
	payload = varlenaPayload(fields[2])
	if len(payload) == 0 {
		return 0, 0, nil
	}
	return cid, seq, payload
}

func extractFields(raw []byte, tHoff int, nulls []bool, layout []struct{ attlen int; isVar bool }) [][]byte {
	pos := 0
	var fields [][]byte
	for i, item := range layout {
		if i < len(nulls) && nulls[i] {
			fields = append(fields, nil)
			continue
		}
		if item.isVar {
			off := tHoff + pos
			if off >= len(raw) {
				fields = append(fields, nil)
				break
			}
			kind, total, _, _ := binary.VarlenaParse(raw, off)
			if kind == "" {
				fields = append(fields, nil)
				break
			}
			fields = append(fields, append([]byte(nil), raw[off:off+total]...))
			pos += total
		} else {
			fields = append(fields, append([]byte(nil), raw[tHoff+pos:tHoff+pos+item.attlen]...))
			pos += item.attlen
		}
	}
	return fields
}

func varlenaPayload(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	kind, total, _, _ := binary.VarlenaParse(b, 0)
	switch kind {
	case binary.VARLENA1B:
		end := 1 + total - 1
		if end > len(b) {
			end = len(b)
		}
		return b[1:end]
	case binary.VARLENA4B, binary.VARLENA4BComp:
		end := 4 + total - 4
		if end > len(b) {
			end = len(b)
		}
		return b[4:end]
	}
	return nil
}

// Resolve 重组一个 valueid 的所有 chunk
func (t *ToastFile) Resolve(valueid int) []byte {
	entries, ok := t.posIdx[valueid]
	if !ok {
		return nil
	}
	data := t.data
	if data == nil {
		// 兜底：BuildIndex 未缓存时再读一次（不应发生）
		var err error
		data, err = os.ReadFile(t.path)
		if err != nil {
			return nil
		}
	}
	var buf []byte
	for _, e := range entries {
		raw := data[e.pageno*t.pageSize : (e.pageno+1)*t.pageSize]
		chunks := extractChunks(raw, e.pageno, t.pageSize)
		found := false
		for _, c := range chunks {
			if c.off == e.off {
				buf = append(buf, c.payload...)
				found = true
				break
			}
		}
		if !found {
			return nil
		}
	}
	return buf
}

func allZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
