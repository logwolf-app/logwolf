package main

import (
	"context"
	"encoding/json"
	"fmt"
	"logwolf-toolbox/data"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The MCP tests drive the real router with the SDK's own client, over HTTP, so
// they exercise what an agent sees: the protocol, the auth in front of it, and
// what the tools ask the logger.

// bearerTransport adds a key to every request, the way an MCP client
// configured with an Authorization header does.
type bearerTransport struct {
	key string
}

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.key)
	return http.DefaultTransport.RoundTrip(r)
}

// mcpSession serves h over HTTP and connects an MCP client to its /mcp with
// key. The session is closed when t ends.
func mcpSession(t *testing.T, h http.Handler, key string) *mcp.ClientSession {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	client := mcp.NewClient(&mcp.Implementation{Name: "logwolf-test", Version: "1"}, nil)
	transport := &mcp.StreamableClientTransport{
		Endpoint:   srv.URL + "/mcp",
		HTTPClient: &http.Client{Transport: bearerTransport{key: key}},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

// callTool calls a tool and decodes its structured result into T. A tool error
// fails the test; see callToolError for the calls meant to fail.
func callTool[T any](t *testing.T, s *mcp.ClientSession, name string, args map[string]any) T {
	t.Helper()
	res := callToolResult(t, s, name, args)
	if res.IsError {
		t.Fatalf("%s(%v) failed: %s", name, args, toolText(res))
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("%s: encode structured content: %v", name, err)
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%s: decode structured content: %v (%s)", name, err, raw)
	}
	return out
}

// callToolError calls a tool that is meant to fail and returns what it told
// the agent.
func callToolError(t *testing.T, s *mcp.ClientSession, name string, args map[string]any) string {
	t.Helper()
	res := callToolResult(t, s, name, args)
	if !res.IsError {
		t.Fatalf("%s(%v) succeeded, want a tool error", name, args)
	}
	return toolText(res)
}

func callToolResult(t *testing.T, s *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	return res
}

func toolText(res *mcp.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if text, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, text.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// addEvent stores e in the fake under projectID.
func (f *fakeLogger) addEvent(projectID string, e data.LogEntry) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e.ProjectID = mustObjectID(projectID)
	f.logs[projectID] = append(f.logs[projectID], e)
}

func TestMCP_ListsReadOnlyTools(t *testing.T) {
	h, _ := newInternalTestServer(t)
	s := mcpSession(t, h, seedKey(t, projAlpha, data.ScopeRead))

	if got := s.InitializeResult().ServerInfo.Name; got != "logwolf" {
		t.Errorf("server name = %q, want logwolf", got)
	}
	if s.InitializeResult().Instructions == "" {
		t.Error("the server gives the agent no instructions")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := s.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}

	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("%s is not marked read-only", tool.Name)
		}
		if tool.OutputSchema == nil {
			t.Errorf("%s has no output schema", tool.Name)
		}
	}
	slices.Sort(names)
	if want := []string{"count_events", "get_event", "get_project_overview", "search_events"}; !slices.Equal(names, want) {
		t.Errorf("tools = %v, want %v", names, want)
	}
}

// TestMCP_ToolsReadTheKeysProjectAlone: every tool reads the key's project,
// and another project's event is not found, as if it did not exist.
func TestMCP_ToolsReadTheKeysProjectAlone(t *testing.T) {
	h, f := newInternalTestServer(t)
	f.metrics[projAlpha] = data.Metrics{TotalEvents: 7, TotalErrors: 2, TopTags: []data.TagCount{{Tag: "web", Count: 3}}}
	f.retention[projAlpha] = 30
	s := mcpSession(t, h, seedKey(t, projAlpha, data.ScopeRead))

	search := callTool[searchEventsOutput](t, s, "search_events", nil)
	if len(search.Events) != 1 || search.Events[0].ID != alphaLogID {
		t.Errorf("search_events = %+v, want alpha's event alone", search.Events)
	}

	if ev := callTool[mcpEvent](t, s, "get_event", map[string]any{"id": alphaLogID}); ev.Name != "alpha-event" {
		t.Errorf("get_event(alpha's) = %+v, want alpha-event", ev)
	}
	for _, id := range []string{betaLogID, "not-an-id"} {
		if msg := callToolError(t, s, "get_event", map[string]any{"id": id}); msg != errEventNotFound.Error() {
			t.Errorf("get_event(%s) = %q, want %q", id, msg, errEventNotFound)
		}
	}

	overview := callTool[overviewOutput](t, s, "get_project_overview", nil)
	if overview.TotalEvents != 7 || overview.TotalErrors != 2 || overview.RetentionDays != 30 || len(overview.TopTags) != 1 {
		t.Errorf("get_project_overview = %+v, want alpha's metrics and retention", overview)
	}
	if time.Since(overview.ServerTime).Abs() > time.Minute {
		t.Errorf("server_time = %v, want now", overview.ServerTime)
	}

	callTool[countEventsOutput](t, s, "count_events", nil)

	f.snapshot(func(f *fakeLogger) {
		for _, a := range f.searchArgs {
			if a.ProjectID != projAlpha {
				t.Errorf("SearchLogs asked for project %s, want the key's", a.ProjectID)
			}
		}
		for _, a := range f.countArgs {
			if a.ProjectID != projAlpha {
				t.Errorf("CountLogs asked for project %s, want the key's", a.ProjectID)
			}
		}
		for _, a := range f.metricsArgs {
			if a.ProjectID != projAlpha {
				t.Errorf("GetMetrics asked for project %s, want the key's", a.ProjectID)
			}
		}
	})
}

// TestMCP_SearchForwardsTheFilters: the filters reach the logger as the query
// they say, since and until read against now, and the page as asked.
func TestMCP_SearchForwardsTheFilters(t *testing.T) {
	h, f := newInternalTestServer(t)
	s := mcpSession(t, h, seedKey(t, projAlpha, data.ScopeRead))

	before := time.Now()
	callTool[searchEventsOutput](t, s, "search_events", map[string]any{
		"severity":  []string{"error", "critical"},
		"name":      "checkout",
		"tags":      []string{"web"},
		"text":      "declined",
		"since":     "24h",
		"until":     "2030-01-02T03:04:05Z",
		"page":      2,
		"page_size": 50,
	})

	f.snapshot(func(f *fakeLogger) {
		if len(f.searchArgs) != 1 {
			t.Fatalf("SearchLogs calls = %d, want 1", len(f.searchArgs))
		}
		a := f.searchArgs[0]
		q := a.Query
		if !slices.Equal(q.Severities, []string{"error", "critical"}) || q.Name != "checkout" || !slices.Equal(q.Tags, []string{"web"}) || q.Text != "declined" {
			t.Errorf("query = %+v, want the filters given", q)
		}
		if since := before.Add(-24 * time.Hour); q.Since.Before(since.Add(-time.Minute)) || q.Since.After(since.Add(time.Minute)) {
			t.Errorf("since = %v, want 24h before %v", q.Since, before)
		}
		if want := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC); !q.Until.Equal(want) {
			t.Errorf("until = %v, want %v", q.Until, want)
		}
		if a.Pagination != (data.PaginationParams{Page: 2, PageSize: 50}) {
			t.Errorf("pagination = %+v, want page 2 of 50", a.Pagination)
		}
	})
}

func TestMCP_SearchPagesAndCutsData(t *testing.T) {
	h, f := newInternalTestServer(t)
	long := strings.Repeat("é", mcpSearchDataBytes) // two bytes each
	for i := range 3 {
		f.addEvent(projAlpha, data.LogEntry{ID: "e" + string(rune('0'+i)), Name: "payment", Severity: "error", Data: long, Tags: []string{"web"}})
	}
	s := mcpSession(t, h, seedKey(t, projAlpha, data.ScopeRead))

	page := callTool[searchEventsOutput](t, s, "search_events", map[string]any{"severity": []string{"error"}, "page_size": 2})
	if len(page.Events) != 2 || !page.HasMore || page.Page != 1 || page.PageSize != 2 {
		t.Fatalf("first page = %d events, has_more %v, page %d of %d; want 2, true, 1 of 2", len(page.Events), page.HasMore, page.Page, page.PageSize)
	}
	ev := page.Events[0]
	if !ev.DataTruncated || len(ev.Data) > mcpSearchDataBytes || !utf8.ValidString(ev.Data) {
		t.Errorf("search data = %d bytes, truncated %v, valid UTF-8 %v; want cut to at most %d on a character boundary",
			len(ev.Data), ev.DataTruncated, utf8.ValidString(ev.Data), mcpSearchDataBytes)
	}

	last := callTool[searchEventsOutput](t, s, "search_events", map[string]any{"severity": []string{"error"}, "page_size": 2, "page": 2})
	if len(last.Events) != 1 || last.HasMore {
		t.Errorf("last page = %d events, has_more %v; want 1, false", len(last.Events), last.HasMore)
	}

	full := callTool[mcpEvent](t, s, "get_event", map[string]any{"id": ev.ID})
	if full.Data != long || full.DataTruncated {
		t.Errorf("get_event data = %d bytes, truncated %v; want all %d", len(full.Data), full.DataTruncated, len(long))
	}
}

func TestMCP_CountEvents(t *testing.T) {
	h, f := newInternalTestServer(t)
	f.addEvent(projAlpha, data.LogEntry{ID: "e1", Name: "payment", Severity: "error"})
	f.addEvent(projAlpha, data.LogEntry{ID: "e2", Name: "payment", Severity: "error"})
	s := mcpSession(t, h, seedKey(t, projAlpha, data.ScopeRead))

	got := callTool[countEventsOutput](t, s, "count_events", map[string]any{"group_by": "severity", "since": "7d"})
	want := []data.LogCount{{Key: "error", Count: 2}, {Key: "info", Count: 1}}
	if got.Total != 3 || got.GroupBy != "severity" || !slices.Equal(got.Groups, want) {
		t.Errorf("count_events = %+v, want 3 in total, %v", got, want)
	}

	callTool[countEventsOutput](t, s, "count_events", map[string]any{"group_by": "hour", "limit": data.MaxCountGroups})
	f.snapshot(func(f *fakeLogger) {
		if len(f.countArgs) != 2 {
			t.Fatalf("CountLogs calls = %d, want 2", len(f.countArgs))
		}
		if a := f.countArgs[0]; a.Limit != data.DefaultCountGroups || a.Query.Since.IsZero() {
			t.Errorf("first count = limit %d, since %v; want the default limit and a week back", a.Limit, a.Query.Since)
		}
		if a := f.countArgs[1]; a.GroupBy != data.GroupByHour || a.Limit != data.MaxCountGroups {
			t.Errorf("second count = by %q, limit %d; want hour, %d", a.GroupBy, a.Limit, data.MaxCountGroups)
		}
	})
}

// TestMCP_RefusesBadInput: input that cannot be run is a tool error the agent
// can read and correct, and the logger is never asked.
func TestMCP_RefusesBadInput(t *testing.T) {
	h, f := newInternalTestServer(t)
	s := mcpSession(t, h, seedKey(t, projAlpha, data.ScopeRead))

	for _, tc := range []struct {
		tool string
		args map[string]any
		want string // in the error
	}{
		{"search_events", map[string]any{"severity": []string{"fatal"}}, "severity"},
		{"search_events", map[string]any{"page_size": data.MaxPageSize + 1}, "page_size"},
		{"search_events", map[string]any{"page": 0}, "page"},
		{"search_events", map[string]any{"since": "yesterday"}, "since"},
		{"search_events", map[string]any{"since": "1h", "until": "2h"}, "since must be before until"},
		{"search_events", map[string]any{"text": strings.Repeat("x", data.MaxQueryTextLength+1)}, "text"},
		{"count_events", map[string]any{"group_by": "data"}, "group_by"},
		{"count_events", map[string]any{"limit": data.MaxCountGroups + 1}, "limit"},
		{"count_events", map[string]any{"until": "-3h"}, "until"},
		{"get_event", map[string]any{}, "id"},
	} {
		if msg := callToolError(t, s, tc.tool, tc.args); !strings.Contains(msg, tc.want) {
			t.Errorf("%s(%v) = %q, want it to mention %q", tc.tool, tc.args, msg, tc.want)
		}
	}

	f.snapshot(func(f *fakeLogger) {
		if len(f.searchArgs)+len(f.countArgs) != 0 {
			t.Errorf("the logger was asked %d searches and %d counts, want none", len(f.searchArgs), len(f.countArgs))
		}
	})
}

// TestMCP_LoggerFailureIsATransientToolError: the agent learns to retry, not
// the database's internals.
func TestMCP_LoggerFailureIsATransientToolError(t *testing.T) {
	h, f := newInternalTestServer(t)
	f.failReads = true
	s := mcpSession(t, h, seedKey(t, projAlpha, data.ScopeRead))

	for _, tool := range []string{"search_events", "count_events"} {
		if msg := callToolError(t, s, tool, nil); msg != errEventsUnavailable.Error() {
			t.Errorf("%s with the database down = %q, want %q", tool, msg, errEventsUnavailable)
		}
	}
}

func TestMCP_ClosesItsRPCClients(t *testing.T) {
	h, f := newInternalTestServer(t)
	s := mcpSession(t, h, seedKey(t, projAlpha, data.ScopeRead))

	callTool[searchEventsOutput](t, s, "search_events", nil)
	callTool[overviewOutput](t, s, "get_project_overview", nil)
	callToolError(t, s, "get_event", map[string]any{"id": betaLogID})

	deadline := time.Now().Add(2 * time.Second)
	for f.openConns.Load() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%d logger connections left open", f.openConns.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// mcpRequest is a JSON-RPC message posted to /mcp, with the headers the
// Streamable HTTP transport demands.
func mcpRequest(key, body string) *http.Request {
	r := keyRequest(http.MethodPost, "/mcp", key, body)
	r.Header.Set("Accept", "application/json, text/event-stream")
	return r
}

const mcpInitialize = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"curl","version":"1"}}}`

func TestMCP_NeedsAKeyWithTheReadScope(t *testing.T) {
	h, _ := newInternalTestServer(t)

	r := mcpRequest("", mcpInitialize)
	r.Header.Del("Authorization")
	if w := do(h, r); w.Code != http.StatusUnauthorized {
		t.Errorf("POST /mcp without a key = %d, want 401", w.Code)
	}

	for _, scopes := range [][]string{{data.ScopeIngest}, {data.ScopeIngest, data.ScopeDelete}} {
		if w := do(h, mcpRequest(seedKey(t, projAlpha, scopes...), mcpInitialize)); w.Code != http.StatusForbidden {
			t.Errorf("POST /mcp with a %v key = %d, want 403", scopes, w.Code)
		}
	}

	w := do(h, mcpRequest(seedKey(t, projAlpha, data.ScopeRead), mcpInitialize))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"serverInfo"`) {
		t.Errorf("POST /mcp initialize with a read key = %d %s, want 200 and the server's info", w.Code, w.Body.String())
	}
}

// TestMCP_IsStateless: no session is handed out, and GET and DELETE, which
// only sessions use, are refused.
func TestMCP_IsStateless(t *testing.T) {
	h, _ := newInternalTestServer(t)
	key := seedKey(t, projAlpha, data.ScopeRead)

	w := do(h, mcpRequest(key, mcpInitialize))
	if id := w.Header().Get("Mcp-Session-Id"); id != "" {
		t.Errorf("initialize handed out session %q, want none", id)
	}

	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		r := keyRequest(method, "/mcp", key, "")
		r.Header.Set("Accept", "text/event-stream")
		if w := do(h, r); w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /mcp = %d, want 405", method, w.Code)
		}
	}

	// A tool call needs no initialize before it on the same session.
	call := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"get_event","arguments":{"id":"` + alphaLogID + `"}}}`
	if w := do(h, mcpRequest(key, call)); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "alpha-event") {
		t.Errorf("tools/call without a session = %d %s, want 200 and the event", w.Code, w.Body.String())
	}
}

func TestParseWhen(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		in   string
		want time.Time
	}{
		{"", time.Time{}},
		{"2026-10-01T08:30:00Z", time.Date(2026, 10, 1, 8, 30, 0, 0, time.UTC)},
		{"2026-10-01T08:30:00.5+02:00", time.Date(2026, 10, 1, 6, 30, 0, 5e8, time.UTC)},
		{"2026-10-01", time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)},
		{"30m", now.Add(-30 * time.Minute)},
		{" 24h ", now.Add(-24 * time.Hour)},
		{"7d", now.Add(-7 * 24 * time.Hour)},
		{"1.5d", now.Add(-36 * time.Hour)},
	} {
		got, err := parseWhen(tc.in, now)
		if err != nil || !got.Equal(tc.want) {
			t.Errorf("parseWhen(%q) = %v, %v; want %v", tc.in, got, err, tc.want)
		}
	}

	for _, in := range []string{"yesterday", "-1h", "0s", "0d", "d", "-2d", "99999999d", "2026-13-01"} {
		if got, err := parseWhen(in, now); err == nil {
			t.Errorf("parseWhen(%q) = %v, want an error", in, got)
		}
	}
}

func TestToMCPEvent(t *testing.T) {
	created := time.Date(2026, 10, 6, 12, 0, 0, 0, time.FixedZone("CEST", 2*3600))
	e := data.LogEntry{ID: "e1", Name: "n", Severity: "info", Data: "aé", Duration: 12, CreatedAt: created}

	got := toMCPEvent(e, 0)
	if got.Data != "aé" || got.DataTruncated || got.Tags == nil || got.DurationMs != 12 || got.CreatedAt.Location() != time.UTC || !got.CreatedAt.Equal(created) {
		t.Errorf("toMCPEvent whole = %+v", got)
	}

	// "é" is two bytes: cutting at 2 would split it, so the cut falls before it.
	if got := toMCPEvent(e, 2); got.Data != "a" || !got.DataTruncated {
		t.Errorf("toMCPEvent cut at 2 = %q, truncated %v; want \"a\", true", got.Data, got.DataTruncated)
	}
	if got := toMCPEvent(e, 3); got.Data != "aé" || got.DataTruncated {
		t.Errorf("toMCPEvent cut at 3 = %q, truncated %v; want all of it", got.Data, got.DataTruncated)
	}
}

// TestMCP_SchemasStateTheDefaults: the schemas leave defaults out (see
// searchEventsSchema), so the descriptions say them, in words struct tags
// cannot take from the constants.
func TestMCP_SchemasStateTheDefaults(t *testing.T) {
	for _, tc := range []struct {
		schema   map[string]*jsonschema.Schema
		property string
		dflt     int
	}{
		{searchEventsSchema.Properties, "page_size", data.DefaultPageSize},
		{countEventsSchema.Properties, "limit", data.DefaultCountGroups},
	} {
		p := tc.schema[tc.property]
		if want := fmt.Sprintf("%d when left out", tc.dflt); !strings.Contains(p.Description, want) {
			t.Errorf("%s: description %q does not say %q", tc.property, p.Description, want)
		}
		if p.Default != nil {
			t.Errorf("%s: schema default %s; the SDK panics applying it to a call without arguments", tc.property, p.Default)
		}
	}
}
