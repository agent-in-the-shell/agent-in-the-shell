package api_test

import (
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/api"
)

// docPath is docs/agentmodel.md relative to this package directory
// (services/agentmodel/api).
const docPath = "../../../docs/agentmodel.md"

// docRowRE matches an API-table row whose first two cells are a backticked HTTP
// method and a backticked /v1 path, e.g. | `POST` | `/v1/keys` | ... |.
var docRowRE = regexp.MustCompile("^\\|\\s*`(GET|POST|PUT|DELETE|PATCH)`\\s*\\|\\s*`(/v1/[^`]+)`")

// TestDocEndpointsMatchRoutes keeps the "## API" table in docs/agentmodel.md
// honest. It walks the live chi router and asserts the documented set of
// `/v1` endpoints exactly equals the registered set — so adding or removing a
// route without updating the doc fails CI, instead of letting the user-facing
// table silently rot (the recurring drift #922 surfaced). Descriptions stay
// human-owned; only presence of each METHOD+path is enforced.
func TestDocEndpointsMatchRoutes(t *testing.T) {
	routes := registeredV1Routes(t)
	documented := documentedV1Endpoints(t)

	for _, ep := range sortedKeys(routes) {
		if !documented[ep] {
			t.Errorf("route %q is registered but missing from the API table in %s — add a row", ep, docPath)
		}
	}
	for _, ep := range sortedKeys(documented) {
		if !routes[ep] {
			t.Errorf("API table in %s documents %q, which is not a registered route — remove the row", docPath, ep)
		}
	}
}

// registeredV1Routes walks the router and returns the set of "METHOD /v1/path"
// endpoints. Route registration is config-independent, so a zero-value Server
// exposes the full surface.
func registeredV1Routes(t *testing.T) map[string]bool {
	t.Helper()
	h := api.New(api.Config{}).Handler()
	rt, ok := h.(chi.Routes)
	if !ok {
		t.Fatalf("handler is %T, not chi.Routes", h)
	}
	out := map[string]bool{}
	err := chi.Walk(rt, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if strings.HasPrefix(route, "/v1/") {
			out[method+" "+route] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk routes: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("no /v1 routes discovered — router construction or walk is broken")
	}
	return out
}

// documentedV1Endpoints parses the markdown API table and returns the set of
// "METHOD /v1/path" rows.
func documentedV1Endpoints(t *testing.T) map[string]bool {
	t.Helper()
	data, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatalf("read %s: %v", docPath, err)
	}
	out := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		if m := docRowRE.FindStringSubmatch(line); m != nil {
			out[m[1]+" "+m[2]] = true
		}
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
