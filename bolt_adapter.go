package gr

import (
	"context"
	"errors"
	"strconv"
	"sync/atomic"

	"github.com/tamnd/gr/bolt"
	"github.com/tamnd/gr/engine"
	"github.com/tamnd/gr/value"
)

// This file is the engine adapter for the Bolt server: it implements the
// bolt.Handler, bolt.Tx, bolt.Cursor, and bolt.Materializer interfaces over a
// *DB (spec 2060 doc 18 §5, §8.1). The bolt package defines those interfaces and
// must not import this package (an import cycle), so the adapter lives here, where
// it can reach both the public query surface and the internal materializer.

// BoltOption configures a Bolt handler.
type BoltOption func(*boltHandler)

// WithBoltAuth makes the handler require authentication: a connection must present
// valid credentials, and the "none" scheme is rejected (doc 18 §10). Without it
// the handler authorizes every connection, the embedded-friendly default for a
// server bound to localhost (doc 18 §11.4).
func WithBoltAuth() BoltOption {
	return func(h *boltHandler) { h.requireAuth = true }
}

// BoltHandler returns a bolt.Handler that runs Cypher over the database for a Bolt
// server (doc 18 §5). Pass it to a bolt.Server, which a bolt.Listener serves over
// TCP:
//
//	srv := &bolt.Server{Handler: db.BoltHandler()}
//	ln := &bolt.Listener{Server: srv, Addr: ":7687"}
//	ln.ListenAndServe()
func (db *DB) BoltHandler(opts ...BoltOption) bolt.Handler {
	h := &boltHandler{db: db}
	for _, o := range opts {
		o(h)
	}
	return h
}

// boltHandler is the engine seam a Bolt session drives (doc 18 §5). It
// authenticates against the credential store and opens a transaction per Bolt
// transaction. The bookmark counter is a process-local commit sequence, the
// single-node reduction of causal bookmarks (doc 18 §7.4): on one node every later
// read already sees every prior commit, so the bookmark is a monotonic token a
// driver can round-trip, not a cross-member coordination value.
type boltHandler struct {
	db          *DB
	requireAuth bool
	bookmark    atomic.Uint64
}

// Authenticate verifies a connection's credentials (doc 18 §10). With auth off it
// authorizes everyone; with auth on it checks the principal and credentials
// against the credential store and rejects the "none" scheme.
func (h *boltHandler) Authenticate(scheme, principal, credentials string) error {
	if !h.requireAuth {
		return nil
	}
	if scheme == "none" || scheme == "" {
		return errors.New("authentication required")
	}
	roles, ok, err := h.db.Authenticate(principal, credentials)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("invalid principal or credentials")
	}
	_ = roles
	return nil
}

// Begin opens a Bolt transaction (doc 18 §5.10). The engine transaction itself is
// opened lazily on the first Run, once the statement is known, so the access mode
// can follow the client's mode hint or the statement kind (doc 18 §8.3) rather
// than always taking the write path.
func (h *boltHandler) Begin(extra map[string]any) (bolt.Tx, error) {
	mode, hinted := boltModeHint(extra)
	return &boltTx{h: h, db: h.db, mode: mode, modeHinted: hinted}, nil
}

// boltModeHint reads the access-mode hint from a BEGIN or RUN extra map (doc 18
// §5.3): "r" for read, "w" for write. The second return reports whether a hint was
// present, so an absent hint falls back to classifying the statement.
func boltModeHint(extra map[string]any) (AccessMode, bool) {
	if m, ok := extra["mode"].(string); ok {
		switch m {
		case "r":
			return Read, true
		case "w":
			return Write, true
		}
	}
	return Write, false
}

// boltTx adapts a managed transaction to bolt.Tx (doc 18 §5.10). A schema,
// administrative, or pragma statement auto-commits itself, so it runs through the
// database-level Run with no held transaction (matching Neo4j, where such commands
// cannot run inside an explicit transaction); a read or write statement runs in a
// lazily-opened engine transaction the client commits.
type boltTx struct {
	h          *boltHandler
	db         *DB
	mode       AccessMode
	modeHinted bool

	tx         *Tx
	standalone bool // last statement auto-committed itself (schema/admin/pragma)
}

// Run executes a query and returns a cursor over its rows (doc 18 §5.6).
func (t *boltTx) Run(query string, params map[string]value.Value) (bolt.Cursor, error) {
	kind, _ := t.db.StatementKind(query)
	p := boltParams(params)

	// Schema, administrative, and pragma statements auto-commit and cannot run
	// inside the held transaction (doc 18 §5.6); route them through the
	// database-level Run, which dispatches and commits them itself.
	if kind == SchemaStatement || kind == AdminStatement || kind == PragmaStatement {
		res, err := t.db.Run(context.Background(), query, p)
		if err != nil {
			return nil, boltErr(err)
		}
		t.standalone = true
		return &boltCursor{res: res, kind: kind}, nil
	}

	// Open the engine transaction on the first read/write statement, choosing the
	// access mode from the hint or, absent a hint, the statement kind (doc 18 §8.3).
	if t.tx == nil {
		mode := t.mode
		if !t.modeHinted && kind == ReadStatement {
			mode = Read
		}
		tx, err := t.db.Begin(context.Background(), mode)
		if err != nil {
			return nil, boltErr(err)
		}
		t.tx = tx
	}
	res, err := t.tx.Run(context.Background(), query, p)
	if err != nil {
		return nil, boltErr(err)
	}
	return &boltCursor{res: res, kind: kind}, nil
}

// Materializer resolves node and relationship handles in a result against the
// transaction's snapshot (doc 18 §6.10). A standalone statement holds no
// transaction and returns no graph handles, so a nil-backed materializer (bare
// handles) is correct there.
func (t *boltTx) Materializer() bolt.Materializer {
	var etx engine.Tx
	if t.tx != nil {
		etx = t.tx.etx
	}
	return boltMat{m: t.db.materializer(etx, false)}
}

// Commit commits the transaction and returns the causal bookmark (doc 18 §5.10).
// A standalone statement already committed, so there is nothing to commit and the
// bookmark still advances.
func (t *boltTx) Commit() (string, error) {
	if t.tx != nil {
		if err := t.tx.Commit(); err != nil {
			return "", boltErr(err)
		}
		t.tx = nil
	}
	return t.h.nextBookmark(), nil
}

// Rollback rolls the transaction back (doc 18 §5.10). A standalone statement
// already committed, so there is nothing to undo.
func (t *boltTx) Rollback() error {
	if t.tx == nil {
		return nil
	}
	err := t.tx.Rollback()
	t.tx = nil
	return err
}

// nextBookmark advances and formats the process-local commit sequence (doc 18
// §7.4).
func (h *boltHandler) nextBookmark() string {
	return "gr:bookmark:" + strconv.FormatUint(h.bookmark.Add(1), 10)
}

// boltCursor adapts a streaming Result to bolt.Cursor (doc 18 §5.7).
type boltCursor struct {
	res  *Result
	kind Kind
}

func (c *boltCursor) Fields() []string { return c.res.Columns() }

func (c *boltCursor) Next() ([]value.Value, bool, error) { return c.res.Row() }

func (c *boltCursor) Summary() bolt.Summary {
	return bolt.Summary{Type: boltQueryType(c.kind), Stats: boltStats(c.res.Summary())}
}

func (c *boltCursor) Close() error { return c.res.Close() }

// boltMat adapts the internal object materializer to bolt.Materializer (doc 18
// §6.10).
type boltMat struct {
	m *objectMaterializer
}

func (b boltMat) MaterializeNode(id uint64) (bolt.Node, error) {
	n := b.m.node(id)
	return bolt.Node{
		ID:        int64(id),
		Labels:    n.Labels(),
		Props:     n.Props(),
		ElementID: n.ElementId(),
	}, nil
}

func (b boltMat) MaterializeRel(id uint64) (bolt.Rel, error) {
	r := b.m.rel(id)
	return bolt.Rel{
		ID:             int64(id),
		StartID:        int64(uint64(r.startID)),
		EndID:          int64(uint64(r.endID)),
		Type:           r.Type(),
		Props:          r.Props(),
		ElementID:      r.ElementId(),
		StartElementID: r.StartElementId(),
		EndElementID:   r.EndElementId(),
	}, nil
}

// boltQueryType maps a statement kind to the Bolt result-summary type letter
// (doc 18 §5.7): "r" read, "w" write, "s" schema or other system statement.
func boltQueryType(k Kind) string {
	switch k {
	case WriteStatement:
		return "w"
	case SchemaStatement, AdminStatement, PragmaStatement:
		return "s"
	default:
		return "r"
	}
}

// boltStats maps the result summary's mutation counters to the Neo4j stat keys a
// driver reads (doc 18 §5.7), omitting zero counters. A statement that changed
// nothing returns nil, so the SUCCESS carries no stats map.
func boltStats(s Summary) map[string]any {
	m := map[string]any{}
	add := func(k string, n int) {
		if n != 0 {
			m[k] = int64(n)
		}
	}
	add("nodes-created", s.NodesCreated)
	add("nodes-deleted", s.NodesDeleted)
	add("relationships-created", s.RelationshipsCreated)
	add("relationships-deleted", s.RelationshipsDeleted)
	add("properties-set", s.PropertiesSet)
	add("labels-added", s.LabelsAdded)
	add("labels-removed", s.LabelsRemoved)
	add("indexes-added", s.IndexesAdded)
	add("indexes-removed", s.IndexesRemoved)
	add("constraints-added", s.ConstraintsAdded)
	add("constraints-removed", s.ConstraintsRemoved)
	if len(m) == 0 {
		return nil
	}
	return m
}

// boltParams converts the session's decoded parameters into the database's Params
// form (doc 18 §6.9). The session has already reduced node and relationship
// parameters to element-id strings, so only scalars, lists, and maps reach here.
func boltParams(p map[string]value.Value) Params {
	if len(p) == 0 {
		return nil
	}
	out := make(Params, len(p))
	for k, v := range p {
		out[k] = valueToAny(v)
	}
	return out
}

// valueToAny converts an internal value to the plain Go value the database's
// parameter binding accepts.
func valueToAny(v value.Value) any {
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
		xs, _ := v.AsList()
		out := make([]any, len(xs))
		for i, e := range xs {
			out[i] = valueToAny(e)
		}
		return out
	case value.TypeMap:
		mp, _ := v.AsMap()
		out := make(map[string]any, len(mp))
		for k, e := range mp {
			out[k] = valueToAny(e)
		}
		return out
	default:
		// A node, relationship, or path cannot be a parameter value; the session
		// already reduced graph parameters to element-id strings (doc 18 §6.9).
		return nil
	}
}

// boltErr maps a database error to a Bolt status error so the session reports the
// right Neo4j status code (doc 18 §12). A query fault is a client error; an engine
// failure is left for the session to report as a generic database error.
func boltErr(err error) error {
	if err == nil {
		return nil
	}
	var se *bolt.StatusError
	if errors.As(err, &se) {
		return err
	}
	switch {
	case errors.Is(err, ErrReadOnly):
		return &bolt.StatusError{Code: "Neo.ClientError.Statement.AccessMode", Message: err.Error()}
	case errors.Is(err, ErrSchemaCommand), errors.Is(err, ErrAdminCommand), errors.Is(err, ErrPragmaCommand):
		return &bolt.StatusError{Code: "Neo.ClientError.Statement.SemanticError", Message: err.Error()}
	case errors.Is(err, ErrTxnDone):
		return &bolt.StatusError{Code: "Neo.ClientError.Request.Invalid", Message: err.Error()}
	case errors.Is(err, ErrClosed):
		return &bolt.StatusError{Code: "Neo.DatabaseError.General.UnknownError", Message: err.Error()}
	default:
		// A parse, bind, or runtime fault in the client's query is a client error.
		return &bolt.StatusError{Code: "Neo.ClientError.Statement.SyntaxError", Message: err.Error()}
	}
}
