package engine

import (
	"errors"
	"strconv"
	"sync"
	"testing"

	"github.com/tamnd/gr/catalog"
	"github.com/tamnd/gr/pager"
	"github.com/tamnd/gr/value"
	"github.com/tamnd/gr/vfs"
	"github.com/tamnd/gr/wal"
)

// Sentinels for the concurrent-expand test (a callback cannot call t.Fatal from a
// worker goroutine, so it returns one of these and the main goroutine reports it).
var (
	errUnexpectedNeighbor = errors.New("expand returned a neighbor not in the graph")
	errWrongDegree        = errors.New("expand returned the wrong neighbor count")
	errWrongProperty      = errors.New("read returned the wrong property value")
)

func openDisk(t *testing.T, fsys vfs.VFS, path string) *DiskEngine {
	t.Helper()
	e, err := Open(fsys, path, pager.Options{Sync: wal.SyncFull, SaltSeed: 1})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// TestDiskRoundTrip builds a small social graph through the SPI, checkpoints,
// closes, reopens, and reads back every node, label, property, and adjacency —
// the M1 programmatic round-trip gate, with no Cypher.
func TestDiskRoundTrip(t *testing.T) {
	fsys := vfs.NewMem()
	e := openDisk(t, fsys, "social.gr")

	person, _ := e.Intern(catalog.KindLabel, "Person")
	knows, _ := e.Intern(catalog.KindRelType, "KNOWS")
	name, _ := e.Intern(catalog.KindPropKey, "name")
	since, _ := e.Intern(catalog.KindPropKey, "since")

	tx, _ := e.Begin(true)
	a, _ := tx.CreateNode([]Token{person})
	b, _ := tx.CreateNode([]Token{person})
	if err := tx.SetNodeProperty(a, name, value.String("alice")); err != nil {
		t.Fatal(err)
	}
	tx.SetNodeProperty(b, name, value.String("bob"))
	r, err := tx.CreateRel(a, b, knows)
	if err != nil {
		t.Fatal(err)
	}
	tx.SetRelProperty(r, since, value.Int(2015))
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := e.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen and read everything back.
	e2 := openDisk(t, fsys, "social.gr")
	defer e2.Close()
	person2, _ := e2.Lookup(catalog.KindLabel, "Person")
	knows2, _ := e2.Lookup(catalog.KindRelType, "KNOWS")
	name2, _ := e2.Lookup(catalog.KindPropKey, "name")
	since2, _ := e2.Lookup(catalog.KindPropKey, "since")

	rx, _ := e2.Begin(false)
	defer rx.Abort()

	var people []NodeID
	if err := rx.ScanLabel(person2, func(id NodeID) error {
		people = append(people, id)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(people) != 2 {
		t.Fatalf("scan Person = %d, want 2", len(people))
	}

	// a and b survive with the same ids (ids are stable across reopen).
	if v, _ := rx.NodeProperty(a, name2); mustStr(t, v) != "alice" {
		t.Fatalf("a.name = %v", v)
	}
	if v, _ := rx.NodeProperty(b, name2); mustStr(t, v) != "bob" {
		t.Fatalf("b.name = %v", v)
	}
	if has, _ := rx.HasLabel(a, person2); !has {
		t.Fatal("a lost Person label across reopen")
	}

	// Expand a-KNOWS->b, read the edge property.
	var reached NodeID
	var edge RelID
	if err := rx.Expand(a, knows2, Outgoing, func(n Neighbor) error {
		reached, edge = n.Node, n.Rel
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if reached != b {
		t.Fatalf("expand reached %d, want %d", reached, b)
	}
	if v, _ := rx.RelProperty(edge, since2); mustInt(t, v) != 2015 {
		t.Fatalf("r.since = %v", v)
	}
	// Backward expand.
	var back NodeID
	rx.Expand(b, knows2, Incoming, func(n Neighbor) error { back = n.Node; return nil })
	if back != a {
		t.Fatalf("backward expand reached %d, want %d", back, a)
	}
}

// TestDiskAbort verifies a rolled-back write transaction leaves no trace, even
// the in-memory store state, after rebuilding from the pager.
func TestDiskAbort(t *testing.T) {
	fsys := vfs.NewMem()
	e := openDisk(t, fsys, "abort.gr")
	defer e.Close()
	person, _ := e.Intern(catalog.KindLabel, "Person")

	// Commit one node.
	tx, _ := e.Begin(true)
	a, _ := tx.CreateNode([]Token{person})
	tx.Commit()

	// Create a second node, then abort.
	tx2, _ := e.Begin(true)
	b, _ := tx2.CreateNode([]Token{person})
	if err := tx2.Abort(); err != nil {
		t.Fatal(err)
	}

	rx, _ := e.Begin(false)
	if ok, _ := rx.NodeExists(a); !ok {
		t.Fatal("committed node a vanished after abort")
	}
	if ok, _ := rx.NodeExists(b); ok {
		t.Fatal("aborted node b is visible")
	}
	rx.Abort() // release the read lock before opening a write transaction
	// The next create should reuse b's id slot (the abort rewound allocation).
	wx, _ := e.Begin(true)
	c, _ := wx.CreateNode([]Token{person})
	wx.Commit()
	if c != b {
		t.Fatalf("post-abort allocation = %d, want reused %d", c, b)
	}
}

// TestDiskConcurrentExpand drives the read stack the way morsel-parallel
// execution will: many goroutines expanding nodes at once against one read
// transaction (one snapshot). This exercises the whole concurrent read path,
// the engine's shared read lock, the buffer pool's pin and pool map, and the
// adjacency base cache that openBase fills lazily on first expand of a slot.
// Before those caches were guarded a concurrent expand of two cold slots was a
// fatal concurrent map write; here every goroutine must read the correct
// neighbor and the race detector must stay clean.
func TestDiskConcurrentExpand(t *testing.T) {
	fsys := vfs.NewMem()
	e := openDisk(t, fsys, "concurrent.gr")
	defer e.Close()

	knows, _ := e.Intern(catalog.KindRelType, "KNOWS")
	person, _ := e.Intern(catalog.KindLabel, "Person")

	// A star: one hub points at many spokes, so every expand of the hub returns
	// the same large run and the readers contend on the same cached base.
	const spokes = 64
	tx, _ := e.Begin(true)
	hub, _ := tx.CreateNode([]Token{person})
	want := map[NodeID]bool{}
	for i := 0; i < spokes; i++ {
		s, _ := tx.CreateNode([]Token{person})
		if _, err := tx.CreateRel(hub, s, knows); err != nil {
			t.Fatal(err)
		}
		want[s] = true
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := e.Checkpoint(); err != nil {
		t.Fatal(err)
	}

	rx, _ := e.Begin(false)
	defer rx.Abort()

	const workers = 16
	const iters = 200
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				var got int
				err := rx.Expand(hub, knows, Outgoing, func(n Neighbor) error {
					if !want[n.Node] {
						return errUnexpectedNeighbor
					}
					got++
					return nil
				})
				if err != nil {
					errs <- err
					return
				}
				if got != spokes {
					errs <- errWrongDegree
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

// TestDiskConcurrentNodeProperty drives the property-read stack the way
// morsel-parallel execution will: many goroutines reading node properties at
// once against one snapshot, over keys whose columns are still cold so the
// readers are the ones that open them. It reaches both lazily-filled column
// caches: keys written before the checkpoint fold into the segmented base
// (colsegstore opens them on first read), and a key written after stays in the
// naive delta (column opens it on first read). Before those caches were guarded
// a concurrent first read of a cold key was a fatal concurrent map write; here
// every goroutine must read the value it wrote and the race detector must stay
// clean.
func TestDiskConcurrentNodeProperty(t *testing.T) {
	fsys := vfs.NewMem()
	e := openDisk(t, fsys, "concprop.gr")
	defer e.Close()

	const nkeys = 5 // k0..k3 fold into the base, k4 stays in the delta
	const nnodes = 32
	keys := make([]Token, nkeys)
	for j := range keys {
		keys[j], _ = e.Intern(catalog.KindPropKey, "p"+strconv.Itoa(j))
	}

	// val is the value node i carries for key j, distinct per pair so a wrong
	// cache lookup is detectable.
	val := func(i, j int) int64 { return int64(i*100 + j) }

	tx, _ := e.Begin(true)
	nodes := make([]NodeID, nnodes)
	for i := range nodes {
		n, _ := tx.CreateNode(nil)
		nodes[i] = n
		for j := 0; j < nkeys-1; j++ { // all but the last key, pre-checkpoint
			if err := tx.SetNodeProperty(n, keys[j], value.Int(val(i, j))); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := e.Checkpoint(); err != nil { // folds k0..k3 into the segmented base
		t.Fatal(err)
	}

	// The last key is written after the checkpoint, so it lives in the naive delta
	// and its reads exercise the other lazy cache.
	tx2, _ := e.Begin(true)
	for i, n := range nodes {
		if err := tx2.SetNodeProperty(n, keys[nkeys-1], value.Int(val(i, nkeys-1))); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx2.Commit(); err != nil {
		t.Fatal(err)
	}

	rx, _ := e.Begin(false)
	defer rx.Abort()

	const workers = 16
	const iters = 200
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for it := 0; it < iters; it++ {
				i := (w*iters + it) % nnodes
				j := it % nkeys
				v, err := rx.NodeProperty(nodes[i], keys[j])
				if err != nil {
					errs <- err
					return
				}
				iv, ok := v.AsInt()
				if !ok || iv != val(i, j) {
					errs <- errWrongProperty
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

func TestDiskReadOnlyRejectsWrites(t *testing.T) {
	fsys := vfs.NewMem()
	e := openDisk(t, fsys, "ro.gr")
	defer e.Close()
	tx, _ := e.Begin(false)
	if _, err := tx.CreateNode(nil); err != ErrReadOnlyTx {
		t.Fatalf("want ErrReadOnlyTx, got %v", err)
	}
	tx.Abort()
}

// --- crash campaign over the SPI ---

const cpath = "engine_crash.gr"

func buildClean(t *testing.T) *vfs.Mem {
	t.Helper()
	fsys := vfs.NewMem()
	e, err := Open(fsys, cpath, pager.Options{Sync: wal.SyncFull, SaltSeed: 1})
	if err != nil {
		t.Fatal(err)
	}
	// Intern the schema in the clean state so the workload only mutates the graph.
	e.Intern(catalog.KindLabel, "Person")
	e.Intern(catalog.KindRelType, "KNOWS")
	e.Intern(catalog.KindPropKey, "name")
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	return fsys
}

// runWorkload builds a path graph through the SPI, one node and (from step 1) one
// edge per committed transaction, with a checkpoint partway, so faults land at
// every commit and fsync boundary including during the checkpoint.
func runWorkload(fsys vfs.VFS, T int) (err error) {
	e, e2 := Open(fsys, cpath, pager.Options{Sync: wal.SyncFull, SaltSeed: 7})
	if e2 != nil {
		return e2
	}
	defer func() { _ = e.Close() }()
	person, _ := e.Lookup(catalog.KindLabel, "Person")
	knows, _ := e.Lookup(catalog.KindRelType, "KNOWS")
	name, _ := e.Lookup(catalog.KindPropKey, "name")

	var prev NodeID
	for j := range T {
		tx, err := e.Begin(true)
		if err != nil {
			return err
		}
		n, err := tx.CreateNode([]Token{person})
		if err != nil {
			return err
		}
		if err := tx.SetNodeProperty(n, name, value.Int(int64(j))); err != nil {
			return err
		}
		if j > 0 {
			if _, err := tx.CreateRel(n, prev, knows); err != nil {
				return err
			}
		}
		prev = n
		if err := tx.Commit(); err != nil {
			return err
		}
		if j == T/2 {
			if err := e.Checkpoint(); err != nil {
				return err
			}
		}
	}
	return nil
}

// verifyConsistent reopens a crashed snapshot and asserts internal consistency
// through the SPI: every visible node is readable, and every edge an expand
// yields lands on nodes that exist (no dangling), in both directions.
func verifyConsistent(t *testing.T, crashed *vfs.Mem, label string) {
	t.Helper()
	e, err := Open(crashed, cpath, pager.Options{})
	if err != nil {
		t.Fatalf("%s: reopen: %v", label, err)
	}
	defer e.Close()
	person, _ := e.Lookup(catalog.KindLabel, "Person")
	knows, _ := e.Lookup(catalog.KindRelType, "KNOWS")
	name, _ := e.Lookup(catalog.KindPropKey, "name")

	rx, _ := e.Begin(false)
	defer rx.Abort()

	var people []NodeID
	if err := rx.ScanLabel(person, func(id NodeID) error {
		people = append(people, id)
		return nil
	}); err != nil {
		t.Fatalf("%s: scan: %v", label, err)
	}
	for _, id := range people {
		if ok, _ := rx.NodeExists(id); !ok {
			t.Fatalf("%s: scanned node %d does not exist", label, id)
		}
		if _, err := rx.NodeProperty(id, name); err != nil {
			t.Fatalf("%s: node %d prop: %v", label, id, err)
		}
		check := func(dir Direction) {
			if err := rx.Expand(id, knows, dir, func(n Neighbor) error {
				if ok, _ := rx.NodeExists(n.Node); !ok {
					t.Fatalf("%s: dangling edge from %d to absent node %d", label, id, n.Node)
				}
				return nil
			}); err != nil {
				t.Fatalf("%s: expand %d: %v", label, id, err)
			}
		}
		check(Outgoing)
		check(Incoming)
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
		verifyConsistent(t, fs.Snapshot(), label)
	}
}

func TestDiskCrashCampaignCrash(t *testing.T) { crashCampaign(t, vfs.TripCrash, "crash") }
func TestDiskCrashCampaignTorn(t *testing.T)  { crashCampaign(t, vfs.TripTear, "torn") }
func TestDiskCrashCampaignFsync(t *testing.T) { crashCampaign(t, vfs.TripFsyncFail, "fsync") }

func mustStr(t *testing.T, v value.Value) string {
	t.Helper()
	s, ok := v.AsString()
	if !ok {
		t.Fatalf("not a string: %v", v)
	}
	return s
}

func mustInt(t *testing.T, v value.Value) int64 {
	t.Helper()
	i, ok := v.AsInt()
	if !ok {
		t.Fatalf("not an int: %v", v)
	}
	return i
}
