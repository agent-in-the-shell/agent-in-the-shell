package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/router"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/store"
)

func TestAdminServiceRotationAndHistorySecurity(t *testing.T) {
	s, st := portalTestServer(t)
	ctx := context.Background()
	s.router = router.New(map[string][]router.Deployment{"chatgpt": {}}, nil)
	created := serviceAdminRequest(s, "POST", "/portal/admin/api/service-keys", "signed", `{"name":"Worker"}`, true)
	var key struct {
		KeyID    string `json:"key_id"`
		Key      string `json:"key"`
		Revision int64  `json:"revision"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &key); err != nil || created.Code != 201 {
		t.Fatal(created.Code, err)
	}
	body := fmt.Sprintf(`{"key_id":%q,"revision":1}`, key.KeyID)
	for _, tc := range []struct {
		method, path, assertion, body string
		csrf                          bool
		want                          int
	}{
		{"GET", "/portal/admin/api/history?key_id=" + key.KeyID, "", "", false, 401},
		{"GET", "/portal/admin/api/history?key_id=" + key.KeyID, "other", "", false, 403},
		{"POST", "/portal/admin/api/service-keys/rotate", "other", body, true, 403},
		{"POST", "/portal/admin/api/service-keys/rotate", "signed", body, false, 403},
		{"POST", "/portal/admin/api/service-keys/rotate", "signed", fmt.Sprintf(`{"key_id":%q,"revision":1,"actor":"evil"}`, key.KeyID), true, 400},
		{"POST", "/portal/admin/api/service-keys/rotate", "signed", fmt.Sprintf(`{"key_id":%q}`, key.KeyID), true, 400},
		{"GET", "/portal/admin/api/history?key_id=" + key.KeyID + "&before=-1", "signed", "", false, 400},
		{"POST", "/portal/admin/api/history", "signed", `{}`, true, 405},
	} {
		w := serviceAdminRequest(s, tc.method, tc.path, tc.assertion, tc.body, tc.csrf)
		if w.Code != tc.want {
			t.Fatalf("%+v: %d %s", tc, w.Code, w.Body.String())
		}
	}
	for _, method := range []string{"PUT"} {
		w := serviceAdminRequest(s, method, "/portal/admin/api/models", "signed", fmt.Sprintf(`{"key_id":%q,"models":["chatgpt"],"revision":1}`, key.KeyID), true)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	w := serviceAdminRequest(s, "POST", "/portal/admin/api/service-keys/rotate", "signed", body, true)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal(w.Code, w.Body.String())
	}
	var rotated struct {
		KeyID    string   `json:"key_id"`
		Key      string   `json:"key"`
		Revision int64    `json:"revision"`
		Models   []string `json:"models"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &rotated); err != nil || rotated.Key == key.Key || rotated.KeyID != key.KeyID || rotated.Revision != 2 || len(rotated.Models) != 1 {
		t.Fatal(rotated, err)
	}
	auth := func(token string) int {
		r := httptest.NewRequest("GET", "/v1/models", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w.Code
	}
	if auth(key.Key) != 401 || auth(rotated.Key) != 200 {
		t.Fatal("rotation auth failure")
	}
	if w := serviceAdminRequest(s, "POST", "/portal/admin/api/service-keys/rotate", "signed", body, true); w.Code != 409 {
		t.Fatal(w.Code)
	}
	if w := serviceAdminRequest(s, "PUT", "/portal/admin/api/models", "signed", fmt.Sprintf(`{"key_id":%q,"models":["chatgpt"],"revision":1}`, key.KeyID), true); w.Code != 409 {
		t.Fatal(w.Code)
	}
	w = serviceAdminRequest(s, "GET", "/portal/admin/api/history?key_id="+key.KeyID, "signed", "", false)
	var result struct {
		Events []store.KeyEvent `json:"events"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != 200 || len(result.Events) != 3 {
		t.Fatal(w.Code, w.Body.String(), err)
	}
	for _, e := range result.Events {
		if e.KeyID != key.KeyID || e.Actor.Kind != "admin" || e.Actor.ID == "" || e.Actor.ID != result.Events[0].Actor.ID {
			t.Fatal(e)
		}
	}
	for _, secret := range []string{key.Key, rotated.Key, hashAPIKey(key.Key), "signed@example.com", "https://team.cloudflareaccess.com", "key_hash"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatal("history leaked sensitive value")
		}
	}
	if strings.Contains(w.Body.String(), "Worker") {
		t.Fatal("name copied into history")
	}
	if _, err := st.GetKeyByHash(ctx, hashAPIKey(rotated.Key)); err != nil {
		t.Fatal(err)
	}
}

func TestMasterServiceRotationUsesSharedActorAndCannotRotatePortal(t *testing.T) {
	s, st := portalTestServer(t)
	ctx := context.Background()
	if _, err := st.CreateServiceKey(ctx, store.ManagedKey{ID: "svc", Name: "Worker", KeyHash: hashAPIKey("old")}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreatePortalKey(ctx, store.PortalIdentity{Issuer: "issuer", Subject: "user", Email: "user@example.com"}, store.ManagedKey{ID: "user", Name: "user", KeyHash: hashAPIKey("user")}); err != nil {
		t.Fatal(err)
	}
	call := func(token, id string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/v1/keys/"+id+"/rotate", strings.NewReader(`{"revision":1}`))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w
	}
	if w := call("old", "svc"); w.Code != 403 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := call("master", "user"); w.Code != 409 {
		t.Fatal(w.Code, w.Body.String())
	}
	w := call("master", "svc")
	if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal(w.Code, w.Body.String())
	}
	events, err := st.GetKeyHistory(ctx, "svc", 0)
	if err != nil || events[0].Action != "rotate" || events[0].Actor.Kind != "master" || events[0].Actor.ID != "shared-master" {
		t.Fatal(events, err)
	}
}
