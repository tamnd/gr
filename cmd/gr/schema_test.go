package main

import (
	"strings"
	"testing"
)

func TestShellLabelsAndTypes(t *testing.T) {
	script := strings.Join([]string{
		"CREATE (:Person)-[:KNOWS]->(:Company);",
		".labels",
	}, "\n") + "\n"
	out, errb, code := runCLI(t, []string{}, script)
	if code != exitOK {
		t.Fatalf("code = %d; stderr=%q", code, errb)
	}
	// .labels sorts, so Company precedes Person.
	if !strings.Contains(out, "Company") || !strings.Contains(out, "Person") {
		t.Fatalf("labels out = %q", out)
	}
	if strings.Index(out, "Company") > strings.Index(out, "Person") {
		t.Fatalf("labels not sorted: %q", out)
	}
}

func TestShellTypes(t *testing.T) {
	script := "CREATE (:Person)-[:KNOWS]->(:Person);\n.types\n"
	out, _, code := runCLI(t, []string{}, script)
	if code != exitOK {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(out, "KNOWS") {
		t.Fatalf("types out = %q", out)
	}
}

func TestShellIndexes(t *testing.T) {
	script := strings.Join([]string{
		"CREATE INDEX person_name FOR (p:Person) ON (p.name);",
		".indexes",
	}, "\n") + "\n"
	out, errb, code := runCLI(t, []string{}, script)
	if code != exitOK {
		t.Fatalf("code = %d; stderr=%q", code, errb)
	}
	if !strings.Contains(out, "person_name on :Person(name)") {
		t.Fatalf("indexes out = %q", out)
	}
}

func TestShellSchema(t *testing.T) {
	script := strings.Join([]string{
		"CREATE (:Person {name: 'Ada'})-[:KNOWS]->(:Person);",
		"CREATE INDEX person_name FOR (p:Person) ON (p.name);",
		".schema",
	}, "\n") + "\n"
	out, errb, code := runCLI(t, []string{}, script)
	if code != exitOK {
		t.Fatalf("code = %d; out=%q", code, out)
	}
	// .schema writes its descriptive sections to the chatter channel.
	for _, want := range []string{"Labels: Person", "Relationship types: KNOWS", "Property keys: name", "person_name on :Person(name)"} {
		if !strings.Contains(errb, want) {
			t.Fatalf("schema chatter %q missing %q", errb, want)
		}
	}
}

func TestShellSchemaEmpty(t *testing.T) {
	_, errb, code := runCLI(t, []string{}, ".schema\n")
	if code != exitOK {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(errb, "Labels: (none)") {
		t.Fatalf("empty schema = %q", errb)
	}
}
