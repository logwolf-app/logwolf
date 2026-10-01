package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// The broker serves public routes (the SDK's, which take an API key, and the
// health checks) and dashboard routes, which take the internal secret and then
// believe X-User-Login. Caddy is what keeps the second kind off the internet:
// it forwards only the paths its @public matcher lists. These tests read that
// matcher out of the real Caddyfile and hold it against the routes the broker
// actually serves, so neither can change without the other.

// caddyPublicPaths returns the path patterns of the Caddyfile's @public matcher.
func caddyPublicPaths(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile("../../../Caddyfile")
	if err != nil {
		t.Fatalf("read Caddyfile: %v", err)
	}
	m := regexp.MustCompile(`(?m)^\s*@public\s+path\s+(.+)$`).FindStringSubmatch(string(raw))
	if m == nil {
		t.Fatal("the Caddyfile has no `@public path ...` matcher")
	}
	return strings.Fields(m[1])
}

// caddyForwards reports whether Caddy's path matcher lets path through: an
// exact pattern, or one ending in /* that matches its prefix. That is all the
// Caddyfile uses; Caddy also cleans the path first, which keeps ../ tricks out.
func caddyForwards(patterns []string, path string) bool {
	for _, p := range patterns {
		if prefix, ok := strings.CutSuffix(p, "*"); ok {
			if strings.HasPrefix(path, prefix) {
				return true
			}
		} else if path == p {
			return true
		}
	}
	return false
}

type brokerRoute struct {
	method  string
	pattern string // as routes.go declares it, e.g. /logs/{id}
	path    string // pattern with its parameters filled in

	internal bool // takes the internal secret: a dashboard route
	apiKey   bool // takes an API key: an SDK route
}

// brokerRoutes lists every route the broker serves, sorted into public and
// internal by how the broker answers it with no credentials at all: the
// internal-secret check answers "unauthorized", the API key check asks for a
// Bearer header, and the health checks answer.
func brokerRoutes(t *testing.T) []brokerRoute {
	t.Helper()
	// Every SDK route counts the request as a failed authentication; enough of
	// them and the address gets a 429, which reads as neither kind.
	resetAuthCaches(t)
	t.Setenv("INTERNAL_API_SECRET", "caddy-test-secret")
	// /health asks the logger; a closed port answers at once.
	t.Setenv("LOGGER_RPC_ADDR", "127.0.0.1:1")
	h := (&Config{}).routes()

	var routes []brokerRoute
	fill := strings.NewReplacer("{id}", "aaaaaaaaaaaaaaaaaaaaaaa1", "{logID}", "bbbbbbbbbbbbbbbbbbbbbbb2",
		"{keyID}", "ccccccccccccccccccccccc3", "{login}", "octocat")
	walk := func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		path := fill.Replace(route)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(method, path, nil))
		body := w.Body.String()
		routes = append(routes, brokerRoute{
			method: method, pattern: route, path: path,
			internal: strings.Contains(body, `"unauthorized"`),
			apiKey:   strings.Contains(body, "Authorization header"),
		})
		return nil
	}
	if err := chi.Walk(h.(chi.Routes), walk); err != nil {
		t.Fatalf("walk routes: %v", err)
	}
	// Answered by middleware rather than a route, so chi.Walk does not see it.
	routes = append(routes, brokerRoute{method: http.MethodGet, pattern: "/ping", path: "/ping"})
	return routes
}

func TestCaddy_ForwardsEveryPublicRouteAndNoInternalOne(t *testing.T) {
	patterns := caddyPublicPaths(t)

	var public, internal int
	for _, r := range brokerRoutes(t) {
		forwarded := caddyForwards(patterns, "/api"+r.path)
		switch {
		case r.internal && forwarded:
			t.Errorf("%s %s is a dashboard route, but Caddy forwards /api%s from the internet", r.method, r.path, r.path)
		case !r.internal && !forwarded:
			t.Errorf("%s %s is public, but Caddy does not forward /api%s: SDK clients would get a 404", r.method, r.path, r.path)
		}
		if r.internal {
			internal++
		} else {
			public++
		}
	}

	// Guards the classification itself: had it put every route on one side,
	// the checks above would pass without testing anything.
	if public < 5 || internal < 10 {
		t.Errorf("classified %d public and %d internal routes; expected the SDK routes and health checks public, the dashboard's internal", public, internal)
	}
}
