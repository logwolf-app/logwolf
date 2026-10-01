package main

import (
	"encoding/json"
	"fmt"
	"logwolf-toolbox/data"
	"mime"
	"net/http"
	"os"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// openapi.yaml, at the repository root, is the contract of the broker's public
// routes: what the SDK and any other client may rely on. These tests hold it
// against the broker the way caddy_test.go holds the Caddyfile, so neither can
// change without the other: a public route the spec leaves out, an operation
// the broker does not serve, a scope or a bound the code disagrees with, or a
// status the broker answers that the spec does not list, with a body of
// another shape, fails them.

const openAPIFile = "../../../openapi.yaml"

// openAPI is the parsed spec, as generic YAML values: the tests walk a few
// parts of it, and a $ref can stand in for almost any of them.
type openAPI struct {
	root map[string]any
}

func loadOpenAPI(t *testing.T) openAPI {
	t.Helper()
	raw, err := os.ReadFile(openAPIFile)
	if err != nil {
		t.Fatalf("read openapi.yaml: %v", err)
	}
	var root map[string]any
	if err := yaml.Unmarshal(raw, &root); err != nil {
		t.Fatalf("parse openapi.yaml: %v", err)
	}
	return openAPI{root}
}

// lookup follows a local reference, #/components/schemas/Event, to its node.
func (s openAPI) lookup(ref string) (any, bool) {
	pointer, ok := strings.CutPrefix(ref, "#/")
	if !ok {
		return nil, false
	}
	var node any = s.root
	for _, part := range strings.Split(pointer, "/") {
		part = strings.NewReplacer("~1", "/", "~0", "~").Replace(part)
		m, ok := node.(map[string]any)
		if !ok {
			return nil, false
		}
		if node, ok = m[part]; !ok {
			return nil, false
		}
	}
	return node, true
}

// resolve returns node as a map, its $ref followed if it has one.
func (s openAPI) resolve(node any) map[string]any {
	m, _ := node.(map[string]any)
	for m != nil {
		ref, ok := m["$ref"].(string)
		if !ok {
			return m
		}
		target, _ := s.lookup(ref)
		m, _ = target.(map[string]any)
	}
	return m
}

var httpMethods = []string{"get", "put", "post", "delete", "options", "head", "patch", "trace"}

// operations returns every operation in the spec, keyed "GET /logs/{id}".
func (s openAPI) operations() map[string]map[string]any {
	ops := map[string]map[string]any{}
	paths, _ := s.root["paths"].(map[string]any)
	for path, item := range paths {
		for method, op := range s.resolve(item) {
			if slices.Contains(httpMethods, method) {
				ops[strings.ToUpper(method)+" "+path] = s.resolve(op)
			}
		}
	}
	return ops
}

// parameter returns the operation's parameter of the given name.
func (s openAPI) parameter(op map[string]any, name string) map[string]any {
	params, _ := op["parameters"].([]any)
	for _, p := range params {
		if p := s.resolve(p); p["name"] == name {
			return p
		}
	}
	return nil
}

// requestSchema returns the schema of the operation's JSON request body.
func (s openAPI) requestSchema(op map[string]any) map[string]any {
	content, _ := s.resolve(op["requestBody"])["content"].(map[string]any)
	return s.resolve(s.resolve(content["application/json"])["schema"])
}

func TestOpenAPI_IsWellFormed(t *testing.T) {
	spec := loadOpenAPI(t)

	if v, _ := spec.root["openapi"].(string); !strings.HasPrefix(v, "3.1.") {
		t.Errorf("openapi = %q, want 3.1.x: the security requirements list scopes for a bearer scheme, which 3.0 does not allow", v)
	}

	// Every reference names something.
	var walk func(node any, at string)
	walk = func(node any, at string) {
		switch n := node.(type) {
		case map[string]any:
			if ref, ok := n["$ref"].(string); ok {
				if _, found := spec.lookup(ref); !found {
					t.Errorf("%s: $ref %q names nothing", at, ref)
				}
			}
			for k, v := range n {
				walk(v, at+"/"+k)
			}
		case []any:
			for i, v := range n {
				walk(v, fmt.Sprintf("%s/%d", at, i))
			}
		}
	}
	walk(spec.root, "#")

	schemes, _ := spec.resolve(spec.root["components"])["securitySchemes"].(map[string]any)
	pathParam := regexp.MustCompile(`\{([^}]+)\}`)
	ids := map[string]string{}
	for key, op := range spec.operations() {
		id, _ := op["operationId"].(string)
		if id == "" {
			t.Errorf("%s has no operationId", key)
		} else if other, dup := ids[id]; dup {
			t.Errorf("%s and %s share the operationId %q", key, other, id)
		}
		ids[id] = key

		// Stated on each operation, so none falls back on a global default.
		security, ok := op["security"].([]any)
		if !ok {
			t.Errorf("%s does not state its security; say `security: []` for none", key)
		}
		for _, req := range security {
			for name := range spec.resolve(req) {
				if _, ok := schemes[name]; !ok {
					t.Errorf("%s names the security scheme %q, which components.securitySchemes does not define", key, name)
				}
			}
		}

		if len(spec.resolve(op["responses"])) == 0 {
			t.Errorf("%s documents no responses", key)
		}

		for _, m := range pathParam.FindAllStringSubmatch(key, -1) {
			if p := spec.parameter(op, m[1]); p == nil || p["in"] != "path" || p["required"] != true {
				t.Errorf("%s has no required path parameter %q", key, m[1])
			}
		}
	}
}

// TestOpenAPI_DescribesEveryPublicRoute: the spec's operations are exactly the
// broker's public routes, the ones caddy_test.go makes sure Caddy forwards.
func TestOpenAPI_DescribesEveryPublicRoute(t *testing.T) {
	ops := loadOpenAPI(t).operations()

	public := map[string]bool{}
	for _, r := range brokerRoutes(t) {
		if r.internal {
			continue
		}
		key := r.method + " " + r.pattern
		public[key] = true
		if _, ok := ops[key]; !ok {
			t.Errorf("%s is public, but openapi.yaml does not describe it", key)
		}
	}
	for key := range ops {
		if !public[key] {
			t.Errorf("openapi.yaml describes %s, which the broker does not serve publicly", key)
		}
	}
}

// TestOpenAPI_SecurityMatchesTheRoutes: an operation asks for an API key with
// the scope its route demands (scope_test.go's publicRoutes, which
// TestPublicRoutes_EachRouteDemandsItsScope checks against the router), and
// one whose route takes no key says so.
func TestOpenAPI_SecurityMatchesTheRoutes(t *testing.T) {
	ops := loadOpenAPI(t).operations()

	scopes := map[string]string{}
	for _, rt := range publicRoutes {
		scopes[rt.method+" "+strings.ReplaceAll(rt.target, alphaLogID, "{id}")] = rt.scope
	}

	for _, r := range brokerRoutes(t) {
		key := r.method + " " + r.pattern
		op, ok := ops[key]
		if r.internal || !ok {
			continue // TestOpenAPI_DescribesEveryPublicRoute reports it
		}

		var want []any
		if r.apiKey {
			scope, ok := scopes[key]
			if !ok {
				t.Errorf("%s takes an API key, but scope_test.go's publicRoutes does not say which scope", key)
				continue
			}
			want = []any{map[string]any{"apiKey": []any{scope}}}
		} else {
			want = []any{}
		}
		if got := op["security"]; !reflect.DeepEqual(got, want) {
			t.Errorf("%s: security = %v, want %v", key, got, want)
		}
	}
}

// TestOpenAPI_LimitsMatchTheBroker: the bounds the spec states are the ones
// the broker enforces.
func TestOpenAPI_LimitsMatchTheBroker(t *testing.T) {
	spec := loadOpenAPI(t)
	ops := spec.operations()

	list := ops["GET /logs"]
	for _, tt := range []struct {
		name                   string
		minimum, maximum, dflt int
	}{
		{"page", 1, data.MaxPage, 1},
		{"pageSize", 1, data.MaxPageSize, data.DefaultPageSize},
	} {
		schema := spec.resolve(spec.parameter(list, tt.name)["schema"])
		got := []any{schema["type"], schema["minimum"], schema["maximum"], schema["default"]}
		if want := []any{"integer", tt.minimum, tt.maximum, tt.dflt}; !reflect.DeepEqual(got, want) {
			t.Errorf("GET /logs %s: [type minimum maximum default] = %v, want %v", tt.name, got, want)
		}
	}

	if got := spec.requestSchema(ops["POST /logs/batch"])["maxItems"]; got != maxBatchSize {
		t.Errorf("POST /logs/batch maxItems = %v, want %d", got, maxBatchSize)
	}

	severity, _ := spec.lookup("#/components/schemas/Severity")
	want := []any{data.SeverityInfo, data.SeverityWarning, data.SeverityError, data.SeverityCritical}
	if got := spec.resolve(severity)["enum"]; !reflect.DeepEqual(got, want) {
		t.Errorf("Severity enum = %v, want %v", got, want)
	}
}

// apiCase is one request to a public route and the status the broker answers
// it with.
type apiCase struct {
	name            string
	method, pattern string // the operation, as the spec names it
	target, body    string

	// scopes is the scopes of the key the request carries. With none, auth is
	// sent as the Authorization header instead, which may be empty.
	scopes []string
	auth   string

	setup func(t *testing.T, app *Config)
	want  int
}

func queueFails(_ *testing.T, app *Config) {
	app.Events = &fakePublisher{err: fmt.Errorf("no confirm")}
}

func loggerDown(t *testing.T, _ *Config) {
	t.Setenv("LOGGER_RPC_ADDR", "127.0.0.1:1")
}

func rateLimited(_ *testing.T, _ *Config) {
	ipLimiterMu.Lock()
	defer ipLimiterMu.Unlock()
	// httptest's requests come from 192.0.2.1.
	ipLimiter["192.0.2.1"] = &ipEntry{failures: maxFailures, windowEnd: time.Now().Add(time.Minute)}
}

// apiCases exercises every status the spec documents, on every operation.
func apiCases() []apiCase {
	const unknownKey = "Bearer lw_nosuchkey00000000000000"
	var cases []apiCase

	// The answers every API key route shares, before its handler runs.
	for _, rt := range publicRoutes {
		pattern := strings.ReplaceAll(rt.target, alphaLogID, "{id}")
		others := slices.DeleteFunc(slices.Clone(data.AllScopes), func(s string) bool { return s == rt.scope })
		base := apiCase{method: rt.method, pattern: pattern, target: rt.target, body: rt.body}
		for _, c := range []apiCase{
			{name: "no Authorization header", want: http.StatusUnauthorized},
			{name: "not a Bearer token", auth: "Basic Zm9vOmJhcg==", want: http.StatusUnauthorized},
			{name: "unknown key", auth: unknownKey, want: http.StatusUnauthorized},
			{name: "key without the scope", scopes: others, want: http.StatusForbidden},
			{name: "rate limited", scopes: []string{rt.scope}, setup: rateLimited, want: http.StatusTooManyRequests},
			{name: "key not checkable", auth: unknownKey, setup: loggerDown, want: http.StatusInternalServerError},
		} {
			c.method, c.pattern, c.target, c.body = base.method, base.pattern, base.target, base.body
			cases = append(cases, c)
		}
	}

	ingest, read, del := []string{data.ScopeIngest}, []string{data.ScopeRead}, []string{data.ScopeDelete}
	event := `{"name":"signup","severity":"info","data":"{\"plan\":\"pro\"}","tags":["web"],"duration":12}`
	tooMany := "[" + strings.TrimSuffix(strings.Repeat(`{"severity":"info"},`, maxBatchSize+1), ",") + "]"

	return append(cases, []apiCase{
		{name: "event", method: "POST", pattern: "/logs", body: event, scopes: ingest, want: http.StatusAccepted},
		{name: "severity in capitals", method: "POST", pattern: "/logs", body: `{"name":"x","severity":" ERROR "}`, scopes: ingest, want: http.StatusAccepted},
		{name: "unknown severity", method: "POST", pattern: "/logs", body: `{"name":"x","severity":"fatal"}`, scopes: ingest, want: http.StatusBadRequest},
		{name: "data not a string", method: "POST", pattern: "/logs", body: `{"severity":"info","data":{}}`, scopes: ingest, want: http.StatusBadRequest},
		{name: "malformed JSON", method: "POST", pattern: "/logs", body: `{`, scopes: ingest, want: http.StatusBadRequest},
		{name: "queue fails", method: "POST", pattern: "/logs", body: event, scopes: ingest, setup: queueFails, want: http.StatusServiceUnavailable},

		{name: "two events", method: "POST", pattern: "/logs/batch", body: "[" + event + "," + event + "]", scopes: ingest, want: http.StatusAccepted},
		{name: "no events", method: "POST", pattern: "/logs/batch", body: `[]`, scopes: ingest, want: http.StatusAccepted},
		{name: "one bad severity", method: "POST", pattern: "/logs/batch", body: "[" + event + `,{"severity":"fatal"}]`, scopes: ingest, want: http.StatusBadRequest},
		{name: "not an array", method: "POST", pattern: "/logs/batch", body: event, scopes: ingest, want: http.StatusBadRequest},
		{name: "too many events", method: "POST", pattern: "/logs/batch", body: tooMany, scopes: ingest, want: http.StatusRequestEntityTooLarge},
		{name: "queue fails", method: "POST", pattern: "/logs/batch", body: "[" + event + "]", scopes: ingest, setup: queueFails, want: http.StatusServiceUnavailable},

		{name: "first page", method: "GET", pattern: "/logs", target: "/logs", scopes: read, want: http.StatusOK},
		{name: "largest page", method: "GET", pattern: "/logs", target: fmt.Sprintf("/logs?page=2&pageSize=%d", data.MaxPageSize), scopes: read, want: http.StatusOK},
		{name: "page too large", method: "GET", pattern: "/logs", target: fmt.Sprintf("/logs?pageSize=%d", data.MaxPageSize+1), scopes: read, want: http.StatusBadRequest},
		{name: "page 0", method: "GET", pattern: "/logs", target: "/logs?page=0", scopes: read, want: http.StatusBadRequest},
		{name: "page not a number", method: "GET", pattern: "/logs", target: "/logs?page=two", scopes: read, want: http.StatusBadRequest},
		{name: "logger down", method: "GET", pattern: "/logs", target: "/logs", scopes: read, setup: loggerDown, want: http.StatusInternalServerError},

		{name: "event of the project", method: "GET", pattern: "/logs/{id}", target: "/logs/" + alphaLogID, scopes: read, want: http.StatusOK},
		{name: "event of another project", method: "GET", pattern: "/logs/{id}", target: "/logs/" + betaLogID, scopes: read, want: http.StatusNotFound},
		{name: "malformed id", method: "GET", pattern: "/logs/{id}", target: "/logs/nope", scopes: read, want: http.StatusNotFound},

		{name: "event of the project", method: "DELETE", pattern: "/logs", body: `{"id":"` + alphaLogID + `"}`, scopes: del, want: http.StatusAccepted},
		{name: "event of another project", method: "DELETE", pattern: "/logs", body: `{"id":"` + betaLogID + `"}`, scopes: del, want: http.StatusAccepted},
		{name: "malformed JSON", method: "DELETE", pattern: "/logs", body: `{`, scopes: del, want: http.StatusBadRequest},

		{name: "healthy", method: "GET", pattern: "/health", want: http.StatusOK},
		{name: "queue down", method: "GET", pattern: "/health", setup: queueFails, want: http.StatusServiceUnavailable},

		{name: "running", method: "GET", pattern: "/ping", want: http.StatusOK},
	}...)
}

// TestOpenAPI_DocumentsEveryResponse sends each of apiCases to the real router
// and checks the spec documents its status, with the body the broker sent. It
// also checks every status the spec documents is one a case produced, so the
// spec cannot promise an answer the broker never gives.
func TestOpenAPI_DocumentsEveryResponse(t *testing.T) {
	spec := loadOpenAPI(t)
	ops := spec.operations()

	seen := map[string]bool{}
	for _, c := range apiCases() {
		key := c.method + " " + c.pattern
		target := c.target
		if target == "" {
			target = c.pattern
		}

		t.Run(key+" "+c.name, func(t *testing.T) {
			newInternalTestServer(t)
			resetAuthCaches(t)
			app := &Config{Events: &fakePublisher{}}
			if c.setup != nil {
				c.setup(t, app)
			}

			r := keyRequest(c.method, target, "", c.body)
			if c.scopes != nil {
				r.Header.Set("Authorization", "Bearer "+seedKey(t, projAlpha, c.scopes...))
			} else if c.auth != "" {
				r.Header.Set("Authorization", c.auth)
			} else {
				r.Header.Del("Authorization")
			}
			w := do(app.routes(), r)

			if w.Code != c.want {
				t.Fatalf("status %d, want %d (body: %s)", w.Code, c.want, w.Body.String())
			}
			status := fmt.Sprint(w.Code)
			seen[key+" "+status] = true

			op, ok := ops[key]
			if !ok {
				t.Fatalf("openapi.yaml does not describe %s", key)
			}
			responses := spec.resolve(op["responses"])
			response, ok := responses[status]
			if !ok {
				t.Fatalf("the broker answered %s, which openapi.yaml does not document for %s (body: %s)", status, key, w.Body.String())
			}

			mediaType, _, err := mime.ParseMediaType(w.Header().Get("Content-Type"))
			if err != nil {
				t.Fatalf("Content-Type %q: %v", w.Header().Get("Content-Type"), err)
			}
			content, _ := spec.resolve(response)["content"].(map[string]any)
			media, ok := content[mediaType]
			if !ok {
				t.Fatalf("the broker answered %s as %s, which openapi.yaml does not document", status, mediaType)
			}

			var body any = w.Body.String()
			if mediaType == "application/json" {
				if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
					t.Fatalf("decode body: %v (body: %s)", err, w.Body.String())
				}
			}
			for _, problem := range spec.check(spec.resolve(media)["schema"], body, "body") {
				t.Errorf("%s (body: %s)", problem, w.Body.String())
			}
		})
	}

	var unexercised []string
	for key, op := range ops {
		for status := range spec.resolve(op["responses"]) {
			if !seen[key+" "+status] {
				unexercised = append(unexercised, key+" "+status)
			}
		}
	}
	sort.Strings(unexercised)
	for _, u := range unexercised {
		t.Errorf("openapi.yaml documents %s, but no case in apiCases gets that answer", u)
	}
}

// check returns how value fails schema, or nothing if it fits. It knows the
// keywords openapi.yaml uses for responses: $ref, allOf, type, const, enum,
// pattern, required, properties and items.
func (s openAPI) check(schema any, value any, at string) []string {
	sc := s.resolve(schema)
	if sc == nil {
		return nil
	}
	var problems []string
	fail := func(format string, args ...any) {
		problems = append(problems, at+": "+fmt.Sprintf(format, args...))
	}

	for _, sub := range asList(sc["allOf"]) {
		problems = append(problems, s.check(sub, value, at)...)
	}
	if c, ok := sc["const"]; ok && !reflect.DeepEqual(c, value) {
		fail("is %v, want %v", value, c)
	}
	if enum, ok := sc["enum"].([]any); ok && !slices.ContainsFunc(enum, func(e any) bool { return reflect.DeepEqual(e, value) }) {
		fail("is %v, want one of %v", value, enum)
	}
	if types := asList(sc["type"]); len(types) > 0 && !slices.ContainsFunc(types, func(t any) bool { return jsonTypeIs(value, t) }) {
		fail("is a %s, want %v", jsonType(value), types)
		return problems
	}
	if pattern, ok := sc["pattern"].(string); ok {
		if str, ok := value.(string); ok && !regexp.MustCompile(pattern).MatchString(str) {
			fail("%q does not match %s", str, pattern)
		}
	}

	switch v := value.(type) {
	case map[string]any:
		for _, name := range asList(sc["required"]) {
			if _, ok := v[name.(string)]; !ok {
				fail("lacks %q", name)
			}
		}
		properties, _ := sc["properties"].(map[string]any)
		for name, sub := range properties {
			if pv, ok := v[name]; ok {
				problems = append(problems, s.check(sub, pv, at+"."+name)...)
			}
		}
	case []any:
		for i, item := range v {
			problems = append(problems, s.check(sc["items"], item, fmt.Sprintf("%s[%d]", at, i))...)
		}
	}
	return problems
}

// asList reads a YAML value that may be one item or a list of them.
func asList(v any) []any {
	switch v := v.(type) {
	case nil:
		return nil
	case []any:
		return v
	default:
		return []any{v}
	}
}

// jsonType names v's JSON Schema type, as decoded by encoding/json.
func jsonType(v any) string {
	switch v := v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case float64:
		if v == float64(int64(v)) {
			return "integer"
		}
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return fmt.Sprintf("%T", v)
}

func jsonTypeIs(v any, want any) bool {
	got := jsonType(v)
	return got == want || (got == "integer" && want == "number")
}
