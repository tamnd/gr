package ast

// AdminCommand is an administrative statement that manages the served database's users
// and their roles (doc 18 §10, §12.3) rather than reading or writing graph data or
// changing the catalog. Like a SchemaCommand it is a whole statement on its own, not a
// clause inside a query, and it carries on ast.Query's Admin field. The marker keeps the
// set closed. Every administrative statement requires the admin role (doc 18 §12.3).
type AdminCommand interface {
	adminNode()
}

// CreateUser is a CREATE USER statement: it adds a user with a password (doc 18 §10.3).
// Name is the user name and Password the plaintext to salt and hash; the statement
// grants no roles, so a fresh user has no privileges until a GRANT ROLE, matching Neo4j.
// IfNotExists makes a repeat creation a no-op rather than an error.
type CreateUser struct {
	Pos
	Name        string
	Password    string
	IfNotExists bool
}

// AlterUser is an ALTER USER statement that changes a user's password (doc 18 §10.3).
// Name is the user and Password the new plaintext; the statement leaves the user's roles
// untouched, which GRANT and REVOKE change.
type AlterUser struct {
	Pos
	Name     string
	Password string
}

// DropUser is a DROP USER statement, addressing a user by name (doc 18 §10.3). IfExists
// makes dropping an absent user a no-op rather than an error.
type DropUser struct {
	Pos
	Name     string
	IfExists bool
}

// ShowUsers is a SHOW USERS statement: it lists every user and its roles (doc 18 §12.3).
// Unlike the other administrative statements it yields rows, a "user" and a "roles"
// column, rather than a bare outcome.
type ShowUsers struct {
	Pos
}

// GrantRole is a GRANT ROLE <role> TO <user> statement (doc 18 §10.6). Role is the role
// to add and User the user to add it to; the grant is idempotent.
type GrantRole struct {
	Pos
	Role string
	User string
}

// RevokeRole is a REVOKE ROLE <role> FROM <user> statement (doc 18 §10.6). Role is the
// role to remove and User the user to remove it from; the revoke is idempotent.
type RevokeRole struct {
	Pos
	Role string
	User string
}

func (*CreateUser) adminNode() {}
func (*AlterUser) adminNode()  {}
func (*DropUser) adminNode()   {}
func (*ShowUsers) adminNode()  {}
func (*GrantRole) adminNode()  {}
func (*RevokeRole) adminNode() {}
