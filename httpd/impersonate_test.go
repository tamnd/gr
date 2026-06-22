package httpd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tamnd/gr"
	"github.com/tamnd/gr/vfs"
)

// impHandler builds a handler whose static provider grants each user the given roles, with
// impersonation enabled, for the impersonation tests.
func impHandler(t *testing.T, users map[string][]string) http.Handler {
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
	return Handler(db, Options{Auth: p, Impersonation: true})
}

// queryAsImp sends a statement as actor while impersonating impUser and returns the recorder.
func queryAsImp(t *testing.T, h http.Handler, actor, impUser, statement string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"statement":` + jsonString(statement) + `,"impersonatedUser":` + jsonString(impUser) + `}`
	r := httptest.NewRequest(http.MethodPost, "/db/neo4j/query/v2", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", basic(actor, "pw"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// TestImpersonationUsesTargetRoles confirms an impersonated statement is authorized with
// the impersonated user's roles, not the actor's: an admin acting as a reader may read but
// not write (doc 18 §10.5, §10.6).
func TestImpersonationUsesTargetRoles(t *testing.T) {
	h := impHandler(t, map[string][]string{"a": {"admin"}, "r": {"reader"}})
	if rec := queryAsImp(t, h, "a", "r", readStmt); rec.Code != http.StatusOK {
		t.Errorf("admin as reader, read = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	if rec := queryAsImp(t, h, "a", "r", writeStmt); rec.Code != http.StatusForbidden {
		t.Errorf("admin as reader, write = %d, want 403", rec.Code)
	}
}

// TestImpersonationEditorWrites confirms an admin acting as an editor may write, the proof
// the impersonated roles are applied positively, not just as a restriction.
func TestImpersonationEditorWrites(t *testing.T) {
	h := impHandler(t, map[string][]string{"a": {"admin"}, "e": {"editor"}})
	if rec := queryAsImp(t, h, "a", "e", writeStmt); rec.Code != http.StatusOK {
		t.Errorf("admin as editor, write = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
}

// TestImpersonationRequiresAdmin confirms a non-admin may not impersonate: an editor trying
// to act as a reader is forbidden, even when the target's roles are weaker (doc 18 §10.5).
func TestImpersonationRequiresAdmin(t *testing.T) {
	h := impHandler(t, map[string][]string{"e": {"editor"}, "r": {"reader"}})
	if rec := queryAsImp(t, h, "e", "r", readStmt); rec.Code != http.StatusForbidden {
		t.Errorf("editor impersonating = %d, want 403", rec.Code)
	}
}

// TestImpersonationUnknownTarget confirms impersonating a user the provider does not know
// is forbidden, non-disclosing about whether the user exists (doc 18 §10.5).
func TestImpersonationUnknownTarget(t *testing.T) {
	h := impHandler(t, map[string][]string{"a": {"admin"}})
	if rec := queryAsImp(t, h, "a", "ghost", readStmt); rec.Code != http.StatusForbidden {
		t.Errorf("impersonate unknown = %d, want 403", rec.Code)
	}
}

// TestImpersonationDisabled confirms that with impersonation off the impersonatedUser field
// is refused even for an admin, so the feature is strictly opt-in (doc 18 §10.5).
func TestImpersonationDisabled(t *testing.T) {
	db, err := gr.Open(":memory:.gr", gr.Options{VFS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	p := NewStaticProvider()
	if err := p.AddUser("a", "pw", "admin"); err != nil {
		t.Fatal(err)
	}
	if err := p.AddUser("r", "pw", "reader"); err != nil {
		t.Fatal(err)
	}
	h := Handler(db, Options{Auth: p}) // impersonation off
	if rec := queryAsImp(t, h, "a", "r", readStmt); rec.Code != http.StatusForbidden {
		t.Errorf("impersonation off = %d, want 403", rec.Code)
	}
}

// nonResolverProvider authenticates a single fixed admin but does not implement
// RoleResolver, so it cannot be a target of impersonation.
type nonResolverProvider struct{}

func (nonResolverProvider) Authenticate(ctx context.Context, scheme, principal string, credential []byte) (*Principal, error) {
	if scheme == "basic" && principal == "a" && string(credential) == "pw" {
		return &Principal{Name: "a", Roles: []string{"admin"}}, nil
	}
	return nil, ErrUnauthorized
}
func (nonResolverProvider) Schemes() []string { return []string{"basic"} }

// TestImpersonationProviderCannotResolve confirms impersonation is refused when the
// configured provider does not implement RoleResolver, since there is no way to learn the
// target's roles (doc 18 §10.5).
func TestImpersonationProviderCannotResolve(t *testing.T) {
	db, err := gr.Open(":memory:.gr", gr.Options{VFS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	h := Handler(db, Options{Auth: nonResolverProvider{}, Impersonation: true})
	if rec := queryAsImp(t, h, "a", "anyone", readStmt); rec.Code != http.StatusForbidden {
		t.Errorf("non-resolver provider impersonation = %d, want 403", rec.Code)
	}
}

// TestImpersonationNoFieldUnaffected confirms a plain request with no impersonatedUser is
// authorized as the actor, so enabling impersonation does not change ordinary requests.
func TestImpersonationNoFieldUnaffected(t *testing.T) {
	h := impHandler(t, map[string][]string{"a": {"admin"}})
	if rec := queryAsImp(t, h, "a", "", writeStmt); rec.Code != http.StatusOK {
		t.Errorf("admin plain write = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
}

// TestImpersonationTxOwnedByActor confirms a transaction begun under impersonation is owned
// by the authenticating admin, so the admin can run further statements and roll it back
// (the body-less rollback carries no impersonatedUser field, so ownership must follow the
// actor, not the impersonated user; doc 18 §9.9, §10.5).
func TestImpersonationTxOwnedByActor(t *testing.T) {
	h := impHandler(t, map[string][]string{"a": {"admin"}, "e": {"editor"}})

	begin := `{"statements":[{"statement":` + jsonString(writeStmt) + `}],"impersonatedUser":"e"}`
	r := httptest.NewRequest(http.MethodPost, "/db/neo4j/tx", strings.NewReader(begin))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", basic("a", "pw"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusCreated {
		t.Fatalf("admin-as-editor begin write tx = %d, want 201, body = %s", rec.Code, rec.Body.String())
	}
	id := decode(t, rec.Body.Bytes())["transaction"].(map[string]any)["id"].(string)

	// The admin rolls it back with a body-less DELETE: ownership must match without the
	// impersonatedUser field being present.
	r = httptest.NewRequest(http.MethodDelete, "/db/neo4j/tx/"+id, nil)
	r.Header.Set("Authorization", basic("a", "pw"))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Errorf("admin rollback of impersonated tx = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
}

// TestImpersonationTxWriteAuthorizedAsTarget confirms a transactional write begun under
// impersonation is authorized with the target's roles: an admin acting as a reader is
// refused a write in a begin batch (doc 18 §10.5, §10.6).
func TestImpersonationTxWriteAuthorizedAsTarget(t *testing.T) {
	h := impHandler(t, map[string][]string{"a": {"admin"}, "r": {"reader"}})
	begin := `{"statements":[{"statement":` + jsonString(writeStmt) + `}],"impersonatedUser":"r"}`
	r := httptest.NewRequest(http.MethodPost, "/db/neo4j/tx", strings.NewReader(begin))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", basic("a", "pw"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Errorf("admin-as-reader begin write = %d, want 403", rec.Code)
	}
}
