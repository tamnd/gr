---
title: "Configuration"
linkTitle: "Configuration"
description: "The configuration model: four surfaces, three tiers, PRAGMA syntax, and key knobs."
weight: 70
featured: false
---

gr has 51 configuration knobs.
Every knob has four ways to reach it — a library option, a PRAGMA, a CLI flag, and a server config key — and belongs to one of three tiers that control when the value can change.

## Configuration sources

Values come from five sources, in precedence order (highest wins):

1. **Session PRAGMA** — `PRAGMA cache_size = 65536` inside a connection. Overrides everything for the life of the connection.
2. **Explicit library option** — `gr.Options{CacheSize: 64 << 20}`. Set at open time; applies for the database's lifetime.
3. **Persisted file metadata** — values stored inside the `.gr` file from a previous session or from `gr pragma`.
4. **Environment variable** — `GR_CACHE_SIZE=67108864` for a subset of knobs.
5. **Compiled-in default** — the baseline value baked into the binary.

## Three tiers

**Create-time knobs** are baked into the file when `gr.Open` creates it.
They cannot change after creation.
Passing a conflicting value on a subsequent open returns `*gr.ErrConfigConflict`.
Examples: `page_size`, `checksum`, `encoding`, `segment_size`.

**Persistent-runtime knobs** are stored in the file and survive reopen.
Change them with `PRAGMA name = value` or `gr pragma graph.gr name value`.
They apply every time the file is opened until changed again.
Examples: `synchronous`, `journal_mode`, `wal_autocheckpoint`, `auto_vacuum`.

**Session knobs** are per-connection and reset to the persisted value on close.
Change them with `PRAGMA name = value` inside a connection or via `gr.Options`.
Examples: `cache_size`, `busy_timeout_ms`, `max_retries`, `access_mode`.

## The four surfaces

Every knob has (up to) four spellings:

| Surface | Example |
|---|---|
| Library option | `gr.Options{Synchronous: gr.SyncNormal}` |
| PRAGMA | `PRAGMA synchronous = NORMAL` |
| CLI flag | `gr serve graph.gr --synchronous NORMAL` |
| Server config key | `durability.synchronous = NORMAL` |

The knob name is the same across all surfaces, modulo capitalisation conventions (Go uses PascalCase, PRAGMA uses snake_case, CLI uses kebab-case).

## PRAGMA syntax

Read a knob:

```sql
PRAGMA synchronous
```

Set a persistent or session knob:

```sql
PRAGMA synchronous = NORMAL
PRAGMA cache_size = 65536
```

From the CLI:

```bash
gr pragma graph.gr synchronous          # print current value
gr pragma graph.gr synchronous NORMAL   # set
```

Introspection:

```sql
PRAGMA pragma_list          -- list all knob names and tiers
PRAGMA database_uuid        -- the database's stable UUID
PRAGMA page_count           -- total pages in the file
PRAGMA file_size            -- file size in bytes
```

## Key knobs

**page_size** (create-time, default 4096): the B-tree page size in bytes.
Larger pages reduce tree height for wide graphs; smaller pages waste less space on small graphs.
Must be a power of 2 between 512 and 65536.

```go
db, err := gr.Open("graph.gr", gr.Options{PageSize: 8192})
```

**synchronous** (persistent, default `FULL`): the durability level.
`OFF` disables fsyncing — fast but loses data on power failure.
`NORMAL` fsyncs after WAL writes but not after checkpoints.
`FULL` fsyncs both — the safe default.
`EXTRA` adds an fsync before deleting the WAL on checkpoint.

```bash
gr pragma graph.gr synchronous NORMAL
```

**cache_size** (session, default system-dependent): the buffer pool size.
Set to the number of pages to cache (positive) or to the negative of the cache size in kibibytes (negative):

```sql
PRAGMA cache_size = -65536   -- 64 MB
```

**max_retries** (session, default 3): how many times `db.ExecuteWrite` retries a write-write conflict before returning the error.

```go
db, err := gr.Open("graph.gr", gr.Options{MaxRetries: 10})
```

**busy_timeout_ms** (session, default 0): how long to wait for the write slot when it is held by another writer, in milliseconds.
`0` means fail immediately.

See the [PRAGMA reference](/configuration/pragmas/) for the complete catalogue of all 51 knobs.
