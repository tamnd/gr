package gr

import (
	"path/filepath"
	"testing"
)

// TestMaxPoolPagesConfiguresBufferPool proves the Options.MaxPoolPages knob
// reaches the pager's buffer pool rather than being silently dropped on the way
// through Open. A large database whose hot pages exceed the pager's small default
// pool thrashes on eviction, so an embedder needs this knob to size the pool to
// the working set. The test opens one database at the default pool size and one
// with a pool four times larger and asserts the configured budget scales with it,
// the signal the option threads all the way to the pager.
func TestMaxPoolPagesConfiguresBufferPool(t *testing.T) {
	open := func(name string, pages int) PoolStatsView {
		path := filepath.Join(t.TempDir(), name)
		db, err := Open(path, Options{MaxPoolPages: pages})
		if err != nil {
			t.Fatalf("Open %s: %v", name, err)
		}
		defer db.Close()
		s := db.eng.BufferPoolStats()
		return PoolStatsView{Budget: s.Budget}
	}

	small := open("small.gr", 0)     // 0 keeps the pager's built-in default
	large := open("large.gr", 65536) // 64K pages, 256 MiB at the 4 KiB default page

	if small.Budget <= 0 {
		t.Fatalf("default pool budget = %d, want positive", small.Budget)
	}
	if large.Budget <= small.Budget {
		t.Fatalf("configured pool budget = %d, want greater than the default %d", large.Budget, small.Budget)
	}
	// The configured pool asks for many more pages than the default, so its memory
	// ceiling must be far larger, not merely a page or two above it.
	if large.Budget < small.Budget*8 {
		t.Fatalf("configured pool budget = %d, want at least 8x the default %d", large.Budget, small.Budget)
	}
}

// PoolStatsView is the slice of the pager's PoolStats this test reads, kept local
// so the test does not depend on the pager package's import path.
type PoolStatsView struct {
	Budget int
}
