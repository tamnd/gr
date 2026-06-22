// Package pack is the PackStream serializer Bolt speaks (spec 2060 doc 18 §4):
// a compact, self-describing, type-tagged binary encoding in the MessagePack
// family. Every Bolt message and every result value travels as a PackStream
// value, so this package is the byte layer the server's wire protocol (doc 18
// §5, §6) is built on.
//
// The codec is deliberately independent of gr's value.Value (doc 02 §4): it
// operates on a small set of plain Go types so it can carry both protocol
// messages (structures of maps, lists, strings, and integers) and result data
// without depending on the engine. The mapping from a Cypher value or a graph
// element to these types is a separate layer (doc 18 §6).
//
// Encoding accepts: nil (Null), bool, the signed and unsigned integer kinds
// (Integer), float64 (Float), string (String), []byte (Bytes), []any (List),
// map[string]any (Dictionary), and Structure (the tagged tuple). Decoding
// produces the canonical forms: nil, bool, int64, float64, string, []byte,
// []any, map[string]any, and Structure.
package pack

import (
	"encoding/binary"
	"fmt"
	"math"
	"sort"
)

// The marker bytes (doc 18 §4.2). A marker either fully encodes a small value
// (a tiny int, an empty list) or names a type and a size class whose size and
// payload follow.
const (
	markerNull  = 0xC0
	markerFalse = 0xC2
	markerTrue  = 0xC3

	markerInt8  = 0xC8
	markerInt16 = 0xC9
	markerInt32 = 0xCA
	markerInt64 = 0xCB

	markerFloat = 0xC1

	markerBytes8  = 0xCC
	markerBytes16 = 0xCD
	markerBytes32 = 0xCE

	markerString8  = 0xD0
	markerString16 = 0xD1
	markerString32 = 0xD2

	markerList8  = 0xD4
	markerList16 = 0xD5
	markerList32 = 0xD6

	markerMap8  = 0xD8
	markerMap16 = 0xD9
	markerMap32 = 0xDA

	markerStruct8  = 0xDC // accepted on decode for robustness; gr never emits it
	markerStruct16 = 0xDD // accepted on decode for robustness; gr never emits it

	// The TINY base markers carry a count or length in the low nibble.
	tinyStringBase = 0x80 // 0x80..0x8F: string of length 0..15
	tinyListBase   = 0x90 // 0x90..0x9F: list of count 0..15
	tinyMapBase    = 0xA0 // 0xA0..0xAF: map of pair count 0..15
	tinyStructBase = 0xB0 // 0xB0..0xBF: struct of field count 0..15
)

// Structure is a PackStream tagged tuple (doc 18 §4.9): a one-byte signature
// that names the structure type and its fields as PackStream values. It is the
// workhorse for Bolt messages (the signature is the message tag, doc 18 §5) and
// for graph and temporal values (doc 18 §6). gr emits only TINY structures, so
// a structure carries at most 15 fields.
type Structure struct {
	Tag    byte
	Fields []any
}

// Marshal encodes v as a single PackStream value and returns the bytes.
func Marshal(v any) ([]byte, error) {
	return Append(nil, v)
}

// Append encodes v as a single PackStream value and appends it to dst, returning
// the extended slice. It is the allocation-friendly form the chunked framing
// layer (doc 18 §3) builds a message body with.
func Append(dst []byte, v any) ([]byte, error) {
	switch x := v.(type) {
	case nil:
		return append(dst, markerNull), nil
	case bool:
		if x {
			return append(dst, markerTrue), nil
		}
		return append(dst, markerFalse), nil
	case int:
		return appendInt(dst, int64(x)), nil
	case int8:
		return appendInt(dst, int64(x)), nil
	case int16:
		return appendInt(dst, int64(x)), nil
	case int32:
		return appendInt(dst, int64(x)), nil
	case int64:
		return appendInt(dst, x), nil
	case uint8:
		return appendInt(dst, int64(x)), nil
	case uint16:
		return appendInt(dst, int64(x)), nil
	case uint32:
		return appendInt(dst, int64(x)), nil
	case uint:
		if uint64(x) > math.MaxInt64 {
			return nil, fmt.Errorf("pack: uint %d overflows a PackStream integer", x)
		}
		return appendInt(dst, int64(x)), nil
	case uint64:
		if x > math.MaxInt64 {
			return nil, fmt.Errorf("pack: uint64 %d overflows a PackStream integer", x)
		}
		return appendInt(dst, int64(x)), nil
	case float64:
		return appendFloat(dst, x), nil
	case string:
		return appendString(dst, x), nil
	case []byte:
		return appendBytes(dst, x), nil
	case []any:
		return appendList(dst, x)
	case map[string]any:
		return appendMap(dst, x)
	case Structure:
		return appendStruct(dst, x)
	case *Structure:
		if x == nil {
			return append(dst, markerNull), nil
		}
		return appendStruct(dst, *x)
	default:
		return nil, fmt.Errorf("pack: cannot encode %T", v)
	}
}

// appendInt encodes an integer in the smallest of the five widths that holds it
// (doc 18 §4.4): the encoder always prefers the narrowest form, while a decoder
// accepts any width.
func appendInt(dst []byte, n int64) []byte {
	switch {
	case n >= -16 && n <= 127:
		// TINY: the value is the marker itself (positive) or value+256 (negative).
		return append(dst, byte(n))
	case n >= math.MinInt8 && n <= math.MaxInt8:
		return append(dst, markerInt8, byte(n))
	case n >= math.MinInt16 && n <= math.MaxInt16:
		dst = append(dst, markerInt16)
		return binary.BigEndian.AppendUint16(dst, uint16(n))
	case n >= math.MinInt32 && n <= math.MaxInt32:
		dst = append(dst, markerInt32)
		return binary.BigEndian.AppendUint32(dst, uint32(n))
	default:
		dst = append(dst, markerInt64)
		return binary.BigEndian.AppendUint64(dst, uint64(n))
	}
}

// appendFloat encodes a double as marker C1 plus 8 big-endian IEEE-754 bytes
// (doc 18 §4.5).
func appendFloat(dst []byte, f float64) []byte {
	dst = append(dst, markerFloat)
	return binary.BigEndian.AppendUint64(dst, math.Float64bits(f))
}

// appendString encodes UTF-8 text with a TINY form for short strings (doc 18
// §4.6). The length is in bytes of the UTF-8 encoding, not code points.
func appendString(dst []byte, s string) []byte {
	n := len(s)
	switch {
	case n <= 15:
		dst = append(dst, tinyStringBase|byte(n))
	case n <= math.MaxUint8:
		dst = append(dst, markerString8, byte(n))
	case n <= math.MaxUint16:
		dst = append(dst, markerString16)
		dst = binary.BigEndian.AppendUint16(dst, uint16(n))
	default:
		dst = append(dst, markerString32)
		dst = binary.BigEndian.AppendUint32(dst, uint32(n))
	}
	return append(dst, s...)
}

// appendBytes encodes a raw byte string (doc 18 §4.6). There is no TINY bytes
// form, so the smallest encoding is the 8-bit length marker.
func appendBytes(dst []byte, b []byte) []byte {
	n := len(b)
	switch {
	case n <= math.MaxUint8:
		dst = append(dst, markerBytes8, byte(n))
	case n <= math.MaxUint16:
		dst = append(dst, markerBytes16)
		dst = binary.BigEndian.AppendUint16(dst, uint16(n))
	default:
		dst = append(dst, markerBytes32)
		dst = binary.BigEndian.AppendUint32(dst, uint32(n))
	}
	return append(dst, b...)
}

// appendList encodes a count-prefixed sequence of values (doc 18 §4.7).
func appendList(dst []byte, xs []any) ([]byte, error) {
	n := len(xs)
	switch {
	case n <= 15:
		dst = append(dst, tinyListBase|byte(n))
	case n <= math.MaxUint8:
		dst = append(dst, markerList8, byte(n))
	case n <= math.MaxUint16:
		dst = append(dst, markerList16)
		dst = binary.BigEndian.AppendUint16(dst, uint16(n))
	default:
		dst = append(dst, markerList32)
		dst = binary.BigEndian.AppendUint32(dst, uint32(n))
	}
	var err error
	for _, e := range xs {
		if dst, err = Append(dst, e); err != nil {
			return nil, err
		}
	}
	return dst, nil
}

// appendMap encodes a count-prefixed sequence of key/value pairs (doc 18 §4.8).
// Keys are emitted in sorted order so the encoding is reproducible (doc 18 §4.8).
func appendMap(dst []byte, m map[string]any) ([]byte, error) {
	n := len(m)
	switch {
	case n <= 15:
		dst = append(dst, tinyMapBase|byte(n))
	case n <= math.MaxUint8:
		dst = append(dst, markerMap8, byte(n))
	case n <= math.MaxUint16:
		dst = append(dst, markerMap16)
		dst = binary.BigEndian.AppendUint16(dst, uint16(n))
	default:
		dst = append(dst, markerMap32)
		dst = binary.BigEndian.AppendUint32(dst, uint32(n))
	}
	var err error
	for _, k := range sortedKeys(m) {
		dst = appendString(dst, k)
		if dst, err = Append(dst, m[k]); err != nil {
			return nil, err
		}
	}
	return dst, nil
}

// sortedKeys returns a map's keys in sorted order, the deterministic emission
// order (doc 18 §4.8). Key order is not semantically significant in PackStream,
// so sorting it only makes the encoding reproducible.
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// appendStruct encodes a tagged tuple as a TINY structure: the field count in
// the low nibble, the signature byte, then the fields (doc 18 §4.9). gr emits no
// structure with more than 15 fields.
func appendStruct(dst []byte, s Structure) ([]byte, error) {
	n := len(s.Fields)
	if n > 15 {
		return nil, fmt.Errorf("pack: structure has %d fields, gr emits only TINY structures (<= 15)", n)
	}
	dst = append(dst, tinyStructBase|byte(n), s.Tag)
	var err error
	for _, f := range s.Fields {
		if dst, err = Append(dst, f); err != nil {
			return nil, err
		}
	}
	return dst, nil
}
