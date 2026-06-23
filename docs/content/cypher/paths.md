---
title: "Paths and graph traversal"
description: "Variable-length patterns, shortestPath, allShortestPaths, and named paths."
weight: 40
---

## Variable-length relationships

Append `*` to a relationship pattern to match paths of variable length:

```cypher
MATCH (a:Person {name:"Alice"})-[:KNOWS*]->(b:Person)
RETURN b.name
```

This matches all Person nodes reachable from Alice by following one or more KNOWS relationships, at any depth.

Bound the depth to avoid traversing the whole graph:

```cypher
MATCH (a:Person {name:"Alice"})-[:KNOWS*1..3]->(b:Person)
RETURN b.name
```

`*1..3` means at least one hop and at most three hops.

Fixed depth:

```cypher
MATCH (a:Person)-[:KNOWS*2]->(b:Person)
RETURN a.name, b.name
```

Minimum only (at least two hops, no upper bound):

```cypher
MATCH (a:Person)-[:KNOWS*2..]->(b:Person)
RETURN a.name, b.name
```

## Relationship-uniqueness

gr follows the standard openCypher rule: within a single path, each relationship instance appears at most once.
This prevents infinite loops in cyclic graphs.
The same relationship can appear in different rows if different paths lead to it.

## Named paths

Bind an entire matched path to a variable:

```cypher
MATCH p = (a:Person {name:"Alice"})-[:KNOWS*1..3]->(b:Person)
RETURN p
```

Extract components from a named path:

```cypher
MATCH p = (a:Person {name:"Alice"})-[:KNOWS*]->(b:Person)
RETURN nodes(p) AS nodeList, relationships(p) AS relList, length(p) AS hops
```

| Function | Description |
|---|---|
| `nodes(path)` | List of nodes in the path |
| `relationships(path)` | List of relationships in the path |
| `length(path)` | Number of relationships in the path |

## shortestPath

`shortestPath` finds a single shortest path between two nodes.
It uses a BFS internally and returns the first path it finds.

```cypher
MATCH
  (a:Person {name:"Alice"}),
  (b:Person {name:"Bob"}),
  p = shortestPath((a)-[:KNOWS*]-(b))
RETURN p, length(p)
```

The pattern inside `shortestPath` must be a variable-length pattern.
Direction is optional: `(a)-[:KNOWS*]-(b)` is undirected.

`shortestPath` returns `null` if no path exists.
Use `OPTIONAL MATCH` to handle that:

```cypher
MATCH (a:Person {name:"Alice"}), (b:Person {name:"Bob"})
OPTIONAL MATCH p = shortestPath((a)-[:KNOWS*]-(b))
RETURN CASE WHEN p IS NULL THEN "unreachable" ELSE toString(length(p)) END AS dist
```

## allShortestPaths

`allShortestPaths` returns every path of minimum length between two nodes:

```cypher
MATCH
  (a:Person {name:"Alice"}),
  (b:Person {name:"Bob"}),
  p = allShortestPaths((a)-[:KNOWS*]-(b))
RETURN p
```

This can return many paths, so bind the depth or use `LIMIT`.

## Performance

Variable-length queries can be slow on dense graphs.
A few practices keep them fast:

- Always bound the depth for ad-hoc queries: `*1..5` not `*`.
- Put an index on the start node's identifying property. The planner uses it to anchor the BFS.
- Use `shortestPath` instead of collecting all paths when you only need to know reachability.
- Use `WITH ... LIMIT` to cut the result set early when exploring large neighbourhoods.

## Running a path query in Go

```go
res, err := db.Query(ctx, `
    MATCH (a:Person {name:$start}), (b:Person {name:$end}),
          p = shortestPath((a)-[:KNOWS*]-(b))
    RETURN length(p) AS dist
`, map[string]any{"start": "Alice", "end": "Carol"})
if err != nil {
    return err
}
defer res.Close()
if res.Next() {
    dist, _ := res.Record().Get("dist")
    fmt.Printf("distance: %v hops\n", dist)
}
return res.Err()
```
