---
title: "Running queries"
description: "db.Run, db.Query, db.Exec, iterating Result, the value model, parameters, and Summary."
weight: 20
---

## The three entry points

gr provides three methods for running Cypher on a `*gr.DB`:

| Method | Use when |
|---|---|
| `db.Run(ctx, query, params)` | You do not know at compile time whether the query reads or writes |
| `db.Query(ctx, query, params)` | The query is a read; reject it at the API level if it writes |
| `db.Exec(ctx, query, params)` | The query writes or changes schema; return a Summary instead of a Result |

`db.Query` and `db.Exec` are convenience wrappers around `db.Run`.
For read-heavy code, use `db.Query` — it enforces at the API level that no write sneaks into a read path.

## db.Run

```go
result, err := db.Run(ctx, `
    MATCH (p:Person {name:$name})-[:KNOWS]->(friend:Person)
    RETURN friend.name AS name, friend.age AS age
`, map[string]any{"name": "Alice"})
if err != nil {
    return err
}
defer result.Close()
```

`db.Run` runs inside an implicit auto-commit transaction.
For explicit transactions, use `tx.Run` instead.

## db.Query

Same signature as `db.Run`.
Returns an error immediately if the query text contains a write clause.

```go
res, err := db.Query(ctx, `MATCH (n:Person) RETURN n.name`, nil)
```

## db.Exec

For write and schema queries.
Returns `(*gr.Summary, error)` instead of `(*gr.Result, error)`.

```go
summary, err := db.Exec(ctx, `CREATE (:Person {name:$name})`, map[string]any{"name": "Alice"})
if err != nil {
    return err
}
fmt.Printf("created %d node(s)\n", summary.NodesCreated())
```

## Iterating a Result

`*gr.Result` is a forward-only cursor.
Call `Next()` before reading each record:

```go
for res.Next() {
    rec := res.Record()
    name, _ := rec.Get("name")
    age, _ := rec.Get("age")
    fmt.Printf("%v (age %v)\n", name, age)
}
if err := res.Err(); err != nil {
    return err
}
```

Always check `res.Err()` after the loop — it returns any error that occurred during streaming.
Always call `res.Close()` when done (a `defer` is the right place).
Calling `Next()` after it returns `false` is safe and always returns `false`.

## Record

`rec.Get(key)` returns the value for the named column as `any`.
`rec.GetByIndex(i)` returns the value for column `i`.
`rec.Keys()` returns the column names.
`rec.Values()` returns all values as `[]any`.

```go
rec := res.Record()
name := rec.Get("name").(string)
age := rec.Get("age").(int64)
```

## The value model

gr maps Cypher types to Go types:

| Cypher type | Go type |
|---|---|
| `null` | `nil` |
| `Boolean` | `bool` |
| `Integer` | `int64` |
| `Float` | `float64` |
| `String` | `string` |
| `Bytes` | `[]byte` |
| `List` | `[]any` |
| `Map` | `map[string]any` |
| `Node` | `gr.Node` |
| `Relationship` | `gr.Relationship` |
| `Path` | `gr.Path` |

Use a type switch for safe extraction:

```go
val := rec.Get("n")
switch v := val.(type) {
case nil:
    fmt.Println("null")
case bool:
    fmt.Println("bool:", v)
case int64:
    fmt.Println("int:", v)
case float64:
    fmt.Println("float:", v)
case string:
    fmt.Println("string:", v)
case []any:
    fmt.Println("list len:", len(v))
case map[string]any:
    fmt.Println("map:", v)
case gr.Node:
    fmt.Println("node id:", v.ElementId())
case gr.Relationship:
    fmt.Println("rel type:", v.Type())
case gr.Path:
    fmt.Println("path length:", v.Length())
}
```

## Parameters

Pass parameters as `map[string]any`.
Reference them in Cypher with `$name`:

```cypher
MATCH (p:Person {name:$name, age:$age}) RETURN p
```

```go
params := map[string]any{"name": "Alice", "age": int64(30)}
res, err := db.Query(ctx, query, params)
```

Parameter values must be one of the Go types in the value model table.
Nested `[]any` and `map[string]any` are supported.
You cannot pass a `gr.Node` or `gr.Relationship` as a parameter — use the node's `ElementId()` instead.

## Summary

`db.Exec` returns a `*gr.Summary`:

```go
summary, _ := db.Exec(ctx, `...`, params)
fmt.Println(summary.NodesCreated())
fmt.Println(summary.NodesDeleted())
fmt.Println(summary.RelationshipsCreated())
fmt.Println(summary.RelationshipsDeleted())
fmt.Println(summary.PropertiesSet())
fmt.Println(summary.LabelsAdded())
fmt.Println(summary.LabelsRemoved())
fmt.Println(summary.IndexesAdded())
fmt.Println(summary.IndexesRemoved())
fmt.Println(summary.ConstraintsAdded())
fmt.Println(summary.ConstraintsRemoved())
```

## Error types

| Error type | Trigger |
|---|---|
| `*gr.ParseError` | The query has a syntax error |
| `*gr.BindError` | The query is syntactically valid but semantically wrong (e.g., undefined variable) |
| `*engine.ConstraintError` | A write violated a uniqueness or existence constraint |
| `*gr.ConflictError` | A write-write conflict with another concurrent transaction |
| `context.DeadlineExceeded` | The context deadline expired during execution |
| `context.Canceled` | The context was cancelled |

## EXPLAIN and PROFILE

`EXPLAIN` returns the query plan without running the query.
`PROFILE` runs the query and annotates the plan with row counts and timing.

```go
res, err := db.Run(ctx, `EXPLAIN MATCH (p:Person) WHERE p.age > 25 RETURN p.name`, nil)
if err != nil {
    log.Fatal(err)
}
defer res.Close()
// The plan is in res.Summary().Plan()
```

Or from the CLI:

```bash
gr run graph.gr "PROFILE MATCH (p:Person) WHERE p.age > 25 RETURN p.name"
```
