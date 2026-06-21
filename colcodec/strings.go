package colcodec

import "github.com/tamnd/gr/format"

// This file adds the string column's codecs to the package: the RAW heap form and
// the dictionary form (doc 15 §3.5, §7; doc 03 §15.5 DICTIONARY). Strings are the
// other common property type after integers, and a low-cardinality string column (a
// status, a country, a type tag) is exactly what a dictionary compresses, turning
// each repeated value into a small code.
//
// The string codecs reuse the same id space and the same self-describing-segment
// convention as the integer codecs: the first byte is the codec id, the body
// carries every count and parameter, and EncodeStrings / DecodeStrings are the
// string-typed entry points that mirror Encode / Decode. The on-disk format pairs a
// codec id with a value_type in the segment header (doc 03 §6.2), so the same
// DICTIONARY id over a String segment and over an Integer segment are distinguished
// by the type, which here is implicit in which decode function the caller uses.

// DICTIONARY stores a sorted dictionary of the distinct values followed by a
// bit-packed code per position (doc 03 §15.5, doc 15 §7), compact for a
// low-cardinality column. The sorted dictionary also lets a range predicate compare
// codes by the dictionary's order, though this slice only encodes and decodes.
const DICTIONARY Codec = 5

// EncodeStrings chooses the smaller of the RAW heap and the dictionary form for
// vals and returns a self-describing segment. An empty input encodes as an empty
// RAW heap.
func EncodeStrings(vals []string) []byte {
	return EncodeStringsWith(ChooseStrings(vals), vals)
}

// ChooseStrings returns the codec that encodes vals more compactly, sizing the two
// candidate forms and keeping the smaller, with RAW winning ties. A column of all
// distinct values stays RAW (the dictionary would just duplicate the heap and add
// codes); a column that repeats a small set of values picks DICTIONARY.
func ChooseStrings(vals []string) Codec {
	if len(vals) == 0 {
		return RAW
	}
	if len(encodeDictStrings(vals)) < len(encodeRawStrings(vals)) {
		return DICTIONARY
	}
	return RAW
}

// EncodeStringsWith encodes vals with a specific string codec, always returning a
// valid self-describing segment.
func EncodeStringsWith(c Codec, vals []string) []byte {
	if c == DICTIONARY {
		return encodeDictStrings(vals)
	}
	return encodeRawStrings(vals)
}

// encodeRawStrings writes the heap form: the element count, a length per position,
// then the concatenated bytes. The per-position lengths are the offset array doc 03
// §15.5 names, stored as lengths so a position is found by summing rather than by
// scanning the heap.
func encodeRawStrings(vals []string) []byte {
	dst := []byte{byte(RAW)}
	dst = format.AppendUvarint(dst, uint64(len(vals)))
	for _, s := range vals {
		dst = format.AppendUvarint(dst, uint64(len(s)))
	}
	for _, s := range vals {
		dst = append(dst, s...)
	}
	return dst
}

// encodeDictStrings writes the dictionary form: the distinct values in sorted
// order, then the element count, then one bit-packed code per position indexing the
// dictionary at the width its size needs.
func encodeDictStrings(vals []string) []byte {
	dict, code := buildDict(vals)
	dst := []byte{byte(DICTIONARY)}
	dst = format.AppendUvarint(dst, uint64(len(dict)))
	for _, s := range dict {
		dst = format.AppendString(dst, s)
	}
	dst = format.AppendUvarint(dst, uint64(len(vals)))
	width := codeWidth(len(dict))
	codes := make([]uint64, len(vals))
	for i, s := range vals {
		codes[i] = uint64(code[s])
	}
	return bitPack(dst, codes, width)
}

// buildDict returns the sorted distinct values and a value->code map keyed to that
// sorted order, so a code is a stable index into the dictionary.
func buildDict(vals []string) ([]string, map[string]int) {
	seen := map[string]struct{}{}
	var dict []string
	for _, s := range vals {
		if _, ok := seen[s]; !ok {
			seen[s] = struct{}{}
			dict = append(dict, s)
		}
	}
	sortStrings(dict)
	code := make(map[string]int, len(dict))
	for i, s := range dict {
		code[s] = i
	}
	return dict, code
}

// DecodeStrings reverses EncodeStrings, reading the leading codec id and decoding
// the string body. It returns ErrBadSegment on malformed input rather than
// panicking.
func DecodeStrings(b []byte) ([]string, error) {
	if len(b) == 0 {
		return nil, ErrBadSegment
	}
	c, body := Codec(b[0]), b[1:]
	switch c {
	case RAW:
		return decodeRawStrings(body)
	case DICTIONARY:
		return decodeDictStrings(body)
	case BLOCK:
		inner, err := unwrapBlock(body)
		if err != nil {
			return nil, err
		}
		return DecodeStrings(inner)
	default:
		return nil, ErrBadSegment
	}
}

func decodeRawStrings(b []byte) ([]string, error) {
	n, off, err := readCount(b)
	if err != nil {
		return nil, err
	}
	lens := make([]int, n)
	for i := range n {
		ln, hn, err := format.Uvarint(b[off:])
		if err != nil || ln > uint64(maxInt) {
			return nil, ErrBadSegment
		}
		lens[i] = int(ln)
		off += hn
	}
	out := make([]string, n)
	for i, ln := range lens {
		if len(b)-off < ln {
			return nil, ErrBadSegment
		}
		out[i] = string(b[off : off+ln])
		off += ln
	}
	return out, nil
}

func decodeDictStrings(b []byte) ([]string, error) {
	dictCount, off, err := readCount(b)
	if err != nil {
		return nil, err
	}
	dict := make([]string, dictCount)
	for i := range dictCount {
		s, n, err := format.String(b[off:])
		if err != nil {
			return nil, ErrBadSegment
		}
		dict[i] = s
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
	out := make([]string, n)
	for i, c := range codes {
		if c >= uint64(dictCount) {
			return nil, ErrBadSegment
		}
		out[i] = dict[c]
	}
	return out, nil
}

// codeWidth is the bit width a dictionary of count entries needs per code: the bits
// to hold the largest index count-1, and zero for a dictionary of zero or one entry
// (every code is then index zero, packed in no bits).
func codeWidth(count int) uint8 {
	if count <= 1 {
		return 0
	}
	return bitsFor(uint64(count - 1))
}

// sortStrings sorts in place with insertion sort; a dictionary is the distinct set
// of one segment, which is small, so the simple sort keeps the dependency surface
// minimal and matches how the catalog sorts its small key sets.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
