package column_test

import (
	"fmt"
	"testing"

	"github.com/tamnd/gr/column"
	"github.com/tamnd/gr/pager"
	"github.com/tamnd/gr/store"
	"github.com/tamnd/gr/value"
	"github.com/tamnd/gr/vfs"
	"github.com/tamnd/gr/wal"
)

const path = "col.gr"

func openPager(t *testing.T, fsys vfs.VFS) *pager.Pager {
	t.Helper()
	p, err := pager.Open(fsys, path, pager.Options{Sync: wal.SyncFull, SaltSeed: 1})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestColumnRoundTrip sets values of every type across several keys and
// positions, including a stored null and a removed property, then reopens and
// verifies absence vs stored-null are distinguished and every value survives.
func TestColumnRoundTrip(t *testing.T) {
	fsys := vfs.NewMem()
	p := openPager(t, fsys)
	secs, _ := store.CreateSections(p)
	c, err := column.Create(p, secs, store.SecNodeCols)
	if err != nil {
		t.Fatal(err)
	}

	// key 0 = name (string) on positions 0 and 2; key 1 = age (int) on 0;
	// key 2 = a stored null on 1; key 3 set then removed on 0.
	must(t, c.Set(0, 0, value.String("alice")))
	must(t, c.Set(0, 2, value.String("carol")))
	must(t, c.Set(1, 0, value.Int(30)))
	must(t, c.Set(2, 1, value.Null))
	must(t, c.Set(3, 0, value.Bool(true)))
	must(t, c.Remove(3, 0))
	if err := p.Commit(); err != nil {
		t.Fatal(err)
	}
	p.Close()

	p2 := openPager(t, fsys)
	defer p2.Close()
	secs2, _ := store.OpenSections(p2)
	c2, err := column.Open(p2, secs2, store.SecNodeCols)
	if err != nil {
		t.Fatal(err)
	}

	checkStr(t, c2, 0, 0, "alice")
	checkStr(t, c2, 0, 2, "carol")
	if v, ok, _ := c2.Get(1, 0); !ok {
		t.Fatalf("key1 pos0 missing")
	} else if iv, _ := v.AsInt(); iv != 30 {
		t.Fatalf("key1 pos0 = %d", iv)
	}
	// Stored null: present but null.
	if v, ok, _ := c2.Get(2, 1); !ok || !v.IsNull() {
		t.Fatalf("key2 pos1 = %v,%v, want present null", v, ok)
	}
	// Absent (never set) on a position that exists in another column.
	if _, ok, _ := c2.Get(1, 2); ok {
		t.Fatal("key1 pos2 should be absent")
	}
	// Removed property is absent again.
	if _, ok, _ := c2.Get(3, 0); ok {
		t.Fatal("key3 pos0 should be absent after Remove")
	}

	all, err := c2.All(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 { // key0 and key1 present at pos0; key3 removed
		t.Fatalf("All(0) = %v, want 2 entries", all)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func checkStr(t *testing.T, c *column.Columns, key uint32, pos uint64, want string) {
	t.Helper()
	v, ok, err := c.Get(key, pos)
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := v.AsString(); !ok || s != want {
		t.Fatalf("key%d pos%d = %v,%v, want %q", key, pos, v, ok, want)
	}
}

// --- crash campaign over the column store ---

const cpath = "colcrash.gr"

func buildClean(t *testing.T) *vfs.Mem {
	t.Helper()
	fsys := vfs.NewMem()
	p, err := pager.Open(fsys, cpath, pager.Options{Sync: wal.SyncFull, SaltSeed: 1})
	if err != nil {
		t.Fatal(err)
	}
	secs, _ := store.CreateSections(p)
	if _, err := column.Create(p, secs, store.SecNodeCols); err != nil {
		t.Fatal(err)
	}
	if err := p.Commit(); err != nil {
		t.Fatal(err)
	}
	p.Close()
	return fsys
}

// runWorkload reopens and, per step, sets two properties (an int that equals the
// step and a string) on a fresh position, one commit per step.
func runWorkload(fsys vfs.VFS, T int) (err error) {
	p, e := pager.Open(fsys, cpath, pager.Options{Sync: wal.SyncFull, SaltSeed: 7})
	if e != nil {
		return e
	}
	defer func() { _ = p.Close() }()
	secs, e := store.OpenSections(p)
	if e != nil {
		return e
	}
	c, e := column.Open(p, secs, store.SecNodeCols)
	if e != nil {
		return e
	}
	for j := range T {
		if e := c.Set(0, uint64(j), value.Int(int64(j))); e != nil {
			return e
		}
		if e := c.Set(1, uint64(j), value.String(fmt.Sprintf("v%d", j))); e != nil {
			return e
		}
		if e := p.Commit(); e != nil {
			return e
		}
	}
	return nil
}

// verifyDurablePrefix reopens a crashed snapshot and asserts every present cell
// decodes to a self-consistent value: key0 at pos must equal pos, and key1 must
// be the matching string. A torn value or an index/value extent mismatch would
// surface as a decode error or a wrong value.
func verifyDurablePrefix(t *testing.T, crashed *vfs.Mem, label string) {
	t.Helper()
	p, err := pager.Open(crashed, cpath, pager.Options{})
	if err != nil {
		t.Fatalf("%s: reopen failed: %v", label, err)
	}
	defer p.Close()
	secs, err := store.OpenSections(p)
	if err != nil {
		t.Fatalf("%s: reopen sections failed: %v", label, err)
	}
	c, err := column.Open(p, secs, store.SecNodeCols)
	if err != nil {
		t.Fatalf("%s: column reopen failed: %v", label, err)
	}
	for pos := range uint64(64) {
		iv, iok, err := c.Get(0, pos)
		if err != nil {
			t.Fatalf("%s: get key0 pos%d: %v", label, pos, err)
		}
		sv, sok, err := c.Get(1, pos)
		if err != nil {
			t.Fatalf("%s: get key1 pos%d: %v", label, pos, err)
		}
		if iok != sok {
			t.Fatalf("%s: pos%d key0 present=%v but key1 present=%v (partial step)",
				label, pos, iok, sok)
		}
		if !iok {
			continue
		}
		if n, _ := iv.AsInt(); n != int64(pos) {
			t.Fatalf("%s: pos%d key0 = %d, want %d", label, pos, n, pos)
		}
		if s, _ := sv.AsString(); s != fmt.Sprintf("v%d", pos) {
			t.Fatalf("%s: pos%d key1 = %q", label, pos, s)
		}
	}
}

func crashCampaign(t *testing.T, mode vfs.TripMode, label string) {
	const T = 6
	clean := buildClean(t)

	counter := vfs.NewCounter()
	cfs := clean.Snapshot()
	cfs.Attach(counter)
	if err := runWorkload(cfs, T); err != nil {
		t.Fatalf("%s: counting run errored: %v", label, err)
	}
	n := counter.Count()
	if n == 0 {
		t.Fatalf("%s: no fault points", label)
	}
	for trip := range n {
		fs := clean.Snapshot()
		fs.Attach(vfs.NewTrip(trip, mode))
		_ = runWorkload(fs, T)
		verifyDurablePrefix(t, fs.Snapshot(), label)
	}
}

func TestColumnCrashCampaignCrash(t *testing.T) { crashCampaign(t, vfs.TripCrash, "crash") }
func TestColumnCrashCampaignTorn(t *testing.T)  { crashCampaign(t, vfs.TripTear, "torn") }
func TestColumnCrashCampaignFsync(t *testing.T) { crashCampaign(t, vfs.TripFsyncFail, "fsync") }
