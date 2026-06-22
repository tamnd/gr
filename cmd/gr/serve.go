package main

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/tamnd/gr"
	"github.com/tamnd/gr/httpd"
	"github.com/tamnd/gr/vfs"
)

// userList collects repeated --user name:password flags.
type userList []string

func (u *userList) String() string { return strings.Join(*u, ",") }
func (u *userList) Set(v string) error {
	*u = append(*u, v)
	return nil
}

// defaultServeAddr is the address gr serve binds when none is given (doc 18 §9.7).
// 7474 is the Neo4j HTTP port, so a tool pointed at the usual port finds the server.
const defaultServeAddr = ":7474"

// runServe implements the `gr serve` subcommand (doc 18 §9): it opens a database and
// serves the HTTP/JSON API over it until the process is stopped. It parses its own
// flag set rather than the shell's, since the serve options (the listen address, the
// database name in the URL path) do not overlap the shell's.
//
// listen is injected so a test can substitute a stub for net/http's ListenAndServe;
// the real entry point passes http.ListenAndServe.
func runServe(args []string, stdout, stderr io.Writer, listen func(addr string, h http.Handler) error) int {
	fs := flag.NewFlagSet("gr serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", defaultServeAddr, "address to listen on")
	name := fs.String("name", "neo4j", "database name in the URL path")
	readonly := fs.Bool("readonly", false, "open the database read-only")
	var users userList
	fs.Var(&users, "user", "name:password[:roles] for HTTP auth (repeatable); roles is a comma list of reader/editor/publisher/admin, default admin; none means auth is off")
	var jwt jwtOptions
	fs.StringVar(&jwt.hmacSecret, "jwt-hmac-secret", "", "HS256 shared secret for bearer-token (JWT) auth; selects the JWT provider")
	fs.StringVar(&jwt.pubKeyPath, "jwt-pubkey", "", "path to a PEM RSA or ECDSA public key for RS256/ES256 bearer-token auth")
	fs.StringVar(&jwt.issuer, "jwt-issuer", "", "required token issuer (iss claim) for bearer-token auth")
	fs.StringVar(&jwt.audience, "jwt-audience", "", "required token audience (aud claim) for bearer-token auth")
	fs.StringVar(&jwt.rolesClaim, "jwt-roles-claim", "", "token claim to read roles from (default roles)")
	tokenCacheTTL := fs.Duration("auth-token-cache-ttl", 0, "how long a validated bearer token is cached before revalidation (0 uses the default)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: gr serve [flags] [database]")
		fmt.Fprintln(stderr)
		fmt.Fprintln(stderr, "Serve the HTTP/JSON API over a database. With no database")
		fmt.Fprintln(stderr, "argument gr serves a transient in-memory database.")
		fmt.Fprintln(stderr)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	path := fs.Arg(0)
	db, err := openServeDB(path, *readonly)
	if err != nil {
		fmt.Fprintln(stderr, "gr:", err)
		return exitOpen
	}
	defer func() { _ = db.Close() }()

	auth, err := buildAuth(users, jwt)
	if err != nil {
		fmt.Fprintln(stderr, "gr:", err)
		return exitUsage
	}
	srv := httpd.New(db, httpd.Options{Name: *name, Auth: auth, TokenCacheTTL: *tokenCacheTTL})
	defer srv.Close()
	fmt.Fprintf(stderr, "gr serving %s on %s (database %q)\n", describeDB(path), *addr, *name)
	if auth == nil {
		fmt.Fprintln(stderr, "gr: WARNING authentication is off, every request is anonymous; pass --user to require auth")
	}

	// Reap transactions whose client vanished, so a dead HTTP client cannot pin the
	// writer until someone touches its id (doc 18 §8.7). The ticker stops when the
	// listener returns, so the goroutine does not outlive the server.
	stop := make(chan struct{})
	go sweepLoop(srv, stop)
	err = listen(*addr, srv)
	close(stop)
	if err != nil {
		fmt.Fprintln(stderr, "gr:", err)
		return exitIO
	}
	return exitOK
}

// sweepInterval is how often the serve command reaps expired transactions.
const sweepInterval = 10 * time.Second

// sweepLoop reaps expired transactions on a ticker until stop is closed.
func sweepLoop(srv *httpd.Server, stop <-chan struct{}) {
	t := time.NewTicker(sweepInterval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case now := <-t.C:
			srv.Sweep(now)
		}
	}
}

// openServeDB opens the database serve will host. An empty or :memory: path opens a
// transient in-memory database over the in-memory VFS, matching the shell's rule.
func openServeDB(path string, readonly bool) (*gr.DB, error) {
	if path == "" || path == ":memory:" {
		return gr.Open(memPath, gr.Options{VFS: vfs.NewMem()})
	}
	if _, err := os.Stat(path); err != nil && readonly {
		return nil, fmt.Errorf("cannot open a read-only database that does not exist: %s", path)
	}
	return gr.Open(path, gr.Options{ReadOnly: readonly})
}

// jwtOptions collects the bearer-token (JWT) flags. When any is set, serve runs the JWT
// provider for the bearer scheme instead of the static basic-credential provider; the two
// are mutually exclusive, since the server runs one provider at a time (doc 18 §10.7).
type jwtOptions struct {
	hmacSecret string
	pubKeyPath string
	issuer     string
	audience   string
	rolesClaim string
}

// set reports whether any JWT flag was given.
func (o jwtOptions) set() bool {
	return o.hmacSecret != "" || o.pubKeyPath != "" || o.issuer != "" || o.audience != "" || o.rolesClaim != ""
}

// buildAuth turns the auth flags into a credential provider, or returns nil when none were
// given so authentication is off. The --user flags select the static basic-credential
// provider (name:password[:roles], roles a comma list of reader/editor/publisher/admin,
// default admin so a single --user keeps full access). The --jwt-* flags select the JWT
// bearer provider instead; the two are mutually exclusive, since the server runs one
// provider at a time (doc 18 §10.7).
func buildAuth(users userList, jwt jwtOptions) (httpd.AuthProvider, error) {
	switch {
	case jwt.set() && len(users) > 0:
		return nil, fmt.Errorf("choose one auth provider: --user (basic) or --jwt-* (bearer), not both")
	case jwt.set():
		return buildJWT(jwt)
	case len(users) > 0:
		return buildStatic(users)
	default:
		return nil, nil
	}
}

// buildStatic builds the static basic-credential provider from the --user flags.
func buildStatic(users userList) (httpd.AuthProvider, error) {
	p := httpd.NewStaticProvider()
	for _, u := range users {
		name, pass, roles, err := parseUser(u)
		if err != nil {
			return nil, err
		}
		if err := p.AddUser(name, pass, roles...); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// buildJWT builds the bearer-token provider from the --jwt-* flags. A PEM public key path
// supplies the RS256/ES256 verification key (the kind is detected from the key), and the
// HMAC secret supplies the HS256 key; at least one must be present.
func buildJWT(o jwtOptions) (httpd.AuthProvider, error) {
	cfg := httpd.JWTConfig{
		Issuer:     o.issuer,
		Audience:   o.audience,
		RolesClaim: o.rolesClaim,
	}
	if o.hmacSecret != "" {
		cfg.HMACSecret = []byte(o.hmacSecret)
	}
	if o.pubKeyPath != "" {
		pemBytes, err := os.ReadFile(o.pubKeyPath)
		if err != nil {
			return nil, fmt.Errorf("read --jwt-pubkey: %w", err)
		}
		rsaKey, ecdsaKey, err := httpd.ParsePEMPublicKey(pemBytes)
		if err != nil {
			return nil, fmt.Errorf("parse --jwt-pubkey: %w", err)
		}
		cfg.RSAPublicKey = rsaKey
		cfg.ECDSAPublicKey = ecdsaKey
	}
	return httpd.NewJWTProvider(cfg)
}

// parseUser splits a --user flag into its name, password, and roles (doc 18 §10.6). The
// form is name:password with an optional :roles suffix; a missing roles suffix means the
// admin role. The name and password are required, so a flag with no colon or an empty
// name is a usage error.
func parseUser(u string) (name, pass string, roles []string, err error) {
	parts := strings.SplitN(u, ":", 3)
	if len(parts) < 2 || parts[0] == "" {
		return "", "", nil, fmt.Errorf("invalid --user %q, want name:password[:roles]", u)
	}
	name, pass = parts[0], parts[1]
	if len(parts) == 3 && parts[2] != "" {
		for _, role := range strings.Split(parts[2], ",") {
			if role = strings.TrimSpace(role); role != "" {
				roles = append(roles, role)
			}
		}
	}
	if len(roles) == 0 {
		roles = []string{"admin"}
	}
	return name, pass, roles, nil
}

// describeDB names the database for the startup banner.
func describeDB(path string) string {
	if path == "" || path == ":memory:" {
		return "an in-memory database"
	}
	return path
}
