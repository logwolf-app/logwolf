package data

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// LogQuery narrows a project's logs to the ones a search or a count looks at.
// Every field it sets must hold; the zero LogQuery is every log of the project.
type LogQuery struct {
	// Severities keeps logs of any of these severities. NormalizeSeverity has
	// to accept each one; they are matched normalized, the way the metrics
	// count them, so a log stored as "ERROR" before severities were normalized
	// is not matched.
	Severities []string
	// Name keeps logs with exactly this name.
	Name string
	// Tags keeps logs that carry every one of these tags.
	Tags []string
	// Text keeps logs whose name or data contains it, ignoring case. It is
	// matched literally, not as a pattern.
	Text string
	// Since keeps logs created at or after it; zero is no lower bound.
	Since time.Time
	// Until keeps logs created before it; zero is no upper bound.
	Until time.Time
}

const (
	// MaxQueryTags caps LogQuery.Tags.
	MaxQueryTags = 10
	// MaxQueryTextLength caps LogQuery.Text, in bytes. Text is matched against
	// every log's data in the window, so it is kept to what a search needs.
	MaxQueryTextLength = 200
)

// ErrInvalidQuery is what a LogQuery that cannot be run is.
var ErrInvalidQuery = errors.New("invalid query")

// Validate reports whether q can be run: known severities, at most
// MaxQueryTags tags, a Text of at most MaxQueryTextLength bytes, and a window
// that does not end before it starts.
func (q LogQuery) Validate() error {
	for _, s := range q.Severities {
		if _, err := NormalizeSeverity(s); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidQuery, err)
		}
	}
	if len(q.Tags) > MaxQueryTags {
		return fmt.Errorf("%w: at most %d tags", ErrInvalidQuery, MaxQueryTags)
	}
	if len(q.Text) > MaxQueryTextLength {
		return fmt.Errorf("%w: text must be at most %d bytes", ErrInvalidQuery, MaxQueryTextLength)
	}
	if !q.Since.IsZero() && !q.Until.IsZero() && !q.Since.Before(q.Until) {
		return fmt.Errorf("%w: since must be before until", ErrInvalidQuery)
	}
	return nil
}

// filter is the MongoDB filter for q's logs within the project. The project
// always comes first: every query is scoped to one, and the
// project_id_created_at index serves it.
func (q LogQuery) filter(projectID primitive.ObjectID) bson.D {
	f := bson.D{{Key: "project_id", Value: projectID}}

	if !q.Since.IsZero() || !q.Until.IsZero() {
		window := bson.D{}
		if !q.Since.IsZero() {
			window = append(window, bson.E{Key: "$gte", Value: q.Since})
		}
		if !q.Until.IsZero() {
			window = append(window, bson.E{Key: "$lt", Value: q.Until})
		}
		f = append(f, bson.E{Key: "created_at", Value: window})
	}
	if len(q.Severities) > 0 {
		severities := bson.A{}
		for _, s := range q.Severities {
			n, _ := NormalizeSeverity(s) // Validate refused anything else
			if !slices.Contains(severities, any(n)) {
				severities = append(severities, n)
			}
		}
		f = append(f, bson.E{Key: "severity", Value: bson.D{{Key: "$in", Value: severities}}})
	}
	if q.Name != "" {
		f = append(f, bson.E{Key: "name", Value: q.Name})
	}
	if len(q.Tags) > 0 {
		f = append(f, bson.E{Key: "tags", Value: bson.D{{Key: "$all", Value: q.Tags}}})
	}
	if q.Text != "" {
		contains := primitive.Regex{Pattern: regexp.QuoteMeta(q.Text), Options: "i"}
		f = append(f, bson.E{Key: "$or", Value: bson.A{
			bson.D{{Key: "name", Value: contains}},
			bson.D{{Key: "data", Value: contains}},
		}})
	}
	return f
}

// RPCSearchLogsArgs is the RPC argument for SearchLogs.
type RPCSearchLogsArgs struct {
	ProjectID  string
	Query      LogQuery
	Pagination PaginationParams
}

// SearchLogsReply is SearchLogs' reply: one page of matching logs, newest
// first, and whether a later page has any.
type SearchLogsReply struct {
	Logs    []LogEntry
	HasMore bool
}

// SearchLogs returns one page of a project's logs that match q, newest first,
// and whether there are more after it. A query or a page that Validate refuses
// is refused before any query.
func (m *Models) SearchLogs(projectID primitive.ObjectID, q LogQuery, p PaginationParams) (*SearchLogsReply, error) {
	if err := q.Validate(); err != nil {
		return nil, fmt.Errorf("SearchLogs: %w", err)
	}
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("SearchLogs: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// One more than the page, to tell whether another page follows. The sort
	// is AllLogs', which the project_id_created_at index serves; a tie-break on
	// _id would make MongoDB sort the whole match in memory.
	opts := options.Find().
		SetSort(bson.D{{Key: "created_at", Value: -1}}).
		SetSkip(p.PageSize * (p.Page - 1)).
		SetLimit(p.PageSize + 1)

	cursor, err := m.client.Database("logs").Collection("logs").Find(ctx, q.filter(projectID), opts)
	if err != nil {
		return nil, fmt.Errorf("SearchLogs find: %w", err)
	}
	defer cursor.Close(ctx)

	reply := &SearchLogsReply{Logs: []LogEntry{}}
	if err := cursor.All(ctx, &reply.Logs); err != nil {
		return nil, fmt.Errorf("SearchLogs decode: %w", err)
	}
	if int64(len(reply.Logs)) > p.PageSize {
		reply.Logs, reply.HasMore = reply.Logs[:p.PageSize], true
	}
	return reply, nil
}

// What CountLogs can group logs by.
const (
	GroupBySeverity = "severity"
	GroupByName     = "name"
	// GroupByTag counts each tag a log carries, so a log with two tags counts
	// in two groups and one with none in no group.
	GroupByTag = "tag"
	// GroupByHour and GroupByDay count logs per UTC hour or day of their
	// creation, keyed 2006-01-02T15:00:00Z and 2006-01-02.
	GroupByHour = "hour"
	GroupByDay  = "day"
)

// GroupByFields lists every grouping CountLogs knows.
var GroupByFields = []string{GroupBySeverity, GroupByName, GroupByTag, GroupByHour, GroupByDay}

const (
	// DefaultCountGroups is how many groups CountLogs returns when not asked.
	DefaultCountGroups = 20
	// MaxCountGroups caps the groups CountLogs returns: a week of hours fits.
	MaxCountGroups = 200
)

// RPCCountLogsArgs is the RPC argument for CountLogs.
type RPCCountLogsArgs struct {
	ProjectID string
	Query     LogQuery
	// GroupBy is one of GroupByFields, or empty for the total alone.
	GroupBy string
	// Limit is how many groups to return, 1 to MaxCountGroups.
	Limit int
}

// LogCount is one group of CountLogs: the key the logs share, and how many
// there are.
type LogCount struct {
	Key   string `bson:"_id" json:"key"`
	Count int64  `bson:"count" json:"count"`
}

// LogCounts is CountLogs' reply.
type LogCounts struct {
	// Total is how many logs match the query.
	Total int64 `json:"total"`
	// Groups holds the largest groups first, or, grouped by hour or day, the
	// latest periods in time order. Empty when not grouping.
	Groups []LogCount `json:"groups"`
	// Truncated is whether there were more groups than the limit.
	Truncated bool `json:"truncated"`
}

// groupKey is the $group key of each grouping. Severities are lower-cased, so
// logs stored before severities were normalized fall in the same group as the
// rest.
var groupKey = map[string]any{
	GroupBySeverity: bson.D{{Key: "$toLower", Value: "$severity"}},
	GroupByName:     "$name",
	GroupByTag:      "$tags",
	GroupByHour:     bson.D{{Key: "$dateToString", Value: bson.D{{Key: "format", Value: "%Y-%m-%dT%H:00:00Z"}, {Key: "date", Value: "$created_at"}}}},
	GroupByDay:      bson.D{{Key: "$dateToString", Value: bson.D{{Key: "format", Value: "%Y-%m-%d"}, {Key: "date", Value: "$created_at"}}}},
}

// ValidateCount reports whether CountLogs can run args' grouping.
func (args RPCCountLogsArgs) ValidateCount() error {
	if args.GroupBy != "" && groupKey[args.GroupBy] == nil {
		return fmt.Errorf("%w: cannot group by %q", ErrInvalidQuery, args.GroupBy)
	}
	if args.Limit < 1 || args.Limit > MaxCountGroups {
		return fmt.Errorf("%w: limit must be between 1 and %d", ErrInvalidQuery, MaxCountGroups)
	}
	return args.Query.Validate()
}

// countPipeline is the aggregation behind CountLogs: one $facet that counts
// the matching logs and, when grouping, their groups, one more than the limit
// to tell whether any were left out.
func countPipeline(projectID primitive.ObjectID, args RPCCountLogsArgs) mongo.Pipeline {
	facets := bson.D{{Key: "total", Value: bson.A{bson.D{{Key: "$count", Value: "n"}}}}}

	if args.GroupBy != "" {
		var groups bson.A
		if args.GroupBy == GroupByTag {
			groups = append(groups, bson.D{{Key: "$unwind", Value: "$tags"}})
		}
		// Time periods are wanted latest first, to keep the latest when they are
		// cut off; CountLogs puts them back in time order. Anything else is
		// wanted largest first, ties broken by key so a cut-off is stable.
		sort := bson.D{{Key: "count", Value: -1}, {Key: "_id", Value: 1}}
		if args.GroupBy == GroupByHour || args.GroupBy == GroupByDay {
			sort = bson.D{{Key: "_id", Value: -1}}
		}
		groups = append(groups,
			bson.D{{Key: "$group", Value: bson.D{
				{Key: "_id", Value: groupKey[args.GroupBy]},
				{Key: "count", Value: bson.D{{Key: "$sum", Value: 1}}},
			}}},
			bson.D{{Key: "$sort", Value: sort}},
			bson.D{{Key: "$limit", Value: args.Limit + 1}},
		)
		facets = append(facets, bson.E{Key: "groups", Value: groups})
	}

	return mongo.Pipeline{
		{{Key: "$match", Value: args.Query.filter(projectID)}},
		{{Key: "$facet", Value: facets}},
	}
}

// CountLogs counts a project's logs that match args.Query, grouped by
// args.GroupBy when it names a grouping.
func (m *Models) CountLogs(projectID primitive.ObjectID, args RPCCountLogsArgs) (*LogCounts, error) {
	if err := args.ValidateCount(); err != nil {
		return nil, fmt.Errorf("CountLogs: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	cursor, err := m.client.Database("logs").Collection("logs").Aggregate(ctx, countPipeline(projectID, args))
	if err != nil {
		return nil, fmt.Errorf("CountLogs aggregate: %w", err)
	}
	defer cursor.Close(ctx)

	// $facet always returns exactly one document.
	var facets []struct {
		Total []struct {
			N int64 `bson:"n"`
		} `bson:"total"`
		Groups []LogCount `bson:"groups"`
	}
	if err := cursor.All(ctx, &facets); err != nil {
		return nil, fmt.Errorf("CountLogs decode: %w", err)
	}

	counts := &LogCounts{Groups: []LogCount{}}
	if len(facets) == 0 {
		return counts, nil
	}
	if len(facets[0].Total) > 0 {
		counts.Total = facets[0].Total[0].N
	}
	if groups := facets[0].Groups; groups != nil {
		if len(groups) > args.Limit {
			groups, counts.Truncated = groups[:args.Limit], true
		}
		if args.GroupBy == GroupByHour || args.GroupBy == GroupByDay {
			slices.Reverse(groups)
		}
		counts.Groups = groups
	}
	return counts, nil
}
