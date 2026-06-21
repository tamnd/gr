package colcodec

import (
	"bytes"
	"compress/flate"
	"io"

	"github.com/tamnd/gr/format"
)

// This file adds the block second stage of doc 03 §15.5 and doc 15 §17: a
// general-purpose byte compressor applied over an already-lightweight-encoded
// segment body. The lightweight codecs keep a column scannable, so they are the
// primary mechanism; the block stage is for the residual, a body that is still large
// despite its lightweight form (a high-cardinality string heap, a RAW float column
// with byte-level redundancy), where a general compressor finds cross-value
// redundancy the type-aware codecs cannot (doc 15 §17, ADR-24).
//
// A BLOCK segment wraps a complete inner segment: its body is a block-algorithm id,
// the inner segment's uncompressed length, the compressed length, then the
// compressed bytes. On decode the bytes inflate back to the inner segment, which the
// matching decoder then decodes, so block wrapping composes over every value type.
// The compressor is DEFLATE from the standard library, which is pure Go and needs no
// cgo (doc 21), so the build stays CGO_ENABLED=0.
//
// The block stage is never part of the default cascade (doc 15 §12.7): Encode and
// the string and float encoders always return an unwrapped lightweight segment. The
// checkpoint encoder calls Block over a chosen body and keeps the wrapped form only
// when it measures a real win, which is the effort-gated policy doc 15 §12.7 names.

// blockDeflate is the block-algorithm id for DEFLATE, the one algorithm this version
// writes. The id is stored in the segment so a future version can add another
// algorithm without breaking readers of files written now (doc 15 §19).
const blockDeflate uint8 = 1

// blockFloor is the smallest body worth attempting to block-wrap. A small body
// (CONSTANT, a tight FOR, a long-run RLE) has no redundancy a general compressor can
// exploit and the wrapper header would only add bytes, so Block leaves it alone (doc
// 15 §12.7). The strict-smaller check is the real guard; this just avoids pointless
// compress attempts.
const blockFloor = 64

// Block offers the block second stage over an already-encoded segment. It returns
// the block-wrapped segment and true when wrapping strictly shrinks it, and the
// original segment and false otherwise, so a caller can apply it unconditionally and
// never grow a body. A body below blockFloor is returned unwrapped without a compress
// attempt.
func Block(seg []byte) ([]byte, bool) {
	if len(seg) < blockFloor {
		return seg, false
	}
	wrapped := wrapBlock(seg)
	if len(wrapped) < len(seg) {
		return wrapped, true
	}
	return seg, false
}

// wrapBlock builds a BLOCK segment around a complete inner segment.
func wrapBlock(inner []byte) []byte {
	var buf bytes.Buffer
	w, _ := flate.NewWriter(&buf, flate.BestCompression)
	// A bytes.Buffer never fails a write and a valid level never fails NewWriter, so
	// these errors cannot occur; Close flushes the final block.
	_, _ = w.Write(inner)
	_ = w.Close()
	comp := buf.Bytes()
	dst := []byte{byte(BLOCK), blockDeflate}
	dst = format.AppendUvarint(dst, uint64(len(inner)))
	dst = format.AppendUvarint(dst, uint64(len(comp)))
	return append(dst, comp...)
}

// unwrapBlock reverses wrapBlock: it reads the body after the BLOCK codec byte,
// inflates the compressed bytes, and returns the inner segment. It returns
// ErrBadSegment on any malformed input, including a length that does not match the
// inflated size and a doubly-wrapped block (the encoder never nests BLOCK, so a
// nested one is corruption, and rejecting it bounds decode recursion to one level).
func unwrapBlock(b []byte) ([]byte, error) {
	if len(b) < 1 || b[0] != blockDeflate {
		return nil, ErrBadSegment
	}
	off := 1
	rawLen, n, err := format.Uvarint(b[off:])
	if err != nil || rawLen > uint64(maxInt) {
		return nil, ErrBadSegment
	}
	off += n
	compLen, n2, err := format.Uvarint(b[off:])
	if err != nil || compLen > uint64(maxInt) {
		return nil, ErrBadSegment
	}
	off += n2
	if len(b)-off < int(compLen) {
		return nil, ErrBadSegment
	}
	r := flate.NewReader(bytes.NewReader(b[off : off+int(compLen)]))
	inner, err := io.ReadAll(io.LimitReader(r, int64(rawLen)+1))
	_ = r.Close()
	if err != nil || uint64(len(inner)) != rawLen {
		return nil, ErrBadSegment
	}
	if len(inner) > 0 && Codec(inner[0]) == BLOCK {
		return nil, ErrBadSegment
	}
	return inner, nil
}
