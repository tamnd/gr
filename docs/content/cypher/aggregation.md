---
title: "Aggregation and expressions"
description: "count, sum, avg, collect, list operations, string and math functions, CASE, and null handling."
weight: 30
---

## Aggregation functions

Aggregation works like SQL's `GROUP BY`, but implicit: every column in `RETURN` that is not wrapped in an aggregation function becomes a group key.

```cypher
MATCH (p:Person)-[:KNOWS]->(friend:Person)
RETURN p.name, count(friend) AS friendCount
ORDER BY friendCount DESC
```

`p.name` is the group key.
`count(friend)` counts the rows per group.

| Function | Description |
|---|---|
| `count(*)` | Count all rows in the group |
| `count(expr)` | Count non-null values of `expr` |
| `sum(expr)` | Sum numeric values |
| `avg(expr)` | Average numeric values |
| `min(expr)` | Minimum value |
| `max(expr)` | Maximum value |
| `collect(expr)` | Collect values into a list |

`DISTINCT` inside an aggregation deduplicates before aggregating:

```cypher
MATCH (p:Person)-[:KNOWS]->(friend:Person)
RETURN count(DISTINCT friend.name) AS uniqueFriends
```

## collect

`collect` builds a list of values:

```cypher
MATCH (p:Person)-[:KNOWS]->(friend:Person)
RETURN p.name, collect(friend.name) AS friends
```

Sort the collected list:

```cypher
MATCH (p:Person)-[:KNOWS]->(friend:Person)
WITH p, friend
ORDER BY friend.name
RETURN p.name, collect(friend.name) AS friends
```

## List operations

Create list literals: `[1, 2, 3]`, `["a", "b", "c"]`.

`size(list)` returns the number of elements.
`head(list)` returns the first element.
`tail(list)` returns all but the first element.
`last(list)` returns the last element.
`reverse(list)` reverses a list.
`range(start, end)` generates an integer list from `start` to `end` (inclusive).
`range(start, end, step)` with a step.

List comprehension — filter and transform:

```cypher
WITH [1,2,3,4,5] AS nums
RETURN [x IN nums WHERE x > 2 | x * 10] AS result
```

`IN` tests membership: `3 IN [1,2,3]` is `true`.

## String functions

| Function | Description |
|---|---|
| `toUpper(s)` | Convert to upper case |
| `toLower(s)` | Convert to lower case |
| `trim(s)` | Remove leading and trailing whitespace |
| `ltrim(s)` | Remove leading whitespace |
| `rtrim(s)` | Remove trailing whitespace |
| `replace(s, find, replace)` | Replace occurrences of `find` with `replace` |
| `split(s, delimiter)` | Split into a list of strings |
| `substring(s, start)` | Substring from `start` |
| `substring(s, start, length)` | Substring from `start` with `length` |
| `left(s, n)` | First `n` characters |
| `right(s, n)` | Last `n` characters |
| `size(s)` | Length in characters |

## Math functions

| Function | Description |
|---|---|
| `abs(n)` | Absolute value |
| `ceil(n)` | Round up to integer |
| `floor(n)` | Round down to integer |
| `round(n)` | Round to nearest integer |
| `sqrt(n)` | Square root |
| `sign(n)` | Sign: -1, 0, or 1 |
| `log(n)` | Natural logarithm |
| `log10(n)` | Base-10 logarithm |
| `exp(n)` | Euler's number raised to `n` |
| `sin(n)`, `cos(n)`, `tan(n)` | Trigonometric functions |

## Type coercions

| Function | Description |
|---|---|
| `toInteger(x)` | Convert to integer (truncates floats) |
| `toFloat(x)` | Convert to float |
| `toString(x)` | Convert to string |
| `toBoolean(x)` | Convert string to boolean |

## CASE expressions

Simple CASE — test a value against a list of alternatives:

```cypher
MATCH (p:Person)
RETURN p.name,
  CASE p.age
    WHEN 30 THEN "thirty"
    WHEN 25 THEN "twenty-five"
    ELSE "other"
  END AS ageLabel
```

General CASE — arbitrary boolean conditions:

```cypher
MATCH (p:Person)
RETURN p.name,
  CASE
    WHEN p.age < 18 THEN "minor"
    WHEN p.age < 65 THEN "adult"
    ELSE "senior"
  END AS category
```

## Null handling

Any operation on `null` produces `null`.
`null = null` is `null`, not `true`.
Use `IS NULL` and `IS NOT NULL` to test for null.

`coalesce(a, b, c)` returns the first non-null argument:

```cypher
MATCH (p:Person)
RETURN p.name, coalesce(p.email, "no email") AS contact
```

`nullIf(a, b)` returns `null` if `a = b`, otherwise `a`.

In aggregations, `null` values are ignored.
`count(*)` counts all rows including those with nulls.
`count(expr)` counts only non-null values of `expr`.
