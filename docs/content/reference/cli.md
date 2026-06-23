---
title: "CLI reference"
description: "Every gr command, subcommand, and flag."
weight: 10
---

## Synopsis

```
gr [command] [flags]
```

## Commands

| Command | Description |
|---|---|
| `shell` | Start the interactive Cypher shell |
| `run` | Run a single Cypher query and exit |
| `import` | Bulk-load nodes and relationships from CSV |
| `export` | Export nodes and relationships to CSV |
| `dump` | Write a logical dump as Cypher CREATE statements |
| `load` | Load (restore) a logical dump into a database |
| `info` | Print file statistics |
| `check` | Validate structural integrity |
| `backup` | Take a hot backup |
| `restore` | Restore a backup |
| `serve` | Start the Bolt+HTTP server |
| `pragma` | Read or set a PRAGMA |
| `create-user` | Create a database user |
| `grant-role` | Grant a role to a user |
| `revoke-role` | Revoke a role from a user |
| `drop-user` | Delete a user |
| `wal-replay` | Replay WAL segments for point-in-time recovery |
| `version` | Print the gr version |

## Global flags

| Flag | Description |
|---|---|
| `--help` | Show help for a command |
| `--version` | Print version and exit |

---

## gr shell

```
gr shell [flags] <database.gr>
```

Start the interactive Cypher REPL.

| Flag | Default | Description |
|---|---|---|
| `--format` | `table` | Output format: `table`, `json`, `csv` |
| `--no-history` | `false` | Disable history file |
| `--history-file` | `~/.gr_history` | Path to the history file |

**Example:**

```bash
gr shell social.gr
```

---

## gr run

```
gr run [flags] <database.gr> <query>
```

Run a single Cypher query and print the results.

| Flag | Default | Description |
|---|---|---|
| `--format` | `table` | Output format: `table`, `json`, `csv` |
| `--params` | — | JSON object of query parameters, e.g. `'{"name":"Alice"}'` |
| `--read-only` | `false` | Open the database in read-only mode |

**Example:**

```bash
gr run social.gr "MATCH (n:Person) RETURN n.name ORDER BY n.name"
gr run --format json social.gr "MATCH (n:Person) RETURN n.name"
```

---

## gr import

```
gr import [flags] <database.gr>
```

Bulk-load nodes and relationships from CSV files.

| Flag | Default | Description |
|---|---|---|
| `--nodes` | — | `Label=file.csv` or `=file.csv` (repeatable); label from header if omitted |
| `--rels` | — | `TYPE=file.csv` or `=file.csv` (repeatable) |
| `--batch-size` | `50000` | Rows to buffer per segment |
| `--on-duplicate` | `skip` | Duplicate ID policy: `skip` or `error` |
| `--on-missing-id` | `skip` | Missing reference policy: `skip` or `error` |
| `--bad-tolerance` | `0` | Bad rows before aborting |
| `--delimiter` | `,` | CSV field delimiter |
| `--quote` | `"` | CSV quote character |
| `--array-delimiter` | `\|` | Array value delimiter within a cell |

**Example:**

```bash
gr import graph.gr \
  --nodes Person=people.csv \
  --rels KNOWS=knows.csv
```

---

## gr export

```
gr export [flags] <database.gr>
```

Export nodes and relationships to CSV.

| Flag | Default | Description |
|---|---|---|
| `--output-dir` | `.` | Directory to write CSV files to |
| `--labels` | all | Comma-separated list of labels to export |
| `--rel-types` | all | Comma-separated list of relationship types to export |
| `--delimiter` | `,` | CSV field delimiter |

**Example:**

```bash
gr export --output-dir ./export/ graph.gr
```

---

## gr dump

```
gr dump [flags] <database.gr>
```

Write the database as Cypher `CREATE` statements to stdout.

| Flag | Default | Description |
|---|---|---|
| `--batch-size` | `1000` | Rows per `CREATE` batch |

**Example:**

```bash
gr dump graph.gr > dump.cypher
```

---

## gr load

```
gr load [flags] <database.gr> [dump.cypher]
```

Execute a Cypher dump file against a database.
Reads from stdin if no file is given.

| Flag | Default | Description |
|---|---|---|
| `--stop-on-error` | `true` | Abort on the first error |

**Example:**

```bash
gr load new.gr dump.cypher
cat dump.cypher | gr load new.gr
```

---

## gr info

```
gr info <database.gr>
```

Print file statistics: node count, relationship count, page count, file size, UUID, and WAL state.

**Example:**

```bash
gr info graph.gr
```

---

## gr check

```
gr check [flags] <database.gr>
```

Validate structural integrity: page checksums, free-list, B-tree structure, WAL consistency.

| Flag | Default | Description |
|---|---|---|
| `--quick` | `false` | Skip deep B-tree traversal; check only the header and free-list |

**Example:**

```bash
gr check graph.gr
```

---

## gr backup

```
gr backup [flags] <database.gr> <output.gr>
```

Take a hot backup of a live or closed database.

| Flag | Default | Description |
|---|---|---|
| `--verify` | `false` | Run `gr check` on the backup file after writing |

**Example:**

```bash
gr backup graph.gr backup.gr
gr backup --verify graph.gr backup.gr
```

---

## gr restore

```
gr restore [flags] <backup.gr> <database.gr>
```

Restore a backup to a database file.
The destination must be closed.

| Flag | Default | Description |
|---|---|---|
| `--check` | `true` | Validate the source backup before restoring |

**Example:**

```bash
gr restore backup.gr graph.gr
```

---

## gr serve

```
gr serve [flags] <database.gr>
```

Start the Bolt wire-protocol server and the HTTP JSON API.

| Flag | Default | Description |
|---|---|---|
| `--bolt-addr` | `0.0.0.0:7687` | Bolt listener address |
| `--http-addr` | `0.0.0.0:7474` | HTTP listener address |
| `--tls-cert` | — | Path to TLS certificate file |
| `--tls-key` | — | Path to TLS private key file |
| `--auth` | `on` | Authentication mode: `on` or `off` |
| `--metrics-addr` | — | Prometheus metrics listener address |
| `--query-log` | — | Path to slow-query log file |
| `--slow-query-threshold` | `0` | Minimum duration to log a query |
| `--wal-archive-cmd` | — | Shell command to ship a sealed WAL segment; `{segment}` is replaced by the path |
| `--read-only` | `false` | Reject all write queries |

**Example:**

```bash
gr serve graph.gr
gr serve --bolt-addr 127.0.0.1:7687 --auth off graph.gr
gr serve --tls-cert server.crt --tls-key server.key graph.gr
```

---

## gr pragma

```
gr pragma <database.gr> <name> [<value>]
```

Read or set a persistent PRAGMA.

**Example:**

```bash
gr pragma graph.gr synchronous
gr pragma graph.gr synchronous NORMAL
gr pragma graph.gr pragma_list
```

---

## gr create-user / grant-role / revoke-role / drop-user

```
gr create-user [flags] <database.gr> <username>
gr grant-role [flags]  <database.gr> <username> <role>
gr revoke-role [flags] <database.gr> <username> <role>
gr drop-user   [flags] <database.gr> <username>
```

Roles: `reader`, `editor`, `publisher`, `admin`.

| Flag | For command | Description |
|---|---|---|
| `--password` | `create-user` | Initial password |
| `--role` | `create-user` | Initial role (default: `reader`) |

**Example:**

```bash
gr create-user --password s3cr3t --role admin graph.gr alice
gr grant-role  graph.gr alice publisher
gr revoke-role graph.gr alice publisher
gr drop-user   graph.gr alice
```

---

## gr wal-replay

```
gr wal-replay [flags] <database.gr> <wal-dir>
```

Replay archived WAL segments into a database file for point-in-time recovery.

| Flag | Default | Description |
|---|---|---|
| `--until` | — | Replay segments up to this RFC 3339 timestamp |
| `--dry-run` | `false` | Print what would be replayed without modifying the file |

**Example:**

```bash
gr wal-replay graph.gr ./wal-archive/ --until "2026-01-15T14:30:00Z"
```

---

## gr version

```
gr version
```

Print the gr version, commit, and build date.

**Example:**

```bash
gr version
# gr v1.0.0 (commit abc1234, built 2026-06-23)
```
