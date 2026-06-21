package colcodec

import (
	"math"
	"testing"
)

func roundTripFloats(t *testing.T, name string, vals []float64) {
	t.Helper()
	seg := EncodeFloats(vals)
	got, err := DecodeFloats(seg)
	if err != nil {
		t.Fatalf("%s: decode: %v", name, err)
	}
	if !equalFloats(vals, got) {
		t.Fatalf("%s: round trip changed data\n in: %v\nout: %v", name, vals, got)
	}
}

// equalFloats compares by bit pattern so NaN (which is never == itself) and negative
// zero are required to round-trip exactly, the property a column store needs.
func equalFloats(a, b []float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if math.Float64bits(a[i]) != math.Float64bits(b[i]) {
			return false
		}
	}
	return true
}

func TestFloatRoundTripShapes(t *testing.T) {
	cases := map[string][]float64{
		"empty":         {},
		"single":        {3.14},
		"all-equal":     {2.5, 2.5, 2.5, 2.5},
		"low-card":      {1.5, 2.5, 1.5, 2.5, 1.5, 2.5, 2.5},
		"all-distinct":  {1.1, 2.2, 3.3, 4.4, 5.5},
		"negatives":     {-1.5, -2.5, 0, 2.5, 1.5},
		"neg-zero":      {0, math.Copysign(0, -1), 0},
		"specials":      {math.NaN(), math.Inf(1), math.Inf(-1), math.MaxFloat64, math.SmallestNonzeroFloat64},
		"coordinates":   {37.7749, -122.4194, 37.7750, -122.4195},
		"constant-zero": {0, 0, 0, 0, 0},
	}
	for name, vals := range cases {
		roundTripFloats(t, name, vals)
	}
}

// TestFloatEncodeWithRoundTrips forces each float codec and proves it round-trips
// independent of what the cascade would have chosen.
func TestFloatEncodeWithRoundTrips(t *testing.T) {
	vals := []float64{1.5, 2.5, 1.5, 2.5}
	for _, c := range []Codec{RAW, CONSTANT, DICTIONARY} {
		seg := EncodeFloatsWith(c, vals)
		out, err := DecodeFloats(seg)
		if err != nil {
			t.Fatalf("codec %v: decode %v", c, err)
		}
		// CONSTANT cannot represent unequal values, so it falls back to RAW; the
		// other two carry the value exactly.
		if !equalFloats(vals, out) {
			t.Fatalf("codec %v changed data: %v", c, out)
		}
	}
}

// TestFloatCascade pins the choice: a constant column picks CONSTANT, a
// low-cardinality column picks the dictionary, a column of measured distinct values
// stays RAW, and none loses to RAW.
func TestFloatCascade(t *testing.T) {
	constant := make([]float64, 256)
	for i := range constant {
		constant[i] = 9.5
	}
	if got := ChooseFloats(constant); got != CONSTANT {
		t.Errorf("constant column chose %v, want CONSTANT", got)
	}

	lowCard := make([]float64, 0, 300)
	for i := range 300 {
		lowCard = append(lowCard, []float64{0.25, 0.5, 0.75}[i%3])
	}
	if got := ChooseFloats(lowCard); got != DICTIONARY {
		t.Errorf("low-cardinality column chose %v, want DICTIONARY", got)
	}

	measured := spreadFloats(256)
	if got := ChooseFloats(measured); got != RAW {
		t.Errorf("measured column chose %v, want RAW", got)
	}

	for _, vals := range [][]float64{constant, lowCard, measured, {}} {
		if len(EncodeFloats(vals)) > len(EncodeFloatsWith(RAW, vals)) {
			t.Errorf("chosen encoding beat RAW on len %d", len(vals))
		}
	}
}

// TestFloatDictionaryShrinksLowCardinality proves the dictionary actually compresses
// a long repetitive float column to well under the RAW size.
func TestFloatDictionaryShrinksLowCardinality(t *testing.T) {
	vals := make([]float64, 0, 1000)
	for i := range 1000 {
		vals = append(vals, []float64{1.0, 2.0, 3.0, 4.0}[i%4])
	}
	dict := len(EncodeFloatsWith(DICTIONARY, vals))
	raw := len(EncodeFloatsWith(RAW, vals))
	if dict >= raw/2 {
		t.Fatalf("dictionary %d not much smaller than RAW %d", dict, raw)
	}
}

func TestDecodeFloatsRejectsBadSegments(t *testing.T) {
	bad := [][]byte{
		nil,
		{0xFF},                         // unknown codec
		{byte(RAW), 0x04},              // claims 4 values, no body
		{byte(DICTIONARY), 0x01},       // claims a dict entry, no entry
		{byte(DICTIONARY), 0x00, 0x03}, // empty dict but claims 3 coded values
	}
	for i, b := range bad {
		if _, err := DecodeFloats(b); err == nil {
			t.Errorf("case %d: DecodeFloats(%v) returned nil error", i, b)
		}
	}
}

// spreadFloats builds well-distributed measured floats whose bit patterns have no
// run, no constant, and high cardinality, the shape no lightweight codec beats RAW
// on (doc 15 §3.3).
func spreadFloats(n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = math.Float64frombits(splitmix64(uint64(i)))
	}
	return out
}
