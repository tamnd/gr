package bolt

import (
	"bytes"
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/tamnd/gr/pack"
	"github.com/tamnd/gr/value"
)

// dialAndWrite opens a TCP connection to addr, writes the whole client byte
// stream, and reads every reply byte the server sends until it closes the
// connection. A trailing GOODBYE in the stream makes the server close, which ends
// the read.
func dialAndWrite(t *testing.T, addr string, stream []byte) []byte {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write(stream); err != nil {
		t.Fatalf("write: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	out, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read replies: %v", err)
	}
	return out
}

// fullClientStream builds a Bolt 5.4 handshake plus the given messages, framed.
func fullClientStream(t *testing.T, msgs ...pack.Structure) []byte {
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
	return buf.Bytes()
}

func startListener(t *testing.T, l *Listener) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go l.Serve(ln)
	t.Cleanup(func() { l.Shutdown(context.Background()) })
	return ln.Addr().String()
}

// TestListenerServesBolt runs a full lifecycle over a real TCP socket: handshake,
// HELLO, LOGON, RUN, PULL, GOODBYE, and confirms the reply sequence.
func TestListenerServesBolt(t *testing.T) {
	tx := newFakeTx([]string{"n"}, [][]value.Value{{value.Int(1)}}, Summary{Type: "r"})
	l := &Listener{Server: &Server{Handler: &fakeHandler{tx: tx}}}
	addr := startListener(t, l)

	goodbye := pack.Structure{Tag: SigGoodbye, Fields: []any{}}
	stream := fullClientStream(t, mHello(), mLogon(), mRun("RETURN 1"), mPull(-1), goodbye)
	out := dialAndWrite(t, addr, stream)

	reps := serverReplies(t, out)
	want := []byte{SigSuccess, SigSuccess, SigSuccess, SigRecord, SigSuccess}
	if !bytes.Equal(tags(reps), want) {
		t.Fatalf("reply tags % X, want % X", tags(reps), want)
	}
	if !tx.committed {
		t.Error("auto-commit transaction did not commit over the wire")
	}
}

// TestListenerHandshakeReply confirms the server writes the negotiated version
// word (Bolt 5.4) before any message.
func TestListenerHandshakeReply(t *testing.T) {
	l := &Listener{Server: &Server{Handler: &fakeHandler{tx: newFakeTx(nil, nil, Summary{})}}}
	addr := startListener(t, l)
	goodbye := pack.Structure{Tag: SigGoodbye, Fields: []any{}}
	out := dialAndWrite(t, addr, fullClientStream(t, mHello(), mLogon(), goodbye))
	if len(out) < 4 {
		t.Fatalf("server wrote %d bytes", len(out))
	}
	if !bytes.Equal(out[:4], []byte{0x00, 0x00, 0x04, 0x05}) {
		t.Errorf("version word % X, want 00 00 04 05 (Bolt 5.4)", out[:4])
	}
}

// TestListenerMaxConnections confirms a connection past the cap is closed without
// a handshake (doc 18 §8.8).
func TestListenerMaxConnections(t *testing.T) {
	tx := newFakeTx(nil, nil, Summary{})
	l := &Listener{Server: &Server{Handler: &fakeHandler{tx: tx}}, MaxConnections: 1}
	addr := startListener(t, l)

	// First connection: handshake and HELLO, then hold it open (no GOODBYE) so it
	// keeps the single slot.
	c1, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial 1: %v", err)
	}
	defer c1.Close()
	if _, err := c1.Write(fullClientStream(t, mHello())); err != nil {
		t.Fatalf("write 1: %v", err)
	}
	// Read the handshake reply and the HELLO SUCCESS so we know c1 holds the slot.
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(c1, hdr); err != nil {
		t.Fatalf("read handshake on c1: %v", err)
	}
	r := NewChunkReader(c1, 0)
	if _, err := r.ReadMessage(); err != nil {
		t.Fatalf("read HELLO reply on c1: %v", err)
	}

	// Second connection: the cap is full, so the server closes it immediately.
	c2, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial 2: %v", err)
	}
	defer c2.Close()
	c2.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(c2, make([]byte, 1)); err == nil {
		t.Error("second connection past the cap should be closed, but it read a byte")
	}
}

// TestListenerOptionalSniffPlaintext confirms an optional-TLS listener routes a
// Bolt magic preamble to the plaintext path (doc 18 §11.1), with no preamble byte
// lost to the sniff.
func TestListenerOptionalSniffPlaintext(t *testing.T) {
	tx := newFakeTx([]string{"n"}, [][]value.Value{{value.Int(1)}}, Summary{Type: "r"})
	l := &Listener{Server: &Server{Handler: &fakeHandler{tx: tx}}, TLSMode: TLSOptional}
	addr := startListener(t, l)
	goodbye := pack.Structure{Tag: SigGoodbye, Fields: []any{}}
	out := dialAndWrite(t, addr, fullClientStream(t, mHello(), mLogon(), mRun("q"), mPull(-1), goodbye))
	reps := serverReplies(t, out)
	want := []byte{SigSuccess, SigSuccess, SigSuccess, SigRecord, SigSuccess}
	if !bytes.Equal(tags(reps), want) {
		t.Errorf("reply tags % X, want % X (preamble byte must survive the sniff)", tags(reps), want)
	}
}

// TestListenerOptionalRejectsUnknown confirms an optional-TLS listener closes a
// connection whose first byte is neither a TLS ClientHello nor the Bolt magic.
func TestListenerOptionalRejectsUnknown(t *testing.T) {
	l := &Listener{Server: &Server{Handler: &fakeHandler{tx: newFakeTx(nil, nil, Summary{})}}, TLSMode: TLSOptional}
	addr := startListener(t, l)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte{0x99, 0x01, 0x02, 0x03}); err != nil {
		t.Fatalf("write: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(conn, make([]byte, 1)); err == nil {
		t.Error("unrecognized preamble should close the connection")
	}
}

// TestListenerShutdownDrains confirms Shutdown returns once a live connection
// finishes, and that Serve then reports the listener is closed.
func TestListenerShutdownDrains(t *testing.T) {
	l := &Listener{Server: &Server{Handler: &fakeHandler{tx: newFakeTx(nil, nil, Summary{})}}}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- l.Serve(ln) }()

	goodbye := pack.Structure{Tag: SigGoodbye, Fields: []any{}}
	_ = dialAndWrite(t, ln.Addr().String(), fullClientStream(t, mHello(), mLogon(), goodbye))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := l.Shutdown(ctx); err != nil {
		t.Errorf("shutdown: %v", err)
	}
	if err := <-serveErr; err != ErrListenerClosed {
		t.Errorf("Serve returned %v, want ErrListenerClosed", err)
	}
}
