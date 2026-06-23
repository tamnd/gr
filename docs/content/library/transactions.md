---
title: "Transactions"
description: "Explicit transactions, managed write closures, retry semantics, and isolation."
weight: 30
---

## Auto-commit transactions

`db.Run`, `db.Query`, and `db.Exec` each run inside an implicit auto-commit transaction.
The transaction commits if the statement succeeds and rolls back if it returns an error.

For most single-statement writes, auto-commit is enough.

## Explicit transactions

Open an explicit transaction when you need multiple statements to succeed or fail together:

```go
tx, err := db.BeginTx(ctx, gr.TxOptions{})
if err != nil {
    return err
}
defer tx.Rollback() // no-op if already committed

_, err = tx.Exec(ctx, `CREATE (:Person {name:$name})`, map[string]any{"name": "Alice"})
if err != nil {
    return err
}
_, err = tx.Exec(ctx, `CREATE (:Person {name:$name})`, map[string]any{"name": "Bob"})
if err != nil {
    return err
}
return tx.Commit()
```

`defer tx.Rollback()` is safe to call after a commit — it is a no-op.

`tx.Run`, `tx.Query`, and `tx.Exec` have the same signatures as their `db` counterparts.

## Managed write transactions

For write transactions that may hit write-write conflicts, use `db.ExecuteWrite`.
gr automatically retries the closure on `*gr.ConflictError` up to `Options.MaxRetries` times:

```go
_, err = db.ExecuteWrite(ctx, func(tx gr.ManagedTx) (any, error) {
    summary, err := tx.Exec(ctx, `
        MATCH (p:Person {name:$name})
        SET p.loginCount = p.loginCount + 1
    `, map[string]any{"name": "Alice"})
    return summary, err
})
```

The closure must be re-runnable with no external side effects.
Do not send emails or call external APIs inside it — the retry may call it more than once.

`db.ExecuteWrite` returns the value your closure returns (typed as `any`) and any error.

## Managed read transactions

`db.ExecuteRead` is the read-only counterpart:

```go
name, err := db.ExecuteRead(ctx, func(tx gr.ManagedTx) (any, error) {
    res, err := tx.Query(ctx, `MATCH (p:Person {id:$id}) RETURN p.name`, map[string]any{"id": id})
    if err != nil {
        return nil, err
    }
    defer res.Close()
    if res.Next() {
        return res.Record().Get("p.name"), nil
    }
    return nil, res.Err()
})
```

Read transactions run under snapshot isolation: they see a consistent view of the graph as of the moment the transaction opened.
Concurrent writes do not affect the read view.

## Read-your-writes

Inside a write transaction, reads see the uncommitted writes from the same transaction:

```go
tx, _ := db.BeginTx(ctx, gr.TxOptions{})
tx.Exec(ctx, `CREATE (:Temp {x:1})`, nil)

res, _ := tx.Query(ctx, `MATCH (t:Temp) RETURN t.x`, nil)
// res sees the newly created node, even though tx has not committed yet
res.Close()
tx.Rollback()
```

## Isolation

gr uses snapshot isolation.
Every transaction sees the graph as it was when the transaction opened.
A concurrent write transaction that commits later is invisible to ongoing read transactions.

Write transactions serialize through a single writer slot by default.
If two write transactions try to modify overlapping data, the later one gets a `*gr.ConflictError` and the `ExecuteWrite` retry loop handles it automatically.

## TxOptions

```go
tx, err := db.BeginTx(ctx, gr.TxOptions{
    ReadOnly:    false,
    MaxRetries:  5,
    BusyTimeout: 2 * time.Second,
})
```

| Field | Default | Description |
|---|---|---|
| `ReadOnly` | `false` | Open a read-only transaction (fails on writes) |
| `MaxRetries` | from Options | Override the retry count for this transaction |
| `BusyTimeout` | from Options | Override the busy-wait timeout |

## Bulk writes

For large imports through the library API, use one large explicit transaction instead of many small ones.
The WAL commit overhead scales with the number of transactions, not the number of statements.

```go
tx, err := db.BeginTx(ctx, gr.TxOptions{})
if err != nil {
    return err
}
defer tx.Rollback()

for _, row := range rows {
    if _, err := tx.Exec(ctx, `CREATE (:Node {id:$id, name:$name})`,
        map[string]any{"id": row.ID, "name": row.Name}); err != nil {
        return err
    }
}
return tx.Commit()
```

For very large datasets, the bulk importer (`gr import`) is faster — it bypasses the WAL entirely.
See the [bulk import guide](/operations/bulk-import/).
