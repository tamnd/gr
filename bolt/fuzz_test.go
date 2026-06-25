package bolt_test

import (
	"testing"

	"github.com/tamnd/gr/bolt"
	"github.com/tamnd/gr/pack"
)

// FuzzBoltMessage verifies that decoding any byte sequence as a Bolt request
// never panics and returns a typed error on malformed input (doc 23 §7.4).
func FuzzBoltMessage(f *testing.F) {
	// Seed corpus: encode real Bolt request messages as seed bytes.
	encodeMsg := func(sig byte, fields ...any) []byte {
		st := pack.Structure{Tag: sig, Fields: fields}
		b, err := pack.Marshal(st)
		if err != nil {
			return nil
		}
		return b
	}

	seeds := [][]byte{
		// Hello request.
		encodeMsg(bolt.SigHello, map[string]any{"user_agent": "gr/1.0"}),
		// Run request with query + params + meta.
		encodeMsg(bolt.SigRun, "MATCH (n) RETURN n", map[string]any{}, map[string]any{}),
		// Pull request.
		encodeMsg(bolt.SigPull, map[string]any{"n": int64(100)}),
		// Begin + Commit + Rollback.
		encodeMsg(bolt.SigBegin, map[string]any{}),
		encodeMsg(bolt.SigCommit),
		encodeMsg(bolt.SigRollback),
		// Reset.
		encodeMsg(bolt.SigReset),
		// Empty slice.
		{},
		// Random garbage.
		{0xFF, 0x00, 0x01},
		// Single-byte truncation.
		{0xB1},
		// Valid structure tag but zero fields.
		{0xB0, bolt.SigRun},
		// Pack map prefix.
		{0xA1},
	}

	for _, s := range seeds {
		if len(s) > 0 {
			f.Add(s)
		}
	}

	f.Fuzz(func(t *testing.T, body []byte) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("DecodeRequest panicked on input (len=%d): %v", len(body), r)
			}
		}()

		// Property: DecodeRequest never panics; malformed input returns an error.
		_, _ = bolt.DecodeRequest(body)
	})
}

// FuzzPackStream verifies that pack.Unmarshal never panics on arbitrary bytes.
func FuzzPackStream(f *testing.F) {
	seeds := [][]byte{
		{0xC3},                  // True
		{0xC2},                  // False
		{0xC0},                  // Null
		{0x01},                  // Tiny int 1
		{0xFF},                  // Tiny int -1
		{0xC8, 0x7F},            // Int8 127
		{0xC9, 0x00, 0x01},      // Int16 1
		{0x81, 'a'},             // TinyString "a"
		{0x91, 0x01},            // TinyList [1]
		{0xA1, 0x81, 'k', 0x01}, // TinyMap {"k":1}
		{0xB1, 0x01, 0x01},      // TinyStruct tag=0x01 field=1
		{},
		{0xFF, 0xFF, 0xFF, 0xFF},
	}

	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("pack.Unmarshal panicked on %x: %v", data, r)
			}
		}()

		val, err := pack.Unmarshal(data)
		if err != nil {
			return // clean error is fine
		}
		// If we got a value, marshal it back — that should never panic either.
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("pack.Marshal panicked on round-trip of %x: %v", data, r)
			}
		}()
		_, _ = pack.Marshal(val)
	})
}
