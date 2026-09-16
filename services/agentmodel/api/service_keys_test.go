package api

import (
	"context"
	"encoding/json"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/router"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/store"
	"github.com/go-chi/chi/v5"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func serviceAdminRequest(s *Server, method, path, assertion, body string, csrf bool) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "https://portal.example.com"+path, strings.NewReader(body))
	r.Header.Set("Cf-Access-Jwt-Assertion", assertion)
	if csrf {
		r.Header.Set("Origin", s.portal.Origin)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-CSRF-Token", strings.Repeat("a", 43))
		r.AddCookie(&http.Cookie{Name: portalCSRFCookie, Value: strings.Repeat("a", 43)})
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func TestAdminCreateServiceKey(t *testing.T) {
	s, _ := portalTestServer(t)
	w := serviceAdminRequest(s, "POST", "/portal/admin/api/service-keys", "signed", `{"name":"build-worker"}`, true)
	if w.Code != 201 {
		t.Fatalf("create service key: got %d, want 201: %s", w.Code, w.Body.String())
	}
}

func TestServiceCreateSecurityAndPolicy(t *testing.T) {
	s, st := portalTestServer(t)
	s.router = router.New(map[string][]router.Deployment{"chatgpt": {}}, nil)
	s.keys = append(s.keys, resolvedKey{name: "Config Worker", disabled: true})
	ctx := context.Background()
	if err := st.CreateKey(ctx, store.ManagedKey{ID: "legacy", Name: "Legacy Worker", KeyHash: hashAPIKey("legacy"), Metadata: `{"service":true}`}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		assertion, body string
		csrf            bool
		want            int
	}{
		{"", `{"name":"worker"}`, true, 401},
		{"other", `{"name":"worker"}`, true, 403},
		{"signed", `{"name":"worker"}`, false, 403},
		{"signed", `{}`, true, 400}, {"signed", `null`, true, 400},
		{"signed", `{"name":"  "}`, true, 400}, {"signed", `{"name":"a\nb"}`, true, 400},
		{"signed", `{"name":"` + strings.Repeat("a", 129) + `"}`, true, 400},
		{"signed", `{"name":"worker","models":[]}`, true, 400},
		{"signed", `{"name":"worker","disabled":false}`, true, 400},
		{"signed", `{"name":"worker","max_budget":1}`, true, 400},
		{"signed", `{"name":"worker","key_hash":"evil"}`, true, 400},
		{"signed", `{"name":" config worker "}`, true, 409},
		{"signed", `{"name":" LEGACY Worker "}`, true, 409},
	} {
		w := serviceAdminRequest(s, "POST", "/portal/admin/api/service-keys", tc.assertion, tc.body, tc.csrf)
		if w.Code != tc.want {
			t.Fatalf("%+v: %d %s", tc, w.Code, w.Body.String())
		}
	}
	w := serviceAdminRequest(s, "POST", "/portal/admin/api/service-keys", "signed", `{"name":" Worker "}`, true)
	var created struct {
		KeyID  string   `json:"key_id"`
		Key    string   `json:"key"`
		Name   string   `json:"name"`
		Kind   string   `json:"kind"`
		Models []string `json:"models"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil || w.Code != 201 || created.Key == "" || created.Name != "Worker" || created.Kind != "service" || created.Models == nil || len(created.Models) != 0 {
		t.Fatal(w.Code, w.Body.String(), err)
	}
	if w.Header().Get("Cache-Control") != "no-store" || strings.Contains(w.Body.String(), "key_hash") || strings.Contains(w.Body.String(), hashAPIKey(created.Key)) {
		t.Fatal("unsafe response", w.Body.String())
	}
	key, err := st.GetKeyByHash(ctx, hashAPIKey(created.Key))
	if err != nil || !key.ServiceIssued || key.PortalIssued {
		t.Fatal(key, err)
	}
	request := func(method, path, body, token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w
	}
	if w := request("GET", "/v1/models", "", created.Key); w.Code != 200 || !strings.Contains(w.Body.String(), `"data":[]`) {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := request("POST", "/v1/chat/completions", `{"model":"chatgpt","messages":[{"role":"user","content":"hi"}]}`, created.Key); w.Code != 403 {
		t.Fatal(w.Code, w.Body.String())
	}
	// Walk actual registered routes: no operator-only or master route may escape.
	err = chi.Walk(s.Handler().(chi.Router), func(method, path string, _ http.Handler, mws ...func(http.Handler) http.Handler) error {
		if !strings.HasPrefix(path, "/v1/") {
			return nil
		}
		denied := false
		for _, mw := range mws {
			name := runtime.FuncForPC(reflect.ValueOf(mw).Pointer()).Name()
			denied = denied || strings.Contains(name, "operatorKeysOnly") || strings.Contains(name, "masterOnly")
		}
		if denied {
			w := request(method, strings.ReplaceAll(path, "{id}", "test"), `{}`, created.Key)
			if w.Code != 403 {
				t.Errorf("service accessed %s %s: %d %s", method, path, w.Code, w.Body.String())
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	mutate := func(path, body string) {
		t.Helper()
		w := serviceAdminRequest(s, "PUT", path, "signed", body, true)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	mutate("/portal/admin/api/models", `{"key_id":"`+created.KeyID+`","revision":1,"models":["chatgpt"]}`)
	if w := request("GET", "/v1/models", "", created.Key); w.Code != 200 || !strings.Contains(w.Body.String(), "chatgpt") {
		t.Fatal(w.Code, w.Body.String())
	}
	mutate("/portal/admin/api/state", `{"key_id":"`+created.KeyID+`","disabled":true}`)
	if w := request("GET", "/v1/models", "", created.Key); w.Code != 401 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = serviceAdminRequest(s, "POST", "/portal/admin/api/service-keys", "signed", `{"name":"worker"}`, true)
	if w.Code != 409 {
		t.Fatal(w.Code, w.Body.String())
	}
	mutate("/portal/admin/api/state", `{"key_id":"`+created.KeyID+`","disabled":false}`)
	mutate("/portal/admin/api/models", `{"key_id":"`+created.KeyID+`","revision":1,"models":[]}`)
	if w := request("GET", "/v1/models", "", created.Key); !strings.Contains(w.Body.String(), `"data":[]`) {
		t.Fatal(w.Body.String())
	}
	if w := request("GET", "/v1/models", "", "legacy"); w.Code != 200 || !strings.Contains(w.Body.String(), "chatgpt") {
		t.Fatal("legacy semantics changed", w.Code, w.Body.String())
	}
	for _, path := range []string{"/portal/admin/api/overview", "/portal/admin/api/monitoring?view=full", "/portal/admin/api/monitoring?view=summary", "/portal/admin/api/monitoring?view=requests", "/portal/admin/api/monitoring?view=options", "/portal/api/me"} {
		w := serviceAdminRequest(s, "GET", path, "signed", "", false)
		if w.Code != 200 {
			t.Fatal(path, w.Code, w.Body.String())
		}
		for _, secret := range []string{created.Key, hashAPIKey(created.Key), "key_hash", "metadata"} {
			if strings.Contains(w.Body.String(), secret) {
				t.Fatal("secret leaked", path, secret)
			}
		}
	}
	keys, err := st.ListKeys(ctx)
	if err != nil || len(keys) != 2 {
		t.Fatal("failed create left rows", len(keys), err)
	}
}

func TestServiceCreateOriginAndCSRFGate(t *testing.T) {
	s, st := portalTestServer(t)
	for _, mode := range []string{"origin", "cookie", "csrf", "content-type", "fetch-site", "host"} {
		t.Run(mode, func(t *testing.T) {
			r := httptest.NewRequest("POST", "https://portal.example.com/portal/admin/api/service-keys", strings.NewReader(`{"name":"worker"}`))
			r.Header.Set("Cf-Access-Jwt-Assertion", "signed")
			r.Header.Set("Origin", s.portal.Origin)
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("X-CSRF-Token", strings.Repeat("a", 43))
			if mode != "cookie" {
				r.AddCookie(&http.Cookie{Name: portalCSRFCookie, Value: strings.Repeat("a", 43)})
			}
			switch mode {
			case "origin":
				r.Header.Set("Origin", "https://evil.example")
			case "csrf":
				r.Header.Set("X-CSRF-Token", strings.Repeat("b", 43))
			case "content-type":
				r.Header.Set("Content-Type", "text/plain")
			case "fetch-site":
				r.Header.Set("Sec-Fetch-Site", "cross-site")
			case "host":
				r.Host = "evil.example"
			}
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, r)
			want := 403
			if mode == "content-type" {
				want = 415
			}
			if w.Code != want {
				t.Fatal(w.Code, w.Body.String())
			}
		})
	}
	keys, err := st.ListKeys(context.Background())
	if err != nil || len(keys) != 0 {
		t.Fatal(keys, err)
	}
}
