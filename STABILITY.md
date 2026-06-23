# Stability

`gr` v1 is a stable release. This document records what is guaranteed not to break within the v1 major version, what is explicitly not covered, and how changes across major versions are managed.

## What is stable

### Library API

Every exported symbol in the `gr` package is a v1 promise (doc 16 §24.1):

- Exported names, signatures, argument orders, return shapes, and error semantics are stable within v1.
- A minor version (v1.1, v1.2) may *add* — a new option, a new helper, a new error sentinel — but never remove or change the meaning of an existing exported symbol.
- A breaking change requires a v2 major-version bump, announced and documented.

The `TestAPIStabilityGuard` test in `gr_api_test.go` guards this commitment: it hashes the sorted exported surface and fails if any symbol is removed or renamed without deliberate, reviewed action.

### Configuration surface

Every knob in `config/knob.go` is a v1 promise (doc 24):

- Canonical names, tiers, PRAGMA spellings, library options, CLI flags, and server-config keys are stable within v1.
- Adding a new knob is additive (minor version).
- Renaming a canonical name or changing a knob's tier is a breaking change requiring v2.

The `TestFreezeGuard` test in `config/freeze_test.go` guards this commitment.

### File format

The `.gr` file format is versioned (doc 03 §9). A v1 library reads any file written by any v1.x release. A file written by a newer v1.x library is readable by older v1.x releases that implement all features the file uses. The format version is recorded in the file header and mismatches return `ErrVersionMismatch`.

### Wire protocol (Bolt)

The Bolt server (doc 18) speaks Bolt v4.4 and v5.0. The supported protocol versions and their message semantics are stable within v1. Additions (new optional fields, new Bolt v5.1 features) are backward-compatible; removals require a major version.

## What is not stable

- **Internal packages** — nothing under paths that are not the root `gr` package. The planner, executor, storage engine, WAL, and catalog are internal and may change at any time within a major version.
- **Error message text** — the typed error sentinels (`ErrConflict`, `*ConstraintError`) are stable; the human-readable message strings may improve.
- **Rendered plan and profile output** — the structured fields (`Plan.Operator()`) are stable; the rendered tree string may change.
- **Performance characteristics** — the API's behavior is stable; its performance may improve without a version bump.

## Deprecation and migration

A symbol to be removed in v2 is marked `// Deprecated:` in v1 (with the replacement named) for at least one minor version before v2. A v2 migration guide names each changed symbol and its replacement. Where feasible, a compatibility shim eases staged migration.

## Conformance

See `CONFORMANCE.md` for the TCK pass rate, deviations, GQL position, and non-goals.
