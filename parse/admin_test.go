package parse_test

import (
	"testing"

	"github.com/tamnd/gr/ast"
	"github.com/tamnd/gr/parse"
)

func TestParseCreateUser(t *testing.T) {
	q, err := parse.Parse("CREATE USER ada SET PASSWORD 's3cret'")
	if err != nil {
		t.Fatal(err)
	}
	cu, ok := q.Admin.(*ast.CreateUser)
	if !ok {
		t.Fatalf("Admin is %T, want *ast.CreateUser", q.Admin)
	}
	if cu.Name != "ada" || cu.Password != "s3cret" || cu.IfNotExists {
		t.Fatalf("got %+v", cu)
	}
	if q.First != nil {
		t.Fatal("admin command should leave First nil")
	}
}

func TestParseCreateUserIfNotExists(t *testing.T) {
	q, err := parse.Parse("CREATE USER ada IF NOT EXISTS SET PASSWORD 'pw'")
	if err != nil {
		t.Fatal(err)
	}
	cu := q.Admin.(*ast.CreateUser)
	if !cu.IfNotExists {
		t.Fatal("IF NOT EXISTS not parsed")
	}
}

func TestParseCreateUserQuotedName(t *testing.T) {
	q, err := parse.Parse("CREATE USER 'ada lovelace' SET PASSWORD 'pw'")
	if err != nil {
		t.Fatal(err)
	}
	if name := q.Admin.(*ast.CreateUser).Name; name != "ada lovelace" {
		t.Fatalf("name = %q, want quoted name", name)
	}
}

func TestParseAlterUser(t *testing.T) {
	q, err := parse.Parse("ALTER USER ada SET PASSWORD 'newpw'")
	if err != nil {
		t.Fatal(err)
	}
	au, ok := q.Admin.(*ast.AlterUser)
	if !ok {
		t.Fatalf("Admin is %T, want *ast.AlterUser", q.Admin)
	}
	if au.Name != "ada" || au.Password != "newpw" {
		t.Fatalf("got %+v", au)
	}
}

func TestParseDropUser(t *testing.T) {
	for _, c := range []struct {
		src      string
		ifExists bool
	}{
		{"DROP USER ada", false},
		{"DROP USER ada IF EXISTS", true},
	} {
		q, err := parse.Parse(c.src)
		if err != nil {
			t.Fatalf("%q: %v", c.src, err)
		}
		du := q.Admin.(*ast.DropUser)
		if du.Name != "ada" || du.IfExists != c.ifExists {
			t.Fatalf("%q: got %+v", c.src, du)
		}
	}
}

func TestParseShowUsers(t *testing.T) {
	q, err := parse.Parse("SHOW USERS")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := q.Admin.(*ast.ShowUsers); !ok {
		t.Fatalf("Admin is %T, want *ast.ShowUsers", q.Admin)
	}
}

func TestParseGrantRole(t *testing.T) {
	q, err := parse.Parse("GRANT ROLE editor TO ada")
	if err != nil {
		t.Fatal(err)
	}
	g, ok := q.Admin.(*ast.GrantRole)
	if !ok {
		t.Fatalf("Admin is %T, want *ast.GrantRole", q.Admin)
	}
	if g.Role != "editor" || g.User != "ada" {
		t.Fatalf("got %+v", g)
	}
}

func TestParseRevokeRole(t *testing.T) {
	q, err := parse.Parse("REVOKE ROLE editor FROM ada")
	if err != nil {
		t.Fatal(err)
	}
	rv, ok := q.Admin.(*ast.RevokeRole)
	if !ok {
		t.Fatalf("Admin is %T, want *ast.RevokeRole", q.Admin)
	}
	if rv.Role != "editor" || rv.User != "ada" {
		t.Fatalf("got %+v", rv)
	}
}

// TestParseUserWordsStillIdentifiers confirms the soft keywords stay usable as ordinary
// names in a normal query, so adding the administrative grammar reserves nothing.
func TestParseUserWordsStillIdentifiers(t *testing.T) {
	for _, src := range []string{
		"MATCH (user:User) RETURN user",
		"MATCH (n) WHERE n.role = 'x' RETURN n",
		"RETURN 1 AS grant",
	} {
		if _, err := parse.Parse(src); err != nil {
			t.Errorf("%q: %v", src, err)
		}
	}
}

// TestParseMissingPassword rejects CREATE USER with no SET PASSWORD clause.
func TestParseMissingPassword(t *testing.T) {
	if _, err := parse.Parse("CREATE USER ada"); err == nil {
		t.Error("CREATE USER with no password parsed without error")
	}
}
