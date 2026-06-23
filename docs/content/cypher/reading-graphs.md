---
title: "Reading graphs"
description: "MATCH patterns, WHERE filters, RETURN, ORDER BY, LIMIT, OPTIONAL MATCH, and WITH."
weight: 10
---

## MATCH

`MATCH` is the core of every read query.
It describes a graph pattern and the engine returns every subgraph that fits it.

```cypher
MATCH (n:Person)
RETURN n.name, n.age
```

A node pattern `(n:Person)` matches every node with the label `Person` and binds it to the variable `n`.
A relationship pattern connects two node patterns:

```cypher
MATCH (a:Person)-[:KNOWS]->(b:Person)
RETURN a.name, b.name
```

The arrow direction matters.
`-[:KNOWS]->` follows the relationship in its stored direction.
`<-[:KNOWS]-` follows it backwards.
`-[:KNOWS]-` (no arrow) matches either direction.

To match any relationship type:

```cypher
MATCH (a)-[r]->(b)
RETURN type(r), a.name, b.name
```

## Node and relationship properties in patterns

Inline property predicates narrow the pattern:

```cypher
MATCH (p:Person {name:"Alice", age:30})
RETURN p
```

This is equivalent to a `WHERE` clause:

```cypher
MATCH (p:Person)
WHERE p.name = "Alice" AND p.age = 30
RETURN p
```

Use the inline form for constant look-ups and `WHERE` for expressions.

## WHERE

`WHERE` accepts any boolean expression:

```cypher
MATCH (p:Person)
WHERE p.age > 25 AND p.name STARTS WITH "A"
RETURN p.name
```

String predicates: `STARTS WITH`, `ENDS WITH`, `CONTAINS`.

Null checks: `IS NULL`, `IS NOT NULL`.

Negation: `NOT`.

List membership: `n.name IN ["Alice", "Bob"]`.

Label check: `n:Person`.

## RETURN

`RETURN` projects the result.
Use aliases to rename columns:

```cypher
MATCH (p:Person)
RETURN p.name AS name, p.age AS age
```

`RETURN *` returns all bound variables.

`RETURN DISTINCT` removes duplicate rows:

```cypher
MATCH (p:Person)-[:KNOWS]->(friend:Person)
RETURN DISTINCT friend.name
```

## ORDER BY, SKIP, LIMIT

```cypher
MATCH (p:Person)
RETURN p.name, p.age
ORDER BY p.age DESC
SKIP 10
LIMIT 5
```

`ORDER BY` accepts multiple columns and `ASC`/`DESC` per column.
`SKIP` and `LIMIT` accept integer literals or parameters.

## OPTIONAL MATCH

`OPTIONAL MATCH` is a LEFT OUTER JOIN.
If the pattern does not match, the variables from it are `null` instead of the row being dropped:

```cypher
MATCH (p:Person)
OPTIONAL MATCH (p)-[:HAS_EMAIL]->(e:Email)
RETURN p.name, e.address
```

Persons without an email still appear; `e.address` is `null` for them.

## Multiple MATCH clauses

Multiple `MATCH` clauses in the same query combine like a cross product filtered by shared variables:

```cypher
MATCH (a:Person {name:"Alice"})
MATCH (b:Person {name:"Bob"})
MATCH (a)-[:KNOWS]->(b)
RETURN a.name, b.name
```

## WITH

`WITH` is a pipeline separator.
It ends one clause, projects intermediate results, and feeds them into the next:

```cypher
MATCH (p:Person)-[:KNOWS]->(friend:Person)
WITH p, count(friend) AS friendCount
WHERE friendCount > 3
RETURN p.name, friendCount
ORDER BY friendCount DESC
```

`WITH` is required whenever you aggregate before filtering on the aggregated value.

## UNWIND

`UNWIND` turns a list into rows:

```cypher
WITH ["Alice", "Bob", "Carol"] AS names
UNWIND names AS name
MATCH (p:Person {name: name})
RETURN p
```

This is the idiomatic way to look up a batch of known values.

## Parameters

Always use parameters for user-supplied values:

```cypher
MATCH (p:Person {name: $name})
RETURN p.age
```

Pass the parameter map from Go:

```go
res, err := db.Query(ctx,
    `MATCH (p:Person {name: $name}) RETURN p.age`,
    map[string]any{"name": "Alice"},
)
```

Parameters prevent injection and let the planner cache the query plan.

## Running a read in Go

```go
res, err := db.Query(ctx, `
    MATCH (a:Person)-[:KNOWS]->(b:Person)
    WHERE a.name = $name
    RETURN b.name AS friend, b.age AS age
    ORDER BY b.age
`, map[string]any{"name": "Alice"})
if err != nil {
    return err
}
defer res.Close()

for res.Next() {
    rec := res.Record()
    fmt.Printf("%v (age %v)\n", rec.Get("friend"), rec.Get("age"))
}
return res.Err()
```

`db.Query` rejects queries that write.
Use `db.Run` or `db.Exec` for write queries.
