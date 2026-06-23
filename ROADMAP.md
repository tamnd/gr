# Roadmap

This document records what `gr` v1.0 ships, what is explicitly deferred to post-1.0, and what is outside the project's scope entirely.
It is the user-facing companion to the spec's deferral catalogue (spec 2060, doc 25 §15).

## What v1.0 ships

- An embedded, single-file labeled-property-graph database in pure Go.
- openCypher read and write (`MATCH`, `CREATE`, `MERGE`, `SET`, `DELETE`, `REMOVE`, `RETURN`, aggregation, variable-length paths, shortestPath, indexes, constraints).
- A `gr` command-line tool with an interactive Cypher shell, one-shot query runner, schema inspection, bulk import, backup/restore, and `EXPLAIN`/`PROFILE`.
- A Bolt+HTTP server (`gr serve`) for driver connectivity (Go, Python, JavaScript, Java, .NET, Rust).
- A stable library API, a frozen configuration surface, a versioned file format, and a Bolt wire protocol — all documented in `STABILITY.md`.
- Asynchronous read-replica WAL shipping for read scaling.
- An honest TCK conformance statement and deviations registry (`CONFORMANCE.md`).

## Post-1.0 roadmap

These are *deferred*, not *declined* — each has a seam already in place that makes it additive.
They are ordered roughly by expected value-to-effort.

### Concurrent writers (high priority)

The 1.0 MVCC model is single-writer-first: write transactions serialize, and the single-writer slot is the default.
The multi-writer growth path is already designed into the MVCC version model, watermark oracle, and write-write conflict detection — enabling it is a correctness project, not a rewrite.
The `WithConcurrentWriters(true)` option is the seam; the post-1.0 release enables it as a first-class, hardened path.

### Replication maturity (high priority)

v1.0 ships asynchronous read-replica WAL shipping.
Post-1.0: synchronous/quorum replication, writable replicas, and automatic failover.
The seam is the WAL-shipping and recovery-is-replication identity already in place.

### Broader language coverage (medium–high priority)

v1.0 ships a hardened Cypher subset with an honest deviations registry.
Post-1.0: the remaining openCypher TCK scenarios not in the deviations registry, and a measured path toward GQL (ISO/IEC 39075:2024).
The parser, binder, and planner accept new clauses additively against frozen seams.
GQL-conformance tracking in `tck/deviations.yaml` makes growth measurable.

### Graph-algorithms library (medium priority)

v1.0 runs graph queries excellently but is not a graph-analytics platform.
Post-1.0: first-class procedures for PageRank, community detection, and centrality metrics, callable from Cypher with `CALL procedure()`.
The seam is the `CALL`-able procedure mechanism already specified; the algorithms bind against the same storage the query engine uses.

### Compiled / JIT execution (low–medium priority)

v1.0 uses the vectorized interpreted operator pipeline, which is fast enough for the 1.0 performance targets.
Post-1.0: a compiled/JIT path, likely LLVM-backed or WebAssembly-targeted.
The seam is the operator interface in the execution engine, which is the natural compile target.

### Richer authorization (low–medium priority)

v1.0 ships role-based access (reader/editor/publisher/admin) and an auth-provider seam.
Post-1.0: fine-grained, attribute-level authorization; multi-tenant isolation.
The seam is the auth-provider interface and the credential store already in place.

### Richer import sources (low priority)

v1.0's bulk loader handles CSV and Parquet.
Post-1.0: additional columnar and streaming sources (Arrow, JSON-lines, Avro).
The seam is the loader's input-model abstraction.

## Non-goals

These are outside the project's scope by identity — not backlog items, not deferrals:

- **Clustering and sharding.** `gr` is a single-node embedded database. Multi-node clustering, distributed query execution, and horizontal sharding are a different product, not a future version.
- **SPARQL and RDF.** `gr` stores a labeled-property graph and queries it with Cypher. RDF's triple model and SPARQL are a different data model and query language.
- **SQL.** The query language is Cypher. SQL over a graph model is a different design (though a graph database can expose a SQL view; that is not `gr`'s goal).
- **Mandatory schema.** `gr` is schema-optional; imposing a mandatory schema on every node or relationship contradicts the LPG model it is built on.
- **Adjacent-shape stores.** Document store, key-value store, time-series store, and vector-only database are all shapes `gr` is not and does not become, even though the underlying storage engine could in principle be bent toward them.

## How to track progress

The TCK deviations registry (`tck/deviations.yaml`) records every scenario skipped with a reason code.
As post-1.0 features land, their scenarios move from the deviations registry to the passing corpus and the `CONFORMANCE.md` numbers update.
Releases follow semantic versioning: minor versions add features additively; a major version (v2) changes a contract. See `STABILITY.md` for the full contract rules.
