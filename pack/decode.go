package pack

import (
	"encoding/binary"
	"fmt"
	"math"
)

// Unmarshal decodes a single PackStream value from b and requires b to hold
// exactly that value: trailing bytes are an error. A Bolt message body is one
// PackStream structure with nothing after it (doc 18 §3.3), so this is the shape
// the framing layer hands to the decoder.
func Unmarshal(b []byte) (any, error) {
	d := &Decoder{buf: b}
	v, err := d.Decode()
	if err != nil {
		return nil, err
	}
	if d.pos != len(b) {
		return nil, fmt.Errorf("pack: %d trailing bytes after value", len(b)-d.pos)
	}
	return v, nil
}

// Decoder reads PackStream values from a byte slice, tracking a cursor so a
// caller can decode several values in sequence (the fields of a structure, the
// elements of a list). It does not own the slice; the caller keeps the framed
// message body alive for the decoder's lifetime.
type Decoder struct {
	buf []byte
	pos int
}

// NewDecoder returns a decoder over b.
func NewDecoder(b []byte) *Decoder { return &Decoder{buf: b} }

// Decode reads the next PackStream value. Integers decode to int64 whatever
// width they were written in, strings to string, lists to []any, dictionaries to
// map[string]any, and structures to Structure (doc 18 §4).
func (d *Decoder) Decode() (any, error) {
	m, err := d.byteAt()
	if err != nil {
		return nil, err
	}
	switch {
	case m == markerNull:
		d.pos++
		return nil, nil
	case m == markerTrue:
		d.pos++
		return true, nil
	case m == markerFalse:
		d.pos++
		return false, nil
	case m <= 0x7F:
		// TINY positive integer: the marker is the value.
		d.pos++
		return int64(m), nil
	case m >= 0xF0:
		// TINY negative integer: the value is marker-256.
		d.pos++
		return int64(int8(m)), nil
	case m >= tinyStringBase && m <= tinyStringBase|0x0F:
		d.pos++
		return d.str(int(m & 0x0F))
	case m >= tinyListBase && m <= tinyListBase|0x0F:
		d.pos++
		return d.list(int(m & 0x0F))
	case m >= tinyMapBase && m <= tinyMapBase|0x0F:
		d.pos++
		return d.dict(int(m & 0x0F))
	case m >= tinyStructBase && m <= tinyStructBase|0x0F:
		d.pos++
		return d.structure(int(m & 0x0F))
	}
	switch m {
	case markerInt8:
		d.pos++
		return d.int(1)
	case markerInt16:
		d.pos++
		return d.int(2)
	case markerInt32:
		d.pos++
		return d.int(4)
	case markerInt64:
		d.pos++
		return d.int(8)
	case markerFloat:
		d.pos++
		return d.float()
	case markerBytes8:
		d.pos++
		return d.bytes(1)
	case markerBytes16:
		d.pos++
		return d.bytes(2)
	case markerBytes32:
		d.pos++
		return d.bytes(4)
	case markerString8:
		d.pos++
		return d.strLen(1)
	case markerString16:
		d.pos++
		return d.strLen(2)
	case markerString32:
		d.pos++
		return d.strLen(4)
	case markerList8:
		d.pos++
		return d.listLen(1)
	case markerList16:
		d.pos++
		return d.listLen(2)
	case markerList32:
		d.pos++
		return d.listLen(4)
	case markerMap8:
		d.pos++
		return d.dictLen(1)
	case markerMap16:
		d.pos++
		return d.dictLen(2)
	case markerMap32:
		d.pos++
		return d.dictLen(4)
	case markerStruct8:
		// gr never emits these, but a peer may; accept them for robustness (doc 18 §4.2).
		d.pos++
		n, err := d.uint(1)
		if err != nil {
			return nil, err
		}
		return d.structure(int(n))
	case markerStruct16:
		d.pos++
		n, err := d.uint(2)
		if err != nil {
			return nil, err
		}
		return d.structure(int(n))
	}
	return nil, fmt.Errorf("pack: unknown marker 0x%02X at offset %d", m, d.pos)
}

// byteAt returns the marker byte at the cursor without consuming it.
func (d *Decoder) byteAt() (byte, error) {
	if d.pos >= len(d.buf) {
		return 0, fmt.Errorf("pack: unexpected end of input at offset %d", d.pos)
	}
	return d.buf[d.pos], nil
}

// take consumes n bytes from the cursor, erroring if fewer remain.
func (d *Decoder) take(n int) ([]byte, error) {
	if n < 0 || d.pos+n > len(d.buf) {
		return nil, fmt.Errorf("pack: want %d bytes at offset %d, have %d", n, d.pos, len(d.buf)-d.pos)
	}
	b := d.buf[d.pos : d.pos+n]
	d.pos += n
	return b, nil
}

// int reads a width-byte signed big-endian integer (doc 18 §4.4).
func (d *Decoder) int(width int) (int64, error) {
	b, err := d.take(width)
	if err != nil {
		return 0, err
	}
	switch width {
	case 1:
		return int64(int8(b[0])), nil
	case 2:
		return int64(int16(binary.BigEndian.Uint16(b))), nil
	case 4:
		return int64(int32(binary.BigEndian.Uint32(b))), nil
	default:
		return int64(binary.BigEndian.Uint64(b)), nil
	}
}

// uint reads a width-byte unsigned big-endian length or count.
func (d *Decoder) uint(width int) (uint64, error) {
	b, err := d.take(width)
	if err != nil {
		return 0, err
	}
	switch width {
	case 1:
		return uint64(b[0]), nil
	case 2:
		return uint64(binary.BigEndian.Uint16(b)), nil
	default:
		return uint64(binary.BigEndian.Uint32(b)), nil
	}
}

// float reads an 8-byte big-endian IEEE-754 double (doc 18 §4.5).
func (d *Decoder) float() (float64, error) {
	b, err := d.take(8)
	if err != nil {
		return 0, err
	}
	return math.Float64frombits(binary.BigEndian.Uint64(b)), nil
}

// bytes reads a width-byte length then that many raw bytes (doc 18 §4.6). The
// result is a copy, so it does not alias the decoder's buffer.
func (d *Decoder) bytes(width int) ([]byte, error) {
	n, err := d.uint(width)
	if err != nil {
		return nil, err
	}
	b, err := d.take(int(n))
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out, nil
}

// strLen reads a width-byte length then that many UTF-8 bytes (doc 18 §4.6).
func (d *Decoder) strLen(width int) (string, error) {
	n, err := d.uint(width)
	if err != nil {
		return "", err
	}
	return d.str(int(n))
}

// str reads n UTF-8 bytes as a string.
func (d *Decoder) str(n int) (string, error) {
	b, err := d.take(n)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// listLen reads a width-byte count then that many values (doc 18 §4.7).
func (d *Decoder) listLen(width int) ([]any, error) {
	n, err := d.uint(width)
	if err != nil {
		return nil, err
	}
	return d.list(int(n))
}

// list reads n values into a slice.
func (d *Decoder) list(n int) ([]any, error) {
	xs := make([]any, n)
	for i := range xs {
		v, err := d.Decode()
		if err != nil {
			return nil, err
		}
		xs[i] = v
	}
	return xs, nil
}

// dictLen reads a width-byte pair count then that many pairs (doc 18 §4.8).
func (d *Decoder) dictLen(width int) (map[string]any, error) {
	n, err := d.uint(width)
	if err != nil {
		return nil, err
	}
	return d.dict(int(n))
}

// dict reads n key/value pairs into a map. Each key is a String value (doc 18
// §4.8); a non-string key is a protocol error.
func (d *Decoder) dict(n int) (map[string]any, error) {
	m := make(map[string]any, n)
	for i := 0; i < n; i++ {
		k, err := d.Decode()
		if err != nil {
			return nil, err
		}
		ks, ok := k.(string)
		if !ok {
			return nil, fmt.Errorf("pack: dictionary key %d is %T, want string", i, k)
		}
		v, err := d.Decode()
		if err != nil {
			return nil, err
		}
		m[ks] = v
	}
	return m, nil
}

// structure reads the signature byte then n field values (doc 18 §4.9).
func (d *Decoder) structure(n int) (Structure, error) {
	b, err := d.take(1)
	if err != nil {
		return Structure{}, err
	}
	s := Structure{Tag: b[0], Fields: make([]any, n)}
	for i := range s.Fields {
		v, err := d.Decode()
		if err != nil {
			return Structure{}, err
		}
		s.Fields[i] = v
	}
	return s, nil
}
