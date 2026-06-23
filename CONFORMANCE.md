# Conformance

This document is the honest conformance statement for `gr` v1 (doc 23 §2.5, §2.6, §2.7; doc 25 §11.2).
It is generated from the TCK runner's actual results so it cannot claim more than the engine backs.

## TCK results

The openCypher TCK (Technology Compatibility Kit) is the authoritative language conformance corpus.
`gr` runs the embedded scenario subset in CI on every commit; the numbers below reflect the embedded corpus.

Full openCypher TCK results are tracked separately as the scenario count scales; the deviations registry (`tck/deviations.yaml`) records every scenario that is deliberately skipped and why.

| Outcome | Count |
|---------|-------|
| Pass | 16 |
| Skip (unimplemented feature) | 0 |
| Skip (known deviation) | 0 |
| Fail | 0 |
| **Total** | **16** |

Conformant-subset pass rate: **100.00%**

The `TestConformanceStatement` test in `tck/conformance_test.go` fails if the embedded TCK pass rate drops below 100%, making this table a live gate rather than a one-time snapshot.

## Deviations registry

`tck/deviations.yaml` records scenarios the engine deliberately handles differently from the TCK expectation.
At v1.0 the registry is empty: every scenario in the embedded corpus passes.

When a deviation is added, it must include:
- the scenario name exactly as it appears in the feature file
- a reason code (`NOT_IMPLEMENTED`, `SPEC_AMBIGUITY`, or `INTENTIONAL`)
- a short human note explaining the decision

## GQL position

`gr` v1 implements a subset of openCypher and does not claim GQL (ISO/IEC 39075:2024) conformance.
The Cypher subset covered is documented in doc 09 (the Cypher language reference).
GQL alignment is a post-1.0 goal.

## Explicit non-goals at v1.0

These are features `gr` deliberately does not include at v1.0 (doc 00 §4):

- **Concurrent writers** — the default model is single-writer-at-a-time; concurrent-writer mode is an opt-in that is present but not the primary supported path in v1.
- **Clustering and replication maturity** — v1 ships asynchronous read-replica WAL shipping; synchronous/quorum replication, writable replicas, and automatic failover are post-v1.
- **Graph algorithms library** — PageRank, community detection, centrality as first-class procedures are post-v1.
- **Compiled/JIT query execution** — the executor is the interpreted pipeline; compilation is explicitly deferred (ADR-6).
- **Full GQL coverage** — the language surface is openCypher-subset; GQL is the post-v1 roadmap.
- **Fine-grained authorization** — v1 ships role-based access (reader/editor/publisher/admin) and an auth-provider seam; attribute-level and fine-grained authorization are post-v1.

## How to read the numbers

A `gr` deployment that needs a scenario this corpus skips as "unimplemented" should check `tck/deviations.yaml` for the reason and the planned release that covers it.
The conformant-subset pass rate is the fraction of scenarios `gr` makes a correctness claim for; the total-corpus rate includes the explicitly-skipped scenarios that are future work.
