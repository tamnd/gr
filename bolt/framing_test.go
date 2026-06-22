package bolt

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// TestWriteMessageSpecTrace frames the RUN body from doc 18 §3.4 and checks the
// bytes: a 2-byte length 00 13 (19), the 19-byte payload, then the 00 00
// end-of-message marker.
func TestWriteMessageSpecTrace(t *testing.T) {
	// B3 10 88 "RETURN 1" A0 A0 is the 19-byte RUN body (doc 18 §3.4, §5.6).
	body := hex(t, "B3 10 88 52 45 54 55 52 4E 20 31 A0 A0")
	var buf bytes.Buffer
	cw := NewChunkWriter(&buf, 0)
	if err := cw.WriteMessage(body); err != nil {
		t.Fatalf("write: %v", err)
	}
	want := hex(t, "00 0D B3 10 88 52 45 54 55 52 4E 20 31 A0 A0 00 00")
	if !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("framed % X, want % X", buf.Bytes(), want)
	}
}

// TestRoundTripSingleChunk writes a message and reads it back through the chunk
// reader, confirming the body survives framing.
func TestRoundTripSingleChunk(t *testing.T) {
	body := []byte("the quick brown fox")
	var buf bytes.Buffer
	if err := NewChunkWriter(&buf, 0).WriteMessage(body); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := NewChunkReader(&buf, 0).ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("read % X, want % X", got, body)
	}
}

// TestRoundTripMultiChunk writes a body larger than the chunk size and confirms
// it is split across several chunks and reassembled by the reader (doc 18 §3.3
// rule 1: chunk boundaries are not semantically meaningful).
func TestRoundTripMultiChunk(t *testing.T) {
	body := bytes.Repeat([]byte{0xAB}, 10000)
	var buf bytes.Buffer
	// Chunk at 4096 bytes: the 10000-byte body needs three chunks.
	if err := NewChunkWriter(&buf, 4096).WriteMessage(body); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Expect chunk headers 0x1000, 0x1000, 0x710 then 0x0000: 3 data chunks.
	framed := buf.Bytes()
	if len(framed) != 10000+3*2+2 {
		t.Errorf("framed length %d, want %d", len(framed), 10000+3*2+2)
	}
	got, err := NewChunkReader(&buf, 0).ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("multi-chunk round trip differs")
	}
}

// TestChunkSizeCappedAtMax confirms a requested chunk size above the 16-bit limit
// is capped at MaxChunkPayload, since a chunk length cannot exceed 0xFFFF.
func TestChunkSizeCappedAtMax(t *testing.T) {
	cw := NewChunkWriter(io.Discard, 1<<20)
	if cw.size != MaxChunkPayload {
		t.Errorf("chunk size %d, want %d", cw.size, MaxChunkPayload)
	}
}

// TestPipelinedMessages confirms two messages written back to back are read back
// in order, the RUN-then-PULL pattern a driver pipelines (doc 18 §3.3 rule 4).
func TestPipelinedMessages(t *testing.T) {
	var buf bytes.Buffer
	cw := NewChunkWriter(&buf, 0)
	if err := cw.WriteMessage([]byte("first")); err != nil {
		t.Fatalf("write 1: %v", err)
	}
	if err := cw.WriteMessage([]byte("second")); err != nil {
		t.Fatalf("write 2: %v", err)
	}
	cr := NewChunkReader(&buf, 0)
	for _, want := range []string{"first", "second"} {
		got, err := cr.ReadMessage()
		if err != nil {
			t.Fatalf("read %q: %v", want, err)
		}
		if string(got) != want {
			t.Errorf("read %q, want %q", got, want)
		}
	}
}

// TestReadEmptyMessage confirms a bare end-of-message marker (00 00) with no
// chunks before it is rejected (doc 18 §3.3 rule 5).
func TestReadEmptyMessage(t *testing.T) {
	cr := NewChunkReader(bytes.NewReader(hex(t, "00 00")), 0)
	if _, err := cr.ReadMessage(); !errors.Is(err, ErrEmptyMessage) {
		t.Errorf("err = %v, want ErrEmptyMessage", err)
	}
}

// TestWriteEmptyMessage confirms an empty body is not a valid message and is
// rejected by the writer (doc 18 §3.3 rule 5).
func TestWriteEmptyMessage(t *testing.T) {
	if err := NewChunkWriter(io.Discard, 0).WriteMessage(nil); !errors.Is(err, ErrEmptyMessage) {
		t.Errorf("err = %v, want ErrEmptyMessage", err)
	}
}

// TestReadMessageTooLarge confirms the reader aborts a message whose chunk
// lengths sum past the configured maximum (doc 18 §3.3, §13.4).
func TestReadMessageTooLarge(t *testing.T) {
	var buf bytes.Buffer
	// Two 100-byte chunks (200 bytes) against a 150-byte max.
	NewChunkWriter(&buf, 100).WriteMessage(bytes.Repeat([]byte{1}, 200))
	cr := NewChunkReader(&buf, 150)
	var tooLarge ErrMessageTooLarge
	if _, err := cr.ReadMessage(); !errors.As(err, &tooLarge) {
		t.Errorf("err = %v, want ErrMessageTooLarge", err)
	}
}

// TestReadTruncatedChunk confirms a chunk header that promises more bytes than
// arrive is a clean error, not a panic.
func TestReadTruncatedChunk(t *testing.T) {
	// Header says 5 bytes follow, but only 2 are present.
	cr := NewChunkReader(bytes.NewReader(hex(t, "00 05 41 42")), 0)
	if _, err := cr.ReadMessage(); err == nil {
		t.Error("truncated chunk read without error")
	}
}

// TestReadAtEOF confirms reading at a clean end of stream returns io.EOF, so the
// session loop can tell an orderly client disconnect from a framing error.
func TestReadAtEOF(t *testing.T) {
	cr := NewChunkReader(bytes.NewReader(nil), 0)
	if _, err := cr.ReadMessage(); !errors.Is(err, io.EOF) {
		t.Errorf("err = %v, want io.EOF", err)
	}
}

// TestReaderReusesBuffer documents that the returned slice is reused across
// calls: a caller that needs a message past the next read must copy it. Here we
// confirm two reads of different lengths each return the right content when used
// before the next read.
func TestReaderReusesBuffer(t *testing.T) {
	var buf bytes.Buffer
	cw := NewChunkWriter(&buf, 0)
	cw.WriteMessage([]byte("longer-first-message"))
	cw.WriteMessage([]byte("short"))
	cr := NewChunkReader(&buf, 0)
	first, _ := cr.ReadMessage()
	if string(first) != "longer-first-message" {
		t.Fatalf("first = %q", first)
	}
	second, _ := cr.ReadMessage()
	if string(second) != "short" {
		t.Errorf("second = %q", second)
	}
}
