---
title: "Indexes and constraints"
description: "Creating, dropping, and querying indexes and constraints in gr."
weight: 20
---

## Indexes

Indexes speed up `MATCH` patterns that filter on a property.
Without an index, gr scans every node with the given label.
With an index, it narrows the search in O(log n).

### Property index

```cypher
CREATE INDEX FOR (n:Person) ON (n.name)
```

gr creates a range index that supports equality (`= "Alice"`), range (`> 25`, `< 30`), prefix (`STARTS WITH "A"`), and `IN [...]` predicates.

### Named index

```cypher
CREATE INDEX idx_person_name FOR (n:Person) ON (n.name)
```

Providing a name makes the index easier to reference in `DROP INDEX` and `SHOW INDEXES`.

### Composite index

```cypher
CREATE INDEX FOR (n:Person) ON (n.name, n.age)
```

A composite index covers queries that filter on both columns together, or on the leading column alone.

### Relationship property index

```cypher
CREATE INDEX FOR ()-[r:KNOWS]-() ON (r.since)
```

Indexes relationship properties the same way as node properties.

### Drop an index

```cypher
DROP INDEX idx_person_name
DROP INDEX IF EXISTS idx_person_name
```

### Show indexes

```cypher
SHOW INDEXES
```

Returns: `name`, `type`, `labelsOrTypes`, `properties`, `state` (`ONLINE` or `POPULATING`).

### Indexes and MERGE

`MERGE` on an indexed property does a single index look-up instead of a full scan.
Always create an index before running `MERGE` on a property that could match many nodes.

---

## Constraints

Constraints enforce invariants at write time.
A write that violates a constraint returns `*engine.ConstraintError` and the transaction rolls back.

### Uniqueness constraint

```cypher
CREATE CONSTRAINT FOR (n:Person) REQUIRE n.email IS UNIQUE
```

Every Person node must have a distinct `email` value.
Two Persons without an `email` property are allowed (nulls are not compared for uniqueness).

gr automatically creates a supporting index for the constrained property.

### Node key constraint

A node key combines uniqueness and existence:

```cypher
CREATE CONSTRAINT FOR (n:Person) REQUIRE (n.id) IS NODE KEY
```

Every Person node must have an `id` property, and no two Person nodes can share the same `id`.

Composite node key:

```cypher
CREATE CONSTRAINT FOR (n:Person) REQUIRE (n.firstName, n.lastName) IS NODE KEY
```

### Property existence constraint

```cypher
CREATE CONSTRAINT FOR (n:Person) REQUIRE n.name IS NOT NULL
```

Every Person node must have a `name` property.

### Relationship property existence

```cypher
CREATE CONSTRAINT FOR ()-[r:KNOWS]-() REQUIRE r.since IS NOT NULL
```

Every KNOWS relationship must have a `since` property.

### Named constraint

```cypher
CREATE CONSTRAINT person_email_unique FOR (n:Person) REQUIRE n.email IS UNIQUE
```

### Drop a constraint

```cypher
DROP CONSTRAINT person_email_unique
DROP CONSTRAINT IF EXISTS person_email_unique
```

### Show constraints

```cypher
SHOW CONSTRAINTS
```

Returns: `name`, `type`, `labelsOrTypes`, `properties`.

---

## Handling constraint errors in Go

```go
import (
    "errors"
    "github.com/tamnd/gr/engine"
)

_, err = db.Exec(ctx, `CREATE (:Person {email:"a@b.com"})`, nil)
var ce *engine.ConstraintError
if errors.As(err, &ce) {
    fmt.Printf("constraint %q violated: %v\n", ce.Constraint, ce.Message)
}
```

---

## Schema catalog

List all labels, relationship types, and property keys:

```cypher
SHOW LABELS
SHOW RELATIONSHIP TYPES
SHOW PROPERTY KEYS
```

From the CLI:

```bash
gr run graph.gr "SHOW INDEXES"
gr run graph.gr "SHOW CONSTRAINTS"
gr run graph.gr "SHOW LABELS"
```

Inside the shell:

```
gr> :schema
```
