package colcodec

import (
	"math"

	"github.com/tamnd/gr/format"
)

// This file adds the float column's codecs to the package (doc 15 §3.3). A Float is
// a 64-bit IEEE-754 double, and unlike an integer its mantissa bits are effectively
// random for measured data, so frame-of-reference and delta buy nothing: the bit
// patterns do not form a small integer range, and successive differences of the bit
// patterns are not meaningful. The float cascade therefore considers only the three
// schemes that can win on a float column: RAW (the default), CONSTANT (a column of
// one repeated value, common for a default or a uniform CRS), and a numeric
// DICTIONARY (a low-cardinality column of enumerated floats).
//
// A float is encoded as its IEEE-754 bit pattern reinterpreted as an int64, so the
// integer RAW and CONSTANT encoders carry it unchanged and the bytes round-trip
// bit-exactly, NaN payloads and negative zero included. The numeric DICTIONARY here
// mirrors the string dictionary in strings.go over int64 values, sharing the same
// codec id (5); on disk the segment header's value_type tells a Float dictionary
// from a String one, and in the library the caller picks DecodeFloats over Decode.
//
// The feature-flagged float schemes (FLOAT_SPLIT, FLOAT_XOR; doc 15 §3.3, §20.5 ids
// 0x20, 0x21) are workload-specific wins behind a flag and are not part of this
// default cascade.

// EncodeFloats chooses the smaller of RAW, CONSTANT, and the numeric dictionary for
// vals and returns a self-describing segment. An empty input encodes as empty RAW.
func EncodeFloats(vals []float64) []byte {
	return EncodeFloatsWith(ChooseFloats(vals), vals)
}

// ChooseFloats returns the codec that encodes vals most compactly, sizing RAW,
// CONSTANT (when every value is equal), and the numeric dictionary, keeping the
// smallest with RAW winning ties. FOR and DELTA are not candidates: over IEEE-754
// bit patterns their structure assumptions do not hold (doc 15 §3.3).
func ChooseFloats(vals []float64) Codec {
	if len(vals) == 0 {
		return RAW
	}
	ints := floatsToInts(vals)
	best := RAW
	bestSize := len(encodeRaw(ints))
	if body, ok := encodeConstant(ints); ok && len(body) < bestSize {
		best, bestSize = CONSTANT, len(body)
	}
	if len(encodeDictInts(ints)) < bestSize {
		best = DICTIONARY
	}
	return best
}

// EncodeFloatsWith encodes vals with a specific float codec, always returning a
// valid self-describing segment (CONSTANT over unequal values falls back to RAW).
func EncodeFloatsWith(c Codec, vals []float64) []byte {
	ints := floatsToInts(vals)
	switch c {
	case CONSTANT:
		if body, ok := encodeConstant(ints); ok {
			return body
		}
	case DICTIONARY:
		return encodeDictInts(ints)
	}
	return encodeRaw(ints)
}

// DecodeFloats reverses EncodeFloats, reading the leading codec id and reinterpreting
// the decoded int64 bit patterns back to float64. It returns ErrBadSegment on
// malformed input rather than panicking.
func DecodeFloats(b []byte) ([]float64, error) {
	if len(b) == 0 {
		return nil, ErrBadSegment
	}
	var ints []int64
	var err error
	switch Codec(b[0]) {
	case RAW:
		ints, err = decodeRaw(b[1:])
	case CONSTANT:
		ints, err = decodeConstant(b[1:])
	case DICTIONARY:
		ints, err = decodeDictInts(b[1:])
	case BLOCK:
		inner, uerr := unwrapBlock(b[1:])
		if uerr != nil {
			return nil, uerr
		}
		return DecodeFloats(inner)
	default:
		return nil, ErrBadSegment
	}
	if err != nil {
		return nil, err
	}
	return intsToFloats(ints), nil
}

// encodeDictInts writes the numeric dictionary form: the distinct values in sorted
// order, then the element count, then one bit-packed code per position indexing the
// dictionary at the width its size needs. It mirrors encodeDictStrings over int64,
// and the float cascade feeds it IEEE-754 bit patterns.
func encodeDictInts(vals []int64) []byte {
	dict, code := buildDictInts(vals)
	dst := []byte{byte(DICTIONARY)}
	dst = format.AppendUvarint(dst, uint64(len(dict)))
	for _, v := range dict {
		dst = format.AppendVarint(dst, v)
	}
	dst = format.AppendUvarint(dst, uint64(len(vals)))
	width := codeWidth(len(dict))
	codes := make([]uint64, len(vals))
	for i, v := range vals {
		codes[i] = uint64(code[v])
	}
	return bitPack(dst, codes, width)
}

// buildDictInts returns the sorted distinct values and a value->code map keyed to
// that sorted order, so a code is a stable index into the dictionary.
func buildDictInts(vals []int64) ([]int64, map[int64]int) {
	seen := map[int64]struct{}{}
	var dict []int64
	for _, v := range vals {
		if _, ok := seen[v]; !ok {
			seen[v] = struct{}{}
			dict = append(dict, v)
		}
	}
	sortInts(dict)
	code := make(map[int64]int, len(dict))
	for i, v := range dict {
		code[v] = i
	}
	return dict, code
}

func decodeDictInts(b []byte) ([]int64, error) {
	dictCount, off, err := readCount(b)
	if err != nil {
		return nil, err
	}
	dict := make([]int64, dictCount)
	for i := range dictCount {
		v, n, err := format.Varint(b[off:])
		if err != nil {
			return nil, ErrBadSegment
		}
		dict[i] = v
		off += n
	}
	n, hn, err := format.Uvarint(b[off:])
	if err != nil || n > uint64(maxInt) {
		return nil, ErrBadSegment
	}
	off += hn
	codes, err := bitUnpack(b[off:], int(n), codeWidth(dictCount))
	if err != nil {
		return nil, err
	}
	out := make([]int64, n)
	for i, c := range codes {
		if c >= uint64(dictCount) {
			return nil, ErrBadSegment
		}
		out[i] = dict[c]
	}
	return out, nil
}

// sortInts sorts in place with insertion sort, matching sortStrings: a dictionary is
// one segment's distinct set, which is small.
func sortInts(s []int64) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// floatsToInts reinterprets each float as its IEEE-754 bit pattern, so the integer
// encoders carry it and the round trip is bit-exact.
func floatsToInts(vals []float64) []int64 {
	out := make([]int64, len(vals))
	for i, v := range vals {
		out[i] = int64(math.Float64bits(v))
	}
	return out
}

func intsToFloats(vals []int64) []float64 {
	out := make([]float64, len(vals))
	for i, v := range vals {
		out[i] = math.Float64frombits(uint64(v))
	}
	return out
}
