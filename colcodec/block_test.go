package colcodec

import (
	"bytes"
	"compress/flate"
	"testing"

	"github.com/tamnd/gr/format"
)

// TestBlockWrapsAndRoundTripsIntegers proves a block-wrapped integer segment decodes
// back to the exact input through the normal Decode path, so block wrapping is
// transparent to the reader.
func TestBlockWrapsAndRoundTripsIntegers(t *testing.T) {
	vals := ramp(0, 1, 4000)
	seg := EncodeWith(RAW, vals)
	wrapped, ok := Block(seg)
	if !ok {
		t.Fatalf("Block did not wrap a compressible RAW segment of %d bytes", len(seg))
	}
	if c, _ := PeekCodec(wrapped); c != BLOCK {
		t.Fatalf("wrapped segment codec = %v, want BLOCK", c)
	}
	if len(wrapped) >= len(seg) {
		t.Fatalf("wrapped %d not smaller than raw %d", len(wrapped), len(seg))
	}
	got, err := Decode(wrapped)
	if err != nil {
		t.Fatalf("decode wrapped: %v", err)
	}
	if !equalInts(vals, got) {
		t.Fatal("block round trip changed integer data")
	}
}

// TestBlockWrapsStringsAndFloats proves the wrapper composes over the other two value
// types: a block-wrapped string heap and float column both decode transparently.
func TestBlockWrapsStringsAndFloats(t *testing.T) {
	strs := make([]string, 0, 1000)
	for i := range 1000 {
		strs = append(strs, "user-"+string(rune('a'+i%26))+"@example.com")
	}
	sseg := EncodeStringsWith(RAW, strs)
	swrapped, ok := Block(sseg)
	if !ok {
		t.Fatalf("Block did not wrap a string heap of %d bytes", len(sseg))
	}
	sgot, err := DecodeStrings(swrapped)
	if err != nil || !equalStrings(strs, sgot) {
		t.Fatalf("string block round trip: err %v", err)
	}

	floats := make([]float64, 0, 1000)
	for i := range 1000 {
		floats = append(floats, float64(i)*0.5)
	}
	fseg := EncodeFloatsWith(RAW, floats)
	fwrapped, ok := Block(fseg)
	if !ok {
		t.Fatalf("Block did not wrap a float column of %d bytes", len(fseg))
	}
	fgot, err := DecodeFloats(fwrapped)
	if err != nil || !equalFloats(floats, fgot) {
		t.Fatalf("float block round trip: err %v", err)
	}
}

// TestBlockLeavesSmallBodiesAlone proves a body below the floor, or one a general
// compressor cannot shrink, is returned unwrapped so wrapping never grows a segment.
func TestBlockLeavesSmallBodiesAlone(t *testing.T) {
	small := EncodeWith(CONSTANT, repeat(7, 64))
	if got, ok := Block(small); ok || !bytes.Equal(got, small) {
		t.Fatalf("Block wrapped a small constant segment (ok=%v)", ok)
	}

	// High-entropy data does not compress, so Block must decline rather than grow it.
	incompressible := EncodeWith(RAW, spread(4000))
	if got, ok := Block(incompressible); ok || !bytes.Equal(got, incompressible) {
		t.Fatalf("Block wrapped incompressible data (ok=%v, in %d out %d)", ok, len(incompressible), len(got))
	}
}

// TestBlockDecodeRejectsBadSegments proves the unwrap path errors rather than
// panicking on malformed block bytes a corrupt file could hand it.
func TestBlockDecodeRejectsBadSegments(t *testing.T) {
	good := EncodeWith(RAW, ramp(0, 1, 4000))
	wrapped, ok := Block(good)
	if !ok {
		t.Fatal("setup: Block did not wrap")
	}

	bad := [][]byte{
		{byte(BLOCK)},                     // no algorithm id
		{byte(BLOCK), 0x09},               // unknown algorithm id
		{byte(BLOCK), blockDeflate, 0x05}, // claims raw len, no compressed len
		append([]byte{byte(BLOCK), blockDeflate, 0xFF, 0x01}, 0x00, 0x00), // bogus deflate body
		truncateBlock(wrapped), // valid header, compressed bytes cut short
		wrapWrongRawLen(good),  // raw length does not match inflated size
		doubleWrap(wrapped),    // a block nested in a block
	}
	for i, b := range bad {
		if _, err := Decode(b); err == nil {
			t.Errorf("case %d: Decode of bad block returned nil error", i)
		}
	}
}

// truncateBlock cuts the compressed bytes of a valid block segment short, leaving the
// declared lengths intact, so unwrap finds fewer bytes than it was promised.
func truncateBlock(wrapped []byte) []byte {
	out := make([]byte, len(wrapped)-2)
	copy(out, wrapped[:len(wrapped)-2])
	return out
}

// wrapWrongRawLen builds a block whose declared uncompressed length is one byte short
// of the true inflated size, so the length check rejects it.
func wrapWrongRawLen(inner []byte) []byte {
	var buf bytes.Buffer
	w, _ := flate.NewWriter(&buf, flate.BestCompression)
	_, _ = w.Write(inner)
	_ = w.Close()
	comp := buf.Bytes()
	dst := []byte{byte(BLOCK), blockDeflate}
	dst = format.AppendUvarint(dst, uint64(len(inner)-1)) // wrong on purpose
	dst = format.AppendUvarint(dst, uint64(len(comp)))
	return append(dst, comp...)
}

// doubleWrap block-wraps an already block-wrapped segment, the nesting the encoder
// never produces and unwrap rejects to bound decode recursion.
func doubleWrap(wrapped []byte) []byte {
	return wrapBlock(wrapped)
}
