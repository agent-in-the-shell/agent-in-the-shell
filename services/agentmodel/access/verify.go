// Package access verifies Cloudflare Access assertions without trusting proxy
// email headers or token-controlled key URLs. It has no credential persistence.
package access

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

var ErrAssertion = errors.New("invalid Access assertion")

type Identity struct{ Issuer, Subject, Email string }

type Verifier struct {
	issuer, audience, domain, certsURL string
	client                             *http.Client
	mu                                 sync.Mutex
	keys                               map[string]*rsa.PublicKey
	expires, retryAfter                time.Time
}

// ParseHTTPSOrigin accepts exactly an origin: https scheme, a host, and nothing
// else (no userinfo, path, query or fragment), spelled canonically. Both the
// portal origin and the Access issuer are validated through it.
func ParseHTTPSOrigin(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.String() != raw {
		return nil, errors.New("access: not an https origin")
	}
	return u, nil
}

func NewVerifier(issuer, audience, domain string) (*Verifier, error) {
	u, err := ParseHTTPSOrigin(issuer)
	if err != nil || u.Port() != "" || !strings.HasSuffix(u.Host, ".cloudflareaccess.com") || strings.Count(u.Host, ".") != 2 || strings.TrimSpace(audience) == "" || domain == "" || u.Host == ".cloudflareaccess.com" || strings.ContainsAny(domain, "/@: ") {
		return nil, ErrAssertion
	}
	return &Verifier{issuer: issuer, audience: audience, domain: strings.ToLower(domain), certsURL: issuer + "/cdn-cgi/access/certs", client: &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (v *Verifier) Verify(ctx context.Context, token string) (Identity, error) {
	if len(token) > 32768 {
		return Identity{}, ErrAssertion
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Identity{}, ErrAssertion
	}
	var header struct {
		Alg  string   `json:"alg"`
		Kid  string   `json:"kid"`
		Crit []string `json:"crit"`
	}
	if err := decodePart(parts[0], &header); err != nil || header.Alg != "RS256" || header.Kid == "" || len(header.Crit) != 0 {
		return Identity{}, ErrAssertion
	}
	key, err := v.publicKey(ctx, header.Kid)
	if err != nil {
		return Identity{}, err
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Identity{}, ErrAssertion
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], signature) != nil {
		return Identity{}, ErrAssertion
	}
	var claims struct {
		Iss   string          `json:"iss"`
		Sub   string          `json:"sub"`
		Email string          `json:"email"`
		Aud   json.RawMessage `json:"aud"`
		Exp   int64           `json:"exp"`
		Nbf   int64           `json:"nbf"`
	}
	if err := decodePart(parts[1], &claims); err != nil {
		return Identity{}, ErrAssertion
	}
	now := time.Now().Unix()
	if claims.Iss != v.issuer || strings.TrimSpace(claims.Sub) == "" || claims.Exp <= now || claims.Nbf > now {
		return Identity{}, ErrAssertion
	}
	var audiences []string
	if err := json.Unmarshal(claims.Aud, &audiences); err != nil {
		var single string
		if json.Unmarshal(claims.Aud, &single) != nil {
			return Identity{}, ErrAssertion
		}
		audiences = []string{single}
	}
	found := false
	for _, a := range audiences {
		if a == v.audience {
			found = true
		}
	}
	if !found {
		return Identity{}, ErrAssertion
	}
	email := strings.ToLower(claims.Email)
	local, domain, ok := strings.Cut(email, "@")
	if !ok || local == "" || domain != v.domain || strings.ContainsAny(email, " \t\r\n") {
		return Identity{}, ErrAssertion
	}
	return Identity{Issuer: claims.Iss, Subject: claims.Sub, Email: email}, nil
}

func decodePart(s string, dst any) error {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, dst)
}

func (v *Verifier) publicKey(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	// Serialize refreshes across callers. Unknown kids can discover rotation
	// before cache expiry, but share the same once/minute retry limit as outages.
	v.mu.Lock()
	defer v.mu.Unlock()
	now := time.Now()
	if now.Before(v.expires) {
		if key := v.keys[kid]; key != nil {
			return key, nil
		}
	}
	if now.Before(v.retryAfter) {
		return nil, ErrAssertion
	}
	v.retryAfter = now.Add(time.Minute)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.certsURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return nil, ErrAssertion
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, ErrAssertion
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 256*1024+1))
	if err != nil || len(b) > 256*1024 {
		return nil, ErrAssertion
	}
	var document struct {
		Keys []struct {
			Kid string `json:"kid"`
			Kty string `json:"kty"`
			Alg string `json:"alg"`
			Use string `json:"use"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if json.Unmarshal(b, &document) != nil || len(document.Keys) > 32 {
		return nil, ErrAssertion
	}
	keys := make(map[string]*rsa.PublicKey)
	for _, j := range document.Keys {
		if j.Kty != "RSA" || (j.Alg != "" && j.Alg != "RS256") || (j.Use != "" && j.Use != "sig") || j.Kid == "" {
			continue
		}
		n, err := base64.RawURLEncoding.DecodeString(j.N)
		if err != nil || len(n) < 256 || len(n) > 1024 {
			continue
		}
		e, err := base64.RawURLEncoding.DecodeString(j.E)
		if err != nil || len(e) == 0 || len(e) > 4 {
			continue
		}
		exponent := new(big.Int).SetBytes(e).Int64()
		if exponent < 3 || exponent > 2147483647 || exponent%2 == 0 {
			continue
		}
		if keys[j.Kid] != nil {
			return nil, ErrAssertion
		}
		keys[j.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(exponent)}
	}
	if len(keys) == 0 {
		return nil, ErrAssertion
	}
	// A successful initial load leaves one immediate rotation check available.
	// Later refreshes (including misses and failures) retain the shared cooldown.
	if v.keys == nil && keys[kid] != nil {
		v.retryAfter = time.Time{}
	}
	v.keys = keys
	v.expires = now.Add(5 * time.Minute)
	if key := keys[kid]; key != nil {
		return key, nil
	}
	return nil, ErrAssertion
}
