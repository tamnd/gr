---
title: "Roadmap"
description: "What gr 1.0 ships, what is deferred to post-1.0, and what is outside the project's scope."
weight: 50
---

The full roadmap is in [ROADMAP.md](https://github.com/tamnd/gr/blob/main/ROADMAP.md) in the repository.
This page is a summary.

## What v1.0 ships

- Embedded Go library (`github.com/tamnd/gr`) with openCypher read and write.
- `gr` CLI: interactive shell, one-shot query runner, bulk importer, backup/restore, schema inspection, `EXPLAIN`/`PROFILE`.
- `gr serve`: Bolt v4.4/v5.0 wire protocol and HTTP JSON API, compatible with Neo4j drivers.
- Stable library API (148 exported names), frozen configuration surface (51 knobs), versioned file format, documented wire protocol.
- WAL journaling, per-page checksums, crash recovery, hot backup.
- Asynchronous read-replica WAL shipping.
- Honest TCK conformance statement and deviations registry.

## Post-1.0 deferrals

These are deferred, not declined — each has a seam already in place.

**Concurrent writers (high priority).**
The 1.0 MVCC model serializes write transactions.
Post-1.0, `WithConcurrentWriters(true)` enables parallel writes through the existing MVCC version model.

**Replication maturity (high priority).**
v1.0 ships async read-replica WAL shipping.
Post-1.0: synchronous/quorum replication, writable replicas, automatic failover.

**Broader language coverage (medium–high).**
v1.0 covers a hardened subset with an honest deviations registry.
Post-1.0: remaining TCK scenarios and a measured path toward GQL.

**Graph-algorithms library (medium).**
Post-1.0: PageRank, community detection, and centrality metrics as `CALL`-able procedures.

**Compiled/JIT execution (low–medium).**
Post-1.0: a compiled or JIT path, likely LLVM-backed or WebAssembly-targeted.

**Richer authorization (low–medium).**
Post-1.0: fine-grained attribute-level authorization and multi-tenant isolation.

**Richer import sources (low).**
Post-1.0: Arrow, JSON-lines, and Avro in addition to CSV.

## Non-goals

These are outside the project's scope by identity:

- **Clustering and sharding.** gr is a single-node embedded database. Horizontal sharding and distributed query execution are a different product.
- **SPARQL and RDF.** gr stores a labeled-property graph and queries it with Cypher. RDF's triple model and SPARQL are a different design.
- **SQL.** The query language is Cypher.
- **Mandatory schema.** gr is schema-optional by design.
- **Adjacent-shape stores.** Document, key-value, time-series, and vector-only databases are not gr and do not become gr.

## Tracking progress

The TCK deviations registry (`tck/deviations.yaml`) records every scenario skipped with a reason code.
As post-1.0 features land, their scenarios move to the passing corpus and `CONFORMANCE.md` updates.
Releases follow semantic versioning: minor versions add features additively; a major version changes a contract.
