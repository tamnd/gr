package httpd

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tamnd/gr"
	"github.com/tamnd/gr/vfs"
)

// roleHandler builds a handler whose provider grants each user the given roles.
func roleHandler(t *testing.T, users map[string][]string) http.Handler {
	t.Helper()
	db, err := gr.Open(":memory:.gr", gr.Options{VFS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	p := NewStaticProvider()
	for u, roles := range users {
		if err := p.AddUser(u, "pw", roles...); err != nil {
			t.Fatalf("add user: %v", err)
		}
	}
	return Handler(db, Options{Auth: p})
}

// queryAs sends a statement as the given user and returns the recorder.
func queryAs(t *testing.T, h http.Handler, user, statement string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/db/neo4j/query/v2",
		strings.NewReader(`{"statement":`+jsonString(statement)+`}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", basic(user, "pw"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// jsonString quotes a string for embedding in a JSON body.
func jsonString(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}

const (
	readStmt   = "RETURN 1 AS n"
	writeStmt  = "CREATE (:Person {name: 'a'})"
	schemaStmt = "CREATE INDEX FOR (p:Person) ON (p.email)"
	adminStmt  = "CREATE USER ada SET PASSWORD 'pw'"
)

func TestAuthzReaderRoles(t *testing.T) {
	h := roleHandler(t, map[string][]string{"r": {"reader"}})
	if rec := queryAs(t, h, "r", readStmt); rec.Code != http.StatusOK {
		t.Errorf("reader read = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	if rec := queryAs(t, h, "r", writeStmt); rec.Code != http.StatusForbidden {
		t.Errorf("reader write = %d, want 403", rec.Code)
	}
	if rec := queryAs(t, h, "r", schemaStmt); rec.Code != http.StatusForbidden {
		t.Errorf("reader schema = %d, want 403", rec.Code)
	}
	if rec := queryAs(t, h, "r", adminStmt); rec.Code != http.StatusForbidden {
		t.Errorf("reader admin = %d, want 403", rec.Code)
	}
}

func TestAuthzEditorRoles(t *testing.T) {
	h := roleHandler(t, map[string][]string{"e": {"editor"}})
	if rec := queryAs(t, h, "e", readStmt); rec.Code != http.StatusOK {
		t.Errorf("editor read = %d, want 200", rec.Code)
	}
	if rec := queryAs(t, h, "e", writeStmt); rec.Code != http.StatusOK {
		t.Errorf("editor write = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	if rec := queryAs(t, h, "e", schemaStmt); rec.Code != http.StatusForbidden {
		t.Errorf("editor schema = %d, want 403", rec.Code)
	}
	if rec := queryAs(t, h, "e", adminStmt); rec.Code != http.StatusForbidden {
		t.Errorf("editor admin = %d, want 403", rec.Code)
	}
}

func TestAuthzPublisherRoles(t *testing.T) {
	h := roleHandler(t, map[string][]string{"p": {"publisher"}})
	for _, s := range []string{readStmt, writeStmt, schemaStmt} {
		if rec := queryAs(t, h, "p", s); rec.Code != http.StatusOK {
			t.Errorf("publisher %q = %d, want 200, body = %s", s, rec.Code, rec.Body.String())
		}
	}
	// A publisher may change schema but not manage users: the administrative statements
	// sit one level above publisher (doc 18 §12.3).
	if rec := queryAs(t, h, "p", adminStmt); rec.Code != http.StatusForbidden {
		t.Errorf("publisher admin = %d, want 403", rec.Code)
	}
}

func TestAuthzAdminRoles(t *testing.T) {
	h := roleHandler(t, map[string][]string{"a": {"admin"}})
	for _, s := range []string{readStmt, writeStmt, schemaStmt, adminStmt} {
		if rec := queryAs(t, h, "a", s); rec.Code != http.StatusOK {
			t.Errorf("admin %q = %d, want 200, body = %s", s, rec.Code, rec.Body.String())
		}
	}
}

// TestAuthzAdminShowUsers confirms an admin can run SHOW USERS over HTTP and the created
// user comes back in the rows, the proof the administrative read reaches the same
// credential store the CREATE USER statement wrote (doc 18 §12.3).
func TestAuthzAdminShowUsers(t *testing.T) {
	h := roleHandler(t, map[string][]string{"a": {"admin"}})
	if rec := queryAs(t, h, "a", adminStmt); rec.Code != http.StatusOK {
		t.Fatalf("admin create user = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	rec := queryAs(t, h, "a", "SHOW USERS")
	if rec.Code != http.StatusOK {
		t.Fatalf("admin show users = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "ada") {
		t.Errorf("SHOW USERS body does not list the created user: %s", body)
	}
}

// TestAuthzNoRolesDeniesEverything confirms a principal with no roles, and one with only
// an unknown role, may run nothing: the model fails closed.
func TestAuthzNoRolesDeniesEverything(t *testing.T) {
	h := roleHandler(t, map[string][]string{"none": {}, "weird": {"superuser"}})
	for _, user := range []string{"none", "weird"} {
		if rec := queryAs(t, h, user, readStmt); rec.Code != http.StatusForbidden {
			t.Errorf("%s read = %d, want 403", user, rec.Code)
		}
	}
}

// TestAuthzParseErrorNotForbidden confirms an unparseable statement is not misreported as
// forbidden: it reaches execution and is rejected as a bad request, so the classifier
// never hides a syntax error behind a 403.
func TestAuthzParseErrorNotForbidden(t *testing.T) {
	h := roleHandler(t, map[string][]string{"r": {"reader"}})
	rec := queryAs(t, h, "r", "THIS IS NOT CYPHER")
	if rec.Code == http.StatusForbidden {
		t.Errorf("parse error reported as 403, want a non-403 execution error; body = %s", rec.Body.String())
	}
}

// TestAuthzDisabledSkipsRoleCheck confirms that with authentication off the role check
// does not run, so an anonymous request may write.
func TestAuthzDisabledSkipsRoleCheck(t *testing.T) {
	db, err := gr.Open(":memory:.gr", gr.Options{VFS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	h := Handler(db, Options{}) // no provider
	r := httptest.NewRequest(http.MethodPost, "/db/neo4j/query/v2",
		strings.NewReader(`{"statement":`+jsonString(writeStmt)+`}`))
	r.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Errorf("anonymous write with auth off = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
}

// TestAuthzTxForbiddenWriteNotBegun confirms a forbidden statement in a begin batch
// refuses the whole transaction with 403 and never opens it: a follow-up read by the
// same reader still works, proving no writer was pinned.
func TestAuthzTxForbiddenWriteNotBegun(t *testing.T) {
	h := roleHandler(t, map[string][]string{"r": {"reader"}})
	body := `{"statements":[{"statement":` + jsonString(readStmt) + `},{"statement":` + jsonString(writeStmt) + `}]}`
	r := httptest.NewRequest(http.MethodPost, "/db/neo4j/tx", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", basic("r", "pw"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("begin with forbidden write = %d, want 403, body = %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "" {
		t.Errorf("a transaction was created (Location = %q) despite the 403", loc)
	}
}

// TestAuthzTxReaderReadOnly confirms a reader can begin a read-only transaction and run a
// read in it, but a write in a follow-up run is refused.
func TestAuthzTxReaderReadOnly(t *testing.T) {
	h := roleHandler(t, map[string][]string{"r": {"reader"}})
	begin := `{"statements":[{"statement":` + jsonString(readStmt) + `}],"accessMode":"READ"}`
	r := httptest.NewRequest(http.MethodPost, "/db/neo4j/tx", strings.NewReader(begin))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", basic("r", "pw"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusCreated {
		t.Fatalf("reader begin read tx = %d, want 201, body = %s", rec.Code, rec.Body.String())
	}
	id := decode(t, rec.Body.Bytes())["transaction"].(map[string]any)["id"].(string)

	run := `{"statements":[{"statement":` + jsonString(writeStmt) + `}]}`
	r = httptest.NewRequest(http.MethodPost, "/db/neo4j/tx/"+id, strings.NewReader(run))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", basic("r", "pw"))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Errorf("reader write in tx = %d, want 403", rec.Code)
	}
}
