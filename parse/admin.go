package parse

import (
	"github.com/tamnd/gr/ast"
	"github.com/tamnd/gr/lex"
)

// The administrative-statement grammar (doc 18 §10, §12.3). The keywords USER, USERS,
// ROLE, PASSWORD, GRANT, REVOKE, ALTER, SHOW, and TO/FROM are soft keywords recognized
// only in these statements (through wordIs), so they stay usable as ordinary identifiers
// in a normal query. A user name is a symbolic name or a quoted string, so a name with
// characters an identifier cannot hold still works.

// createUser parses CREATE USER name [IF NOT EXISTS] SET PASSWORD 'pw'.
func (p *parser) createUser() (*ast.Query, error) {
	start := p.advance() // CREATE
	if _, err := p.expectWord("USER"); err != nil {
		return nil, err
	}
	name, err := p.userName()
	if err != nil {
		return nil, err
	}
	cu := &ast.CreateUser{Pos: pos(start), Name: name}
	if p.acceptWord("IF") {
		// NOT is a hard keyword (it negates expressions), so it comes through as
		// lex.Not rather than a bare identifier; match it by token, the same way the
		// schema grammar reads IF NOT EXISTS (parse/schema.go).
		if _, err := p.expect(lex.Not); err != nil {
			return nil, err
		}
		if _, err := p.expectWord("EXISTS"); err != nil {
			return nil, err
		}
		cu.IfNotExists = true
	}
	pw, err := p.setPassword()
	if err != nil {
		return nil, err
	}
	cu.Password = pw
	return &ast.Query{Pos: pos(start), Admin: cu}, nil
}

// alterUser parses ALTER USER name SET PASSWORD 'pw'.
func (p *parser) alterUser() (*ast.Query, error) {
	start := p.advance() // ALTER
	if _, err := p.expectWord("USER"); err != nil {
		return nil, err
	}
	name, err := p.userName()
	if err != nil {
		return nil, err
	}
	pw, err := p.setPassword()
	if err != nil {
		return nil, err
	}
	return &ast.Query{Pos: pos(start), Admin: &ast.AlterUser{Pos: pos(start), Name: name, Password: pw}}, nil
}

// dropUser parses DROP USER name [IF EXISTS].
func (p *parser) dropUser() (*ast.Query, error) {
	start := p.advance() // DROP
	if _, err := p.expectWord("USER"); err != nil {
		return nil, err
	}
	name, err := p.userName()
	if err != nil {
		return nil, err
	}
	du := &ast.DropUser{Pos: pos(start), Name: name}
	if p.acceptWord("IF") {
		if _, err := p.expectWord("EXISTS"); err != nil {
			return nil, err
		}
		du.IfExists = true
	}
	return &ast.Query{Pos: pos(start), Admin: du}, nil
}

// showUsers parses SHOW USERS.
func (p *parser) showUsers() (*ast.Query, error) {
	start := p.advance() // SHOW
	if _, err := p.expectWord("USERS"); err != nil {
		return nil, err
	}
	return &ast.Query{Pos: pos(start), Admin: &ast.ShowUsers{Pos: pos(start)}}, nil
}

// grantRole parses GRANT ROLE role TO user.
func (p *parser) grantRole() (*ast.Query, error) {
	start := p.advance() // GRANT
	role, user, err := p.roleAssignment("TO")
	if err != nil {
		return nil, err
	}
	return &ast.Query{Pos: pos(start), Admin: &ast.GrantRole{Pos: pos(start), Role: role, User: user}}, nil
}

// revokeRole parses REVOKE ROLE role FROM user.
func (p *parser) revokeRole() (*ast.Query, error) {
	start := p.advance() // REVOKE
	role, user, err := p.roleAssignment("FROM")
	if err != nil {
		return nil, err
	}
	return &ast.Query{Pos: pos(start), Admin: &ast.RevokeRole{Pos: pos(start), Role: role, User: user}}, nil
}

// roleAssignment parses the shared ROLE <role> <connector> <user> tail of GRANT and
// REVOKE, where connector is "TO" for GRANT and "FROM" for REVOKE.
func (p *parser) roleAssignment(connector string) (role, user string, err error) {
	if _, err := p.expectWord("ROLE"); err != nil {
		return "", "", err
	}
	role, err = p.userName()
	if err != nil {
		return "", "", err
	}
	if _, err := p.expectWord(connector); err != nil {
		return "", "", err
	}
	user, err = p.userName()
	if err != nil {
		return "", "", err
	}
	return role, user, nil
}

// setPassword parses the SET PASSWORD 'pw' tail shared by CREATE USER and ALTER USER.
func (p *parser) setPassword() (string, error) {
	if _, err := p.expect(lex.Set); err != nil {
		return "", err
	}
	if _, err := p.expectWord("PASSWORD"); err != nil {
		return "", err
	}
	t, err := p.expect(lex.String)
	if err != nil {
		return "", err
	}
	return t.Text, nil
}

// userName parses a user or role name: a symbolic name (identifier) or a quoted string.
// Accepting a string lets a name carry characters an identifier cannot hold.
func (p *parser) userName() (string, error) {
	switch p.cur().Kind {
	case lex.Ident, lex.String:
		return p.advance().Text, nil
	default:
		return "", p.errAt(p.cur(), "expected a user name, found "+p.cur().Kind.String())
	}
}
