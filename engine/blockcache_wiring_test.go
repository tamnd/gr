package engine

import (
	"testing"

	"github.com/tamnd/gr/catalog"
	"github.com/tamnd/gr/value"
	"github.com/tamnd/gr/vfs"
)

// TestSegmentReadCachesDecodedSegment proves the read path caches a decoded segment:
// the first read of a folded value decodes its segment and caches it, and a second
// read of another position in the same segment is served from that one entry without
// adding another. The fold against an empty base on the first checkpoint caches
// nothing, so the cache starts clean.
func TestSegmentReadCachesDecodedSegment(t *testing.T) {
	fsys := vfs.NewMem()
	e := openDisk(t, fsys, "cache.gr")
	defer e.Close()

	name, _ := e.Intern(catalog.KindPropKey, "name")
	tx, _ := e.Begin(true)
	a, _ := tx.CreateNode(nil)
	b, _ := tx.CreateNode(nil)
	tx.SetNodeProperty(a, name, value.String("alice"))
	tx.SetNodeProperty(b, name, value.String("bob"))
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := e.Checkpoint(); err != nil {
		t.Fatal(err)
	}

	// The first fold ran against an empty base, so nothing was cached by it.
	if got := e.bc.Len(); got != 0 {
		t.Fatalf("cache not empty after first checkpoint: %d entries", got)
	}

	rx, _ := e.Begin(false)
	defer rx.Abort()

	// a and b share one segment (both positions are under segPositions), so the
	// first read decodes and caches it and the second is a hit on the same entry.
	if v, _ := rx.NodeProperty(a, name); mustStr(t, v) != "alice" {
		t.Fatalf("a.name = %v, want alice", v)
	}
	if got := e.bc.Len(); got != 1 {
		t.Fatalf("cache = %d entries after first read, want 1", got)
	}
	if v, _ := rx.NodeProperty(b, name); mustStr(t, v) != "bob" {
		t.Fatalf("b.name = %v, want bob", v)
	}
	if got := e.bc.Len(); got != 1 {
		t.Fatalf("cache = %d entries after same-segment read, want 1 (a hit, not a new entry)", got)
	}
}

// TestCrossSegmentReadsCacheSeparately reads positions in two different segments and
// asserts each caches its own entry, so the cache key separates segments within a
// column.
func TestCrossSegmentReadsCacheSeparately(t *testing.T) {
	fsys := vfs.NewMem()
	e := openDisk(t, fsys, "twoseg.gr")
	defer e.Close()

	val, _ := e.Intern(catalog.KindPropKey, "val")
	const n = segPositions + 1 // one position spills into a second segment

	tx, _ := e.Begin(true)
	ids := make([]NodeID, n)
	for i := range ids {
		id, _ := tx.CreateNode(nil)
		ids[i] = id
		tx.SetNodeProperty(id, val, value.Int(int64(i)))
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := e.Checkpoint(); err != nil {
		t.Fatal(err)
	}

	rx, _ := e.Begin(false)
	defer rx.Abort()

	// One read in the first segment, one in the second: two distinct cache entries.
	if v, _ := rx.NodeProperty(ids[0], val); mustInt(t, v) != 0 {
		t.Fatalf("ids[0].val = %v, want 0", v)
	}
	if v, _ := rx.NodeProperty(ids[n-1], val); mustInt(t, v) != int64(n-1) {
		t.Fatalf("ids[n-1].val = %v, want %d", v, n-1)
	}
	if got := e.bc.Len(); got != 2 {
		t.Fatalf("cache = %d entries across two segments, want 2", got)
	}
}

// TestBlockCacheStatsCountsHitsAndMisses proves the stats snapshot tracks the lookup
// outcomes the column-cache metrics expose: the first read of a folded segment is a
// miss that decodes and caches it, a second read in the same segment is a hit on that
// one entry, and the resident counts reflect the single cached block. The fold against
// an empty base on the first checkpoint caches nothing and serves no reads, so the
// counts start clean.
func TestBlockCacheStatsCountsHitsAndMisses(t *testing.T) {
	fsys := vfs.NewMem()
	e := openDisk(t, fsys, "stats.gr")
	defer e.Close()

	name, _ := e.Intern(catalog.KindPropKey, "name")
	tx, _ := e.Begin(true)
	a, _ := tx.CreateNode(nil)
	b, _ := tx.CreateNode(nil)
	tx.SetNodeProperty(a, name, value.String("alice"))
	tx.SetNodeProperty(b, name, value.String("bob"))
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := e.Checkpoint(); err != nil {
		t.Fatal(err)
	}

	if s := e.BlockCacheStats(); s.Hits != 0 || s.Misses != 0 {
		t.Fatalf("stats after checkpoint = %+v, want zero hits and misses", s)
	}

	rx, _ := e.Begin(false)
	defer rx.Abort()

	// The first read decodes and caches its segment: a miss.
	if v, _ := rx.NodeProperty(a, name); mustStr(t, v) != "alice" {
		t.Fatalf("a.name = %v, want alice", v)
	}
	if s := e.BlockCacheStats(); s.Hits != 0 || s.Misses != 1 {
		t.Fatalf("stats after first read = %+v, want 0 hits and 1 miss", s)
	}

	// b shares a's segment, so its read is served from the cached entry: a hit.
	if v, _ := rx.NodeProperty(b, name); mustStr(t, v) != "bob" {
		t.Fatalf("b.name = %v, want bob", v)
	}
	s := e.BlockCacheStats()
	if s.Hits != 1 || s.Misses != 1 {
		t.Fatalf("stats after same-segment read = %+v, want 1 hit and 1 miss", s)
	}
	if s.Blocks != 1 {
		t.Fatalf("stats blocks = %d, want 1 resident block", s.Blocks)
	}
	if s.Bytes <= 0 {
		t.Fatalf("stats bytes = %d, want a positive resident size", s.Bytes)
	}
}

// TestCheckpointInvalidatesCachedSegment proves a checkpoint that rebuilds the base
// invalidates the cache by epoch: a value cached before the second checkpoint is not
// served afterward, so the read returns the freshly folded value, not the stale one.
func TestCheckpointInvalidatesCachedSegment(t *testing.T) {
	fsys := vfs.NewMem()
	e := openDisk(t, fsys, "epoch.gr")
	defer e.Close()

	name, _ := e.Intern(catalog.KindPropKey, "name")
	tx, _ := e.Begin(true)
	a, _ := tx.CreateNode(nil)
	tx.SetNodeProperty(a, name, value.String("v1"))
	tx.Commit()
	if err := e.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	epoch1 := e.epoch

	// Read v1 so it is cached at this epoch.
	r1, _ := e.Begin(false)
	if v, _ := r1.NodeProperty(a, name); mustStr(t, v) != "v1" {
		t.Fatalf("a.name = %v, want v1", v)
	}
	r1.Abort()
	if e.bc.Len() == 0 {
		t.Fatal("read did not cache the segment")
	}

	// Overwrite and checkpoint: the fold bumps the epoch, so the v1 entry is stale.
	tx2, _ := e.Begin(true)
	tx2.SetNodeProperty(a, name, value.String("v2"))
	tx2.Commit()
	if err := e.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if e.epoch == epoch1 {
		t.Fatal("checkpoint did not bump the epoch")
	}

	// The read must see v2: a stale v1 cache entry from the old epoch is not served.
	r2, _ := e.Begin(false)
	defer r2.Abort()
	if v, _ := r2.NodeProperty(a, name); mustStr(t, v) != "v2" {
		t.Fatalf("a.name = %v, want v2 (stale cache must not be served across epochs)", v)
	}
}
