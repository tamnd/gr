// Package cred is gr's credential model: the user records the served database
// keeps in its reserved system area (doc 18 §10.3), the salted password hashing
// that protects them, and the wire format they persist in. It is deliberately
// storage-agnostic: it turns a set of records into bytes and back and verifies a
// password against a record, while the engine owns where those bytes live and the
// library API owns the create/alter/drop operations over them.
//
// The hash is salted PBKDF2-HMAC-SHA256. The spec prefers Argon2id or scrypt (doc
// 18 §10.3), but both live outside the standard library and gr is zero-dependency,
// so the built-in store uses stdlib PBKDF2 with a high iteration count, the same
// choice the HTTP StaticProvider already made; the auth-provider seam (doc 18 §10.4)
// is where a deployment plugs a stronger external KDF in.
package cred

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
)

// The role vocabulary (doc 18 §10.6). The set is fixed: it is the privileges a
// single-database server genuinely needs, not a re-implementation of Neo4j's
// privilege graph. Authorization ordering (reader < editor < publisher < admin)
// lives with the server's enforcement; this package only validates membership.
const (
	RoleReader    = "reader"
	RoleEditor    = "editor"
	RolePublisher = "publisher"
	RoleAdmin     = "admin"
)

// ValidRole reports whether name is one of the four built-in roles.
func ValidRole(name string) bool {
	switch name {
	case RoleReader, RoleEditor, RolePublisher, RoleAdmin:
		return true
	default:
		return false
	}
}

// iter is the PBKDF2 iteration count. It matches the HTTP StaticProvider's count so
// a credential hashed here verifies identically there; it is stored per record so a
// future raise applies to new and re-hashed records without stranding old ones.
const iter = 210_000

// saltLen is the per-record random salt length in bytes.
const saltLen = 16

// Record is one user: the name, a per-user random salt, the derived hash, the
// iteration count the hash was derived with, and the granted roles. The plaintext
// password is never stored, only its salted hash.
type Record struct {
	Name  string
	Salt  []byte
	Hash  []byte
	Iter  uint32
	Roles []string
}

// errors the model raises. The library API maps them onto its public sentinels.
var (
	// ErrFormat is returned by Decode when the blob is not a credential store this
	// build understands (a wrong magic byte, a truncated record, an unknown version).
	ErrFormat = errors.New("gr/cred: malformed credential store")
)

// Hash derives a fresh salted hash for a password, drawing a new random salt. It is
// what CreateUser and a password-changing AlterUser call to build a record's secret.
func Hash(password string) (salt, hash []byte, err error) {
	salt = make([]byte, saltLen)
	if _, err = rand.Read(salt); err != nil {
		return nil, nil, err
	}
	hash, err = derive(password, salt, iter)
	if err != nil {
		return nil, nil, err
	}
	return salt, hash, nil
}

// Iter reports the iteration count fresh hashes are derived with, so a caller
// building a Record from Hash records the matching count.
func Iter() uint32 { return iter }

// Verify reports whether password matches the record, using a constant-time compare
// so a wrong password and a right one take the same time on the comparison. A record
// with no hash (a malformed or zero record) never verifies.
func Verify(rec Record, password string) bool {
	if len(rec.Hash) == 0 {
		return false
	}
	got, err := derive(password, rec.Salt, int(rec.Iter))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, rec.Hash) == 1
}

// derive computes the PBKDF2-HMAC-SHA256 hash of a password with a salt.
func derive(password string, salt []byte, iterations int) ([]byte, error) {
	if iterations <= 0 {
		iterations = iter
	}
	return pbkdf2.Key(sha256.New, password, salt, iterations, 32)
}

// the wire format. Version 1: a magic byte, then a big-endian record count, then
// each record as length-prefixed fields. Records are written sorted by name so the
// encoding is stable for a given set and a round trip preserves order.
const (
	magic   = 0xC9 // distinguishes a credential blob from an empty/foreign section
	version = 1
)

// Encode serializes a set of records into the credential blob. It sorts a copy by
// name first, so the byte output depends only on the set, not on insertion order.
func Encode(recs []Record) []byte {
	sorted := slices.Clone(recs)
	slices.SortFunc(sorted, func(a, b Record) int {
		if a.Name < b.Name {
			return -1
		}
		if a.Name > b.Name {
			return 1
		}
		return 0
	})
	var b []byte
	b = append(b, magic, version)
	b = binary.BigEndian.AppendUint32(b, uint32(len(sorted)))
	for _, r := range sorted {
		b = appendBytes16(b, []byte(r.Name))
		b = appendBytes8(b, r.Salt)
		b = appendBytes8(b, r.Hash)
		b = binary.BigEndian.AppendUint32(b, r.Iter)
		b = binary.BigEndian.AppendUint16(b, uint16(len(r.Roles)))
		for _, role := range r.Roles {
			b = appendBytes8(b, []byte(role))
		}
	}
	return b
}

// Decode parses a credential blob back into records. An empty blob is an empty set
// (a database that never created a user), not an error.
func Decode(blob []byte) ([]Record, error) {
	if len(blob) == 0 {
		return nil, nil
	}
	r := reader{b: blob}
	if r.u8() != magic || r.u8() != version {
		return nil, ErrFormat
	}
	n := r.u32()
	out := make([]Record, 0, n)
	for range n {
		var rec Record
		rec.Name = string(r.bytes16())
		rec.Salt = r.bytes8()
		rec.Hash = r.bytes8()
		rec.Iter = r.u32()
		roleCount := r.u16()
		rec.Roles = make([]string, 0, roleCount)
		for range roleCount {
			rec.Roles = append(rec.Roles, string(r.bytes8()))
		}
		out = append(out, rec)
	}
	if r.err != nil {
		return nil, fmt.Errorf("%w: %v", ErrFormat, r.err)
	}
	return out, nil
}

func appendBytes8(b, v []byte) []byte  { return append(append(b, byte(len(v))), v...) }
func appendBytes16(b, v []byte) []byte { return append(binary.BigEndian.AppendUint16(b, uint16(len(v))), v...) }

// reader is a bounds-checked cursor over a blob: any read past the end sets err and
// returns zero, so Decode checks err once at the end rather than after every field.
type reader struct {
	b   []byte
	off int
	err error
}

func (r *reader) take(n int) []byte {
	if r.err != nil {
		return nil
	}
	if r.off+n > len(r.b) {
		r.err = errors.New("unexpected end of blob")
		return nil
	}
	v := r.b[r.off : r.off+n]
	r.off += n
	return v
}

func (r *reader) u8() byte {
	v := r.take(1)
	if v == nil {
		return 0
	}
	return v[0]
}

func (r *reader) u16() uint16 {
	v := r.take(2)
	if v == nil {
		return 0
	}
	return binary.BigEndian.Uint16(v)
}

func (r *reader) u32() uint32 {
	v := r.take(4)
	if v == nil {
		return 0
	}
	return binary.BigEndian.Uint32(v)
}

func (r *reader) bytes8() []byte {
	n := int(r.u8())
	return slices.Clone(r.take(n))
}

func (r *reader) bytes16() []byte {
	n := int(r.u16())
	return slices.Clone(r.take(n))
}
