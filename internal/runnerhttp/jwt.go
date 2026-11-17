// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package runnerhttp

import (
	"cmp"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/ax/internal/substrate"
)

const (
	maxActorJWTBytes     = 16 << 10
	maxDiscoveryDocBytes = 256 << 10
	jwksCacheTTL         = 5 * time.Minute
	jwksMinRefreshPeriod = 5 * time.Second
	jwtClockSkew         = time.Minute
)

// ActorIdentity identifies the Agent Substrate actor authenticated by an
// inbound runner request.
type ActorIdentity struct {
	Atespace  string
	ActorName string
	ActorUID  string
}

// ActorVerifier authenticates an HTTP request arriving on the runner control
// port (:8081) and returns the calling actor's verified Substrate identity.
type ActorVerifier interface {
	VerifyActorRequest(ctx context.Context, r *http.Request) (ActorIdentity, error)
}

// ActorVerifierFunc adapts a function to [ActorVerifier], primarily for unit
// tests running against in-memory Substrate fakes.
type ActorVerifierFunc func(ctx context.Context, r *http.Request) (ActorIdentity, error)

// VerifyActorRequest calls f(ctx, r).
func (f ActorVerifierFunc) VerifyActorRequest(ctx context.Context, r *http.Request) (ActorIdentity, error) {
	return f(ctx, r)
}

// OIDCVerifierConfig configures an [OIDCVerifier] that validates Actor JWTs
// injected by Substrate's egress gateway against Substrate's OIDC discovery
// endpoint.
type OIDCVerifierConfig struct {
	// Issuer is the expected JWT "iss" claim and OIDC discovery base URL
	// (for example "https://idp.ate-system.svc"). Required.
	Issuer string
	// Audience is the expected JWT "aud" claim (defaults to
	// substrate.DefaultRunnerAudience).
	Audience string
	// CAFile is an optional PEM CA bundle path used when fetching OIDC
	// discovery and JWKS over HTTPS.
	CAFile string
	// InsecureTLS disables TLS verification when fetching discovery/JWKS.
	InsecureTLS bool
	// HTTPClient overrides the HTTP client used for discovery and JWKS fetches.
	HTTPClient *http.Client
	// Now overrides the clock in tests; defaults to time.Now.
	Now func() time.Time
}

// OIDCVerifier verifies ES256/RS256 Actor JWTs issued by Substrate's OIDC
// identity provider and caches its JWKS keys in memory.
type OIDCVerifier struct {
	issuer     string
	audience   string
	httpClient *http.Client
	now        func() time.Time

	mu          sync.Mutex
	jwksURI     string
	keys        map[string]crypto.PublicKey
	fetchedAt   time.Time
	lastRefresh time.Time
}

// NewOIDCVerifier constructs an [OIDCVerifier] from cfg.
func NewOIDCVerifier(cfg OIDCVerifierConfig) (*OIDCVerifier, error) {
	issuer := strings.TrimRight(strings.TrimSpace(cfg.Issuer), "/")
	if issuer == "" {
		return nil, errors.New("runnerhttp: OIDC issuer is required")
	}
	client := cfg.HTTPClient
	if client == nil {
		var err error
		client, err = newOIDCHTTPClient(cfg.CAFile, cfg.InsecureTLS)
		if err != nil {
			return nil, err
		}
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &OIDCVerifier{
		issuer:     issuer,
		audience:   cmp.Or(strings.TrimSpace(cfg.Audience), substrate.DefaultRunnerAudience),
		httpClient: client,
		now:        now,
		keys:       make(map[string]crypto.PublicKey),
	}, nil
}

// VerifyActorRequest parses and verifies the Authorization: Bearer JWT on r
// and returns the caller's [ActorIdentity].
func (v *OIDCVerifier) VerifyActorRequest(ctx context.Context, r *http.Request) (ActorIdentity, error) {
	rawToken, err := parseBearerToken(r.Header.Get("Authorization"))
	if err != nil {
		return ActorIdentity{}, err
	}
	if len(rawToken) > maxActorJWTBytes {
		return ActorIdentity{}, errors.New("actor JWT exceeds size limit")
	}

	parts := strings.Split(rawToken, ".")
	if len(parts) != 3 {
		return ActorIdentity{}, errors.New("malformed actor JWT")
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return ActorIdentity{}, fmt.Errorf("decoding JWT header: %w", err)
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ActorIdentity{}, fmt.Errorf("decoding JWT claims: %w", err)
	}
	sigBytes, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return ActorIdentity{}, fmt.Errorf("decoding JWT signature: %w", err)
	}

	var hdr jwtHeader
	if err := json.Unmarshal(headerBytes, &hdr); err != nil {
		return ActorIdentity{}, fmt.Errorf("parsing JWT header: %w", err)
	}
	if hdr.Alg != "ES256" && hdr.Alg != "RS256" {
		return ActorIdentity{}, fmt.Errorf("unsupported JWT signing algorithm %q", hdr.Alg)
	}

	key, err := v.lookupKey(ctx, hdr.Kid)
	if err != nil {
		return ActorIdentity{}, err
	}
	if err := verifyJWTSignature(hdr.Alg, key, parts[0]+"."+parts[1], sigBytes); err != nil {
		return ActorIdentity{}, err
	}

	var claims actorJWTClaims
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return ActorIdentity{}, fmt.Errorf("parsing JWT claims: %w", err)
	}
	return v.validateClaims(claims)
}

func (v *OIDCVerifier) validateClaims(claims actorJWTClaims) (ActorIdentity, error) {
	if strings.TrimRight(claims.Iss, "/") != v.issuer {
		return ActorIdentity{}, fmt.Errorf("unexpected JWT issuer %q", claims.Iss)
	}
	if !slices.Contains(claims.Aud, v.audience) {
		return ActorIdentity{}, fmt.Errorf("JWT missing expected audience %q", v.audience)
	}

	now := v.now().Unix()
	if claims.Exp == 0 || now > claims.Exp+int64(jwtClockSkew.Seconds()) {
		return ActorIdentity{}, errors.New("actor JWT is expired")
	}
	if claims.Nbf > 0 && now < claims.Nbf-int64(jwtClockSkew.Seconds()) {
		return ActorIdentity{}, errors.New("actor JWT is not valid yet")
	}

	atespace := strings.TrimSpace(claims.Actor.GetAtespace())
	actorName := strings.TrimSpace(claims.Actor.GetActorName())
	actorUID := strings.TrimSpace(claims.Actor.GetActorUID())
	if atespace == "" || actorName == "" || actorUID == "" {
		return ActorIdentity{}, errors.New("actor JWT missing actor.ate.dev identity fields")
	}
	if claims.Sub != "" {
		subAtespace, subActorName, err := parseActorSubject(claims.Sub)
		if err == nil && (subAtespace != atespace || subActorName != actorName) {
			return ActorIdentity{}, errors.New("actor JWT subject does not match actor.ate.dev claim")
		}
	}
	return ActorIdentity{
		Atespace:  atespace,
		ActorName: actorName,
		ActorUID:  actorUID,
	}, nil
}

func (v *OIDCVerifier) lookupKey(ctx context.Context, kid string) (crypto.PublicKey, error) {
	v.mu.Lock()
	key, found := v.keys[kid]
	stale := time.Since(v.fetchedAt) > jwksCacheTTL
	if len(v.keys) == 1 && kid == "" {
		for _, only := range v.keys {
			key = only
			found = true
		}
	}
	v.mu.Unlock()

	if found && !stale {
		return key, nil
	}
	if err := v.refreshJWKS(ctx, !found); err != nil {
		if found {
			return key, nil
		}
		return nil, err
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	if key, ok := v.keys[kid]; ok {
		return key, nil
	}
	if len(v.keys) == 1 && kid == "" {
		for _, only := range v.keys {
			return only, nil
		}
	}
	return nil, fmt.Errorf("signing key %q not found in JWKS", kid)
}

func (v *OIDCVerifier) refreshJWKS(ctx context.Context, forceOnMissing bool) error {
	v.mu.Lock()
	if forceOnMissing && !v.lastRefresh.IsZero() && v.now().Sub(v.lastRefresh) < jwksMinRefreshPeriod {
		v.mu.Unlock()
		return errors.New("JWKS refresh rate-limited")
	}
	jwksURI := v.jwksURI
	v.lastRefresh = v.now()
	v.mu.Unlock()

	if jwksURI == "" {
		var err error
		jwksURI, err = v.discoverJWKSURI(ctx)
		if err != nil {
			return err
		}
	}
	keys, err := v.fetchJWKS(ctx, jwksURI)
	if err != nil {
		return err
	}

	v.mu.Lock()
	v.jwksURI = jwksURI
	v.keys = keys
	v.fetchedAt = v.now()
	v.mu.Unlock()
	return nil
}

func (v *OIDCVerifier) discoverJWKSURI(ctx context.Context) (string, error) {
	u := v.issuer + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", fmt.Errorf("building OIDC discovery request: %w", err)
	}
	resp, err := v.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetching OIDC discovery %q: %w", u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("OIDC discovery %q returned HTTP %d", u, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDiscoveryDocBytes))
	if err != nil {
		return "", fmt.Errorf("reading OIDC discovery: %w", err)
	}
	var doc struct {
		JWKSURI string `json:"jwks_uri"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", fmt.Errorf("parsing OIDC discovery: %w", err)
	}
	if doc.JWKSURI == "" {
		return "", errors.New("OIDC discovery missing jwks_uri")
	}
	parsed, err := url.Parse(doc.JWKSURI)
	if err != nil || !parsed.IsAbs() {
		return "", fmt.Errorf("invalid jwks_uri %q", doc.JWKSURI)
	}
	return doc.JWKSURI, nil
}

func (v *OIDCVerifier) fetchJWKS(ctx context.Context, jwksURI string) (map[string]crypto.PublicKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, jwksURI, nil)
	if err != nil {
		return nil, fmt.Errorf("building JWKS request: %w", err)
	}
	resp, err := v.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching JWKS %q: %w", jwksURI, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("JWKS %q returned HTTP %d", jwksURI, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDiscoveryDocBytes))
	if err != nil {
		return nil, fmt.Errorf("reading JWKS: %w", err)
	}
	var doc struct {
		Keys []jwkKey `json:"keys"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("parsing JWKS: %w", err)
	}
	out := make(map[string]crypto.PublicKey, len(doc.Keys))
	for _, k := range doc.Keys {
		pub, err := parseJWKPublicKey(k)
		if err != nil {
			continue
		}
		out[k.Kid] = pub
	}
	if len(out) == 0 {
		return nil, errors.New("JWKS contained no supported public keys")
	}
	return out, nil
}

type jwtHeader struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
}

type actorClaim struct {
	Atespace  string `json:"atespace"`
	ActorName string `json:"actorName"`
	ActorUID  string `json:"actorUID"`
}

func (c *actorClaim) GetAtespace() string {
	if c == nil {
		return ""
	}
	return c.Atespace
}

func (c *actorClaim) GetActorName() string {
	if c == nil {
		return ""
	}
	return c.ActorName
}

func (c *actorClaim) GetActorUID() string {
	if c == nil {
		return ""
	}
	return c.ActorUID
}

type actorJWTClaims struct {
	Iss   string      `json:"iss"`
	Sub   string      `json:"sub"`
	Aud   audienceSet `json:"aud"`
	Exp   int64       `json:"exp"`
	Nbf   int64       `json:"nbf"`
	Actor *actorClaim `json:"actor.ate.dev"`
}

type audienceSet []string

func (a *audienceSet) UnmarshalJSON(data []byte) error {
	var single string
	if err := json.Unmarshal(data, &single); err == nil {
		if single != "" {
			*a = []string{single}
		}
		return nil
	}
	var multi []string
	if err := json.Unmarshal(data, &multi); err != nil {
		return err
	}
	*a = multi
	return nil
}

type jwkKey struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
	N   string `json:"n"`
	E   string `json:"e"`
}

func newOIDCHTTPClient(caFile string, insecureTLS bool) (*http.Client, error) {
	tlsCfg := &tls.Config{InsecureSkipVerify: insecureTLS}
	if caFile != "" {
		pemBytes, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("reading OIDC CA file %q: %w", caFile, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pemBytes) {
			return nil, fmt.Errorf("parsing OIDC CA file %q", caFile)
		}
		tlsCfg.RootCAs = pool
	}
	return &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: tlsCfg},
	}, nil
}

func parseBearerToken(header string) (string, error) {
	parts := strings.SplitN(strings.TrimSpace(header), " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || strings.TrimSpace(parts[1]) == "" {
		return "", errors.New("missing or malformed Authorization Bearer token")
	}
	return strings.TrimSpace(parts[1]), nil
}

func parseActorSubject(sub string) (string, string, error) {
	const prefix = "actor/"
	if !strings.HasPrefix(sub, prefix) {
		return "", "", fmt.Errorf("unexpected actor JWT subject %q", sub)
	}
	rest := strings.TrimPrefix(sub, prefix)
	atespace, actorName, ok := strings.Cut(rest, "/")
	if !ok || atespace == "" || actorName == "" || strings.Contains(actorName, "/") {
		return "", "", fmt.Errorf("unexpected actor JWT subject %q", sub)
	}
	return atespace, actorName, nil
}

func parseJWKPublicKey(k jwkKey) (crypto.PublicKey, error) {
	switch k.Kty {
	case "EC":
		if k.Crv != "P-256" {
			return nil, fmt.Errorf("unsupported EC curve %q", k.Crv)
		}
		xb, err := base64.RawURLEncoding.DecodeString(k.X)
		if err != nil {
			return nil, err
		}
		yb, err := base64.RawURLEncoding.DecodeString(k.Y)
		if err != nil {
			return nil, err
		}
		if len(xb) > 32 || len(yb) > 32 {
			return nil, errors.New("invalid P-256 coordinate length")
		}
		uncompressed := make([]byte, 1+64)
		uncompressed[0] = 0x04
		copy(uncompressed[1+32-len(xb):1+32], xb)
		copy(uncompressed[1+64-len(yb):], yb)
		return ecdsa.ParseUncompressedPublicKey(elliptic.P256(), uncompressed)
	case "RSA":
		nb, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil {
			return nil, err
		}
		eb, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil {
			return nil, err
		}
		exp := new(big.Int).SetBytes(eb).Int64()
		if exp <= 0 || exp > math.MaxInt {
			return nil, errors.New("invalid RSA exponent")
		}
		return &rsa.PublicKey{
			N: new(big.Int).SetBytes(nb),
			E: int(exp),
		}, nil
	default:
		return nil, fmt.Errorf("unsupported JWK key type %q", k.Kty)
	}
}

func verifyJWTSignature(alg string, key crypto.PublicKey, signingInput string, sig []byte) error {
	digest := crypto.SHA256.New()
	_, _ = digest.Write([]byte(signingInput))
	sum := digest.Sum(nil)

	switch alg {
	case "ES256":
		ecKey, ok := key.(*ecdsa.PublicKey)
		if !ok {
			return errors.New("JWT key type mismatch for ES256")
		}
		if len(sig) != 64 {
			return errors.New("invalid ES256 signature length")
		}
		r := new(big.Int).SetBytes(sig[:32])
		s := new(big.Int).SetBytes(sig[32:])
		if !ecdsa.Verify(ecKey, sum, r, s) {
			return errors.New("invalid ES256 JWT signature")
		}
		return nil
	case "RS256":
		rsaKey, ok := key.(*rsa.PublicKey)
		if !ok {
			return errors.New("JWT key type mismatch for RS256")
		}
		if err := rsa.VerifyPKCS1v15(rsaKey, crypto.SHA256, sum, sig); err != nil {
			return fmt.Errorf("invalid RS256 JWT signature: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("unsupported JWT algorithm %q", alg)
	}
}
