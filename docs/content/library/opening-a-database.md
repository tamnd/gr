---
title: "Opening a database"
description: "gr.Open, Options, the file lifecycle, in-memory databases, and db.Info()."
weight: 10
---

## gr.Open

`gr.Open` is the entry point for the library.
It opens a `.gr` file and returns a `*gr.DB`.

```go
db, err := gr.Open("graph.gr", gr.Options{})
if err != nil {
    log.Fatal(err)
}
defer db.Close()
```

If the file does not exist, `gr.Open` creates it as a new, empty database.
If the file exists, gr opens it, validates the header, and replays the WAL if the previous session did not close cleanly.

`*gr.DB` is safe to use concurrently from multiple goroutines.
Create one instance and share it across your program.

## Options

`gr.Options` controls the database at open time.

```go
db, err := gr.Open("graph.gr", gr.Options{
    ReadOnly:      false,
    CacheSize:     64 * 1024 * 1024, // 64 MB buffer pool
    Synchronous:   gr.SyncNormal,
    MaxRetries:    5,
    BusyTimeout:   5 * time.Second,
})
```

| Field | Default | Description |
|---|---|---|
| `ReadOnly` | `false` | Open the database in read-only mode; all writes fail |
| `CacheSize` | system default | Buffer pool size in bytes |
| `Synchronous` | `SyncFull` | Durability level for fsync calls |
| `MaxRetries` | `3` | Retry limit for write-write conflicts |
| `BusyTimeout` | `0` | Duration to wait when the write slot is busy |
| `VFS` | disk VFS | Virtual filesystem — use `vfs.NewMem()` for in-memory databases |

`SyncFull` is the safest: gr fsyncs after every WAL write.
`SyncNormal` skips the extra fsync after the checkpoint write, which is safe on most filesystems.
`SyncOff` turns off all fsyncing — fast but loses data on power failure.

## The file lifecycle

When `gr.Open` returns, the database is ready.
Three files may exist on disk:

| File | Purpose |
|---|---|
| `graph.gr` | The main database file — the source of truth |
| `graph.gr-wal` | Write-ahead log — grows during writes, folded back on checkpoint |
| `graph.gr-shm` | Shared memory file for WAL index — always reproducible from the WAL |

Back up only `graph.gr` for a closed database.
For a live backup of an open database, use `gr backup` or `db.Backup()`.
See the [backup guide](/operations/backup/).

`db.Close()` checkpoints the WAL, fsyncs the database file, and removes the sidecar files.
Always call it, even in an error path.
`defer db.Close()` is the right pattern.

## In-memory databases

For tests, use an in-memory VFS so the test does not touch the disk:

```go
import "github.com/tamnd/gr/vfs"

db, err := gr.Open(":memory:.gr", gr.Options{
    VFS: vfs.NewMem(),
})
```

An in-memory database is not shared between `gr.Open` calls.
Create a helper function that returns a fresh `*gr.DB` for each test.

## Read-only open

Open an existing database for reading only:

```go
db, err := gr.Open("graph.gr", gr.Options{ReadOnly: true})
```

Write queries return an error.
This is safe to open concurrently alongside a read-write open from the same process (the WAL protocol handles it), or from a separate process (file-level reader-writer locking applies).

## db.Info

`db.Info()` returns runtime statistics for the open database:

```go
info, err := db.Info()
if err != nil {
    log.Fatal(err)
}
fmt.Printf("nodes: %d, relationships: %d, file size: %d bytes\n",
    info.NodeCount, info.RelationshipCount, info.FileSize)
```

Fields include `NodeCount`, `RelationshipCount`, `PageCount`, `FileSize`, `WALFrameCount`, `FreelistPages`, and `DatabaseUUID`.

## Errors

`gr.Open` returns typed errors:

| Error | Meaning |
|---|---|
| `*gr.ErrVersionMismatch` | The file was created by a newer version of gr; upgrade gr |
| `*gr.ErrConfigConflict` | The open options conflict with the file's create-time config |
| `*gr.ErrNotADatabase` | The file exists but is not a gr database file |
