package api_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/router"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/store"
)

func TestRevocationRejectsCachedInferenceAndReplacementNeedsNewGrants(t *testing.T) {
	for _, kind := range []string{"legacy", "portal", "service"} {
		t.Run(kind, func(t *testing.T) {
			upstream := cacheStub()
			ts, st := newTestServer(t, map[string][]router.Deployment{"gpt-4": {{Provider: upstream, Model: "gpt-4", Weight: 1}}}, withCache(t))
			ctx := context.Background()
			token := "old-" + kind
			key := store.ManagedKey{ID: "key", Name: kind, KeyHash: sha256hex(token)}
			owner := store.PortalIdentity{Issuer: "issuer", Subject: "subject", Email: "employee@example.com"}
			var err error
			switch kind {
			case "legacy":
				err = st.CreateKey(ctx, key)
			case "portal":
				_, err = st.CreatePortalKey(ctx, owner, key)
			case "service":
				_, err = st.CreateServiceKey(ctx, key)
			}
			if err != nil {
				t.Fatal(err)
			}
			if kind != "legacy" {
				if err := st.SetPortalModels(ctx, key.ID, []string{"gpt-4"}); err != nil {
					t.Fatal(err)
				}
			}
			infer := func(tok string, want int) {
				t.Helper()
				r := mustPost(t, ts, "/v1/chat/completions", cacheReq("same"), tok)
				defer r.Body.Close()
				if r.StatusCode != want {
					t.Fatalf("status %d want %d", r.StatusCode, want)
				}
			}
			infer(testToken, 200) // warm shared cache before revocation
			infer(token, 200)
			toggle := func(action string, want int) {
				t.Helper()
				r := keyReq(t, ts, "POST", "/v1/keys/key/"+action, testToken)
				defer r.Body.Close()
				if r.StatusCode != want {
					t.Fatalf("%s: %d want %d", action, r.StatusCode, want)
				}
			}
			toggle("disable", 200)
			infer(token, 401)
			toggle("enable", 200)
			infer(token, 200)
			toggle("revoke", 200)
			before := upstream.CallCount()
			infer(token, 401)
			toggle("enable", 409)
			infer(token, 401)
			if upstream.CallCount() != before {
				t.Fatal("revoked request reached upstream")
			}
			if kind == "portal" {
				if _, err := st.RotatePortalKey(ctx, owner, 1, sha256hex("replacement")); err != nil {
					t.Fatal(err)
				}
				infer(token, 401)
				infer("replacement", 403)
				if err := st.SetPortalModels(ctx, key.ID, []string{"gpt-4"}); err != nil {
					t.Fatal(err)
				}
				infer("replacement", 200)
				infer(token, http.StatusUnauthorized)
			}
		})
	}
}
