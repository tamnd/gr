package httpd

import (
	"math"
	"net"
	"net/http"
	"strconv"
)

// rateLimit charges one query against the request's rate-limit budget (doc 18 §8.8). It
// returns true when the request may proceed; when the budget is spent it writes 429 Too
// Many Requests with a Retry-After header and a transient status code and returns false,
// so a driver backs off and retries. A nil limiter always allows, so the call is safe on
// an unlimited server.
func (s *server) rateLimit(w http.ResponseWriter, r *http.Request) bool {
	ok, wait := s.limiter.Allow(rateLimitKey(r))
	if ok {
		return true
	}
	// Retry-After is whole seconds, rounded up so a sub-second wait still asks for at
	// least one second rather than zero (which a client reads as "retry immediately").
	secs := int(math.Ceil(wait.Seconds()))
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	s.writeError(w, http.StatusTooManyRequests, apiError{
		Code:    "Neo.TransientError.General.TransientError",
		Message: "query rate limit exceeded, retry shortly",
	})
	return false
}

// rateLimitKey is the bucket key a request charges against (doc 18 §8.8): the
// authenticated principal when there is one (a per-token limit) or the request's source
// address when the request is anonymous (a per-connection limit). The two namespaces are
// kept apart with a prefix so a user named after an IP cannot share a bucket with that
// address.
func rateLimitKey(r *http.Request) string {
	if p := principalFrom(r.Context()); p != nil && p.Name != "" {
		return "user:" + p.Name
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return "ip:" + host
}
