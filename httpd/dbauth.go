package httpd

import (
	"context"
	"time"

	"github.com/tamnd/gr"
)

// DBProvider authenticates against the database's own persistent credential store (doc
// 18 §10.3, §10.4): the users created with CREATE USER and held in the file's reserved
// system area, rather than a list configured in code at startup. It is the provider a
// served database uses once it has its own users, so the same database file carries its
// users wherever it is opened. It verifies the basic scheme only, like the in-memory
// StaticProvider, and shares the same failed-attempt lockout, so swapping the backing
// store does not change the brute-force protection.
type DBProvider struct {
	lockout
	db *gr.DB
}

// NewDBProvider returns a provider that authenticates against db's credential store with
// the default lockout policy (doc 18 §10.3). The database must outlive the provider; the
// provider only reads its credential store and never closes it.
func NewDBProvider(db *gr.DB) *DBProvider {
	return &DBProvider{lockout: newLockout(), db: db}
}

// SetLockout sets the failed-attempt lockout policy (doc 18 §10.3) and returns the
// provider so a caller can chain it after NewDBProvider.
func (p *DBProvider) SetLockout(maxFailed int, dur time.Duration) *DBProvider {
	p.setPolicy(maxFailed, dur)
	return p
}

// Authenticate verifies a basic credential against the database's credential store (doc
// 18 §10.4). A bearer or any other scheme is rejected, so a deployment that needs tokens
// installs the JWT provider instead. The constant-time compare and the unknown-user
// timing defense live in the store's own Authenticate (the library guards the
// user-enumeration channel), so this method adds only the scheme check, the lockout, and
// the principal shaping.
func (p *DBProvider) Authenticate(ctx context.Context, scheme, principal string, credential []byte) (*Principal, error) {
	if scheme != "basic" {
		return nil, ErrUnauthorized
	}
	// A locked principal is refused without consulting the store (doc 18 §10.3), even
	// with a correct password. A throwaway verify against a name that cannot exist still
	// runs so a locked attempt costs about the same as a normal failure and the lockout
	// does not leak through timing.
	if p.locked(principal) {
		_, _, _ = p.db.Authenticate("\x00locked", string(credential))
		return nil, ErrLockedOut
	}
	roles, ok, err := p.db.Authenticate(principal, string(credential))
	if err != nil {
		return nil, err
	}
	if !ok {
		p.recordFailure(principal)
		return nil, ErrUnauthorized
	}
	p.recordSuccess(principal)
	return &Principal{Name: principal, Roles: roles}, nil
}

// Schemes reports that the database-backed store verifies only the basic scheme.
func (p *DBProvider) Schemes() []string { return []string{"basic"} }

// Resolve returns the principal for a user by name, without a credential check, so an
// admin may impersonate it (doc 18 §10.5). It reads the live credential store, so a user
// created or dropped after the server starts resolves correctly, and returns
// ErrNoSuchPrincipal for a name the store does not hold.
func (p *DBProvider) Resolve(ctx context.Context, name string) (*Principal, error) {
	users, err := p.db.Users()
	if err != nil {
		return nil, err
	}
	for _, u := range users {
		if u.Name == name {
			return &Principal{Name: name, Roles: append([]string(nil), u.Roles...)}, nil
		}
	}
	return nil, ErrNoSuchPrincipal
}
