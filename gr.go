// Package gr is an embedded, single-file, labeled-property-graph database for Go
// that speaks Cypher (spec 2060). It is pure Go (CGO_ENABLED=0), stores a whole
// graph in one self-describing .gr file with -wal/-shm sidecars, and gives the
// SQLite "open a file, get a database" feel for graphs.
//
// At M0 the public surface is only the lifecycle: Open creates or opens a .gr
// file, validates its header, mounts the pager over a VFS, and Close leaves a
// quiescent, checksum-valid file. Query execution arrives in M2; the write path
// in M3 (doc 25 §1.5). The library API in full is doc 16.
package gr

import (
	"github.com/tamnd/gr/pager"
	"github.com/tamnd/gr/vfs"
	"github.com/tamnd/gr/wal"
)

// DB is an open gr database.
type DB struct {
	path  string
	pager *pager.Pager
}

// Options configure how a database is opened. The zero value is the default:
// the OS filesystem, the default page size, and full synchronous durability.
type Options struct {
	// VFS is the virtual filesystem to open the file through; nil uses the real
	// OS filesystem. Tests pass an in-memory, fault-injecting VFS here.
	VFS vfs.VFS
	// PageSize is used only when creating a new file; 0 means the default.
	PageSize uint32
	// Sync is the durability level; 0 means full synchronous (the safe default).
	Sync wal.SyncLevel
	// ReadOnly opens the database for queries only.
	ReadOnly bool
	// SaltSeed makes WAL salt generation deterministic for tests; 0 is fine in
	// production where determinism is not required.
	SaltSeed uint64
}

// Open opens the database at path, creating it with a fresh header if it does
// not exist, and validating the header if it does. The WAL is recovered on open
// so a crash before this call leaves the database at its last committed state.
func Open(path string, opt Options) (*DB, error) {
	fsys := opt.VFS
	if fsys == nil {
		fsys = vfs.NewOS()
	}
	p, err := pager.Open(fsys, path, pager.Options{
		PageSize: opt.PageSize,
		Sync:     opt.Sync,
		ReadOnly: opt.ReadOnly,
		SaltSeed: opt.SaltSeed,
	})
	if err != nil {
		return nil, err
	}
	return &DB{path: path, pager: p}, nil
}

// Path returns the database file path.
func (db *DB) Path() string { return db.path }

// PageSize returns the file's page size in bytes.
func (db *DB) PageSize() uint32 { return db.pager.PageSize() }

// Close flushes and closes the database. Callers that want a sidecar-free file
// should ensure all work is committed first; a clean checkpoint runs on the last
// commit so a quiescent database has no live WAL frames.
func (db *DB) Close() error {
	if db.pager == nil {
		return nil
	}
	err := db.pager.Close()
	db.pager = nil
	return err
}
