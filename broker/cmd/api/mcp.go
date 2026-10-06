package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"logwolf-toolbox/data"
	"net/http"
	"net/rpc"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The broker serves the Model Context Protocol at POST /mcp, so AI agents can
// read and analyze a project's events. It takes an API key with the read
// scope, like GET /logs, and every tool reads the key's project alone: the
// project is bound when the request comes in, and no tool takes one.
//
// The server is stateless and answers in JSON (no event streams): each request
// stands alone, so it needs nothing kept between requests and works the same
// behind any number of broker replicas. GET and DELETE /mcp, which only
// sessions use, are 405s.

// mcpMaxRequestBytes caps a request to /mcp. A tool call is a few hundred
// bytes; the SDK's default is 4 MiB.
const mcpMaxRequestBytes = 64 << 10

// mcpInstructions tells the agent what it is looking at and how to go about it.
const mcpInstructions = `Logwolf stores the events applications send it. Each event has a name, a severity (info, warning, error or critical), tags, data (a string, often JSON), an optional duration in milliseconds, and the UTC time it was created.

This server reads the events of one project: the one the API key belongs to. It cannot change or delete anything.

To investigate, start with get_project_overview. Use count_events to see where events cluster, grouped by severity, name, tag, hour or day, and to compare time windows. Then use search_events to read the events themselves, and get_event for one event's full data, which search results cut short.`

// mcpHandler serves /mcp. It runs behind requireAPIKey and requireScope(read),
// which put the key's project in the request context.
func (app *Config) mcpHandler() http.Handler {
	schemas := mcp.NewSchemaCache()
	return mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		projectID := projectIDFromContext(r)
		if projectID == "" {
			return nil // the SDK answers 400; requireAPIKey makes it unreachable
		}
		return newMCPServer(projectID, schemas)
	}, &mcp.StreamableHTTPOptions{
		Stateless:           true,
		JSONResponse:        true,
		MaxRequestBodyBytes: mcpMaxRequestBytes,
	})
}

// newMCPServer is the MCP server for one request, bound to its project. The
// schema cache spares inferring the tools' schemas on every request.
func newMCPServer(projectID string, schemas *mcp.SchemaCache) *mcp.Server {
	s := mcp.NewServer(
		&mcp.Implementation{Name: "logwolf", Title: "Logwolf", Version: "1.0.0"},
		&mcp.ServerOptions{Instructions: mcpInstructions, SchemaCache: schemas},
	)
	t := mcpTools{projectID: projectID}

	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: new(bool)}

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_project_overview",
		Title:       "Project overview",
		Description: "Totals for the project: events and errors overall and in the last 24 hours, the average duration, the most used tags, how many days events are kept, and the current server time.",
		Annotations: readOnly,
	}, t.overview)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "search_events",
		Title:       "Search events",
		Description: fmt.Sprintf("Lists the project's events that match every filter given, newest first, %d to a page by default and at most %d. Each event's data is cut to %d bytes; get_event returns it whole.", data.DefaultPageSize, data.MaxPageSize, mcpSearchDataBytes),
		InputSchema: searchEventsSchema,
		Annotations: readOnly,
	}, t.search)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_event",
		Title:       "Get event",
		Description: "Returns one event of the project by its id, with its full data.",
		Annotations: readOnly,
	}, t.event)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "count_events",
		Title:       "Count events",
		Description: fmt.Sprintf("Counts the project's events that match every filter given, in total and, with group_by, per severity, name, tag, UTC hour or UTC day. Groups come largest first, or, per hour or day, in time order, keeping the latest periods. Periods with no events are left out. Returns %d groups by default and at most %d.", data.DefaultCountGroups, data.MaxCountGroups),
		InputSchema: countEventsSchema,
		Annotations: readOnly,
	}, t.count)

	return s
}

// --- Inputs and outputs ---

// eventFilters are the filters search_events and count_events share. Every
// one given must hold.
type eventFilters struct {
	Severity []string `json:"severity,omitempty" jsonschema:"keep events of any of these severities"`
	Name     string   `json:"name,omitempty" jsonschema:"keep events with exactly this name"`
	Tags     []string `json:"tags,omitempty" jsonschema:"keep events that carry every one of these tags"`
	Text     string   `json:"text,omitempty" jsonschema:"keep events whose name or data contains this text, ignoring case"`
	Since    string   `json:"since,omitempty" jsonschema:"keep events created at or after this time: an RFC 3339 timestamp, a date (UTC midnight), or a duration back from now such as 30m, 24h or 7d"`
	Until    string   `json:"until,omitempty" jsonschema:"keep events created before this time, in the same forms as since"`
}

type searchEventsInput struct {
	eventFilters
	Page     int `json:"page,omitempty" jsonschema:"the page to return, from 1; the first when left out"`
	PageSize int `json:"page_size,omitempty" jsonschema:"how many events to a page; 20 when left out"`
}

type getEventInput struct {
	ID string `json:"id" jsonschema:"the event's id"`
}

type countEventsInput struct {
	eventFilters
	GroupBy string `json:"group_by,omitempty" jsonschema:"what to count events per; leave out for the total alone"`
	Limit   int    `json:"limit,omitempty" jsonschema:"how many groups to return; 20 when left out"`
}

// searchEventsSchema and countEventsSchema are the inferred schemas with what
// struct tags cannot say: the values severity and group_by take, and the
// bounds of the numbers.
//
// They state no defaults: the SDK applies a schema's defaults to the
// arguments, and panics on a call that has none, which an agent may send. The
// tools apply them instead, and the descriptions say what they are.
var (
	searchEventsSchema = mustInputSchema[searchEventsInput](func(s *jsonschema.Schema) {
		bound(s.Properties["page"], 1, data.MaxPage)
		bound(s.Properties["page_size"], 1, data.MaxPageSize)
	})
	countEventsSchema = mustInputSchema[countEventsInput](func(s *jsonschema.Schema) {
		s.Properties["group_by"].Enum = anyList(data.GroupByFields)
		bound(s.Properties["limit"], 1, data.MaxCountGroups)
	})
)

func mustInputSchema[T any](adjust func(*jsonschema.Schema)) *jsonschema.Schema {
	s, err := jsonschema.For[T](nil)
	if err != nil {
		panic(fmt.Sprintf("schema of %T: %v", *new(T), err))
	}
	s.Properties["severity"].Items.Enum = anyList([]string{data.SeverityInfo, data.SeverityWarning, data.SeverityError, data.SeverityCritical})
	adjust(s)
	return s
}

func bound(s *jsonschema.Schema, minimum, maximum int) {
	lo, hi := float64(minimum), float64(maximum)
	s.Minimum, s.Maximum = &lo, &hi
}

func anyList(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

// mcpEvent is an event as the tools return it.
type mcpEvent struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Severity string   `json:"severity"`
	Tags     []string `json:"tags"`
	Data     string   `json:"data"`
	// DataTruncated is whether Data was cut short; get_event returns it whole.
	DataTruncated bool      `json:"data_truncated,omitempty"`
	DurationMs    int       `json:"duration_ms,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

// mcpSearchDataBytes is how much of each event's data search_events returns. A
// page of whole payloads could fill an agent's context; it asks get_event for
// the ones it needs.
const mcpSearchDataBytes = 1000

func toMCPEvent(e data.LogEntry, maxData int) mcpEvent {
	ev := mcpEvent{
		ID:         e.ID,
		Name:       e.Name,
		Severity:   e.Severity,
		Tags:       e.Tags,
		Data:       e.Data,
		DurationMs: e.Duration,
		CreatedAt:  e.CreatedAt.UTC(),
	}
	if ev.Tags == nil {
		ev.Tags = []string{}
	}
	if maxData > 0 && len(ev.Data) > maxData {
		cut := maxData
		for cut > 0 && !utf8.RuneStart(ev.Data[cut]) {
			cut--
		}
		ev.Data, ev.DataTruncated = ev.Data[:cut], true
	}
	return ev
}

type overviewOutput struct {
	TotalEvents   int             `json:"total_events"`
	TotalErrors   int             `json:"total_errors" jsonschema:"events of severity error or critical"`
	TotalCritical int             `json:"total_critical"`
	EventsLast24h int             `json:"events_last_24h"`
	ErrorsLast24h int             `json:"errors_last_24h"`
	AvgDurationMs float64         `json:"avg_duration_ms" jsonschema:"the average duration of the events that have one"`
	TopTags       []data.TagCount `json:"top_tags"`
	RetentionDays int             `json:"retention_days" jsonschema:"how many days events are kept; 0 is forever"`
	ServerTime    time.Time       `json:"server_time"`
}

type searchEventsOutput struct {
	Events   []mcpEvent `json:"events"`
	Page     int64      `json:"page"`
	PageSize int64      `json:"page_size"`
	HasMore  bool       `json:"has_more" jsonschema:"whether the next page has events"`
}

type countEventsOutput struct {
	Total     int64           `json:"total"`
	GroupBy   string          `json:"group_by,omitempty"`
	Groups    []data.LogCount `json:"groups"`
	Truncated bool            `json:"truncated" jsonschema:"whether there were more groups than the limit"`
}

// --- Tools ---

// mcpTools are the tools of one request's server, bound to its key's project.
type mcpTools struct {
	projectID string
}

func (t mcpTools) overview(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, overviewOutput, error) {
	var metrics data.Metrics
	if err := t.call("get_project_overview", "RPCServer.GetMetrics", &data.ProjectArgs{ProjectID: t.projectID}, &metrics); err != nil {
		return nil, overviewOutput{}, err
	}
	var days int
	if err := t.call("get_project_overview", "RPCServer.GetRetention", &data.RetentionArgs{ProjectID: t.projectID}, &days); err != nil {
		return nil, overviewOutput{}, err
	}

	out := overviewOutput{
		TotalEvents:   metrics.TotalEvents,
		TotalErrors:   metrics.TotalErrors,
		TotalCritical: metrics.TotalCritical,
		EventsLast24h: metrics.EventsLast24h,
		ErrorsLast24h: metrics.ErrorsLast24h,
		AvgDurationMs: metrics.AvgDurationMs,
		TopTags:       metrics.TopTags,
		RetentionDays: days,
		ServerTime:    time.Now().UTC(),
	}
	if out.TopTags == nil {
		out.TopTags = []data.TagCount{}
	}
	return nil, out, nil
}

func (t mcpTools) search(_ context.Context, _ *mcp.CallToolRequest, in searchEventsInput) (*mcp.CallToolResult, searchEventsOutput, error) {
	q, err := in.query(time.Now())
	if err != nil {
		return nil, searchEventsOutput{}, err
	}
	p := data.PaginationParams{Page: int64(orDefault(in.Page, firstPage)), PageSize: int64(orDefault(in.PageSize, data.DefaultPageSize))}
	if err := p.Validate(); err != nil {
		return nil, searchEventsOutput{}, err
	}

	var reply data.SearchLogsReply
	if err := t.call("search_events", "RPCServer.SearchLogs", &data.RPCSearchLogsArgs{ProjectID: t.projectID, Query: q, Pagination: p}, &reply); err != nil {
		return nil, searchEventsOutput{}, err
	}

	out := searchEventsOutput{Events: make([]mcpEvent, 0, len(reply.Logs)), Page: p.Page, PageSize: p.PageSize, HasMore: reply.HasMore}
	for _, e := range reply.Logs {
		out.Events = append(out.Events, toMCPEvent(e, mcpSearchDataBytes))
	}
	return nil, out, nil
}

func (t mcpTools) event(_ context.Context, _ *mcp.CallToolRequest, in getEventInput) (*mcp.CallToolResult, mcpEvent, error) {
	var entry data.LogEntry
	filter := data.RPCLogEntryFilter{ID: strings.TrimSpace(in.ID), ProjectID: t.projectID}
	if err := t.call("get_event", "RPCServer.GetLog", filter, &entry); err != nil {
		return nil, mcpEvent{}, err
	}
	return nil, toMCPEvent(entry, 0), nil
}

func (t mcpTools) count(_ context.Context, _ *mcp.CallToolRequest, in countEventsInput) (*mcp.CallToolResult, countEventsOutput, error) {
	q, err := in.query(time.Now())
	if err != nil {
		return nil, countEventsOutput{}, err
	}
	args := data.RPCCountLogsArgs{ProjectID: t.projectID, Query: q, GroupBy: in.GroupBy, Limit: orDefault(in.Limit, data.DefaultCountGroups)}
	if err := args.ValidateCount(); err != nil {
		return nil, countEventsOutput{}, err
	}

	var counts data.LogCounts
	if err := t.call("count_events", "RPCServer.CountLogs", &args, &counts); err != nil {
		return nil, countEventsOutput{}, err
	}

	out := countEventsOutput{Total: counts.Total, GroupBy: in.GroupBy, Groups: counts.Groups, Truncated: counts.Truncated}
	if out.Groups == nil {
		out.Groups = []data.LogCount{}
	}
	return nil, out, nil
}

// errEventNotFound is what get_event answers for an id that names no event of
// the project, including another project's.
var errEventNotFound = errors.New("event not found")

// errEventsUnavailable is what a tool answers when the logger fails. The cause
// is logged; it names internals the agent can do nothing about.
var errEventsUnavailable = errors.New("events cannot be read right now; try again shortly")

// call makes one logger RPC for a tool. A failure is an error the SDK returns
// to the agent as the tool's result, so it reads as something to act on: a
// not-found is said so, and anything else is logged and said to be passing.
func (t mcpTools) call(tool, method string, args, reply any) error {
	client, err := rpc.Dial("tcp", loggerRPCAddr())
	if err == nil {
		defer client.Close()
		err = client.Call(method, args, reply)
	}
	if err == nil {
		log.Printf(`{"event":"mcp_tool","outcome":"ok","tool":%q,"project_id":%q}`, tool, t.projectID)
		return nil
	}

	if classifyRPCError(err) == rpcErrNotFound && method == "RPCServer.GetLog" {
		log.Printf(`{"event":"mcp_tool","outcome":"not_found","tool":%q,"project_id":%q}`, tool, t.projectID)
		return errEventNotFound
	}
	log.Printf(`{"event":"mcp_tool","outcome":"error","tool":%q,"project_id":%q,"error":%q}`, tool, t.projectID, err.Error())
	return errEventsUnavailable
}

func orDefault(n, dflt int) int {
	if n == 0 {
		return dflt
	}
	return n
}

// query turns the filters into a data.LogQuery, reading since and until
// against now. A filter that cannot be read, or a query data.LogQuery.Validate
// refuses, is an error that says which.
func (f eventFilters) query(now time.Time) (data.LogQuery, error) {
	q := data.LogQuery{Severities: f.Severity, Name: f.Name, Tags: f.Tags, Text: f.Text}

	var err error
	if q.Since, err = parseWhen(f.Since, now); err != nil {
		return q, fmt.Errorf("since: %w", err)
	}
	if q.Until, err = parseWhen(f.Until, now); err != nil {
		return q, fmt.Errorf("until: %w", err)
	}
	return q, q.Validate()
}

// firstPage is the page search_events returns when not asked for one.
const firstPage = 1

// maxDaysBack bounds a duration given in days, well past any retention, so it
// cannot overflow a time.Duration.
const maxDaysBack = 36500

// parseWhen reads a point in time as the tools take it: an RFC 3339
// timestamp, a date, which is its UTC midnight, or a duration back from now,
// in Go's units (30m, 24h) or days (7d). Empty is the zero time: no bound.
func parseWhen(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse(time.DateOnly, s); err == nil {
		return t, nil
	}

	var d time.Duration
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.ParseFloat(days, 64)
		if err != nil || n > maxDaysBack {
			return time.Time{}, fmt.Errorf("%q is not a time: want an RFC 3339 timestamp, a date or a duration such as 24h or 7d", s)
		}
		d = time.Duration(n * float64(24*time.Hour))
	} else {
		var err error
		if d, err = time.ParseDuration(s); err != nil {
			return time.Time{}, fmt.Errorf("%q is not a time: want an RFC 3339 timestamp, a date or a duration such as 24h or 7d", s)
		}
	}
	if d <= 0 {
		return time.Time{}, fmt.Errorf("%q: a duration back from now must be positive", s)
	}
	return now.Add(-d), nil
}
