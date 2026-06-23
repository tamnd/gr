---
title: "PRAGMA reference"
description: "Every gr PRAGMA: create-time, persistent-runtime, session, and introspection knobs."
weight: 10
---

51 knobs in four groups.
PRAGMA names use snake_case; the corresponding library option is in PascalCase.

## Create-time knobs

Set at database creation; immutable after that.
Passing a conflicting value to `gr.Open` on an existing file returns `*gr.ErrConfigConflict`.

| PRAGMA | Type | Default | Description |
|---|---|---|---|
| `page_size` | int | 4096 | B-tree page size in bytes; must be a power of 2 between 512 and 65536 |
| `checksum` | string | `crc32c` | Per-page checksum algorithm: `crc32c`, `xxh3`, `none` |
| `encoding` | string | `utf8` | Character encoding for string values |
| `segment_size` | int | 65536 | Node/relationship segment size; affects memory vs IO trade-off |
| `case_sensitive` | bool | `true` | Whether label and property-key comparisons are case-sensitive |
| `application_id` | int | 0 | Application-specific 32-bit identifier stored in the header |
| `embedded_zone_db` | bool | `false` | Embed a time-zone database in the file for full datetime support without a system TZ |
| `compression_baseline` | string | `none` | Default block compression for new segments: `none`, `lz4`, `zstd` |
| `encryption` | string | `none` | Page-level encryption: `none`, `aes256-gcm` |
| `cipher` | string | — | AES-256-GCM key (hex-encoded); required when encryption is set |

## Persistent-runtime knobs

Stored in the file; survive reopen until changed with `PRAGMA name = value` or `gr pragma`.

| PRAGMA | Type | Default | Description |
|---|---|---|---|
| `synchronous` | string | `FULL` | Durability level: `OFF`, `NORMAL`, `FULL`, `EXTRA` |
| `journal_mode` | string | `WAL` | Journal mode: `WAL`, `DELETE`, `TRUNCATE`, `PERSIST`, `MEMORY`, `OFF` |
| `full_page_writes` | bool | `true` | Write full page images on the first write after a checkpoint (protects against torn writes) |
| `wal_autocheckpoint` | int | 1000 | Automatically checkpoint when WAL reaches this many frames |
| `wal_size_limit` | int | 0 | Soft limit on WAL size in bytes; 0 = unlimited |
| `checkpoint_interval_s` | int | 0 | Background checkpoint interval in seconds; 0 = disabled |
| `auto_vacuum` | string | `NONE` | Automatic space reclamation: `NONE`, `FULL`, `INCREMENTAL` |
| `auto_compaction` | bool | `false` | Automatically compact cold segments during idle periods |
| `statistics_target` | int | 100 | Histogram bucket count for query statistics |
| `statistics_refresh_policy` | string | `auto` | When to refresh statistics: `auto`, `manual` |
| `compression` | string | `none` | Default compression for new pages: `none`, `lz4`, `zstd` |
| `cold_compression` | string | `zstd` | Compression for cold (rarely accessed) segments |
| `default_cache_size` | int | — | Persistent cache size override (page count); overrides the compiled default |
| `cache_auto_fraction` | float | 0.25 | Fraction of available RAM to use as cache when `default_cache_size` is not set |
| `concurrency_model` | string | `single-writer` | Write concurrency model: `single-writer`, `multi-writer` (post-1.0) |
| `inline_threshold` | int | 256 | Maximum bytes for an inline property value before spilling to overflow pages |

## Session knobs

Per-connection; reset to the persisted value on close.
Set with `PRAGMA name = value` inside a connection or via `gr.Options`.

| PRAGMA | Type | Default | Description |
|---|---|---|---|
| `cache_size` | int | from `default_cache_size` | Buffer pool size: positive = page count, negative = kibibytes |
| `page_cache_size` | int | 0 | Secondary OS page-cache advice size (bytes) |
| `busy_timeout_ms` | int | 0 | Milliseconds to wait for the write slot; 0 = fail immediately |
| `query_timeout_ms` | int | 0 | Query wall-clock limit in milliseconds; 0 = unlimited |
| `access_mode` | string | `READ_WRITE` | Session access mode: `READ_WRITE`, `READ` |
| `isolation` | string | `snapshot` | Transaction isolation level: `snapshot` (only supported level in v1.0) |
| `max_retries` | int | 3 | Retry limit for `ExecuteWrite` on write-write conflict |
| `max_txn_size` | int | 0 | Soft limit on transaction dirty pages; 0 = unlimited |
| `plan_cache` | bool | `true` | Cache parsed and planned query plans |
| `adaptive_replanning` | bool | `true` | Replan when runtime statistics diverge significantly from the compiled plan |
| `auto_parameterize` | bool | `true` | Automatically extract literal constants as parameters to improve plan cache hit rate |
| `max_query_memory` | int | 0 | Soft limit on per-query memory in bytes; 0 = unlimited |
| `spill_enabled` | bool | `true` | Allow spilling large intermediate results to disk |
| `morsel_size` | int | 1024 | Vectorized execution batch size (rows per morsel) |
| `parallelism` | int | 1 | Number of parallel worker threads per query |
| `readahead` | bool | `true` | Prefetch pages ahead of sequential scans |
| `cache_disabled` | bool | `false` | Bypass the buffer pool (useful for one-shot bulk reads that would pollute the cache) |

## Introspection (read-only)

These PRAGMAs read database state; they cannot be set.

| PRAGMA | Type | Description |
|---|---|---|
| `database_uuid` | string | Stable UUID assigned at creation |
| `page_count` | int | Total pages in the database file |
| `file_size` | int | File size in bytes |
| `wal_frame_count` | int | Number of frames in the current WAL |
| `freelist_pages` | int | Number of free (reclaimed) pages |
| `pragma_list` | table | All PRAGMA names with their tier and current value |
