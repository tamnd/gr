package gr

import (
	"errors"

	"github.com/tamnd/gr/ast"
	"github.com/tamnd/gr/eval"
	"github.com/tamnd/gr/value"
)

// ErrAdminRows is returned by Exec for SHOW USERS, an administrative statement that
// yields rows. The row-less Exec cannot carry them, so SHOW USERS runs through Run.
var ErrAdminRows = errors.New("gr: SHOW USERS yields rows; run it through Run, not Exec")

// execAdmin runs an administrative statement against the credential API (doc 18 §10,
// §12.3) and returns its result: the rows for SHOW USERS, or an empty result for a
// mutation. An administrative statement manages users and roles, not graph data or the
// catalog, so it runs outside the read/write operator pipeline, through the same
// credential API the library exposes (gr_users.go). Authorization (admin only, doc 18
// §12.3) is the transport's job, checked before this runs, the same place it checks the
// role for a read, write, or schema statement.
func (db *DB) execAdmin(cmd ast.AdminCommand) (*Result, error) {
	switch c := cmd.(type) {
	case *ast.CreateUser:
		err := db.CreateUser(c.Name, c.Password)
		if err != nil && c.IfNotExists && errors.Is(err, ErrUserExists) {
			// IF NOT EXISTS makes a repeat creation a no-op rather than an error.
			err = nil
		}
		return emptyResult(), err
	case *ast.AlterUser:
		return emptyResult(), db.AlterUser(c.Name, c.Password)
	case *ast.DropUser:
		err := db.DropUser(c.Name)
		if err != nil && c.IfExists && errors.Is(err, ErrNoSuchUser) {
			// IF EXISTS makes dropping an absent user a no-op rather than an error.
			err = nil
		}
		return emptyResult(), err
	case *ast.ShowUsers:
		return db.showUsersResult()
	case *ast.GrantRole:
		return emptyResult(), db.GrantRole(c.User, c.Role)
	case *ast.RevokeRole:
		return emptyResult(), db.RevokeRole(c.User, c.Role)
	default:
		return nil, errors.New("gr: unsupported administrative statement")
	}
}

// showUsersResult runs SHOW USERS and builds its result: one row per user with a "user"
// string column and a "roles" list column, sorted by name (doc 18 §12.3). It carries no
// secret, only names and roles.
func (db *DB) showUsersResult() (*Result, error) {
	users, err := db.Users()
	if err != nil {
		return nil, err
	}
	buf := make([]eval.Row, 0, len(users))
	for _, u := range users {
		roles := make([]value.Value, len(u.Roles))
		for i, r := range u.Roles {
			roles[i] = value.String(r)
		}
		buf = append(buf, eval.Row{"user": value.String(u.Name), "roles": value.List(roles...)})
	}
	return &Result{cols: []string{"user", "roles"}, buf: buf}, nil
}

// emptyResult is the result of an administrative mutation: no columns and no rows, the
// outcome-only shape (doc 18 §12.3). The mutation's effect is durable when execAdmin
// returns, since the credential API commits its own transaction.
func emptyResult() *Result {
	return &Result{}
}
