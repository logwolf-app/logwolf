package data

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// --- Usage metering ---
// The usage collection holds what each project uses, in hourly buckets: one
// document per project, hour and source.
//
// Each broker is a source. It counts the events it accepts (answers 202 for)
// and their bytes in memory, per project per hour, and flushes its running
// totals for the hour through the logger's RecordUsage RPC. A bucket holds a
// broker's total so far, not an increment: a flush that is retried, or whose
// reply was lost after the write, sets the same values again instead of
// counting them twice. A broker that restarts is a new source, so what its
// previous run flushed stays in that run's buckets.
//
// A broker's bucket also names the organization the project was in when it
// was recorded, so an organization's events add up across its projects
// (GetOrganizationEvents), deleted ones included: the monthly event quota is
// the organization's, and deleting a project does not give its events back.
//
// The logger's storage job is the source StorageSource. Its bucket holds the
// project's stored logs as last measured in that hour.
//
// The buckets expire UsageRetention after their hour, on a TTL index: usage
// outlives the logs it counts, however short the project's retention.

// UsageRetention is how long a usage bucket is kept after its hour: a little
// over a year, so the same month of the year before is still there.
//
// It is the TTL of an index MongoDB already has on every deployment, and
// creating that index again with another TTL fails. Changing it takes a
// collMod of usage_hour_ttl on existing databases, not just a new value here.
const UsageRetention = 400 * 24 * time.Hour

// StorageSource is the source of the buckets the logger's storage job writes.
// No broker may use it.
const StorageSource = "storage"

// UsageHour is the hour bucket t falls in: t truncated to the hour, in UTC.
func UsageHour(t time.Time) time.Time {
	return t.UTC().Truncate(time.Hour)
}

// UsageMonth is the calendar month t falls in, in UTC: the first instant of it.
// The monthly event quota counts the events of the hour buckets in it.
func UsageMonth(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// NextUsageMonth is the first instant of the month after t's, in UTC: when a
// quota used up in t's month is renewed.
func NextUsageMonth(t time.Time) time.Time {
	return UsageMonth(t).AddDate(0, 1, 0)
}

// UsageCount is a source's running total for one project in one hour: the
// events it accepted and their size in bytes.
type UsageCount struct {
	ProjectID primitive.ObjectID
	Hour      time.Time
	Events    int64
	Bytes     int64
}

// RPCUsageCount is a UsageCount as the RecordUsage RPC carries it, with the
// project as a hex string.
type RPCUsageCount struct {
	ProjectID string
	Hour      time.Time
	Events    int64
	Bytes     int64
}

// RPCRecordUsageArgs is the RPC argument for RecordUsage: a broker's running
// totals, per project and hour, for the buckets that changed since its last
// flush.
type RPCRecordUsageArgs struct {
	// Source names the broker run the counts are from.
	Source string
	Counts []RPCUsageCount
}

// ProjectStorage is how much a project stores: its logs, and their size as
// BSON documents. The size is the logical one, before compression and without
// indexes, so it is the same however MongoDB lays the collection out.
type ProjectStorage struct {
	Events int64 `bson:"stored_events" json:"events"`
	Bytes  int64 `bson:"stored_bytes" json:"bytes"`
}

// ProjectUsage is what a project used over a window of hours: the events
// accepted for it and their bytes, from every source, and its storage as last
// measured before the window ended.
type ProjectUsage struct {
	Events  int64          `json:"events"`
	Bytes   int64          `json:"bytes"`
	Storage ProjectStorage `json:"storage"`
	// StorageMeasuredAt is when Storage was measured; zero when it never was.
	StorageMeasuredAt time.Time `json:"storage_measured_at"`
}

// ProjectQuota is what a project's events are held to under the monthly event
// quota: the organization they count toward, the name of its plan, which sets
// the quota, and the events the organization has ingested in Month so far, as
// far as the brokers have flushed them.
type ProjectQuota struct {
	// OrganizationID is the organization's id as a hex string, the way
	// services pass ids to each other.
	OrganizationID string
	Plan           string
	Month          time.Time
	Events         int64
}

// ErrInvalidUsage is what RecordUsage answers for counts it cannot store.
var ErrInvalidUsage = errors.New("invalid usage")

func (m *Models) usage() *mongo.Collection {
	return m.client.Database("logs").Collection("usage")
}

// EnsureUsageIndexes creates the indexes on usage: the unique (project_id,
// hour, source), which keeps one bucket per source per project per hour and
// serves a project's reads over a window; (organization_id, hour), which serves
// an organization's month (GetOrganizationEvents); and the TTL index on hour,
// which expires buckets UsageRetention after their hour. Safe to call on
// startup.
func (m *Models) EnsureUsageIndexes() error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	coll := m.usage()
	if _, err := coll.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "project_id", Value: 1}, {Key: "hour", Value: 1}, {Key: "source", Value: 1}},
		Options: options.Index().SetUnique(true).SetName("usage_project_hour_source"),
	}); err != nil {
		return fmt.Errorf("EnsureUsageIndexes usage.(project_id,hour,source): %w", err)
	}

	if _, err := coll.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "organization_id", Value: 1}, {Key: "hour", Value: 1}},
		Options: options.Index().SetName("usage_organization_hour"),
	}); err != nil {
		return fmt.Errorf("EnsureUsageIndexes usage.(organization_id,hour): %w", err)
	}

	if _, err := coll.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "hour", Value: 1}},
		Options: options.Index().SetName("usage_hour_ttl").SetExpireAfterSeconds(int32(UsageRetention / time.Second)),
	}); err != nil {
		return fmt.Errorf("EnsureUsageIndexes usage.hour: %w", err)
	}
	return nil
}

// RecordUsage stores a source's running totals. Each count sets its bucket to
// the larger of the stored values and its own, so it can be sent any number of
// times, in any order, and the bucket ends at the source's latest total.
//
// Each bucket is also given the organization its project is in now, for
// GetOrganizationEvents. One whose project no longer exists keeps the
// organization it already names.
//
// The source must be a broker's: neither empty nor StorageSource. Counts are
// refused whole, before any write, if one is negative.
func (m *Models) RecordUsage(ctx context.Context, source string, counts []UsageCount) error {
	if source == "" || source == StorageSource {
		return fmt.Errorf("RecordUsage: %w: source %q", ErrInvalidUsage, source)
	}
	for _, c := range counts {
		if c.Events < 0 || c.Bytes < 0 {
			return fmt.Errorf("RecordUsage: %w: negative count for project %s", ErrInvalidUsage, c.ProjectID.Hex())
		}
	}
	if len(counts) == 0 {
		return nil
	}

	orgs, err := m.organizationsOfProjects(ctx, counts)
	if err != nil {
		return fmt.Errorf("RecordUsage: %w", err)
	}

	now := time.Now()
	writes := make([]mongo.WriteModel, len(counts))
	for i, c := range counts {
		set := bson.M{"updated_at": now}
		if org, ok := orgs[c.ProjectID]; ok {
			set["organization_id"] = org
		}
		writes[i] = mongo.NewUpdateOneModel().
			SetFilter(bson.M{"project_id": c.ProjectID, "hour": UsageHour(c.Hour), "source": source}).
			SetUpdate(bson.M{
				"$max": bson.M{"events": c.Events, "bytes": c.Bytes},
				"$set": set,
			}).
			SetUpsert(true)
	}
	if _, err := m.usage().BulkWrite(ctx, writes, options.BulkWrite().SetOrdered(false)); err != nil {
		return fmt.Errorf("RecordUsage: %w", err)
	}
	return nil
}

// organizationsOfProjects maps the projects counts name to the organizations
// they are in. A project that does not exist, or is in no organization, is
// left out.
func (m *Models) organizationsOfProjects(ctx context.Context, counts []UsageCount) (map[primitive.ObjectID]primitive.ObjectID, error) {
	ids := make([]primitive.ObjectID, 0, len(counts))
	seen := make(map[primitive.ObjectID]bool, len(counts))
	for _, c := range counts {
		if !seen[c.ProjectID] {
			seen[c.ProjectID] = true
			ids = append(ids, c.ProjectID)
		}
	}

	cursor, err := m.client.Database("logs").Collection("projects").Find(ctx,
		bson.M{"_id": bson.M{"$in": ids}},
		options.Find().SetProjection(bson.M{"organization_id": 1}),
	)
	if err != nil {
		return nil, fmt.Errorf("organizations of projects: %w", err)
	}
	var projects []Project
	if err := cursor.All(ctx, &projects); err != nil {
		return nil, fmt.Errorf("organizations of projects decode: %w", err)
	}

	orgs := make(map[primitive.ObjectID]primitive.ObjectID, len(projects))
	for _, p := range projects {
		if !p.OrganizationID.IsZero() {
			orgs[p.ID] = p.OrganizationID
		}
	}
	return orgs, nil
}

// MeasureProjectStorage counts a project's logs and adds up their sizes. It
// reads every one of them, so it belongs in a periodic job, not a request.
func (m *Models) MeasureProjectStorage(ctx context.Context, projectID primitive.ObjectID) (ProjectStorage, error) {
	cursor, err := m.client.Database("logs").Collection("logs").Aggregate(ctx, mongo.Pipeline{
		{{Key: "$match", Value: bson.M{"project_id": projectID}}},
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: nil},
			{Key: "stored_events", Value: bson.D{{Key: "$sum", Value: 1}}},
			{Key: "stored_bytes", Value: bson.D{{Key: "$sum", Value: bson.D{{Key: "$bsonSize", Value: "$$ROOT"}}}}},
		}}},
	})
	if err != nil {
		return ProjectStorage{}, fmt.Errorf("MeasureProjectStorage: %w", err)
	}
	var out []ProjectStorage
	if err := cursor.All(ctx, &out); err != nil {
		return ProjectStorage{}, fmt.Errorf("MeasureProjectStorage decode: %w", err)
	}
	if len(out) == 0 {
		return ProjectStorage{}, nil
	}
	return out[0], nil
}

// RecordProjectStorage stores a measure of a project's storage, taken at, in
// the StorageSource bucket of at's hour. A later measure in the same hour
// replaces it.
func (m *Models) RecordProjectStorage(ctx context.Context, projectID primitive.ObjectID, at time.Time, s ProjectStorage) error {
	_, err := m.usage().UpdateOne(ctx,
		bson.M{"project_id": projectID, "hour": UsageHour(at), "source": StorageSource},
		bson.M{"$set": bson.M{
			"stored_events": s.Events,
			"stored_bytes":  s.Bytes,
			"measured_at":   at,
			"updated_at":    time.Now(),
		}},
		options.Update().SetUpsert(true),
	)
	if err != nil {
		return fmt.Errorf("RecordProjectStorage: %w", err)
	}
	return nil
}

// GetProjectUsage adds up what a project used in the hour buckets from from's
// up to, but not including, to: the events and bytes every source recorded.
// Its storage is the last measure taken before to.
func (m *Models) GetProjectUsage(ctx context.Context, projectID primitive.ObjectID, from, to time.Time) (ProjectUsage, error) {
	coll := m.usage()

	cursor, err := coll.Aggregate(ctx, mongo.Pipeline{
		{{Key: "$match", Value: bson.M{
			"project_id": projectID,
			"hour":       bson.M{"$gte": UsageHour(from), "$lt": to},
			"source":     bson.M{"$ne": StorageSource},
		}}},
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: nil},
			{Key: "events", Value: bson.D{{Key: "$sum", Value: "$events"}}},
			{Key: "bytes", Value: bson.D{{Key: "$sum", Value: "$bytes"}}},
		}}},
	})
	if err != nil {
		return ProjectUsage{}, fmt.Errorf("GetProjectUsage: %w", err)
	}
	var sums []struct {
		Events int64 `bson:"events"`
		Bytes  int64 `bson:"bytes"`
	}
	if err := cursor.All(ctx, &sums); err != nil {
		return ProjectUsage{}, fmt.Errorf("GetProjectUsage decode: %w", err)
	}

	var usage ProjectUsage
	if len(sums) > 0 {
		usage.Events, usage.Bytes = sums[0].Events, sums[0].Bytes
	}

	var storage struct {
		ProjectStorage `bson:",inline"`
		MeasuredAt     time.Time `bson:"measured_at"`
	}
	err = coll.FindOne(ctx,
		bson.M{"project_id": projectID, "source": StorageSource, "measured_at": bson.M{"$lt": to}},
		options.FindOne().SetSort(bson.D{{Key: "measured_at", Value: -1}}),
	).Decode(&storage)
	switch {
	case errors.Is(err, mongo.ErrNoDocuments):
	case err != nil:
		return ProjectUsage{}, fmt.Errorf("GetProjectUsage storage: %w", err)
	default:
		usage.Storage, usage.StorageMeasuredAt = storage.ProjectStorage, storage.MeasuredAt
	}
	return usage, nil
}

// GetOrganizationEvents adds up the events every source recorded for the
// organization's projects, deleted ones included, in the hour buckets from
// from's up to, but not including, to.
func (m *Models) GetOrganizationEvents(ctx context.Context, orgID primitive.ObjectID, from, to time.Time) (int64, error) {
	cursor, err := m.usage().Aggregate(ctx, mongo.Pipeline{
		{{Key: "$match", Value: bson.M{
			"organization_id": orgID,
			"hour":            bson.M{"$gte": UsageHour(from), "$lt": to},
			"source":          bson.M{"$ne": StorageSource},
		}}},
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: nil},
			{Key: "events", Value: bson.D{{Key: "$sum", Value: "$events"}}},
		}}},
	})
	if err != nil {
		return 0, fmt.Errorf("GetOrganizationEvents: %w", err)
	}
	var sums []struct {
		Events int64 `bson:"events"`
	}
	if err := cursor.All(ctx, &sums); err != nil {
		return 0, fmt.Errorf("GetOrganizationEvents decode: %w", err)
	}
	if len(sums) == 0 {
		return 0, nil
	}
	return sums[0].Events, nil
}

// ProjectQuota returns what the project's events are held to in the month of
// now: its organization, that organization's plan, and the events the
// organization has ingested in that month so far. It fails like ProjectPlan: a
// project that does not exist is mongo.ErrNoDocuments (wrapped); one in no
// organization, or in one that does not exist, is ErrUnknownOrganization.
func (m *Models) ProjectQuota(ctx context.Context, projectID primitive.ObjectID, now time.Time) (ProjectQuota, error) {
	o, err := m.projectOrganization(ctx, "ProjectQuota", projectID)
	if err != nil {
		return ProjectQuota{}, err
	}
	month := UsageMonth(now)
	events, err := m.GetOrganizationEvents(ctx, o.ID, month, NextUsageMonth(month))
	if err != nil {
		return ProjectQuota{}, fmt.Errorf("ProjectQuota: %w", err)
	}
	return ProjectQuota{OrganizationID: o.ID.Hex(), Plan: o.Plan, Month: month, Events: events}, nil
}
