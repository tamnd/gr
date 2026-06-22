package pack

import (
	"bytes"
	"math"
	"reflect"
	"strings"
	"testing"
)

// hexBytes builds a byte slice from space-separated hex pairs, the form the spec
// writes its byte traces in (doc 18 §4).
func hexBytes(t *testing.T, s string) []byte {
	t.Helper()
	fields := strings.Fields(s)
	out := make([]byte, len(fields))
	for i, f := range fields {
		var b byte
		for _, c := range f {
			b <<= 4
			switch {
			case c >= '0' && c <= '9':
				b |= byte(c - '0')
			case c >= 'A' && c <= 'F':
				b |= byte(c-'A') + 10
			case c >= 'a' && c <= 'f':
				b |= byte(c-'a') + 10
			default:
				t.Fatalf("bad hex %q", f)
			}
		}
		out[i] = b
	}
	return out
}

// TestEncodeScalarTraces checks the encoder against the exact byte traces the
// spec gives (doc 18 §4.3 through §4.8).
func TestEncodeScalarTraces(t *testing.T) {
	cases := []struct {
		name string
		v    any
		want string
	}{
		{"null", nil, "C0"},
		{"false", false, "C2"},
		{"true", true, "C3"},
		{"tiny-pos", int64(1), "01"},
		{"tiny-127", int64(127), "7F"},
		{"tiny-neg", int64(-1), "FF"},
		{"int8", int64(-20), "C8 EC"},
		{"int16", int64(200), "C9 00 C8"},
		{"int64", int64(5000000000), "CB 00 00 00 01 2A 05 F2 00"},
		{"float", 1.1, "C1 3F F1 99 99 99 99 99 9A"},
		{"empty-string", "", "80"},
		{"one-char", "A", "81 41"},
		{"hello", "hello", "85 68 65 6C 6C 6F"},
		{"list", []any{int64(1), "a", true}, "93 01 81 61 C3"},
		{"empty-map", map[string]any{}, "A0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Marshal(tc.v)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if want := hexBytes(t, tc.want); !bytes.Equal(got, want) {
				t.Errorf("got % X, want % X", got, want)
			}
		})
	}
}

// TestEncodeStructure checks a tagged structure encodes as a TINY struct: field
// count in the low nibble, signature byte, then fields (doc 18 §4.9).
func TestEncodeStructure(t *testing.T) {
	s := Structure{Tag: 0x10, Fields: []any{"RETURN 1", map[string]any{}, map[string]any{}}}
	got, err := Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := hexBytes(t, "B3 10 88 52 45 54 55 52 4E 20 31 A0 A0")
	if !bytes.Equal(got, want) {
		t.Errorf("got % X, want % X", got, want)
	}
}

// TestEncodeIntegerWidths confirms the encoder always picks the narrowest width
// (doc 18 §4.4): the preference is normative.
func TestEncodeIntegerWidths(t *testing.T) {
	cases := []struct {
		n      int64
		marker byte
		length int
	}{
		{0, 0x00, 1},
		{127, 0x7F, 1},
		{-16, 0xF0, 1},
		{-17, markerInt8, 2},
		{128, markerInt16, 3},
		{32767, markerInt16, 3},
		{32768, markerInt32, 5},
		{math.MaxInt32, markerInt32, 5},
		{int64(math.MaxInt32) + 1, markerInt64, 9},
		{math.MaxInt64, markerInt64, 9},
		{math.MinInt64, markerInt64, 9},
	}
	for _, tc := range cases {
		got, err := Marshal(tc.n)
		if err != nil {
			t.Fatalf("marshal %d: %v", tc.n, err)
		}
		if len(got) != tc.length {
			t.Errorf("n=%d length=%d, want %d (% X)", tc.n, len(got), tc.length, got)
		}
		if tc.length == 1 {
			if got[0] != tc.marker {
				t.Errorf("n=%d marker=%02X, want %02X", tc.n, got[0], tc.marker)
			}
		} else if got[0] != tc.marker {
			t.Errorf("n=%d marker=%02X, want %02X", tc.n, got[0], tc.marker)
		}
	}
}

// TestRoundTrip encodes then decodes a range of values and confirms the decoded
// value matches the canonical form.
func TestRoundTrip(t *testing.T) {
	cases := []any{
		nil,
		true,
		false,
		int64(0),
		int64(-16),
		int64(-17),
		int64(200),
		int64(-5000000000),
		int64(math.MaxInt64),
		int64(math.MinInt64),
		3.14159,
		math.Inf(1),
		math.Inf(-1),
		"",
		"hello world",
		strings.Repeat("x", 300),     // STRING_16 width
		strings.Repeat("y", 70000),   // STRING_32 width
		[]byte{1, 2, 3, 4},           // BYTES_8
		bytes.Repeat([]byte{7}, 300), // BYTES_16
		[]any{int64(1), "a", true, nil},
		map[string]any{"name": "Ada", "age": int64(42), "ok": true},
		Structure{Tag: 0x4E, Fields: []any{int64(1), []any{"Person"}, map[string]any{"name": "Ada"}}},
	}
	for i, v := range cases {
		b, err := Marshal(v)
		if err != nil {
			t.Fatalf("case %d marshal: %v", i, err)
		}
		got, err := Unmarshal(b)
		if err != nil {
			t.Fatalf("case %d unmarshal: %v", i, err)
		}
		if !reflect.DeepEqual(got, v) {
			t.Errorf("case %d: round trip = %#v, want %#v", i, got, v)
		}
	}
}

// TestDecodeAcceptsAnyWidth confirms a decoder accepts a value written in a wider
// integer form than the encoder would have chosen (doc 18 §4.4): the smallest
// width is the encoder's preference, not a decode requirement.
func TestDecodeAcceptsAnyWidth(t *testing.T) {
	// The integer 1 written as INT_64 (CB 00..01) must still decode to 1.
	v, err := Unmarshal(hexBytes(t, "CB 00 00 00 00 00 00 00 01"))
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if v != int64(1) {
		t.Errorf("decoded %v, want 1", v)
	}
}

// TestDecodeReservedStructMarkers confirms the decoder accepts the STRUCT_8 and
// STRUCT_16 markers a peer may emit, even though gr never writes them (doc 18 §4.2).
func TestDecodeReservedStructMarkers(t *testing.T) {
	// STRUCT_8: DC, count 1, signature 0x4E, one field (the integer 7).
	v, err := Unmarshal(hexBytes(t, "DC 01 4E 07"))
	if err != nil {
		t.Fatalf("struct8: %v", err)
	}
	s, ok := v.(Structure)
	if !ok || s.Tag != 0x4E || len(s.Fields) != 1 || s.Fields[0] != int64(7) {
		t.Errorf("struct8 decoded %#v", v)
	}
	// STRUCT_16: DD, count 0001, signature 0x52, one field.
	v, err = Unmarshal(hexBytes(t, "DD 00 01 52 09"))
	if err != nil {
		t.Fatalf("struct16: %v", err)
	}
	if s, ok := v.(Structure); !ok || s.Tag != 0x52 || s.Fields[0] != int64(9) {
		t.Errorf("struct16 decoded %#v", v)
	}
}

// TestDecodeErrors confirms malformed input is a loud error, not a partial decode.
func TestDecodeErrors(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"unknown-marker", "C4"},
		{"truncated-int", "C9 00"},
		{"truncated-string", "83 41 42"},
		{"truncated-float", "C1 00 00"},
		{"trailing-bytes", "C0 C0"},
		{"empty", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Unmarshal(hexBytes(t, tc.in)); err == nil {
				t.Errorf("input %q decoded without error", tc.in)
			}
		})
	}
}

// TestDecodeNonStringMapKey confirms a dictionary with a non-string key is a
// protocol error (doc 18 §4.8).
func TestDecodeNonStringMapKey(t *testing.T) {
	// MAP with one pair whose key is the integer 1 (01) instead of a string.
	if _, err := Unmarshal(hexBytes(t, "A1 01 01")); err == nil {
		t.Error("non-string key decoded without error")
	}
}

// TestEncodeOversizeStructure confirms a structure with more than 15 fields is
// rejected, since gr emits only TINY structures (doc 18 §4.9).
func TestEncodeOversizeStructure(t *testing.T) {
	fields := make([]any, 16)
	for i := range fields {
		fields[i] = int64(i)
	}
	if _, err := Marshal(Structure{Tag: 0x01, Fields: fields}); err == nil {
		t.Error("16-field structure encoded without error")
	}
}

// TestEncodeUnsupportedType confirms an unencodable Go type is a loud error.
func TestEncodeUnsupportedType(t *testing.T) {
	if _, err := Marshal(struct{ X int }{1}); err == nil {
		t.Error("unsupported type encoded without error")
	}
}

// TestEncodeUintOverflow confirms a uint64 above the signed range is rejected,
// since PackStream integers are signed 64-bit (doc 18 §4.4).
func TestEncodeUintOverflow(t *testing.T) {
	if _, err := Marshal(uint64(math.MaxUint64)); err == nil {
		t.Error("oversize uint64 encoded without error")
	}
}

// TestEncodeMapDeterministic confirms map keys are emitted in sorted order so the
// encoding is reproducible (doc 18 §4.8).
func TestEncodeMapDeterministic(t *testing.T) {
	m := map[string]any{"b": int64(2), "a": int64(1), "c": int64(3)}
	got, err := Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// A1-less: A3 then ("a",1)("b",2)("c",3) each as 81 <ch> <int>.
	want := hexBytes(t, "A3 81 61 01 81 62 02 81 63 03")
	if !bytes.Equal(got, want) {
		t.Errorf("got % X, want % X", got, want)
	}
}

// TestIntKinds confirms the encoder accepts the various Go integer kinds and they
// all reduce to the same PackStream integer.
func TestIntKinds(t *testing.T) {
	want, _ := Marshal(int64(42))
	for _, v := range []any{int(42), int8(42), int16(42), int32(42), uint8(42), uint16(42), uint32(42), uint(42), uint64(42)} {
		got, err := Marshal(v)
		if err != nil {
			t.Fatalf("marshal %T: %v", v, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%T encoded % X, want % X", v, got, want)
		}
	}
}
