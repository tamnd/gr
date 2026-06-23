---
title: "Running the server"
description: "gr serve, driver connectivity (Go/Python/JS/Java), auth, HTTP API, TLS, and metrics."
weight: 10
---

## Starting the server

```bash
gr serve graph.gr
```

By default, gr listens on:
- **Bolt:** `0.0.0.0:7687` (the Neo4j Bolt wire protocol)
- **HTTP:** `0.0.0.0:7474` (the Neo4j HTTP API)

Override the addresses:

```bash
gr serve graph.gr --bolt-addr 127.0.0.1:7687 --http-addr 127.0.0.1:7474
```

The server uses the same `.gr` file the library opens.
There is no import step; changes made through the Bolt server are immediately visible to library users in the same process (if any) and vice versa.

## Connecting with drivers

### Go (neo4j-go-driver)

```go
import "github.com/neo4j/neo4j-go-driver/v5/neo4j"

driver, err := neo4j.NewDriverWithContext("bolt://localhost:7687", neo4j.NoAuth())
if err != nil {
    log.Fatal(err)
}
defer driver.Close(ctx)

session := driver.NewSession(ctx, neo4j.SessionConfig{})
defer session.Close(ctx)

result, err := session.Run(ctx,
    "MATCH (p:Person {name:$name}) RETURN p.age AS age",
    map[string]any{"name": "Alice"},
)
if err != nil {
    log.Fatal(err)
}
for result.Next(ctx) {
    fmt.Println(result.Record().AsMap())
}
```

### Python (neo4j)

```python
from neo4j import GraphDatabase

driver = GraphDatabase.driver("bolt://localhost:7687", auth=None)
with driver.session() as session:
    result = session.run("MATCH (p:Person {name:$name}) RETURN p.age", name="Alice")
    for record in result:
        print(record["p.age"])
driver.close()
```

### JavaScript (neo4j-driver)

```javascript
import neo4j from 'neo4j-driver'

const driver = neo4j.driver('bolt://localhost:7687', neo4j.auth.none())
const session = driver.session()

const result = await session.run(
  'MATCH (p:Person {name:$name}) RETURN p.age AS age',
  { name: 'Alice' }
)
for (const record of result.records) {
  console.log(record.get('age').toNumber())
}
await session.close()
await driver.close()
```

### Java

```java
import org.neo4j.driver.*;

try (Driver driver = GraphDatabase.driver("bolt://localhost:7687", AuthTokens.none());
     Session session = driver.session()) {
    Result result = session.run(
        "MATCH (p:Person {name:$name}) RETURN p.age AS age",
        Values.parameters("name", "Alice")
    );
    while (result.hasNext()) {
        System.out.println(result.next().get("age").asInt());
    }
}
```

## Authentication

By default, `gr serve` requires no authentication (suitable for local development).
To require a password:

```bash
gr create-user admin --password s3cr3t --role admin
gr serve graph.gr
```

Then connect with basic auth:

```bash
# Go driver:
neo4j.BasicAuth("admin", "s3cr3t", "")
# Python:
GraphDatabase.driver("bolt://localhost:7687", auth=("admin", "s3cr3t"))
```

Roles: `reader`, `editor`, `publisher`, `admin`.
See `gr --help` for `create-user`, `grant-role`, `revoke-role`, and `drop-user`.

## The HTTP JSON API

gr exposes a subset of the Neo4j HTTP Transactional API on the HTTP port.

Start a transaction:

```
POST /db/gr/tx
Content-Type: application/json

{"statements": [{"statement": "MATCH (n:Person) RETURN n.name"}]}
```

Run more statements in the same transaction:

```
POST /db/gr/tx/{id}
{"statements": [...]}
```

Commit:

```
POST /db/gr/tx/{id}/commit
```

Rollback:

```
DELETE /db/gr/tx/{id}
```

Health check:

```
GET /health
```

Returns `200 OK` with `{"status":"ok"}` when the server is ready.

## TLS

Provide a certificate and key:

```bash
gr serve graph.gr --tls-cert server.crt --tls-key server.key
```

Drivers use `bolt+s://localhost:7687` for single-hop TLS or `bolt+ssc://localhost:7687` for self-signed certificates.

## Observability

Prometheus metrics on a separate port:

```bash
gr serve graph.gr --metrics-addr 0.0.0.0:9090
```

Metrics include: active connections, queries per second, query latency percentiles, transaction rate, buffer pool hit rate, WAL write rate.

Slow query log:

```bash
gr serve graph.gr --query-log /var/log/gr-queries.log --slow-query-threshold 100ms
```

Queries that take longer than the threshold are appended to the log as JSON lines.
