package format

import (
	"errors"
	"hash/crc32"
)

// Magic identifies a gr database file. It is 16 bytes so the file is recognized
// by `file(1)`-style sniffing and so a non-gr file is rejected immediately.
var Magic = [16]byte{'g', 'r', ' ', 'g', 'r', 'a', 'p', 'h', ' ', 'd', 'b', 0, 0, 0, 0, 1}

// FormatVersion is the on-disk format version. A file with a newer major version
// than the binary understands is rejected cleanly (doc 03 §9, §20).
const FormatVersion uint32 = 1

// Page size bounds. Pages are a power of two between these limits; the default
// is 4096 (doc 03 §4). The size is fixed at file creation.
const (
	MinPageSize     = 512
	MaxPageSize     = 65536
	DefaultPageSize = 4096
)

// HeaderSize is the fixed size of the file header, which lives at the start of
// page 0. The remainder of page 0 is reserved.
const HeaderSize = 100

// Header is the file header: the single source of truth for the file's geometry
// (doc 03 §3). It round-trips exactly through Marshal/Unmarshal.
type Header struct {
	Magic         [16]byte
	FormatVersion uint32 // format version of the file
	PageSize      uint32 // bytes per page, power of two in [Min,Max]
	FeatureFlags  uint64 // optional features in use (bitset)
	Encryption    uint8  // 0 = none (encryption is post-1.0)
	PageCount     uint64 // number of pages currently allocated in the main file
	SectionDir    uint64 // page id of the section directory root (0 = none yet)
	CatalogRoot   uint64 // page id of the catalog root (0 = none yet)
	FreeListRoot  uint64 // page id of the free-list root (0 = none yet)
	ChangeCounter uint64 // bumped on every committed change; cheap staleness check
}

var (
	// ErrBadMagic means the file is not a gr database.
	ErrBadMagic = errors.New("gr/format: bad magic (not a gr database)")
	// ErrNewerFormat means the file's format version is newer than supported.
	ErrNewerFormat = errors.New("gr/format: file format is newer than this build supports")
	// ErrBadPageSize means the header's page size is out of range or not a power of two.
	ErrBadPageSize = errors.New("gr/format: invalid page size")
	// ErrBadChecksum means the header checksum did not validate.
	ErrBadChecksum = errors.New("gr/format: header checksum mismatch")
)

// NewHeader returns a header for a freshly created database with the given page
// size. PageCount starts at 1 (page 0, the header page itself).
func NewHeader(pageSize uint32) (Header, error) {
	if !validPageSize(pageSize) {
		return Header{}, ErrBadPageSize
	}
	return Header{
		Magic:         Magic,
		FormatVersion: FormatVersion,
		PageSize:      pageSize,
		PageCount:     1,
	}, nil
}

func validPageSize(s uint32) bool {
	if s < MinPageSize || s > MaxPageSize {
		return false
	}
	return s&(s-1) == 0 // power of two
}

// Marshal writes the header into a HeaderSize-byte slice. The layout is fixed
// (little-endian) and ends with a CRC32 over the preceding bytes.
func (h Header) Marshal() []byte {
	b := make([]byte, HeaderSize)
	copy(b[0:16], h.Magic[:])
	PutU32(b[16:], h.FormatVersion)
	PutU32(b[20:], h.PageSize)
	PutU64(b[24:], h.FeatureFlags)
	b[32] = h.Encryption
	// bytes 33..39 reserved (zero)
	PutU64(b[40:], h.PageCount)
	PutU64(b[48:], h.SectionDir)
	PutU64(b[56:], h.CatalogRoot)
	PutU64(b[64:], h.FreeListRoot)
	PutU64(b[72:], h.ChangeCounter)
	// bytes 80..95 reserved (zero)
	PutU32(b[96:], crc32.ChecksumIEEE(b[:96]))
	return b
}

// Unmarshal parses and validates a header from b (which must be at least
// HeaderSize bytes). It rejects bad magic, a too-new format, a bad page size,
// and a checksum mismatch, all as typed errors and never a panic (doc 03 §14,
// §20, §21).
func Unmarshal(b []byte) (Header, error) {
	if len(b) < HeaderSize {
		return Header{}, ErrShortBuffer
	}
	want := crc32.ChecksumIEEE(b[:96])
	if U32(b[96:]) != want {
		return Header{}, ErrBadChecksum
	}
	var h Header
	copy(h.Magic[:], b[0:16])
	if h.Magic != Magic {
		return Header{}, ErrBadMagic
	}
	h.FormatVersion = U32(b[16:])
	if h.FormatVersion > FormatVersion {
		return Header{}, ErrNewerFormat
	}
	h.PageSize = U32(b[20:])
	if !validPageSize(h.PageSize) {
		return Header{}, ErrBadPageSize
	}
	h.FeatureFlags = U64(b[24:])
	h.Encryption = b[32]
	h.PageCount = U64(b[40:])
	h.SectionDir = U64(b[48:])
	h.CatalogRoot = U64(b[56:])
	h.FreeListRoot = U64(b[64:])
	h.ChangeCounter = U64(b[72:])
	return h, nil
}
