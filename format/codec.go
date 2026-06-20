// Package format defines gr's on-disk file format: the primitive encodings, the
// file header, the page geometry, and the section directory (spec 2060 doc 03).
// Everything here is normative: the byte layouts are the storage contract.
//
// Endianness is little-endian throughout (doc 03 §15), chosen because gr's
// target platforms are little-endian and it lets fixed-width reads alias the
// buffer on those platforms. Variable-length integers use the standard LEB128
// unsigned varint (the same encoding encoding/binary uses), with zig-zag for
// signed values so small-magnitude negatives stay small.
package format

import (
	"encoding/binary"
	"errors"
	"math"

	"github.com/tamnd/gr/value"
)

var (
	// ErrShortBuffer means a decode ran past the end of its input.
	ErrShortBuffer = errors.New("gr/format: short buffer")
	// ErrBadVarint means a varint was malformed (overlong / overflowing).
	ErrBadVarint = errors.New("gr/format: malformed varint")
	// ErrBadValueTag means a value tag byte was not a known value.Type.
	ErrBadValueTag = errors.New("gr/format: bad value tag")
)

// Fixed-width little-endian helpers. These never allocate and panic only on a
// caller bug (buffer too small), which is a programming error, not data error.

func PutU16(b []byte, v uint16) { binary.LittleEndian.PutUint16(b, v) }
func PutU32(b []byte, v uint32) { binary.LittleEndian.PutUint32(b, v) }
func PutU64(b []byte, v uint64) { binary.LittleEndian.PutUint64(b, v) }
func U16(b []byte) uint16       { return binary.LittleEndian.Uint16(b) }
func U32(b []byte) uint32       { return binary.LittleEndian.Uint32(b) }
func U64(b []byte) uint64       { return binary.LittleEndian.Uint64(b) }

// AppendUvarint appends an unsigned LEB128 varint.
func AppendUvarint(dst []byte, v uint64) []byte {
	return binary.AppendUvarint(dst, v)
}

// AppendVarint appends a zig-zag signed varint.
func AppendVarint(dst []byte, v int64) []byte {
	return binary.AppendUvarint(dst, zigzag(v))
}

func zigzag(v int64) uint64   { return uint64((v << 1) ^ (v >> 63)) }
func unzigzag(u uint64) int64 { return int64(u>>1) ^ -int64(u&1) }

// Uvarint decodes an unsigned varint, returning the value and the number of
// bytes consumed, or an error on a malformed/short input.
func Uvarint(b []byte) (uint64, int, error) {
	v, n := binary.Uvarint(b)
	if n == 0 {
		return 0, 0, ErrShortBuffer
	}
	if n < 0 {
		return 0, 0, ErrBadVarint
	}
	return v, n, nil
}

// Varint decodes a zig-zag signed varint.
func Varint(b []byte) (int64, int, error) {
	u, n, err := Uvarint(b)
	if err != nil {
		return 0, 0, err
	}
	return unzigzag(u), n, nil
}

// AppendString appends a length-prefixed UTF-8 string.
func AppendString(dst []byte, s string) []byte {
	dst = AppendUvarint(dst, uint64(len(s)))
	return append(dst, s...)
}

// String decodes a length-prefixed string.
func String(b []byte) (string, int, error) {
	n, hn, err := Uvarint(b)
	if err != nil {
		return "", 0, err
	}
	if uint64(len(b[hn:])) < n {
		return "", 0, ErrShortBuffer
	}
	return string(b[hn : hn+int(n)]), hn + int(n), nil
}

// AppendValue serializes a property value with a leading type tag (doc 03 §15,
// doc 15 §3). This is the RAW value encoding used by the M1 storage engine;
// columnar compression (doc 15) is a later layer that wraps this.
func AppendValue(dst []byte, v value.Value) []byte {
	dst = append(dst, byte(v.Type()))
	switch v.Type() {
	case value.TypeNull:
		// tag only
	case value.TypeBool:
		bv, _ := v.AsBool()
		if bv {
			dst = append(dst, 1)
		} else {
			dst = append(dst, 0)
		}
	case value.TypeInt:
		iv, _ := v.AsInt()
		dst = AppendVarint(dst, iv)
	case value.TypeFloat:
		fv, _ := v.AsFloat()
		var buf [8]byte
		PutU64(buf[:], math.Float64bits(fv))
		dst = append(dst, buf[:]...)
	case value.TypeString:
		sv, _ := v.AsString()
		dst = AppendString(dst, sv)
	case value.TypeBytes:
		bv, _ := v.AsBytes()
		dst = AppendUvarint(dst, uint64(len(bv)))
		dst = append(dst, bv...)
	case value.TypeList:
		lv, _ := v.AsList()
		dst = AppendUvarint(dst, uint64(len(lv)))
		for _, e := range lv {
			dst = AppendValue(dst, e)
		}
	case value.TypeMap:
		mv, _ := v.AsMap()
		dst = AppendUvarint(dst, uint64(len(mv)))
		// Deterministic key order keeps the encoding stable for golden tests.
		for _, k := range sortedKeys(mv) {
			dst = AppendString(dst, k)
			dst = AppendValue(dst, mv[k])
		}
	}
	return dst
}

// DecodeValue is the inverse of AppendValue.
func DecodeValue(b []byte) (value.Value, int, error) {
	if len(b) < 1 {
		return value.Null, 0, ErrShortBuffer
	}
	t := value.Type(b[0])
	off := 1
	switch t {
	case value.TypeNull:
		return value.Null, off, nil
	case value.TypeBool:
		if len(b) < off+1 {
			return value.Null, 0, ErrShortBuffer
		}
		return value.Bool(b[off] != 0), off + 1, nil
	case value.TypeInt:
		iv, n, err := Varint(b[off:])
		if err != nil {
			return value.Null, 0, err
		}
		return value.Int(iv), off + n, nil
	case value.TypeFloat:
		if len(b) < off+8 {
			return value.Null, 0, ErrShortBuffer
		}
		f := math.Float64frombits(U64(b[off:]))
		return value.Float(f), off + 8, nil
	case value.TypeString:
		s, n, err := String(b[off:])
		if err != nil {
			return value.Null, 0, err
		}
		return value.String(s), off + n, nil
	case value.TypeBytes:
		ln, hn, err := Uvarint(b[off:])
		if err != nil {
			return value.Null, 0, err
		}
		off += hn
		if uint64(len(b[off:])) < ln {
			return value.Null, 0, ErrShortBuffer
		}
		return value.Bytes(b[off : off+int(ln)]), off + int(ln), nil
	case value.TypeList:
		ln, hn, err := Uvarint(b[off:])
		if err != nil {
			return value.Null, 0, err
		}
		off += hn
		elems := make([]value.Value, 0, ln)
		for i := uint64(0); i < ln; i++ {
			e, n, err := DecodeValue(b[off:])
			if err != nil {
				return value.Null, 0, err
			}
			elems = append(elems, e)
			off += n
		}
		return value.List(elems...), off, nil
	case value.TypeMap:
		ln, hn, err := Uvarint(b[off:])
		if err != nil {
			return value.Null, 0, err
		}
		off += hn
		m := make(map[string]value.Value, ln)
		for i := uint64(0); i < ln; i++ {
			k, n, err := String(b[off:])
			if err != nil {
				return value.Null, 0, err
			}
			off += n
			e, n2, err := DecodeValue(b[off:])
			if err != nil {
				return value.Null, 0, err
			}
			off += n2
			m[k] = e
		}
		return value.Map(m), off, nil
	default:
		return value.Null, 0, ErrBadValueTag
	}
}

func sortedKeys(m map[string]value.Value) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// insertion sort: maps are usually tiny property bags
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}
