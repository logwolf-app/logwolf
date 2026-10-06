//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"

	"logwolf-toolbox/data"
)

// seedSearchLogs inserts events straight into MongoDB, at the times given, so
// the windows and periods the tests ask about are fixed. Each test uses a
// project of its own, so the shared database needs no clearing.
func seedSearchLogs(t *testing.T, m *data.Models, entries []data.LogEntry) {
	t.Helper()
	logs := testMongo(t, sharedModelsMongo(t)).Database("logs").Collection("logs")
	docs := make([]any, len(entries))
	for i, e := range entries {
		if e.Tags == nil {
			e.Tags = []string{}
		}
		docs[i] = e
	}
	if _, err := logs.InsertMany(context.Background(), docs); err != nil {
		t.Fatalf("seed logs: %v", err)
	}
	if err := m.EnsureLogsIndexes(); err != nil {
		t.Fatalf("indexes: %v", err)
	}
}

func TestSearchAndCountLogs(t *testing.T) {
	m := data.New(testMongo(t, sharedModelsMongo(t)))
	project, other := primitive.NewObjectID(), primitive.NewObjectID()
	day := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	at := func(h, min int) time.Time { return day.Add(time.Duration(h)*time.Hour + time.Duration(min)*time.Minute) }

	seedSearchLogs(t, &m, []data.LogEntry{
		{ProjectID: project, Name: "checkout", Severity: "error", Data: `{"reason":"Card declined"}`, Tags: []string{"web", "eu"}, CreatedAt: at(9, 10)},
		{ProjectID: project, Name: "checkout", Severity: "error", Data: `{"reason":"timeout"}`, Tags: []string{"web"}, CreatedAt: at(9, 40)},
		{ProjectID: project, Name: "checkout", Severity: "info", Data: `{}`, Tags: []string{"web"}, CreatedAt: at(10, 5)},
		{ProjectID: project, Name: "signup", Severity: "critical", Data: `{"why":"db (down)"}`, Tags: []string{"api"}, CreatedAt: at(10, 30)},
		// Stored before severities were normalized.
		{ProjectID: project, Name: "legacy", Severity: "ERROR", Data: `{}`, CreatedAt: day.Add(-2 * time.Hour)},
		// Another project's, which nothing here may see.
		{ProjectID: other, Name: "checkout", Severity: "error", Data: `Card declined`, Tags: []string{"web"}, CreatedAt: at(9, 15)},
	})

	search := func(q data.LogQuery, page, size int64) *data.SearchLogsReply {
		t.Helper()
		r, err := m.SearchLogs(project, q, data.PaginationParams{Page: page, PageSize: size})
		if err != nil {
			t.Fatalf("SearchLogs(%+v): %v", q, err)
		}
		return r
	}
	names := func(r *data.SearchLogsReply) []string {
		var out []string
		for _, l := range r.Logs {
			out = append(out, l.Name+"@"+l.CreatedAt.UTC().Format("15:04"))
		}
		return out
	}

	t.Run("search", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			q    data.LogQuery
			want []string
		}{
			{"everything, newest first", data.LogQuery{}, []string{"signup@10:30", "checkout@10:05", "checkout@09:40", "checkout@09:10", "legacy@22:00"}},
			{"severities", data.LogQuery{Severities: []string{"Error", "critical"}}, []string{"signup@10:30", "checkout@09:40", "checkout@09:10"}},
			{"name", data.LogQuery{Name: "signup"}, []string{"signup@10:30"}},
			{"every tag", data.LogQuery{Tags: []string{"web", "eu"}}, []string{"checkout@09:10"}},
			{"text in data, any case", data.LogQuery{Text: "card DECLINED"}, []string{"checkout@09:10"}},
			{"text is literal", data.LogQuery{Text: "(down)"}, []string{"signup@10:30"}},
			{"text in name", data.LogQuery{Text: "sign"}, []string{"signup@10:30"}},
			{"window", data.LogQuery{Since: at(9, 40), Until: at(10, 30)}, []string{"checkout@10:05", "checkout@09:40"}},
		} {
			if got := names(search(tc.q, 1, 100)); !slices.Equal(got, tc.want) {
				t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
			}
		}
	})

	t.Run("pages", func(t *testing.T) {
		first, last := search(data.LogQuery{}, 1, 3), search(data.LogQuery{}, 2, 3)
		if len(first.Logs) != 3 || !first.HasMore || len(last.Logs) != 2 || last.HasMore {
			t.Errorf("pages of 3 = %d (more %v), %d (more %v); want 3 (true), 2 (false)", len(first.Logs), first.HasMore, len(last.Logs), last.HasMore)
		}
		if exact := search(data.LogQuery{}, 1, 5); exact.HasMore {
			t.Error("a page holding the last event says there are more")
		}
	})

	count := func(groupBy string, limit int, q data.LogQuery) *data.LogCounts {
		t.Helper()
		c, err := m.CountLogs(project, data.RPCCountLogsArgs{Query: q, GroupBy: groupBy, Limit: limit})
		if err != nil {
			t.Fatalf("CountLogs(%s): %v", groupBy, err)
		}
		return c
	}

	t.Run("count", func(t *testing.T) {
		for _, tc := range []struct {
			name      string
			groupBy   string
			limit     int
			q         data.LogQuery
			total     int64
			want      []data.LogCount
			truncated bool
		}{
			{"total alone", "", 5, data.LogQuery{}, 5, []data.LogCount{}, false},
			// The legacy "ERROR" counts with the rest.
			{"severity", data.GroupBySeverity, 5, data.LogQuery{}, 5,
				[]data.LogCount{{Key: "error", Count: 3}, {Key: "critical", Count: 1}, {Key: "info", Count: 1}}, false},
			{"name, cut off", data.GroupByName, 1, data.LogQuery{}, 5, []data.LogCount{{Key: "checkout", Count: 3}}, true},
			{"tag", data.GroupByTag, 5, data.LogQuery{}, 5,
				[]data.LogCount{{Key: "web", Count: 3}, {Key: "api", Count: 1}, {Key: "eu", Count: 1}}, false},
			{"hour, in time order", data.GroupByHour, 5, data.LogQuery{Since: day}, 4,
				[]data.LogCount{{Key: "2026-10-05T09:00:00Z", Count: 2}, {Key: "2026-10-05T10:00:00Z", Count: 2}}, false},
			{"day, latest kept", data.GroupByDay, 1, data.LogQuery{}, 5, []data.LogCount{{Key: "2026-10-05", Count: 4}}, true},
			{"filtered", data.GroupByName, 5, data.LogQuery{Severities: []string{"error"}}, 2, []data.LogCount{{Key: "checkout", Count: 2}}, false},
		} {
			got := count(tc.groupBy, tc.limit, tc.q)
			if got.Total != tc.total || !slices.Equal(got.Groups, tc.want) || got.Truncated != tc.truncated {
				t.Errorf("%s: total %d, %v, truncated %v; want %d, %v, %v", tc.name, got.Total, got.Groups, got.Truncated, tc.total, tc.want, tc.truncated)
			}
		}
	})

	t.Run("nothing matches", func(t *testing.T) {
		if r := search(data.LogQuery{Name: "nope"}, 1, 10); len(r.Logs) != 0 || r.Logs == nil || r.HasMore {
			t.Errorf("search for nothing = %+v, want an empty page", r)
		}
		if c := count(data.GroupByTag, 5, data.LogQuery{Name: "nope"}); c.Total != 0 || len(c.Groups) != 0 {
			t.Errorf("count of nothing = %+v, want 0", c)
		}
	})
}

// TestMCP_EndToEnd asks the real broker's MCP server about events sent through
// the SDK route, and gets the key's project's alone.
func TestMCP_EndToEnd(t *testing.T) {
	stack := sharedStack(t)

	const ownStem, otherStem = "lw_mcpown", "lw_mcpother"
	ownKey := seedAPIKey(t, stack.mongoURI, seedProject(t, stack.mongoURI, "mcp"), ownStem+strings.Repeat("0", 46-len(ownStem)-1)+"1")
	otherKey := seedAPIKey(t, stack.mongoURI, seedProject(t, stack.mongoURI, "mcp-other"), otherStem+strings.Repeat("0", 46-len(otherStem)-1)+"1")

	postLog(t, stack.brokerURL, ownKey, "mcp-own-event")
	postLog(t, stack.brokerURL, otherKey, "mcp-other-event")
	waitForLog(t, stack.mongoURI, "mcp-own-event")
	waitForLog(t, stack.mongoURI, "mcp-other-event")

	status, body := mcpCall(t, stack.brokerURL, ownKey, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"integration","version":"1"}}}`)
	if status != http.StatusOK || !strings.Contains(string(body), `"name":"logwolf"`) {
		t.Fatalf("initialize = %d %s", status, body)
	}

	status, body = mcpCall(t, stack.brokerURL, ownKey, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search_events","arguments":{"text":"mcp-","since":"1h"}}}`)
	var reply struct {
		Result struct {
			IsError           bool `json:"isError"`
			StructuredContent struct {
				Events []struct {
					Name string `json:"name"`
				} `json:"events"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &reply); status != http.StatusOK || err != nil || reply.Result.IsError {
		t.Fatalf("search_events = %d %s (%v)", status, body, err)
	}
	var got []string
	for _, e := range reply.Result.StructuredContent.Events {
		got = append(got, e.Name)
	}
	if !slices.Equal(got, []string{"mcp-own-event"}) {
		t.Errorf("search_events found %v, want the key's project's event alone", got)
	}

	status, body = mcpCall(t, stack.brokerURL, ownKey, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"count_events","arguments":{"group_by":"severity","since":"1h","text":"mcp-"}}}`)
	if status != http.StatusOK || !strings.Contains(string(body), `"total":1`) || strings.Contains(string(body), `"isError":true`) {
		t.Errorf("count_events = %d %s, want one event", status, body)
	}
}

func mcpCall(t *testing.T, brokerURL, apiKey, message string) (int, []byte) {
	t.Helper()

	req, _ := http.NewRequest(http.MethodPost, brokerURL+"/mcp", bytes.NewReader([]byte(message)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /mcp: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}
