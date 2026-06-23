---
title: "Graph objects"
description: "Node, Relationship, Path, ElementId, and the value model type switch."
weight: 40
---

When a Cypher query returns a node, relationship, or path, gr represents it as a Go struct.

## gr.Node

```go
var node gr.Node = rec.Get("n").(gr.Node)

id     := node.ElementId()   // string — stable identifier
labels := node.Labels()      // []string — e.g. ["Person", "Admin"]
props  := node.Props()       // map[string]any — all properties

name, ok := node.GetProp("name") // any, bool
```

`ElementId()` is a stable, opaque string identifier for the node.
It is stable within a database file: the same node always gets the same ElementId.
Use it to refer to the node in later queries.

`Labels()` returns the labels in sorted order.

`Props()` returns a snapshot of all properties at query time.

## gr.Relationship

```go
var rel gr.Relationship = rec.Get("r").(gr.Relationship)

id         := rel.ElementId()        // string
relType    := rel.Type()             // string — e.g. "KNOWS"
startId    := rel.StartElementId()   // string — ElementId of the start node
endId      := rel.EndElementId()     // string — ElementId of the end node
props      := rel.Props()            // map[string]any
since, ok  := rel.GetProp("since")   // any, bool
```

The direction of a relationship is fixed at creation time.
`StartElementId()` is always the node the relationship points from.

## gr.Path

```go
var path gr.Path = rec.Get("p").(gr.Path)

nodes := path.Nodes()         // []gr.Node
rels  := path.Relationships() // []gr.Relationship
hops  := path.Length()        // int — number of relationships
```

`Nodes()` returns nodes in order from start to end.
`Relationships()` returns relationships in traversal order.
`len(nodes) == len(rels) + 1` always holds for a non-empty path.

## ElementId

`ElementId` is the primary handle for identifying a specific node or relationship.
It is a string, opaque to callers.
Store it and use it in subsequent queries:

```go
// First query: find the node and remember its ID.
res, _ := db.Query(ctx, `MATCH (p:Person {name:$name}) RETURN p`, map[string]any{"name": "Alice"})
res.Next()
node := res.Record().Get("p").(gr.Node)
aliceId := node.ElementId()
res.Close()

// Later query: use the ID directly.
res2, _ := db.Query(ctx, `
    MATCH (p) WHERE elementId(p) = $id
    RETURN p.name, p.age
`, map[string]any{"id": aliceId})
```

The Cypher function `elementId(n)` returns the element's ID as a string.

## Immutability

`gr.Node`, `gr.Relationship`, and `gr.Path` are value types.
They are safe to read from multiple goroutines.
They are snapshots: mutations to the database after the query completes do not affect the returned objects.

## The complete type switch

When you do not know the concrete type of a value at compile time:

```go
func printValue(val any) {
    switch v := val.(type) {
    case nil:
        fmt.Println("null")
    case bool:
        fmt.Printf("bool: %v\n", v)
    case int64:
        fmt.Printf("int: %d\n", v)
    case float64:
        fmt.Printf("float: %f\n", v)
    case string:
        fmt.Printf("string: %q\n", v)
    case []byte:
        fmt.Printf("bytes: %d bytes\n", len(v))
    case []any:
        fmt.Printf("list of %d elements\n", len(v))
    case map[string]any:
        fmt.Printf("map with %d keys\n", len(v))
    case gr.Node:
        fmt.Printf("node %s labels=%v\n", v.ElementId(), v.Labels())
    case gr.Relationship:
        fmt.Printf("rel %s type=%s\n", v.ElementId(), v.Type())
    case gr.Path:
        fmt.Printf("path length=%d\n", v.Length())
    default:
        fmt.Printf("unknown type: %T\n", v)
    }
}
```

## Passing graph objects as parameters

You cannot pass a `gr.Node` or `gr.Relationship` directly as a Cypher parameter.
Extract the `ElementId()` and pass that:

```go
// Wrong — will error at the API layer.
db.Exec(ctx, `SET $node.age = 31`, map[string]any{"node": someNode})

// Right — use the element ID and match in Cypher.
db.Exec(ctx, `
    MATCH (p) WHERE elementId(p) = $id
    SET p.age = $age
`, map[string]any{"id": someNode.ElementId(), "age": int64(31)})
```
