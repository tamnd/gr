---
title: "Release notes"
description: "What shipped in each gr release, newest first."
weight: 60
---

Release notes for gr, newest first.
Every release is also on the [GitHub releases page](https://github.com/tamnd/gr/releases), which carries the prebuilt binaries, Linux packages, checksums, the cosign signature, and the SBOM.

## v0.1.0

The first tagged release of gr.

gr is an embedded, single-file labeled-property-graph database written in pure Go.
It speaks openCypher, runs in your process with no server and no cgo, and stores a whole graph in one `.gr` file.
This release carries the full feature set; the `0.x` version line means the public surfaces are settling in real use before the project commits to the 1.0 stability contract.

### Embedded library

Import `github.com/tamnd/gr`, call `gr.Open`, and run Cypher against a file or an in-memory database.
Reads and writes both go through the same API, so a query behaves the same embedded as it does from the shell or over the wire.

Cypher coverage spans `MATCH`, `CREATE`, `MERGE`, `SET`, `DELETE`, `REMOVE`, and `RETURN`, with aggregation, variable-length paths, `shortestPath`, indexes, and constraints.

### The gr CLI

The `gr` command is an interactive Cypher shell, a one-shot query runner, and a script runner over a single file or a transient in-memory database.

It also ships the operational subcommands: `import` for bulk loading, `export` and `dump`/`load` for moving data out and back, `backup`/`restore` for a consistent physical copy, `info` and `health` for inspecting a database, and `check` for verifying file integrity.
`EXPLAIN` and `PROFILE` show the plan and the measured operator tree.

### Server

`gr serve` exposes the same database over the Bolt v4.4/v5.0 wire protocol and an HTTP JSON API, so existing Neo4j drivers connect to it directly.

### Durability

gr journals writes to a WAL, checksums every page, recovers cleanly after a crash, and takes a hot backup of a live database.
It also ships asynchronous read-replica WAL shipping for a standby copy.

### Stability and conformance

The library API, the configuration surface, the on-disk file format, and the Bolt wire protocol are documented in [STABILITY.md](https://github.com/tamnd/gr/blob/main/STABILITY.md).
The TCK conformance statement and the registry of deliberate deviations are in [CONFORMANCE.md](https://github.com/tamnd/gr/blob/main/CONFORMANCE.md).

### Install

Prebuilt binaries for Linux, macOS, and Windows are on the [releases page](https://github.com/tamnd/gr/releases), alongside `deb`, `rpm`, and `apk` packages and a multi-arch container image on GHCR.
Or build from source with `go install github.com/tamnd/gr/cmd/gr@v0.1.0`.
