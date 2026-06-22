package bolt

import (
	"errors"
	"fmt"
	"io"
	"sync/atomic"

	"github.com/tamnd/gr/pack"
	"github.com/tamnd/gr/value"
)

// The Neo4j status codes the session emits itself (doc 18 §12). Codes for query
// faults come from the handler via StatusError.
const (
	codeRequestInvalid = "Neo.ClientError.Request.Invalid"
	codeInvalidFormat  = "Neo.ClientError.Request.InvalidFormat"
	codeUnauthorized   = "Neo.ClientError.Security.Unauthorized"
	codeUnknown        = "Neo.DatabaseError.General.UnknownError"
)

// StatusError carries a Neo4j status code and message from the handler back to
// the client as a FAILURE (doc 18 §12). A handler returns one to control the
// code a query fault surfaces as; any other error becomes a generic database
// error.
type StatusError struct {
	Code    string
	Message string
}

func (e *StatusError) Error() string { return e.Code + ": " + e.Message }

// Summary is the result summary a cursor reports for the terminating SUCCESS of a
// PULL or DISCARD (doc 18 §5.7): the query type and the update counters.
type Summary struct {
	Type  string         // "r", "w", "rw", or "s"
	Stats map[string]any // update counters, omitted when empty
}

// Cursor streams a query's result rows (doc 18 §5.7). The session pulls rows from
// it in n-bounded batches and serializes each through EncodeValue.
type Cursor interface {
	// Fields returns the result column names, the RUN reply's field list.
	Fields() []string
	// Next advances to the next row; ok is false at the end of the stream.
	Next() (row []value.Value, ok bool, err error)
	// Summary returns the result summary for the terminating SUCCESS.
	Summary() Summary
	// Close releases the cursor.
	Close() error
}

// Tx is an open transaction the session runs queries in (doc 18 §5.10). An
// auto-commit RUN opens one implicitly and commits it when the stream ends; an
// explicit BEGIN opens one the client commits with COMMIT.
type Tx interface {
	// Run executes a query and returns a cursor over its rows.
	Run(query string, params map[string]value.Value) (Cursor, error)
	// Materializer resolves node and relationship handles in the result within
	// this transaction's snapshot.
	Materializer() Materializer
	// Commit commits the transaction and returns the causal bookmark.
	Commit() (bookmark string, err error)
	// Rollback rolls the transaction back.
	Rollback() error
}

// Auth is the authenticated identity a session carries from Authenticate to
// Begin, so the engine can authorize each statement against the principal's roles
// (doc 18 §10.6). An anonymous connection (authentication off) carries the zero
// Auth, and the engine runs without authorization.
type Auth struct {
	// Principal is the authenticated account name, empty for an anonymous connection.
	Principal string
	// Roles are the principal's granted roles, the input to per-statement
	// authorization.
	Roles []string
}

// Handler is the engine seam the session drives (doc 18 §5). The engine adapter
// implements it over a gr.DB; tests implement it with fakes. Keeping the session
// behind this interface keeps the bolt package free of the engine and unit
// testable over an in-memory connection.
type Handler interface {
	// Authenticate verifies credentials for an auth scheme ("none", "basic",
	// "bearer", "kerberos"). A nil error authorizes the connection; the returned
	// Auth carries the principal and roles the session hands to Begin.
	Authenticate(scheme, principal, credentials string) (Auth, error)
	// Begin opens a transaction with the given extra metadata (mode, db,
	// tx_timeout, and the rest, doc 18 §5.3) for the authenticated identity, so the
	// engine can authorize each statement against the principal's roles (doc 18 §10.6).
	Begin(extra map[string]any, auth Auth) (Tx, error)
}

// Server holds the configuration shared across Bolt connections (doc 18 §5, §7).
// One Server fields many sessions; Serve runs one connection.
type Server struct {
	// Handler is the engine seam (required).
	Handler Handler
	// ServerAgent is the version string HELLO reports, e.g. "Neo4j/5.23.0"; a
	// driver gates features on it (doc 18 §5.4, §15.2).
	ServerAgent string
	// Address is the advertised Bolt address a routing table names (doc 18 §7.2).
	Address string
	// Database is the served database name, default "neo4j" (doc 18 §5.3, §7.5).
	Database string
	// MaxMessage caps an inbound message's size; 0 uses the framing default.
	MaxMessage int
	// ChunkSize sets the outbound chunk payload size; 0 uses the framing default.
	ChunkSize int

	conns atomic.Int64
}

func (s *Server) serverAgent() string {
	if s.ServerAgent == "" {
		return "Neo4j/5.23.0"
	}
	return s.ServerAgent
}

func (s *Server) database() string {
	if s.Database == "" {
		return "neo4j"
	}
	return s.Database
}

func (s *Server) address() string {
	if s.Address == "" {
		return "localhost:7687"
	}
	return s.Address
}

// state is a Bolt connection's state (doc 18 §5.2).
type state int

const (
	stateNegotiation state = iota
	stateAuthentication
	stateReady
	stateStreaming
	stateTxReady
	stateTxStreaming
	stateFailed
	stateDefunct
)

// session is one Bolt connection: a state machine driving the handler over a
// framed connection (doc 18 §5.2). One goroutine owns a session, so it needs no
// internal locking.
type session struct {
	srv     *Server
	conn    io.ReadWriter
	r       *ChunkReader
	w       *ChunkWriter
	version Version
	state   state
	connID  string

	auth       Auth
	tx         Tx
	cursor     Cursor
	autocommit bool

	pending    []value.Value
	hasPending bool
}

// Serve runs the Bolt protocol on conn: the version handshake, then the message
// loop until the client says GOODBYE, the transport closes, or a fatal framing
// error occurs (doc 18 §5). It is the entry point a listener calls per accepted
// connection.
func (s *Server) Serve(conn io.ReadWriter) error {
	version, err := Handshake(conn)
	if err != nil {
		return err
	}
	sess := &session{
		srv:     s,
		conn:    conn,
		r:       NewChunkReader(conn, s.MaxMessage),
		w:       NewChunkWriter(conn, s.ChunkSize),
		version: version,
		state:   stateNegotiation,
		connID:  fmt.Sprintf("bolt-%d", s.conns.Add(1)),
	}
	return sess.loop()
}

// loop reads and dispatches messages until the connection ends.
func (s *session) loop() error {
	for {
		body, err := s.r.ReadMessage()
		if err != nil {
			if errors.Is(err, io.EOF) {
				s.cleanup()
				return nil
			}
			// A framing fault (oversize, truncation): best-effort FAILURE, then
			// close (doc 18 §13.4).
			_ = s.send(Failure(codeInvalidFormat, err.Error()))
			s.cleanup()
			return err
		}
		req, derr := DecodeRequest(body)
		if derr != nil {
			if ferr := s.fail(codeRequestInvalid, derr.Error()); ferr != nil {
				return ferr
			}
			continue
		}
		done, lerr := s.dispatch(req)
		if lerr != nil {
			return lerr
		}
		if done {
			s.cleanup()
			return nil
		}
	}
}

// dispatch routes one request by the current state (doc 18 §5.2). It returns done
// when the connection should close (GOODBYE) and an error only on a transport
// write failure.
func (s *session) dispatch(req Request) (done bool, err error) {
	// GOODBYE ends the connection from any state, with no reply (doc 18 §5).
	if _, ok := req.(Goodbye); ok {
		return true, nil
	}
	// RESET is valid from any authenticated state and returns to READY (doc 18
	// §5.2); it also clears the FAILED state.
	if _, ok := req.(Reset); ok {
		return false, s.handleReset()
	}
	// In FAILED, every request but RESET/GOODBYE is answered IGNORED (doc 18
	// §5.2).
	if s.state == stateFailed {
		return false, s.send(Ignored())
	}

	switch s.state {
	case stateNegotiation:
		if h, ok := req.(Hello); ok {
			return false, s.handleHello(h)
		}
	case stateAuthentication:
		if l, ok := req.(Logon); ok {
			return false, s.handleLogon(l)
		}
	case stateReady, stateTxReady:
		return false, s.handleReadyOrTx(req)
	case stateStreaming, stateTxStreaming:
		switch r := req.(type) {
		case Pull:
			return false, s.handlePull(r.N(), true)
		case Discard:
			return false, s.handlePull(r.N(), false)
		}
	}
	return false, s.fail(codeRequestInvalid, fmt.Sprintf("message 0x%02X not valid in the current state", req.Signature()))
}

// handleReadyOrTx handles the messages valid in READY and TX_READY (doc 18 §5.2).
func (s *session) handleReadyOrTx(req Request) error {
	switch r := req.(type) {
	case Run:
		return s.handleRun(r)
	case Logoff:
		if s.state != stateReady {
			break
		}
		s.rollbackTx()
		s.state = stateAuthentication
		return s.send(Success(nil))
	case Begin:
		if s.state != stateReady {
			break
		}
		return s.handleBegin(r)
	case Commit:
		if s.state == stateTxReady {
			return s.handleCommit()
		}
	case Rollback:
		if s.state == stateTxReady {
			return s.handleRollback()
		}
	case Route:
		if s.state == stateReady {
			return s.handleRoute(r)
		}
	case Telemetry:
		if s.state == stateReady {
			return s.send(Success(nil))
		}
	}
	return s.fail(codeRequestInvalid, fmt.Sprintf("message 0x%02X not valid in the current state", req.Signature()))
}

// handleHello initializes the connection (doc 18 §5.4). On Bolt < 5.1 it also
// authenticates; on 5.1+ it defers auth to LOGON.
func (s *session) handleHello(h Hello) error {
	meta := map[string]any{
		"server":        s.srv.serverAgent(),
		"connection_id": s.connID,
		"hints":         map[string]any{},
	}
	pre51 := s.version.Major < 5 || (s.version.Major == 5 && s.version.Minor < 1)
	if pre51 {
		scheme, principal, credentials := authFields(h.Extra)
		auth, err := s.srv.Handler.Authenticate(scheme, principal, credentials)
		if err != nil {
			// A failed legacy HELLO leaves the connection unusable (doc 18 §5.4).
			_ = s.send(Failure(codeUnauthorized, err.Error()))
			s.state = stateDefunct
			return nil
		}
		s.auth = auth
		s.state = stateReady
		return s.send(Success(meta))
	}
	s.state = stateAuthentication
	return s.send(Success(meta))
}

// handleLogon authenticates a 5.1+ connection (doc 18 §5.5).
func (s *session) handleLogon(l Logon) error {
	scheme, principal, credentials := authFields(l.Auth)
	auth, err := s.srv.Handler.Authenticate(scheme, principal, credentials)
	if err != nil {
		// The connection stays in AUTHENTICATION so the driver can retry
		// (doc 18 §5.5).
		return s.send(Failure(codeUnauthorized, err.Error()))
	}
	s.auth = auth
	s.state = stateReady
	return s.send(Success(nil))
}

// handleReset interrupts current work and returns to READY (doc 18 §5.2).
func (s *session) handleReset() error {
	s.rollbackTx()
	// RESET before authentication is a protocol misuse, but treat it leniently by
	// returning to the appropriate pre-auth state rather than failing hard.
	if s.state == stateNegotiation || s.state == stateAuthentication || s.state == stateDefunct {
		return s.send(Success(nil))
	}
	s.state = stateReady
	return s.send(Success(nil))
}

// handleRun runs a query, auto-commit in READY or within the explicit
// transaction in TX_READY (doc 18 §5.6).
func (s *session) handleRun(r Run) error {
	params, err := decodeParams(r.Params)
	if err != nil {
		return s.fail("Neo.ClientError.Statement.TypeError", err.Error())
	}

	autocommit := s.state == stateReady
	tx := s.tx
	if autocommit {
		tx, err = s.srv.Handler.Begin(r.Extra, s.auth)
		if err != nil {
			return s.failFrom(err)
		}
	}
	cursor, err := tx.Run(r.Query, params)
	if err != nil {
		if autocommit {
			_ = tx.Rollback()
		}
		return s.failFrom(err)
	}

	s.tx = tx
	s.cursor = cursor
	s.autocommit = autocommit
	s.hasPending = false
	if autocommit {
		s.state = stateStreaming
	} else {
		s.state = stateTxStreaming
	}

	fields := make([]any, len(cursor.Fields()))
	for i, f := range cursor.Fields() {
		fields[i] = f
	}
	return s.send(Success(map[string]any{"fields": fields}))
}

// handleBegin opens an explicit transaction (doc 18 §5.10).
func (s *session) handleBegin(b Begin) error {
	tx, err := s.srv.Handler.Begin(b.Extra, s.auth)
	if err != nil {
		return s.failFrom(err)
	}
	s.tx = tx
	s.autocommit = false
	s.state = stateTxReady
	return s.send(Success(nil))
}

// handleCommit commits the explicit transaction (doc 18 §5.10).
func (s *session) handleCommit() error {
	bookmark, err := s.tx.Commit()
	s.tx = nil
	s.state = stateReady
	if err != nil {
		return s.failFrom(err)
	}
	return s.send(Success(map[string]any{"bookmark": bookmark}))
}

// handleRollback rolls back the explicit transaction (doc 18 §5.10).
func (s *session) handleRollback() error {
	err := s.tx.Rollback()
	s.tx = nil
	s.state = stateReady
	if err != nil {
		return s.failFrom(err)
	}
	return s.send(Success(nil))
}

// handleRoute answers a routing request with the single-node table (doc 18 §7.2,
// §7.3).
func (s *session) handleRoute(r Route) error {
	db := s.srv.database()
	if d, ok := r.Extra["db"].(string); ok && d != "" {
		db = d
	}
	addr := s.srv.address()
	one := func(role string) map[string]any {
		return map[string]any{"role": role, "addresses": []any{addr}}
	}
	rt := map[string]any{
		"ttl": int64(300),
		"db":  db,
		"servers": []any{
			one("ROUTE"), one("READ"), one("WRITE"),
		},
	}
	return s.send(Success(map[string]any{"rt": rt}))
}

// handlePull streams (PULL) or drains (DISCARD) up to n rows, then sends the
// terminating SUCCESS (doc 18 §5.7, §5.8). n < 0 means all remaining.
func (s *session) handlePull(n int64, emit bool) error {
	mat := s.tx.Materializer()
	var emitted int64
	for n < 0 || emitted < n {
		row, ok, err := s.nextRow()
		if err != nil {
			return s.failStream(err)
		}
		if !ok {
			return s.finishStream(false)
		}
		if emit {
			values := make([]any, len(row))
			for i, v := range row {
				ev, eerr := EncodeValue(v, s.version, mat)
				if eerr != nil {
					return s.failStream(eerr)
				}
				values[i] = ev
			}
			if werr := s.send(Record(values)); werr != nil {
				return werr
			}
		}
		emitted++
	}
	// Hit the n limit: is there more?
	row, ok, err := s.nextRow()
	if err != nil {
		return s.failStream(err)
	}
	if !ok {
		return s.finishStream(false)
	}
	s.pushBack(row)
	return s.finishStream(true)
}

// finishStream sends the terminating SUCCESS for a PULL/DISCARD batch (doc 18
// §5.7). When hasMore, the stream stays open; otherwise it closes, an auto-commit
// transaction commits, and the connection returns to READY/TX_READY.
func (s *session) finishStream(hasMore bool) error {
	if hasMore {
		return s.send(Success(map[string]any{"has_more": true}))
	}

	summary := s.cursor.Summary()
	meta := map[string]any{}
	if summary.Type != "" {
		meta["type"] = summary.Type
	}
	if len(summary.Stats) > 0 {
		meta["stats"] = summary.Stats
	}
	meta["db"] = s.srv.database()

	_ = s.cursor.Close()
	s.cursor = nil

	if s.autocommit {
		bookmark, err := s.tx.Commit()
		s.tx = nil
		s.state = stateReady
		if err != nil {
			return s.failFrom(err)
		}
		meta["bookmark"] = bookmark
	} else {
		s.state = stateTxReady
	}
	return s.send(Success(meta))
}

// nextRow returns the next result row, honoring a pushed-back lookahead row.
func (s *session) nextRow() ([]value.Value, bool, error) {
	if s.hasPending {
		row := s.pending
		s.pending = nil
		s.hasPending = false
		return row, true, nil
	}
	return s.cursor.Next()
}

// pushBack stores a row read for the has-more lookahead so the next PULL returns
// it.
func (s *session) pushBack(row []value.Value) {
	s.pending = row
	s.hasPending = true
}

// failStream fails an active stream: roll back an auto-commit transaction, then
// report the failure.
func (s *session) failStream(err error) error {
	if s.cursor != nil {
		_ = s.cursor.Close()
		s.cursor = nil
	}
	if s.autocommit {
		s.rollbackTx()
	}
	return s.failFrom(err)
}

// rollbackTx rolls back and clears any open transaction.
func (s *session) rollbackTx() {
	if s.cursor != nil {
		_ = s.cursor.Close()
		s.cursor = nil
	}
	if s.tx != nil {
		_ = s.tx.Rollback()
		s.tx = nil
	}
	s.hasPending = false
}

// cleanup releases connection resources on close (doc 18 §8.7): an open
// transaction is rolled back.
func (s *session) cleanup() {
	s.rollbackTx()
	s.state = stateDefunct
}

// fail sends a FAILURE with a code and message and moves to FAILED (doc 18 §5.2,
// §12).
func (s *session) fail(code, message string) error {
	s.state = stateFailed
	return s.send(Failure(code, message))
}

// failFrom sends a FAILURE derived from a handler error: a StatusError carries
// its own code, anything else is a generic database error (doc 18 §12).
func (s *session) failFrom(err error) error {
	var se *StatusError
	if errors.As(err, &se) {
		return s.fail(se.Code, se.Message)
	}
	return s.fail(codeUnknown, err.Error())
}

// send marshals a reply structure and writes it as one framed message.
func (s *session) send(st pack.Structure) error {
	body, err := pack.Marshal(st)
	if err != nil {
		return err
	}
	return s.w.WriteMessage(body)
}

// authFields pulls the scheme/principal/credentials from an auth or HELLO extra
// map (doc 18 §5.4, §5.5), defaulting the scheme to "none".
func authFields(m map[string]any) (scheme, principal, credentials string) {
	scheme = "none"
	if v, ok := m["scheme"].(string); ok && v != "" {
		scheme = v
	}
	principal, _ = m["principal"].(string)
	credentials, _ = m["credentials"].(string)
	return scheme, principal, credentials
}

// decodeParams decodes a RUN parameters map into gr values (doc 18 §6.9).
func decodeParams(m map[string]any) (map[string]value.Value, error) {
	out := make(map[string]value.Value, len(m))
	for k, v := range m {
		dv, err := DecodeParam(v)
		if err != nil {
			return nil, err
		}
		out[k] = dv
	}
	return out, nil
}
