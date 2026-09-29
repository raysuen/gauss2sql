// Package page 移植自 gauss2sql/page.py（Author: raysuen）
// openGauss 堆文件页面解析（支持任意 block size）。
package page

import "gauss2sql-go/internal/binary"

// PageSize 默认页大小
const PageSize = 8192

// KnownPageSizes 支持的页大小
var KnownPageSizes = []int{8192, 16384, 32768}

// PageHeaderSize 标准页头大小
const PageHeaderSize = 24

// ItemId flags
const (
	ItemIDUnused   = 0
	ItemIDNormal   = 1
	ItemIDRedirect = 2
	ItemIDDead     = 3
)

// PageVersion openGauss 实测 psv 低字节 = 6
const PageVersion = 6

// ItemId 4B itemid
type ItemId struct {
	Index int
	Off   int
	Flags int
	Len   int
}

// Page 一个数据页
type Page struct {
	Pageno         int
	Raw            []byte
	Lower          int
	Upper          int
	Special        int
	HeaderSize     int
	PageSize       int
	HasValidLayout bool
	Error          string
	Items          []ItemId
}

// 模块级布局缓存
var (
	cachePsvOffset int = -1
	cacheVersion    int
	cacheHeaderEnd  int
	cachePageSize   int
)

func updateLayoutCache(psvOff, version, headerEnd, pageSize int) {
	cachePsvOffset = psvOff
	cacheVersion = version
	cacheHeaderEnd = headerEnd
	cachePageSize = pageSize
}

// DetectPageSize 从页头自动探测页面大小
func DetectPageSize(raw []byte) int {
	if len(raw) < 130 {
		return 0
	}
	for off := 8; off < 128; off += 2 {
		psv := int(binary.U16(raw, off))
		version := psv & 0xFF
		size := psv & 0xFF00
		if version != PageVersion {
			continue
		}
		found := false
		for _, ks := range KnownPageSizes {
			if size == ks {
				found = true
				break
			}
		}
		if !found {
			continue
		}
		if off < 6 {
			continue
		}
		pdLower := int(binary.U16(raw, off-6))
		pdUpper := int(binary.U16(raw, off-4))
		pdSpecial := int(binary.U16(raw, off-2))
		headerEnd := (off + 2 + 3) &^ 3
		if headerEnd <= pdLower && pdLower <= pdUpper && pdUpper <= pdSpecial && pdSpecial <= size {
			return size
		}
	}
	return 0
}

// NewPage 解析一页
func NewPage(pageno int, raw []byte, pageSize int) *Page {
	p := &Page{Pageno: pageno, Raw: raw, PageSize: pageSize}
	p.parse()
	return p
}

func (p *Page) parse() {
	if p.PageSize == 0 {
		ps := DetectPageSize(p.Raw)
		if ps == 0 {
			p.PageSize = len(p.Raw)
		} else {
			p.PageSize = ps
		}
	}
	if len(p.Raw) != p.PageSize {
		p.Error = "page size mismatch"
		return
	}

	if p.tryCachedLayout() {
		return
	}
	if p.tryStandardLayout() {
		return
	}
	if p.tryAutoDetectLayout() {
		return
	}
	if p.tryLooseDetectLayout() {
		return
	}
	p.Error = "cannot identify page layout"
}

func (p *Page) tryCachedLayout() bool {
	if cachePsvOffset < 0 || cachePageSize != p.PageSize {
		return false
	}
	psv := int(binary.U16(p.Raw, cachePsvOffset))
	if (psv&0xFF) != cacheVersion || (psv&0xFF00) != p.PageSize {
		return false
	}
	pdLower := int(binary.U16(p.Raw, cachePsvOffset-6))
	pdUpper := int(binary.U16(p.Raw, cachePsvOffset-4))
	pdSpecial := int(binary.U16(p.Raw, cachePsvOffset-2))
	if !(cacheHeaderEnd <= pdLower && pdLower <= pdUpper && pdUpper <= pdSpecial && pdSpecial <= p.PageSize) {
		return false
	}
	p.HeaderSize = cacheHeaderEnd
	p.Lower, p.Upper, p.Special = pdLower, pdUpper, pdSpecial
	p.parseItems(pdLower)
	p.HasValidLayout = true
	return true
}

func (p *Page) tryStandardLayout() bool {
	psv := int(binary.U16(p.Raw, 18))
	version := psv & 0xFF
	size := psv & 0xFF00
	if version != PageVersion || size != p.PageSize {
		return false
	}
	pdLower := int(binary.U16(p.Raw, 12))
	pdUpper := int(binary.U16(p.Raw, 14))
	pdSpecial := int(binary.U16(p.Raw, 16))
	if !(24 <= pdLower && pdLower <= pdUpper && pdUpper <= pdSpecial && pdSpecial <= p.PageSize) {
		return false
	}
	p.HeaderSize = 24
	p.Lower, p.Upper, p.Special = pdLower, pdUpper, pdSpecial
	p.parseItems(pdLower)
	p.HasValidLayout = true
	updateLayoutCache(18, version, 24, p.PageSize)
	return true
}

func (p *Page) tryAutoDetectLayout() bool {
	for off := 8; off < 64; off += 2 {
		psv := int(binary.U16(p.Raw, off))
		version := psv & 0xFF
		size := psv & 0xFF00
		if version != PageVersion || size != p.PageSize {
			continue
		}
		if off < 6 {
			continue
		}
		pdLower := int(binary.U16(p.Raw, off-6))
		pdUpper := int(binary.U16(p.Raw, off-4))
		pdSpecial := int(binary.U16(p.Raw, off-2))
		headerEnd := (off + 2 + 3) &^ 3
		if !(headerEnd <= pdLower && pdLower <= pdUpper && pdUpper <= pdSpecial && pdSpecial <= p.PageSize) {
			continue
		}
		p.HeaderSize = headerEnd
		p.Lower, p.Upper, p.Special = pdLower, pdUpper, pdSpecial
		p.parseItems(pdLower)
		p.HasValidLayout = true
		updateLayoutCache(off, version, headerEnd, p.PageSize)
		return true
	}
	return false
}

func (p *Page) tryLooseDetectLayout() bool {
	for off := 8; off < 128; off += 2 {
		if off+2 > len(p.Raw) {
			break
		}
		psv := int(binary.U16(p.Raw, off))
		version := psv & 0xFF
		size := psv & 0xFF00
		if version != PageVersion || size != p.PageSize {
			continue
		}
		if off < 6 {
			continue
		}
		pdLower := int(binary.U16(p.Raw, off-6))
		pdUpper := int(binary.U16(p.Raw, off-4))
		pdSpecial := int(binary.U16(p.Raw, off-2))
		headerEnd := (off + 2 + 3) &^ 3
		if !(headerEnd <= pdLower && pdLower <= pdUpper && pdUpper <= pdSpecial && pdSpecial <= p.PageSize) {
			continue
		}
		p.HeaderSize = headerEnd
		p.Lower, p.Upper, p.Special = pdLower, pdUpper, pdSpecial
		p.parseItems(pdLower)
		p.HasValidLayout = true
		updateLayoutCache(off, version, headerEnd, p.PageSize)
		return true
	}
	return false
}

func (p *Page) parseItems(pdLower int) {
	nItems := (pdLower - p.HeaderSize) / 4
	var items []ItemId
	for i := 0; i < nItems; i++ {
		offset := p.HeaderSize + i*4
		if offset+4 > pdLower {
			break
		}
		rawID := int(binary.U16(p.Raw, offset)) | (int(binary.U16(p.Raw, offset+2)) << 16)
		it := ItemId{
			Index: i + 1,
			Off:   rawID & 0x7FFF,
			Flags: (rawID >> 15) & 0x03,
			Len:   (rawID >> 17) & 0x7FFF,
		}
		if it.Flags == ItemIDUnused {
			continue
		}
		if it.Off >= p.PageSize || it.Len > p.PageSize {
			continue
		}
		if it.Off+it.Len > p.PageSize {
			continue
		}
		items = append(items, it)
	}
	p.Items = items
}
