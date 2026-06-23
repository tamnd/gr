---
title: "Bulk import"
description: "Loading large datasets from CSV with gr import: node and relationship files, the header grammar, and performance notes."
weight: 10
---

## When to use bulk import

For initial loads of more than roughly 100,000 nodes or relationships, use `gr import` instead of individual `CREATE` statements.
The bulk importer bypasses the WAL, builds the storage structures directly, and is typically 10–100x faster than Cypher for cold loads.

For incremental writes after the initial load, use Cypher through the library or the CLI.

## Node CSV format

Each column is named by its header.
Special header tokens control how the importer reads the column:

| Header | Meaning |
|---|---|
| `id:ID` | Node ID used in relationship files to connect nodes; not stored as a property |
| `id:ID(Space)` | Node ID within a named ID space |
| `:LABEL` | One or more labels, pipe-separated: `Person\|Employee` |
| `name:string` | String property named `name` |
| `age:int` | Integer property named `age` |
| `score:float` | Float property named `score` |
| `active:boolean` | Boolean property named `active` |

Example `people.csv`:

```
id:ID(Person),name:string,age:int,:LABEL
1,Alice,30,Person
2,Bob,25,Person|Employee
3,Carol,35,Person
```

## Relationship CSV format

| Header | Meaning |
|---|---|
| `:START_ID` | ID of the start node (from node files) |
| `:START_ID(Space)` | Start node ID within a named ID space |
| `:END_ID` | ID of the end node |
| `:END_ID(Space)` | End node ID within a named ID space |
| `:TYPE` | Relationship type |
| `since:int` | Integer property named `since` |

Example `knows.csv`:

```
:START_ID(Person),:END_ID(Person),:TYPE,since:int
1,2,KNOWS,2022
2,3,KNOWS,2021
1,3,KNOWS,2020
```

## Running the import

```bash
gr import graph.gr \
  --nodes Person=people.csv \
  --rels KNOWS=knows.csv
```

Multiple node files, each with a different label group:

```bash
gr import graph.gr \
  --nodes Person=people.csv \
  --nodes Product=products.csv \
  --rels BOUGHT=bought.csv \
  --rels KNOWS=knows.csv
```

If your CSV has a `:LABEL` column, omit the label from the `--nodes` flag:

```bash
gr import graph.gr --nodes =people-with-labels.csv
```

## Options

| Flag | Default | Description |
|---|---|---|
| `--batch-size` | `50000` | Rows to buffer before flushing a segment |
| `--on-duplicate` | `skip` | What to do when two nodes have the same ID: `skip` or `error` |
| `--on-missing-id` | `skip` | What to do when a relationship references an unknown node ID: `skip` or `error` |
| `--bad-tolerance` | `0` | Number of bad rows to tolerate before aborting |
| `--delimiter` | `,` | CSV field delimiter |
| `--quote` | `"` | CSV quote character |
| `--array-delimiter` | `\|` | Delimiter for array values within a cell |

## How it works

The importer runs in four passes:

1. **Scan IDs** — read every node file and build an ID-to-position map.
2. **Build node columns** — write node storage segments from the buffered input.
3. **Sort relationships** — sort the relationship file by start-node position, then write the CSR (compressed sparse row) adjacency structure.
4. **Finalize** — write the catalog, flush, and remove the import temp files.

Because it builds storage directly, the importer does not checkpoint through the WAL.
The resulting file is a complete, sealed database file ready to open immediately.

## After import

The database is ready to open and query as soon as `gr import` exits.

Create indexes after the import if you plan to query on the imported properties:

```bash
gr run graph.gr "CREATE INDEX FOR (p:Person) ON (p.name)"
gr run graph.gr "CREATE INDEX FOR (p:Product) ON (p.sku)"
```

Verify the import:

```bash
gr info graph.gr
gr run graph.gr "MATCH (n) RETURN labels(n), count(*) ORDER BY count(*) DESC"
```
