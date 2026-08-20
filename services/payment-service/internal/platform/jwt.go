package platform

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

// RS256 JWT verification against a cached JWKS (docs/CONTRACTS.md §7).
//
// Written by hand rather than pulled from a library for one reason: the
// verification rules here are the security boundary of the whole platform, and
// they are short enough to read in full. Every check below exists because
// skipping it is a known, named attack.
//
// The signature algorithm is pinned to RS256 in code. Trusting the token's own
// `alg` header is the alg-confusion vulnerability — a token claiming `none`,
// or claiming HS256 and signing with the public key as the HMAC secret, both
// verify successfully against a naive implementation.

var (
	ErrTokenMalformed   = errors.New("token is malformed")
	ErrTokenExpired     = errors.New("token has expired")
	ErrTokenNotYetValid = errors.New("token is not valid yet")
	ErrTokenSignature   = errors.New("token signature is invalid")
	ErrTokenAlgorithm   = errors.New("token algorithm is not permitted")
	ErrTokenIssuer      = errors.New("token issuer is not trusted")
	ErrTokenAudience    = errors.New("token audience does not include this service")
	ErrKeyUnknown       = errors.New("token was signed with an unknown key")
)

type JWKSVerifier struct {
	jwksURL  string
	issuer   string
	audience string
	client   *http.Client

	mu        sync.RWMutex
	keys      map[string]*rsa.PublicKey
	fetchedAt time.Time
	ttl       time.Duration

	// Collapses a stampede of concurrent refreshes into one HTTP call. Without
	// it, a key rotation makes every in-flight request fetch the JWKS at once.
	refresh sync.Mutex

	// Leeway absorbs clock skew between pods. 60s is the usual compromise:
	// enough for NTP drift, short enough that a stolen token's window is not
	// meaningfully extended.
	leeway time.Duration
}

func NewJWKSVerifier(jwksURL, issuer, audience string) *JWKSVerifier {
	return &JWKSVerifier{
		jwksURL:  jwksURL,
		issuer:   issuer,
		audience: audience,
		client:   &http.Client{Timeout: 3 * time.Second},
		keys:     map[string]*rsa.PublicKey{},
		ttl:      5 * time.Minute,
		leeway:   60 * time.Second,
	}
}

func (v *JWKSVerifier) Verify(token string) (*Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, ErrTokenMalformed
	}

	headerRaw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, ErrTokenMalformed
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
		Typ string `json:"typ"`
	}
	if err := json.Unmarshal(headerRaw, &header); err != nil {
		return nil, ErrTokenMalformed
	}

	// Pinned, not read from the token. See the note at the top of this file.
	if header.Alg != "RS256" {
		return nil, fmt.Errorf("%w: %q", ErrTokenAlgorithm, header.Alg)
	}
	if header.Kid == "" {
		return nil, fmt.Errorf("%w: no kid in header", ErrTokenMalformed)
	}

	key, err := v.keyFor(header.Kid)
	if err != nil {
		return nil, err
	}

	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, ErrTokenMalformed
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig); err != nil {
		return nil, ErrTokenSignature
	}

	// Only parse the payload AFTER the signature checks out. Deserialising
	// attacker-controlled JSON first is a needless expansion of the attack
	// surface.
	payloadRaw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, ErrTokenMalformed
	}
	var claims Claims
	if err := json.Unmarshal(payloadRaw, &claims); err != nil {
		return nil, ErrTokenMalformed
	}

	now := time.Now().Unix()
	switch {
	case claims.Exp == 0:
		// A token with no expiry never stops being valid. Reject it outright
		// rather than treating the field as optional.
		return nil, fmt.Errorf("%w: no exp claim", ErrTokenMalformed)
	case now > claims.Exp+int64(v.leeway.Seconds()):
		return nil, ErrTokenExpired
	case claims.Iat > 0 && now < claims.Iat-int64(v.leeway.Seconds()):
		return nil, ErrTokenNotYetValid
	case v.issuer != "" && claims.Iss != v.issuer:
		return nil, fmt.Errorf("%w: %q", ErrTokenIssuer, claims.Iss)
	case claims.Sub == "":
		return nil, fmt.Errorf("%w: no sub claim", ErrTokenMalformed)
	}

	if v.audience != "" && !contains(claims.Aud, v.audience) {
		return nil, ErrTokenAudience
	}

	return &claims, nil
}

func (v *JWKSVerifier) keyFor(kid string) (*rsa.PublicKey, error) {
	v.mu.RLock()
	key, ok := v.keys[kid]
	fresh := time.Since(v.fetchedAt) < v.ttl
	v.mu.RUnlock()

	if ok && fresh {
		return key, nil
	}

	// Unknown kid, or a stale cache. Refresh once, then look again. An unknown
	// kid is the normal appearance of a key rotation, so it must trigger a
	// refetch rather than a rejection — but only one refetch per stampede.
	v.refresh.Lock()
	defer v.refresh.Unlock()

	v.mu.RLock()
	key, ok = v.keys[kid]
	fresh = time.Since(v.fetchedAt) < v.ttl
	v.mu.RUnlock()
	if ok && fresh {
		return key, nil
	}

	if err := v.fetch(context.Background()); err != nil {
		// If the fetch failed but we have a cached key, use it. identity-service
		// being briefly unreachable must not log the entire platform out; keys
		// rotate on the order of weeks, so a stale JWKS is safe for minutes.
		if ok {
			slog.Warn("JWKS refresh failed; serving from a stale cache",
				slog.String("error", err.Error()))
			return key, nil
		}
		return nil, fmt.Errorf("%w: %v", ErrKeyUnknown, err)
	}

	v.mu.RLock()
	key, ok = v.keys[kid]
	v.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: kid %q", ErrKeyUnknown, kid)
	}
	return key, nil
}

type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
}

func (v *JWKSVerifier) fetch(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.jwksURL, nil)
	if err != nil {
		return err
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("jwks endpoint returned %d", resp.StatusCode)
	}

	var doc struct {
		Keys []jwk `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return err
	}

	parsed := make(map[string]*rsa.PublicKey, len(doc.Keys))
	for _, k := range doc.Keys {
		if k.Kty != "RSA" || (k.Use != "" && k.Use != "sig") {
			continue
		}
		pub, err := parseRSAJWK(k)
		if err != nil {
			slog.Warn("skipping unparseable JWK",
				slog.String("kid", k.Kid), slog.String("error", err.Error()))
			continue
		}
		parsed[k.Kid] = pub
	}
	if len(parsed) == 0 {
		return errors.New("jwks contained no usable RSA signing keys")
	}

	v.mu.Lock()
	v.keys = parsed
	v.fetchedAt = time.Now()
	v.mu.Unlock()

	slog.Info("JWKS refreshed", slog.Int("keys", len(parsed)))
	return nil
}

func parseRSAJWK(k jwk) (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(k.N)
	if err != nil {
		return nil, fmt.Errorf("modulus: %w", err)
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(k.E)
	if err != nil {
		return nil, fmt.Errorf("exponent: %w", err)
	}

	// A short modulus means a weak key. 2048 bits is the floor.
	if len(nBytes) < 256 {
		return nil, fmt.Errorf("modulus is %d bits; minimum is 2048", len(nBytes)*8)
	}

	// The exponent is big-endian and usually 3 bytes (65537). Left-pad to 8 so
	// it can be read as a uint64 regardless of length.
	if len(eBytes) > 8 {
		return nil, errors.New("exponent is implausibly large")
	}
	padded := make([]byte, 8)
	copy(padded[8-len(eBytes):], eBytes)
	e := binary.BigEndian.Uint64(padded)
	if e < 3 || e > 1<<31 {
		return nil, fmt.Errorf("exponent %d is out of range", e)
	}

	return &rsa.PublicKey{N: new(big.Int).SetBytes(nBytes), E: int(e)}, nil
}

// Prime warms the cache at startup so the first real request does not pay for
// the JWKS fetch. A failure here is not fatal — the service starts and the
// first token verification retries.
func (v *JWKSVerifier) Prime(ctx context.Context) {
	if err := v.fetch(ctx); err != nil {
		slog.Warn("could not prime the JWKS cache at startup",
			slog.String("url", v.jwksURL), slog.String("error", err.Error()))
	}
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------

// StaticVerifier accepts a fixed token. For local development and integration
// tests only — it refuses to construct itself outside those environments so it
// cannot be enabled in production by an errant environment variable.
type StaticVerifier struct {
	Token  string
	Claims *Claims
}

func NewStaticVerifier(env, token string, claims *Claims) (*StaticVerifier, error) {
	if env != "local" && env != "test" {
		return nil, fmt.Errorf("the static token verifier cannot be used in the %q environment", env)
	}
	return &StaticVerifier{Token: token, Claims: claims}, nil
}

func (s *StaticVerifier) Verify(token string) (*Claims, error) {
	if !ConstantTimeEqual(token, s.Token) {
		return nil, ErrTokenSignature
	}
	return s.Claims, nil
}
