package access

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestVerifierSignedClaimsAndBoundedRefresh(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/cdn-cgi/access/certs" {
			t.Errorf("wrong path %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{"kid": "test", "kty": "RSA", "alg": "RS256", "use": "sig", "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())}}})
	}))
	defer server.Close()
	v, err := NewVerifier("https://team.cloudflareaccess.com", "audience", "example.com")
	if err != nil {
		t.Fatal(err)
	}
	v.certsURL = server.URL + "/cdn-cgi/access/certs"
	claims := func() map[string]any {
		return map[string]any{"iss": "https://team.cloudflareaccess.com", "aud": []string{"audience"}, "sub": "stable-subject", "email": "employee@example.com", "exp": time.Now().Add(time.Hour).Unix()}
	}
	token := func(c map[string]any, alg, kid string) string {
		h, _ := json.Marshal(map[string]string{"alg": alg, "kid": kid})
		b, _ := json.Marshal(c)
		msg := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(b)
		digest := sha256.Sum256([]byte(msg))
		sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
		if err != nil {
			t.Fatal(err)
		}
		return msg + "." + base64.RawURLEncoding.EncodeToString(sig)
	}
	got, err := v.Verify(context.Background(), token(claims(), "RS256", "test"))
	if err != nil || got.Subject != "stable-subject" {
		t.Fatalf("verify %+v %v", got, err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"issuer", func(c map[string]any) { c["iss"] = "https://other.cloudflareaccess.com" }},
		{"audience", func(c map[string]any) { c["aud"] = []string{"other"} }},
		{"domain suffix", func(c map[string]any) { c["email"] = "employee@evil-example.com" }},
		{"missing email", func(c map[string]any) { delete(c, "email") }},
		{"missing subject", func(c map[string]any) { delete(c, "sub") }},
		{"missing expiry", func(c map[string]any) { delete(c, "exp") }},
		{"expired", func(c map[string]any) { c["exp"] = time.Now().Unix() }},
		{"future nbf", func(c map[string]any) { c["nbf"] = time.Now().Add(time.Hour).Unix() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := claims()
			tc.mutate(c)
			if _, err := v.Verify(context.Background(), token(c, "RS256", "test")); err == nil {
				t.Fatal("accepted invalid claims")
			}
		})
	}
	for _, alg := range []string{"none", "HS256", "RS512"} {
		if _, err := v.Verify(context.Background(), token(claims(), alg, "test")); err == nil {
			t.Fatalf("accepted %s", alg)
		}
	}
	for range 10 {
		if _, err := v.Verify(context.Background(), token(claims(), "RS256", "unknown")); err == nil {
			t.Fatal("unknown kid accepted")
		}
	}
	if calls != 2 {
		t.Fatalf("want initial fetch plus one unknown-kid refresh, got %d", calls)
	}
	good := token(claims(), "RS256", "test")
	if _, err := v.Verify(context.Background(), good[:len(good)-6]+"AAAAAA"); err == nil {
		t.Fatal("bad signature accepted")
	}
}

func TestVerifierUnknownKidRefresh(t *testing.T) {
	oldKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	newKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	var published, unavailable atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if unavailable.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		keys := map[string]*rsa.PrivateKey{"old": oldKey}
		if published.Load() {
			keys["new"] = newKey
		}
		var jwks []any
		for kid, key := range keys {
			jwks = append(jwks, map[string]any{"kid": kid, "kty": "RSA", "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())})
		}
		json.NewEncoder(w).Encode(map[string]any{"keys": jwks})
	}))
	defer server.Close()
	v, err := NewVerifier("https://team.cloudflareaccess.com", "audience", "example.com")
	if err != nil {
		t.Fatal(err)
	}
	v.certsURL = server.URL
	token := func(kid string, key *rsa.PrivateKey) string {
		h, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": kid})
		b, _ := json.Marshal(map[string]any{"iss": v.issuer, "aud": "audience", "sub": "employee", "email": "employee@example.com", "exp": time.Now().Add(time.Hour).Unix()})
		msg := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(b)
		digest := sha256.Sum256([]byte(msg))
		sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
		if err != nil {
			t.Fatal(err)
		}
		return msg + "." + base64.RawURLEncoding.EncodeToString(sig)
	}
	oldToken := token("old", oldKey)
	if _, err := v.Verify(context.Background(), oldToken); err != nil {
		t.Fatal("initial key:", err)
	}
	if calls.Load() != 1 || !time.Now().Before(v.expires) {
		t.Fatal("expected one fetch and a fresh cache")
	}
	published.Store(true)
	newToken := token("new", newKey)
	if got, err := v.Verify(context.Background(), newToken); err != nil || got.Subject != "employee" {
		t.Fatalf("published new key rejected with fresh old-key cache: %+v %v (fetches: %d)", got, err, calls.Load())
	}
	if calls.Load() != 2 {
		t.Fatalf("rotation fetched %d times, want 2", calls.Load())
	}
	unknownTokens := make([]string, 32)
	for i := range unknownTokens {
		unknownTokens[i] = token(fmt.Sprintf("arbitrary-%d", i), newKey)
	}
	assertBounded := func(want int64) {
		t.Helper()
		var wg sync.WaitGroup
		for _, tok := range unknownTokens {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := v.Verify(context.Background(), tok); err == nil {
					t.Error("accepted unknown kid")
				}
			}()
		}
		wg.Wait()
		if got := calls.Load(); got != want {
			t.Fatalf("fetches %d, want %d", got, want)
		}
		for _, tok := range []string{oldToken, newToken} {
			if _, err := v.Verify(context.Background(), tok); err != nil {
				t.Fatal("refresh throttle discarded known key:", err)
			}
		}
	}
	assertBounded(2)
	// Advance only the refresh cooldown, not the five-minute key cache.
	v.mu.Lock()
	v.retryAfter = time.Now().Add(-time.Second)
	v.mu.Unlock()
	assertBounded(3)
	unavailable.Store(true)
	v.mu.Lock()
	v.retryAfter = time.Now().Add(-time.Second)
	v.mu.Unlock()
	assertBounded(4)
	assertBounded(4)
}

func TestVerifierConfig(t *testing.T) {
	for _, issuer := range []string{"http://team.cloudflareaccess.com", "https://team.cloudflareaccess.com/", "https://team.cloudflareaccess.com.evil.test", "https://user@team.cloudflareaccess.com", "https://team.cloudflareaccess.com:443", "https://example.com"} {
		if _, err := NewVerifier(issuer, "audience", "example.com"); err == nil {
			t.Errorf("accepted %s", issuer)
		}
	}
}
