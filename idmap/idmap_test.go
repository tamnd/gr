package idmap

import (
	"testing"

	"github.com/tamnd/gr/pager"
	"github.com/tamnd/gr/store"
	"github.com/tamnd/gr/vfs"
	"github.com/tamnd/gr/wal"
)

const path = "idmap.gr"

func openPager(t *testing.T, fsys vfs.VFS) *pager.Pager {
	t.Helper()
	p, err := pager.Open(fsys, path, pager.Options{Sync: wal.SyncFull, SaltSeed: 1})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestAllocEncoding checks the two-id scheme: node and relationship ids never
// collide, ids are monotone per kind, and sequences start at 1.
func TestAllocEncoding(t *testing.T) {
	fsys := vfs.NewMem()
	p := openPager(t, fsys)
	defer p.Close()
	secs, _ := store.CreateSections(p)
	m, err := Create(p, secs)
	if err != nil {
		t.Fatal(err)
	}

	n0, p0, _ := m.Alloc(KindNode)
	n1, p1, _ := m.Alloc(KindNode)
	r0, rp0, _ := m.Alloc(KindRel)
	if n0 == 0 {
		t.Fatal("element id 0 must never be valid")
	}
	if KindOf(n0) != KindNode || KindOf(r0) != KindRel {
		t.Fatal("kind not encoded in id")
	}
	if n0 == r0 {
		t.Fatal("node and relationship ids collided")
	}
	if !(n1 > n0) {
		t.Fatal("node ids must be monotone")
	}
	if p0 != 0 || p1 != 1 || rp0 != 0 {
		t.Fatalf("positions = %d,%d,%d, want 0,1,0", p0, p1, rp0)
	}
}

// TestRoundTrip allocates and deletes across kinds, commits, reopens, and checks
// the maps replayed identically.
func TestRoundTrip(t *testing.T) {
	fsys := vfs.NewMem()
	p := openPager(t, fsys)
	secs, _ := store.CreateSections(p)
	m, _ := Create(p, secs)

	var nodes []uint64
	for range 50 {
		eid, _, err := m.Alloc(KindNode)
		if err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, eid)
	}
	// Delete every third node.
	for i := 0; i < len(nodes); i += 3 {
		if err := m.Delete(nodes[i]); err != nil {
			t.Fatal(err)
		}
	}
	wantLive := m.Live()
	wantNext := m.next
	if err := p.Commit(); err != nil {
		t.Fatal(err)
	}
	p.Close()

	p2 := openPager(t, fsys)
	defer p2.Close()
	secs2, _ := store.OpenSections(p2)
	m2, err := Open(p2, secs2)
	if err != nil {
		t.Fatal(err)
	}
	if m2.Live() != wantLive {
		t.Fatalf("live = %d, want %d", m2.Live(), wantLive)
	}
	if m2.next != wantNext {
		t.Fatalf("next = %v, want %v after reopen", m2.next, wantNext)
	}
	// Deleted ids resolve to absence; survivors resolve to their position.
	for i, eid := range nodes {
		pos, ok := m2.Pos(eid)
		if i%3 == 0 {
			if ok {
				t.Fatalf("deleted node %d still resolves to %d", eid, pos)
			}
			continue
		}
		if !ok {
			t.Fatalf("live node %d lost after reopen", eid)
		}
		// Reverse agrees with forward (id consistency invariant).
		if back, ok := m2.Eid(KindNode, pos); !ok || back != eid {
			t.Fatalf("reverse[%d] = %d,%v, want %d", pos, back, ok, eid)
		}
	}
	// A fresh allocation after reopen does not reuse a past sequence.
	fresh, _, _ := m2.Alloc(KindNode)
	if _, seen := func() (uint64, bool) {
		for _, e := range nodes {
			if e == fresh {
				return e, true
			}
		}
		return 0, false
	}(); seen {
		t.Fatal("fresh allocation reused an existing element id")
	}
}

// checkInvariants asserts the id-consistency invariant (doc 04 §15.1): forward
// and reverse agree on every live element, and no two live ids share a position.
func checkInvariants(t *testing.T, m *Map, label string) {
	t.Helper()
	seenPos := make(map[uint64]uint64)
	for eid, pos := range m.fwd {
		if back, ok := m.Eid(KindOf(eid), pos); !ok || back != eid {
			t.Fatalf("%s: forward %d->%d but reverse->%d,%v", label, eid, pos, back, ok)
		}
		key := uint64(KindOf(eid))<<63 | pos
		if other, dup := seenPos[key]; dup {
			t.Fatalf("%s: position %d shared by live ids %d and %d", label, pos, other, eid)
		}
		seenPos[key] = eid
	}
}

func buildClean(t *testing.T) *vfs.Mem {
	t.Helper()
	fsys := vfs.NewMem()
	p := openPager(t, fsys)
	secs, _ := store.CreateSections(p)
	if _, err := Create(p, secs); err != nil {
		t.Fatal(err)
	}
	if err := p.Commit(); err != nil {
		t.Fatal(err)
	}
	p.Close()
	return fsys
}

// runWorkload reopens and performs T transactions, each allocating a node and a
// relationship and occasionally deleting an earlier node, one commit per step.
func runWorkload(fsys vfs.VFS, T int) (err error) {
	p, e := pager.Open(fsys, path, pager.Options{Sync: wal.SyncFull, SaltSeed: 7})
	if e != nil {
		return e
	}
	defer func() { _ = p.Close() }()
	secs, e := store.OpenSections(p)
	if e != nil {
		return e
	}
	m, e := Open(p, secs)
	if e != nil {
		return e
	}
	var nodes []uint64
	for j := 1; j <= T; j++ {
		nid, _, e := m.Alloc(KindNode)
		if e != nil {
			return e
		}
		nodes = append(nodes, nid)
		if _, _, e := m.Alloc(KindRel); e != nil {
			return e
		}
		if j%4 == 0 {
			if e := m.Delete(nodes[j/4-1]); e != nil {
				return e
			}
		}
		if e := p.Commit(); e != nil {
			return e
		}
	}
	return nil
}

// verifyDurablePrefix reopens a crashed snapshot and asserts the id-map recovered
// to an internally consistent committed prefix.
func verifyDurablePrefix(t *testing.T, crashed *vfs.Mem, label string) int {
	t.Helper()
	p, err := pager.Open(crashed, path, pager.Options{})
	if err != nil {
		t.Fatalf("%s: reopen failed: %v", label, err)
	}
	defer p.Close()
	secs, err := store.OpenSections(p)
	if err != nil {
		t.Fatalf("%s: reopen sections failed: %v", label, err)
	}
	m, err := Open(p, secs)
	if err != nil {
		t.Fatalf("%s: id-map replay failed: %v", label, err)
	}
	checkInvariants(t, m, label)
	return m.Live()
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

func TestIDMapCrashCampaignCrash(t *testing.T) { crashCampaign(t, vfs.TripCrash, "crash") }
func TestIDMapCrashCampaignTorn(t *testing.T)  { crashCampaign(t, vfs.TripTear, "torn") }
func TestIDMapCrashCampaignFsync(t *testing.T) { crashCampaign(t, vfs.TripFsyncFail, "fsync") }
