package api

import (
	"net/http"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// Every /v1 route must declare its employee-key policy at registration: either
// employeeKeysAllowed or operatorKeysOnly (masterOnly implies the latter). A
// route wrapped in neither, or in both, is a policy gap this test turns into a
// failure instead of a silent default.
func TestEveryV1RouteClassifiesEmployeeKeys(t *testing.T) {
	s := New(Config{BearerToken: "master"})
	rt, ok := s.Handler().(chi.Router)
	if !ok {
		t.Fatal("handler is not a chi router")
	}
	allowed, denied := map[string]bool{}, map[string]bool{}
	err := chi.Walk(rt, func(method, route string, _ http.Handler, middlewares ...func(http.Handler) http.Handler) error {
		if !strings.HasPrefix(route, "/v1/") {
			return nil
		}
		key := method + " " + route
		for _, mw := range middlewares {
			name := runtime.FuncForPC(reflect.ValueOf(mw).Pointer()).Name()
			switch {
			case strings.Contains(name, "employeeKeysAllowed"):
				allowed[key] = true
			case strings.Contains(name, "operatorKeysOnly"), strings.Contains(name, "masterOnly"):
				denied[key] = true
			}
		}
		if allowed[key] == denied[key] {
			t.Errorf("%s: wrapped in %v employee policies, want exactly one", key, map[bool]int{false: 0, true: 2}[allowed[key]])
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// The allowlist itself is part of the contract: adding a route here means
	// employees may call it.
	want := []string{"POST /v1/chat/completions", "POST /v1/responses", "POST /v1/messages", "POST /v1/embeddings", "POST /v1/images/generations", "GET /v1/models"}
	if len(allowed) != len(want) {
		t.Fatalf("employee-allowed routes = %v, want %v", allowed, want)
	}
	for _, key := range want {
		if !allowed[key] {
			t.Errorf("%s should allow employee keys", key)
		}
	}
}
