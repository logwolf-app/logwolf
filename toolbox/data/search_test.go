package data

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestLogQuery_Validate(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		q    LogQuery
		ok   bool
	}{
		{"every log", LogQuery{}, true},
		{"severities in any casing", LogQuery{Severities: []string{"ERROR", " critical"}}, true},
		{"unknown severity", LogQuery{Severities: []string{"error", "fatal"}}, false},
		{"most tags", LogQuery{Tags: make([]string, MaxQueryTags)}, true},
		{"too many tags", LogQuery{Tags: make([]string, MaxQueryTags+1)}, false},
		{"longest text", LogQuery{Text: strings.Repeat("x", MaxQueryTextLength)}, true},
		{"text too long", LogQuery{Text: strings.Repeat("x", MaxQueryTextLength+1)}, false},
		{"window", LogQuery{Since: t0, Until: t0.Add(time.Hour)}, true},
		{"since alone", LogQuery{Since: t0}, true},
		{"until alone", LogQuery{Until: t0}, true},
		{"empty window", LogQuery{Since: t0, Until: t0}, false},
		{"window backwards", LogQuery{Since: t0.Add(time.Hour), Until: t0}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.q.Validate()
			if tc.ok && err != nil {
				t.Errorf("Validate() = %v, want nil", err)
			}
			if !tc.ok && !errors.Is(err, ErrInvalidQuery) {
				t.Errorf("Validate() = %v, want ErrInvalidQuery", err)
			}
		})
	}
}

func TestLogQuery_FilterIsScopedToTheProject(t *testing.T) {
	project := primitive.NewObjectID()

	if got, want := (LogQuery{}).filter(project), (bson.D{{Key: "project_id", Value: project}}); !reflect.DeepEqual(got, want) {
		t.Errorf("filter of the zero query = %v, want %v", got, want)
	}

	// Whatever else it asks for, the project comes first.
	f := LogQuery{Name: "signup", Text: "x", Tags: []string{"web"}}.filter(project)
	if f[0].Key != "project_id" || f[0].Value != project {
		t.Errorf("filter starts with %v, want project_id %v", f[0], project)
	}
}

func TestLogQuery_Filter(t *testing.T) {
	project := primitive.NewObjectID()
	since := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	until := since.Add(24 * time.Hour)

	q := LogQuery{
		Severities: []string{"ERROR", "error", "critical"},
		Name:       "checkout",
		Tags:       []string{"web", "eu"},
		Text:       "card (declined)",
		Since:      since,
		Until:      until,
	}
	got := map[string]any{}
	for _, e := range q.filter(project) {
		got[e.Key] = e.Value
	}

	want := map[string]any{
		"project_id": project,
		"created_at": bson.D{{Key: "$gte", Value: since}, {Key: "$lt", Value: until}},
		// Normalized, and each once.
		"severity": bson.D{{Key: "$in", Value: bson.A{"error", "critical"}}},
		"name":     "checkout",
		"tags":     bson.D{{Key: "$all", Value: []string{"web", "eu"}}},
		// Literal: the parentheses are escaped, not a group.
		"$or": bson.A{
			bson.D{{Key: "name", Value: primitive.Regex{Pattern: `card \(declined\)`, Options: "i"}}},
			bson.D{{Key: "data", Value: primitive.Regex{Pattern: `card \(declined\)`, Options: "i"}}},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("filter =\n%v\nwant\n%v", got, want)
	}
}

func TestLogQuery_FilterHalfOpenWindow(t *testing.T) {
	project := primitive.NewObjectID()
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		q    LogQuery
		want bson.D
	}{
		{LogQuery{Since: t0}, bson.D{{Key: "$gte", Value: t0}}},
		{LogQuery{Until: t0}, bson.D{{Key: "$lt", Value: t0}}},
	} {
		f := tc.q.filter(project)
		if len(f) != 2 || f[1].Key != "created_at" || !reflect.DeepEqual(f[1].Value, tc.want) {
			t.Errorf("filter(%+v) = %v, want created_at %v", tc.q, f, tc.want)
		}
	}
}

func TestRPCCountLogsArgs_ValidateCount(t *testing.T) {
	cases := []struct {
		name string
		args RPCCountLogsArgs
		ok   bool
	}{
		{"total alone", RPCCountLogsArgs{Limit: 1}, true},
		{"most groups", RPCCountLogsArgs{GroupBy: GroupByHour, Limit: MaxCountGroups}, true},
		{"too many groups", RPCCountLogsArgs{GroupBy: GroupByHour, Limit: MaxCountGroups + 1}, false},
		{"no groups", RPCCountLogsArgs{GroupBy: GroupByName, Limit: 0}, false},
		{"unknown grouping", RPCCountLogsArgs{GroupBy: "data", Limit: 10}, false},
		{"invalid query", RPCCountLogsArgs{Limit: 10, Query: LogQuery{Severities: []string{"fatal"}}}, false},
	}
	for _, g := range GroupByFields {
		cases = append(cases, struct {
			name string
			args RPCCountLogsArgs
			ok   bool
		}{"by " + g, RPCCountLogsArgs{GroupBy: g, Limit: DefaultCountGroups}, true})
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.args.ValidateCount()
			if tc.ok && err != nil {
				t.Errorf("ValidateCount() = %v, want nil", err)
			}
			if !tc.ok && !errors.Is(err, ErrInvalidQuery) {
				t.Errorf("ValidateCount() = %v, want ErrInvalidQuery", err)
			}
		})
	}
}

// TestCountPipeline_Groups checks each grouping's stages: tags are unwound
// before grouping, periods are sorted latest first, and one group more than
// the limit is asked for, to tell whether any were left out.
func TestCountPipeline_Groups(t *testing.T) {
	project := primitive.NewObjectID()

	totalOnly := countPipeline(project, RPCCountLogsArgs{Limit: 5})
	facets := totalOnly[1][0].Value.(bson.D)
	if len(facets) != 1 || facets[0].Key != "total" {
		t.Errorf("pipeline without a grouping has facets %v, want total alone", facets)
	}

	for _, tc := range []struct {
		groupBy string
		unwind  bool
		sort    bson.D
	}{
		{GroupBySeverity, false, bson.D{{Key: "count", Value: -1}, {Key: "_id", Value: 1}}},
		{GroupByName, false, bson.D{{Key: "count", Value: -1}, {Key: "_id", Value: 1}}},
		{GroupByTag, true, bson.D{{Key: "count", Value: -1}, {Key: "_id", Value: 1}}},
		{GroupByHour, false, bson.D{{Key: "_id", Value: -1}}},
		{GroupByDay, false, bson.D{{Key: "_id", Value: -1}}},
	} {
		t.Run(tc.groupBy, func(t *testing.T) {
			p := countPipeline(project, RPCCountLogsArgs{GroupBy: tc.groupBy, Limit: 5})

			if match := p[0][0]; match.Key != "$match" || !reflect.DeepEqual(match.Value, (LogQuery{}).filter(project)) {
				t.Errorf("first stage = %v, want the query's $match", match)
			}

			var groups bson.A
			for _, f := range p[1][0].Value.(bson.D) {
				if f.Key == "groups" {
					groups = f.Value.(bson.A)
				}
			}
			stages := map[string]any{}
			var order []string
			for _, s := range groups {
				st := s.(bson.D)[0]
				stages[st.Key] = st.Value
				order = append(order, st.Key)
			}

			wantOrder := []string{"$group", "$sort", "$limit"}
			if tc.unwind {
				wantOrder = append([]string{"$unwind"}, wantOrder...)
			}
			if !reflect.DeepEqual(order, wantOrder) {
				t.Fatalf("group stages = %v, want %v", order, wantOrder)
			}
			if !reflect.DeepEqual(stages["$sort"], tc.sort) {
				t.Errorf("$sort = %v, want %v", stages["$sort"], tc.sort)
			}
			if stages["$limit"] != 6 {
				t.Errorf("$limit = %v, want the limit plus one", stages["$limit"])
			}
		})
	}
}
