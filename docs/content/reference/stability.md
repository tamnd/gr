---
title: "Stability"
description: "What gr commits not to break across releases."
weight: 30
---

The full stability contract is in [STABILITY.md](https://github.com/tamnd/gr/blob/main/STABILITY.md) in the repository.
This page is a summary.

## Four axes

gr makes stability commitments across four axes.

**Library API** — the exported names of `github.com/tamnd/gr`.
gr tracks 148 exported names as of v1.0.
Adding a new exported name or changing a non-breaking signature (adding a functional option) is a minor-version increment.
Removing or changing an existing name is a major-version increment.

**Configuration surface** — the 51 configuration knobs: their names, tiers, and default values.
Renaming a knob or changing its tier is a breaking change.
Adding a new knob is not.

**File format** — the `.gr` file format.
Every file carries a format version in its header byte.
A newer gr can read files created by an older gr.
An older gr refuses to open a file created by a newer gr and returns `*gr.ErrVersionMismatch`.

**Wire protocol** — the Bolt protocol over `gr serve`.
gr supports Bolt v4.4 and Bolt v5.0.
Adding a new Bolt version is additive.
Dropping a Bolt version requires a major release with deprecation notice.

## Semantic versioning

- **Patch versions** (1.0.x) — bug fixes only; no new behaviour, no API additions.
- **Minor versions** (1.x.0) — additive changes only: new exported names, new knobs, new PRAGMA values, expanded TCK coverage.
- **Major versions** (x.0.0) — breaking changes with a deprecation cycle.

## What is not covered

The [CONFORMANCE.md](https://github.com/tamnd/gr/blob/main/CONFORMANCE.md) document records TCK deviations.
Scenarios in the deviations registry are not stable: gr may implement them in a minor version.
Scenarios that currently pass are stable: gr will not regress them without a major version.

Internal packages (`github.com/tamnd/gr/internal/...` — none exist in gr; see [no internal/ policy](https://github.com/tamnd/gr)) and test helpers are not covered by the stability guarantee.
