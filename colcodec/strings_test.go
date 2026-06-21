package colcodec

import (
	"fmt"
	"reflect"
	"testing"
)

func roundTripStrings(t *testing.T, name string, vals []string) {
	t.Helper()
	seg := EncodeStrings(vals)
	got, err := DecodeStrings(seg)
	if err != nil {
		t.Fatalf("%s: decode: %v", name, err)
	}
	if !equalStrings(vals, got) {
		t.Fatalf("%s: round trip changed data\n in: %q\nout: %q", name, vals, got)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}

func TestStringRoundTripShapes(t *testing.T) {
	cases := map[string][]string{
		"empty":         {},
		"single":        {"hello"},
		"all-equal":     {"x", "x", "x", "x"},
		"low-card":      {"red", "green", "blue", "red", "blue", "green", "red"},
		"all-distinct":  {"a", "bb", "ccc", "dddd"},
		"empty-strings": {"", "", "a", ""},
		"unicode":       {"café", "naïve", "café", "Ω", "Ω"},
		"long":          {repeatStr("ab", 500), "x", repeatStr("ab", 500)},
	}
	for name, vals := range cases {
		roundTripStrings(t, name, vals)
	}
}

// TestStringEncodeWithRoundTrips forces each string codec and proves it round-trips
// independent of the cascade's choice.
func TestStringEncodeWithRoundTrips(t *testing.T) {
	vals := []string{"a", "b", "a", "c", "b", "a"}
	for _, c := range []Codec{RAW, DICTIONARY} {
		seg := EncodeStringsWith(c, vals)
		if got, err := PeekCodec(seg); err != nil || got != c {
			t.Fatalf("EncodeStringsWith(%v) wrote codec %v (err %v)", c, got, err)
		}
		out, err := DecodeStrings(seg)
		if err != nil || !equalStrings(vals, out) {
			t.Fatalf("codec %v: out %q err %v", c, out, err)
		}
	}
}

// TestStringCascade pins the choice: a repetitive low-cardinality column picks the
// dictionary, an all-distinct column stays RAW, and neither loses to RAW.
func TestStringCascade(t *testing.T) {
	lowCard := make([]string, 0, 300)
	for i := range 300 {
		lowCard = append(lowCard, []string{"active", "pending", "closed"}[i%3])
	}
	if got := ChooseStrings(lowCard); got != DICTIONARY {
		t.Errorf("low-cardinality column chose %v, want DICTIONARY", got)
	}

	distinct := make([]string, 0, 64)
	for i := range 64 {
		distinct = append(distinct, fmt.Sprintf("id-%d", i))
	}
	if got := ChooseStrings(distinct); got != RAW {
		t.Errorf("all-distinct column chose %v, want RAW", got)
	}

	for _, vals := range [][]string{lowCard, distinct, {}} {
		if len(EncodeStrings(vals)) > len(EncodeStringsWith(RAW, vals)) {
			t.Errorf("chosen encoding beat RAW on len %d", len(vals))
		}
	}
}

// TestDictionaryShrinksLowCardinality proves the dictionary actually compresses,
// not just that it round-trips: a long repetitive column is much smaller than RAW.
func TestDictionaryShrinksLowCardinality(t *testing.T) {
	vals := make([]string, 0, 1000)
	for i := range 1000 {
		vals = append(vals, []string{"alpha", "beta", "gamma", "delta"}[i%4])
	}
	dict := len(EncodeStringsWith(DICTIONARY, vals))
	raw := len(EncodeStringsWith(RAW, vals))
	if dict >= raw/2 {
		t.Fatalf("dictionary %d not much smaller than RAW %d", dict, raw)
	}
}

func TestDecodeStringsRejectsBadSegments(t *testing.T) {
	bad := [][]byte{
		nil,
		{0xFF},                         // unknown codec
		{byte(RAW), 0x02, 0x05},        // claims 2 values, one length, no heap
		{byte(DICTIONARY), 0x01},       // claims a dict entry, no entry
		{byte(DICTIONARY), 0x00, 0x03}, // empty dict but claims 3 coded values
	}
	for i, b := range bad {
		if _, err := DecodeStrings(b); err == nil {
			t.Errorf("case %d: DecodeStrings(%v) returned nil error", i, b)
		}
	}
}

func repeatStr(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for range n {
		out = append(out, s...)
	}
	return string(out)
}
