---
title: "Quick start"
description: "From an empty terminal to a persisted, queryable graph database in five minutes."
weight: 30
---

This guide walks you from nothing to a real graph database with nodes, relationships, an index, and a persisted `.gr` file you can reopen later.

## With the Go library

Create a file `main.go`:

```go
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/tamnd/gr"
)

func main() {
	ctx := context.Background()

	db, err := gr.Open("social.gr", gr.Options{})
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	// Create an index so name lookups are fast.
	_, err = db.Exec(ctx, `CREATE INDEX FOR (p:Person) ON (p.name)`, nil)
	if err != nil {
		log.Fatal(err)
	}

	// Add two people.
	_, err = db.Exec(ctx, `
		CREATE (:Person {name: $a, age: 30})
		CREATE (:Person {name: $b, age: 25})
	`, map[string]any{"a": "Alice", "b": "Bob"})
	if err != nil {
		log.Fatal(err)
	}

	// Connect them.
	_, err = db.Exec(ctx, `
		MATCH (a:Person {name: $a}), (b:Person {name: $b})
		CREATE (a)-[:KNOWS {since: 2022}]->(b)
	`, map[string]any{"a": "Alice", "b": "Bob"})
	if err != nil {
		log.Fatal(err)
	}

	// Query the graph.
	res, err := db.Query(ctx, `
		MATCH (a:Person)-[r:KNOWS]->(b:Person)
		RETURN a.name AS from, b.name AS to, r.since AS since
	`, nil)
	if err != nil {
		log.Fatal(err)
	}
	defer res.Close()

	for res.Next() {
		rec := res.Record()
		fmt.Printf("%v knows %v (since %v)\n",
			rec.Get("from"), rec.Get("to"), rec.Get("since"))
	}
	if err := res.Err(); err != nil {
		log.Fatal(err)
	}
}
```

Run it:

```bash
go mod init example && go get github.com/tamnd/gr@latest && go run main.go
```

```
Alice knows Bob (since 2022)
```

The file `social.gr` now exists on disk.
Run the program again and it picks up where it left off — the nodes, relationships, and index are all there.

## With the CLI

Create the same graph interactively:

```bash
gr shell social.gr
```

```
gr> CREATE INDEX FOR (p:Person) ON (p.name);
Created 1 index.
gr> CREATE (:Person {name:"Alice", age:30}), (:Person {name:"Bob", age:25});
Created 2 nodes, set 4 properties.
gr> MATCH (a:Person {name:"Alice"}), (b:Person {name:"Bob"}) CREATE (a)-[:KNOWS {since:2022}]->(b);
Created 1 relationship, set 1 property.
gr> MATCH (a:Person)-[r:KNOWS]->(b:Person) RETURN a.name, b.name, r.since;
╔══════════╦══════════╦═══════╗
║ a.name   ║ b.name   ║ since ║
╠══════════╬══════════╬═══════╣
║ Alice    ║ Bob      ║ 2022  ║
╚══════════╩══════════╩═══════╝
gr> :quit
```

One-shot mode without entering the shell:

```bash
gr run social.gr "MATCH (n:Person) RETURN n.name, n.age ORDER BY n.age"
```

```
╔══════════╦═══════╗
║ n.name   ║ n.age ║
╠══════════╬═══════╣
║ Bob      ║ 25    ║
║ Alice    ║ 30    ║
╚══════════╩═══════╝
```

JSON output:

```bash
gr run --format json social.gr "MATCH (n:Person) RETURN n.name"
```

```json
[{"n.name":"Alice"},{"n.name":"Bob"}]
```

## What was persisted

After either path, `social.gr` holds the complete graph: the nodes, the relationship, the index, and all properties.
Two sidecar files (`social.gr-wal`, `social.gr-shm`) appear while the database is open and disappear on a clean close, exactly like SQLite's WAL files.
Include only `social.gr` in a backup; the sidecars are ephemeral.

## Schema inspection

```bash
gr run social.gr "SHOW INDEXES"
gr run social.gr "SHOW LABELS"
gr run social.gr "SHOW RELATIONSHIP TYPES"
```

Or inside the shell:

```
gr> :schema
Labels    : Person
Rel types : KNOWS
Indexes   : INDEX FOR (p:Person) ON (p.name)
Constraints: (none)
```

## Next steps

- The [Cypher guide](/cypher/) explains patterns, writes, aggregation, and paths.
- The [library guide](/library/) covers the full Go API.
- The [CLI guide](/cli/) covers every command and flag.
- The [server guide](/server/) shows how to connect with Neo4j drivers over Bolt.
