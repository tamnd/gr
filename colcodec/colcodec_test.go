package colcodec

import (
	"math"
	"reflect"
	"testing"
)

// roundTrip is the core oracle: an encoded segment decodes back to the exact input
// for every codec, which is what makes swapping a codec in safe.
func roundTrip(t *testing.T, name string, vals []int64) {
	t.Helper()
	seg := Encode(vals)
	got, err := Decode(seg)
	if err != nil {
		t.Fatalf("%s: decode: %v", name, err)
	}
	if !equalInts(vals, got) {
		t.Fatalf("%s: round trip changed data\n in: %v\nout: %v", name, vals, got)
	}
}

func equalInts(a, b []int64) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}

func TestRoundTripShapes(t *testing.T) {
	cases := map[string][]int64{
		"empty":          {},
		"single":         {42},
		"all-equal":      {7, 7, 7, 7, 7},
		"runs":           {1, 1, 1, 5, 5, 9, 9, 9, 9},
		"monotone":       {100, 101, 103, 106, 110, 115},
		"clustered":      {1000, 1002, 1001, 1003, 1000, 1004},
		"negatives":      {-5, -4, -3, -2, -1, 0, 1},
		"wide-range":     {0, 1 << 40, -(1 << 40), 1<<62 - 1},
		"min-max":        {math.MinInt64, 0, math.MaxInt64},
		"descending":     {50, 40, 30, 20, 10},
		"zero-then-jump": {0, 0, 0, 1000000},
	}
	for name, vals := range cases {
		roundTrip(t, name, vals)
	}
}

// TestEncodeWithRoundTripsEveryCodec forces each codec over data it can represent
// and proves it round-trips, independent of what the cascade would have chosen.
func TestEncodeWithRoundTripsEveryCodec(t *testing.T) {
	vals := []int64{3, 3, 3, 3}
	for _, c := range []Codec{RAW, CONSTANT, RLE, FOR, DELTA} {
		seg := EncodeWith(c, vals)
		if got, err := PeekCodec(seg); err != nil || got != c {
			t.Fatalf("EncodeWith(%v) wrote codec %v (err %v)", c, got, err)
		}
		out, err := Decode(seg)
		if err != nil {
			t.Fatalf("decode %v: %v", c, err)
		}
		if !equalInts(vals, out) {
			t.Fatalf("codec %v changed data: %v", c, out)
		}
	}
}

// TestEncodeWithFallsBackWhenInapplicable proves CONSTANT over unequal values does
// not silently corrupt: EncodeWith falls back to a codec that can represent it.
func TestEncodeWithFallsBackWhenInapplicable(t *testing.T) {
	vals := []int64{1, 2, 3}
	seg := EncodeWith(CONSTANT, vals)
	if c, _ := PeekCodec(seg); c == CONSTANT {
		t.Fatalf("CONSTANT accepted unequal values")
	}
	out, err := Decode(seg)
	if err != nil || !equalInts(vals, out) {
		t.Fatalf("fallback did not preserve data: %v err %v", out, err)
	}
}

// TestCascadePicksExpectedCodec pins the cascade's choice on shapes where one
// scheme is clearly best, so a regression that changes the selection is caught.
func TestCascadePicksExpectedCodec(t *testing.T) {
	cases := []struct {
		name string
		vals []int64
		want Codec
	}{
		{"constant", repeat(9, 64), CONSTANT},
		{"long-runs", runs(8, 32), RLE},
		// A long uniform ramp favors DELTA: its per-element cost is one varint byte
		// for the constant step, while FOR pays bits(range) per element, which grows
		// with the length.
		{"long-monotone", ramp(0, 1, 2000), DELTA},
		{"clustered-narrow", clustered(1_000_000, 8, 64), FOR},
		{"high-entropy", spread(64), RAW},
	}
	for _, tc := range cases {
		if got := Choose(tc.vals); got != tc.want {
			t.Errorf("%s: Choose = %v, want %v (size %d)", tc.name, got, tc.want, len(Encode(tc.vals)))
		}
		roundTrip(t, tc.name, tc.vals)
	}
}

// TestCascadeNeverLosesToRaw proves the chosen encoding is never larger than RAW,
// the property that makes the cascade safe to always apply.
func TestCascadeNeverLosesToRaw(t *testing.T) {
	for _, vals := range [][]int64{
		repeat(1, 100), ramp(0, 3, 100), clustered(500, 16, 100), spread(100), {},
	} {
		chosen := len(Encode(vals))
		raw := len(EncodeWith(RAW, vals))
		if chosen > raw {
			t.Fatalf("chosen %d bytes beat RAW %d bytes? data len %d", chosen, raw, len(vals))
		}
	}
}

// TestDecodeRejectsBadSegments proves the read path returns an error, never panics,
// on malformed bytes a corrupt or wrong-version file could hand it.
func TestDecodeRejectsBadSegments(t *testing.T) {
	bad := [][]byte{
		nil,
		{},
		{0xFF},                        // unknown codec id
		{byte(RAW), 0x05},             // claims 5 values, no body
		{byte(FOR), 0x02, 0x00, 0x40}, // claims width 64 but no packed bytes
		{byte(DELTA), 0x03},           // claims 3 values, no first value
		{byte(RLE), 0x01},             // claims a run, no run body
	}
	for i, b := range bad {
		if _, err := Decode(b); err == nil {
			t.Errorf("case %d: Decode(%v) returned nil error", i, b)
		}
	}
}

func TestPeekCodec(t *testing.T) {
	if _, err := PeekCodec(nil); err == nil {
		t.Fatal("PeekCodec(nil) should error")
	}
	seg := EncodeWith(DELTA, ramp(0, 1, 10))
	if c, err := PeekCodec(seg); err != nil || c != DELTA {
		t.Fatalf("PeekCodec = %v err %v, want DELTA", c, err)
	}
}

// Helpers that build the representative shapes the cascade is meant to recognize.

func repeat(v int64, n int) []int64 {
	out := make([]int64, n)
	for i := range out {
		out[i] = v
	}
	return out
}

func runs(runLen, runCount int) []int64 {
	var out []int64
	for r := range runCount {
		for range runLen {
			out = append(out, int64(r))
		}
	}
	return out
}

func ramp(start, step int64, n int) []int64 {
	out := make([]int64, n)
	v := start
	for i := range out {
		out[i] = v
		v += step
	}
	return out
}

// clustered builds values within a narrow band above a large base, the shape
// frame-of-reference packs tightest (a big shared base, small per-value spread).
func clustered(base int64, spread, n int) []int64 {
	out := make([]int64, n)
	for i := range out {
		out[i] = base + int64((i*7)%spread)
	}
	return out
}

// spread builds well-distributed full-width values with no run, monotonicity, or
// narrow band, the shape no lightweight codec beats RAW on: the range forces FOR to
// a 64-bit width (so it only adds frame overhead) and the differences are
// full-magnitude so DELTA's varints are large. The endpoints are pinned to the
// int64 extremes so the frame width is unambiguously 64, and splitmix64 fills the
// rest so the deltas carry no structure.
func spread(n int) []int64 {
	out := make([]int64, n)
	for i := range out {
		out[i] = int64(splitmix64(uint64(i)))
	}
	out[0] = math.MinInt64
	out[n-1] = math.MaxInt64
	return out
}

func splitmix64(x uint64) uint64 {
	x += 0x9E3779B97F4A7C15
	x = (x ^ (x >> 30)) * 0xBF58476D1CE4E5B9
	x = (x ^ (x >> 27)) * 0x94D049BB133111EB
	return x ^ (x >> 31)
}
