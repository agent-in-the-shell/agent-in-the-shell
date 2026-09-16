package api_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/router"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/store"
)

func TestPortalKeyBypassesSharedCacheWithoutChangingOtherKeys(t *testing.T) {
	upstream := cacheStub()
	ts, ledger := newTestServer(t, map[string][]router.Deployment{"gpt-4": {{Provider: upstream, Model: "gpt-4", Weight: 1}}}, withCache(t))
	token := "portal-token"
	_, err := ledger.CreatePortalKey(context.Background(), store.PortalIdentity{Issuer: "issuer", Subject: "subject", Email: "employee@example.com"}, store.ManagedKey{ID: "portal", Name: "employee@example.com", KeyHash: sha256hex(token), Models: []string{"gpt-4"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.SetPortalModels(context.Background(), "portal", []string{"gpt-4"}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ token, cache string }{{testToken, "miss"}, {token, ""}, {token, ""}, {testToken, "hit"}} {
		response := mustPost(t, ts, "/v1/chat/completions", cacheReq("same"), tc.token)
		if response.StatusCode != http.StatusOK {
			t.Fatal(response.StatusCode)
		}
		response.Body.Close()
		if got := response.Header.Get("X-Agentmodel-Cache"); got != tc.cache {
			t.Fatalf("cache marker %q want %q", got, tc.cache)
		}
	}
	if upstream.CallCount() != 3 {
		t.Fatalf("portal reused cache: %d calls", upstream.CallCount())
	}
	response := mustPost(t, ts, "/v1/chat/completions", cacheReq("portal-only"), token)
	response.Body.Close()
	response = mustPost(t, ts, "/v1/chat/completions", cacheReq("portal-only"), testToken)
	response.Body.Close()
	if response.Header.Get("X-Agentmodel-Cache") != "miss" {
		t.Fatal("portal seeded shared cache")
	}
	denied := mustPost(t, ts, "/v1/chat/completions", chatBody("forbidden-model"), token)
	defer denied.Body.Close()
	if denied.StatusCode != 403 {
		t.Fatal("scope bypass", denied.StatusCode)
	}
}
