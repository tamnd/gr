---
title: "Writing graphs"
description: "CREATE, MERGE, SET, REMOVE, DELETE, and DETACH DELETE."
weight: 20
---

## CREATE

`CREATE` inserts nodes and relationships.

Create a node with a label and properties:

```cypher
CREATE (:Person {name:"Alice", age:30})
```

Create two nodes in one statement:

```cypher
CREATE (:Person {name:"Alice"}), (:Person {name:"Bob"})
```

Create a relationship between two existing nodes:

```cypher
MATCH (a:Person {name:"Alice"}), (b:Person {name:"Bob"})
CREATE (a)-[:KNOWS {since:2022}]->(b)
```

Create a node and a relationship in one pattern:

```cypher
CREATE (a:Person {name:"Alice"})-[:KNOWS]->(b:Person {name:"Bob"})
```

Return what you created:

```cypher
CREATE (p:Person {name:"Carol", age:28})
RETURN p
```

## MERGE

`MERGE` is find-or-create.
It matches the pattern and creates it only if nothing matches:

```cypher
MERGE (p:Person {name:"Alice"})
```

If a Person node with `name:"Alice"` already exists, `MERGE` returns it.
If it does not exist, `MERGE` creates it.

Use `ON CREATE SET` to set properties only when creating:

```cypher
MERGE (p:Person {name:"Alice"})
ON CREATE SET p.age = 30, p.created = timestamp()
ON MATCH SET p.lastSeen = timestamp()
RETURN p
```

`MERGE` is often used on indexed properties so it can find existing nodes efficiently.
Without an index, `MERGE` scans every node with the given label.

## SET

`SET` updates properties or adds labels:

```cypher
MATCH (p:Person {name:"Alice"})
SET p.age = 31
```

Set multiple properties:

```cypher
MATCH (p:Person {name:"Alice"})
SET p.age = 31, p.city = "Berlin"
```

Replace all properties at once:

```cypher
MATCH (p:Person {name:"Alice"})
SET p = {name:"Alice", age:31, city:"Berlin"}
```

Add a label:

```cypher
MATCH (p:Person {name:"Alice"})
SET p:Admin
```

`SET p += {city:"Berlin"}` merges the map into the existing properties without removing others.

## REMOVE

`REMOVE` removes a property or a label:

```cypher
MATCH (p:Person {name:"Alice"})
REMOVE p.city
```

Remove a label:

```cypher
MATCH (p:Person {name:"Alice"})
REMOVE p:Admin
```

## DELETE

`DELETE` removes nodes and relationships.
You must delete a node's relationships before deleting the node:

```cypher
MATCH (p:Person {name:"Alice"})-[r]-()
DELETE r
```

Then:

```cypher
MATCH (p:Person {name:"Alice"})
DELETE p
```

`DETACH DELETE` does both in one step:

```cypher
MATCH (p:Person {name:"Alice"})
DETACH DELETE p
```

This deletes the node and all relationships connected to it.

## FOREACH

`FOREACH` iterates over a list and applies an update clause to each element:

```cypher
MATCH p = (start:Person {name:"Alice"})-[:KNOWS*]->(end:Person)
FOREACH (n IN nodes(p) | SET n.visited = true)
```

`FOREACH` accepts only write clauses (`SET`, `CREATE`, `MERGE`, `DELETE`, `REMOVE`, `FOREACH`) in its body.

## Transactions and atomicity

Every write query runs in a transaction.
If you call `db.Exec` without opening an explicit transaction, gr wraps the statement in an auto-commit transaction.

For a sequence of writes that must succeed or fail together, open an explicit transaction:

```go
tx, err := db.BeginTx(ctx, gr.TxOptions{})
if err != nil {
    return err
}
defer tx.Rollback()

if _, err := tx.Exec(ctx, `CREATE (:Person {name:$name})`, map[string]any{"name": "Alice"}); err != nil {
    return err
}
if _, err := tx.Exec(ctx, `CREATE (:Person {name:$name})`, map[string]any{"name": "Bob"}); err != nil {
    return err
}
return tx.Commit()
```

See the [transactions guide](/library/transactions/) for the full API.

## Constraint violations

If a write violates a uniqueness or existence constraint, gr returns `*engine.ConstraintError`.
The transaction is rolled back automatically.

```go
_, err = db.Exec(ctx, `CREATE (:Person {email:"a@b.com"})`, nil)
if ce := (*engine.ConstraintError)(nil); errors.As(err, &ce) {
    fmt.Println("constraint violated:", ce.Constraint)
}
```

## Running a write in Go

```go
summary, err := db.Exec(ctx, `
    CREATE (:Person {name:$name, age:$age})
`, map[string]any{"name": "Alice", "age": 30})
if err != nil {
    return err
}
fmt.Printf("created %d node(s)\n", summary.NodesCreated())
```
