package httpd

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/tamnd/gr"
)

// Version is the HTTP API's reported server version (doc 18 §9.7). It tracks the gr
// release, not a Neo4j version, but it is reported in the Neo4j discovery shape so a
// tool that probes / to learn the API works.
const Version = "0.1.0"

// server holds the database the handler serves and the configured database name (doc
// 18 §7.5). A gr server holds one database; the name in a request path is validated
// against this and otherwise carries no routing meaning.
type server struct {
	db          *gr.DB
	name        string
	txns        *txStore
	now         func() time.Time
	txTimeout   time.Duration
	auth        AuthProvider
	bearerCache *tokenCache
	metrics     *metrics
	// admission is the shared in-flight-query gate (doc 18 §8.8); nil leaves queries
	// ungated.
	admission *gr.Admission
	// queryMaxTime is the server-wide wall-clock cap on a single query (doc 18 §8.6);
	// zero leaves queries uncapped except for any per-request maxExecutionTime.
	queryMaxTime time.Duration
	// impersonation enables the imp_user/impersonatedUser request field (doc 18 §10.5).
	// It is off by default, so a deployment opts into impersonation explicitly.
	impersonation bool
}

// Options configures the handler.
type Options struct {
	// Name is the database name the path /db/{name}/... must use. Empty defaults to
	// "neo4j", the Neo4j default, so an unconfigured driver's path works.
	Name string
	// TxTimeout bounds how long a server-side HTTP transaction may sit idle between
	// requests before it is reaped. Zero uses defaultTxTimeout.
	TxTimeout time.Duration
	// Auth verifies credentials per request. Nil disables authentication, so every
	// request is anonymous; set it (for example to a StaticProvider) to require auth.
	Auth AuthProvider
	// TokenCacheTTL bounds how long a validated bearer token is cached before it is
	// revalidated (doc 18 §10.4). Zero uses defaultTokenCacheTTL; the cache is only
	// consulted when Auth is set, since an anonymous server has no token to cache.
	TokenCacheTTL time.Duration
	// Impersonation enables the impersonatedUser request field (doc 18 §10.5), which
	// lets an admin run a query as another user. It is off by default; turning it on
	// requires an auth provider that resolves users (a RoleResolver), since an anonymous
	// server has no principal to authorize the impersonation against.
	Impersonation bool
	// Admission is the shared in-flight-query gate (doc 18 §8.8). When set, every query
	// passes through it before executing, so the bound holds across both server surfaces
	// when the same gate is also given to the Bolt handler. Nil leaves the HTTP path
	// ungated.
	Admission *gr.Admission
	// QueryMaxTime is a server-wide wall-clock cap on a single query (doc 18 §8.6). When
	// set, a query runs under the smaller of this cap and any per-request maxExecutionTime,
	// and a query that runs longer is cancelled and reported as a timed out transaction.
	// Pass the same value to the Bolt handler so both surfaces enforce one cap. Zero leaves
	// queries uncapped except for a per-request maxExecutionTime.
	QueryMaxTime time.Duration
	// now overrides the clock for tests; nil uses time.Now.
	now func() time.Time
}

// Server is the HTTP/JSON server over a database (doc 18 §9.1). It is an http.Handler so
// it mounts into any net/http server and is driven by httptest in the tests without
// binding a port, and it adds the lifecycle the bare handler cannot express: Sweep reaps
// expired transactions, which the serve command runs on a ticker, and Close rolls back
// any still-open transaction on shutdown.
type Server struct {
	s   *server
	mux *http.ServeMux
}

// New builds a Server over a database (doc 18 §9.1). It routes the discovery, health,
// metrics, auto-commit query, and transactional tx endpoints, and authenticates and
// authorizes the data endpoints when an auth provider is configured.
func New(db *gr.DB, opts Options) *Server {
	name := opts.Name
	if name == "" {
		name = "neo4j"
	}
	timeout := opts.TxTimeout
	if timeout == 0 {
		timeout = defaultTxTimeout
	}
	clock := opts.now
	if clock == nil {
		clock = time.Now
	}
	s := &server{db: db, name: name, txns: newTxStore(), now: clock, txTimeout: timeout, auth: opts.Auth, metrics: newMetrics(), admission: opts.Admission, queryMaxTime: opts.QueryMaxTime, impersonation: opts.Impersonation}
	if opts.Auth != nil {
		ttl := opts.TokenCacheTTL
		if ttl == 0 {
			ttl = defaultTokenCacheTTL
		}
		s.bearerCache = newTokenCache(ttl, clock)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/readyz", s.handleReadyz)
	mux.HandleFunc("/metrics", s.handleMetrics)
	mux.HandleFunc("/", s.route)
	return &Server{s: s, mux: mux}
}

// ServeHTTP routes a request, so a Server is an http.Handler.
func (sv *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { sv.mux.ServeHTTP(w, r) }

// Sweep reaps every server-side transaction past its expiry and returns the count (doc
// 18 §8.7). The serve command calls it on a ticker so a vanished client's transaction
// does not pin the writer until someone happens to touch its id.
func (sv *Server) Sweep(now time.Time) int { return sv.s.txns.sweep(now) }

// Close rolls back every still-open transaction (doc 18 §13.1). It is called on shutdown
// so no held transaction leaks a snapshot or a write intent past the server's life.
func (sv *Server) Close() { sv.s.txns.closeAll() }

// Handler builds the HTTP/JSON handler over a database (doc 18 §9.1) as a plain
// http.Handler, for a caller that does not need the Sweep/Close lifecycle. It is New
// with the concrete type erased.
func Handler(db *gr.DB, opts Options) http.Handler {
	return New(db, opts)
}

// route dispatches the path-parameterized endpoints (doc 18 §9.1). The discovery
// document is served at the root; a /db/{name}/query/v2 path routes to the query
// handler after the name is validated.
func (s *server) route(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" {
		s.handleDiscovery(w, r)
		return
	}
	name, rest, ok := parseDBPath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	// The data endpoints are authenticated; discovery and the health probes above are
	// not, so a load balancer or an operator can probe liveness without a credential.
	princ, fail := s.authenticate(r)
	if s.auth != nil {
		// Count the outcome only when authentication actually ran (a provider is
		// configured); with auth off every request is anonymous and is not an auth event.
		switch {
		case fail == nil:
			s.metrics.countAuth(authSuccess)
		case fail.lockout:
			s.metrics.countAuth(authLocked)
		default:
			s.metrics.countAuth(authFailure)
		}
	}
	if fail != nil {
		s.writeAuthError(w, fail)
		return
	}
	r = r.WithContext(withPrincipal(r.Context(), princ))
	if name != s.name && name != "data" {
		s.writeError(w, http.StatusNotFound, apiError{
			Code:    "Neo.ClientError.Database.DatabaseNotFound",
			Message: "no such database: " + name,
		})
		return
	}
	switch {
	case rest == "query/v2":
		s.handleQuery(w, r)
	case rest == "tx":
		s.routeTxBegin(w, r, name)
	case strings.HasPrefix(rest, "tx/"):
		s.routeTx(w, r, rest[len("tx/"):])
	default:
		http.NotFound(w, r)
	}
}

// routeTxBegin handles POST /db/{name}/tx (doc 18 §9.5).
func (s *server) routeTxBegin(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPost {
		s.writeError(w, http.StatusMethodNotAllowed, apiError{
			Code:    "Neo.ClientError.Request.Invalid",
			Message: "begin a transaction with POST",
		})
		return
	}
	s.handleTxBegin(w, r, name)
}

// routeTx dispatches the id-bearing tx paths (doc 18 §9.5): /tx/{id} runs (POST) or
// rolls back (DELETE), /tx/{id}/commit commits (POST). The id is the segment after
// tx/; a /commit suffix selects the commit handler.
func (s *server) routeTx(w http.ResponseWriter, r *http.Request, idPath string) {
	if id, ok := strings.CutSuffix(idPath, "/commit"); ok {
		if strings.Contains(id, "/") || id == "" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			s.writeError(w, http.StatusMethodNotAllowed, apiError{
				Code:    "Neo.ClientError.Request.Invalid",
				Message: "commit with POST",
			})
			return
		}
		s.handleTxCommit(w, r, id)
		return
	}
	if strings.Contains(idPath, "/") || idPath == "" {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodPost:
		s.handleTxRun(w, r, idPath)
	case http.MethodDelete:
		s.handleTxRollback(w, r, idPath)
	default:
		s.writeError(w, http.StatusMethodNotAllowed, apiError{
			Code:    "Neo.ClientError.Request.Invalid",
			Message: "run with POST or roll back with DELETE",
		})
	}
}

// parseDBPath splits /db/{name}/{rest...} into the database name and the remaining
// path. It returns ok=false for any path that is not under /db/{name}/.
func parseDBPath(path string) (name, rest string, ok bool) {
	const prefix = "/db/"
	if !strings.HasPrefix(path, prefix) {
		return "", "", false
	}
	tail := path[len(prefix):]
	slash := strings.IndexByte(tail, '/')
	if slash < 0 {
		return "", "", false
	}
	return tail[:slash], tail[slash+1:], true
}

// handleDiscovery serves GET / (doc 18 §9.7): the server version and the query and
// transaction endpoint templates, the shape a tool probing / expects.
func (s *server) handleDiscovery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	doc := map[string]any{
		"gr_version":          Version,
		"neo4j_version":       "5.0.0",
		"neo4j_edition":       "community",
		"query":               "/db/" + s.name + "/query/v2",
		"transaction":         "/db/" + s.name + "/tx",
		"db/cluster/overview": nil,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(doc)
}

// handleHealthz serves GET /healthz (doc 18 §9.7): 200 while the process is alive.
func (s *server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// handleReadyz serves GET /readyz (doc 18 §9.7): 200 when the engine is open and
// accepting queries, 503 once it is closed.
func (s *server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if _, err := s.db.Labels(); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("not ready\n"))
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ready\n"))
}

// writeError writes a JSON error body in the doc 18 §9.3 shape with the given status and
// records the error by its Neo4j status code for the metrics endpoint (doc 18 §13.5).
func (s *server) writeError(w http.ResponseWriter, status int, ae apiError) {
	s.metrics.countError(ae.Code)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"errors": []apiError{ae}})
}
