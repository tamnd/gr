package main

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/tamnd/gr"
	"github.com/tamnd/gr/httpd"
	"github.com/tamnd/gr/vfs"
)

// defaultServeAddr is the address gr serve binds when none is given (doc 18 §9.7).
// 7474 is the Neo4j HTTP port, so a tool pointed at the usual port finds the server.
const defaultServeAddr = ":7474"

// runServe implements the `gr serve` subcommand (doc 18 §9): it opens a database and
// serves the HTTP/JSON API over it until the process is stopped. It parses its own
// flag set rather than the shell's, since the serve options (the listen address, the
// database name in the URL path) do not overlap the shell's.
//
// listen is injected so a test can substitute a stub for net/http's ListenAndServe;
// the real entry point passes http.ListenAndServe.
func runServe(args []string, stdout, stderr io.Writer, listen func(addr string, h http.Handler) error) int {
	fs := flag.NewFlagSet("gr serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", defaultServeAddr, "address to listen on")
	name := fs.String("name", "neo4j", "database name in the URL path")
	readonly := fs.Bool("readonly", false, "open the database read-only")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: gr serve [flags] [database]")
		fmt.Fprintln(stderr)
		fmt.Fprintln(stderr, "Serve the HTTP/JSON API over a database. With no database")
		fmt.Fprintln(stderr, "argument gr serves a transient in-memory database.")
		fmt.Fprintln(stderr)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	path := fs.Arg(0)
	db, err := openServeDB(path, *readonly)
	if err != nil {
		fmt.Fprintln(stderr, "gr:", err)
		return exitOpen
	}
	defer func() { _ = db.Close() }()

	h := httpd.Handler(db, httpd.Options{Name: *name})
	fmt.Fprintf(stderr, "gr serving %s on %s (database %q)\n", describeDB(path), *addr, *name)
	if err := listen(*addr, h); err != nil {
		fmt.Fprintln(stderr, "gr:", err)
		return exitIO
	}
	return exitOK
}

// openServeDB opens the database serve will host. An empty or :memory: path opens a
// transient in-memory database over the in-memory VFS, matching the shell's rule.
func openServeDB(path string, readonly bool) (*gr.DB, error) {
	if path == "" || path == ":memory:" {
		return gr.Open(memPath, gr.Options{VFS: vfs.NewMem()})
	}
	if _, err := os.Stat(path); err != nil && readonly {
		return nil, fmt.Errorf("cannot open a read-only database that does not exist: %s", path)
	}
	return gr.Open(path, gr.Options{ReadOnly: readonly})
}

// describeDB names the database for the startup banner.
func describeDB(path string) string {
	if path == "" || path == ":memory:" {
		return "an in-memory database"
	}
	return path
}
