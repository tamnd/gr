---
title: "Backup and restore"
description: "Hot backup, physical restore, logical dump/restore, and WAL archiving for PITR."
weight: 20
---

## Hot backup

`gr backup` copies a live, open database to a new file while it is being used:

```bash
gr backup graph.gr backup-$(date +%Y%m%d).gr
```

The backup is a complete, consistent snapshot.
It folds the WAL into the backup file, so the backup file has no sidecars and can be opened directly.

```bash
# Verify the backup immediately.
gr check backup-20260101.gr

# Open the backup to confirm.
gr info backup-20260101.gr
```

## Restore

```bash
gr restore backup-20260101.gr graph.gr
```

`gr restore` checks the source file, then copies it over the destination.
The destination must be closed before restoring into it.

For a live restore without stopping the current process, take a new hot backup and swap atomically:

```bash
gr backup graph.gr restored.gr
mv graph.gr graph.gr.old
mv restored.gr graph.gr
```

## Physical backup artifacts

When the database is open, three files exist on disk:

| File | Role | Include in backup? |
|---|---|---|
| `graph.gr` | Main database file | Yes — always |
| `graph.gr-wal` | Write-ahead log | Only for point-in-time recovery |
| `graph.gr-shm` | WAL index (ephemeral) | Never — reproducible from the WAL |

For a cold backup (database closed), copy `graph.gr` only.
The WAL is flushed and folded on a clean close, so the main file is the complete state.

For a live backup, use `gr backup` — do not copy `graph.gr` directly while it is open.
An unchecked copy of an open database file may be inconsistent.

## Logical dump and restore

`gr dump` produces a Cypher script of `CREATE` statements:

```bash
gr dump graph.gr > dump-20260101.cypher
```

The dump is human-readable and version-independent.
Use it for migrations between gr versions or for seeding a new database from an existing one.

Restore from the dump:

```bash
gr shell new-graph.gr < dump-20260101.cypher
```

A logical dump is slower to restore than a physical backup for large graphs, but it is portable and can be edited by hand.

## WAL archiving and point-in-time recovery

gr uses WAL (write-ahead log) journaling.
Every write appends to the WAL before modifying the main file.
A WAL archive is a sequence of WAL segments that lets you replay the database to any point in time.

Enable WAL shipping to a remote store:

```bash
gr serve graph.gr --wal-archive-cmd "aws s3 cp {segment} s3://my-bucket/wal/{segment}"
```

The `--wal-archive-cmd` receives each sealed WAL segment as `{segment}`.
gr calls it asynchronously after sealing.

To restore to a point in time:
1. Start from the closest physical backup before the target time.
2. Replay WAL segments up to the target time with `gr wal-replay`:

```bash
gr wal-replay graph.gr ./wal-segments/ --until "2026-01-15T14:30:00Z"
```

## Backup validation

Always validate a backup before trusting it:

```bash
gr check backup.gr
```

This verifies every page checksum, the free-list, the B-tree structure, and the WAL consistency.
It exits `0` if the file is healthy and non-zero otherwise.

Include this step in automated backup pipelines before uploading to long-term storage.

## Recommendations

- **Frequency:** take a hot backup at least once per day for production databases.
- **Retention:** keep at least 7 daily backups and 4 weekly backups.
- **Testing:** restore from backup to a staging database at least monthly.
- **Validation:** run `gr check` on every new backup before the pipeline considers it successful.
- **WAL archiving:** enable WAL shipping for databases where point-in-time recovery matters.
