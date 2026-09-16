package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/api"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider/stub"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/router"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/store"
)

func TestPortalIndividualModelsImmediatelyBoundInferenceModelsAndFallbacks(t *testing.T) {
	fallback := &stub.Stub{NameValue: "anthropic"}
	deps := map[string][]router.Deployment{
		"gpt-4":  {{Provider: &stub.Stub{NameValue: "openai", CompleteErr: errors.New("503 overloaded")}, Model: "gpt-4", Weight: 1}},
		"claude": {{Provider: fallback, Model: "claude-sonnet", Weight: 1}},
	}
	ts, st := newTestServer(t, deps, func(c *api.Config) { c.Router = router.New(deps, map[string][]string{"gpt-4": {"claude"}}) })
	ctx := context.Background()
	set := func(models []string) {
		t.Helper()
		if err := st.SetPortalModels(ctx, "portal", models); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.CreatePortalKey(ctx, store.PortalIdentity{Issuer: "issuer", Subject: "employee", Email: "employee@example.com"}, store.ManagedKey{ID: "portal", Name: "Employee", KeyHash: sha256hex("portal")}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateKey(ctx, store.ManagedKey{ID: "legacy", Name: "Legacy", KeyHash: sha256hex("legacy")}); err != nil {
		t.Fatal(err)
	}
	infer := func(token string, wantOK bool) {
		t.Helper()
		resp := mustPost(t, ts, "/v1/chat/completions", chatBody("gpt-4"), token)
		defer resp.Body.Close()
		if (resp.StatusCode == 200) != wantOK {
			t.Fatal(token, resp.StatusCode)
		}
	}
	models := func(want int) {
		t.Helper()
		req, _ := http.NewRequest("GET", ts.URL+"/v1/models", nil)
		req.Header.Set("Authorization", "Bearer portal")
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var body struct {
			Data []json.RawMessage `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 200 || len(body.Data) != want {
			t.Fatal(resp.StatusCode, len(body.Data), want)
		}
	}
	infer("portal", false)
	models(0)
	set([]string{"gpt-4", "claude"})
	infer("portal", true)
	models(2)
	set([]string{"gpt-4"})
	before := fallback.CallCount()
	infer("portal", false)
	models(1)
	if fallback.CallCount() != before {
		t.Fatal("blocked fallback called")
	}
	set([]string{})
	before = fallback.CallCount()
	infer("portal", false)
	models(0)
	if fallback.CallCount() != before {
		t.Fatal("empty policy called upstream")
	}
	infer("legacy", true)
	infer(testToken, true)
	set([]string{"gpt-4", "claude"})
	infer("portal", true)
	models(2)
}
