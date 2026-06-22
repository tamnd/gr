package httpd

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/tamnd/gr"
	"github.com/tamnd/gr/vfs"
)

// dbWithUser opens an in-memory database with one user and returns it for a provider test.
func dbWithUser(t *testing.T, name, password string, roles ...string) *gr.DB {
	t.Helper()
	db, err := gr.Open(":memory:.gr", gr.Options{VFS: vfs.NewMem()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.CreateUser(name, password, roles...); err != nil {
		t.Fatal(err)
	}
	return db
}

// TestDBProviderAuthenticate confirms the provider verifies the database's own users,
// returns their roles, and rejects a wrong password and an unknown user alike.
func TestDBProviderAuthenticate(t *testing.T) {
	db := dbWithUser(t, "ada", "s3cret", gr.RoleEditor)
	p := NewDBProvider(db)

	princ, err := p.Authenticate(t.Context(), "basic", "ada", []byte("s3cret"))
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if princ.Name != "ada" || !slices.Equal(princ.Roles, []string{gr.RoleEditor}) {
		t.Errorf("principal = %+v, want ada [editor]", princ)
	}

	if _, err := p.Authenticate(t.Context(), "basic", "ada", []byte("wrong")); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("wrong password err = %v, want ErrUnauthorized", err)
	}
	if _, err := p.Authenticate(t.Context(), "basic", "nobody", []byte("s3cret")); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("unknown user err = %v, want ErrUnauthorized", err)
	}
}

// TestDBProviderRejectsBearer confirms the database-backed store is basic-only, so a
// bearer credential is refused and a deployment that needs tokens installs the JWT
// provider instead.
func TestDBProviderRejectsBearer(t *testing.T) {
	db := dbWithUser(t, "ada", "s3cret", gr.RoleReader)
	p := NewDBProvider(db)
	if _, err := p.Authenticate(t.Context(), "bearer", "", []byte("tok")); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("bearer err = %v, want ErrUnauthorized", err)
	}
	if !slices.Equal(p.Schemes(), []string{"basic"}) {
		t.Errorf("schemes = %v, want [basic]", p.Schemes())
	}
}

// TestDBProviderLockout confirms the database-backed provider shares the same
// failed-attempt lockout: a threshold of wrong passwords locks the principal even
// against the correct password, and the lock lifts when the window passes.
func TestDBProviderLockout(t *testing.T) {
	db := dbWithUser(t, "ada", "s3cret", gr.RoleAdmin)
	clock := time.Unix(1_700_000_000, 0)
	p := NewDBProvider(db).SetLockout(3, time.Minute)
	p.now = func() time.Time { return clock }

	for i := 0; i < 3; i++ {
		if _, err := p.Authenticate(t.Context(), "basic", "ada", []byte("wrong")); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("attempt %d err = %v, want ErrUnauthorized", i, err)
		}
	}
	if _, err := p.Authenticate(t.Context(), "basic", "ada", []byte("s3cret")); !errors.Is(err, ErrLockedOut) {
		t.Errorf("locked correct-password err = %v, want ErrLockedOut", err)
	}

	clock = clock.Add(time.Minute + time.Second)
	princ, err := p.Authenticate(t.Context(), "basic", "ada", []byte("s3cret"))
	if err != nil {
		t.Fatalf("after expiry: %v", err)
	}
	if princ.Name != "ada" {
		t.Errorf("principal after expiry = %+v, want ada", princ)
	}
}

// TestDBProviderResolve confirms the provider resolves a user's roles by name for
// impersonation, with no credential check, and reports ErrNoSuchPrincipal for a name the
// store does not hold (doc 18 §10.5).
func TestDBProviderResolve(t *testing.T) {
	db := dbWithUser(t, "ada", "s3cret", gr.RoleEditor)
	p := NewDBProvider(db)

	princ, err := p.Resolve(t.Context(), "ada")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if princ.Name != "ada" || !slices.Equal(princ.Roles, []string{gr.RoleEditor}) {
		t.Errorf("resolved = %+v, want ada [editor]", princ)
	}
	if _, err := p.Resolve(t.Context(), "ghost"); !errors.Is(err, ErrNoSuchPrincipal) {
		t.Errorf("resolve unknown err = %v, want ErrNoSuchPrincipal", err)
	}
}

// TestDBProviderSeesLiveUsers confirms the provider reads the live store, so a user
// created after the provider is built authenticates, and one dropped no longer does.
func TestDBProviderSeesLiveUsers(t *testing.T) {
	db, err := gr.Open(":memory:.gr", gr.Options{VFS: vfs.NewMem()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	p := NewDBProvider(db)

	if _, err := p.Authenticate(t.Context(), "basic", "lin", []byte("pw")); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("before create err = %v, want ErrUnauthorized", err)
	}
	if err := db.CreateUser("lin", "pw", gr.RoleReader); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Authenticate(t.Context(), "basic", "lin", []byte("pw")); err != nil {
		t.Errorf("after create: %v", err)
	}
	if err := db.DropUser("lin"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Authenticate(t.Context(), "basic", "lin", []byte("pw")); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("after drop err = %v, want ErrUnauthorized", err)
	}
}
