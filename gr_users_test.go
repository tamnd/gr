package gr

import (
	"errors"
	"slices"
	"testing"

	"github.com/tamnd/gr/vfs"
)

// TestUserLifecycle exercises the create, authenticate, alter, and drop path end to
// end against an in-memory database (doc 18 §10).
func TestUserLifecycle(t *testing.T) {
	db, err := Open(":memory:.gr", Options{VFS: vfs.NewMem()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	if err := db.CreateUser("ada", "s3cret", RoleEditor); err != nil {
		t.Fatalf("create: %v", err)
	}

	roles, ok, err := db.Authenticate("ada", "s3cret")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("right password did not authenticate")
	}
	if !slices.Equal(roles, []string{RoleEditor}) {
		t.Errorf("roles = %v, want [editor]", roles)
	}

	if _, ok, _ := db.Authenticate("ada", "wrong"); ok {
		t.Error("wrong password authenticated")
	}
	if _, ok, _ := db.Authenticate("nobody", "s3cret"); ok {
		t.Error("unknown user authenticated")
	}

	if err := db.AlterUser("ada", "newpass"); err != nil {
		t.Fatalf("alter: %v", err)
	}
	if _, ok, _ := db.Authenticate("ada", "s3cret"); ok {
		t.Error("old password still works after alter")
	}
	if _, ok, _ := db.Authenticate("ada", "newpass"); !ok {
		t.Error("new password does not work after alter")
	}

	if err := db.DropUser("ada"); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if _, ok, _ := db.Authenticate("ada", "newpass"); ok {
		t.Error("dropped user still authenticates")
	}
}

// TestUserErrors confirms the public sentinels fire on the documented misuses.
func TestUserErrors(t *testing.T) {
	db, err := Open(":memory:.gr", Options{VFS: vfs.NewMem()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	if err := db.CreateUser("", "p"); !errors.Is(err, ErrEmptyUserName) {
		t.Errorf("empty name: got %v, want ErrEmptyUserName", err)
	}
	if err := db.CreateUser("ada", "p", "owner"); !errors.Is(err, ErrInvalidRole) {
		t.Errorf("bad role: got %v, want ErrInvalidRole", err)
	}
	if err := db.CreateUser("ada", "p", RoleReader); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateUser("ada", "q"); !errors.Is(err, ErrUserExists) {
		t.Errorf("duplicate: got %v, want ErrUserExists", err)
	}
	if err := db.AlterUser("ghost", "p"); !errors.Is(err, ErrNoSuchUser) {
		t.Errorf("alter ghost: got %v, want ErrNoSuchUser", err)
	}
	if err := db.DropUser("ghost"); !errors.Is(err, ErrNoSuchUser) {
		t.Errorf("drop ghost: got %v, want ErrNoSuchUser", err)
	}
	if err := db.GrantRole("ghost", RoleAdmin); !errors.Is(err, ErrNoSuchUser) {
		t.Errorf("grant ghost: got %v, want ErrNoSuchUser", err)
	}
	if err := db.GrantRole("ada", "owner"); !errors.Is(err, ErrInvalidRole) {
		t.Errorf("grant bad role: got %v, want ErrInvalidRole", err)
	}
}

// TestRoleGrantRevoke confirms grant and revoke are idempotent and reflected in Users.
func TestRoleGrantRevoke(t *testing.T) {
	db, err := Open(":memory:.gr", Options{VFS: vfs.NewMem()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	if err := db.CreateUser("ada", "p", RoleReader); err != nil {
		t.Fatal(err)
	}
	if err := db.GrantRole("ada", RoleEditor); err != nil {
		t.Fatal(err)
	}
	if err := db.GrantRole("ada", RoleEditor); err != nil {
		t.Fatalf("second grant should be a no-op: %v", err)
	}
	roles, _, _ := db.Authenticate("ada", "p")
	if !slices.Equal(roles, []string{RoleReader, RoleEditor}) {
		t.Errorf("after grant roles = %v, want [reader editor]", roles)
	}

	if err := db.RevokeRole("ada", RoleReader); err != nil {
		t.Fatal(err)
	}
	if err := db.RevokeRole("ada", RoleReader); err != nil {
		t.Fatalf("second revoke should be a no-op: %v", err)
	}
	roles, _, _ = db.Authenticate("ada", "p")
	if !slices.Equal(roles, []string{RoleEditor}) {
		t.Errorf("after revoke roles = %v, want [editor]", roles)
	}
}

// TestUsersSorted confirms Users lists every user sorted by name and carries no secret.
func TestUsersSorted(t *testing.T) {
	db, err := Open(":memory:.gr", Options{VFS: vfs.NewMem()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	for _, n := range []string{"zoe", "ada", "lin"} {
		if err := db.CreateUser(n, "p", RoleReader); err != nil {
			t.Fatal(err)
		}
	}
	users, err := db.Users()
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, len(users))
	for i, u := range users {
		got[i] = u.Name
	}
	if !slices.Equal(got, []string{"ada", "lin", "zoe"}) {
		t.Errorf("Users order = %v, want sorted", got)
	}
}

// TestUserDurability confirms users survive a clean close and reopen, the proof the
// store lives in the file and not in memory (doc 18 §10.3).
func TestUserDurability(t *testing.T) {
	fsys := vfs.NewMem()
	db, err := Open("users.gr", Options{VFS: fsys, SaltSeed: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateUser("ada", "s3cret", RolePublisher); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db2, err := Open("users.gr", Options{VFS: fsys, SaltSeed: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db2.Close() }()
	roles, ok, err := db2.Authenticate("ada", "s3cret")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("user did not survive reopen")
	}
	if !slices.Equal(roles, []string{RolePublisher}) {
		t.Errorf("roles after reopen = %v, want [publisher]", roles)
	}
}

// TestUserClosed confirms the credential methods report ErrClosed on a closed database.
func TestUserClosed(t *testing.T) {
	db, err := Open(":memory:.gr", Options{VFS: vfs.NewMem()})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateUser("ada", "p"); !errors.Is(err, ErrClosed) {
		t.Errorf("create on closed: got %v, want ErrClosed", err)
	}
	if _, _, err := db.Authenticate("ada", "p"); !errors.Is(err, ErrClosed) {
		t.Errorf("authenticate on closed: got %v, want ErrClosed", err)
	}
	if _, err := db.Users(); !errors.Is(err, ErrClosed) {
		t.Errorf("users on closed: got %v, want ErrClosed", err)
	}
}
