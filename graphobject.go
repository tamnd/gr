package gr

import (
	"sort"
	"strconv"

	"github.com/tamnd/gr/catalog"
	"github.com/tamnd/gr/engine"
	"github.com/tamnd/gr/value"
)

// The structural values Node, Relationship, and Path are the typed Go objects a
// query returns when it projects a node, a relationship, or a path (doc 16 §10.1).
// They are opaque read-only views of the snapshot's graph: a node carries its
// identity and labels, a relationship its identity, type, and endpoint ids, and
// each carries its properties eagerly or lazily per the materialization rule
// (§10.6). The accessors below are the whole surface; the fields are unexported so
// a program reads a node through Labels/Props/Get, never by reaching into it.

// Node is a node value read from a result (doc 16 §10.2). It carries the node's
// identity and labels (always eagerly materialized) and its properties (eagerly by
// default, lazily under WithLazyProperties). It is a small value type, cheap to
// copy; the heavy state (the property map) is shared, not copied per value.
type Node struct {
	id     uint64
	labels []string
	props  map[string]Value
	loaded bool
	mat    *objectMaterializer
}

// ElementId returns the node's stable external element id (doc 16 §10.2, doc 02
// §5.2). It is the identifier a program stores to refer to the node later and
// passes back through tx.NodeByElementId; it is opaque and encodes the element
// kind, so a node id never equals a relationship id.
func (n Node) ElementId() string { return encodeElementID(elemNode, n.id) }

// Labels returns the node's labels, in the catalog's stable order (doc 16 §10.2). A
// node with no labels returns an empty slice.
func (n Node) Labels() []string {
	if n.labels == nil {
		return []string{}
	}
	return n.labels
}

// HasLabel reports whether the node carries the label, answered from the
// eagerly-materialized label set with no property read (doc 16 §10.2).
func (n Node) HasLabel(label string) bool {
	for _, l := range n.labels {
		if l == label {
			return true
		}
	}
	return false
}

// Props returns all of the node's properties (doc 16 §10.2). An absent property is
// simply not a key. It triggers the lazy fetch when the node was returned without
// materialized properties (doc 16 §10.6).
func (n Node) Props() map[string]Value {
	if n.loaded {
		return nonNilProps(n.props)
	}
	if n.mat == nil {
		return map[string]Value{}
	}
	return n.mat.nodeProps(n.id)
}

// Get returns one property by name, with ok=false if the property is absent (doc 16
// §10.2). It is the cheap way to read one property without materializing the whole
// map under lazy materialization.
func (n Node) Get(key string) (Value, bool) {
	if n.loaded {
		v, ok := n.props[key]
		return v, ok
	}
	if n.mat == nil {
		return nil, false
	}
	return n.mat.nodeGet(n.id, key)
}

// Keys returns the names of the node's present properties, sorted (doc 16 §10.2).
func (n Node) Keys() []string { return keysOf(n.Props()) }

// Equal reports whether two nodes are the same node, by element id (doc 16 §10.2,
// doc 02 §7.2).
func (n Node) Equal(o Node) bool { return n.id == o.id }

// Relationship is a relationship value read from a result (doc 16 §10.3). It carries
// its identity, single type, and endpoint ids (eagerly), and its properties (eagerly
// by default, lazily under WithLazyProperties). The endpoints are ids, not Node
// objects, so returning the edges of a traversal does not force materializing every
// endpoint node (doc 16 §10.3).
type Relationship struct {
	id      uint64
	relType string
	startID engine.NodeID
	endID   engine.NodeID
	props   map[string]Value
	loaded  bool
	mat     *objectMaterializer
}

// ElementId returns the relationship's stable external element id (doc 16 §10.3),
// drawn from a space distinct from node ids.
func (r Relationship) ElementId() string { return encodeElementID(elemRel, r.id) }

// Type returns the relationship's single type (doc 16 §10.3). A relationship always
// has exactly one type, so this is a string, not a slice.
func (r Relationship) Type() string { return r.relType }

// StartElementId returns the start node's element id, preserving the relationship's
// direction (doc 16 §10.3). For a self-loop it equals EndElementId.
func (r Relationship) StartElementId() string { return encodeElementID(elemNode, uint64(r.startID)) }

// EndElementId returns the end node's element id (doc 16 §10.3).
func (r Relationship) EndElementId() string { return encodeElementID(elemNode, uint64(r.endID)) }

// Props returns all of the relationship's properties, with the same lazy-fetch
// behavior as a node (doc 16 §10.3, §10.6).
func (r Relationship) Props() map[string]Value {
	if r.loaded {
		return nonNilProps(r.props)
	}
	if r.mat == nil {
		return map[string]Value{}
	}
	return r.mat.relProps(r.id)
}

// Get returns one relationship property by name, ok=false if absent (doc 16 §10.3).
func (r Relationship) Get(key string) (Value, bool) {
	if r.loaded {
		v, ok := r.props[key]
		return v, ok
	}
	if r.mat == nil {
		return nil, false
	}
	return r.mat.relGet(r.id, key)
}

// Keys returns the names of the relationship's present properties, sorted (doc 16
// §10.3).
func (r Relationship) Keys() []string { return keysOf(r.Props()) }

// Equal reports whether two relationships are the same, by element id (doc 16 §10.3).
func (r Relationship) Equal(o Relationship) bool { return r.id == o.id }

// Path is a path value: an alternating sequence of nodes and relationships, node
// first (doc 16 §10.4). It is what a variable-length pattern or a shortestPath
// returns; a program walks it by zipping Nodes and Relationships.
type Path struct {
	nodes []Node
	rels  []Relationship
}

// Nodes returns the path's nodes in traversal order, Length()+1 of them (doc 16
// §10.4).
func (p Path) Nodes() []Node { return p.nodes }

// Relationships returns the path's relationships in traversal order, Length() of
// them (doc 16 §10.4).
func (p Path) Relationships() []Relationship { return p.rels }

// Length returns the path's hop count: its number of relationships (doc 16 §10.4).
func (p Path) Length() int { return len(p.rels) }

// Start returns the path's first node (doc 16 §10.4); the zero Node for an empty
// path.
func (p Path) Start() Node {
	if len(p.nodes) == 0 {
		return Node{}
	}
	return p.nodes[0]
}

// End returns the path's last node (doc 16 §10.4); the zero Node for an empty path.
func (p Path) End() Node {
	if len(p.nodes) == 0 {
		return Node{}
	}
	return p.nodes[len(p.nodes)-1]
}

// Equal reports path equality as sequence equality: the same nodes and
// relationships in the same order (doc 16 §10.4, doc 02 §7.2).
func (p Path) Equal(o Path) bool {
	if len(p.nodes) != len(o.nodes) || len(p.rels) != len(o.rels) {
		return false
	}
	for i := range p.nodes {
		if !p.nodes[i].Equal(o.nodes[i]) {
			return false
		}
	}
	for i := range p.rels {
		if !p.rels[i].Equal(o.rels[i]) {
			return false
		}
	}
	return true
}

// Entity is the property-and-identity view a Node and a Relationship share (doc 16
// §10.5), so generic code reads properties from "a node or a relationship" without
// caring which. Both Node and Relationship satisfy it.
type Entity interface {
	ElementId() string
	Props() map[string]Value
	Get(key string) (Value, bool)
	Keys() []string
}

// The element-id string format (doc 02 §5.2) is a one-byte kind tag ('n' for a
// node, 'r' for a relationship) followed by the decimal id. It is opaque to a
// program, which treats it as an identifier, but the kind tag keeps a node id and a
// relationship id distinct and lets tx.NodeByElementId / RelationshipByElementId
// route a stored id back to the right element kind.
const (
	elemNode = 'n'
	elemRel  = 'r'
)

func encodeElementID(kind byte, id uint64) string {
	return string(kind) + strconv.FormatUint(id, 10)
}

// decodeElementID parses an element id back to its kind tag and raw id. A string
// that is not a well-formed element id is ErrNotFound, the same outcome as an id
// that resolves to no element, so a caller need not distinguish a malformed id from
// a deleted one (doc 16 §10.7).
func decodeElementID(s string) (byte, uint64, error) {
	if len(s) < 2 {
		return 0, 0, ErrNotFound
	}
	kind := s[0]
	if kind != elemNode && kind != elemRel {
		return 0, 0, ErrNotFound
	}
	id, err := strconv.ParseUint(s[1:], 10, 64)
	if err != nil {
		return 0, 0, ErrNotFound
	}
	return kind, id, nil
}

// objectMaterializer turns a value.Value graph handle (a node or relationship id)
// into a self-describing Node/Relationship, reading the structural attributes and,
// in eager mode, the properties from a transaction's snapshot (doc 16 §10.2, §10.3,
// §10.6). It holds the snapshot to read from and the reverse token namers that turn
// catalog tokens into names. A nil materializer (or one with no snapshot) builds a
// bare handle carrying only the id, for the contexts where there is no snapshot to
// read (a parameter round-trip, an EXPLAIN listing).
type objectMaterializer struct {
	tx          engine.Tx
	labelName   func(engine.Token) (string, bool)
	relTypeName func(engine.Token) (string, bool)
	propKeyName func(engine.Token) (string, bool)
	lazy        bool
}

// fromValue converts an internal Cypher value to the Go value a record hands out
// (doc 16 §9.2, §9.3): null to nil, the scalars to their Go types, a list to []Value
// and a map to map[string]Value (recursively), and the graph types to materialized
// Node/Relationship/Path. A nil receiver is safe and produces bare graph handles.
func (m *objectMaterializer) fromValue(v value.Value) Value {
	switch v.Type() {
	case value.TypeNull:
		return nil
	case value.TypeBool:
		b, _ := v.AsBool()
		return b
	case value.TypeInt:
		i, _ := v.AsInt()
		return i
	case value.TypeFloat:
		f, _ := v.AsFloat()
		return f
	case value.TypeString:
		s, _ := v.AsString()
		return s
	case value.TypeBytes:
		b, _ := v.AsBytes()
		return b
	case value.TypeList:
		l, _ := v.AsList()
		out := make([]Value, len(l))
		for i, e := range l {
			out[i] = m.fromValue(e)
		}
		return out
	case value.TypeMap:
		mp, _ := v.AsMap()
		out := make(map[string]Value, len(mp))
		for k, e := range mp {
			out[k] = m.fromValue(e)
		}
		return out
	case value.TypeNode:
		id, _ := v.AsNode()
		return m.node(id)
	case value.TypeRel:
		id, _ := v.AsRel()
		return m.rel(id)
	case value.TypePath:
		el, _ := v.AsPath()
		return m.path(el)
	default:
		return nil
	}
}

// node builds a Node from a node id, reading its labels eagerly and its properties
// eagerly unless lazy materialization is in effect (doc 16 §10.6).
func (m *objectMaterializer) node(id uint64) Node {
	n := Node{id: id, mat: m}
	if m == nil || m.tx == nil {
		return n
	}
	n.labels = m.nodeLabels(id)
	if !m.lazy {
		n.props = m.nodeProps(id)
		n.loaded = true
	}
	return n
}

// rel builds a Relationship from a relationship id, reading its type and endpoint
// ids eagerly and its properties eagerly unless lazy (doc 16 §10.3, §10.6).
func (m *objectMaterializer) rel(id uint64) Relationship {
	r := Relationship{id: id, mat: m}
	if m == nil || m.tx == nil {
		return r
	}
	r.relType = m.relTypeOf(id)
	if src, dst, err := m.tx.RelEndpoints(engine.RelID(id)); err == nil {
		r.startID, r.endID = src, dst
	}
	if !m.lazy {
		r.props = m.relProps(id)
		r.loaded = true
	}
	return r
}

// path builds a Path from the executor's alternating element sequence: even
// positions are nodes, odd positions relationships (doc 16 §10.4).
func (m *objectMaterializer) path(elems []value.Value) Path {
	var p Path
	for i, e := range elems {
		if i%2 == 0 {
			id, _ := e.AsNode()
			p.nodes = append(p.nodes, m.node(id))
		} else {
			id, _ := e.AsRel()
			p.rels = append(p.rels, m.rel(id))
		}
	}
	return p
}

// nodeLabels reads a node's labels and names them, preserving the storage order
// (which is deterministic), dropping any token the catalog cannot name.
func (m *objectMaterializer) nodeLabels(id uint64) []string {
	toks, err := m.tx.NodeLabels(engine.NodeID(id))
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(toks))
	for _, t := range toks {
		if name, ok := m.labelName(t); ok {
			out = append(out, name)
		}
	}
	return out
}

// nodeProps reads every property a node carries under the snapshot into a name-keyed
// map (doc 16 §10.6). It is best-effort: a read error or a token the catalog cannot
// name is skipped rather than failing the whole object, so a node that vanished from
// the snapshot reads as a node with no properties rather than an error.
func (m *objectMaterializer) nodeProps(id uint64) map[string]Value {
	out := map[string]Value{}
	toks, err := m.tx.NodePropertyKeys(engine.NodeID(id))
	if err != nil {
		return out
	}
	for _, t := range toks {
		name, ok := m.propKeyName(t)
		if !ok {
			continue
		}
		v, err := m.tx.NodeProperty(engine.NodeID(id), t)
		if err != nil || v.IsNull() {
			continue
		}
		out[name] = m.fromValue(v)
	}
	return out
}

// nodeGet reads one node property by name, resolving the key to its token first; an
// unknown key or an absent value is ok=false (doc 16 §10.2).
func (m *objectMaterializer) nodeGet(id uint64, key string) (Value, bool) {
	if m == nil || m.tx == nil {
		return nil, false
	}
	tok, ok := m.tx.Lookup(catalog.KindPropKey, key)
	if !ok {
		return nil, false
	}
	v, err := m.tx.NodeProperty(engine.NodeID(id), tok)
	if err != nil || v.IsNull() {
		return nil, false
	}
	return m.fromValue(v), true
}

// relTypeOf reads a relationship's type and names it; the empty string if it cannot
// be read or named.
func (m *objectMaterializer) relTypeOf(id uint64) string {
	tok, err := m.tx.RelType(engine.RelID(id))
	if err != nil {
		return ""
	}
	if name, ok := m.relTypeName(tok); ok {
		return name
	}
	return ""
}

// relProps reads every property a relationship carries, like nodeProps.
func (m *objectMaterializer) relProps(id uint64) map[string]Value {
	out := map[string]Value{}
	toks, err := m.tx.RelPropertyKeys(engine.RelID(id))
	if err != nil {
		return out
	}
	for _, t := range toks {
		name, ok := m.propKeyName(t)
		if !ok {
			continue
		}
		v, err := m.tx.RelProperty(engine.RelID(id), t)
		if err != nil || v.IsNull() {
			continue
		}
		out[name] = m.fromValue(v)
	}
	return out
}

// relGet reads one relationship property by name, like nodeGet.
func (m *objectMaterializer) relGet(id uint64, key string) (Value, bool) {
	if m == nil || m.tx == nil {
		return nil, false
	}
	tok, ok := m.tx.Lookup(catalog.KindPropKey, key)
	if !ok {
		return nil, false
	}
	v, err := m.tx.RelProperty(engine.RelID(id), tok)
	if err != nil || v.IsNull() {
		return nil, false
	}
	return m.fromValue(v), true
}

// keysOf returns a property map's keys in sorted order, so Keys is deterministic.
func keysOf(m map[string]Value) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// nonNilProps returns m, or an empty map when m is nil, so Props never returns a nil
// map a caller would have to guard.
func nonNilProps(m map[string]Value) map[string]Value {
	if m == nil {
		return map[string]Value{}
	}
	return m
}
