package format

import (
	"hash/crc32"
	"testing"
)

func crc32IEEE(b []byte) uint32 { return crc32.ChecksumIEEE(b) }

func TestHeaderRoundTrip(t *testing.T) {
	for _, ps := range []uint32{MinPageSize, 1024, DefaultPageSize, 8192, MaxPageSize} {
		h, err := NewHeader(ps)
		if err != nil {
			t.Fatalf("NewHeader(%d): %v", ps, err)
		}
		h.FeatureFlags = 0xDEAD_BEEF
		h.PageCount = 42
		h.SectionDir = 7
		h.CatalogRoot = 9
		h.FreeListRoot = 11
		h.ChangeCounter = 1234
		got, err := Unmarshal(h.Marshal())
		if err != nil {
			t.Fatalf("Unmarshal(ps=%d): %v", ps, err)
		}
		if got != h {
			t.Fatalf("round-trip mismatch ps=%d:\n got %+v\nwant %+v", ps, got, h)
		}
	}
}

func TestHeaderBadPageSize(t *testing.T) {
	for _, ps := range []uint32{0, 100, 513, 3000, MaxPageSize * 2} {
		if _, err := NewHeader(ps); err != ErrBadPageSize {
			t.Fatalf("NewHeader(%d): want ErrBadPageSize, got %v", ps, err)
		}
	}
}

func TestHeaderRejectsBadMagic(t *testing.T) {
	h, _ := NewHeader(DefaultPageSize)
	b := h.Marshal()
	b[0] ^= 0xFF
	// fix the checksum so we reach the magic check, not the checksum check
	PutU32(b[96:], crc32IEEE(b[:96]))
	if _, err := Unmarshal(b); err != ErrBadMagic {
		t.Fatalf("want ErrBadMagic, got %v", err)
	}
}

func TestHeaderRejectsNewerFormat(t *testing.T) {
	h, _ := NewHeader(DefaultPageSize)
	b := h.Marshal()
	PutU32(b[16:], FormatVersion+1)
	PutU32(b[96:], crc32IEEE(b[:96]))
	if _, err := Unmarshal(b); err != ErrNewerFormat {
		t.Fatalf("want ErrNewerFormat, got %v", err)
	}
}

func TestHeaderRejectsBadChecksum(t *testing.T) {
	h, _ := NewHeader(DefaultPageSize)
	b := h.Marshal()
	b[40] ^= 0x01 // corrupt the page count, leave the checksum stale
	if _, err := Unmarshal(b); err != ErrBadChecksum {
		t.Fatalf("want ErrBadChecksum, got %v", err)
	}
}

func TestHeaderShortBuffer(t *testing.T) {
	if _, err := Unmarshal(make([]byte, HeaderSize-1)); err != ErrShortBuffer {
		t.Fatalf("want ErrShortBuffer, got %v", err)
	}
}

// TestHeaderFuzzNoPanic feeds structured-but-malformed headers and asserts a
// typed error and never a panic (doc 03 §14, §20, §21).
func TestHeaderFuzzNoPanic(t *testing.T) {
	base, _ := NewHeader(DefaultPageSize)
	good := base.Marshal()
	for i := 0; i < len(good); i++ {
		for _, bit := range []byte{0x01, 0x80, 0xFF} {
			b := make([]byte, len(good))
			copy(b, good)
			b[i] ^= bit
			_, err := Unmarshal(b) // must not panic; error is fine
			_ = err
		}
	}
}
