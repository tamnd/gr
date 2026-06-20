package format

// Page geometry (spec 2060 doc 03 §4, §5). A page is a fixed-size byte block.
// Every page except the raw header page carries a small page header (type +
// page-LSN for idempotent redo) and ends with a checksum the pager validates.

// PageID is a zero-based page index into the main file. Page 0 is the file
// header page. PageID 0 doubles as "no page" in root pointers, which is
// unambiguous because page 0 is never a section/catalog/free root.
type PageID uint64

// NoPage is the sentinel for an absent page pointer.
const NoPage PageID = 0

// PageType tags what a page holds (doc 03 §4). The numeric values are part of
// the storage contract.
type PageType uint8

const (
	PageTypeHeader   PageType = 0  // page 0, carries the file Header
	PageTypeFree     PageType = 1  // a free-list page
	PageTypeSectDir  PageType = 2  // the section directory
	PageTypeData     PageType = 3  // generic data (M0 raw pages)
	PageTypeNode     PageType = 10 // node store (M1)
	PageTypeRel      PageType = 11 // relationship store (M1)
	PageTypeRelGroup PageType = 12 // relationship-group store (M1)
	PageTypeColumn   PageType = 13 // columnar property segment (M1)
	PageTypeDynamic  PageType = 14 // dynamic/blob store (M1)
	PageTypeIDMap    PageType = 15 // id-map (M1)
	PageTypeCatalog  PageType = 16 // catalog/token store (M1)
	PageTypeStats    PageType = 17 // statistics counts (M1)
)

// PageHeaderSize is the size of the per-page header, and ChecksumSize is the
// trailer the pager appends. Page payload is the region between them.
const (
	PageHeaderSize = 16
	ChecksumSize   = 4
)

// PageHeader is the in-page header.
type PageHeader struct {
	Type    PageType
	Flags   uint8
	PageLSN uint64 // LSN of the last WAL record applied to this page (idempotent redo)
}

// PayloadOffset is the first byte of usable payload in a page.
func PayloadOffset() int { return PageHeaderSize }

// PayloadSize is the usable payload bytes in a page of the given size.
func PayloadSize(pageSize uint32) int { return int(pageSize) - PageHeaderSize - ChecksumSize }

// WriteHeader writes the page header into the front of page p.
func WriteHeader(p []byte, h PageHeader) {
	p[0] = byte(h.Type)
	p[1] = h.Flags
	// bytes 2..7 reserved
	PutU64(p[8:], h.PageLSN)
}

// ReadHeader reads the page header from the front of page p.
func ReadHeader(p []byte) PageHeader {
	return PageHeader{
		Type:    PageType(p[0]),
		Flags:   p[1],
		PageLSN: U64(p[8:]),
	}
}

// SectionKind identifies a logical store within the file (doc 03 §5, §6). The
// section directory maps each kind to its root page and bookkeeping.
type SectionKind uint16

const (
	SectionNodes SectionKind = iota
	SectionRels
	SectionRelGroups
	SectionColumns
	SectionDynamic
	SectionIDMap
	SectionCatalog
	SectionIndexes
	SectionFreeList
	sectionCount
)

// SectionEntry is one row of the section directory: where a store's root lives
// and how big it has grown.
type SectionEntry struct {
	Kind     SectionKind
	Root     PageID
	PageSpan uint64 // number of pages the section spans (bookkeeping)
}

// SectionDirectory is the set of section entries, serialized into the section
// directory page(s). M0 supports a single-page directory (the default page size
// holds all sections comfortably); spilling to multiple pages is a later concern.
type SectionDirectory struct {
	Entries []SectionEntry
}

// Marshal serializes the directory into payload bytes.
func (d SectionDirectory) Marshal() []byte {
	var b []byte
	b = AppendUvarint(b, uint64(len(d.Entries)))
	for _, e := range d.Entries {
		b = AppendUvarint(b, uint64(e.Kind))
		b = AppendUvarint(b, uint64(e.Root))
		b = AppendUvarint(b, e.PageSpan)
	}
	return b
}

// UnmarshalSectionDirectory parses a directory from payload bytes.
func UnmarshalSectionDirectory(b []byte) (SectionDirectory, error) {
	n, off, err := Uvarint(b)
	if err != nil {
		return SectionDirectory{}, err
	}
	d := SectionDirectory{Entries: make([]SectionEntry, 0, n)}
	for i := uint64(0); i < n; i++ {
		kind, k1, err := Uvarint(b[off:])
		if err != nil {
			return SectionDirectory{}, err
		}
		off += k1
		root, k2, err := Uvarint(b[off:])
		if err != nil {
			return SectionDirectory{}, err
		}
		off += k2
		span, k3, err := Uvarint(b[off:])
		if err != nil {
			return SectionDirectory{}, err
		}
		off += k3
		d.Entries = append(d.Entries, SectionEntry{
			Kind:     SectionKind(kind),
			Root:     PageID(root),
			PageSpan: span,
		})
	}
	return d, nil
}
