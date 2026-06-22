package httpd

import (
	"errors"
	"net/http"
)

// impersonate resolves the effective principal for a request that asks to run as another
// user (doc 18 §10.5). It is called by each data handler after the body is decoded, since
// the impersonatedUser field travels in the body, not the Authorization header.
//
// With no impersonation requested the request is returned unchanged. Otherwise every gate
// must pass: impersonation must be enabled on the server, an auth provider must be
// configured (an anonymous server has no actor to authorize), the authenticated principal
// must hold the impersonation privilege (the admin role, doc 18 §10.6), and the provider
// must be a RoleResolver that knows the target user. A failure is a forbidden response,
// non-disclosing about which gate failed so a probe cannot tell a missing target from one
// the actor may not assume.
//
// On success the returned request carries an effective principal whose Name and Roles are
// the impersonated user's, so authorization and execution run as that user, with
// ImpersonatedBy set to the actor so the audit trail records both identities (doc 18
// §10.5). The returned bool reports whether the caller may proceed; on false the forbidden
// response is already written.
func (s *server) impersonate(w http.ResponseWriter, r *http.Request, impUser string) (*http.Request, bool) {
	if impUser == "" {
		return r, true
	}
	if !s.impersonation || s.auth == nil {
		s.writeForbiddenImpersonation(w)
		return r, false
	}
	actor := principalFrom(r.Context())
	if principalLevel(actor) < levelAdmin {
		s.writeForbiddenImpersonation(w)
		return r, false
	}
	resolver, ok := s.auth.(RoleResolver)
	if !ok {
		s.writeForbiddenImpersonation(w)
		return r, false
	}
	target, err := resolver.Resolve(r.Context(), impUser)
	if err != nil {
		if errors.Is(err, ErrNoSuchPrincipal) {
			s.writeForbiddenImpersonation(w)
			return r, false
		}
		status, ae := mapError(err)
		s.writeError(w, status, ae)
		return r, false
	}
	eff := &Principal{Name: target.Name, Roles: target.Roles, ImpersonatedBy: actor.Name}
	return r.WithContext(withPrincipal(r.Context(), eff)), true
}

// writeForbiddenImpersonation writes the 403 for a disallowed impersonation (doc 18
// §10.5). The message names the privilege but not which specific gate failed, so a probe
// cannot use it to enumerate users or learn the impersonation policy.
func (s *server) writeForbiddenImpersonation(w http.ResponseWriter) {
	s.writeError(w, http.StatusForbidden, apiError{
		Code:    "Neo.ClientError.Security.Forbidden",
		Message: "this account is not allowed to impersonate",
	})
}
