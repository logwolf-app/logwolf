//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/rpc"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"

	"logwolf-toolbox/data"
	"logwolf-toolbox/limits"
)

// TestOrganizationEvents: an organization's month is the events every broker
// recorded for its projects in that month, deleted projects included, and
// nothing of other months, other organizations or the storage job.
func TestOrganizationEvents(t *testing.T) {
	m, _ := setupOrganizationModels(t)
	_, db := setupUsageModels(t)
	ctx := context.Background()

	org := createOrganization(t, m, "Metered", 3301, "metered-owner")
	other := createOrganization(t, m, "Elsewhere", 3302, "elsewhere-owner")
	project := func(o *data.Organization) primitive.ObjectID {
		t.Helper()
		p, err := m.InsertProject(data.Project{Name: "P", Slug: "p", OrganizationID: o.ID})
		if err != nil {
			t.Fatalf("InsertProject: %v", err)
		}
		return p.ID
	}
	first, second, theirs := project(org), project(org), project(other)

	month := data.UsageMonth(time.Now())
	record := func(source string, counts ...data.UsageCount) {
		t.Helper()
		if err := m.RecordUsage(ctx, source, counts); err != nil {
			t.Fatalf("RecordUsage %s: %v", source, err)
		}
	}
	record("broker-a",
		data.UsageCount{ProjectID: first, Hour: month, Events: 5, Bytes: 50},
		data.UsageCount{ProjectID: second, Hour: month.Add(time.Hour), Events: 3, Bytes: 30},
		data.UsageCount{ProjectID: theirs, Hour: month, Events: 100, Bytes: 1000},
		data.UsageCount{ProjectID: first, Hour: month.Add(-time.Hour), Events: 50, Bytes: 500}, // last month
	)
	record("broker-b", data.UsageCount{ProjectID: first, Hour: month, Events: 2, Bytes: 20})
	if err := m.RecordProjectStorage(ctx, first, month, data.ProjectStorage{Events: 1000, Bytes: 1 << 20}); err != nil {
		t.Fatalf("RecordProjectStorage: %v", err)
	}

	if n, err := db.Collection("usage").CountDocuments(ctx, bson.M{"project_id": first, "source": "broker-a", "organization_id": org.ID}); err != nil || n != 2 {
		t.Errorf("first's broker-a buckets naming the organization = %d, %v; want 2", n, err)
	}

	events := func(o *data.Organization) int64 {
		t.Helper()
		n, err := m.GetOrganizationEvents(ctx, o.ID, month, data.NextUsageMonth(month))
		if err != nil {
			t.Fatalf("GetOrganizationEvents: %v", err)
		}
		return n
	}
	if got := events(org); got != 10 {
		t.Errorf("the organization's month = %d events, want 10: 5 and 2 of the first project, 3 of the second", got)
	}
	if got := events(other); got != 100 {
		t.Errorf("the other organization's month = %d events, want 100", got)
	}

	q, err := m.ProjectQuota(ctx, second, time.Now())
	want := data.ProjectQuota{OrganizationID: org.ID.Hex(), Plan: "free", Month: month, Events: 10}
	if err != nil || q.OrganizationID != want.OrganizationID || q.Plan != want.Plan || !q.Month.Equal(want.Month) || q.Events != want.Events {
		t.Errorf("ProjectQuota = %+v, %v; want %+v", q, err, want)
	}

	// Deleting a project does not give its events back, and a flush that
	// comes after the project is gone still counts toward its organization.
	if _, err := db.Collection("projects").DeleteOne(ctx, bson.M{"_id": first}); err != nil {
		t.Fatalf("delete project: %v", err)
	}
	record("broker-a", data.UsageCount{ProjectID: first, Hour: month, Events: 6, Bytes: 60})
	if got := events(org); got != 11 {
		t.Errorf("after the project's deletion = %d events, want 11", got)
	}

	usage, err := m.GetOrganizationUsage(ctx, org.ID)
	if err != nil || usage.Events != 11 || usage.Projects != 1 {
		t.Errorf("GetOrganizationUsage = %+v, %v; want 11 events this month, of 1 project left", usage, err)
	}
}

// TestProjectQuota_Errors: ProjectQuota fails like ProjectPlan.
func TestProjectQuota_Errors(t *testing.T) {
	m, _ := setupOrganizationModels(t)
	setupUsageModels(t)
	ctx := context.Background()

	if q, err := m.ProjectQuota(ctx, primitive.NewObjectID(), time.Now()); !errors.Is(err, mongo.ErrNoDocuments) {
		t.Errorf("ProjectQuota of an unknown project = %+v, %v; want mongo.ErrNoDocuments", q, err)
	}
	for name, orgID := range map[string]primitive.ObjectID{"no organization": primitive.NilObjectID, "a missing organization": primitive.NewObjectID()} {
		p, err := m.InsertProject(data.Project{Name: "P", Slug: "p", OrganizationID: orgID})
		if err != nil {
			t.Fatalf("InsertProject: %v", err)
		}
		if q, err := m.ProjectQuota(ctx, p.ID, time.Now()); !errors.Is(err, data.ErrUnknownOrganization) {
			t.Errorf("ProjectQuota of a project in %s = %+v, %v; want ErrUnknownOrganization", name, q, err)
		}
	}
}

// TestProjectQuotaRPC: the logger answers a project's quota with its
// organization, plan and month, and the events recorded since.
func TestProjectQuotaRPC(t *testing.T) {
	stack := sharedStack(t)

	conn, err := rpc.Dial("tcp", stack.loggerRPCAddr)
	if err != nil {
		t.Fatalf("dial logger RPC: %v", err)
	}
	defer conn.Close()

	var project data.Project
	if err := conn.Call("RPCServer.CreateProject", data.RPCCreateProjectArgs{
		Name: "Quota RPC", Slug: "quota-rpc", OwnerID: time.Now().UnixNano(), Owner: "quota-rpc-owner",
	}, &project); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	quota := func() data.ProjectQuota {
		t.Helper()
		var q data.ProjectQuota
		if err := conn.Call("RPCServer.ProjectQuota", data.RPCProjectIDArgs{ID: project.ID.Hex()}, &q); err != nil {
			t.Fatalf("ProjectQuota: %v", err)
		}
		return q
	}
	before := quota()
	if before.OrganizationID != project.OrganizationID.Hex() || before.Plan != data.SelfHostedPlan || !before.Month.Equal(data.UsageMonth(time.Now())) {
		t.Errorf("ProjectQuota = %+v; want the Default organization %s, on %s, this month", before, project.OrganizationID.Hex(), data.SelfHostedPlan)
	}

	// The Default organization is shared with the other tests: count what
	// this one adds.
	var reply string
	if err := conn.Call("RPCServer.RecordUsage", data.RPCRecordUsageArgs{
		Source: "quota-rpc-test",
		Counts: []data.RPCUsageCount{{ProjectID: project.ID.Hex(), Hour: time.Now(), Events: 7, Bytes: 70}},
	}, &reply); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	if after := quota(); after.Events != before.Events+7 {
		t.Errorf("ProjectQuota after 7 more events = %d, want %d", after.Events, before.Events+7)
	}

	var q data.ProjectQuota
	if err := conn.Call("RPCServer.ProjectQuota", data.RPCProjectIDArgs{ID: "not-a-project"}, &q); err == nil {
		t.Error("ProjectQuota accepted a malformed project id")
	}
}

// TestMonthlyQuota_EndToEnd: a broker of the hosted edition refuses a key's
// events once its project's organization has used its plan's monthly events,
// with a 429 that says so, and queues nothing more.
func TestMonthlyQuota_EndToEnd(t *testing.T) {
	stack := sharedStack(t)
	db := testMongo(t, stack.mongoURI).Database("logs")
	ctx := context.Background()

	// An organization on the free plan, one event short of its quota.
	free, _ := limits.PlanByName(limits.PlanFree)
	orgID := primitive.NewObjectID()
	if _, err := db.Collection("organizations").InsertOne(ctx, bson.M{
		"_id": orgID, "name": "Quota end to end", "plan": limits.PlanFree, "billing_customer_id": "", "created_at": time.Now(),
	}); err != nil {
		t.Fatalf("insert organization: %v", err)
	}
	projectID := seedProject(t, stack.mongoURI, "monthly-quota")
	if _, err := db.Collection("projects").UpdateOne(ctx, bson.M{"_id": oid(t, projectID)}, bson.M{"$set": bson.M{"organization_id": orgID}}); err != nil {
		t.Fatalf("put the project in the organization: %v", err)
	}
	if _, err := db.Collection("usage").InsertOne(ctx, bson.M{
		"project_id": oid(t, projectID), "organization_id": orgID, "hour": data.UsageMonth(time.Now()),
		"source": "quota-seed", "events": free.MonthlyEvents - 1, "bytes": int64(0),
	}); err != nil {
		t.Fatalf("insert usage: %v", err)
	}
	const keyStem = "lw_monthlyquota"
	key := seedAPIKey(t, stack.mongoURI, projectID, keyStem+strings.Repeat("0", 46-len(keyStem)-1)+"1")

	addr := freeAddr(t)
	startProcess(t, "../broker/cmd/api", map[string]string{
		"RABBITMQ_URL":        stack.rabbitURI,
		"LOGGER_RPC_ADDR":     stack.loggerRPCAddr,
		"BROKER_PORT":         portOf(addr),
		"INTERNAL_API_SECRET": internalSecret,
		"LOGWOLF_EDITION":     limits.EditionCloud,
	})
	url := "http://" + addr
	if err := waitHTTP(url+"/ping", 60*time.Second); err != nil {
		t.Fatalf("broker: %v", err)
	}

	postLog(t, url, key, "the-quotas-last-event")

	body, _ := json.Marshal(map[string]any{"name": "past-the-quota", "severity": "info"})
	req, _ := http.NewRequest(http.MethodPost, url+"/logs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /logs: %v", err)
	}
	defer resp.Body.Close()

	var envelope struct {
		Error bool   `json:"error"`
		Code  string `json:"code"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.StatusCode != http.StatusTooManyRequests || !envelope.Error || envelope.Code != "quota_exceeded" {
		t.Errorf("past the quota: %d %+v, want 429 quota_exceeded", resp.StatusCode, envelope)
	}
	retry, err := strconv.ParseInt(resp.Header.Get("Retry-After"), 10, 64)
	if until := time.Until(data.NextUsageMonth(time.Now())); err != nil || retry < int64(until.Seconds())-60 || retry > int64(until.Seconds())+60 {
		t.Errorf("Retry-After = %q, want about %v, the rest of the month", resp.Header.Get("Retry-After"), until)
	}

	// The event within the quota is stored; the one past it never is.
	deadline := time.Now().Add(15 * time.Second)
	for {
		n, err := db.Collection("logs").CountDocuments(ctx, bson.M{"project_id": oid(t, projectID)})
		if err != nil {
			t.Fatalf("count logs: %v", err)
		}
		if n == 1 {
			break
		}
		if n > 1 || time.Now().After(deadline) {
			t.Fatalf("%d logs stored, want only the one within the quota", n)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
