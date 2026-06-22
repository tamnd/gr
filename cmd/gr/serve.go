package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
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
// the real entry point passes http.ListenAndServe. boltServe is the matching seam for
// the Bolt listener; serve builds the fully configured listener (address, TLS posture)
// and boltServe runs it, so a test can inspect the configuration without binding a port.
// The real entry point passes startBolt, a test passes a stub.
func runServe(args []string, stdout, stderr io.Writer, listen func(addr string, h http.Handler, tlsConf *tls.Config) error, boltServe func(ln *bolt.Listener) (io.Closer, error)) int {
	fs := flag.NewFlagSet("gr serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", defaultServeAddr, "address to listen on")
	name := fs.String("name", "neo4j", "database name in the URL path")
	readonly := fs.Bool("readonly", false, "open the database read-only")
	boltEnabled := fs.Bool("bolt", false, "also serve the Bolt protocol so Neo4j drivers can connect")
	boltAddr := fs.String("bolt-addr", defaultBoltAddr, "address the Bolt listener binds when --bolt is set")
	boltTLS := fs.String("bolt-tls", "disabled", "Bolt transport security: disabled (plaintext), optional (sniff TLS or plaintext), or required (TLS only); optional and required need --tls-cert and --tls-key")
	httpTLS := fs.String("http-tls", "disabled", "HTTP transport security: disabled (plaintext) or required (HTTPS only); required needs --tls-cert and --tls-key")
	tlsCert := fs.String("tls-cert", "", "path to the PEM certificate chain for TLS listeners")
	tlsKey := fs.String("tls-key", "", "path to the PEM private key for TLS listeners")
	maxInFlight := fs.Int("max-in-flight", 0, "bound on queries executing at once across all connections; 0 means unlimited")
	queryMaxTime := fs.Duration("query-max-time", 0, "server-wide wall-clock cap on a single query; a query that runs longer is cancelled and reported as timed out; 0 means no cap")
	maxQPS := fs.Float64("max-queries-per-second", 0, "per-principal query rate limit; a principal over the rate is refused with a retry hint; 0 means no limit")
	rateBurst := fs.Int("rate-limit-burst", 0, "momentary burst a principal may make above the steady rate; defaults to the per-second rate rounded up when --max-queries-per-second is set")
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
	queryLog := fs.String("query-log", "none", "structured query log level: none (off), off (slow queries only), errors, slow (failures and slow queries), or all")
	queryLogFormat := fs.String("query-log-format", "json", "query log format: json or logfmt")
	queryLogRedact := fs.String("query-log-redact", "all", "query parameter redaction: all (keys and types only), hashed (stable value hashes), or none (verbatim values)")
	queryLogSlow := fs.Duration("query-log-slow", 0, "slow-query threshold; a query slower than this is logged regardless of level (0 uses the default of one second)")
	eventLog := fs.String("log", "none", "structured event log: none (off) or on (open, close, auth failures, overload, and the rest of the taxonomy)")
	eventLogFormat := fs.String("log-format", "json", "event log format: json or logfmt")
	eventLogLevel := fs.String("log-level", "info", "event log severity threshold: trace, debug, info, warn, or error")
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

	// The event log is built before the database opens so the open event is the first
	// entry it records (doc 20 §11.3). A level of none returns a nil log, which records
	// nothing. It writes to stderr through slog, alongside the startup banner.
	elog, err := buildEventLog(stderr, *eventLog, *eventLogFormat, *eventLogLevel)
	if err != nil {
		fmt.Fprintln(stderr, "gr:", err)
		return exitUsage
	}

	path := fs.Arg(0)
	db, err := openServeDB(path, *readonly, elog)
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
	httpTLSConf, err := httpTLSPosture(*httpTLS, *tlsCert, *tlsKey)
	if err != nil {
		fmt.Fprintln(stderr, "gr:", err)
		return exitUsage
	}
	// One admission gate is shared by both transports, so the in-flight bound holds
	// across the whole process rather than per surface (doc 18 §8.8). A zero limit
	// returns a nil gate, which admits every query.
	admission := gr.NewAdmission(*maxInFlight, 0)
	// One rate limiter is shared by both transports too, so the per-principal bound holds
	// across the whole process (doc 18 §8.8). The burst defaults to the per-second rate
	// when not set, so a steady client at the rate is never throttled by a too-small
	// bucket. A zero rate returns a nil limiter, which allows every query.
	burst := *rateBurst
	if burst <= 0 && *maxQPS > 0 {
		burst = int(math.Ceil(*maxQPS))
	}
	limiter := gr.NewRateLimiter(*maxQPS, burst)
	// One query log is shared by both transports, so a query reads the same whether it
	// arrived over HTTP or Bolt (doc 20 §10). A level of none returns a nil log, which
	// records nothing. The log writes to stderr through slog, alongside the startup banner.
	qlog, err := buildQueryLog(stderr, *queryLog, *queryLogFormat, *queryLogRedact, *queryLogSlow)
	if err != nil {
		fmt.Fprintln(stderr, "gr:", err)
		return exitUsage
	}
	// Link the query log to the event log so a slow or failed query also raises the lighter
	// query_slow/query_error event an operator alerts on (doc 20 §11.3), independent of the
	// query-log level.
	qlog.WithEvents(elog)
	srv := httpd.New(db, httpd.Options{Name: *name, Auth: auth, TokenCacheTTL: *tokenCacheTTL, Impersonation: *impersonation, Admission: admission, QueryMaxTime: *queryMaxTime, RateLimiter: limiter, QueryLog: qlog, EventLog: elog})
	defer srv.Close()
	fmt.Fprintf(stderr, "gr serving %s on %s (database %q, TLS %s)\n", describeDB(path), *addr, *name, *httpTLS)
	if auth == nil {
		fmt.Fprintln(stderr, "gr: WARNING authentication is off, every request is anonymous; pass --user or --auth-store to require auth")
	}

	// Serve Bolt alongside HTTP over the same database when asked, so a Neo4j driver
	// can connect to the same data the HTTP surface serves (doc 18 §5, §11.4). Auth is
	// the same provider as HTTP, so a deployment configures it once (doc 18 §10.4). The
	// listener runs in the background; closing it on exit drains its connections.
	if *boltEnabled {
		mode, tlsConf, err := boltTLSPosture(*boltTLS, *tlsCert, *tlsKey)
		if err != nil {
			fmt.Fprintln(stderr, "gr:", err)
			return exitUsage
		}
		bh := db.BoltHandler(append(boltAuthOptions(auth), gr.WithBoltAdmission(admission), gr.WithBoltQueryMaxTime(*queryMaxTime), gr.WithBoltRateLimiter(limiter), gr.WithBoltQueryLog(qlog))...)
		ln := &bolt.Listener{
			Server:    &bolt.Server{Handler: bh},
			Addr:      *boltAddr,
			TLSMode:   mode,
			TLSConfig: tlsConf,
		}
		closer, err := boltServe(ln)
		if err != nil {
			fmt.Fprintln(stderr, "gr:", err)
			return exitIO
		}
		defer func() { _ = closer.Close() }()
		fmt.Fprintf(stderr, "gr serving Bolt on %s (TLS %s)\n", *boltAddr, *boltTLS)
	}

	// Reap transactions whose client vanished, so a dead HTTP client cannot pin the
	// writer until someone touches its id (doc 18 §8.7). The ticker stops when the
	// listener returns, so the goroutine does not outlive the server.
	stop := make(chan struct{})
	go sweepLoop(srv, stop)
	err = listen(*addr, srv, httpTLSConf)
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
func openServeDB(path string, readonly bool, elog *gr.EventLog) (*gr.DB, error) {
	if path == "" || path == ":memory:" {
		return gr.Open(memPath, gr.Options{VFS: vfs.NewMem(), EventLog: elog})
	}
	if _, err := os.Stat(path); err != nil && readonly {
		return nil, fmt.Errorf("cannot open a read-only database that does not exist: %s", path)
	}
	return gr.Open(path, gr.Options{ReadOnly: readonly, EventLog: elog})
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

// buildQueryLog turns the --query-log flags into a shared query log, or nil when the level
// is none so no log is constructed (doc 20 §10). The level off still constructs a log, since
// the slow-query subset fires even at off; only none means truly nothing. The format selects
// the slog handler (JSON or logfmt), the redaction selects the parameter policy, and the
// slow threshold marks the always-on slow tail.
func buildQueryLog(w io.Writer, level, format, redact string, slow time.Duration) (*gr.QueryLog, error) {
	if level == "none" || level == "" {
		return nil, nil
	}
	var lvl gr.QueryLogLevel
	switch level {
	case "off":
		lvl = gr.QueryLogOff
	case "errors":
		lvl = gr.QueryLogErrors
	case "slow":
		lvl = gr.QueryLogSlow
	case "all":
		lvl = gr.QueryLogAll
	default:
		return nil, fmt.Errorf("invalid --query-log %q, want none, off, errors, slow, or all", level)
	}
	var pol gr.RedactPolicy
	switch redact {
	case "all", "":
		pol = gr.RedactAll
	case "hashed":
		pol = gr.RedactHashed
	case "none":
		pol = gr.RedactNone
	default:
		return nil, fmt.Errorf("invalid --query-log-redact %q, want all, hashed, or none", redact)
	}
	var handler slog.Handler
	switch format {
	case "json", "":
		handler = slog.NewJSONHandler(w, nil)
	case "logfmt":
		handler = slog.NewTextHandler(w, nil)
	default:
		return nil, fmt.Errorf("invalid --query-log-format %q, want json or logfmt", format)
	}
	return gr.NewQueryLog(slog.New(handler), lvl, pol, slow), nil
}

// buildEventLog turns the --log flags into an event log, or nil when the level is none so
// no log is constructed (doc 20 §11). The format selects the slog handler (JSON or
// logfmt), and the level sets the handler's severity threshold, which gates every event:
// an event below it costs only the call. trace is gr's own level below slog's debug.
func buildEventLog(w io.Writer, mode, format, level string) (*gr.EventLog, error) {
	if mode == "none" || mode == "" {
		return nil, nil
	}
	if mode != "on" {
		return nil, fmt.Errorf("invalid --log %q, want none or on", mode)
	}
	var lvl slog.Level
	switch level {
	case "trace":
		lvl = gr.LevelTrace
	case "debug":
		lvl = slog.LevelDebug
	case "info", "":
		lvl = slog.LevelInfo
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		return nil, fmt.Errorf("invalid --log-level %q, want trace, debug, info, warn, or error", level)
	}
	opts := &slog.HandlerOptions{Level: lvl}
	var handler slog.Handler
	switch format {
	case "json", "":
		handler = slog.NewJSONHandler(w, opts)
	case "logfmt":
		handler = slog.NewTextHandler(w, opts)
	default:
		return nil, fmt.Errorf("invalid --log-format %q, want json or logfmt", format)
	}
	return gr.NewEventLog(slog.New(handler)), nil
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

// startBolt binds the configured listener's address and serves the protocol in the
// background, returning a closer so the caller can drain it on shutdown. Binding here
// rather than inside Serve surfaces an address-in-use error synchronously, so serve can
// report it and exit instead of failing silently on a background goroutine. It is the
// real boltServe the serve command injects; a test substitutes a stub.
func startBolt(ln *bolt.Listener) (io.Closer, error) {
	addr := ln.Addr
	if addr == "" {
		addr = defaultBoltAddr
	}
	netLn, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	go func() { _ = ln.Serve(netLn) }()
	return boltCloser{ln}, nil
}

// boltTLSPosture turns the --bolt-tls flag into a listener TLS mode and, for the encrypted
// postures, the loaded server configuration (doc 18 §11.1). The disabled posture serves
// plaintext and ignores any certificate material; optional and required both need a
// certificate and key, since optional still serves TLS when a client offers a ClientHello.
func boltTLSPosture(mode, certPath, keyPath string) (bolt.TLSMode, *tls.Config, error) {
	switch mode {
	case "disabled", "":
		return bolt.TLSDisabled, nil, nil
	case "optional", "required":
		conf, err := loadServerTLS(certPath, keyPath)
		if err != nil {
			return bolt.TLSDisabled, nil, err
		}
		if mode == "optional" {
			return bolt.TLSOptional, conf, nil
		}
		return bolt.TLSRequired, conf, nil
	default:
		return bolt.TLSDisabled, nil, fmt.Errorf("invalid --bolt-tls %q, want disabled, optional, or required", mode)
	}
}

// httpTLSPosture turns the --http-tls flag into the HTTP listener's TLS configuration
// (doc 18 §11.2). Unlike Bolt there is no sniffing posture: HTTP TLS is decided by the
// port, so the choice is plaintext or HTTPS. The disabled posture returns a nil config so
// the listener serves plaintext; required loads the shared certificate material.
func httpTLSPosture(mode, certPath, keyPath string) (*tls.Config, error) {
	switch mode {
	case "disabled", "":
		return nil, nil
	case "required":
		return loadServerTLS(certPath, keyPath)
	default:
		return nil, fmt.Errorf("invalid --http-tls %q, want disabled or required", mode)
	}
}

// startHTTP serves the HTTP handler at addr, plaintext when tlsConf is nil and HTTPS when
// it is set. It is the real listen the serve command injects; a test substitutes a stub.
// The certificate and key already live in tlsConf, so ListenAndServeTLS takes empty paths.
func startHTTP(addr string, h http.Handler, tlsConf *tls.Config) error {
	if tlsConf == nil {
		return http.ListenAndServe(addr, h)
	}
	srv := &http.Server{Addr: addr, Handler: h, TLSConfig: tlsConf}
	return srv.ListenAndServeTLS("", "")
}

// loadServerTLS loads the certificate chain and private key and returns a server
// configuration with the hardening defaults of doc 18 §11.3: TLS 1.2 minimum with 1.3
// preferred, and AEAD cipher suites only (no CBC, RC4, or export ciphers). The cipher
// list applies to TLS 1.2; TLS 1.3 negotiates its own AEAD suites regardless.
func loadServerTLS(certPath, keyPath string) (*tls.Config, error) {
	if certPath == "" || keyPath == "" {
		return nil, errors.New("TLS needs both --tls-cert and --tls-key")
	}
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("load TLS certificate: %w", err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
		CipherSuites: []uint16{
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,
			tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,
		},
	}, nil
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
