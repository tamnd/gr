package httpd

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tamnd/gr"
	"github.com/tamnd/gr/vfs"
)

// baseClock is the fixed instant the JWT tests run at, so token expiry is deterministic.
var baseClock = time.Unix(1_700_000_000, 0).UTC()

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// signingInput encodes a header and a claims set into the dotted signing input of a JWT.
func signingInput(t *testing.T, alg string, claims map[string]any) string {
	t.Helper()
	header := mustJSON(t, map[string]any{"alg": alg, "typ": "JWT"})
	return jwtB64.EncodeToString(header) + "." + jwtB64.EncodeToString(mustJSON(t, claims))
}

func mintHS256(t *testing.T, secret []byte, claims map[string]any) string {
	t.Helper()
	in := signingInput(t, "HS256", claims)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(in))
	return in + "." + jwtB64.EncodeToString(mac.Sum(nil))
}

func mintRS256(t *testing.T, key *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	in := signingInput(t, "RS256", claims)
	sum := sha256.Sum256([]byte(in))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatalf("rsa sign: %v", err)
	}
	return in + "." + jwtB64.EncodeToString(sig)
}

func mintES256(t *testing.T, key *ecdsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	in := signingInput(t, "ES256", claims)
	sum := sha256.Sum256([]byte(in))
	r, s, err := ecdsa.Sign(rand.Reader, key, sum[:])
	if err != nil {
		t.Fatalf("ecdsa sign: %v", err)
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return in + "." + jwtB64.EncodeToString(sig)
}

// claimsAt builds a standard claims set valid at baseClock with the given subject and roles.
func claimsAt(sub string, roles []string) map[string]any {
	return map[string]any{
		"sub":   sub,
		"exp":   float64(baseClock.Add(time.Hour).Unix()),
		"iat":   float64(baseClock.Unix()),
		"roles": roles,
	}
}

func hsProvider(t *testing.T, secret []byte) *JWTProvider {
	t.Helper()
	p, err := NewJWTProvider(JWTConfig{HMACSecret: secret})
	if err != nil {
		t.Fatalf("new jwt provider: %v", err)
	}
	p.now = func() time.Time { return baseClock }
	return p
}

func TestJWTValidHS256(t *testing.T) {
	secret := []byte("super-secret-signing-key")
	p := hsProvider(t, secret)
	tok := mintHS256(t, secret, claimsAt("alice", []string{"editor", "reader"}))

	princ, err := p.Authenticate(context.Background(), "bearer", "", []byte(tok))
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if princ.Name != "alice" {
		t.Errorf("name = %q, want alice", princ.Name)
	}
	if strings.Join(princ.Roles, ",") != "editor,reader" {
		t.Errorf("roles = %v", princ.Roles)
	}
	if princ.Token == nil || princ.Token.Subject != "alice" {
		t.Errorf("token claims not carried: %+v", princ.Token)
	}
}

func TestJWTExpired(t *testing.T) {
	secret := []byte("super-secret-signing-key")
	p := hsProvider(t, secret)
	claims := claimsAt("alice", []string{"reader"})
	claims["exp"] = float64(baseClock.Add(-time.Minute).Unix()) // already past
	tok := mintHS256(t, secret, claims)

	_, err := p.Authenticate(context.Background(), "bearer", "", []byte(tok))
	if !errors.Is(err, ErrTokenExpired) {
		t.Errorf("err = %v, want ErrTokenExpired", err)
	}
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("ErrTokenExpired should wrap ErrUnauthorized, got %v", err)
	}
}

func TestJWTNoExpiryRejected(t *testing.T) {
	secret := []byte("super-secret-signing-key")
	p := hsProvider(t, secret)
	claims := map[string]any{"sub": "alice", "roles": []string{"reader"}} // no exp
	tok := mintHS256(t, secret, claims)

	if _, err := p.Authenticate(context.Background(), "bearer", "", []byte(tok)); !errors.Is(err, ErrUnauthorized) || errors.Is(err, ErrTokenExpired) {
		t.Errorf("err = %v, want plain ErrUnauthorized", err)
	}
}

func TestJWTNotYetValid(t *testing.T) {
	secret := []byte("super-secret-signing-key")
	p := hsProvider(t, secret)
	claims := claimsAt("alice", []string{"reader"})
	claims["nbf"] = float64(baseClock.Add(time.Minute).Unix()) // future
	tok := mintHS256(t, secret, claims)

	if _, err := p.Authenticate(context.Background(), "bearer", "", []byte(tok)); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("err = %v, want ErrUnauthorized", err)
	}
}

func TestJWTLeewayAllowsSkew(t *testing.T) {
	secret := []byte("super-secret-signing-key")
	p, err := NewJWTProvider(JWTConfig{HMACSecret: secret, Leeway: 30 * time.Second})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	p.now = func() time.Time { return baseClock }
	claims := claimsAt("alice", []string{"reader"})
	claims["exp"] = float64(baseClock.Add(-10 * time.Second).Unix()) // just past, within leeway
	tok := mintHS256(t, secret, claims)

	if _, err := p.Authenticate(context.Background(), "bearer", "", []byte(tok)); err != nil {
		t.Errorf("within leeway err = %v, want success", err)
	}
}

func TestJWTIssuerAndAudience(t *testing.T) {
	secret := []byte("super-secret-signing-key")
	p, err := NewJWTProvider(JWTConfig{HMACSecret: secret, Issuer: "gr-issuer", Audience: "gr-api"})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	p.now = func() time.Time { return baseClock }

	good := claimsAt("alice", []string{"reader"})
	good["iss"] = "gr-issuer"
	good["aud"] = []any{"other", "gr-api"} // array form, must contain the audience
	if _, err := p.Authenticate(context.Background(), "bearer", "", []byte(mintHS256(t, secret, good))); err != nil {
		t.Errorf("matching iss/aud err = %v, want success", err)
	}

	wrongIss := claimsAt("alice", []string{"reader"})
	wrongIss["iss"] = "evil"
	wrongIss["aud"] = "gr-api"
	if _, err := p.Authenticate(context.Background(), "bearer", "", []byte(mintHS256(t, secret, wrongIss))); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("wrong issuer err = %v, want ErrUnauthorized", err)
	}

	wrongAud := claimsAt("alice", []string{"reader"})
	wrongAud["iss"] = "gr-issuer"
	wrongAud["aud"] = "someone-else"
	if _, err := p.Authenticate(context.Background(), "bearer", "", []byte(mintHS256(t, secret, wrongAud))); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("wrong audience err = %v, want ErrUnauthorized", err)
	}
}

func TestJWTBadSignature(t *testing.T) {
	p := hsProvider(t, []byte("the-real-secret"))
	tok := mintHS256(t, []byte("a-different-secret"), claimsAt("alice", []string{"reader"}))
	if _, err := p.Authenticate(context.Background(), "bearer", "", []byte(tok)); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("err = %v, want ErrUnauthorized", err)
	}
}

func TestJWTMalformed(t *testing.T) {
	p := hsProvider(t, []byte("secret"))
	for _, tok := range []string{"", "a.b", "not-a-token", "a.b.c.d", "...", "x.y.z"} {
		if _, err := p.Authenticate(context.Background(), "bearer", "", []byte(tok)); !errors.Is(err, ErrUnauthorized) {
			t.Errorf("token %q err = %v, want ErrUnauthorized", tok, err)
		}
	}
}

func TestJWTAlgorithmConfusion(t *testing.T) {
	// Provider configured with only an RSA public key must reject an HS256 token, even
	// one whose HMAC was computed over the public key bytes (the classic attack).
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("gen rsa: %v", err)
	}
	p, err := NewJWTProvider(JWTConfig{RSAPublicKey: &key.PublicKey})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	p.now = func() time.Time { return baseClock }
	pub := pkixPEM(t, &key.PublicKey)
	tok := mintHS256(t, pub, claimsAt("attacker", []string{"admin"}))
	if _, err := p.Authenticate(context.Background(), "bearer", "", []byte(tok)); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("HS256 against RSA-only provider err = %v, want ErrUnauthorized", err)
	}
}

func TestJWTRS256(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("gen rsa: %v", err)
	}
	p, err := NewJWTProvider(JWTConfig{RSAPublicKey: &key.PublicKey})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	p.now = func() time.Time { return baseClock }
	tok := mintRS256(t, key, claimsAt("bob", []string{"publisher"}))
	princ, err := p.Authenticate(context.Background(), "bearer", "", []byte(tok))
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if princ.Name != "bob" || strings.Join(princ.Roles, ",") != "publisher" {
		t.Errorf("principal = %+v", princ)
	}
}

func TestJWTES256(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen ecdsa: %v", err)
	}
	p, err := NewJWTProvider(JWTConfig{ECDSAPublicKey: &key.PublicKey})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	p.now = func() time.Time { return baseClock }
	tok := mintES256(t, key, claimsAt("carol", []string{"editor"}))
	princ, err := p.Authenticate(context.Background(), "bearer", "", []byte(tok))
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if princ.Name != "carol" || strings.Join(princ.Roles, ",") != "editor" {
		t.Errorf("principal = %+v", princ)
	}
}

func TestNewJWTProviderNeedsKey(t *testing.T) {
	if _, err := NewJWTProvider(JWTConfig{}); err == nil {
		t.Errorf("expected an error with no verification key")
	}
}

func TestParsePEMPublicKey(t *testing.T) {
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	rPub, ePub, err := ParsePEMPublicKey(pkixPEM(t, &rsaKey.PublicKey))
	if err != nil || rPub == nil || ePub != nil {
		t.Errorf("rsa parse: r=%v e=%v err=%v", rPub != nil, ePub != nil, err)
	}
	rPub, ePub, err = ParsePEMPublicKey(pkixPEM(t, &ec.PublicKey))
	if err != nil || ePub == nil || rPub != nil {
		t.Errorf("ecdsa parse: r=%v e=%v err=%v", rPub != nil, ePub != nil, err)
	}
	if _, _, err := ParsePEMPublicKey([]byte("not pem")); err == nil {
		t.Errorf("expected an error for non-PEM input")
	}
}

// TestJWTOverHTTP drives a bearer token end to end: a token with the editor role can write,
// a token with only the reader role is forbidden, and an expired token returns the distinct
// TokenExpired code.
func TestJWTOverHTTP(t *testing.T) {
	db, err := gr.Open(":memory:.gr", gr.Options{VFS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	secret := []byte("super-secret-signing-key")
	p := hsProvider(t, secret)
	sv := New(db, Options{Auth: p})

	post := func(token, stmt string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/db/neo4j/query/v2", strings.NewReader(`{"statement":`+jsonStr(stmt)+`}`))
		r.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		sv.ServeHTTP(rec, r)
		return rec
	}

	editor := mintHS256(t, secret, claimsAt("alice", []string{"editor"}))
	if rec := post(editor, "CREATE (:Person)"); rec.Code != http.StatusOK {
		t.Errorf("editor write = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}

	reader := mintHS256(t, secret, claimsAt("bob", []string{"reader"}))
	if rec := post(reader, "CREATE (:Person)"); rec.Code != http.StatusForbidden {
		t.Errorf("reader write = %d, want 403", rec.Code)
	}
	if rec := post(reader, "MATCH (n) RETURN count(n)"); rec.Code != http.StatusOK {
		t.Errorf("reader read = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}

	expired := claimsAt("alice", []string{"editor"})
	expired["exp"] = float64(baseClock.Add(-time.Minute).Unix())
	rec := post(mintHS256(t, secret, expired), "MATCH (n) RETURN n")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired token = %d, want 401", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Neo.ClientError.Security.TokenExpired") {
		t.Errorf("expired body missing TokenExpired code: %s", rec.Body.String())
	}
	if ch := rec.Header().Get("WWW-Authenticate"); !strings.HasPrefix(ch, "Bearer ") {
		t.Errorf("expired challenge = %q, want a Bearer challenge", ch)
	}
}

func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// pkixPEM marshals a public key to a PKIX PEM block for the parse tests.
func pkixPEM(t *testing.T, pub any) []byte {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatalf("marshal pkix: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}
