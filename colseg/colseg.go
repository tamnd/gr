// Package colseg is gr's column-segment encoder: it serializes a contiguous run
// of one property column's cells into the COLUMN_SEGMENT byte form of doc 03 §6.2
// and reads it back. It is the bridge between the logical column (a run of
// present-or-null typed values) and the codec library ([colcodec]), and it is the
// form the checkpoint and compaction path will write and the read path will
// decode once the column store is wired to compress (doc 25 §4, doc 15).
//
// A segment is self-describing (doc 03 §6.2): its header records the element
// count, the value type, the plane codec, and flags, then the zone-map min and
// max, then the codec body, then the null bitmap. A reader decodes a segment from
// its bytes alone, so a column may hold segments encoded different ways and a
// reader always knows from each segment's header how to decode it (doc 03 §6.2).
//
// The body is a colcodec segment, which is itself self-describing: its first byte
// is the codec id and its metadata is inline (doc 15 §5). So the segment header's
// codec-metadata length is always zero here, and the header codec byte mirrors the
// body's leading id as a redundant record a reader can validate without decoding.
//
// The value plane is chosen from the value type: integers and booleans go through
// the int64 codec, floats through the float codec, strings and bytes through the
// string codec (doc 03 §15 line 444 stores both the same way, told apart by the
// value type). A column whose values do not all fit one typed plane (a composite
// type, or a heterogeneous schema-optional column, doc 02 §4.6) falls back to a
// union plane: each present value is serialized with its own type tag, the union
// representation doc 03 §6.5 names, marked by the union flag in the header.
//
// Only present values reach the body; a null position is recorded in the null
// bitmap and carries no value (doc 03 §15.4). The bitmap uses the present-bit
// convention, bit i set meaning position i is present, so an all-present segment's
// bitmap is all ones and can be omitted behind a one-byte flag (doc 03 §15.4).
package colseg

import (
	"bytes"
	"errors"
	"math"
	"strings"

	"github.com/tamnd/gr/colcodec"
	"github.com/tamnd/gr/format"
	"github.com/tamnd/gr/value"
)

// ErrBadSegment means a segment's bytes are malformed: a header cut short, a
// value type or plane out of range, a codec body that does not decode, a present
// count that disagrees with the bitmap, or a trailing byte count that is wrong.
var ErrBadSegment = errors.New("gr/colseg: malformed column segment")

// Cell is one logical column position: a value and whether it is present. An
// absent cell (Present false) is a null position in the column, distinct from a
// present cell holding an explicit null value (doc 02 §6).
type Cell struct {
	Present bool
	Value   value.Value
}

// plane is the value plane a segment body is encoded in: it selects which codec
// decodes the body and how a value is reconstructed. It is derived from the value
// type at encode and recorded so a reader dispatches without re-deriving.
type plane uint8

const (
	planeInt    plane = 0 // colcodec int64 body; integers and booleans
	planeFloat  plane = 1 // colcodec float body
	planeString plane = 2 // colcodec string body; strings and bytes
	planeUnion  plane = 3 // per-value tagged serialization; mixed or composite
)

// Header flag bits (doc 03 §6.2). Bit 1 (has-blob-references) is reserved for the
// out-of-line large-value path and is not produced yet.
const (
	flagAllPresent uint8 = 0x01 // bit 0: null bitmap omitted, every position present
	flagUnion      uint8 = 0x04 // bit 2: union plane, per-value type tags in the body
)

// Encode serializes a run of column cells into a COLUMN_SEGMENT blob. valueType is
// the column's declared type (doc 02 §4), which selects the value plane; a present
// cell whose value does not fit that plane, or a composite type, forces the union
// plane so the result is always correct regardless of the declared type.
func Encode(valueType value.Type, cells []Cell) ([]byte, error) {
	if len(cells) > math.MaxUint32 {
		return nil, ErrBadSegment
	}
	present := make([]value.Value, 0, len(cells))
	allPresent := true
	for _, c := range cells {
		if c.Present {
			present = append(present, c.Value)
		} else {
			allPresent = false
		}
	}

	pl, body := encodeBody(valueType, present)
	min, max := zoneMap(pl, present)

	var codec byte
	if pl != planeUnion && len(body) > 0 {
		codec = body[0]
	}
	flags := uint8(0)
	if allPresent {
		flags |= flagAllPresent
	}
	if pl == planeUnion {
		flags |= flagUnion
	}

	dst := make([]byte, 0, 8+len(body)+len(cells)/8+16)
	var hdr [4]byte
	format.PutU32(hdr[:], uint32(len(cells)))
	dst = append(dst, hdr[:]...)
	dst = append(dst, byte(valueType), codec, flags, 0)
	dst = format.AppendUvarint(dst, 0) // codec_meta_len: colcodec inlines its metadata
	dst = format.AppendValue(dst, min)
	dst = format.AppendValue(dst, max)
	dst = format.AppendUvarint(dst, uint64(len(body)))
	dst = append(dst, body...)
	if !allPresent {
		dst = appendBitmap(dst, cells)
	}
	return dst, nil
}

// encodeBody picks the value plane from the declared type and the present values,
// and returns the plane and the encoded body. A typed plane is used only when
// every present value matches it; otherwise the union plane keeps the segment
// correct for a heterogeneous or composite column.
func encodeBody(valueType value.Type, present []value.Value) (plane, []byte) {
	switch valueType {
	case value.TypeInt, value.TypeBool:
		if ints, ok := asInts(present); ok {
			return planeInt, colcodec.Encode(ints)
		}
	case value.TypeFloat:
		if fs, ok := asFloats(present); ok {
			return planeFloat, colcodec.EncodeFloats(fs)
		}
	case value.TypeString, value.TypeBytes:
		if ss, ok := asStrings(present); ok {
			return planeString, colcodec.EncodeStrings(ss)
		}
	}
	return planeUnion, encodeUnion(present)
}

// asInts projects present values to int64 when every one is an integer or
// boolean; a boolean encodes as 0 or 1. It fails on any other type so the caller
// falls back to the union plane.
func asInts(vals []value.Value) ([]int64, bool) {
	out := make([]int64, len(vals))
	for i, v := range vals {
		switch v.Type() {
		case value.TypeInt:
			n, _ := v.AsInt()
			out[i] = n
		case value.TypeBool:
			b, _ := v.AsBool()
			if b {
				out[i] = 1
			}
		default:
			return nil, false
		}
	}
	return out, true
}

// asFloats projects present values to float64 when every one is a float. Integers
// are not widened here: a column declared float but holding integers is rare and
// the union plane keeps it exact rather than lossily widening.
func asFloats(vals []value.Value) ([]float64, bool) {
	out := make([]float64, len(vals))
	for i, v := range vals {
		if v.Type() != value.TypeFloat {
			return nil, false
		}
		f, _ := v.AsFloat()
		out[i] = f
	}
	return out, true
}

// asStrings projects present values to strings when every one is a string or
// bytes. A bytes value carries arbitrary bytes, which a Go string holds exactly,
// so the round trip is byte-for-byte; the value type tells decode which to rebuild.
func asStrings(vals []value.Value) ([]string, bool) {
	out := make([]string, len(vals))
	for i, v := range vals {
		switch v.Type() {
		case value.TypeString:
			s, _ := v.AsString()
			out[i] = s
		case value.TypeBytes:
			b, _ := v.AsBytes()
			out[i] = string(b)
		default:
			return nil, false
		}
	}
	return out, true
}

// encodeUnion serializes each present value with its own type tag (doc 03 §6.5,
// §15.7), the representation a heterogeneous or composite column needs. The count
// is recovered from the null bitmap on decode, so no length prefix is stored.
func encodeUnion(vals []value.Value) []byte {
	var dst []byte
	for _, v := range vals {
		dst = format.AppendValue(dst, v)
	}
	return dst
}

// appendBitmap appends the null bitmap: element_count bits, bit i set meaning
// position i is present (doc 03 §15.4).
func appendBitmap(dst []byte, cells []Cell) []byte {
	n := (len(cells) + 7) / 8
	start := len(dst)
	dst = append(dst, make([]byte, n)...)
	for i, c := range cells {
		if c.Present {
			dst[start+i/8] |= 1 << uint(i%8)
		}
	}
	return dst
}

// Decode reverses Encode: it returns the value type and the run of cells. It
// validates rather than trusts every length and count, since it runs over bytes a
// corrupt file could supply, returning ErrBadSegment on any inconsistency.
func Decode(blob []byte) (value.Type, []Cell, error) {
	if len(blob) < 8 {
		return 0, nil, ErrBadSegment
	}
	count := int(format.U32(blob[0:4]))
	valueType := value.Type(blob[4])
	codec := blob[5]
	flags := blob[6]
	off := 8

	metaLen, n, err := format.Uvarint(blob[off:])
	if err != nil || metaLen != 0 {
		return 0, nil, ErrBadSegment
	}
	off += n

	_, n, err = format.DecodeValue(blob[off:]) // min zone-map value, not needed to reconstruct
	if err != nil {
		return 0, nil, ErrBadSegment
	}
	off += n
	_, n, err = format.DecodeValue(blob[off:]) // max zone-map value
	if err != nil {
		return 0, nil, ErrBadSegment
	}
	off += n

	bodyLen, n, err := format.Uvarint(blob[off:])
	if err != nil || bodyLen > uint64(len(blob)-off) {
		return 0, nil, ErrBadSegment
	}
	off += n
	body := blob[off : off+int(bodyLen)]
	off += int(bodyLen)

	allPresent := flags&flagAllPresent != 0
	presentFlags, err := readBitmap(blob[off:], count, allPresent)
	if err != nil {
		return 0, nil, err
	}
	presentCount := 0
	for _, p := range presentFlags {
		if p {
			presentCount++
		}
	}

	pl := planeForFlags(flags)
	values, err := decodeBody(pl, valueType, codec, body, presentCount)
	if err != nil {
		return 0, nil, err
	}

	cells := make([]Cell, count)
	vi := 0
	for i := range cells {
		if presentFlags[i] {
			cells[i] = Cell{Present: true, Value: values[vi]}
			vi++
		}
	}
	return valueType, cells, nil
}

// SegmentCodec names the codec a segment body is encoded with, lowercased, for the
// decode-time metric label (doc 20 §4.4). A union-plane segment serializes each value
// on its own and has no single body codec, so it reports "union". The blob is only
// peeked at the header, never fully decoded.
func SegmentCodec(blob []byte) (string, error) {
	if len(blob) < 8 {
		return "", ErrBadSegment
	}
	if blob[6]&flagUnion != 0 {
		return "union", nil
	}
	c, err := colcodec.PeekCodec(blob[5:])
	if err != nil {
		return "", err
	}
	return strings.ToLower(c.String()), nil
}

// planeForFlags recovers the value plane: the union flag forces the union plane,
// otherwise the value type selects the typed plane in decodeBody.
func planeForFlags(flags uint8) plane {
	if flags&flagUnion != 0 {
		return planeUnion
	}
	return planeInt // placeholder; decodeBody dispatches on value type for typed planes
}

// readBitmap returns the per-position present flags. An all-present segment omits
// the bitmap and every position is present; otherwise the bitmap is the exact
// number of bytes for count bits and any trailing bytes are an error.
func readBitmap(rest []byte, count int, allPresent bool) ([]bool, error) {
	flags := make([]bool, count)
	if allPresent {
		if len(rest) != 0 {
			return nil, ErrBadSegment
		}
		for i := range flags {
			flags[i] = true
		}
		return flags, nil
	}
	n := (count + 7) / 8
	if len(rest) != n {
		return nil, ErrBadSegment
	}
	for i := range flags {
		flags[i] = rest[i/8]&(1<<uint(i%8)) != 0
	}
	return flags, nil
}

// decodeBody decodes the value plane back to the present values. For a typed plane
// it dispatches on the value type and checks the header codec byte matches the
// body's self-describing id; the union plane reads per-value tagged serializations.
func decodeBody(pl plane, valueType value.Type, codec byte, body []byte, presentCount int) ([]value.Value, error) {
	if pl == planeUnion {
		return decodeUnion(body, presentCount)
	}
	if len(body) > 0 && body[0] != codec {
		return nil, ErrBadSegment
	}
	switch valueType {
	case value.TypeInt, value.TypeBool:
		ints, err := colcodec.Decode(body)
		if err != nil {
			return nil, err
		}
		return intsToValues(valueType, ints, presentCount)
	case value.TypeFloat:
		fs, err := colcodec.DecodeFloats(body)
		if err != nil {
			return nil, err
		}
		if len(fs) != presentCount {
			return nil, ErrBadSegment
		}
		out := make([]value.Value, len(fs))
		for i, f := range fs {
			out[i] = value.Float(f)
		}
		return out, nil
	case value.TypeString, value.TypeBytes:
		ss, err := colcodec.DecodeStrings(body)
		if err != nil {
			return nil, err
		}
		if len(ss) != presentCount {
			return nil, ErrBadSegment
		}
		out := make([]value.Value, len(ss))
		for i, s := range ss {
			if valueType == value.TypeBytes {
				out[i] = value.Bytes([]byte(s))
			} else {
				out[i] = value.String(s)
			}
		}
		return out, nil
	default:
		return nil, ErrBadSegment
	}
}

// intsToValues rebuilds integer or boolean values from the int64 plane, checking
// the count matches the bitmap's present count.
func intsToValues(valueType value.Type, ints []int64, presentCount int) ([]value.Value, error) {
	if len(ints) != presentCount {
		return nil, ErrBadSegment
	}
	out := make([]value.Value, len(ints))
	for i, n := range ints {
		if valueType == value.TypeBool {
			out[i] = value.Bool(n != 0)
		} else {
			out[i] = value.Int(n)
		}
	}
	return out, nil
}

// decodeUnion reads exactly presentCount tagged values from the union body and
// rejects a body that does not consume to exactly its end.
func decodeUnion(body []byte, presentCount int) ([]value.Value, error) {
	out := make([]value.Value, 0, presentCount)
	off := 0
	for range presentCount {
		v, n, err := format.DecodeValue(body[off:])
		if err != nil {
			return nil, ErrBadSegment
		}
		out = append(out, v)
		off += n
	}
	if off != len(body) {
		return nil, ErrBadSegment
	}
	return out, nil
}

// zoneMap computes the min and max of the present values within a typed plane for
// the segment's skip index (doc 03 §6.2, §15.6). A union plane mixes types whose
// order is not comparable, and an empty or all-null run has no extent, so both
// return null and a scan simply cannot skip on the zone map.
func zoneMap(pl plane, present []value.Value) (min, max value.Value) {
	if pl == planeUnion || len(present) == 0 {
		return value.Null, value.Null
	}
	switch pl {
	case planeInt:
		return intZone(present)
	case planeFloat:
		return floatZone(present)
	case planeString:
		return stringZone(present)
	default:
		return value.Null, value.Null
	}
}

func intZone(present []value.Value) (value.Value, value.Value) {
	lo, _ := present[0].AsInt()
	hi := lo
	for _, v := range present[1:] {
		n, _ := v.AsInt()
		if n < lo {
			lo = n
		}
		if n > hi {
			hi = n
		}
	}
	// A boolean run reports its extent as integers 0/1, which is fine for skipping:
	// the zone map is only consulted to rule a segment out, not to reconstruct.
	return value.Int(lo), value.Int(hi)
}

func floatZone(present []value.Value) (value.Value, value.Value) {
	// NaN is unordered, so it is skipped for the extent; a run that is all NaN has
	// no usable zone map and reports null.
	have := false
	var lo, hi float64
	for _, v := range present {
		f, _ := v.AsFloat()
		if math.IsNaN(f) {
			continue
		}
		if !have {
			lo, hi, have = f, f, true
			continue
		}
		if f < lo {
			lo = f
		}
		if f > hi {
			hi = f
		}
	}
	if !have {
		return value.Null, value.Null
	}
	return value.Float(lo), value.Float(hi)
}

func stringZone(present []value.Value) (value.Value, value.Value) {
	lo := planeBytesOf(present[0])
	hi := lo
	for _, v := range present[1:] {
		b := planeBytesOf(v)
		if bytes.Compare(b, lo) < 0 {
			lo = b
		}
		if bytes.Compare(b, hi) > 0 {
			hi = b
		}
	}
	// The zone map records the extent as a string regardless of string-vs-bytes,
	// since both order by their bytes (doc 03 §15.6); the value type on the cells
	// already tells a reader the true type.
	return value.String(string(lo)), value.String(string(hi))
}

// planeBytesOf returns the comparable bytes of a string or bytes value.
func planeBytesOf(v value.Value) []byte {
	if s, ok := v.AsString(); ok {
		return []byte(s)
	}
	b, _ := v.AsBytes()
	return b
}
