//go:build integration

package integration

import (
	"context"
	"net/rpc"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"

	"logwolf-toolbox/data"
)

// setupUsageModels gives a test an empty usage collection, with its indexes,
// and an empty logs collection.
func setupUsageModels(t *testing.T) (data.Models, *mongo.Database) {
	t.Helper()

	client := testMongo(t, sharedModelsMongo(t))
	db := client.Database("logs")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, name := range []string{"usage", "logs"} {
		if err := db.Collection(name).Drop(ctx); err != nil {
			t.Fatalf("setupUsageModels: drop %s: %v", name, err)
		}
	}

	m := data.New(client)
	if err := m.EnsureUsageIndexes(); err != nil {
		t.Fatalf("setupUsageModels: indexes: %v", err)
	}
	return m, db
}

// TestRecordUsage_RunningTotals: a source's bucket ends at the latest total it
// was sent, however often and in whatever order; the sources of a project add
// up, and a window takes only its own hours.
func TestRecordUsage_RunningTotals(t *testing.T) {
	m, db := setupUsageModels(t)
	ctx := context.Background()

	project, other := primitive.NewObjectID(), primitive.NewObjectID()
	hour := data.UsageHour(time.Now())
	record := func(source string, counts ...data.UsageCount) {
		t.Helper()
		if err := m.RecordUsage(ctx, source, counts); err != nil {
			t.Fatalf("RecordUsage %s: %v", source, err)
		}
	}

	record("broker-a", data.UsageCount{ProjectID: project, Hour: hour.Add(10 * time.Minute), Events: 3, Bytes: 30})
	record("broker-a", data.UsageCount{ProjectID: project, Hour: hour.Add(10 * time.Minute), Events: 3, Bytes: 30}) // a retry
	record("broker-a", data.UsageCount{ProjectID: project, Hour: hour, Events: 5, Bytes: 50})
	record("broker-a", data.UsageCount{ProjectID: project, Hour: hour, Events: 4, Bytes: 40}) // late, and older
	record("broker-b",
		data.UsageCount{ProjectID: project, Hour: hour, Events: 1, Bytes: 10},
		data.UsageCount{ProjectID: other, Hour: hour, Events: 100, Bytes: 1000},
	)
	record("broker-a", data.UsageCount{ProjectID: project, Hour: hour.Add(-time.Hour), Events: 7, Bytes: 70})

	if n, err := db.Collection("usage").CountDocuments(ctx, bson.M{"project_id": project, "hour": hour}); err != nil || n != 2 {
		t.Errorf("buckets for the project's hour = %d, %v; want 2, one per source", n, err)
	}

	cases := []struct {
		name          string
		from, to      time.Time
		events, bytes int64
	}{
		{"this hour", hour, hour.Add(time.Hour), 6, 60},
		{"both hours", hour.Add(-time.Hour), hour.Add(time.Hour), 13, 130},
		{"the hour before", hour.Add(-time.Hour), hour, 7, 70},
		{"no usage", hour.Add(time.Hour), hour.Add(2 * time.Hour), 0, 0},
	}
	for _, tc := range cases {
		got, err := m.GetProjectUsage(ctx, project, tc.from, tc.to)
		if err != nil || got.Events != tc.events || got.Bytes != tc.bytes {
			t.Errorf("%s: GetProjectUsage = %+v, %v; want %d events, %d bytes", tc.name, got, err, tc.events, tc.bytes)
		}
	}
}

// TestProjectStorage: a project's storage is its logs and their BSON size; a
// later measure in the same hour replaces the earlier one, and it is never
// counted as accepted events.
func TestProjectStorage(t *testing.T) {
	m, db := setupUsageModels(t)
	ctx := context.Background()

	project, other, empty := primitive.NewObjectID(), primitive.NewObjectID(), primitive.NewObjectID()
	for i, p := range []primitive.ObjectID{project, project, project, other} {
		if err := m.Insert(data.LogEntry{ProjectID: p, Name: "stored", Data: strings.Repeat("x", 100*(i+1)), Severity: "info"}); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}

	cursor, err := db.Collection("logs").Find(ctx, bson.M{"project_id": project})
	if err != nil {
		t.Fatalf("find logs: %v", err)
	}
	var size int64
	for cursor.Next(ctx) {
		size += int64(len(cursor.Current))
	}
	cursor.Close(ctx)

	got, err := m.MeasureProjectStorage(ctx, project)
	if want := (data.ProjectStorage{Events: 3, Bytes: size}); err != nil || got != want {
		t.Errorf("MeasureProjectStorage = %+v, %v; want %+v", got, err, want)
	}
	if got, err := m.MeasureProjectStorage(ctx, empty); err != nil || got != (data.ProjectStorage{}) {
		t.Errorf("MeasureProjectStorage of a project without logs = %+v, %v; want zero", got, err)
	}

	at := time.Now().Truncate(time.Millisecond)
	if err := m.RecordProjectStorage(ctx, project, at, data.ProjectStorage{Events: 3, Bytes: size}); err != nil {
		t.Fatalf("RecordProjectStorage: %v", err)
	}
	later := at.Add(time.Millisecond)
	if data.UsageHour(later) != data.UsageHour(at) {
		t.Skip("the hour turned between the two measures")
	}
	if err := m.RecordProjectStorage(ctx, project, later, data.ProjectStorage{Events: 4, Bytes: size + 1}); err != nil {
		t.Fatalf("RecordProjectStorage: %v", err)
	}
	if err := m.RecordUsage(ctx, "broker-a", []data.UsageCount{{ProjectID: project, Hour: at, Events: 2, Bytes: 20}}); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}

	if n, err := db.Collection("usage").CountDocuments(ctx, bson.M{"project_id": project, "source": data.StorageSource}); err != nil || n != 1 {
		t.Errorf("storage buckets = %d, %v; want 1 for the hour", n, err)
	}

	usage, err := m.GetProjectUsage(ctx, project, data.UsageHour(at), data.UsageHour(at).Add(time.Hour))
	if err != nil {
		t.Fatalf("GetProjectUsage: %v", err)
	}
	if usage.Events != 2 || usage.Bytes != 20 {
		t.Errorf("accepted = %d events, %d bytes; want 2 and 20, without the storage", usage.Events, usage.Bytes)
	}
	if usage.Storage != (data.ProjectStorage{Events: 4, Bytes: size + 1}) || !usage.StorageMeasuredAt.Equal(later) {
		t.Errorf("storage = %+v at %s; want the later measure, at %s", usage.Storage, usage.StorageMeasuredAt, later)
	}

	before, err := m.GetProjectUsage(ctx, project, data.UsageHour(at).Add(-time.Hour), at)
	if err != nil || before.Storage != (data.ProjectStorage{}) || !before.StorageMeasuredAt.IsZero() {
		t.Errorf("GetProjectUsage of a window before any measure = %+v, %v; want no storage", before, err)
	}
}

// TestEnsureUsageIndexes: one bucket per project, hour and source, and buckets
// expire UsageRetention after their hour. Running it again changes nothing.
func TestEnsureUsageIndexes(t *testing.T) {
	m, db := setupUsageModels(t)
	ctx := context.Background()

	if err := m.EnsureUsageIndexes(); err != nil {
		t.Fatalf("EnsureUsageIndexes again: %v", err)
	}

	cursor, err := db.Collection("usage").Indexes().List(ctx)
	if err != nil {
		t.Fatalf("list indexes: %v", err)
	}
	var indexes []struct {
		Name               string `bson:"name"`
		Unique             bool   `bson:"unique"`
		ExpireAfterSeconds *int64 `bson:"expireAfterSeconds"`
	}
	if err := cursor.All(ctx, &indexes); err != nil {
		t.Fatalf("decode indexes: %v", err)
	}

	var unique, ttl bool
	for _, ix := range indexes {
		switch ix.Name {
		case "usage_project_hour_source":
			unique = ix.Unique
		case "usage_hour_ttl":
			ttl = ix.ExpireAfterSeconds != nil && *ix.ExpireAfterSeconds == int64(data.UsageRetention/time.Second)
		}
	}
	if !unique || !ttl {
		t.Errorf("indexes = %+v; want the unique (project_id, hour, source) and a TTL of %s on hour", indexes, data.UsageRetention)
	}

	doc := bson.M{"project_id": primitive.NewObjectID(), "hour": data.UsageHour(time.Now()), "source": "broker-a"}
	if _, err := db.Collection("usage").InsertOne(ctx, doc); err != nil {
		t.Fatalf("insert bucket: %v", err)
	}
	delete(doc, "_id")
	if _, err := db.Collection("usage").InsertOne(ctx, doc); !mongo.IsDuplicateKeyError(err) {
		t.Errorf("a second bucket for the same project, hour and source: %v, want a duplicate key error", err)
	}
}

// TestUsageMetering_EndToEnd: what a broker accepts reaches the usage
// collection through the logger within a flush interval, and what it flushed
// outlives it: a broker killed and started again adds its new counts to them.
func TestUsageMetering_EndToEnd(t *testing.T) {
	stack := sharedStack(t)
	db := testMongo(t, stack.mongoURI).Database("logs")

	projectID := seedProject(t, stack.mongoURI, "usage-metering")
	const keyStem = "lw_usagemetering"
	key := seedAPIKey(t, stack.mongoURI, projectID, keyStem+strings.Repeat("0", 46-len(keyStem)-1)+"1")

	startBroker := func() (url string, stop func()) {
		addr := freeAddr(t)
		stop = startProcess(t, "../broker/cmd/api", map[string]string{
			"RABBITMQ_URL":         stack.rabbitURI,
			"LOGGER_RPC_ADDR":      stack.loggerRPCAddr,
			"BROKER_PORT":          portOf(addr),
			"INTERNAL_API_SECRET":  internalSecret,
			"USAGE_FLUSH_INTERVAL": "500ms",
		})
		url = "http://" + addr
		if err := waitHTTP(url+"/ping", 60*time.Second); err != nil {
			t.Fatalf("broker: %v", err)
		}
		return url, stop
	}

	// metered adds up the project's accepted events and bytes, and counts the
	// broker runs they came from.
	metered := func() (events, bytes int64, sources int) {
		cursor, err := db.Collection("usage").Find(context.Background(), bson.M{
			"project_id": oid(t, projectID),
			"source":     bson.M{"$ne": data.StorageSource},
		})
		if err != nil {
			t.Fatalf("find usage: %v", err)
		}
		var buckets []struct {
			Source string `bson:"source"`
			Events int64  `bson:"events"`
			Bytes  int64  `bson:"bytes"`
		}
		if err := cursor.All(context.Background(), &buckets); err != nil {
			t.Fatalf("decode usage: %v", err)
		}
		seen := map[string]bool{}
		for _, b := range buckets {
			events += b.Events
			bytes += b.Bytes
			seen[b.Source] = true
		}
		return events, bytes, len(seen)
	}
	waitMetered := func(want int64) (int64, int) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for {
			events, bytes, sources := metered()
			if events == want {
				return bytes, sources
			}
			if time.Now().After(deadline) {
				t.Fatalf("metered %d events, want %d", events, want)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}

	first, stopFirst := startBroker()
	for i := range 4 {
		postLog(t, first, key, "metered-"+string(rune('a'+i)))
	}
	firstBytes, sources := waitMetered(4)
	if firstBytes <= 0 || sources != 1 {
		t.Errorf("after the first broker: %d bytes from %d sources, want some bytes from 1", firstBytes, sources)
	}

	stopFirst() // killed: no final flush, and none needed
	second, _ := startBroker()
	postLog(t, second, key, "metered-after-restart")
	postLog(t, second, key, "metered-after-restart")

	bytes, sources := waitMetered(6)
	if bytes <= firstBytes || sources != 2 {
		t.Errorf("after the restart: %d bytes from %d sources, want more than %d from 2", bytes, sources, firstBytes)
	}
}

// TestRecordUsageRPC: the logger stores a flush's counts, dropping one under a
// malformed project id rather than refusing the rest, and refuses a flush
// without a broker source.
func TestRecordUsageRPC(t *testing.T) {
	stack := sharedStack(t)
	db := testMongo(t, stack.mongoURI).Database("logs")

	conn, err := rpc.Dial("tcp", stack.loggerRPCAddr)
	if err != nil {
		t.Fatalf("dial logger RPC: %v", err)
	}
	defer conn.Close()

	project := primitive.NewObjectID()
	hour := data.UsageHour(time.Now())
	var reply string
	if err := conn.Call("RPCServer.RecordUsage", data.RPCRecordUsageArgs{
		Source: "rpc-test",
		Counts: []data.RPCUsageCount{
			{ProjectID: "not-a-project", Hour: hour, Events: 9, Bytes: 90},
			{ProjectID: project.Hex(), Hour: hour.Add(5 * time.Minute), Events: 2, Bytes: 20},
		},
	}, &reply); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}

	var bucket struct {
		Events int64 `bson:"events"`
		Bytes  int64 `bson:"bytes"`
	}
	err = db.Collection("usage").FindOne(context.Background(), bson.M{"project_id": project, "hour": hour, "source": "rpc-test"}).Decode(&bucket)
	if err != nil || bucket.Events != 2 || bucket.Bytes != 20 {
		t.Errorf("bucket = %+v, %v; want 2 events, 20 bytes in the hour", bucket, err)
	}

	for _, source := range []string{"", data.StorageSource} {
		err := conn.Call("RPCServer.RecordUsage", data.RPCRecordUsageArgs{
			Source: source,
			Counts: []data.RPCUsageCount{{ProjectID: project.Hex(), Hour: hour, Events: 1, Bytes: 1}},
		}, &reply)
		if err == nil || !strings.Contains(err.Error(), data.ErrInvalidUsage.Error()) {
			t.Errorf("RecordUsage from source %q = %v, want %v", source, err, data.ErrInvalidUsage)
		}
	}
}

// TestOrganizationProjectsUsage: an organization's month, project by project,
// names every project in it, those that used nothing included, with the
// events and bytes every broker recorded and the last storage measure; what
// projects since deleted used is added up apart, so the lines add up to the
// organization's events.
func TestOrganizationProjectsUsage(t *testing.T) {
	m, _ := setupOrganizationModels(t)
	_, db := setupUsageModels(t)
	ctx := context.Background()

	org := createOrganization(t, m, "Itemized", 3401, "itemized-owner")
	other := createOrganization(t, m, "Elsewhere", 3402, "elsewhere-owner")
	project := func(o *data.Organization, name string) primitive.ObjectID {
		t.Helper()
		p, err := m.InsertProject(data.Project{Name: name, Slug: strings.ToLower(name), OrganizationID: o.ID})
		if err != nil {
			t.Fatalf("InsertProject: %v", err)
		}
		return p.ID
	}
	api, web, idle, gone, theirs := project(org, "API"), project(org, "Web"), project(org, "Idle"), project(org, "Gone"), project(other, "Theirs")

	month := data.UsageMonth(time.Now())
	record := func(source string, counts ...data.UsageCount) {
		t.Helper()
		if err := m.RecordUsage(ctx, source, counts); err != nil {
			t.Fatalf("RecordUsage %s: %v", source, err)
		}
	}
	record("broker-a",
		data.UsageCount{ProjectID: api, Hour: month, Events: 5, Bytes: 50},
		data.UsageCount{ProjectID: web, Hour: month.Add(time.Hour), Events: 9, Bytes: 90},
		data.UsageCount{ProjectID: gone, Hour: month, Events: 4, Bytes: 40},
		data.UsageCount{ProjectID: theirs, Hour: month, Events: 100, Bytes: 1000},
		data.UsageCount{ProjectID: api, Hour: month.Add(-time.Hour), Events: 50, Bytes: 500}, // last month
	)
	record("broker-b", data.UsageCount{ProjectID: api, Hour: month.Add(2 * time.Hour), Events: 2, Bytes: 20})

	measured := month.Add(30 * time.Minute)
	for _, s := range []struct {
		at      time.Time
		storage data.ProjectStorage
	}{
		{month.Add(-time.Hour), data.ProjectStorage{Events: 1, Bytes: 100}},
		{measured, data.ProjectStorage{Events: 7, Bytes: 700}},
	} {
		if err := m.RecordProjectStorage(ctx, api, s.at, s.storage); err != nil {
			t.Fatalf("RecordProjectStorage: %v", err)
		}
	}

	if _, err := db.Collection("projects").DeleteOne(ctx, bson.M{"_id": gone}); err != nil {
		t.Fatalf("delete project: %v", err)
	}

	got, err := m.GetOrganizationProjectsUsage(ctx, org.ID, month, data.NextUsageMonth(month))
	if err != nil {
		t.Fatalf("GetOrganizationProjectsUsage: %v", err)
	}
	if !got.Month.Equal(month) {
		t.Errorf("month = %v, want %v", got.Month, month)
	}

	type line struct {
		id            primitive.ObjectID
		name          string
		events, bytes int64
		storage       data.ProjectStorage
	}
	want := []line{
		{web, "Web", 9, 90, data.ProjectStorage{}},
		{api, "API", 7, 70, data.ProjectStorage{Events: 7, Bytes: 700}},
		{idle, "Idle", 0, 0, data.ProjectStorage{}},
	}
	if len(got.Projects) != len(want) {
		t.Fatalf("projects = %+v, want %d lines", got.Projects, len(want))
	}
	for i, w := range want {
		p := got.Projects[i]
		if p.ProjectID != w.id.Hex() || p.Name != w.name || p.Events != w.events || p.Bytes != w.bytes || p.Storage != w.storage {
			t.Errorf("line %d = %+v, want %+v", i, p, w)
		}
	}
	if !got.Projects[1].StorageMeasuredAt.Equal(measured) {
		t.Errorf("API's storage measured at %v, want the last measure, at %v", got.Projects[1].StorageMeasuredAt, measured)
	}
	if !got.Projects[0].StorageMeasuredAt.IsZero() {
		t.Errorf("Web's storage measured at %v, want never", got.Projects[0].StorageMeasuredAt)
	}
	if want := (data.UsageTotals{Events: 4, Bytes: 40}); got.Deleted != want {
		t.Errorf("deleted = %+v, want %+v", got.Deleted, want)
	}

	total, err := m.GetOrganizationEvents(ctx, org.ID, month, data.NextUsageMonth(month))
	if err != nil || total != 9+7+4 {
		t.Errorf("GetOrganizationEvents = %d, %v; want the lines' 20", total, err)
	}

	empty := createOrganization(t, m, "Empty", 3403, "empty-owner")
	if got, err := m.GetOrganizationProjectsUsage(ctx, empty.ID, month, data.NextUsageMonth(month)); err != nil || len(got.Projects) != 0 || got.Deleted != (data.UsageTotals{}) {
		t.Errorf("GetOrganizationProjectsUsage of an organization without projects = %+v, %v; want nothing", got, err)
	}
}
