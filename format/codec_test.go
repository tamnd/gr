package format

import (
	"math"
	"testing"

	"github.com/tamnd/gr/value"
)

func TestUvarintRoundTrip(t *testing.T) {
	cases := []uint64{0, 1, 127, 128, 300, 1 << 20, math.MaxUint32, math.MaxUint64}
	for _, n := range cases {
		b := AppendUvarint(nil, n)
		got, k, err := Uvarint(b)
		if err != nil {
			t.Fatalf("Uvarint(%d): %v", n, err)
		}
		if got != n || k != len(b) {
			t.Fatalf("Uvarint(%d) = (%d,%d), want (%d,%d)", n, got, k, n, len(b))
		}
	}
}

func TestVarintRoundTrip(t *testing.T) {
	cases := []int64{0, -1, 1, -128, 127, math.MinInt64, math.MaxInt64}
	for _, n := range cases {
		b := AppendVarint(nil, n)
		got, k, err := Varint(b)
		if err != nil {
			t.Fatalf("Varint(%d): %v", n, err)
		}
		if got != n || k != len(b) {
			t.Fatalf("Varint(%d) = (%d,%d), want (%d,%d)", n, got, k, n, len(b))
		}
	}
}

func TestUvarintTruncated(t *testing.T) {
	b := AppendUvarint(nil, 1<<35)
	if _, _, err := Uvarint(b[:len(b)-1]); err != ErrShortBuffer {
		t.Fatalf("want ErrShortBuffer on truncated input, got %v", err)
	}
}

func TestValueCodecRoundTrip(t *testing.T) {
	vals := []value.Value{
		value.Null,
		value.Bool(true),
		value.Bool(false),
		value.Int(0),
		value.Int(-9223372036854775808),
		value.Int(9223372036854775807),
		value.Float(3.14159),
		value.Float(math.Inf(1)),
		value.String(""),
		value.String("héllo, 世界"),
		value.Bytes([]byte{0, 1, 2, 255}),
		value.List(value.Int(1), value.String("x"), value.Null),
		value.Map(map[string]value.Value{"a": value.Int(1), "b": value.List()}),
	}
	for _, v := range vals {
		b := AppendValue(nil, v)
		got, k, err := DecodeValue(b)
		if err != nil {
			t.Fatalf("DecodeValue(%s): %v", v.String(), err)
		}
		if k != len(b) {
			t.Fatalf("DecodeValue(%s): consumed %d of %d", v.String(), k, len(b))
		}
		if !got.Equal(v) {
			t.Fatalf("value round-trip mismatch: got %s want %s", got.String(), v.String())
		}
	}
}

func TestU16U32U64(t *testing.T) {
	var b [8]byte
	PutU16(b[:], 0xABCD)
	if U16(b[:]) != 0xABCD {
		t.Fatal("u16 round-trip")
	}
	PutU32(b[:], 0xDEADBEEF)
	if U32(b[:]) != 0xDEADBEEF {
		t.Fatal("u32 round-trip")
	}
	PutU64(b[:], 0x0123456789ABCDEF)
	if U64(b[:]) != 0x0123456789ABCDEF {
		t.Fatal("u64 round-trip")
	}
}
