package mvcc_test

import (
	"testing"

	"github.com/tamnd/gr/mvcc"
	"github.com/tamnd/gr/value"
)

func TestOracleSeqAndSnapshots(t *testing.T) {
	o := mvcc.NewOracle(10)
	if o.Seq() != 10 {
		t.Fatalf("seq = %d, want 10", o.Seq())
	}
	id1, r1 := o.Begin()
	if r1 != 10 {
		t.Fatalf("snapshot read = %d, want 10", r1)
	}
	o.SetSeq(11)
	id2, r2 := o.Begin()
	if r2 != 11 {
		t.Fatalf("second snapshot read = %d, want 11", r2)
	}
	// Watermark is the oldest live read sequence.
	if wm := o.Watermark(); wm != 10 {
		t.Fatalf("watermark = %d, want 10 (oldest live)", wm)
	}
	o.End(id1)
	if wm := o.Watermark(); wm != 11 {
		t.Fatalf("watermark after releasing oldest = %d, want 11", wm)
	}
	o.End(id2)
	// No live snapshots: watermark is the current sequence.
	if wm := o.Watermark(); wm != 11 {
		t.Fatalf("watermark with none live = %d, want 11", wm)
	}
	// SetSeq never goes backwards.
	o.SetSeq(5)
	if o.Seq() != 11 {
		t.Fatalf("seq went backwards to %d", o.Seq())
	}
}

func TestOverlayResolveAndGC(t *testing.T) {
	ov := mvcc.NewOverlay()
	key := mvcc.Key{Kind: mvcc.NodeProp, Pos: 7, Sub: 2}

	// The datum was "v1" until a write at seq 5 replaced it; retain that pre-image.
	ov.Record(5, key, mvcc.Pre{Present: true, Val: value.String("v1")})
	// Then a write at seq 9 replaced "v2" (the value between seq 5 and 9).
	ov.Record(9, key, mvcc.Pre{Present: true, Val: value.String("v2")})

	// A snapshot at read seq 4 (before either write) sees the earliest pre-image.
	if pre, ok := ov.Resolve(key, 4); !ok || mustStr(t, pre.Val) != "v1" {
		t.Fatalf("resolve@4 = %v ok=%v, want v1", pre.Val, ok)
	}
	// A snapshot at read seq 6 (after the seq-5 write, before the seq-9 write)
	// sees the value that the seq-9 write replaced.
	if pre, ok := ov.Resolve(key, 6); !ok || mustStr(t, pre.Val) != "v2" {
		t.Fatalf("resolve@6 = %v ok=%v, want v2", pre.Val, ok)
	}
	// A snapshot at or after the latest write reads the base (no overlay entry).
	if _, ok := ov.Resolve(key, 9); ok {
		t.Fatal("resolve@9 should fall through to base")
	}

	// GC at watermark 5 drops the seq-5 pre-image (no live snapshot reads below 5
	// once the watermark reaches it) but keeps the seq-9 one.
	ov.GC(5)
	if ov.Len() != 1 {
		t.Fatalf("after GC(5) Len = %d, want 1 (seq-9 survives)", ov.Len())
	}
	if _, ok := ov.Resolve(key, 6); !ok {
		t.Fatal("seq-9 pre-image should survive GC(5)")
	}
	// GC at watermark 9 drops everything.
	ov.GC(9)
	if ov.Len() != 0 {
		t.Fatalf("overlay should be empty after GC(9), got %d", ov.Len())
	}
}

func mustStr(t *testing.T, v value.Value) string {
	t.Helper()
	s, ok := v.AsString()
	if !ok {
		t.Fatalf("not a string: %v", v)
	}
	return s
}
