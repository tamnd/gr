package gr

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strconv"
	"sync/atomic"
	"time"

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
// server bound to localhost (doc 18 §11.4). Authentication runs against the
// database's own credential store.
func WithBoltAuth() BoltOption {
	return func(h *boltHandler) { h.requireAuth = true }
}

// WithBoltAuthFunc routes Bolt authentication through an external verifier instead
// of the database credential store, so the Bolt and HTTP transports share one auth
// provider (doc 18 §10.4): a deployment configures auth once and both surfaces
// enforce it identically. The function receives the HELLO/LOGON scheme, principal,
// and credentials and returns the principal's roles to admit the connection, or an
// error to reject it; it is responsible for rejecting the "none" scheme when auth is
// required. The returned roles drive per-statement authorization (doc 18 §10.6).
func WithBoltAuthFunc(fn func(scheme, principal, credentials string) (roles []string, err error)) BoltOption {
	return func(h *boltHandler) {
		h.requireAuth = true
		h.authFunc = fn
	}
}

// WithBoltAdmission gives the handler a shared in-flight-query admission gate (doc 18
// §8.8). Every statement passes through the gate before it executes, so the gate bounds
// how many queries run at once across both server surfaces. Passing the same gate to the
// HTTP surface makes the bound hold across the whole process. A nil gate, or no option,
// leaves the Bolt path ungated, the embedded-friendly default.
func WithBoltAdmission(a *Admission) BoltOption {
	return func(h *boltHandler) { h.admission = a }
}

// WithBoltQueryMaxTime caps the wall-clock time a single statement may run (doc 18 §8.6,
// §8.8). A statement that runs longer has its context cancelled and is reported as a timed
// out transaction, so one runaway query cannot pin an engine slot indefinitely. The cap
// spans the cursor's whole life, not just submission, so a query that streams for too long
// is cut off too. A zero or negative duration, or no option, leaves the Bolt path
// uncapped, the embedded-friendly default. Pass the same value to the HTTP surface so both
// transports enforce one cap.
func WithBoltQueryMaxTime(d time.Duration) BoltOption {
	return func(h *boltHandler) { h.queryMaxTime = d }
}

// WithBoltRateLimiter gives the handler a shared per-principal query rate limiter (doc 18
// §8.8). Every statement charges one token against the connection's principal before it
// executes, so a single client cannot monopolize the engine with a flood of cheap
// queries. Passing the same limiter to the HTTP surface makes the bound hold across the
// whole process. A nil limiter, or no option, leaves the Bolt path unlimited, the
// embedded-friendly default.
func WithBoltRateLimiter(r *RateLimiter) BoltOption {
	return func(h *boltHandler) { h.limiter = r }
}

// WithBoltQueryLog gives the handler a shared structured query log (doc 20 §10). Each
// statement is recorded through it, by its level and slow threshold, with parameters
// redacted by the log's policy. Passing the same log to the HTTP surface makes both
// transports feed one query stream that reads identically. A nil log, or no option, records
// nothing, the embedded-friendly default.
func WithBoltQueryLog(l *QueryLog) BoltOption {
	return func(h *boltHandler) { h.qlog = l }
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
	// Wire the admission gate to the database's queue-wait metrics, so a Bolt query that waits
	// for a slot is counted in gr_query_queued_total (doc 20 §3.1). A nil gate is a no-op.
	db.InstrumentAdmission(h.admission)
	return h
}

// BoltObserver returns a bolt.Observer that feeds the database's metric registry the Bolt
// session and protocol counters (doc 20 §3.3): sessions opened and closed, their lifetimes,
// the messages dispatched, the protocol errors, and the authentication outcomes. Set it on the
// bolt.Server alongside the handler:
//
//	srv := &bolt.Server{Handler: db.BoltHandler(), Observer: db.BoltObserver()}
//
// An embedded database that runs no Bolt server never calls it, so leaving the server's Observer
// nil simply records nothing.
func (db *DB) BoltObserver() bolt.Observer {
	return boltObserver{m: db.metrics}
}

// boltObserver adapts the query-metric registry to bolt.Observer (doc 20 §3.3). The bolt package
// defines the interface and must not import this one, so the implementation lives here where it can
// reach db.metrics. Every method is one registry update keyed by the protocol name "bolt".
type boltObserver struct{ m *queryMetrics }

func (o boltObserver) SessionOpen()                   { o.m.sessionOpen("bolt") }
func (o boltObserver) SessionClose(dur time.Duration) { o.m.sessionClose("bolt", dur) }
func (o boltObserver) Message(msgType string)         { o.m.recordBoltMessage(msgType) }
func (o boltObserver) Error(code string)              { o.m.recordBoltError(code) }
func (o boltObserver) Auth(ok bool)                   { o.m.recordAuth(ok) }

// boltHandler is the engine seam a Bolt session drives (doc 18 §5). It
// authenticates against the credential store and opens a transaction per Bolt
// transaction. The bookmark counter is a process-local commit sequence, the
// single-node reduction of causal bookmarks (doc 18 §7.4): on one node every later
// read already sees every prior commit, so the bookmark is a monotonic token a
// driver can round-trip, not a cross-member coordination value.
type boltHandler struct {
	db           *DB
	requireAuth  bool
	authFunc     func(scheme, principal, credentials string) (roles []string, err error)
	admission    *Admission
	limiter      *RateLimiter
	queryMaxTime time.Duration
	qlog         *QueryLog
	now          func() time.Time // clock for query-log timing; nil uses time.Now
	bookmark     atomic.Uint64
}

// clock returns the handler's current time for query-log timing, falling back to time.Now
// when no clock seam is set, so a directly-constructed handler works without configuration.
func (h *boltHandler) clock() time.Time {
	if h.now != nil {
		return h.now()
	}
	return time.Now()
}

// queryContext builds the context a statement runs under, applying the wall-clock cap when
// one is configured (doc 18 §8.6). The returned cancel must be called when the cursor
// closes, both to release the timer and to stop a still-running query at the deadline.
func (h *boltHandler) queryContext() (context.Context, context.CancelFunc) {
	if h.queryMaxTime > 0 {
		return context.WithTimeout(context.Background(), h.queryMaxTime)
	}
	return context.WithCancel(context.Background())
}

// Authenticate verifies a connection's credentials (doc 18 §10). With auth off it
// authorizes everyone; with auth on it checks the principal and credentials
// against the credential store and rejects the "none" scheme.
func (h *boltHandler) Authenticate(scheme, principal, credentials string) (bolt.Auth, error) {
	if !h.requireAuth {
		return bolt.Auth{}, nil
	}
	// An external verifier (the shared auth provider) takes over entirely when set,
	// including rejecting the "none" scheme (doc 18 §10.4). It returns the principal's
	// roles, so per-statement authorization runs the same role model as HTTP.
	if h.authFunc != nil {
		roles, err := h.authFunc(scheme, principal, credentials)
		if err != nil {
			return bolt.Auth{}, err
		}
		return bolt.Auth{Principal: principal, Roles: roles}, nil
	}
	if scheme == "none" || scheme == "" {
		return bolt.Auth{}, errors.New("authentication required")
	}
	roles, ok, err := h.db.Authenticate(principal, credentials)
	if err != nil {
		return bolt.Auth{}, err
	}
	if !ok {
		return bolt.Auth{}, errors.New("invalid principal or credentials")
	}
	return bolt.Auth{Principal: principal, Roles: roles}, nil
}

// Begin opens a Bolt transaction (doc 18 §5.10). The engine transaction itself is
// opened lazily on the first Run, once the statement is known, so the access mode
// can follow the client's mode hint or the statement kind (doc 18 §8.3) rather
// than always taking the write path.
func (h *boltHandler) Begin(extra map[string]any, auth bolt.Auth) (bolt.Tx, error) {
	mode, hinted := boltModeHint(extra)
	return &boltTx{h: h, db: h.db, mode: mode, modeHinted: hinted, roles: auth.Roles, principal: auth.Principal}, nil
}

// boltRateKey is the rate-limit bucket key for a connection's principal (doc 18 §8.8).
// An authenticated connection keys by its principal, prefixed to share the namespace with
// the HTTP surface's per-token keys; an anonymous connection (auth off) keys to one shared
// bucket, so an auth-off Bolt server rate-limits as a single tenant, which is the
// localhost, single-application posture that runs without auth.
func boltRateKey(principal string) string {
	if principal == "" {
		return "bolt:anon"
	}
	return "user:" + principal
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
	roles      []string // the connection's roles, for per-statement authorization
	principal  string   // the connection's principal, for the rate-limit key

	tx         *Tx
	standalone bool // last statement auto-committed itself (schema/admin/pragma)
}

// Run executes a query and returns a cursor over its rows (doc 18 §5.6).
func (t *boltTx) Run(query string, params map[string]value.Value) (bolt.Cursor, error) {
	started := t.h.clock()
	kind, kindErr := t.db.StatementKind(query)
	p := boltParams(params)

	// Authorize the statement against the connection's roles before any side effect
	// (doc 18 §10.6), the same role model the HTTP surface enforces. With auth off
	// there is no authorization. An unparseable statement is let through so execution
	// reports it as a syntax error rather than a misleading forbidden error.
	if t.h.requireAuth && kindErr == nil {
		if boltRoleLevel(t.roles) < boltKindLevel(kind) {
			err := &bolt.StatusError{
				Code:    "Neo.ClientError.Security.Forbidden",
				Message: "this account is not allowed to run " + kind.String() + " statements",
			}
			t.logQuery(started, query, p, kind, kindErr, err, 0)
			return nil, err
		}
	}

	// Charge the per-principal rate limit before taking an engine slot, so a throttled
	// query sheds at once rather than waiting at the gate (doc 18 §8.8). A spent budget is
	// a retryable transient, the same call the HTTP surface makes. A nil limiter allows
	// every query.
	if ok, _ := t.h.limiter.Allow(boltRateKey(t.principal)); !ok {
		err := &bolt.StatusError{
			Code:    "Neo.TransientError.General.TransientError",
			Message: "query rate limit exceeded, retry shortly",
		}
		t.logQuery(started, query, p, kind, kindErr, err, 0)
		return nil, err
	}

	// Pass the in-flight-query gate before executing, so a query that finds the gate
	// full sheds as a retryable transient rather than the server queueing without bound
	// (doc 18 §8.8, §8.9). The slot is held for the cursor's life and released when the
	// cursor closes, so the gate bounds queries that are still streaming, not only those
	// being submitted. A disabled gate admits immediately with a no-op release.
	release, err := t.h.admission.Acquire(context.Background())
	if err != nil {
		be := boltAdmitErr(err)
		t.logQuery(started, query, p, kind, kindErr, err, 0)
		return nil, be
	}

	// Run the statement under the wall-clock cap when one is configured (doc 18 §8.6).
	// The cancel lives for the cursor's life and runs on Close, alongside the slot
	// release, so a query that streams past the deadline is cut off too. An uncapped
	// handler returns a plain cancellable context.
	ctx, cancel := t.h.queryContext()

	// The cursor records the query through the shared log when it closes, once the row
	// count and the final status are known (doc 20 §10). The skeleton carries the facts
	// fixed at submission; Close fills the duration, rows, and outcome.
	rec := t.queryRecord(started, query, p, kind, kindErr)

	// Schema, administrative, and pragma statements auto-commit and cannot run
	// inside the held transaction (doc 18 §5.6); route them through the
	// database-level Run, which dispatches and commits them itself.
	if kind == SchemaStatement || kind == AdminStatement || kind == PragmaStatement {
		res, err := t.db.Run(ctx, query, p)
		if err != nil {
			cancel()
			release()
			t.logQuery(started, query, p, kind, kindErr, err, 0)
			return nil, boltErr(err)
		}
		t.standalone = true
		return &boltCursor{res: res, kind: kind, release: release, cancel: cancel, rec: rec}, nil
	}

	// Open the engine transaction on the first read/write statement, choosing the
	// access mode from the hint or, absent a hint, the statement kind (doc 18 §8.3).
	if t.tx == nil {
		mode := t.mode
		if !t.modeHinted && kind == ReadStatement {
			mode = Read
		}
		tx, err := t.db.Begin(ctx, mode)
		if err != nil {
			cancel()
			release()
			t.logQuery(started, query, p, kind, kindErr, err, 0)
			return nil, boltErr(err)
		}
		t.tx = tx
	}
	res, err := t.tx.Run(ctx, query, p)
	if err != nil {
		cancel()
		release()
		t.logQuery(started, query, p, kind, kindErr, err, 0)
		return nil, boltErr(err)
	}
	return &boltCursor{res: res, kind: kind, release: release, cancel: cancel, rec: rec}, nil
}

// queryRecord builds the query-log skeleton a cursor carries until it closes (doc 20 §10):
// the facts fixed when a query is submitted. It returns nil when no query log is configured,
// so the cursor skips recording. Close fills the duration, row count, and final status.
func (t *boltTx) queryRecord(started time.Time, query string, p Params, kind Kind, kindErr error) *boltPendingRecord {
	if t.h.qlog == nil {
		return nil
	}
	k := ""
	if kindErr == nil {
		k = kind.String()
	}
	return &boltPendingRecord{
		qlog:    t.h.qlog,
		clock:   t.h.clock,
		started: started,
		user:    boltUser(t.principal),
		query:   query,
		params:  map[string]any(p),
		kind:    k,
	}
}

// logQuery records a query that failed before a cursor was created (doc 20 §10): an
// authorization refusal, a throttle, a shed, or a run error. A nil log is a no-op. The
// status follows the error, so a deadline reads as a timeout and anything else as an error.
func (t *boltTx) logQuery(started time.Time, query string, p Params, kind Kind, kindErr error, qerr error, rows int) {
	if t.h.qlog == nil {
		return
	}
	k := ""
	if kindErr == nil {
		k = kind.String()
	}
	t.h.qlog.Record(QueryRecord{
		StartedAt:    started,
		QueryID:      boltQueryID(),
		User:         boltUser(t.principal),
		Cypher:       query,
		Params:       map[string]any(p),
		Kind:         k,
		Status:       queryStatus(qerr),
		Err:          qerr,
		Duration:     t.h.clock().Sub(started),
		RowsReturned: rows,
	})
}

// boltPendingRecord is the query-log skeleton a cursor carries from Run to Close (doc 20
// §10). It holds what is known at submission; the cursor fills the rest as it streams and
// emits one entry on Close.
type boltPendingRecord struct {
	qlog    *QueryLog
	clock   func() time.Time
	started time.Time
	user    string
	query   string
	params  map[string]any
	kind    string
}

// boltUser names the user for a query-log entry: the connection's principal, or "anonymous"
// when the connection is unauthenticated.
func boltUser(principal string) string {
	if principal == "" {
		return "anonymous"
	}
	return principal
}

// boltQueryID returns an unguessable per-query id for the query log, a random 128-bit token
// hex-encoded, so an entry can be correlated without exposing a sequential counter.
func boltQueryID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(b[:])
}

// boltAdmitErr maps an admission-gate failure to a Bolt status (doc 18 §8.9). A full gate
// is a retryable transient, so a driver backs off and retries; a cancelled context (a
// RESET or timeout while queued) maps through the ordinary engine-error path.
func boltAdmitErr(err error) error {
	if errors.Is(err, ErrOverloaded) {
		return &bolt.StatusError{
			Code:    "Neo.TransientError.General.TransientError",
			Message: "server is busy, too many queries in flight, retry shortly",
		}
	}
	return boltErr(err)
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

// boltCursor adapts a streaming Result to bolt.Cursor (doc 18 §5.7). It holds the
// admission slot the query was admitted on and the cancel for the query's wall-clock
// cap, releasing the slot and cancelling the context on Close, so the in-flight bound and
// the time cap both cover a query for as long as it streams (doc 18 §8.6, §8.9).
type boltCursor struct {
	res     *Result
	kind    Kind
	release func()
	cancel  context.CancelFunc

	// query-log state, emitted on Close (doc 20 §10). rec is nil when no log is configured.
	rec    *boltPendingRecord
	rows   int
	rawErr error // the unmapped streaming error, for the logged status
}

func (c *boltCursor) Fields() []string { return c.res.Columns() }

// Next returns the next row, mapping a streaming fault to a Bolt status (doc 18 §12). The
// wall-clock cap can fire while a query streams, so the deadline surfaces here rather than
// at Run; routing the error through boltErr reports it as a timed out transaction instead
// of a generic database error. The raw error is kept for the query-log status, and a
// returned row is counted so the log records the rows actually streamed.
func (c *boltCursor) Next() ([]value.Value, bool, error) {
	row, ok, err := c.res.Row()
	if err != nil {
		c.rawErr = err
		return nil, false, boltErr(err)
	}
	if ok {
		c.rows++
	}
	return row, ok, nil
}

func (c *boltCursor) Summary() bolt.Summary {
	return bolt.Summary{Type: boltQueryType(c.kind), Stats: boltStats(c.res.Summary())}
}

func (c *boltCursor) Close() error {
	err := c.res.Close()
	if c.cancel != nil {
		c.cancel()
		c.cancel = nil
	}
	if c.release != nil {
		c.release()
		c.release = nil
	}
	// Record the query now that the row count and the final status are known (doc 20 §10).
	// The status follows the streaming error, so a query that hit the wall-clock cap mid
	// stream reads as a timeout. The skeleton is cleared so a second Close does not log twice.
	if c.rec != nil {
		c.rec.qlog.Record(QueryRecord{
			StartedAt:    c.rec.started,
			QueryID:      boltQueryID(),
			User:         c.rec.user,
			Cypher:       c.rec.query,
			Params:       c.rec.params,
			Kind:         c.rec.kind,
			Status:       queryStatus(c.rawErr),
			Err:          c.rawErr,
			Duration:     c.rec.clock().Sub(c.rec.started),
			RowsReturned: c.rows,
			// The result is drained but not yet released at this point, so its captured plan is
			// readable: hand the log res.PlanText for the slow-query log's captured plan (doc 20
			// §10.6). The log runs it only for a slow query, so a fast one pays nothing.
			Plan: c.res.PlanText,
		})
		c.rec = nil
	}
	return err
}

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

// Authorization levels order the built-in roles by what statement kind they may run
// (doc 18 §10.6), mirroring the HTTP surface's model so both transports authorize
// identically. A principal's effective level is the highest its roles grant, and a
// statement runs when that level reaches the kind's level.
const (
	boltLevelNone   = -1 // no role, or only unknown roles: may run nothing
	boltLevelRead   = 0  // reader: read statements only
	boltLevelWrite  = 1  // editor: read and write data
	boltLevelSchema = 2  // publisher: read, write, and change schema
	boltLevelAdmin  = 3  // admin: everything publisher may, plus user management
)

// boltRoleLevel returns the highest access level a set of roles grants (doc 18 §10.6).
// An unknown role grants nothing, so a typo fails closed rather than open.
func boltRoleLevel(roles []string) int {
	level := boltLevelNone
	for _, role := range roles {
		l := boltLevelNone
		switch role {
		case RoleReader:
			l = boltLevelRead
		case RoleEditor:
			l = boltLevelWrite
		case RolePublisher:
			l = boltLevelSchema
		case RoleAdmin:
			l = boltLevelAdmin
		}
		if l > level {
			level = l
		}
	}
	return level
}

// boltKindLevel returns the access level a statement of the given kind requires
// (doc 18 §10.6, §12.3): a read needs read, a write needs write, a schema change
// needs schema, and an administrative statement needs admin. This mirrors the HTTP
// surface's kindLevel so both transports authorize a statement identically.
func boltKindLevel(k Kind) int {
	switch k {
	case WriteStatement:
		return boltLevelWrite
	case SchemaStatement:
		return boltLevelSchema
	case AdminStatement:
		return boltLevelAdmin
	default:
		return boltLevelRead
	}
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
	case errors.Is(err, context.DeadlineExceeded):
		// The wall-clock cap fired (doc 18 §8.6). A timed out transaction is a client
		// error so a driver does not treat it as a server fault to retry blindly.
		return &bolt.StatusError{Code: "Neo.ClientError.Transaction.TransactionTimedOut", Message: "query exceeded the server time limit"}
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
