---
title: "gr"
description: "gr is an embedded, single-file, labeled-property-graph database for Go that speaks Cypher. Pure Go, zero cgo, one .gr file, SQLite feel for graphs."
heroTitle: "A graph database that fits in a file"
heroLead: "gr stores a whole labeled-property graph in one self-describing .gr file, queries it with openCypher, and gives you the SQLite feel for graphs: open a file, run queries, close it. No server, no configuration, no cluster."
heroPrimaryURL: "/getting-started/quick-start/"
heroPrimaryText: "Get started"
---

A relational database keeps rows in tables.
A graph database keeps nodes and relationships, so you model a social network, a dependency graph, or a knowledge base the way it actually is, not as a tangle of foreign-key joins.

gr brings that model to Go without a server:

```go
db, err := gr.Open("friends.gr", gr.Options{})
if err != nil {
    log.Fatal(err)
}
defer db.Close()

db.Exec(ctx, `CREATE (:Person {name:"Alice"})-[:KNOWS]->(:Person {name:"Bob"})`, nil)

res, err := db.Query(ctx, `MATCH (a)-[:KNOWS]->(b) RETURN a.name, b.name`, nil)
if err != nil {
    log.Fatal(err)
}
defer res.Close()
for res.Next() {
    fmt.Println(res.Record().Values())
}
```

No network call.
No daemon.
One file on disk.

## What gr provides

- **An embedded library.** Import `github.com/tamnd/gr`, open a `.gr` file, and run Cypher queries. The database lives inside your process.
- **openCypher.** The same graph query language Neo4j, Amazon Neptune, and others use. MATCH patterns, CREATE and MERGE, variable-length paths, aggregation, indexes, and constraints.
- **A CLI.** `gr` is an interactive Cypher shell, a one-shot runner, an importer, a backup tool, and a schema inspector in one pure-Go binary.
- **A Bolt server.** `gr serve` accepts connections from any Neo4j driver over the standard Bolt wire protocol.
- **Durability.** WAL journaling, per-page checksums, crash recovery. A file the process crashes into comes back clean on the next open.
- **Pure Go.** No cgo. Cross-compiles everywhere Go does. One static binary, no shared libraries.

## Where to go next

- New here? Read the [introduction](/getting-started/introduction/) to understand the data model, then follow the [quick start](/getting-started/quick-start/).
- Installing? See [installation](/getting-started/installation/).
- Writing Cypher? Start with [reading graphs](/cypher/reading-graphs/) and [writing graphs](/cypher/writing-graphs/).
- Embedding gr in Go? See the [library guide](/library/).
- Running the CLI or a server? See the [CLI guide](/cli/) and the [server guide](/server/).
- Need every flag and knob? The [reference](/reference/) has the full CLI surface and configuration catalogue.
