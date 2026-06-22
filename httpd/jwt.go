package httpd

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/hmac"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"strings"
	"time"
)

// jwtB64 is the base64url-without-padding encoding JWT segments use (RFC 7515 §2).
var jwtB64 = base64.RawURLEncoding

// JWTConfig configures the built-in bearer-token provider (doc 18 §10.4). At least one
// verification key must be set; each enables the matching algorithm: HMACSecret enables
// HS256, RSAPublicKey enables RS256, ECDSAPublicKey enables ES256. A token signed with an
// algorithm whose key is not configured is rejected, so a deployment that sets only an
// RSA public key cannot be tricked into accepting an HS256 token signed with that public
// key as an HMAC secret (the classic JWT algorithm-confusion attack).
type JWTConfig struct {
	HMACSecret     []byte           // HS256 shared secret
	RSAPublicKey   *rsa.PublicKey   // RS256 verification key
	ECDSAPublicKey *ecdsa.PublicKey // ES256 verification key
	// Issuer, when set, must equal the token's iss claim.
	Issuer string
	// Audience, when set, must appear in the token's aud claim.
	Audience string
	// RolesClaim names the claim the principal's roles are read from; empty means "roles".
	RolesClaim string
	// Leeway allows for clock skew when checking exp and nbf; zero means no leeway.
	Leeway time.Duration
}

// JWTProvider verifies bearer tokens as JWTs and yields a principal from their claims (doc
// 18 §10.4). It is the built-in provider for the bearer scheme, the counterpart to the
// StaticProvider's basic scheme; a deployment that needs LDAP or an opaque-token service
// installs its own provider through the same seam. Verification is offline (signature plus
// the registered claims), so it adds no network dependency and stays zero-dependency:
// every algorithm is built on the standard library's crypto packages.
type JWTProvider struct {
	hmacSecret []byte
	rsaPub     *rsa.PublicKey
	ecdsaPub   *ecdsa.PublicKey
	issuer     string
	audience   string
	rolesClaim string
	leeway     time.Duration
	now        func() time.Time
}

// NewJWTProvider builds a bearer-token provider from a config (doc 18 §10.4). It errors
// when no verification key is configured, since a provider that can verify nothing would
// accept nothing and is a configuration mistake.
func NewJWTProvider(cfg JWTConfig) (*JWTProvider, error) {
	if cfg.HMACSecret == nil && cfg.RSAPublicKey == nil && cfg.ECDSAPublicKey == nil {
		return nil, errors.New("gr: jwt provider needs at least one verification key")
	}
	roles := cfg.RolesClaim
	if roles == "" {
		roles = "roles"
	}
	return &JWTProvider{
		hmacSecret: cfg.HMACSecret,
		rsaPub:     cfg.RSAPublicKey,
		ecdsaPub:   cfg.ECDSAPublicKey,
		issuer:     cfg.Issuer,
		audience:   cfg.Audience,
		rolesClaim: roles,
		leeway:     cfg.Leeway,
		now:        time.Now,
	}, nil
}

// Schemes reports that the JWT provider verifies the bearer scheme.
func (p *JWTProvider) Schemes() []string { return []string{"bearer"} }

// Authenticate validates a bearer token and returns the principal its claims name (doc 18
// §10.4). It handles only the bearer scheme; a basic credential is rejected, so a
// deployment that needs both schemes runs the matching provider for each transport. The
// subject becomes the principal name and the roles claim becomes the roles.
func (p *JWTProvider) Authenticate(ctx context.Context, scheme, principal string, credential []byte) (*Principal, error) {
	if scheme != "bearer" {
		return nil, ErrUnauthorized
	}
	claims, err := p.verify(credential)
	if err != nil {
		return nil, err
	}
	return &Principal{
		Name:  claims.Subject,
		Roles: append([]string(nil), claims.Roles...),
		Token: claims,
	}, nil
}

// verify checks a compact-serialized JWS and returns its claims (RFC 7519). It verifies
// the signature before it trusts any claim, so an unverified payload is never inspected,
// and it returns ErrUnauthorized for any structural or signature failure (non-disclosing)
// and ErrTokenExpired only for a token that verified but whose expiry has passed.
func (p *JWTProvider) verify(token []byte) (*Claims, error) {
	parts := strings.Split(string(token), ".")
	if len(parts) != 3 {
		return nil, ErrUnauthorized
	}
	headerJSON, err := jwtB64.DecodeString(parts[0])
	if err != nil {
		return nil, ErrUnauthorized
	}
	var header struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
	}
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		return nil, ErrUnauthorized
	}
	sig, err := jwtB64.DecodeString(parts[2])
	if err != nil {
		return nil, ErrUnauthorized
	}
	// The signing input is the header and payload segments joined by a dot, exactly as
	// they appear in the token (RFC 7515 §5.1), so re-encoding is never needed.
	if err := p.verifySignature(header.Alg, parts[0]+"."+parts[1], sig); err != nil {
		return nil, err
	}
	payloadJSON, err := jwtB64.DecodeString(parts[1])
	if err != nil {
		return nil, ErrUnauthorized
	}
	return p.parseClaims(payloadJSON)
}

// verifySignature checks the token signature for the named algorithm against the matching
// configured key (doc 18 §10.4). An algorithm whose key is not configured is rejected,
// which closes the algorithm-confusion attack where a token claims HS256 to have its
// signature checked as an HMAC over a public key.
func (p *JWTProvider) verifySignature(alg, signingInput string, sig []byte) error {
	switch alg {
	case "HS256":
		if p.hmacSecret == nil {
			return ErrUnauthorized
		}
		mac := hmac.New(sha256.New, p.hmacSecret)
		mac.Write([]byte(signingInput))
		if subtle.ConstantTimeCompare(sig, mac.Sum(nil)) != 1 {
			return ErrUnauthorized
		}
		return nil
	case "RS256":
		if p.rsaPub == nil {
			return ErrUnauthorized
		}
		sum := sha256.Sum256([]byte(signingInput))
		if rsa.VerifyPKCS1v15(p.rsaPub, crypto.SHA256, sum[:], sig) != nil {
			return ErrUnauthorized
		}
		return nil
	case "ES256":
		if p.ecdsaPub == nil || len(sig) != 64 {
			return ErrUnauthorized
		}
		r := new(big.Int).SetBytes(sig[:32])
		s := new(big.Int).SetBytes(sig[32:])
		sum := sha256.Sum256([]byte(signingInput))
		if !ecdsa.Verify(p.ecdsaPub, sum[:], r, s) {
			return ErrUnauthorized
		}
		return nil
	default:
		return ErrUnauthorized
	}
}

// parseClaims decodes the verified payload and checks the registered claims (RFC 7519
// §4.1). The expiry is required, so a token with no exp is rejected rather than treated as
// non-expiring; an expired token is the one ErrTokenExpired case, and a not-yet-valid
// token, a wrong issuer, or a wrong audience is a plain ErrUnauthorized.
func (p *JWTProvider) parseClaims(payload []byte) (*Claims, error) {
	var raw map[string]any
	if err := json.Unmarshal(payload, &raw); err != nil {
		return nil, ErrUnauthorized
	}
	c := &Claims{Raw: raw}
	if s, ok := raw["sub"].(string); ok {
		c.Subject = s
	}
	if s, ok := raw["iss"].(string); ok {
		c.Issuer = s
	}
	c.Audience = toStringSlice(raw["aud"])
	c.ExpiresAt = toTime(raw["exp"])
	c.NotBefore = toTime(raw["nbf"])
	c.IssuedAt = toTime(raw["iat"])
	c.Roles = toStringSlice(raw[p.rolesClaim])

	now := p.now()
	if c.ExpiresAt.IsZero() {
		return nil, ErrUnauthorized
	}
	if now.After(c.ExpiresAt.Add(p.leeway)) {
		return nil, ErrTokenExpired
	}
	if !c.NotBefore.IsZero() && now.Add(p.leeway).Before(c.NotBefore) {
		return nil, ErrUnauthorized
	}
	if p.issuer != "" && c.Issuer != p.issuer {
		return nil, ErrUnauthorized
	}
	if p.audience != "" && !containsString(c.Audience, p.audience) {
		return nil, ErrUnauthorized
	}
	return c, nil
}

// toTime converts a JSON numeric date claim (seconds since the Unix epoch, RFC 7519 §2)
// to a time.Time. A missing or non-numeric value yields the zero time.
func toTime(v any) time.Time {
	f, ok := v.(float64)
	if !ok {
		return time.Time{}
	}
	sec := int64(f)
	nsec := int64((f - float64(sec)) * 1e9)
	return time.Unix(sec, nsec).UTC()
}

// toStringSlice normalizes a claim that may be a single string or an array of strings
// (the aud claim is either, RFC 7519 §4.1.3) into a slice. A roles claim follows the same
// shape, so one helper covers both.
func toStringSlice(v any) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

// containsString reports whether s appears in xs.
func containsString(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// ParsePEMPublicKey parses a PEM-encoded RSA or ECDSA public key for a JWTConfig (doc 18
// §10.4). It accepts a PKIX public key (the common "PUBLIC KEY" block) and reports which
// kind it found, so a caller wiring a config from a key file does not need to know the
// algorithm in advance.
func ParsePEMPublicKey(pemBytes []byte) (rsaKey *rsa.PublicKey, ecdsaKey *ecdsa.PublicKey, err error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, nil, errors.New("gr: no PEM block in public key")
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, nil, err
	}
	switch k := key.(type) {
	case *rsa.PublicKey:
		return k, nil, nil
	case *ecdsa.PublicKey:
		return nil, k, nil
	default:
		return nil, nil, errors.New("gr: unsupported public key type, want RSA or ECDSA")
	}
}
