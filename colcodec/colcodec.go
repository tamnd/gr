// Package colcodec encodes and decodes a column segment of integers with the
// lightweight per-segment codecs of doc 15 (RAW, constant, run-length,
// frame-of-reference plus bit-packing, and delta), choosing one per segment from
// cheap statistics (the cascade, doc 15 §12). The byte forms are the normative
// ones of doc 03 §15.5; this package is the implementation those forms name.
//
// It is the integer column's codecs, the heart of the compression work since
// integers are where the lightweight schemes win (doc 15 §3.2). Floats default to
// RAW (doc 15 §3.3), strings to a dictionary or heap (doc 15 §3.5), and the
// block-compression second stage (doc 03 §15.5 BLOCK) are later slices; this one
// lands the integer cascade and proves it round-trips.
//
// A segment here is self-describing end to end: the first byte is the codec id and
// every count and parameter the decoder needs rides in the body, so Decode reads a
// segment from its bytes alone. In the on-disk format those counts live in the
// segment header (doc 03 §6.2) rather than the body, but a standalone library is
// cleaner and safer to test when each encoding carries its own length, so the
// checkpoint encoder that consumes this later supplies the same numbers from the
// header it already writes.
package colcodec

import (
	"errors"
	"math/bits"

	"github.com/tamnd/gr/format"
)

// Codec is a column segment's compression scheme, the u8 codec id of a segment
// header (doc 03 §6.2, §15.5). The ids are stable: they are written into segments
// and must not be renumbered.
type Codec uint8

const (
	// RAW stores each value as a fixed eight-byte little-endian word, the
	// uncompressed baseline the others beat when the data has structure (doc 15 §5).
	RAW Codec = 0
	// CONSTANT stores a single value for a segment whose positions are all equal
	// (doc 15 §6), the cheapest form and common for a flag column or an all-default
	// stretch.
	CONSTANT Codec = 1
	// RLE stores (value, run-length) pairs for a segment of long equal runs (doc 15
	// §8), which a sorted or repetitive column falls into.
	RLE Codec = 2
	// FOR is frame-of-reference plus bit-packing: a base (the segment min) and a bit
	// width, then each value's distance above the base packed at that width (doc 15
	// §9, §10), compact for clustered integers like dense ids or timestamps.
	FOR Codec = 3
	// DELTA stores the first value then signed varints of successive differences
	// (doc 03 §15.5, doc 15 §11), compact for a monotone or near-monotone sequence.
	DELTA Codec = 4
)

// ErrBadSegment means a segment's bytes are malformed: an unknown codec id, a
// truncated body, or a parameter that does not fit its declared length. Decode
// returns it rather than panicking, since it runs on the read path over bytes a
// corrupt or wrong-version file can supply.
var ErrBadSegment = errors.New("gr/colcodec: malformed column segment")

func (c Codec) String() string {
	switch c {
	case RAW:
		return "RAW"
	case CONSTANT:
		return "CONSTANT"
	case RLE:
		return "RLE"
	case FOR:
		return "FOR"
	case DELTA:
		return "DELTA"
	case DICTIONARY:
		return "DICTIONARY"
	default:
		return "UNKNOWN"
	}
}

// Encode chooses the smallest codec for vals and returns a self-describing segment
// whose first byte is the chosen codec id. An empty input encodes as an empty RAW
// segment. The choice is the cascade: it sizes each applicable codec and keeps the
// smallest, so the result is the best of the set for this exact data (doc 15 §12).
func Encode(vals []int64) []byte {
	return EncodeWith(Choose(vals), vals)
}

// Choose runs the cascade and returns the codec that encodes vals most compactly.
// It sizes a small bounded set of candidates and picks the smallest body, which is
// the measure-a-few approach doc 15 §2.1 and §12 sanction over trying every
// scheme. Ties break toward the cheaper-to-decode codec by the preference order
// CONSTANT, RLE, DELTA, FOR, RAW.
func Choose(vals []int64) Codec {
	if len(vals) == 0 {
		return RAW
	}
	best := RAW
	bestSize := len(EncodeWith(RAW, vals))
	// The preference order is also the tie-break order: an earlier candidate only
	// loses to a strictly smaller later one, so equal sizes keep the earlier pick.
	for _, c := range []Codec{DELTA, FOR, RLE, CONSTANT} {
		body, ok := encode(c, vals)
		if !ok {
			continue
		}
		if len(body) < bestSize {
			best, bestSize = c, len(body)
		}
	}
	return best
}

// EncodeWith encodes vals with a specific codec, falling back to RAW when the
// codec does not apply to this data (CONSTANT over unequal values). It always
// returns a valid self-describing segment, so a caller that forces a codec for a
// test or a policy still gets a decodable result.
func EncodeWith(c Codec, vals []int64) []byte {
	if body, ok := encode(c, vals); ok {
		return body
	}
	body, _ := encode(RAW, vals)
	return body
}

// PeekCodec returns the codec id of an encoded segment without decoding its body.
func PeekCodec(b []byte) (Codec, error) {
	if len(b) == 0 {
		return 0, ErrBadSegment
	}
	switch Codec(b[0]) {
	case RAW, CONSTANT, RLE, FOR, DELTA, DICTIONARY:
		return Codec(b[0]), nil
	default:
		return 0, ErrBadSegment
	}
}

// encode writes one codec's segment, returning ok=false when the codec cannot
// represent vals (only CONSTANT is conditional: it needs every value equal).
func encode(c Codec, vals []int64) ([]byte, bool) {
	switch c {
	case RAW:
		return encodeRaw(vals), true
	case CONSTANT:
		return encodeConstant(vals)
	case RLE:
		return encodeRLE(vals), true
	case FOR:
		return encodeFOR(vals), true
	case DELTA:
		return encodeDelta(vals), true
	default:
		return nil, false
	}
}

func encodeRaw(vals []int64) []byte {
	dst := make([]byte, 0, 1+9+len(vals)*8)
	dst = append(dst, byte(RAW))
	dst = format.AppendUvarint(dst, uint64(len(vals)))
	var buf [8]byte
	for _, v := range vals {
		format.PutU64(buf[:], uint64(v))
		dst = append(dst, buf[:]...)
	}
	return dst
}

func encodeConstant(vals []int64) ([]byte, bool) {
	for _, v := range vals {
		if v != vals[0] {
			return nil, false
		}
	}
	dst := []byte{byte(CONSTANT)}
	dst = format.AppendUvarint(dst, uint64(len(vals)))
	dst = format.AppendVarint(dst, vals[0])
	return dst, true
}

func encodeRLE(vals []int64) []byte {
	// Build (value, run-length) runs, then write the run count and each run.
	type run struct {
		v int64
		n uint64
	}
	var runs []run
	for _, v := range vals {
		if len(runs) > 0 && runs[len(runs)-1].v == v {
			runs[len(runs)-1].n++
			continue
		}
		runs = append(runs, run{v: v, n: 1})
	}
	dst := []byte{byte(RLE)}
	dst = format.AppendUvarint(dst, uint64(len(runs)))
	for _, r := range runs {
		dst = format.AppendVarint(dst, r.v)
		dst = format.AppendUvarint(dst, r.n)
	}
	return dst
}

func encodeFOR(vals []int64) []byte {
	base := vals[0]
	var maxDelta uint64
	for _, v := range vals {
		if v < base {
			base = v
		}
	}
	for _, v := range vals {
		if d := uint64(v - base); d > maxDelta {
			maxDelta = d
		}
	}
	width := bitsFor(maxDelta)
	dst := []byte{byte(FOR)}
	dst = format.AppendUvarint(dst, uint64(len(vals)))
	dst = format.AppendVarint(dst, base)
	dst = append(dst, width)
	deltas := make([]uint64, len(vals))
	for i, v := range vals {
		deltas[i] = uint64(v - base)
	}
	return bitPack(dst, deltas, width)
}

func encodeDelta(vals []int64) []byte {
	dst := []byte{byte(DELTA)}
	dst = format.AppendUvarint(dst, uint64(len(vals)))
	dst = format.AppendVarint(dst, vals[0])
	prev := vals[0]
	for _, v := range vals[1:] {
		dst = format.AppendVarint(dst, v-prev)
		prev = v
	}
	return dst
}

// Decode reverses Encode: it reads the leading codec id and decodes the body into
// the original values. It returns ErrBadSegment for any malformed input rather
// than panicking.
func Decode(b []byte) ([]int64, error) {
	if len(b) == 0 {
		return nil, ErrBadSegment
	}
	c, body := Codec(b[0]), b[1:]
	switch c {
	case RAW:
		return decodeRaw(body)
	case CONSTANT:
		return decodeConstant(body)
	case RLE:
		return decodeRLE(body)
	case FOR:
		return decodeFOR(body)
	case DELTA:
		return decodeDelta(body)
	default:
		return nil, ErrBadSegment
	}
}

func decodeRaw(b []byte) ([]int64, error) {
	n, off, err := readCount(b)
	if err != nil {
		return nil, err
	}
	if len(b)-off < n*8 {
		return nil, ErrBadSegment
	}
	out := make([]int64, n)
	for i := range n {
		out[i] = int64(format.U64(b[off : off+8]))
		off += 8
	}
	return out, nil
}

func decodeConstant(b []byte) ([]int64, error) {
	n, off, err := readCount(b)
	if err != nil {
		return nil, err
	}
	v, _, err := format.Varint(b[off:])
	if err != nil {
		return nil, ErrBadSegment
	}
	out := make([]int64, n)
	for i := range out {
		out[i] = v
	}
	return out, nil
}

func decodeRLE(b []byte) ([]int64, error) {
	runs, off, err := readCount(b)
	if err != nil {
		return nil, err
	}
	var out []int64
	for range runs {
		v, vn, err := format.Varint(b[off:])
		if err != nil {
			return nil, ErrBadSegment
		}
		off += vn
		n, nn, err := format.Uvarint(b[off:])
		if err != nil {
			return nil, ErrBadSegment
		}
		off += nn
		for range n {
			out = append(out, v)
		}
	}
	return out, nil
}

func decodeFOR(b []byte) ([]int64, error) {
	n, off, err := readCount(b)
	if err != nil {
		return nil, err
	}
	base, bn, err := format.Varint(b[off:])
	if err != nil {
		return nil, ErrBadSegment
	}
	off += bn
	if off >= len(b) {
		return nil, ErrBadSegment
	}
	width := b[off]
	off++
	deltas, err := bitUnpack(b[off:], n, width)
	if err != nil {
		return nil, err
	}
	out := make([]int64, n)
	for i, d := range deltas {
		out[i] = base + int64(d)
	}
	return out, nil
}

func decodeDelta(b []byte) ([]int64, error) {
	n, off, err := readCount(b)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, nil
	}
	first, fn, err := format.Varint(b[off:])
	if err != nil {
		return nil, ErrBadSegment
	}
	off += fn
	out := make([]int64, n)
	out[0] = first
	for i := 1; i < n; i++ {
		d, dn, err := format.Varint(b[off:])
		if err != nil {
			return nil, ErrBadSegment
		}
		off += dn
		out[i] = out[i-1] + d
	}
	return out, nil
}

// readCount reads a body's leading element/run count as an int, rejecting a value
// that would not fit so later length math cannot overflow.
func readCount(b []byte) (int, int, error) {
	n, off, err := format.Uvarint(b)
	if err != nil || n > uint64(maxInt) {
		return 0, 0, ErrBadSegment
	}
	return int(n), off, nil
}

const maxInt = int(^uint(0) >> 1)

// bitsFor is the number of bits needed to represent x, zero for x == 0 so an
// all-equal FOR segment packs at width zero (no body bytes).
func bitsFor(x uint64) uint8 {
	return uint8(bits.Len64(x))
}

// bitPack appends vals to dst packed at width bits each, LSB-first within a value
// and values laid end to end across the bit stream (doc 15 §9). Width zero appends
// nothing, since every value is the frame base.
func bitPack(dst []byte, vals []uint64, width uint8) []byte {
	if width == 0 {
		return dst
	}
	totalBits := uint64(len(vals)) * uint64(width)
	start := len(dst)
	dst = append(dst, make([]byte, (totalBits+7)/8)...)
	buf := dst[start:]
	var pos uint64
	for _, v := range vals {
		for i := range width {
			if v&(1<<i) != 0 {
				buf[pos/8] |= 1 << (pos % 8)
			}
			pos++
		}
	}
	return dst
}

// bitUnpack reads n values of width bits each from a bit stream written by
// bitPack. Width zero yields n zeros without reading any bytes.
func bitUnpack(b []byte, n int, width uint8) ([]uint64, error) {
	out := make([]uint64, n)
	if width == 0 {
		return out, nil
	}
	if uint64(len(b))*8 < uint64(n)*uint64(width) {
		return nil, ErrBadSegment
	}
	var pos uint64
	for i := range n {
		var v uint64
		for k := range width {
			if b[pos/8]&(1<<(pos%8)) != 0 {
				v |= 1 << k
			}
			pos++
		}
		out[i] = v
	}
	return out, nil
}
