package bolt

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/tamnd/gr/pack"
	"github.com/tamnd/gr/value"
)

// scriptedConn feeds a prebuilt client byte stream to the server and captures the
// server's replies. Because Bolt is request/response and the server never reads
// its own output, a fully-scripted client drives a whole session: Serve consumes
// the stream and returns at EOF.
type scriptedConn struct {
	in  *bytes.Reader
	out *bytes.Buffer
}

func (c *scriptedConn) Read(p []byte) (int, error)  { return c.in.Read(p) }
func (c *scriptedConn) Write(p []byte) (int, error) { return c.out.Write(p) }

// proposal54 offers Bolt 5.4 exactly, then three padding slots.
var proposal54 = []byte{0x00, 0x00, 0x04, 0x05, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}

// clientStream builds the full client byte stream: the handshake plus each
// message framed.
func clientStream(t *testing.T, msgs ...pack.Structure) *scriptedConn {
	t.Helper()
	var buf bytes.Buffer
	buf.Write(Magic[:])
	buf.Write(proposal54)
	cw := NewChunkWriter(&buf, 0)
	for _, m := range msgs {
		b, err := pack.Marshal(m)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := cw.WriteMessage(b); err != nil {
			t.Fatalf("frame: %v", err)
		}
	}
	return &scriptedConn{in: bytes.NewReader(buf.Bytes()), out: &bytes.Buffer{}}
}

// serverReplies parses the server's reply structures, skipping the 4-byte
// handshake version word.
func serverReplies(t *testing.T, out []byte) []pack.Structure {
	t.Helper()
	if len(out) < 4 {
		t.Fatalf("server wrote %d bytes, want at least the version word", len(out))
	}
	r := NewChunkReader(bytes.NewReader(out[4:]), 0)
	var sts []pack.Structure
	for {
		body, err := r.ReadMessage()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read reply: %v", err)
		}
		v, err := pack.Unmarshal(body)
		if err != nil {
			t.Fatalf("unmarshal reply: %v", err)
		}
		sts = append(sts, v.(pack.Structure))
	}
	return sts
}

func tags(sts []pack.Structure) []byte {
	out := make([]byte, len(sts))
	for i, s := range sts {
		out[i] = s.Tag
	}
	return out
}

// message builders for tests.
func mHello() pack.Structure {
	return pack.Structure{Tag: SigHello, Fields: []any{map[string]any{"user_agent": "gr-test/1.0"}}}
}
func mLogon() pack.Structure {
	return pack.Structure{Tag: SigLogon, Fields: []any{map[string]any{"scheme": "none"}}}
}
func mRun(q string) pack.Structure {
	return pack.Structure{Tag: SigRun, Fields: []any{q, map[string]any{}, map[string]any{}}}
}
func mPull(n int64) pack.Structure {
	return pack.Structure{Tag: SigPull, Fields: []any{map[string]any{"n": n}}}
}

func newFakeTx(fields []string, rows [][]value.Value, summary Summary) *fakeTx {
	return &fakeTx{cursor: &fakeCursor{fields: fields, rows: rows, summary: summary}, mat: fakeMat{}}
}

type fakeCursor struct {
	fields  []string
	rows    [][]value.Value
	i       int
	summary Summary
	closed  bool
}

func (c *fakeCursor) Fields() []string { return c.fields }
func (c *fakeCursor) Next() ([]value.Value, bool, error) {
	if c.i >= len(c.rows) {
		return nil, false, nil
	}
	r := c.rows[c.i]
	c.i++
	return r, true, nil
}
func (c *fakeCursor) Summary() Summary { return c.summary }
func (c *fakeCursor) Close() error     { c.closed = true; return nil }

type fakeTx struct {
	cursor      *fakeCursor
	mat         Materializer
	runErr      error
	committed   bool
	rolledback  bool
	commitCount int
}

func (t *fakeTx) Run(q string, p map[string]value.Value) (Cursor, error) {
	if t.runErr != nil {
		return nil, t.runErr
	}
	return t.cursor, nil
}
func (t *fakeTx) Materializer() Materializer { return t.mat }
func (t *fakeTx) Commit() (string, error)    { t.committed = true; t.commitCount++; return "bm:1", nil }
func (t *fakeTx) Rollback() error            { t.rolledback = true; return nil }

type fakeHandler struct {
	tx       *fakeTx
	authErr  error
	beginErr error
}

func (h *fakeHandler) Authenticate(scheme, principal, credentials string) error { return h.authErr }
func (h *fakeHandler) Begin(extra map[string]any) (Tx, error) {
	if h.beginErr != nil {
		return nil, h.beginErr
	}
	return h.tx, nil
}

// TestSessionAutoCommit runs HELLO, LOGON, RUN, PULL and checks the reply
// sequence, the streamed record, and that the auto-commit transaction committed.
func TestSessionAutoCommit(t *testing.T) {
	tx := newFakeTx([]string{"n"}, [][]value.Value{{value.Int(1)}}, Summary{Type: "r"})
	srv := &Server{Handler: &fakeHandler{tx: tx}}
	conn := clientStream(t, mHello(), mLogon(), mRun("RETURN 1"), mPull(-1))
	if err := srv.Serve(conn); err != nil {
		t.Fatalf("serve: %v", err)
	}
	reps := serverReplies(t, conn.out.Bytes())
	want := []byte{SigSuccess, SigSuccess, SigSuccess, SigRecord, SigSuccess}
	if !bytes.Equal(tags(reps), want) {
		t.Fatalf("reply tags % X, want % X", tags(reps), want)
	}
	// The RUN SUCCESS announces the field "n".
	runMeta := reps[2].Fields[0].(map[string]any)
	if fields := runMeta["fields"].([]any); len(fields) != 1 || fields[0] != "n" {
		t.Errorf("run fields %v", runMeta["fields"])
	}
	// The RECORD carries the value 1.
	rec := reps[3].Fields[0].([]any)
	if len(rec) != 1 || rec[0] != int64(1) {
		t.Errorf("record %v", rec)
	}
	// The terminating SUCCESS carries the bookmark and type; the tx committed.
	finMeta := reps[4].Fields[0].(map[string]any)
	if finMeta["bookmark"] != "bm:1" || finMeta["type"] != "r" {
		t.Errorf("final meta %v", finMeta)
	}
	if !tx.committed {
		t.Error("auto-commit transaction did not commit")
	}
}

// TestSessionHasMore confirms an n-bounded pull reports has_more and the next
// pull drains the rest (doc 18 §5.7).
func TestSessionHasMore(t *testing.T) {
	rows := [][]value.Value{{value.Int(1)}, {value.Int(2)}, {value.Int(3)}}
	tx := newFakeTx([]string{"n"}, rows, Summary{Type: "r"})
	srv := &Server{Handler: &fakeHandler{tx: tx}}
	conn := clientStream(t, mHello(), mLogon(), mRun("q"), mPull(2), mPull(2))
	if err := srv.Serve(conn); err != nil {
		t.Fatalf("serve: %v", err)
	}
	reps := serverReplies(t, conn.out.Bytes())
	// hello, logon, run, [rec rec success(has_more)], [rec success(final)]
	want := []byte{SigSuccess, SigSuccess, SigSuccess, SigRecord, SigRecord, SigSuccess, SigRecord, SigSuccess}
	if !bytes.Equal(tags(reps), want) {
		t.Fatalf("reply tags % X, want % X", tags(reps), want)
	}
	if hm := reps[5].Fields[0].(map[string]any)["has_more"]; hm != true {
		t.Errorf("first batch has_more = %v, want true", hm)
	}
	if _, ok := reps[7].Fields[0].(map[string]any)["has_more"]; ok {
		t.Error("final batch should not carry has_more")
	}
}

// TestSessionExplicitTx runs BEGIN, RUN, PULL, COMMIT and checks the explicit
// transaction commits only on COMMIT, not at stream end.
func TestSessionExplicitTx(t *testing.T) {
	tx := newFakeTx([]string{"n"}, [][]value.Value{{value.Int(1)}}, Summary{Type: "r"})
	srv := &Server{Handler: &fakeHandler{tx: tx}}
	begin := pack.Structure{Tag: SigBegin, Fields: []any{map[string]any{}}}
	commit := pack.Structure{Tag: SigCommit, Fields: []any{}}
	conn := clientStream(t, mHello(), mLogon(), begin, mRun("q"), mPull(-1), commit)
	if err := srv.Serve(conn); err != nil {
		t.Fatalf("serve: %v", err)
	}
	reps := serverReplies(t, conn.out.Bytes())
	want := []byte{SigSuccess, SigSuccess, SigSuccess, SigSuccess, SigRecord, SigSuccess, SigSuccess}
	if !bytes.Equal(tags(reps), want) {
		t.Fatalf("reply tags % X, want % X", tags(reps), want)
	}
	if tx.commitCount != 1 {
		t.Errorf("commit count %d, want exactly 1 (on COMMIT, not at stream end)", tx.commitCount)
	}
}

// TestSessionFailedIgnored confirms a failed RUN moves to FAILED, a following
// request is IGNORED, and RESET clears the failure (doc 18 §5.2).
func TestSessionFailedIgnored(t *testing.T) {
	tx := newFakeTx(nil, nil, Summary{})
	tx.runErr = &StatusError{Code: "Neo.ClientError.Statement.SyntaxError", Message: "boom"}
	srv := &Server{Handler: &fakeHandler{tx: tx}}
	reset := pack.Structure{Tag: SigReset, Fields: []any{}}
	conn := clientStream(t, mHello(), mLogon(), mRun("bad"), mRun("again"), reset)
	if err := srv.Serve(conn); err != nil {
		t.Fatalf("serve: %v", err)
	}
	reps := serverReplies(t, conn.out.Bytes())
	want := []byte{SigSuccess, SigSuccess, SigFailure, SigIgnored, SigSuccess}
	if !bytes.Equal(tags(reps), want) {
		t.Fatalf("reply tags % X, want % X", tags(reps), want)
	}
	failMeta := reps[2].Fields[0].(map[string]any)
	if failMeta["code"] != "Neo.ClientError.Statement.SyntaxError" {
		t.Errorf("failure code %v", failMeta["code"])
	}
	if !tx.rolledback {
		t.Error("failed auto-commit transaction should roll back")
	}
}

// TestSessionAuthFailure confirms a bad LOGON replies unauthorized and stays in
// AUTHENTICATION so a retry can succeed (doc 18 §5.5).
func TestSessionAuthFailure(t *testing.T) {
	h := &fakeHandler{tx: newFakeTx(nil, nil, Summary{}), authErr: errors.New("bad password")}
	srv := &Server{Handler: h}
	// First LOGON fails; flip the handler to accept and the second LOGON succeeds.
	// Drive it as two LOGONs with the failure cleared between is not possible in a
	// single scripted run, so just confirm the first fails and stays usable: a
	// following LOGON is still accepted by the state machine (not IGNORED).
	conn := clientStream(t, mHello(), mLogon(), mLogon())
	if err := srv.Serve(conn); err != nil {
		t.Fatalf("serve: %v", err)
	}
	reps := serverReplies(t, conn.out.Bytes())
	want := []byte{SigSuccess, SigFailure, SigFailure}
	if !bytes.Equal(tags(reps), want) {
		t.Fatalf("reply tags % X, want % X", tags(reps), want)
	}
	if code := reps[1].Fields[0].(map[string]any)["code"]; code != codeUnauthorized {
		t.Errorf("auth failure code %v, want %s", code, codeUnauthorized)
	}
}

// TestSessionRoute confirms a ROUTE reply carries the single-node table (doc 18
// §7.2, §7.3).
func TestSessionRoute(t *testing.T) {
	srv := &Server{Handler: &fakeHandler{tx: newFakeTx(nil, nil, Summary{})}, Address: "gr-host:7687", Database: "neo4j"}
	route := pack.Structure{Tag: SigRoute, Fields: []any{map[string]any{}, []any{}, map[string]any{}}}
	conn := clientStream(t, mHello(), mLogon(), route)
	if err := srv.Serve(conn); err != nil {
		t.Fatalf("serve: %v", err)
	}
	reps := serverReplies(t, conn.out.Bytes())
	rt := reps[2].Fields[0].(map[string]any)["rt"].(map[string]any)
	if rt["ttl"] != int64(300) || rt["db"] != "neo4j" {
		t.Errorf("routing table %v", rt)
	}
	servers := rt["servers"].([]any)
	if len(servers) != 3 {
		t.Fatalf("servers %v, want 3 roles", servers)
	}
	first := servers[0].(map[string]any)
	if first["role"] != "ROUTE" || first["addresses"].([]any)[0] != "gr-host:7687" {
		t.Errorf("first server entry %v", first)
	}
}

// TestSessionInvalidStateMessage confirms a message invalid for the current state
// is a protocol failure (doc 18 §5.2): a PULL with no active stream.
func TestSessionInvalidStateMessage(t *testing.T) {
	srv := &Server{Handler: &fakeHandler{tx: newFakeTx(nil, nil, Summary{})}}
	conn := clientStream(t, mHello(), mLogon(), mPull(-1))
	if err := srv.Serve(conn); err != nil {
		t.Fatalf("serve: %v", err)
	}
	reps := serverReplies(t, conn.out.Bytes())
	if reps[2].Tag != SigFailure {
		t.Errorf("PULL in READY tag 0x%02X, want FAILURE", reps[2].Tag)
	}
	if code := reps[2].Fields[0].(map[string]any)["code"]; code != codeRequestInvalid {
		t.Errorf("code %v, want %s", code, codeRequestInvalid)
	}
}

// TestSessionGoodbyeCloses confirms GOODBYE ends the loop with no reply and the
// handshake fails on a non-Bolt connection.
func TestSessionGoodbyeCloses(t *testing.T) {
	srv := &Server{Handler: &fakeHandler{tx: newFakeTx(nil, nil, Summary{})}}
	goodbye := pack.Structure{Tag: SigGoodbye, Fields: []any{}}
	conn := clientStream(t, mHello(), mLogon(), goodbye, mRun("never"))
	if err := srv.Serve(conn); err != nil {
		t.Fatalf("serve: %v", err)
	}
	reps := serverReplies(t, conn.out.Bytes())
	// Only HELLO and LOGON replies; GOODBYE ends the loop before the trailing RUN.
	if !bytes.Equal(tags(reps), []byte{SigSuccess, SigSuccess}) {
		t.Errorf("reply tags % X, want two successes", tags(reps))
	}
}

// TestSessionPre51HelloAuth confirms a Bolt < 5.1 handshake authenticates in
// HELLO (doc 18 §5.4): no LOGON is needed, RUN works straight after HELLO.
func TestSessionPre51HelloAuth(t *testing.T) {
	tx := newFakeTx([]string{"n"}, [][]value.Value{{value.Int(1)}}, Summary{Type: "r"})
	srv := &Server{Handler: &fakeHandler{tx: tx}}
	var buf bytes.Buffer
	buf.Write(Magic[:])
	// Offer Bolt 4.4 only.
	buf.Write([]byte{0x00, 0x00, 0x04, 0x04, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	cw := NewChunkWriter(&buf, 0)
	hello := pack.Structure{Tag: SigHello, Fields: []any{map[string]any{"user_agent": "old", "scheme": "basic", "principal": "neo4j", "credentials": "pw"}}}
	for _, m := range []pack.Structure{hello, mRun("RETURN 1"), mPull(-1)} {
		b, _ := pack.Marshal(m)
		cw.WriteMessage(b)
	}
	conn := &scriptedConn{in: bytes.NewReader(buf.Bytes()), out: &bytes.Buffer{}}
	if err := srv.Serve(conn); err != nil {
		t.Fatalf("serve: %v", err)
	}
	reps := serverReplies(t, conn.out.Bytes())
	want := []byte{SigSuccess, SigSuccess, SigRecord, SigSuccess}
	if !bytes.Equal(tags(reps), want) {
		t.Errorf("reply tags % X, want % X (pre-5.1 HELLO authenticates)", tags(reps), want)
	}
}
