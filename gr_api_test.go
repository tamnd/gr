package gr_test

import (
	"crypto/sha256"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestAPIStabilityGuard is the 1.0 API surface guard (doc 25 §11.4, doc 16 §24.1).
// It parses the top-level Go source files of the gr package, collects every
// exported name (func/type/var/const), hashes the sorted list, and fails if the
// digest does not match the committed value.
//
// Adding a new export is additive (a minor version, allowed): the digest changes
// and the committed constant must be updated deliberately here.
// Removing or renaming an export is a breaking change: it requires a major-version
// bump and the test makes that impossible to ship silently (doc 16 §24.1).
func TestAPIStabilityGuard(t *testing.T) {
	// committedDigest is the SHA-256 of the sorted exported names as of the 1.0 freeze.
	// Update it only when adding a new export (minor version, additive); never update
	// it for a removal or rename without bumping the major version (v2).
	const committedDigest = "a138467af49d96650321ff7a4ca11178ef9acb9fc32a9f1f8ee46c631f39b17b"

	names := exportedNames(t, ".")
	joined := strings.Join(names, "\n")
	sum := sha256.Sum256([]byte(joined))
	got := fmt.Sprintf("%x", sum)

	if committedDigest == "PLACEHOLDER" {
		t.Logf("API surface (%d names):\n%s", len(names), joined)
		t.Logf("Set committedDigest to: %s", got)
		t.Skip("committedDigest not yet set — run once to capture the baseline")
	}
	if got != committedDigest {
		t.Logf("current surface (%d names):\n%s", len(names), joined)
		t.Errorf("API surface digest mismatch\ngot:  %s\nwant: %s\n\n"+
			"If you ADDED an export (allowed): update committedDigest to %s.\n"+
			"If you REMOVED or RENAMED an export: revert — that is a breaking change\n"+
			"requiring a major-version bump (doc 16 §24.1).",
			got, committedDigest, got)
	}
}

// exportedNames parses all non-test .go files in dir and returns the sorted list
// of exported top-level declaration names.
func exportedNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}

	fset := token.NewFileSet()
	var names []string
	seen := make(map[string]bool)

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		// Skip test files; they are not part of the public surface.
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Recv == nil && d.Name.IsExported() && !seen[d.Name.Name] {
					names = append(names, "func "+d.Name.Name)
					seen[d.Name.Name] = true
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						if s.Name.IsExported() && !seen[s.Name.Name] {
							names = append(names, "type "+s.Name.Name)
							seen[s.Name.Name] = true
						}
					case *ast.ValueSpec:
						for _, id := range s.Names {
							if id.IsExported() && !seen[id.Name] {
								var kind string
								if d.Tok == token.CONST {
									kind = "const"
								} else {
									kind = "var"
								}
								names = append(names, kind+" "+id.Name)
								seen[id.Name] = true
							}
						}
					}
				}
			}
		}
	}
	sort.Strings(names)
	return names
}
