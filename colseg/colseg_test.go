package colseg

import (
	"math"
	"testing"

	"github.com/tamnd/gr/format"
	"github.com/tamnd/gr/value"
)

// cellsEqual compares two cell runs by present flag and value, with value.Equal
// handling NaN and negative zero exactly so the float plane is checked precisely.
func cellsEqual(a, b []Cell) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Present != b[i].Present {
			return false
		}
		if a[i].Present && !a[i].Value.Equal(b[i].Value) {
			return false
		}
	}
	return true
}

// roundTrip encodes a run and decodes it, failing the test on any error and
// returning the decoded value type and cells.
func roundTrip(t *testing.T, vt value.Type, cells []Cell) (value.Type, []Cell) {
	t.Helper()
	blob, err := Encode(vt, cells)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	gotVT, got, err := Decode(blob)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return gotVT, got
}

func present(v value.Value) Cell { return Cell{Present: true, Value: v} }

var absent = Cell{}

// TestRoundTripTypedPlanes proves each typed plane round-trips a fully-present run
// exactly: integers, floats, strings, bytes, and booleans.
func TestRoundTripTypedPlanes(t *testing.T) {
	cases := []struct {
		name  string
		vt    value.Type
		cells []Cell
	}{
		{"ints", value.TypeInt, []Cell{present(value.Int(1)), present(value.Int(2)), present(value.Int(1000))}},
		{"floats", value.TypeFloat, []Cell{present(value.Float(0.5)), present(value.Float(-3.25)), present(value.Float(1e308))}},
		{"strings", value.TypeString, []Cell{present(value.String("Ada")), present(value.String("Lovelace")), present(value.String("Ada"))}},
		{"bytes", value.TypeBytes, []Cell{present(value.Bytes([]byte{0, 1, 2})), present(value.Bytes([]byte{0xff}))}},
		{"bools", value.TypeBool, []Cell{present(value.Bool(true)), present(value.Bool(false)), present(value.Bool(true))}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotVT, got := roundTrip(t, tc.vt, tc.cells)
			if gotVT != tc.vt {
				t.Fatalf("value type = %v, want %v", gotVT, tc.vt)
			}
			if !cellsEqual(tc.cells, got) {
				t.Fatalf("round trip changed cells: %v", got)
			}
		})
	}
}

// TestNullBitmapRoundTrip proves a run with absent positions reconstructs the
// nulls in the right places, the value plane carrying only present values.
func TestNullBitmapRoundTrip(t *testing.T) {
	cells := []Cell{
		present(value.Int(10)),
		absent,
		present(value.Int(20)),
		absent,
		absent,
		present(value.Int(30)),
	}
	_, got := roundTrip(t, value.TypeInt, cells)
	if !cellsEqual(cells, got) {
		t.Fatalf("null bitmap round trip changed cells: %v", got)
	}
}

// TestAllPresentOmitsBitmap proves a fully-present run sets the all-present flag
// and writes no bitmap, while a run with one null does write one, so the byte
// form differs and the flag is meaningful.
func TestAllPresentOmitsBitmap(t *testing.T) {
	// The two runs share the same present values, so their bodies are identical and
	// the only byte difference is the null bitmap the second run must carry.
	full := []Cell{present(value.Int(1)), present(value.Int(2))}
	withNull := []Cell{present(value.Int(1)), present(value.Int(2)), absent}

	fb, err := Encode(value.TypeInt, full)
	if err != nil {
		t.Fatal(err)
	}
	if fb[6]&flagAllPresent == 0 {
		t.Fatal("all-present flag not set on a full run")
	}
	nb, err := Encode(value.TypeInt, withNull)
	if err != nil {
		t.Fatal(err)
	}
	if nb[6]&flagAllPresent != 0 {
		t.Fatal("all-present flag set on a run with a null")
	}
	if len(nb) <= len(fb) {
		t.Fatalf("run with a null (%d bytes) not larger than full run (%d bytes), bitmap missing", len(nb), len(fb))
	}
}

// TestEmptyAndAllNull proves a zero-length run and an all-null run both round-trip,
// the degenerate cases the segment directory can hand the reader.
func TestEmptyAndAllNull(t *testing.T) {
	_, gotEmpty := roundTrip(t, value.TypeInt, []Cell{})
	if len(gotEmpty) != 0 {
		t.Fatalf("empty run decoded to %d cells", len(gotEmpty))
	}
	allNull := []Cell{absent, absent, absent}
	_, got := roundTrip(t, value.TypeString, allNull)
	if !cellsEqual(allNull, got) {
		t.Fatalf("all-null run changed: %v", got)
	}
}

// TestFloatSpecialsRoundTrip proves the float plane keeps NaN, the infinities, and
// negative zero exact through the bit-pattern codec.
func TestFloatSpecialsRoundTrip(t *testing.T) {
	cells := []Cell{
		present(value.Float(math.NaN())),
		present(value.Float(math.Inf(1))),
		present(value.Float(math.Inf(-1))),
		present(value.Float(math.Copysign(0, -1))),
		present(value.Float(0)),
	}
	_, got := roundTrip(t, value.TypeFloat, cells)
	if !cellsEqual(cells, got) {
		t.Fatalf("float specials round trip changed cells: %v", got)
	}
	// Negative zero must stay distinct from positive zero, which Equal checks by
	// bit pattern for non-NaN floats only via ==, so assert the bits directly.
	negZero, _ := got[3].Value.AsFloat()
	if math.Signbit(negZero) == false {
		t.Fatal("negative zero lost its sign through the round trip")
	}
}

// TestUnionPlaneHeterogeneous proves a column declared one type but holding mixed
// types falls back to the union plane and still round-trips every value exactly.
func TestUnionPlaneHeterogeneous(t *testing.T) {
	cells := []Cell{
		present(value.Int(7)),
		present(value.String("mixed")),
		absent,
		present(value.Bool(true)),
		present(value.Float(2.5)),
	}
	blob, err := Encode(value.TypeInt, cells) // declared int, but values are mixed
	if err != nil {
		t.Fatal(err)
	}
	if blob[6]&flagUnion == 0 {
		t.Fatal("union flag not set on a heterogeneous run")
	}
	_, got, err := Decode(blob)
	if err != nil {
		t.Fatal(err)
	}
	if !cellsEqual(cells, got) {
		t.Fatalf("union round trip changed cells: %v", got)
	}
}

// TestCompositeUnionPlane proves a composite-typed column (lists, maps) round-trips
// through the union plane, since composites have no typed value plane.
func TestCompositeUnionPlane(t *testing.T) {
	cells := []Cell{
		present(value.List(value.Int(1), value.Int(2))),
		absent,
		present(value.Map(map[string]value.Value{"k": value.String("v")})),
	}
	gotVT, got := roundTrip(t, value.TypeList, cells)
	if gotVT != value.TypeList {
		t.Fatalf("value type = %v, want LIST", gotVT)
	}
	if !cellsEqual(cells, got) {
		t.Fatalf("composite round trip changed cells: %v", got)
	}
}

// TestStoredNullDistinctFromAbsent proves a present cell holding an explicit null
// value stays present and null, distinct from an absent position.
func TestStoredNullDistinctFromAbsent(t *testing.T) {
	cells := []Cell{
		present(value.Null),
		absent,
		present(value.Int(5)),
	}
	_, got := roundTrip(t, value.TypeInt, cells) // stored null forces the union plane
	if !got[0].Present || !got[0].Value.IsNull() {
		t.Fatalf("stored null became %+v", got[0])
	}
	if got[1].Present {
		t.Fatal("absent position became present")
	}
}

// TestZoneMapRecordsExtent proves the encoded zone map carries the true min and
// max of a typed run, the skip index a predicated scan reads.
func TestZoneMapRecordsExtent(t *testing.T) {
	cells := []Cell{present(value.Int(50)), present(value.Int(-10)), present(value.Int(99))}
	blob, err := Encode(value.TypeInt, cells)
	if err != nil {
		t.Fatal(err)
	}
	min, max := readZoneMap(t, blob)
	lo, _ := min.AsInt()
	hi, _ := max.AsInt()
	if lo != -10 || hi != 99 {
		t.Fatalf("zone map = [%d, %d], want [-10, 99]", lo, hi)
	}
}

// readZoneMap pulls the two zone-map values back out of a segment header for the
// zone-map assertion, mirroring the header layout Decode walks.
func readZoneMap(t *testing.T, blob []byte) (value.Value, value.Value) {
	t.Helper()
	off := 8
	_, n, err := format.Uvarint(blob[off:])
	if err != nil {
		t.Fatal(err)
	}
	off += n
	min, n, err := format.DecodeValue(blob[off:])
	if err != nil {
		t.Fatal(err)
	}
	off += n
	max, _, err := format.DecodeValue(blob[off:])
	if err != nil {
		t.Fatal(err)
	}
	return min, max
}

// TestDecodeRejectsBadSegments proves the decoder errors rather than panicking on
// malformed bytes a corrupt file could hand it.
func TestDecodeRejectsBadSegments(t *testing.T) {
	good, err := Encode(value.TypeInt, []Cell{present(value.Int(1)), absent, present(value.Int(3))})
	if err != nil {
		t.Fatal(err)
	}
	bad := [][]byte{
		nil,                  // empty
		{0, 0, 0},            // shorter than the fixed header
		good[:len(good)-1],   // bitmap cut short
		corruptMetaLen(good), // nonzero codec_meta_len, which this version never writes
		corruptBodyLen(good), // body length past the end of the blob
		flipUnionFlag(good),  // union flag set but the body is a colcodec int segment
	}
	for i, b := range bad {
		if _, _, err := Decode(b); err == nil {
			t.Errorf("case %d: Decode of bad segment returned nil error", i)
		}
	}
}

func corruptMetaLen(good []byte) []byte {
	out := make([]byte, len(good))
	copy(out, good)
	out[8] = 1 // codec_meta_len varint first byte: claims one metadata byte
	return out
}

func corruptBodyLen(good []byte) []byte {
	out := make([]byte, len(good))
	copy(out, good)
	// Walk to the body-length varint and overwrite it with a huge value.
	off := 9 // past header and the zero codec_meta_len
	_, n, _ := format.DecodeValue(out[off:])
	off += n
	_, n, _ = format.DecodeValue(out[off:])
	off += n
	out[off] = 0x7f // a one-byte varint of 127, larger than any real body here
	return out
}

func flipUnionFlag(good []byte) []byte {
	out := make([]byte, len(good))
	copy(out, good)
	out[6] |= flagUnion
	return out
}
