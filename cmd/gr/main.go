// Command gr is the command-line entry point for the gr graph database. At M0 it
// is intentionally minimal — it can report its version and create or open a .gr
// file to prove the lifecycle end-to-end. The interactive Cypher shell and the
// server land in M5 (spec 2060 doc 25 §7).
package main

import (
	"fmt"
	"os"

	"github.com/tamnd/gr"
)

const version = "0.0.0-m0"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "version", "-v", "--version":
		fmt.Println("gr", version)
	case "open", "create":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: gr open <file.gr>")
			os.Exit(2)
		}
		if err := openClose(os.Args[2]); err != nil {
			fmt.Fprintln(os.Stderr, "gr:", err)
			os.Exit(1)
		}
	default:
		usage()
		os.Exit(2)
	}
}

func openClose(path string) error {
	db, err := gr.Open(path, gr.Options{})
	if err != nil {
		return err
	}
	fmt.Printf("opened %s (page size %d)\n", db.Path(), db.PageSize())
	return db.Close()
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: gr <version|open <file.gr>>")
}
