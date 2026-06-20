package rel_test

import (
	"testing"

	"github.com/tamnd/gr/node"
	"github.com/tamnd/gr/pager"
	"github.com/tamnd/gr/rel"
	"github.com/tamnd/gr/store"
	"github.com/tamnd/gr/vfs"
	"github.com/tamnd/gr/wal"
)

const path = "rel.gr"

func openPager(t *testing.T, fsys vfs.VFS) *pager.Pager {
	t.Helper()
	p, err := pager.Open(fsys, path, pager.Options{Sync: wal.SyncFull, SaltSeed: 1})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRelRoundTrip(t *testing.T) {
	fsys := vfs.NewMem()
	p := openPager(t, fsys)
	secs, _ := store.CreateSections(p)
	s, err := rel.Create(p, secs)
	if err != nil {
		t.Fatal(err)
	}
	r0, _ := s.Create(1, 10, 20)
	r1, _ := s.Create(2, 20, 30)
	if r0 != 0 || r1 != 1 {
		t.Fatalf("positions = %d,%d", r0, r1)
	}
	if err := s.Delete(r0); err != nil {
		t.Fatal(err)
	}
	if err := p.Commit(); err != nil {
		t.Fatal(err)
	}
	p.Close()

	p2 := openPager(t, fsys)
	defer p2.Close()
	secs2, _ := store.OpenSections(p2)
	s2, err := rel.Open(p2, secs2)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s2.Get(r1)
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != 2 || got.Src != 20 || got.Dst != 30 {
		t.Fatalf("rel r1 = %+v after reopen", got)
	}
	if s2.Exists(r0) {
		t.Fatal("deleted rel still exists after reopen")
	}
	if _, err := s2.Get(r0); err != rel.ErrNoSuchRel {
		t.Fatalf("Get on deleted = %v, want ErrNoSuchRel", err)
	}
}

// --- combined graph crash campaign over the node and relationship stores ---

const gpath = "graph.gr"

func buildClean(t *testing.T) *vfs.Mem {
	t.Helper()
	fsys := vfs.NewMem()
	p, err := pager.Open(fsys, gpath, pager.Options{Sync: wal.SyncFull, SaltSeed: 1})
	if err != nil {
		t.Fatal(err)
	}
	secs, _ := store.CreateSections(p)
	if _, err := node.Create(p, secs); err != nil {
		t.Fatal(err)
	}
	if _, err := rel.Create(p, secs); err != nil {
		t.Fatal(err)
	}
	if err := p.Commit(); err != nil {
		t.Fatal(err)
	}
	p.Close()
	return fsys
}

// runWorkload reopens and builds a small graph: each step creates a node, and
// from the second step on creates an edge from the new node to the previous one,
// one commit per step.
func runWorkload(fsys vfs.VFS, T int) (err error) {
	p, e := pager.Open(fsys, gpath, pager.Options{Sync: wal.SyncFull, SaltSeed: 7})
	if e != nil {
		return e
	}
	defer func() { _ = p.Close() }()
	secs, e := store.OpenSections(p)
	if e != nil {
		return e
	}
	ns, e := node.Open(p, secs)
	if e != nil {
		return e
	}
	rs, e := rel.Open(p, secs)
	if e != nil {
		return e
	}
	var prev uint64
	for j := range T {
		pos, e := ns.Create([]uint32{uint32(j%3 + 1)})
		if e != nil {
			return e
		}
		if j > 0 {
			if _, e := rs.Create(1, pos, prev); e != nil {
				return e
			}
		}
		prev = pos
		if e := p.Commit(); e != nil {
			return e
		}
	}
	return nil
}

// verifyDurablePrefix reopens a crashed snapshot and asserts the no-dangling
// invariant (doc 04 §15.1): every live relationship's endpoints are live nodes.
// A torn record or a relationship committed without its endpoints would break it.
func verifyDurablePrefix(t *testing.T, crashed *vfs.Mem, label string) {
	t.Helper()
	p, err := pager.Open(crashed, gpath, pager.Options{})
	if err != nil {
		t.Fatalf("%s: reopen failed: %v", label, err)
	}
	defer p.Close()
	secs, err := store.OpenSections(p)
	if err != nil {
		t.Fatalf("%s: reopen sections failed: %v", label, err)
	}
	ns, err := node.Open(p, secs)
	if err != nil {
		t.Fatalf("%s: node reopen failed: %v", label, err)
	}
	rs, err := rel.Open(p, secs)
	if err != nil {
		t.Fatalf("%s: rel reopen failed: %v", label, err)
	}
	for pos := range uint64(rs.Count()) {
		if !rs.Exists(pos) {
			continue
		}
		r, err := rs.Get(pos)
		if err != nil {
			t.Fatalf("%s: live rel %d unreadable: %v", label, pos, err)
		}
		if !ns.Exists(r.Src) || !ns.Exists(r.Dst) {
			t.Fatalf("%s: dangling rel %d (src %d live=%v, dst %d live=%v)",
				label, pos, r.Src, ns.Exists(r.Src), r.Dst, ns.Exists(r.Dst))
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

func TestGraphCrashCampaignCrash(t *testing.T) { crashCampaign(t, vfs.TripCrash, "crash") }
func TestGraphCrashCampaignTorn(t *testing.T)  { crashCampaign(t, vfs.TripTear, "torn") }
func TestGraphCrashCampaignFsync(t *testing.T) { crashCampaign(t, vfs.TripFsyncFail, "fsync") }
