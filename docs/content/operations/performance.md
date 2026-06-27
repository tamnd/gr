---
title: "Performance"
description: "How gr stores data for fast reads, the knobs that matter (bulk import, the buffer pool, indexes, parallel execution, checkpointing), and how to measure a query."
weight: 30
---

gr is built so that the common graph query, a pattern match that walks
relationships, stays close to the speed of the underlying storage.
This page explains where the time goes and which levers move it.

## The storage shape

A `.gr` file holds two layers.

The **base** is the sealed, read-optimized image: node and relationship
properties in columnar segments, and adjacency in CSR (compressed sparse row)
arrays so a node's neighbors sit contiguously and expand in a single sequential
scan.

The **delta** is everything written since the last checkpoint, living in the
write-ahead log and an in-memory overlay.
Reads merge the delta on top of the base.

A **checkpoint** folds the delta back into the base.
After a checkpoint, a read of an unchanged node hits the base CSR directly with
no overlay merge, which is the fastest path gr has.
Checkpoints happen automatically (see [pragmas](/configuration/pragmas/):
`wal_autocheckpoint` every 1000 WAL frames by default, plus the optional
`checkpoint_interval_s`) and on `db.Close()`.

The practical consequence: a database that has just been bulk-loaded or freshly
checkpointed reads faster than one carrying a large uncommitted delta.
If you load a lot of data through Cypher and then run read-heavy queries, a
checkpoint in between pays for itself.

## Loading fast

For a cold load of more than roughly 100,000 nodes or relationships, use
[`gr import`](/operations/bulk-import/) rather than individual `CREATE`
statements.
The importer writes the columnar segments and CSR arrays directly, skips the
WAL, and fsyncs once at the end, so it is typically 10 to 100x faster than the
transactional write path for the same data.
The file it produces is a normal, sealed `.gr` file: it opens with `gr.Open`,
passes `gr check`, and is indistinguishable from one grown transactionally.

Use the transactional path (the library or the CLI) for incremental writes
after the initial load, not for the initial load itself.

## The buffer pool

gr caches pages in a buffer pool.
A query that finds its pages already resident never touches the disk.

Size it to your working set with `cache_size`, or let gr size it from available
memory with `cache_auto_fraction` (0.25 of RAM by default):

```
PRAGMA cache_size = -262144   -- 256 MB, negative value is kibibytes
```

For a one-shot bulk read that would otherwise evict your hot pages, set
`PRAGMA cache_disabled = true` for that connection so the scan does not pollute
the pool.

## Indexes turn scans into seeks

Without an index, a `MATCH` that filters on a property scans every node of the
label.
With an index, gr seeks straight to the matching nodes.

```bash
gr run graph.gr "CREATE INDEX FOR (p:Person) ON (p.email)"
```

Create indexes on the properties you filter or join on, and create them after a
bulk import rather than before, so the load does not pay to maintain them.
Use `EXPLAIN` to confirm the planner picked the seek:

```bash
gr run graph.gr "EXPLAIN MATCH (p:Person {email:'a@b.com'}) RETURN p"
```

## Parallel execution

By default a query runs on a single worker.
For large scans and aggregations, raise the worker count so gr splits the work
across cores:

```
PRAGMA parallelism = 8
```

Execution is vectorized: rows flow through operators in batches (morsels) rather
than one at a time.
`morsel_size` (1024 rows by default) sets the batch size.
Parallelism helps queries that touch a lot of data; it does nothing for a query
that seeks a single node, and on a saturated machine more workers can lose to a
single-threaded run, so set it to match the cores you actually have free.

## Repeated queries

gr caches parsed and planned queries (`plan_cache`, on by default) and extracts
literal constants as parameters so structurally identical queries share a plan
(`auto_parameterize`, on by default).
You get the most out of both by parameterizing queries yourself instead of
inlining values, which also avoids re-planning and is safer:

```go
db.Query(ctx, "MATCH (p:Person {email:$email}) RETURN p", map[string]any{"email": addr})
```

## Write throughput and durability

Write speed trades against how hard gr works to survive a crash.
The default fsyncs on checkpoint so a power failure cannot corrupt the file.
`SyncNormal` skips the extra post-checkpoint fsync (safe on most filesystems),
and `SyncOff` turns off fsyncing entirely: fast, but a power failure can lose
recent writes.
See [opening a database](/library/opening-a-database/) for the sync modes and
[pragmas](/configuration/pragmas/) for `wal_autocheckpoint` and `wal_size_limit`,
which control how large the delta grows before it is folded back.

## Measuring a query

`PROFILE` runs the query and reports the operator tree with row counts and
timing, so you can see which operator dominates:

```bash
gr run graph.gr "PROFILE MATCH (a:Person)-[:KNOWS]->(b) RETURN count(*)"
```

`EXPLAIN` shows the plan without running it, which is enough to confirm an index
is used or a join order is sane.

For a repeatable microbenchmark, drive gr from a Go `testing.B` and measure with
`go test -bench`, checkpointing first so the read path hits the base CSR rather
than a warm in-memory delta.
A query measured against an un-checkpointed database is measuring the delta
overlay, not the storage gr actually ships data on.

To compare gr against other graph engines on your own workload, the
[graph-bench](https://github.com/tamnd/graph-bench) harness runs the same query
set against several engines in one process and reports per-percentile latency.
Numbers depend heavily on the workload and the machine, so measure the queries
you actually run, on the hardware you actually deploy on.
