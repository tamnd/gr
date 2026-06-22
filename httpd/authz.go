package httpd

import (
	"net/http"

	"github.com/tamnd/gr"
)

// Role levels order the built-in roles by what kind of statement they may run (doc 18
// §10.6). A principal's effective level is the highest level any of its roles grants;
// a statement is allowed when that level reaches the statement's kind level. The roles
// nest: an editor may do everything a reader may, a publisher everything an editor may,
// and so on, so a single >= comparison is the whole check.
const (
	levelNone   = -1 // no role, or only unknown roles: may run nothing.
	levelRead   = 0  // reader: read statements only.
	levelWrite  = 1  // editor: read and write data.
	levelSchema = 2  // publisher: read, write, and change schema.
	levelAdmin  = 3  // admin: everything publisher may, plus user management.
)

// roleLevel returns the access level a single role grants (doc 18 §10.6). An unknown
// role grants nothing, so a typo in a role name fails closed rather than open. Admin sits
// above publisher because it alone may run the administrative statements (user and role
// management, doc 18 §12.3), which a publisher may not.
func roleLevel(role string) int {
	switch role {
	case "reader":
		return levelRead
	case "editor":
		return levelWrite
	case "publisher":
		return levelSchema
	case "admin":
		return levelAdmin
	default:
		return levelNone
	}
}

// principalLevel returns the highest access level a principal's roles grant (doc 18
// §10.6). A principal with no roles, or only unknown ones, is levelNone and may run
// nothing.
func principalLevel(p *Principal) int {
	level := levelNone
	for _, role := range p.Roles {
		if l := roleLevel(role); l > level {
			level = l
		}
	}
	return level
}

// kindLevel returns the access level a statement of the given kind requires (doc 18
// §10.6, §12.3): a read needs levelRead, a write needs levelWrite, a schema change needs
// levelSchema, and an administrative statement needs levelAdmin.
func kindLevel(k gr.Kind) int {
	switch k {
	case gr.WriteStatement:
		return levelWrite
	case gr.SchemaStatement:
		return levelSchema
	case gr.AdminStatement:
		return levelAdmin
	default:
		return levelRead
	}
}

// authorize checks that the request's principal may run the statement, before any side
// effect (doc 18 §10.6). With authentication off (no provider) authorization is off too,
// so every statement runs; otherwise the statement is classified by kind without
// executing it and the principal's role level must reach the kind's level, or the
// request is refused with 403. An unparseable statement is allowed through so the normal
// execution path reports it as a syntax error rather than a misleading forbidden error.
// It returns true when the caller may proceed and writes the 403 itself otherwise.
func (s *server) authorize(w http.ResponseWriter, r *http.Request, cypher string) bool {
	if s.auth == nil {
		return true
	}
	kind, err := s.db.StatementKind(cypher)
	if err != nil {
		// Let execution surface the parse error as a 400; a 403 here would be wrong
		// and would leak that the statement was even classified.
		return true
	}
	princ := principalFrom(r.Context())
	if principalLevel(princ) < kindLevel(kind) {
		s.writeError(w, http.StatusForbidden, apiError{
			Code:    "Neo.ClientError.Security.Forbidden",
			Message: "this account is not allowed to run " + kind.String() + " statements",
		})
		return false
	}
	return true
}

// authorizeStatements checks every statement in a transactional batch before any of them
// runs (doc 18 §10.6, §9.5). The first statement the principal may not run refuses the
// whole batch with 403, so a forbidden write buried after an allowed read never executes.
func (s *server) authorizeStatements(w http.ResponseWriter, r *http.Request, stmts []statement) bool {
	for _, st := range stmts {
		if !s.authorize(w, r, st.Statement) {
			return false
		}
	}
	return true
}
