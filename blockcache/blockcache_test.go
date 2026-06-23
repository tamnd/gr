package blockcache

import (
	"reflect"
	"testing"
)

func TestDecodedRoundTrip(t *testing.T) {
	c := New(1 << 20)
	vec := []int64{1, 2, 3, 4}
	c.PutDecoded(Key{Column: 7, Segment: 0}, 1, vec, 32, []byte("comp"), []byte("a"), []byte("z"))

	got, ok := c.GetDecoded(Key{Column: 7, Segment: 0}, 1)
	if !ok {
		t.Fatal("decoded miss after put")
	}
	if !reflect.DeepEqual(got, vec) {
		t.Fatalf("decoded vector changed: %v", got)
	}
}

// TestVersionMismatchMisses proves a read at a different base version misses and drops
// the stale entry, the coherence rule that lets a checkpoint replace a base segment.
func TestVersionMismatchMisses(t *testing.T) {
	c := New(1 << 20)
	k := Key{Column: 1, Segment: 2}
	c.PutDecoded(k, 5, []int64{9}, 8, nil, nil, nil)

	if _, ok := c.GetDecoded(k, 6); ok {
		t.Fatal("stale version returned a hit")
	}
	if c.Len() != 0 {
		t.Fatalf("stale entry not dropped, Len = %d", c.Len())
	}
}

// TestZoneMapHitsAtEveryResidency proves the zone map is served from a decoded entry
// and still from a stub after the values are cooled away, the resident skip index.
func TestZoneMapHitsAtEveryResidency(t *testing.T) {
	c := New(1 << 20)
	k := Key{Column: 3, Segment: 1}
	c.PutDecoded(k, 1, []int64{1}, 8, []byte("c"), []byte("lo"), []byte("hi"))

	min, max, ok := c.ZoneMap(k, 1)
	if !ok || string(min) != "lo" || string(max) != "hi" {
		t.Fatalf("zone map miss at decoded residency: %q %q %v", min, max, ok)
	}

	// Cool the entry all the way to a stub by hand, then the zone map must survive.
	c.mu.Lock()
	el := c.entries[k]
	c.cool(el) // decoded -> compressed
	c.cool(el) // compressed -> stub
	c.mu.Unlock()

	if _, ok := c.GetDecoded(k, 1); ok {
		t.Fatal("decoded hit after cooling to stub")
	}
	min, max, ok = c.ZoneMap(k, 1)
	if !ok || string(min) != "lo" || string(max) != "hi" {
		t.Fatalf("zone map lost after cooling to stub: %q %q %v", min, max, ok)
	}
}

// TestCompressedResidency proves a compressed-resident entry serves its bytes and not
// a decoded vector.
func TestCompressedResidency(t *testing.T) {
	c := New(1 << 20)
	k := Key{Column: 2, Segment: 0}
	c.PutCompressed(k, 1, []byte("packed"), []byte("a"), []byte("b"))

	if _, ok := c.GetDecoded(k, 1); ok {
		t.Fatal("compressed entry returned a decoded hit")
	}
	got, ok := c.GetCompressed(k, 1)
	if !ok || string(got) != "packed" {
		t.Fatalf("compressed miss: %q %v", got, ok)
	}
}

// TestEvictionStagesCooling proves the budget drives the staged cooling: an unreferenced
// decoded entry downgrades to compressed, then to a stub, freeing value memory while
// keeping the zone map, as new entries push the cache over budget.
func TestEvictionStagesCooling(t *testing.T) {
	// Budget holds about one decoded vector. Each entry is 100 bytes decoded plus 10
	// compressed.
	c := New(120)
	mk := func(col uint32) Key { return Key{Column: col} }
	put := func(col uint32) {
		c.PutDecoded(mk(col), 1, []int64{int64(col)}, 100, make([]byte, 10), []byte("lo"), []byte("hi"))
	}

	put(1)
	// Touch column 1 so it is referenced and survives the next sweep.
	c.GetDecoded(mk(1), 1)
	put(2) // pushes over budget; the sweep should cool the cold cold entry, sparing the referenced one once
	put(3)

	if c.Bytes() > c.maxBytes {
		t.Fatalf("cache over budget after evictions: %d > %d", c.Bytes(), c.maxBytes)
	}
	// Every column's zone map must still be reachable: cooled entries leave stubs.
	for _, col := range []uint32{1, 2, 3} {
		if _, _, ok := c.ZoneMap(mk(col), 1); !ok {
			t.Errorf("zone map for column %d lost", col)
		}
	}
}

// TestPinPreventsCooling proves a pinned entry keeps its decoded vector even under
// budget pressure that cools everything else.
func TestPinPreventsCooling(t *testing.T) {
	c := New(120)
	pinned := Key{Column: 1}
	c.PutDecoded(pinned, 1, []int64{1}, 100, make([]byte, 10), nil, nil)
	c.Pin(pinned)

	for col := uint32(2); col < 8; col++ {
		c.PutDecoded(Key{Column: col}, 1, []int64{int64(col)}, 100, make([]byte, 10), nil, nil)
	}

	if _, ok := c.GetDecoded(pinned, 1); !ok {
		t.Fatal("pinned decoded vector was cooled away")
	}
	c.Unpin(pinned)
}

// TestInvalidateDrops proves the eager invalidation path removes an entry outright.
func TestInvalidateDrops(t *testing.T) {
	c := New(1 << 20)
	k := Key{Column: 4, Segment: 9}
	c.PutDecoded(k, 1, []int64{1}, 8, nil, nil, nil)
	c.Invalidate(k)
	if _, ok := c.GetDecoded(k, 1); ok {
		t.Fatal("entry survived Invalidate")
	}
	if c.Len() != 0 {
		t.Fatalf("Len = %d after Invalidate", c.Len())
	}
}

// TestEvictionStatsBySplitReason proves the eviction counters split a block leaving the
// cache by why: a budget sweep that cools a cold entry to nothing is a capacity eviction,
// a version-mismatched lookup and an eager Invalidate are invalidations (doc 20 §4.4).
func TestEvictionStatsBySplitReason(t *testing.T) {
	// A version mismatch drops the stale entry as an invalidation.
	c := New(1 << 20)
	k := Key{Column: 1}
	c.PutDecoded(k, 1, []int64{1}, 8, nil, nil, nil)
	if _, ok := c.GetDecoded(k, 2); ok {
		t.Fatal("a version-2 lookup hit a version-1 entry")
	}
	if s := c.Stats(); s.EvictInvalidation != 1 || s.EvictCapacity != 0 {
		t.Fatalf("after a version mismatch: invalidation=%d capacity=%d, want 1 and 0", s.EvictInvalidation, s.EvictCapacity)
	}

	// An eager Invalidate is the second invalidation form.
	c.PutDecoded(k, 1, []int64{1}, 8, nil, nil, nil)
	c.Invalidate(k)
	if s := c.Stats(); s.EvictInvalidation != 2 {
		t.Fatalf("after an eager Invalidate: invalidation=%d, want 2", s.EvictInvalidation)
	}

	// Budget pressure on cold entries cools them stage by stage to a stub and finally evicts
	// them, the capacity reason. Each entry is 100 bytes decoded plus 10 compressed against a
	// 120-byte budget, so a run of cold inserts forces full evictions.
	cap := New(120)
	for col := uint32(0); col < 8; col++ {
		cap.PutDecoded(Key{Column: col}, 1, []int64{int64(col)}, 100, make([]byte, 10), nil, nil)
	}
	if s := cap.Stats(); s.EvictCapacity == 0 {
		t.Fatalf("capacity evictions after eight cold inserts on a one-entry budget = 0, want some")
	}
	if s := cap.Stats(); s.EvictInvalidation != 0 {
		t.Fatalf("capacity-only workload reported %d invalidations, want 0", s.EvictInvalidation)
	}
}

// TestZeroBudgetKeepsStubs proves a cache with no value budget still retains zone-map
// stubs for segment skipping, cooling away every value vector on insert.
func TestZeroBudgetKeepsStubs(t *testing.T) {
	c := New(0)
	k := Key{Column: 1}
	c.PutDecoded(k, 1, []int64{1, 2, 3}, 24, []byte("c"), []byte("lo"), []byte("hi"))

	if c.Bytes() != 0 {
		t.Fatalf("zero-budget cache holds %d value bytes", c.Bytes())
	}
	if _, ok := c.GetDecoded(k, 1); ok {
		t.Fatal("zero-budget cache kept a decoded vector")
	}
	if _, _, ok := c.ZoneMap(k, 1); !ok {
		t.Fatal("zero-budget cache dropped the zone-map stub")
	}
}
