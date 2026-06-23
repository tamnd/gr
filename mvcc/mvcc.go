// Package mvcc realizes graph-element multi-version concurrency control for the
// storage engine in its single-writer-first form (doc 06; doc 25 §4 deliverable
// 9). It provides the three pieces the engine composes into snapshot isolation:
//
//   - the [Oracle], the global commit-sequence clock and the watermark over live
//     snapshots that governs version reclamation (doc 06 §2.1, §4);
//   - the [Snapshot], a captured read sequence that defines a transaction's
//     stable, point-in-time view (doc 06 §2.2);
//   - the [Overlay], an in-memory retention of pre-images that lets a snapshot
//     resolve a datum as of its read sequence even after a later writer has
//     overwritten the durable base (doc 06 §2.3; doc 04 §11).
//
// The model here is retention of superseded values: the durable base stores
// always hold the latest committed state (so recovery needs nothing extra and a
// fresh open with no live readers reads straight through), and the overlay keeps
// the value a datum had *before* each committed write, tagged with the sequence
// at which the new value took effect. A reader at read sequence r resolves a
// datum by finding the earliest committed write with sequence greater than r and
// returning the pre-image it saved — the value as of r — falling back to the
// base when no such write exists. The watermark bounds how long a pre-image must
// be kept: once no live snapshot can read below a write's sequence, its
// pre-image is dropped.
//
// Single-writer-first means one writer at a time creates versions, so version
// creation is serialized and conflict-free; the concurrent-writer growth path
// (doc 06 §6) adds only a commit-time disjointness check behind this same model
// and is not part of M1.
package mvcc

import (
	"sync"
	"time"

	"github.com/tamnd/gr/value"
)

// Seq is a commit sequence number, the monotonic global clock of the version
// model (doc 06 §2.1). The engine derives it from the pager's durable change
// counter, so it survives recovery and stays monotonic across reopens.
type Seq = uint64

// Kind names a versioned datum's category, so a [Key] is unambiguous across the
// node and relationship stores and their property columns.
type Kind uint8

const (
	// NodeExist versions a node's existence (created or deleted).
	NodeExist Kind = iota
	// NodeLabels versions a node's label set.
	NodeLabels
	// NodeProp versions one node property value (keyed by property token in Sub).
	NodeProp
	// RelExist versions a relationship's existence; the adjacency reads this to
	// decide whether an edge is visible to a snapshot.
	RelExist
	// RelProp versions one relationship property value (keyed by token in Sub).
	RelProp
)

// isRel reports whether a kind versions a relationship datum rather than a node datum, so GC can
// split its reclaim count by element (doc 20 §5.1).
func (k Kind) isRel() bool { return k == RelExist || k == RelProp }

// Key identifies a versioned datum: its kind, the dense position of the element
// it belongs to, and a sub-key (a property token for the property kinds, zero
// otherwise).
type Key struct {
	Kind Kind
	Pos  uint64
	Sub  uint32
}

// Pre is a datum's pre-image: the value it held before a write, retained so an
// older snapshot can resolve it. Which fields are meaningful depends on the
// key's kind: Present alone for the existence kinds, Present plus Val for the
// property kinds, Labels for NodeLabels.
type Pre struct {
	Present bool
	Val     value.Value
	Labels  []uint32
}

// --- the oracle ---

// Oracle is the commit-sequence clock and the watermark oracle (doc 06 §2.1,
// §4). It hands read sequences to snapshots, advances the commit sequence as
// writers commit, and reports the watermark — the oldest read sequence any live
// snapshot holds — below which superseded versions can be reclaimed.
type Oracle struct {
	mu   sync.Mutex
	seq  Seq
	next uint64
	live map[uint64]liveSnap // snapshot id -> live snapshot record
}

// liveSnap records a live snapshot's read sequence and the wall-clock instant it began, so the oracle
// can report both the watermark (over read sequences) and the oldest snapshot's age (over begin times).
type liveSnap struct {
	read  Seq
	begin time.Time
}

// NewOracle starts the clock at seq, the last durably committed sequence (the
// engine passes the recovered change counter, so the clock continues monotonically).
func NewOracle(seq Seq) *Oracle {
	return &Oracle{seq: seq, live: map[uint64]liveSnap{}}
}

// Seq returns the current commit sequence.
func (o *Oracle) Seq() Seq {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.seq
}

// SetSeq advances the commit sequence to s after a durable commit. It never goes
// backwards (a commit that bumped the durable counter only moves it forward).
func (o *Oracle) SetSeq(s Seq) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if s > o.seq {
		o.seq = s
	}
}

// Begin registers a new snapshot at the current commit sequence and returns its
// id and read sequence. The id is passed back to End to deregister.
func (o *Oracle) Begin() (id uint64, read Seq) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.next++
	id = o.next
	o.live[id] = liveSnap{read: o.seq, begin: time.Now()}
	return id, o.seq
}

// End deregisters a snapshot, allowing the watermark to advance past it.
func (o *Oracle) End(id uint64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.live, id)
}

// Watermark returns the oldest read sequence among live snapshots, or the
// current commit sequence if none are live (nothing to retain). A version
// superseded at sequence s is needed only by snapshots whose read sequence is
// below s, so once the watermark reaches s its pre-image can be dropped.
func (o *Oracle) Watermark() Seq {
	o.mu.Lock()
	defer o.mu.Unlock()
	wm := o.seq
	for _, s := range o.live {
		if s.read < wm {
			wm = s.read
		}
	}
	return wm
}

// WatermarkLag returns the commit versions between the current commit sequence and the GC
// watermark (doc 20 §5.1): the reclaimable backlog, the versions GC could drop if the oldest live
// snapshot released. It is computed under one lock so the sequence and the watermark are consistent,
// and the watermark never exceeds the sequence, so the result never underflows.
func (o *Oracle) WatermarkLag() uint64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	wm := o.seq
	for _, s := range o.live {
		if s.read < wm {
			wm = s.read
		}
	}
	return o.seq - wm
}

// OldestSnapshotAge returns the wall-clock age of the oldest live snapshot, or zero when none is live
// (doc 20 §5.1). It is the long-reader signal in time rather than in versions: a snapshot whose age
// keeps climbing is pinning the watermark and bloating the version store, and a reader left open across
// a maintenance window shows here as a steadily rising age. Begin stamps each snapshot's start, so this
// is the gap between now and the earliest such stamp still live.
func (o *Oracle) OldestSnapshotAge() time.Duration {
	o.mu.Lock()
	defer o.mu.Unlock()
	var oldest time.Time
	for _, s := range o.live {
		if oldest.IsZero() || s.begin.Before(oldest) {
			oldest = s.begin
		}
	}
	if oldest.IsZero() {
		return 0
	}
	return time.Since(oldest)
}

// --- the retention overlay ---

type entry struct {
	seq Seq
	pre Pre
}

// Overlay retains pre-images of superseded datums so older snapshots resolve the
// value as of their read sequence (doc 04 §11.3). It is purely in-memory: it
// holds only versions superseded since the base last reflected them, and it
// starts empty after a crash (a fresh open has no live old snapshots, so the
// base is the whole truth).
type Overlay struct {
	mu     sync.RWMutex
	chains map[Key][]entry // each chain ascending by seq
}

// NewOverlay returns an empty overlay.
func NewOverlay() *Overlay {
	return &Overlay{chains: map[Key][]entry{}}
}

// Record saves a datum's pre-image, tagged with the sequence at which the new
// value took effect. Commits are monotonic, so the per-key chain stays ascending
// by sequence with a plain append.
func (ov *Overlay) Record(seq Seq, key Key, pre Pre) {
	ov.mu.Lock()
	defer ov.mu.Unlock()
	ov.chains[key] = append(ov.chains[key], entry{seq: seq, pre: pre})
}

// Resolve returns the value of a datum as of read sequence r: the pre-image of
// the earliest committed write whose sequence is greater than r (that write
// replaced the value the snapshot should see). ok is false when no retained
// write supersedes the base for this snapshot, meaning the caller reads the base.
func (ov *Overlay) Resolve(key Key, r Seq) (Pre, bool) {
	ov.mu.RLock()
	defer ov.mu.RUnlock()
	for _, e := range ov.chains[key] {
		if e.seq > r {
			return e.pre, true
		}
	}
	return Pre{}, false
}

// NodeCandidates returns the dense positions whose snapshot view at read sequence
// r may differ from the base for a node's existence, its label set, or the given
// property key: every position with a retained pre-image (of one of those kinds)
// tagged after r. A base-built property index reflects the latest committed state,
// so a snapshot reader can miss a node whose indexed value, label, or existence
// changed after r; these candidates are exactly the positions a snapshot-correct
// lookup must reconsider beyond the base index (doc 07 §9). The returned positions
// are deduplicated; the caller filters them against the actual snapshot value.
func (ov *Overlay) NodeCandidates(propKey uint32, r Seq) []uint64 {
	ov.mu.RLock()
	defer ov.mu.RUnlock()
	var out []uint64
	seen := make(map[uint64]struct{})
	for k, ch := range ov.chains {
		if k.Kind != NodeExist && k.Kind != NodeLabels && !(k.Kind == NodeProp && k.Sub == propKey) {
			continue
		}
		relevant := false
		for _, e := range ch {
			if e.seq > r {
				relevant = true
				break
			}
		}
		if !relevant {
			continue
		}
		if _, dup := seen[k.Pos]; dup {
			continue
		}
		seen[k.Pos] = struct{}{}
		out = append(out, k.Pos)
	}
	return out
}

// GCStats reports what one GC pass reclaimed (doc 20 §5.1): the pre-images dropped, split by
// element so the version-reclaim throughput is readable per node and rel.
type GCStats struct {
	Node uint64
	Rel  uint64
}

// GC drops pre-images no live snapshot can need: an entry tagged seq serves
// snapshots whose read sequence is below seq, so once the watermark reaches seq
// it is reclaimable (doc 06 §4.3). It returns what it reclaimed, split by element.
func (ov *Overlay) GC(watermark Seq) GCStats {
	ov.mu.Lock()
	defer ov.mu.Unlock()
	var st GCStats
	for k, ch := range ov.chains {
		kept := ch[:0]
		for _, e := range ch {
			if e.seq > watermark {
				kept = append(kept, e)
			}
		}
		if dropped := uint64(len(ch) - len(kept)); dropped > 0 {
			if k.Kind.isRel() {
				st.Rel += dropped
			} else {
				st.Node += dropped
			}
		}
		if len(kept) == 0 {
			delete(ov.chains, k)
		} else {
			ov.chains[k] = kept
		}
	}
	return st
}

// ChainLengths reports the length of every retained version chain, split by element (doc 20 §5.1):
// node-element chains (existence, labels, properties) and rel-element chains (existence, properties).
// It is a point-in-time read for the version-chain-length histogram, taken under the overlay's own
// read lock, never the engine lock, so a metrics scrape never waits on a write transaction. A deep
// chain here is a hot element carrying history GC has not reclaimed, the long-reader signature.
func (ov *Overlay) ChainLengths() (node, rel []float64) {
	ov.mu.RLock()
	defer ov.mu.RUnlock()
	for k, ch := range ov.chains {
		if k.Kind.isRel() {
			rel = append(rel, float64(len(ch)))
		} else {
			node = append(node, float64(len(ch)))
		}
	}
	return node, rel
}

// Len reports the number of retained pre-images, for tests and observability.
func (ov *Overlay) Len() int {
	ov.mu.RLock()
	defer ov.mu.RUnlock()
	var n int
	for _, ch := range ov.chains {
		n += len(ch)
	}
	return n
}
