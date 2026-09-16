package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/store"
)

func TestPortalMePersonalStats(t *testing.T) {
	s, st := portalTestServer(t)
	ctx := context.Background()
	h := s.Handler()
	readMe := func(assertion string, want [4]int64) {
		t.Helper()
		// Client-supplied identity/key selectors must not affect ownership or dates.
		r := httptest.NewRequest(http.MethodGet, "https://portal.example.com/portal/api/me?subject=other&issuer=evil&email=other@example.com&key_id=other&api_key_hash=other-hash&since=0", nil)
		r.Header.Set("Cf-Access-Jwt-Assertion", assertion)
		r.Header.Set("Cf-Access-Authenticated-User-Email", "other@example.com")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal(w.Code, w.Body.String())
		}
		var me struct {
			Stats    map[string]int64     `json:"stats"`
			Calendar store.PortalCalendar `json:"calendar"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &me); err != nil {
			t.Fatal(err)
		}
		if len(me.Stats) != 4 {
			t.Fatalf("want four personal statistics, got %s", w.Body.String())
		}
		got := [4]int64{me.Stats["request_count"], me.Stats["prompt_tokens"], me.Stats["completion_tokens"], me.Stats["total_tokens"]}
		if got != want {
			t.Fatalf("%s stats = %v, want %v", assertion, got, want)
		}
		if me.Calendar.Timezone != "UTC" || len(me.Calendar.Days) < 365 {
			t.Fatalf("missing UTC year calendar: %+v", me.Calendar)
		}
		today := time.Now().UTC().Format("2006-01-02")
		found := false
		for _, day := range me.Calendar.Days {
			if day.Date == today {
				found = true
				if day.TotalTokens == nil || day.Requests == nil || *day.TotalTokens != want[3] || *day.Requests != want[0] {
					t.Fatalf("%s calendar not isolated: %+v", assertion, day)
				}
			}
			if day.Date > today && (day.TotalTokens != nil || day.Requests != nil) {
				t.Fatal("future calendar counts exposed", day)
			}
		}
		if !found {
			t.Fatal("calendar omitted today")
		}
		for _, secret := range []string{"personal-secret", "rotated-secret", hashAPIKey("personal-secret"), hashAPIKey("rotated-secret"), "other-hash", "key_hash"} {
			if strings.Contains(w.Body.String(), secret) {
				t.Fatalf("me exposed credential/hash %q", secret)
			}
		}
	}
	readMe("signed", [4]int64{})
	for _, subject := range []string{"signed", "other"} {
		hash := "other-hash"
		if subject == "signed" {
			hash = hashAPIKey("personal-secret")
		}
		identity := store.PortalIdentity{Issuer: "https://team.cloudflareaccess.com", Subject: subject, Email: subject + "@example.com"}
		if _, err := st.CreatePortalKey(ctx, identity, store.ManagedKey{ID: subject, Name: identity.Email, KeyHash: hash, Models: []string{"chatgpt"}}); err != nil {
			t.Fatal(err)
		}
		if subject == "signed" {
			readMe("signed", [4]int64{})
			if _, err := st.RotatePortalKey(ctx, identity, 1, hashAPIKey("rotated-secret")); err != nil {
				t.Fatal(err)
			}
		}
		// The old hash finishes after rotation; errors are included as logged too.
		if err := st.LogRequest(ctx, store.RequestLog{ID: subject, APIKeyHash: hash, PromptTokens: 11, CompletionTokens: 7, TotalTokens: 23, CacheReadInputTokens: 100, CacheCreationInputTokens: 200, Status: "error"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.LogRequest(ctx, store.RequestLog{ID: "expired", APIKeyHash: hashAPIKey("personal-secret"), PromptTokens: 900, TotalTokens: 900, CreatedAt: time.Now().Add(-31 * 24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := st.LogRequest(ctx, store.RequestLog{ID: "other-extra", APIKeyHash: "other-hash", PromptTokens: 1000, TotalTokens: 1000}); err != nil {
		t.Fatal(err)
	}
	readMe("signed", [4]int64{1, 11, 7, 23})
	readMe("other", [4]int64{2, 1011, 7, 1023})
}

func TestPortalMeReportingTimezoneContract(t *testing.T) {
	s, _ := portalTestServer(t)
	for _, tc := range []struct {
		query, zone string
		status      int
	}{
		{"", "UTC", 200}, {"?timezone=UTC", "UTC", 200}, {"?timezone=Asia%2FTaipei", "Asia/Taipei", 200},
		{"?timezone=America%2FLos_Angeles", "", 400}, {"?timezone=", "", 400}, {"?timezone=UTC&timezone=Asia%2FTaipei", "", 400}, {"?timezone=%zz", "", 400},
	} {
		r := httptest.NewRequest("GET", "https://portal.example.com/portal/api/me"+tc.query, nil)
		r.Header.Set("Cf-Access-Jwt-Assertion", "signed")
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatal(tc.query, w.Code, w.Body.String())
		}
		if tc.status != 200 {
			continue
		}
		var data struct {
			Calendar store.PortalCalendar `json:"calendar"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &data); err != nil {
			t.Fatal(err)
		}
		loc, _ := store.PortalReportingLocation(tc.zone)
		if data.Calendar.Timezone != tc.zone || data.Calendar.Year != time.Now().In(loc).Year() {
			t.Fatal(tc.query, data.Calendar)
		}
	}
}
