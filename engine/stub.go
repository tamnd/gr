package engine

import (
	"errors"
	"sync"

	"github.com/tamnd/gr/value"
)

// ErrReadOnlyTx is returned by mutating calls on a read transaction.
var ErrReadOnlyTx = errors.New("gr/engine: write on a read-only transaction")

// ErrNoSuchNode / ErrNoSuchRel are returned for missing elements.
var (
	ErrNoSuchNode = errors.New("gr/engine: no such node")
	ErrNoSuchRel  = errors.New("gr/engine: no such relationship")
)

// MemEngine is the M0 in-memory stub of the engine SPI (doc 25 §3.2 item 8,
// §3.7). It is intentionally naïve — a single mutex over plain maps, no MVCC, no
// durability — and exists only so the query stack can be built and tested
// against the SPI before the real storage engine (M1) lands. M1 replaces this
// with the durable, snapshot-isolated implementation behind the same interface.
type MemEngine struct {
	mu      sync.Mutex
	nodes   map[NodeID]*memNode
	rels    map[RelID]*memRel
	nextN   NodeID
	nextR   RelID
	propKey map[string]Token
	nextTok Token
	closed  bool
}

type memNode struct {
	labels map[Token]struct{}
	props  map[Token]value.Value
	out    []RelID
	in     []RelID
}

type memRel struct {
	src, dst NodeID
	typ      Token
	props    map[Token]value.Value
}

// NewMemEngine returns an empty in-memory stub engine.
func NewMemEngine() *MemEngine {
	return &MemEngine{
		nodes:   make(map[NodeID]*memNode),
		rels:    make(map[RelID]*memRel),
		nextN:   1,
		nextR:   1,
		propKey: make(map[string]Token),
		// Interned keys start well above the small tokens tests assign by hand,
		// so a run-time intern never collides with a fixture's literal token.
		nextTok: 1000,
	}
}

// Begin returns a transaction over the shared in-memory state. The stub does not
// isolate snapshots; writes are applied directly and Commit/Abort are bookkeeping.
func (e *MemEngine) Begin(write bool) (Tx, error) {
	return &memTx{e: e, write: write}, nil
}

// Checkpoint is a no-op for the in-memory stub (nothing to flush).
func (e *MemEngine) Checkpoint() error { return nil }

// Close marks the engine closed.
func (e *MemEngine) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closed = true
	return nil
}

type memTx struct {
	e     *MemEngine
	write bool
}

func (t *memTx) NodeExists(id NodeID) (bool, error) {
	t.e.mu.Lock()
	defer t.e.mu.Unlock()
	_, ok := t.e.nodes[id]
	return ok, nil
}

func (t *memTx) RelExists(id RelID) (bool, error) {
	t.e.mu.Lock()
	defer t.e.mu.Unlock()
	_, ok := t.e.rels[id]
	return ok, nil
}

func (t *memTx) NodeLabels(id NodeID) ([]Token, error) {
	t.e.mu.Lock()
	defer t.e.mu.Unlock()
	n, ok := t.e.nodes[id]
	if !ok {
		return nil, ErrNoSuchNode
	}
	out := make([]Token, 0, len(n.labels))
	for l := range n.labels {
		out = append(out, l)
	}
	return out, nil
}

func (t *memTx) HasLabel(id NodeID, label Token) (bool, error) {
	t.e.mu.Lock()
	defer t.e.mu.Unlock()
	n, ok := t.e.nodes[id]
	if !ok {
		return false, ErrNoSuchNode
	}
	_, has := n.labels[label]
	return has, nil
}

func (t *memTx) NodeProperty(id NodeID, key Token) (value.Value, error) {
	t.e.mu.Lock()
	defer t.e.mu.Unlock()
	n, ok := t.e.nodes[id]
	if !ok {
		return value.Null, ErrNoSuchNode
	}
	return n.props[key], nil
}

func (t *memTx) RelProperty(id RelID, key Token) (value.Value, error) {
	t.e.mu.Lock()
	defer t.e.mu.Unlock()
	r, ok := t.e.rels[id]
	if !ok {
		return value.Null, ErrNoSuchRel
	}
	return r.props[key], nil
}

func (t *memTx) RelType(id RelID) (Token, error) {
	t.e.mu.Lock()
	defer t.e.mu.Unlock()
	r, ok := t.e.rels[id]
	if !ok {
		return 0, ErrNoSuchRel
	}
	return r.typ, nil
}

func (t *memTx) NodePropertyKeys(id NodeID) ([]Token, error) {
	t.e.mu.Lock()
	defer t.e.mu.Unlock()
	n, ok := t.e.nodes[id]
	if !ok {
		return nil, ErrNoSuchNode
	}
	out := make([]Token, 0, len(n.props))
	for k := range n.props {
		out = append(out, k)
	}
	return out, nil
}

func (t *memTx) RelPropertyKeys(id RelID) ([]Token, error) {
	t.e.mu.Lock()
	defer t.e.mu.Unlock()
	r, ok := t.e.rels[id]
	if !ok {
		return nil, ErrNoSuchRel
	}
	out := make([]Token, 0, len(r.props))
	for k := range r.props {
		out = append(out, k)
	}
	return out, nil
}

func (t *memTx) ScanLabel(label Token, fn func(NodeID) error) error {
	t.e.mu.Lock()
	ids := make([]NodeID, 0, len(t.e.nodes))
	for id, n := range t.e.nodes {
		if label == 0 {
			ids = append(ids, id)
			continue
		}
		if _, ok := n.labels[label]; ok {
			ids = append(ids, id)
		}
	}
	t.e.mu.Unlock()
	for _, id := range ids {
		if err := fn(id); err != nil {
			return err
		}
	}
	return nil
}

func (t *memTx) Expand(id NodeID, relType Token, dir Direction, fn func(Neighbor) error) error {
	t.e.mu.Lock()
	n, ok := t.e.nodes[id]
	if !ok {
		t.e.mu.Unlock()
		return ErrNoSuchNode
	}
	var nbrs []Neighbor
	emit := func(rid RelID, incoming bool) {
		r := t.e.rels[rid]
		if r == nil {
			return
		}
		if relType != 0 && r.typ != relType {
			return
		}
		other := r.dst
		if incoming {
			other = r.src
		}
		nbrs = append(nbrs, Neighbor{Rel: rid, Node: other, Type: r.typ})
	}
	if dir == Outgoing || dir == Both {
		for _, rid := range n.out {
			emit(rid, false)
		}
	}
	if dir == Incoming || dir == Both {
		for _, rid := range n.in {
			emit(rid, true)
		}
	}
	t.e.mu.Unlock()
	for _, nb := range nbrs {
		if err := fn(nb); err != nil {
			return err
		}
	}
	return nil
}

func (t *memTx) Degree(id NodeID, relType Token, dir Direction) (int64, error) {
	var c int64
	err := t.Expand(id, relType, dir, func(Neighbor) error { c++; return nil })
	return c, err
}

func (t *memTx) CreateNode(labels []Token) (NodeID, error) {
	if !t.write {
		return 0, ErrReadOnlyTx
	}
	t.e.mu.Lock()
	defer t.e.mu.Unlock()
	id := t.e.nextN
	t.e.nextN++
	n := &memNode{labels: make(map[Token]struct{}), props: make(map[Token]value.Value)}
	for _, l := range labels {
		n.labels[l] = struct{}{}
	}
	t.e.nodes[id] = n
	return id, nil
}

func (t *memTx) DeleteNode(id NodeID) error {
	if !t.write {
		return ErrReadOnlyTx
	}
	t.e.mu.Lock()
	defer t.e.mu.Unlock()
	n, ok := t.e.nodes[id]
	if !ok {
		return ErrNoSuchNode
	}
	if len(n.out) > 0 || len(n.in) > 0 {
		return errors.New("gr/engine: cannot delete node with relationships")
	}
	delete(t.e.nodes, id)
	return nil
}

func (t *memTx) CreateRel(src, dst NodeID, relType Token) (RelID, error) {
	if !t.write {
		return 0, ErrReadOnlyTx
	}
	t.e.mu.Lock()
	defer t.e.mu.Unlock()
	sn, ok := t.e.nodes[src]
	if !ok {
		return 0, ErrNoSuchNode
	}
	dn, ok := t.e.nodes[dst]
	if !ok {
		return 0, ErrNoSuchNode
	}
	id := t.e.nextR
	t.e.nextR++
	t.e.rels[id] = &memRel{src: src, dst: dst, typ: relType, props: make(map[Token]value.Value)}
	sn.out = append(sn.out, id)
	dn.in = append(dn.in, id)
	return id, nil
}

func (t *memTx) DeleteRel(id RelID) error {
	if !t.write {
		return ErrReadOnlyTx
	}
	t.e.mu.Lock()
	defer t.e.mu.Unlock()
	r, ok := t.e.rels[id]
	if !ok {
		return ErrNoSuchRel
	}
	if sn := t.e.nodes[r.src]; sn != nil {
		sn.out = removeRel(sn.out, id)
	}
	if dn := t.e.nodes[r.dst]; dn != nil {
		dn.in = removeRel(dn.in, id)
	}
	delete(t.e.rels, id)
	return nil
}

func removeRel(s []RelID, id RelID) []RelID {
	for i, x := range s {
		if x == id {
			return append(s[:i], s[i+1:]...)
		}
	}
	return s
}

func (t *memTx) InternPropKey(name string) (Token, error) {
	if !t.write {
		return 0, ErrReadOnlyTx
	}
	t.e.mu.Lock()
	defer t.e.mu.Unlock()
	if tok, ok := t.e.propKey[name]; ok {
		return tok, nil
	}
	tok := t.e.nextTok
	t.e.nextTok++
	t.e.propKey[name] = tok
	return tok, nil
}

func (t *memTx) SetNodeProperty(id NodeID, key Token, v value.Value) error {
	if !t.write {
		return ErrReadOnlyTx
	}
	t.e.mu.Lock()
	defer t.e.mu.Unlock()
	n, ok := t.e.nodes[id]
	if !ok {
		return ErrNoSuchNode
	}
	if v.IsNull() {
		delete(n.props, key)
	} else {
		n.props[key] = v
	}
	return nil
}

func (t *memTx) SetRelProperty(id RelID, key Token, v value.Value) error {
	if !t.write {
		return ErrReadOnlyTx
	}
	t.e.mu.Lock()
	defer t.e.mu.Unlock()
	r, ok := t.e.rels[id]
	if !ok {
		return ErrNoSuchRel
	}
	if v.IsNull() {
		delete(r.props, key)
	} else {
		r.props[key] = v
	}
	return nil
}

func (t *memTx) AddLabel(id NodeID, label Token) error {
	if !t.write {
		return ErrReadOnlyTx
	}
	t.e.mu.Lock()
	defer t.e.mu.Unlock()
	n, ok := t.e.nodes[id]
	if !ok {
		return ErrNoSuchNode
	}
	n.labels[label] = struct{}{}
	return nil
}

func (t *memTx) RemoveLabel(id NodeID, label Token) error {
	if !t.write {
		return ErrReadOnlyTx
	}
	t.e.mu.Lock()
	defer t.e.mu.Unlock()
	n, ok := t.e.nodes[id]
	if !ok {
		return ErrNoSuchNode
	}
	delete(n.labels, label)
	return nil
}

func (t *memTx) Commit() error { return nil }
func (t *memTx) Abort() error  { return nil }
