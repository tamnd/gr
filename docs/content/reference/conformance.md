---
title: "Conformance"
description: "gr's openCypher TCK pass rate and documented deviations."
weight: 40
---

The full conformance statement is in [CONFORMANCE.md](https://github.com/tamnd/gr/blob/main/CONFORMANCE.md) in the repository.
This page is a summary.

## TCK results

As of v1.0:

- **Passing:** 16 / 16 scenarios
- **Failing:** 0
- **Pass rate:** 100%
- **GQL claim:** none

The conformance test runs automatically on every commit as `TestConformanceStatement` in `tck/conformance_test.go`.
It fails if any scenario fails, and it fails if the total scenario count drops below 16, which prevents silent deletion of coverage.

## Deviations registry

Deviations — TCK scenarios gr does not yet pass — are recorded in `tck/deviations.yaml` with a reason code.
As post-1.0 features land, scenarios move from the deviations registry to the passing corpus and the numbers above update.

## Explicit non-goals

- **GQL (ISO/IEC 39075:2024):** gr targets openCypher v1.0, not the ISO GQL standard. The two languages overlap significantly but are not identical. gr will not claim GQL conformance.
- **Full openCypher TCK:** gr implements a hardened subset and documents what it skips. The post-1.0 roadmap includes expanding TCK coverage incrementally.

## Language coverage

What gr implements in v1.0:

- Read clauses: `MATCH`, `WHERE`, `RETURN`, `WITH`, `UNWIND`, `OPTIONAL MATCH`, `ORDER BY`, `SKIP`, `LIMIT`
- Write clauses: `CREATE`, `MERGE`, `SET`, `REMOVE`, `DELETE`, `DETACH DELETE`, `FOREACH`
- Schema: `CREATE INDEX`, `DROP INDEX`, `CREATE CONSTRAINT`, `DROP CONSTRAINT`, `SHOW INDEXES`, `SHOW CONSTRAINTS`
- Catalog: `SHOW LABELS`, `SHOW RELATIONSHIP TYPES`, `SHOW PROPERTY KEYS`
- Path functions: `shortestPath`, `allShortestPaths`, named paths, `nodes()`, `relationships()`, `length()`
- Aggregation: `count`, `sum`, `avg`, `min`, `max`, `collect`, `DISTINCT`
- Expressions: arithmetic, comparison, string predicates, `CASE`, `IN`, `IS NULL`, list comprehensions
- Functions: string, math, type conversion, `coalesce`, `elementId`, `labels`, `type`, `properties`, `keys`, `size`, `range`
- Parameters: `$name` syntax for all value types

What is deferred to post-1.0 (see [ROADMAP.md](/reference/roadmap/)):

- `CALL` procedures for graph algorithms
- Remaining TCK scenarios in the deviations registry
- GQL compatibility
