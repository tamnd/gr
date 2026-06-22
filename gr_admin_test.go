package gr

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/tamnd/gr/vfs"
)

// adminDB opens a fresh in-memory database for the administrative-statement tests.
func adminDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(":memory:.gr", Options{VFS: vfs.NewMem()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// run is a thin helper that runs an administrative statement through Run and fails on error.
func run(t *testing.T, db *DB, cypher string) *Result {
	t.Helper()
	res, err := db.Run(context.Background(), cypher, nil)
	if err != nil {
		t.Fatalf("run %q: %v", cypher, err)
	}
	return res
}

// TestAdminCreateUserStatement runs CREATE USER as Cypher and confirms the user can then
// authenticate, the proof the statement reaches the same credential store the library API
// writes (doc 18 §12.3).
func TestAdminCreateUserStatement(t *testing.T) {
	db := adminDB(t)
	run(t, db, "CREATE USER ada SET PASSWORD 's3cret'")

	roles, ok, err := db.Authenticate("ada", "s3cret")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("user created by statement does not authenticate")
	}
	if len(roles) != 0 {
		t.Errorf("fresh user roles = %v, want none", roles)
	}
}

// TestAdminCreateUserIfNotExists confirms IF NOT EXISTS makes a repeat creation a no-op
// while a plain repeat is an error (doc 18 §12.3).
func TestAdminCreateUserIfNotExists(t *testing.T) {
	db := adminDB(t)
	run(t, db, "CREATE USER ada SET PASSWORD 'pw'")

	if _, err := db.Run(context.Background(), "CREATE USER ada SET PASSWORD 'pw'", nil); !errors.Is(err, ErrUserExists) {
		t.Errorf("plain repeat: got %v, want ErrUserExists", err)
	}
	run(t, db, "CREATE USER ada IF NOT EXISTS SET PASSWORD 'pw'")
}

// TestAdminAlterUserStatement confirms ALTER USER changes the password.
func TestAdminAlterUserStatement(t *testing.T) {
	db := adminDB(t)
	run(t, db, "CREATE USER ada SET PASSWORD 'old'")
	run(t, db, "ALTER USER ada SET PASSWORD 'new'")

	if _, ok, _ := db.Authenticate("ada", "old"); ok {
		t.Error("old password still works after ALTER USER")
	}
	if _, ok, _ := db.Authenticate("ada", "new"); !ok {
		t.Error("new password does not work after ALTER USER")
	}
}

// TestAdminDropUserStatement confirms DROP USER removes the user, with IF EXISTS turning
// a missing target into a no-op (doc 18 §12.3).
func TestAdminDropUserStatement(t *testing.T) {
	db := adminDB(t)
	run(t, db, "CREATE USER ada SET PASSWORD 'pw'")
	run(t, db, "DROP USER ada")

	if _, ok, _ := db.Authenticate("ada", "pw"); ok {
		t.Error("dropped user still authenticates")
	}
	if _, err := db.Run(context.Background(), "DROP USER ada", nil); !errors.Is(err, ErrNoSuchUser) {
		t.Errorf("plain drop of absent user: got %v, want ErrNoSuchUser", err)
	}
	run(t, db, "DROP USER ada IF EXISTS")
}

// TestAdminGrantRevokeStatement confirms GRANT ROLE and REVOKE ROLE change a user's roles.
func TestAdminGrantRevokeStatement(t *testing.T) {
	db := adminDB(t)
	run(t, db, "CREATE USER ada SET PASSWORD 'pw'")

	run(t, db, "GRANT ROLE editor TO ada")
	roles, _, _ := db.Authenticate("ada", "pw")
	if !slices.Equal(roles, []string{RoleEditor}) {
		t.Errorf("after grant roles = %v, want [editor]", roles)
	}

	run(t, db, "REVOKE ROLE editor FROM ada")
	roles, _, _ = db.Authenticate("ada", "pw")
	if len(roles) != 0 {
		t.Errorf("after revoke roles = %v, want none", roles)
	}
}

// TestAdminShowUsers confirms SHOW USERS yields one row per user, sorted by name, with the
// name and its roles and no secret (doc 18 §12.3).
func TestAdminShowUsers(t *testing.T) {
	db := adminDB(t)
	run(t, db, "CREATE USER zoe SET PASSWORD 'pw'")
	run(t, db, "CREATE USER ada SET PASSWORD 'pw'")
	run(t, db, "GRANT ROLE editor TO ada")

	res := run(t, db, "SHOW USERS")
	defer func() { _ = res.Close() }()

	if cols := res.Columns(); !slices.Equal(cols, []string{"user", "roles"}) {
		t.Fatalf("columns = %v, want [user roles]", cols)
	}

	var names []string
	for res.Next() {
		name, err := res.Record().GetString("user")
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if err := res.Err(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(names, []string{"ada", "zoe"}) {
		t.Errorf("SHOW USERS names = %v, want sorted [ada zoe]", names)
	}
}

// TestAdminShowUsersThroughExec confirms SHOW USERS run through the row-less Exec is
// refused with ErrAdminRows rather than silently dropping its rows (doc 18 §12.3).
func TestAdminShowUsersThroughExec(t *testing.T) {
	db := adminDB(t)
	if _, err := db.Exec("SHOW USERS", nil); !errors.Is(err, ErrAdminRows) {
		t.Errorf("SHOW USERS via Exec: got %v, want ErrAdminRows", err)
	}
}

// TestAdminMutationThroughExec confirms an administrative mutation does run through Exec,
// since it yields only an outcome and no rows.
func TestAdminMutationThroughExec(t *testing.T) {
	db := adminDB(t)
	if _, err := db.Exec("CREATE USER ada SET PASSWORD 'pw'", nil); err != nil {
		t.Fatalf("CREATE USER via Exec: %v", err)
	}
	if _, ok, _ := db.Authenticate("ada", "pw"); !ok {
		t.Error("user created through Exec does not authenticate")
	}
}

// TestAdminRejectedByQuery confirms Query, the read-only entry point, refuses an
// administrative statement with ErrAdminCommand (doc 18 §12.3).
func TestAdminRejectedByQuery(t *testing.T) {
	db := adminDB(t)
	if _, err := db.Query("CREATE USER ada SET PASSWORD 'pw'", nil); !errors.Is(err, ErrAdminCommand) {
		t.Errorf("admin via Query: got %v, want ErrAdminCommand", err)
	}
}

// TestAdminRejectedInTransaction confirms a managed transaction refuses an administrative
// statement through both Run and Exec, since the credential API runs its own durable
// transaction and cannot join the caller's (doc 18 §12.3).
func TestAdminRejectedInTransaction(t *testing.T) {
	db := adminDB(t)
	tx, err := db.Begin(context.Background(), Write)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Run(context.Background(), "CREATE USER ada SET PASSWORD 'pw'", nil); !errors.Is(err, ErrAdminCommand) {
		t.Errorf("admin via tx.Run: got %v, want ErrAdminCommand", err)
	}
	if _, err := tx.Exec("CREATE USER ada SET PASSWORD 'pw'", nil); !errors.Is(err, ErrAdminCommand) {
		t.Errorf("admin via tx.Exec: got %v, want ErrAdminCommand", err)
	}
}

// TestAdminExplainRejected confirms EXPLAIN refuses an administrative statement: there is
// no operator plan to show for a credential-store mutation (doc 18 §12.3).
func TestAdminExplainRejected(t *testing.T) {
	db := adminDB(t)
	if _, err := db.Run(context.Background(), "EXPLAIN CREATE USER ada SET PASSWORD 'pw'", nil); err == nil {
		t.Error("EXPLAIN of an administrative statement was accepted")
	}
}

// TestAdminStatementKind confirms an administrative statement classifies as
// AdminStatement, the kind the transport reads to require the admin role (doc 18 §10.6).
func TestAdminStatementKind(t *testing.T) {
	db := adminDB(t)
	for _, src := range []string{
		"CREATE USER ada SET PASSWORD 'pw'",
		"ALTER USER ada SET PASSWORD 'pw'",
		"DROP USER ada",
		"SHOW USERS",
		"GRANT ROLE editor TO ada",
		"REVOKE ROLE editor FROM ada",
	} {
		kind, err := db.StatementKind(src)
		if err != nil {
			t.Fatalf("%q: %v", src, err)
		}
		if kind != AdminStatement {
			t.Errorf("%q kind = %v, want admin", src, kind)
		}
	}
}
