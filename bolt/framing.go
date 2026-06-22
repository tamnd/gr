package bolt

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
)

// The framing constants (doc 18 §3.3, §13.4).
const (
	// MaxChunkPayload is the largest payload a single chunk can carry: the chunk
	// length is an unsigned 16-bit big-endian integer, so a payload is at most
	// 0xFFFF bytes (doc 18 §3.3). A message larger than this is split across
	// several chunks.
	MaxChunkPayload = 0xFFFF

	// DefaultChunkSize is the payload size gr chunks an outbound message at by
	// default (doc 18 §3.3): a large RECORD or result streams in fixed-size
	// chunks without buffering the whole serialized message. It is configurable
	// through server.bolt.chunk_size (doc 24); 16 KiB is the default.
	DefaultChunkSize = 16 * 1024

	// DefaultMaxMessageSize is the largest message the framing reader accepts
	// before aborting (doc 18 §3.3, §13.4): the reader sums chunk lengths and
	// rejects a message past this bound so a hostile "huge message" cannot force
	// unbounded buffering. It is configurable through server.bolt.max_message_size
	// (doc 24); 4 MiB is the default.
	DefaultMaxMessageSize = 4 * 1024 * 1024
)

// ErrMessageTooLarge is returned by a reader when an inbound message's chunk
// lengths sum past the configured maximum (doc 18 §3.3, §13.4). The session
// layer maps it to Neo.ClientError.Request.InvalidFormat and closes the
// connection.
type ErrMessageTooLarge struct {
	Max int
}

func (e ErrMessageTooLarge) Error() string {
	return fmt.Sprintf("bolt: message exceeds the %d-byte maximum", e.Max)
}

// ErrEmptyMessage is returned when a message is just the end-of-message marker
// with no chunks before it (doc 18 §3.3 rule 5): an empty message is not valid,
// the smallest message is one chunk carrying a one-byte structure header.
var ErrEmptyMessage = fmt.Errorf("bolt: empty message (end-of-message marker with no chunks)")

// ChunkReader reads chunked Bolt messages from a connection (doc 18 §3.3). A
// message is one or more length-prefixed chunks followed by the zero-length
// end-of-message marker; ReadMessage returns the concatenated payload, which the
// PackStream decoder then parses as one structure.
type ChunkReader struct {
	r       *bufio.Reader
	max     int
	hdr     [2]byte
	scratch []byte
}

// NewChunkReader returns a reader over r that rejects any message whose chunk
// lengths sum past maxMessage bytes. A non-positive maxMessage uses
// DefaultMaxMessageSize.
func NewChunkReader(r io.Reader, maxMessage int) *ChunkReader {
	if maxMessage <= 0 {
		maxMessage = DefaultMaxMessageSize
	}
	return &ChunkReader{r: bufio.NewReader(r), max: maxMessage}
}

// ReadMessage reads one whole message: chunks until the zero-length terminator,
// concatenating their payloads. It returns the message body, or an error if a
// chunk is truncated, the message is empty, or the running size exceeds the
// configured maximum. The returned slice is reused across calls, so a caller that
// keeps a message past the next ReadMessage must copy it.
func (cr *ChunkReader) ReadMessage() ([]byte, error) {
	cr.scratch = cr.scratch[:0]
	chunks := 0
	for {
		if _, err := io.ReadFull(cr.r, cr.hdr[:]); err != nil {
			return nil, err
		}
		n := int(binary.BigEndian.Uint16(cr.hdr[:]))
		if n == 0 {
			// The end-of-message marker. A message with no chunks before it is
			// not valid (doc 18 §3.3 rule 5).
			if chunks == 0 {
				return nil, ErrEmptyMessage
			}
			return cr.scratch, nil
		}
		if len(cr.scratch)+n > cr.max {
			return nil, ErrMessageTooLarge{Max: cr.max}
		}
		start := len(cr.scratch)
		cr.scratch = append(cr.scratch, make([]byte, n)...)
		if _, err := io.ReadFull(cr.r, cr.scratch[start:]); err != nil {
			return nil, err
		}
		chunks++
	}
}

// ChunkWriter writes chunked Bolt messages to a connection (doc 18 §3.3). It
// splits an outbound message body into chunks of at most its configured size,
// each prefixed with its 2-byte big-endian length, and closes the message with
// the zero-length end-of-message marker.
type ChunkWriter struct {
	w    *bufio.Writer
	size int
	hdr  [2]byte
}

// NewChunkWriter returns a writer over w that chunks messages at chunkSize bytes
// of payload. A non-positive chunkSize uses DefaultChunkSize; a chunkSize above
// MaxChunkPayload is capped there, since a chunk length cannot exceed 16 bits.
func NewChunkWriter(w io.Writer, chunkSize int) *ChunkWriter {
	if chunkSize <= 0 {
		chunkSize = DefaultChunkSize
	}
	if chunkSize > MaxChunkPayload {
		chunkSize = MaxChunkPayload
	}
	return &ChunkWriter{w: bufio.NewWriter(w), size: chunkSize}
}

// WriteMessage frames body as one Bolt message: the payload split into chunks of
// at most the writer's chunk size, each length-prefixed, followed by the
// zero-length end-of-message marker. An empty body is not a valid message
// (doc 18 §3.3 rule 5) and is rejected. The message is flushed before return so
// a single RECORD or reply reaches the socket without waiting for the next one.
func (cw *ChunkWriter) WriteMessage(body []byte) error {
	if len(body) == 0 {
		return ErrEmptyMessage
	}
	for len(body) > 0 {
		n := len(body)
		if n > cw.size {
			n = cw.size
		}
		binary.BigEndian.PutUint16(cw.hdr[:], uint16(n))
		if _, err := cw.w.Write(cw.hdr[:]); err != nil {
			return err
		}
		if _, err := cw.w.Write(body[:n]); err != nil {
			return err
		}
		body = body[n:]
	}
	// The end-of-message marker: a zero-length chunk (doc 18 §3.3 rule 2).
	cw.hdr[0], cw.hdr[1] = 0, 0
	if _, err := cw.w.Write(cw.hdr[:]); err != nil {
		return err
	}
	return cw.w.Flush()
}
