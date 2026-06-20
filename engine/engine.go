// Package engine declares the storage-engine SPI — the narrow internal contract
// the whole query stack is built against (spec 2060 doc 04 §14, doc 25 §3.8).
//
// This is the most consequential seam in gr: it is provided by the storage
// engine (M1) and consumed by the entire query stack (M2 read path, M3 write
// path, M4 mature executor). It is declared here in M0, verbatim from the design
// contract, and held stable thereafter so the engine can evolve its internals —
// from the M1 naïve form to the M4 compressed-and-cached form — without the
// query stack noticing. The SPI hides the base/delta split, segmentation,
// encodings, and the two-id scheme; a consumer sees a snapshot-consistent graph
// with cheap expands and vectorized property reads.
//
// M0 ships the declaration plus a trivial in-memory stub (stub.go); the real
// engine lands in M1.
package engine

import "github.com/tamnd/gr/value"

// NodeID is the stable element id of a node (doc 04 §3, the two-id scheme's
// public half). It is opaque to the query stack.
type NodeID uint64

// RelID is the stable element id of a relationship.
type RelID uint64

// Token is a catalog-interned identifier for a label, relationship type, or
// property key (doc 08 §7). The query stack carries tokens, not strings.
type Token uint32

// Direction selects which side of a node's adjacency an expand walks.
type Direction uint8

const (
	// Outgoing follows relationships where the node is the source.
	Outgoing Direction = iota
	// Incoming follows relationships where the node is the target.
	Incoming
	// Both follows relationships in either direction.
	Both
)

// Neighbor is one edge yielded by Expand: the relationship and the node reached.
type Neighbor struct {
	Rel  RelID
	Node NodeID
	Type Token
}

// Predicate is a pushed-down property test for predicate scans (doc 04 §6.6).
// A nil Predicate matches everything.
type Predicate func(value.Value) bool

// Engine is the storage-engine SPI. All access is scoped to a Snapshot or a
// write Tx obtained from it. The lifecycle is Open → (Begin → … → Commit/Abort)*
// → Close, with Checkpoint available to force durable, WAL-truncating flushes.
type Engine interface {
	// Begin opens a transaction. A read transaction sees a stable snapshot at its
	// start point; a write transaction additionally accumulates changes that
	// become visible atomically at Commit (doc 06).
	Begin(write bool) (Tx, error)

	// Checkpoint forces all committed changes into the main file and truncates the
	// WAL (doc 05 §8). It is safe to call concurrently with readers.
	Checkpoint() error

	// Close releases the engine. It does not checkpoint; callers checkpoint first
	// if they want a sidecar-free file.
	Close() error
}

// Tx is a snapshot-scoped unit of access. Read transactions expose the read half
// of the SPI; write transactions add the mutating half. Commit makes a write
// transaction's changes durable and visible; Abort discards them. A read
// transaction is closed with Abort (Commit on a read tx is a no-op).
type Tx interface {
	// --- snapshot-scoped reads (doc 04 §14, "Node/Relationship/Predicate access") ---

	// NodeExists reports whether a node is visible under this snapshot.
	NodeExists(id NodeID) (bool, error)
	// RelExists reports whether a relationship is visible under this snapshot.
	RelExists(id RelID) (bool, error)
	// NodeLabels returns the labels of a node as catalog tokens.
	NodeLabels(id NodeID) ([]Token, error)
	// HasLabel tests a single label without materializing the label set.
	HasLabel(id NodeID, label Token) (bool, error)
	// NodeProperty reads one property of a node; a null Value means absent.
	NodeProperty(id NodeID, key Token) (value.Value, error)
	// RelProperty reads one property of a relationship.
	RelProperty(id RelID, key Token) (value.Value, error)
	// RelType returns the type token of a relationship (the read behind the
	// type() function, doc 09 §7).
	RelType(id RelID) (Token, error)
	// NodePropertyKeys returns the property keys a node carries under this
	// snapshot, the enumeration behind keys() and properties() (doc 09 §7).
	NodePropertyKeys(id NodeID) ([]Token, error)
	// RelPropertyKeys returns the property keys a relationship carries under this
	// snapshot.
	RelPropertyKeys(id RelID) ([]Token, error)

	// ScanLabel yields the visible nodes carrying a label, in position order. A
	// zero label scans all nodes.
	ScanLabel(label Token, fn func(NodeID) error) error
	// Expand walks a node's adjacency along a type and direction, yielding the
	// snapshot-visible neighbors. A zero relType expands all types.
	Expand(id NodeID, relType Token, dir Direction, fn func(Neighbor) error) error

	// --- statistics for the planner (doc 04 §14, "Statistics"; doc 11) ---

	// Degree returns the number of relationships of a node along a direction.
	Degree(id NodeID, relType Token, dir Direction) (int64, error)

	// --- writes (write tx only; doc 04 §14, "Writes"; doc 13) ---

	// CreateNode creates a node with the given labels and returns its id.
	CreateNode(labels []Token) (NodeID, error)
	// DeleteNode removes a node (and, per doc 13, requires it be detached).
	DeleteNode(id NodeID) error
	// CreateRel creates a typed relationship from src to dst.
	CreateRel(src, dst NodeID, relType Token) (RelID, error)
	// DeleteRel removes a relationship.
	DeleteRel(id RelID) error
	// SetNodeProperty sets (or, with a null Value, removes) a node property.
	SetNodeProperty(id NodeID, key Token, v value.Value) error
	// SetRelProperty sets (or removes) a relationship property.
	SetRelProperty(id RelID, key Token, v value.Value) error
	// AddLabel adds a label to a node.
	AddLabel(id NodeID, label Token) error
	// RemoveLabel removes a label from a node.
	RemoveLabel(id NodeID, label Token) error

	// --- lifecycle ---

	// Commit makes a write transaction durable and visible (no-op on a read tx).
	Commit() error
	// Abort discards a write transaction's changes and releases the snapshot.
	Abort() error
}
