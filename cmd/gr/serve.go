package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/tamnd/gr"
	"github.com/tamnd/gr/bolt"
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

// defaultBoltAddr is the address the Bolt listener binds when --bolt is given with no
// --bolt-addr (doc 18 §1.4). 7687 is the Neo4j Bolt port, so a driver pointed at the
// usual port finds the server.
const defaultBoltAddr = ":7687"

// runServe implements the `gr serve` subcommand (doc 18 §9): it opens a database and
// serves the HTTP/JSON API over it until the process is stopped. It parses its own
// flag set rather than the shell's, since the serve options (the listen address, the
// database name in the URL path) do not overlap the shell's.
//
// listen is injected so a test can substitute a stub for net/http's ListenAndServe;
// the real entry point passes http.ListenAndServe. boltListen is the matching seam for
// the Bolt listener; the real entry point passes startBolt, a test passes a stub.
func runServe(args []string, stdout, stderr io.Writer, listen func(addr string, h http.Handler) error, boltListen func(addr string, h bolt.Handler) (io.Closer, error)) int {
	fs := flag.NewFlagSet("gr serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", defaultServeAddr, "address to listen on")
	name := fs.String("name", "neo4j", "database name in the URL path")
	readonly := fs.Bool("readonly", false, "open the database read-only")
	boltEnabled := fs.Bool("bolt", false, "also serve the Bolt protocol so Neo4j drivers can connect")
	boltAddr := fs.String("bolt-addr", defaultBoltAddr, "address the Bolt listener binds when --bolt is set")
	var users userList
	fs.Var(&users, "user", "name:password[:roles] for HTTP auth (repeatable); roles is a comma list of reader/editor/publisher/admin, default admin; none means auth is off")
	authStore := fs.Bool("auth-store", false, "authenticate against the database's own credential store (the users created with CreateUser), not an in-memory --user list")
	var jwt jwtOptions
	fs.StringVar(&jwt.hmacSecret, "jwt-hmac-secret", "", "HS256 shared secret for bearer-token (JWT) auth; selects the JWT provider")
	fs.StringVar(&jwt.pubKeyPath, "jwt-pubkey", "", "path to a PEM RSA or ECDSA public key for RS256/ES256 bearer-token auth")
	fs.StringVar(&jwt.issuer, "jwt-issuer", "", "required token issuer (iss claim) for bearer-token auth")
	fs.StringVar(&jwt.audience, "jwt-audience", "", "required token audience (aud claim) for bearer-token auth")
	fs.StringVar(&jwt.rolesClaim, "jwt-roles-claim", "", "token claim to read roles from (default roles)")
	tokenCacheTTL := fs.Duration("auth-token-cache-ttl", 0, "how long a validated bearer token is cached before revalidation (0 uses the default)")
	impersonation := fs.Bool("auth-impersonation", false, "allow an admin to run a query as another user via the impersonatedUser request field")
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

	auth, err := buildAuth(db, users, jwt, *authStore)
	if err != nil {
		fmt.Fprintln(stderr, "gr:", err)
		return exitUsage
	}
	if *impersonation && auth == nil {
		fmt.Fprintln(stderr, "gr: --auth-impersonation needs an auth provider; pass --user, --auth-store, or --jwt-*")
		return exitUsage
	}
	srv := httpd.New(db, httpd.Options{Name: *name, Auth: auth, TokenCacheTTL: *tokenCacheTTL, Impersonation: *impersonation})
	defer srv.Close()
	fmt.Fprintf(stderr, "gr serving %s on %s (database %q)\n", describeDB(path), *addr, *name)
	if auth == nil {
		fmt.Fprintln(stderr, "gr: WARNING authentication is off, every request is anonymous; pass --user or --auth-store to require auth")
	}

	// Serve Bolt alongside HTTP over the same database when asked, so a Neo4j driver
	// can connect to the same data the HTTP surface serves (doc 18 §5, §11.4). Auth is
	// the same provider as HTTP, so a deployment configures it once (doc 18 §10.4). The
	// listener runs in the background; closing it on exit drains its connections.
	if *boltEnabled {
		bh := db.BoltHandler(boltAuthOptions(auth)...)
		closer, err := boltListen(*boltAddr, bh)
		if err != nil {
			fmt.Fprintln(stderr, "gr:", err)
			return exitIO
		}
		defer func() { _ = closer.Close() }()
		fmt.Fprintf(stderr, "gr serving Bolt on %s\n", *boltAddr)
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
// given so authentication is off. The --auth-store flag selects the database's own
// persistent credential store (the users created with CreateUser, doc 18 §10.3). The
// --user flags select the static basic-credential provider (name:password[:roles], roles a
// comma list of reader/editor/publisher/admin, default admin so a single --user keeps full
// access). The --jwt-* flags select the JWT bearer provider instead. The three are mutually
// exclusive, since the server runs one provider at a time (doc 18 §10.7).
func buildAuth(db *gr.DB, users userList, jwt jwtOptions, authStore bool) (httpd.AuthProvider, error) {
	selected := 0
	if authStore {
		selected++
	}
	if len(users) > 0 {
		selected++
	}
	if jwt.set() {
		selected++
	}
	switch {
	case selected > 1:
		return nil, fmt.Errorf("choose one auth provider: --auth-store, --user (basic), or --jwt-* (bearer), not more than one")
	case authStore:
		return httpd.NewDBProvider(db), nil
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

// boltAuthOptions builds the Bolt handler options that match the HTTP auth posture. With
// an auth provider configured, Bolt routes authentication through the same provider so
// both transports enforce one credential source (doc 18 §10.4); with none, Bolt accepts
// anonymous connections, matching the anonymous HTTP surface (doc 18 §11.4).
func boltAuthOptions(auth httpd.AuthProvider) []gr.BoltOption {
	if auth == nil {
		return nil
	}
	return []gr.BoltOption{gr.WithBoltAuthFunc(boltAuthFunc(auth))}
}

// boltAuthFunc adapts an HTTP auth provider to the Bolt handler's auth seam (doc 18
// §10.4). The Bolt "basic" scheme carries the principal and password, "bearer" carries a
// token in the credentials with no principal; both route to the provider's Authenticate.
// The "none" scheme is rejected, since reaching here means auth is required. The
// authenticated principal's roles are returned so the Bolt path authorizes each statement
// against the same role model as HTTP (doc 18 §10.6).
func boltAuthFunc(auth httpd.AuthProvider) func(scheme, principal, credentials string) ([]string, error) {
	return func(scheme, principal, credentials string) ([]string, error) {
		if scheme == "" || scheme == "none" {
			return nil, errors.New("authentication required")
		}
		p, err := auth.Authenticate(context.Background(), scheme, principal, []byte(credentials))
		if err != nil {
			return nil, err
		}
		return p.Roles, nil
	}
}

// startBolt binds the Bolt listen address and serves the protocol in the background,
// returning the listener so the caller can close it on shutdown. It is the real
// boltListen the serve command injects; a test substitutes a stub.
func startBolt(addr string, h bolt.Handler) (io.Closer, error) {
	netLn, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	ln := &bolt.Listener{Server: &bolt.Server{Handler: h}}
	go func() { _ = ln.Serve(netLn) }()
	return boltCloser{ln}, nil
}

// boltCloser shuts a Bolt listener down with a short drain deadline when the serve
// command exits, so a lingering connection cannot block the process from stopping.
type boltCloser struct {
	ln *bolt.Listener
}

func (c boltCloser) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return c.ln.Shutdown(ctx)
}

// describeDB names the database for the startup banner.
func describeDB(path string) string {
	if path == "" || path == ":memory:" {
		return "an in-memory database"
	}
	return path
}
