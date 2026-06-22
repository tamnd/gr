package bolt

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// rw pairs a reader (the bytes the client sent) with a writer (what the server
// replies), the two halves of a connection for testing Handshake.
type rw struct {
	r *bytes.Reader
	w *bytes.Buffer
}

func newRW(client []byte) *rw {
	return &rw{r: bytes.NewReader(client), w: &bytes.Buffer{}}
}

func (c *rw) Read(p []byte) (int, error)  { return c.r.Read(p) }
func (c *rw) Write(p []byte) (int, error) { return c.w.Write(p) }

// hex builds a byte slice from space-separated hex pairs, the form the spec
// writes its handshake traces in (doc 18 §3.1).
func hex(t *testing.T, s string) []byte {
	t.Helper()
	out := []byte{}
	for _, f := range bytes.Fields([]byte(s)) {
		if len(f) != 2 {
			t.Fatalf("bad hex %q", f)
		}
		var b byte
		for _, c := range f {
			b <<= 4
			switch {
			case c >= '0' && c <= '9':
				b |= byte(c - '0')
			case c >= 'A' && c <= 'F':
				b |= byte(c-'A') + 10
			case c >= 'a' && c <= 'f':
				b |= byte(c-'a') + 10
			default:
				t.Fatalf("bad hex %q", f)
			}
		}
		out = append(out, b)
	}
	return out
}

// TestHandshakeSpecTrace runs the exact trace from doc 18 §3.1: a modern driver
// offering 5.4 down to 5.1 (range 3), three padding slots, and the server
// selecting 5.4 with the reply 00 00 04 05.
func TestHandshakeSpecTrace(t *testing.T) {
	client := hex(t, "60 60 B0 17 00 03 04 05 00 00 00 00 00 00 00 00 00 00 00 00")
	conn := newRW(client)
	v, err := Handshake(conn)
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	if v != (Version{5, 4}) {
		t.Errorf("selected %s, want 5.4", v)
	}
	if want := hex(t, "00 00 04 05"); !bytes.Equal(conn.w.Bytes(), want) {
		t.Errorf("reply % X, want % X", conn.w.Bytes(), want)
	}
}

// TestHandshakeSelectsHighestInRange confirms that within a range proposal the
// server takes the highest minor it supports, not the lowest (doc 18 §3.1).
func TestHandshakeSelectsHighestInRange(t *testing.T) {
	// Propose 5.6 down to 5.3 (range 3). gr supports all four, so it picks 5.6.
	client := hex(t, "60 60 B0 17 00 03 06 05 00 00 00 00 00 00 00 00 00 00 00 00")
	conn := newRW(client)
	v, err := Handshake(conn)
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	if v != (Version{5, 6}) {
		t.Errorf("selected %s, want 5.6", v)
	}
}

// TestHandshakeSkipsUnsupportedMinor confirms the range walk skips a minor gr
// does not support (5.5) and lands on the next one down it does (5.4).
func TestHandshakeSkipsUnsupportedMinor(t *testing.T) {
	// Propose 5.5 down to 5.2. gr has no 5.5, so 5.4 is the highest in range.
	client := hex(t, "60 60 B0 17 00 03 05 05 00 00 00 00 00 00 00 00 00 00 00 00")
	conn := newRW(client)
	v, err := Handshake(conn)
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	if v != (Version{5, 4}) {
		t.Errorf("selected %s, want 5.4", v)
	}
}

// TestHandshakeProposalOrder confirms the server honors the client's preference
// order: the first proposal it can satisfy wins, even if a later proposal names
// a higher version.
func TestHandshakeProposalOrder(t *testing.T) {
	// First proposal 5.2 (exact), second 5.4 (exact). Client prefers 5.2.
	client := hex(t, "60 60 B0 17 00 00 02 05 00 00 04 05 00 00 00 00 00 00 00 00")
	conn := newRW(client)
	v, err := Handshake(conn)
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	if v != (Version{5, 2}) {
		t.Errorf("selected %s, want 5.2 (client preference order)", v)
	}
}

// TestHandshakeFloor confirms a driver offering only versions below the 4.4 floor
// gets the 00 00 00 00 no-version reply and a clean error (doc 18 §3.1, §15.1).
func TestHandshakeFloor(t *testing.T) {
	// Propose 4.2 down to 4.0, all below gr's floor of 4.4.
	client := hex(t, "60 60 B0 17 00 02 02 04 00 00 00 00 00 00 00 00 00 00 00 00")
	conn := newRW(client)
	_, err := Handshake(conn)
	if !errors.Is(err, ErrNoCompatibleVersion) {
		t.Fatalf("err = %v, want ErrNoCompatibleVersion", err)
	}
	if want := hex(t, "00 00 00 00"); !bytes.Equal(conn.w.Bytes(), want) {
		t.Errorf("reply % X, want % X", conn.w.Bytes(), want)
	}
}

// TestHandshakeManifestSentinelFallsBack confirms gr ignores the 5.7 manifest
// sentinel (00 00 01 FF) and selects from the real proposals beside it (doc 18
// §3.2): a driver sends three real proposals alongside the sentinel.
func TestHandshakeManifestSentinelFallsBack(t *testing.T) {
	// Sentinel first, then a real proposal for 5.4.
	client := hex(t, "60 60 B0 17 00 00 01 FF 00 00 04 05 00 00 00 00 00 00 00 00")
	conn := newRW(client)
	v, err := Handshake(conn)
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	if v != (Version{5, 4}) {
		t.Errorf("selected %s, want 5.4 (manifest fell back to legacy)", v)
	}
}

// TestHandshakeNotBolt confirms a connection that does not begin with the magic
// preamble is rejected with ErrNotBolt and no reply written (doc 18 §3.1).
func TestHandshakeNotBolt(t *testing.T) {
	client := hex(t, "47 45 54 20 2F 20 48 54 54 50 00 00 00 00 00 00 00 00 00 00") // "GET / HTTP..."
	conn := newRW(client)
	_, err := Handshake(conn)
	if !errors.Is(err, ErrNotBolt) {
		t.Fatalf("err = %v, want ErrNotBolt", err)
	}
	if conn.w.Len() != 0 {
		t.Errorf("wrote % X to a non-Bolt connection, want nothing", conn.w.Bytes())
	}
}

// TestHandshakeShortRead confirms a truncated handshake is a clean error, not a
// panic, whether the preamble or the proposals are cut short.
func TestHandshakeShortRead(t *testing.T) {
	for _, in := range []string{"60 60", "60 60 B0 17 00 03 04"} {
		conn := newRW(hex(t, in))
		if _, err := Handshake(conn); err == nil {
			t.Errorf("input %q handshook without error", in)
		}
	}
}

// TestVersionWord confirms a version renders to its 4-byte reply word and its
// string form.
func TestVersionWord(t *testing.T) {
	v := Version{5, 4}
	if w := v.word(); !bytes.Equal(w[:], hex(t, "00 00 04 05")) {
		t.Errorf("word % X, want 00 00 04 05", w)
	}
	if v.String() != "5.4" {
		t.Errorf("string %q, want 5.4", v.String())
	}
}

// readWriterCheck keeps the io import in use for the interface assertion below.
var _ io.ReadWriter = (*rw)(nil)
