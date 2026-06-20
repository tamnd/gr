# gr

`gr` is an embedded, single-file, labeled-property-graph database for Go that speaks Cypher.
It is pure Go with no cgo, it stores a whole graph in one self-describing `.gr` file with optional `-wal` and `-shm` sidecars, and it gives the SQLite "open a file, get a database" feel for graph data.

A query is a function call, not a network round trip.
There is no server to run, no port to open, and no cluster to keep healthy until and unless you choose to add one with `gr serve`.

## Status

`gr` is under construction, built milestone by milestone against [spec 2060](../../../notes/Spec/2060).
This is **M0**: the foundation pour.

What works today:

- Open and close a `.gr` file through `gr.Open` and `db.Close`, creating it with a fresh, validated header if it does not exist.
- The on-disk file format: a checksummed file header, fixed-size pages with per-page headers and checksums, the section directory, and the primitive value codec.
- A pager and buffer pool with pin and unpin, clock eviction that never evicts a pinned page, and per-page checksum validation.
- A write-ahead log in the SQLite-WAL lineage: full-page-image frames with a chained checksum, atomic group commit, and crash recovery that honors the durable-prefix property.
- A virtual filesystem seam with a real OS backend and an in-memory, fault-injecting backend that can crash the database at any write or fsync boundary and can tear writes.
- Deterministic clock and PRNG hooks so crash tests replay identically.
- The storage-engine SPI declared as a Go interface, backed by an in-memory stub, so the query stack can be built against it in later milestones.

What does not work yet: any graph storage, any Cypher, any query.
Those arrive in M1 and beyond.

The headline M0 gate is the substrate crash campaign: the fault-injecting filesystem crashes the database at every write and fsync boundary of a transactional page workload, and after every injected crash the reopened file recovers to exactly one committed prefix, never a torn or partial state.
See `gr_test.go`.

## Usage

```go
package main

import (
	"log"

	"github.com/tamnd/gr"
)

func main() {
	db, err := gr.Open("graph.gr", gr.Options{})
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	// Queries arrive in M2.
}
```

The command-line tool can report its version and open or create a file:

```
go run ./cmd/gr version
go run ./cmd/gr open graph.gr
```

## Layout

| Package | Role |
|---------|------|
| `gr` (root) | The library entry point: `Open`, `Close`, the database handle |
| `value` | The labeled-property-graph value type system |
| `format` | The on-disk file format: header, pages, section directory, codecs |
| `vfs` | The virtual filesystem seam: OS backend and fault-injecting in-memory backend |
| `wal` | The write-ahead log and crash recovery |
| `pager` | The pager and buffer pool over the format and the WAL |
| `engine` | The storage-engine SPI and its M0 in-memory stub |
| `determ` | Deterministic clock and PRNG hooks for reproducible tests |
| `cmd/gr` | The command-line entry point |

## Building

```
go build ./...
go test ./...
go test -race ./...
```

`gr` builds with `CGO_ENABLED=0` everywhere.

## License

Apache License 2.0. See [LICENSE](LICENSE).
