package gr

import (
	"errors"
	"sync"

	"github.com/tamnd/gr/cred"
)

// The credential API: the served database's users, their passwords, and their roles
// (doc 18 §10). gr is a single-database server, so its users live in the database
// file itself, in a reserved system area the engine owns (doc 18 §10.3); these
// methods are the library surface CREATE USER / ALTER USER / DROP USER and the
// administrative GRANT / REVOKE ROLE sit on, and the seam a DB-backed AuthProvider
// reads when a connection authenticates (doc 18 §10.4).
//
// The whole user set is small and rewritten wholesale on every change, so each
// mutation loads the set, edits it in memory, and stores it back under credMu. The
// store is opaque on disk: only the salted hash is kept, never the plaintext (doc 18
// §10.3).

// The role vocabulary, re-exported from package cred so a caller names a role without
// importing the credential model (doc 18 §10.6). The set is fixed: reader < editor <
// publisher < admin in privilege, the ordering an authorization layer enforces.
const (
	RoleReader    = cred.RoleReader
	RoleEditor    = cred.RoleEditor
	RolePublisher = cred.RolePublisher
	RoleAdmin     = cred.RoleAdmin
)

// ErrUserExists is returned by CreateUser when a user with that name already exists.
var ErrUserExists = errors.New("gr: user already exists")

// ErrNoSuchUser is returned by AlterUser, DropUser, GrantRole, and RevokeRole when no
// user has the given name.
var ErrNoSuchUser = errors.New("gr: no such user")

// ErrInvalidRole is returned when a role name is not one of the four built-in roles
// (reader, editor, publisher, admin; doc 18 §10.6).
var ErrInvalidRole = errors.New("gr: invalid role")

// ErrEmptyUserName is returned when a user name is empty. A name is a connection's
// principal, so it must be non-empty to be addressable.
var ErrEmptyUserName = errors.New("gr: empty user name")

// credMu serializes the load-edit-store sequence every credential mutation runs, so
// two concurrent CreateUser calls cannot read the same set and clobber each other's
// edit. It is a package-level guard keyed by nothing because a process opens one
// database file; the engine's own write lock makes the store durable, this lock makes
// the read-modify-write atomic above it.
var credMu sync.Mutex

// UserInfo is one user as the library reports it: the name and the granted roles. It
// never carries the password or its hash, which stay inside the store (doc 18 §10.3).
type UserInfo struct {
	Name  string
	Roles []string
}

// CreateUser adds a user with the given password and roles (doc 18 §10, the CREATE
// USER surface). The password is salted and hashed before it is stored; the plaintext
// is never written. Roles must each be a built-in role; an empty roles list creates a
// user with no privileges, which a later GrantRole fills in. It returns ErrUserExists
// if the name is already taken and ErrInvalidRole for an unknown role.
func (db *DB) CreateUser(name, password string, roles ...string) error {
	if db.eng == nil {
		return ErrClosed
	}
	if name == "" {
		return ErrEmptyUserName
	}
	if err := checkRoles(roles); err != nil {
		return err
	}
	credMu.Lock()
	defer credMu.Unlock()
	recs, err := db.loadCreds()
	if err != nil {
		return err
	}
	if _, ok := findRec(recs, name); ok {
		return ErrUserExists
	}
	salt, hash, err := cred.Hash(password)
	if err != nil {
		return err
	}
	recs = append(recs, cred.Record{
		Name:  name,
		Salt:  salt,
		Hash:  hash,
		Iter:  cred.Iter(),
		Roles: dedupRoles(roles),
	})
	return db.storeCreds(recs)
}

// AlterUser changes a user's password (doc 18 §10, the ALTER USER surface). It draws a
// fresh salt and rehashes, so the new password replaces the old secret completely. It
// leaves the user's roles untouched; GrantRole and RevokeRole change those. It returns
// ErrNoSuchUser if the name is unknown.
func (db *DB) AlterUser(name, password string) error {
	if db.eng == nil {
		return ErrClosed
	}
	credMu.Lock()
	defer credMu.Unlock()
	recs, err := db.loadCreds()
	if err != nil {
		return err
	}
	i, ok := findRec(recs, name)
	if !ok {
		return ErrNoSuchUser
	}
	salt, hash, err := cred.Hash(password)
	if err != nil {
		return err
	}
	recs[i].Salt = salt
	recs[i].Hash = hash
	recs[i].Iter = cred.Iter()
	return db.storeCreds(recs)
}

// DropUser removes a user (doc 18 §10, the DROP USER surface). It returns ErrNoSuchUser
// if the name is unknown, so a caller can tell a real removal from a no-op.
func (db *DB) DropUser(name string) error {
	if db.eng == nil {
		return ErrClosed
	}
	credMu.Lock()
	defer credMu.Unlock()
	recs, err := db.loadCreds()
	if err != nil {
		return err
	}
	i, ok := findRec(recs, name)
	if !ok {
		return ErrNoSuchUser
	}
	recs = append(recs[:i], recs[i+1:]...)
	return db.storeCreds(recs)
}

// Users returns every user with its roles, sorted by name (doc 18 §10, the SHOW USERS
// surface). It reports no secrets, only names and roles. The result is empty for a
// database that never created a user.
func (db *DB) Users() ([]UserInfo, error) {
	if db.eng == nil {
		return nil, ErrClosed
	}
	credMu.Lock()
	defer credMu.Unlock()
	recs, err := db.loadCreds()
	if err != nil {
		return nil, err
	}
	out := make([]UserInfo, 0, len(recs))
	for _, r := range recs {
		out = append(out, UserInfo{Name: r.Name, Roles: append([]string(nil), r.Roles...)})
	}
	return out, nil
}

// GrantRole adds a role to a user (doc 18 §10.6, the GRANT ROLE surface). Granting a
// role the user already holds is a no-op that still succeeds, so a grant is idempotent.
// It returns ErrNoSuchUser for an unknown name and ErrInvalidRole for an unknown role.
func (db *DB) GrantRole(name, role string) error {
	if db.eng == nil {
		return ErrClosed
	}
	if !cred.ValidRole(role) {
		return ErrInvalidRole
	}
	credMu.Lock()
	defer credMu.Unlock()
	recs, err := db.loadCreds()
	if err != nil {
		return err
	}
	i, ok := findRec(recs, name)
	if !ok {
		return ErrNoSuchUser
	}
	for _, r := range recs[i].Roles {
		if r == role {
			return nil
		}
	}
	recs[i].Roles = append(recs[i].Roles, role)
	return db.storeCreds(recs)
}

// RevokeRole removes a role from a user (doc 18 §10.6, the REVOKE ROLE surface).
// Revoking a role the user does not hold is a no-op that still succeeds, so a revoke is
// idempotent. It returns ErrNoSuchUser for an unknown name and ErrInvalidRole for an
// unknown role.
func (db *DB) RevokeRole(name, role string) error {
	if db.eng == nil {
		return ErrClosed
	}
	if !cred.ValidRole(role) {
		return ErrInvalidRole
	}
	credMu.Lock()
	defer credMu.Unlock()
	recs, err := db.loadCreds()
	if err != nil {
		return err
	}
	i, ok := findRec(recs, name)
	if !ok {
		return ErrNoSuchUser
	}
	kept := recs[i].Roles[:0]
	for _, r := range recs[i].Roles {
		if r != role {
			kept = append(kept, r)
		}
	}
	recs[i].Roles = kept
	return db.storeCreds(recs)
}

// Authenticate verifies a name and password and returns the user's roles on success
// (doc 18 §10.4, the read a DB-backed AuthProvider performs). ok is false for an
// unknown user or a wrong password, with a nil error: a failed login is a normal
// outcome, not a fault. The compare is constant-time (cred.Verify), and an unknown
// user still runs a verify against a dummy record so a missing name and a wrong
// password take comparable time, narrowing a user-enumeration timing side channel.
func (db *DB) Authenticate(name, password string) (roles []string, ok bool, err error) {
	if db.eng == nil {
		return nil, false, ErrClosed
	}
	credMu.Lock()
	defer credMu.Unlock()
	recs, err := db.loadCreds()
	if err != nil {
		return nil, false, err
	}
	i, found := findRec(recs, name)
	if !found {
		// Run a verify against a throwaway record so an unknown name costs about the
		// same as a known one with a wrong password (doc 18 §10.4).
		cred.Verify(cred.Record{}, password)
		return nil, false, nil
	}
	if !cred.Verify(recs[i], password) {
		return nil, false, nil
	}
	return append([]string(nil), recs[i].Roles...), true, nil
}

// loadCreds reads the credential blob from the engine and decodes it into records. It
// must be called with credMu held.
func (db *DB) loadCreds() ([]cred.Record, error) {
	blob, err := db.eng.CredentialBlob()
	if err != nil {
		return nil, err
	}
	return cred.Decode(blob)
}

// storeCreds encodes records and writes them back through the engine, committing. It
// must be called with credMu held.
func (db *DB) storeCreds(recs []cred.Record) error {
	return db.eng.SetCredentialBlob(cred.Encode(recs))
}

// findRec returns the index of the record named name and whether it was found.
func findRec(recs []cred.Record, name string) (int, bool) {
	for i := range recs {
		if recs[i].Name == name {
			return i, true
		}
	}
	return 0, false
}

// checkRoles reports the first invalid role in a list, or nil if all are valid.
func checkRoles(roles []string) error {
	for _, r := range roles {
		if !cred.ValidRole(r) {
			return ErrInvalidRole
		}
	}
	return nil
}

// dedupRoles returns the roles with duplicates removed, order preserved, so a create
// with a repeated role stores it once.
func dedupRoles(roles []string) []string {
	if len(roles) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(roles))
	out := make([]string, 0, len(roles))
	for _, r := range roles {
		if seen[r] {
			continue
		}
		seen[r] = true
		out = append(out, r)
	}
	return out
}
