# gr

`gr` is an embedded, single-file, labeled-property-graph database for Go that speaks Cypher.
Pure Go, no cgo, one `.gr` file.
The SQLite feel, but for graphs.

```go
db, err := gr.Open("friends.gr", gr.Options{})
if err != nil {
    log.Fatal(err)
}
defer db.Close()

ctx := context.Background()

db.Exec(ctx, `
    CREATE (:Person {name:"Alice", age:30})-[:KNOWS {since:2022}]->(:Person {name:"Bob", age:25})
`, nil)

res, _ := db.Query(ctx, `
    MATCH (a:Person)-[:KNOWS]->(b:Person)
    RETURN a.name AS from, b.name AS to
`, nil)
defer res.Close()
for res.Next() {
    rec := res.Record()
    fmt.Printf("%v -> %v\n", rec.Get("from"), rec.Get("to"))
}
```

No daemon. No connection string. One file.

## Features

- **openCypher** — MATCH, CREATE, MERGE, SET, DELETE, REMOVE, variable-length paths, shortestPath, aggregation, indexes, constraints.
- **Pure Go** — no cgo. Cross-compiles to every target Go supports. One static binary.
- **Single file** — the graph lives in one `.gr` file. Back it up, move it, email it.
- **WAL durability** — write-ahead journaling, per-page checksums, crash recovery. A file the process crashes into opens clean.
- **CLI** — `gr shell`, `gr run`, `gr import`, `gr backup`, `gr info`, `gr check`. Everything you need from the terminal.
- **Bolt server** — `gr serve` exposes the graph over Bolt v4.4/v5.0. Connect from Go, Python, JavaScript, Java, .NET, or Rust using any Neo4j driver.
- **Stable** — 148 exported names, 51 configuration knobs, and the file format are all frozen at 1.0 and guarded by CI digest tests.

## Installation

**Go:**

```bash
go get github.com/tamnd/gr@latest          # library
go install github.com/tamnd/gr/cmd/gr@latest  # CLI
```

**Homebrew (macOS/Linux):**

```bash
brew install tamnd/tap/gr
```

**Scoop (Windows):**

```powershell
scoop bucket add tamnd https://github.com/tamnd/scoop-bucket
scoop install gr
```

**Linux packages (apt/dnf):**

```bash
# Debian/Ubuntu
curl -fsSL https://tamnd.github.io/linux-repo/gpg.key | sudo gpg --dearmor -o /usr/share/keyrings/tamnd.gpg
echo "deb [signed-by=/usr/share/keyrings/tamnd.gpg] https://tamnd.github.io/linux-repo stable main" \
  | sudo tee /etc/apt/sources.list.d/tamnd.list
sudo apt update && sudo apt install gr

# Fedora/RHEL
sudo dnf config-manager --add-repo https://tamnd.github.io/linux-repo/tamnd.repo
sudo dnf install gr
```

**Container:**

```bash
docker run --rm -v "$PWD:/data" ghcr.io/tamnd/gr shell /data/graph.gr
```

**Release archives:** download `.tar.gz`, `.zip`, `.deb`, `.rpm`, or `.apk` from the [releases page](https://github.com/tamnd/gr/releases).

## Usage

### Library

```go
import "github.com/tamnd/gr"

db, err := gr.Open("social.gr", gr.Options{})
if err != nil {
    log.Fatal(err)
}
defer db.Close()

ctx := context.Background()

// Create an index.
db.Exec(ctx, `CREATE INDEX FOR (p:Person) ON (p.name)`, nil)

// Write.
db.Exec(ctx, `
    CREATE (:Person {name:$name, age:$age})
`, map[string]any{"name": "Alice", "age": 30})

// Read.
res, _ := db.Query(ctx, `
    MATCH (p:Person) WHERE p.age > $min RETURN p.name, p.age ORDER BY p.age
`, map[string]any{"min": 20})
defer res.Close()
for res.Next() {
    rec := res.Record()
    fmt.Printf("%v (age %v)\n", rec.Get("p.name"), rec.Get("p.age"))
}

// Explicit transaction.
tx, _ := db.BeginTx(ctx, gr.TxOptions{})
tx.Exec(ctx, `MERGE (p:Person {name:"Bob"}) ON CREATE SET p.age = 25`, nil)
tx.Commit()

// Managed write with automatic conflict retry.
db.ExecuteWrite(ctx, func(tx gr.ManagedTx) (any, error) {
    return tx.Exec(ctx, `MATCH (p:Person {name:"Alice"}) SET p.loginCount = p.loginCount + 1`, nil)
})
```

### CLI

```bash
# Interactive shell.
gr shell social.gr

# One-shot query.
gr run social.gr "MATCH (n:Person) RETURN n.name, n.age ORDER BY n.name"

# JSON output.
gr run --format json social.gr "MATCH (n:Person) RETURN n.name"

# Bulk load from CSV.
gr import social.gr --nodes Person=people.csv --rels KNOWS=knows.csv

# File stats.
gr info social.gr

# Hot backup.
gr backup social.gr backup.gr
```

### Bolt server

```bash
gr serve social.gr
```

Connect with any Neo4j driver:

```python
from neo4j import GraphDatabase
driver = GraphDatabase.driver("bolt://localhost:7687", auth=None)
with driver.session() as s:
    result = s.run("MATCH (p:Person) RETURN p.name")
    for r in result:
        print(r["p.name"])
```

## Building from source

```bash
git clone https://github.com/tamnd/gr
cd gr
go build -o gr ./cmd/gr

# Full test suite including API stability, config-freeze, and conformance gates.
go test ./...
CGO_ENABLED=1 go test -race ./...
```

## Documentation

Full docs at **[gr.tamnd.com](https://gr.tamnd.com)**.

## License

Apache-2.0. See [LICENSE](LICENSE).
