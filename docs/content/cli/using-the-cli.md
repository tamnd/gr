---
title: "Using the CLI"
description: "Shell, one-shot queries, import, export, dump, info, check, backup, and serve."
weight: 10
---

## Interactive shell

```bash
gr shell graph.gr
```

Starts an interactive Cypher REPL.
Type a Cypher query and end it with a semicolon to run it.
Multi-line queries are fine — gr buffers until it sees `;`.

```
gr> MATCH (n:Person)
...> RETURN n.name, n.age
...> ORDER BY n.age;
╔══════════╦═══════╗
║ n.name   ║ n.age ║
╠══════════╬═══════╣
║ Bob      ║ 25    ║
║ Alice    ║ 30    ║
╚══════════╩═══════╝
(2 rows, 1ms)
```

Shell commands (no semicolon needed):

| Command | Description |
|---|---|
| `:quit` or `:exit` | Exit the shell |
| `:help` | Print available shell commands |
| `:schema` | Show labels, relationship types, indexes, and constraints |
| `:indexes` | Show indexes |
| `:constraints` | Show constraints |
| `:begin` | Begin an explicit transaction |
| `:commit` | Commit the current transaction |
| `:rollback` | Roll back the current transaction |
| `:format table` | Set output to the table format (default) |
| `:format json` | Set output to JSON |
| `:format csv` | Set output to CSV |

History and tab completion work automatically.
The shell stores history in `~/.gr_history`.

## One-shot queries

Run a single query without entering the shell:

```bash
gr run graph.gr "MATCH (n:Person) RETURN n.name, n.age ORDER BY n.name"
```

With parameters:

```bash
gr run graph.gr "MATCH (p:Person {name:\$name}) RETURN p.age" --params '{"name":"Alice"}'
```

Output formats:

```bash
gr run --format json graph.gr "MATCH (n:Person) RETURN n.name"
gr run --format csv  graph.gr "MATCH (n:Person) RETURN n.name, n.age"
```

Exit codes: `0` success, `1` query error, `2` file or argument error.

## Bulk import

Load nodes and relationships from CSV:

```bash
gr import graph.gr \
  --nodes Person=people.csv \
  --nodes Product=products.csv \
  --rels BOUGHT=bought.csv
```

Node CSV header: `id:ID(Space), :LABEL, name:string, age:int`
Relationship CSV header: `:START_ID(Space), :END_ID(Space), :TYPE, since:int`

See the [bulk import guide](/operations/bulk-import/) for the full header grammar and options.

## Export

Export nodes and relationships to CSV:

```bash
gr export graph.gr --output-dir ./export/
```

This creates one CSV per label for nodes and one per relationship type for relationships, in the same format `gr import` accepts.

## Dump

Produce a logical dump as Cypher `CREATE` statements:

```bash
gr dump graph.gr > dump.cypher
```

Restore from a dump:

```bash
gr shell graph.gr < dump.cypher
```

Or pipe into `gr run`:

```bash
gr run graph.gr "$(cat dump.cypher)"
```

## Info

Print file statistics:

```bash
gr info graph.gr
```

```
File:          graph.gr
UUID:          550e8400-e29b-41d4-a716-446655440000
Nodes:         12 450
Relationships: 83 291
Properties:    248 744
Page size:     4096 bytes
Page count:    8 192
File size:     32.0 MB
WAL frames:    0
```

## Check

Validate the structural integrity of a database file:

```bash
gr check graph.gr
```

Checks every page checksum, the free-list, the B-tree structure, and the WAL consistency.
Exits `0` if the file is healthy.
Use this after a restore before trusting the backup.

## Backup

Take a hot backup of a live database:

```bash
gr backup graph.gr backup.gr
```

The backup is a complete, consistent snapshot that can be opened immediately.
The WAL is folded into the backup file — no sidecars needed for the backup.

Restore:

```bash
gr restore backup.gr graph.gr
```

See the [backup guide](/operations/backup/) for PITR, validation, and recommendations.

## Start the server

```bash
gr serve graph.gr
```

Starts the Bolt (port 7687) and HTTP (port 7474) servers.
See the [server guide](/server/) for driver connectivity, auth, and TLS.

## PRAGMA

Read or set a configuration knob:

```bash
gr pragma graph.gr synchronous          # print current value
gr pragma graph.gr synchronous NORMAL   # set the value (persistent)
```

See the [PRAGMA reference](/configuration/pragmas/) for the full knob catalogue.
