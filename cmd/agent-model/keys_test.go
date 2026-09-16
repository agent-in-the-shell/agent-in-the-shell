package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestKeysCreateTokenOnly(t *testing.T) {
	var gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if r.Method != http.MethodPost || r.URL.Path != "/v1/keys" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		var body keyCreateBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Name != "ci" || strings.Join(body.Models, ",") != "gpt-a,gpt-b" {
			t.Fatalf("body = %+v", body)
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"id":"key_1","name":"ci","key":"sk-am-secret"}`)
	}))
	defer ts.Close()

	var out, errOut strings.Builder
	err := doKeys([]string{"create", "--base-url", ts.URL, "--token", "master", "--name", "ci", "--models", "gpt-a,gpt-b", "--token-only"}, &out, &errOut)
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer master" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if got := out.String(); got != "sk-am-secret\n" {
		t.Fatalf("stdout = %q", got)
	}
	if errOut.Len() != 0 {
		t.Fatalf("stderr = %q", errOut.String())
	}
}

func TestKeysRotateKeepsOldByDefault(t *testing.T) {
	var revoked bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/keys/key_old":
			fmt.Fprint(w, `{"id":"key_old","name":"svc","models":["gpt-a"],"budget_duration":"24h","metadata":{"owner":"ops"},"created_at":1}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/keys":
			var body keyCreateBody
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			var meta map[string]any
			if err := json.Unmarshal(body.Metadata, &meta); err != nil {
				t.Fatal(err)
			}
			if body.Name != "svc" || meta["rotated_from"] != "key_old" {
				t.Fatalf("rotation body = %+v metadata=%v", body, meta)
			}
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":"key_new","name":"svc","key":"sk-am-new"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/keys/key_old/revoke":
			revoked = true
			fmt.Fprint(w, `{"id":"key_old","name":"svc","disabled":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	var out, errOut strings.Builder
	if err := doKeys([]string{"rotate", "--base-url", ts.URL, "--token", "master", "--token-only", "key_old"}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if revoked {
		t.Fatal("old key was revoked without --revoke-old")
	}
	if out.String() != "sk-am-new\n" {
		t.Fatalf("stdout = %q", out.String())
	}
}

func TestKeysRequireMasterToken(t *testing.T) {
	t.Setenv("AGENT_MODEL_TOKEN", "")
	var out, errOut strings.Builder
	err := doKeys([]string{"list"}, &out, &errOut)
	if err == nil || !strings.Contains(err.Error(), "master token is required") {
		t.Fatalf("error = %v", err)
	}
}

func TestKeysRotateRejectsOwnedAndRevokedBeforeCreating(t *testing.T) {
	for _, response := range []string{`{"id":"key","kind":"portal"}`, `{"id":"key","kind":"legacy","revoked_at":123}`} {
		t.Run(response, func(t *testing.T) {
			writes := 0
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					writes++
				}
				fmt.Fprint(w, response)
			}))
			defer ts.Close()
			var out, errOut strings.Builder
			if err := doKeys([]string{"rotate", "--base-url", ts.URL, "--token", "master", "key"}, &out, &errOut); err == nil {
				t.Fatal("unsafe rotation accepted")
			}
			if writes != 0 || out.Len() != 0 {
				t.Fatal("created replacement", writes, out.String())
			}
		})
	}
}

func TestKeyStatusDistinguishesDisabledRevokedAndExpired(t *testing.T) {
	timestamp := int64(1)
	for _, tc := range []struct {
		key   keyCLIView
		state string
	}{
		{keyCLIView{Disabled: true}, "disabled"},
		{keyCLIView{Disabled: true, RevokedAt: &timestamp}, "revoked"},
		{keyCLIView{ExpiresAt: &timestamp}, "expired"},
	} {
		var out strings.Builder
		if err := printKeyView(&out, tc.key); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "Status   "+tc.state) {
			t.Fatal(out.String())
		}
	}
}

func TestKeysRejectsUnrevoke(t *testing.T) {
	var out, errOut strings.Builder
	err := doKeys([]string{"unrevoke", "key"}, &out, &errOut)
	if err == nil || err.Error() != `unknown keys subcommand "unrevoke"` {
		t.Fatalf("error = %v, want unknown subcommand", err)
	}
	if out.Len() != 0 {
		t.Fatalf("stdout = %q", out.String())
	}
	if strings.Contains(usage, "unrevoke") {
		t.Fatal("help advertises removed unrevoke subcommand")
	}
	err = doKeys(nil, &out, &errOut)
	if err == nil || strings.Contains(err.Error(), "unrevoke") {
		t.Fatalf("subcommand help = %v", err)
	}
}

func TestKeysRotateServicePreservesIDAndAlwaysRetiresOldSecret(t *testing.T) {
	for _, flags := range [][]string{nil, {"--revoke-old"}} {
		t.Run(fmt.Sprint(flags), func(t *testing.T) {
			writes := 0
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" && r.URL.Path == "/v1/keys/svc" {
					fmt.Fprint(w, `{"id":"svc","name":"Worker","kind":"service","revision":4,"revoked_at":123}`)
					return
				}
				if r.Method != "POST" || r.URL.Path != "/v1/keys/svc/rotate" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(500)
					return
				}
				var body struct {
					Revision int64 `json:"revision"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Revision != 4 {
					t.Error(body, err)
				}
				writes++
				fmt.Fprint(w, `{"id":"svc","name":"Worker","kind":"service","revision":5,"key":"sk-am-new"}`)
			}))
			defer ts.Close()
			args := append([]string{"rotate", "--base-url", ts.URL, "--token", "master", "--token-only"}, flags...)
			args = append(args, "svc")
			var out, errOut strings.Builder
			if err := doKeys(args, &out, &errOut); err != nil {
				t.Fatal(err)
			}
			if writes != 1 || out.String() != "sk-am-new\n" || errOut.Len() != 0 {
				t.Fatal(writes, out.String(), errOut.String())
			}
		})
	}
}
