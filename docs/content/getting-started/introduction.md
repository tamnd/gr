---
title: "Introduction"
description: "The graph model, openCypher, and why gr fits some problems better than a relational database."
weight: 10
---

A relational database is built around the table: data is rows, queries are joins.
For many problems — inventory, user accounts, billing — that shape is a perfect fit.
For others it fights you.

Model a social network as tables and a "who are Alice's friends of friends who live in Berlin?" query turns into a three-way join and a subquery.
Model a software dependency tree and you need a recursive CTE to walk it.
Model a fraud ring and the rings are literally in the edges, but edges are not first-class citizens in SQL.

gr stores data as a labeled-property graph.
Nodes and relationships are the primary objects, properties are just values attached to them, and Cypher is a query language built around graph patterns instead of table joins.

## Nodes, relationships, labels, properties

A **node** is a vertex — a person, a product, a city.
It has zero or more **labels** that classify it: `:Person`, `:Product`, `:City`.
It has zero or more **properties**: key-value pairs where values are booleans, integers, floats, strings, or lists of those.

A **relationship** is a directed edge between two nodes.
It has exactly one **type**: `:KNOWS`, `:BOUGHT`, `:LOCATED_IN`.
It also has properties.

```
(:Person {name:"Alice", age:30})-[:KNOWS {since:2022}]->(:Person {name:"Bob", age:25})
```

That is a complete data model for a friendship: two Person nodes, one KNOWS relationship, three properties.

## openCypher

Cypher is the query language gr speaks.
It is declarative: you describe the graph pattern you are looking for, and the engine finds it.

```cypher
MATCH (alice:Person {name:"Alice"})-[:KNOWS]->(friend:Person)
WHERE friend.age < 30
RETURN friend.name, friend.age
ORDER BY friend.age
```

Read that as: find every Person node named Alice, follow outgoing KNOWS edges to friend nodes that are younger than 30, and return their names and ages.
No joins.
No foreign keys.
The pattern is the query.

gr implements openCypher — the same language Neo4j, Amazon Neptune, Memgraph, and others use.
Queries you write for gr work on those systems and vice versa.

## The SQLite feel

SQLite's pitch is that a database does not have to be a server.
Open a file, run SQL, close it.
The file is the database.

gr's pitch is the same thing for graphs.
Call `gr.Open("social.gr", gr.Options{})` and you have a running, durable, ACID-compliant graph database in your process.
No daemon to start, no connection string to configure, no deployment to manage.

```go
db, err := gr.Open("social.gr", gr.Options{})
if err != nil {
    log.Fatal(err)
}
defer db.Close()

_, err = db.Exec(ctx, `CREATE (:Person {name:"Alice", age:30})`, nil)
```

The `.gr` file holds the graph.
Two sidecar files (`.gr-wal` and `.gr-shm`) appear while the database is open and are folded back in on a clean close, the same way SQLite's WAL works.
Back up the `.gr` file and you have the whole graph.

## When gr fits and when it does not

gr fits when:

- The problem is naturally a graph (social networks, dependency trees, knowledge bases, fraud detection, route planning).
- You want the graph database in your Go program, not as a separate service.
- Your dataset fits on one machine (gr is single-node by design; see the [roadmap](/reference/roadmap/) for post-1.0 plans).
- You want portable, schema-optional data in a single file.

gr does not fit when:

- You need distributed query execution across many machines — that is a different product.
- Your data is fundamentally relational and you are comfortable with SQL — use SQLite or Postgres.
- You need the full GQL (ISO/IEC 39075) standard — gr targets openCypher v1.0 and documents deviations honestly in [CONFORMANCE.md](https://github.com/tamnd/gr/blob/main/CONFORMANCE.md).

## What gr 1.0 ships

- The embedded Go library (`github.com/tamnd/gr`).
- openCypher read and write: MATCH, CREATE, MERGE, SET, DELETE, REMOVE, RETURN, aggregation, variable-length paths, shortestPath, indexes, and constraints.
- The `gr` CLI: interactive Cypher shell, one-shot query runner, bulk importer, schema inspector, backup, and `EXPLAIN`/`PROFILE`.
- The Bolt+HTTP server (`gr serve`) for Neo4j driver connectivity from Go, Python, JavaScript, Java, .NET, and Rust.
- A stable library API, a frozen configuration surface, a versioned file format, and the Bolt wire protocol — all documented in [STABILITY.md](https://github.com/tamnd/gr/blob/main/STABILITY.md).

Next: [install gr](/getting-started/installation/).
