package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"logwolf-toolbox/limits"
)

// ratePlan is a limits.Provider whose every project is on a plan with the given
// ingestion rate, or whose lookups fail with err. It counts the lookups.
type ratePlan struct {
	limits.SelfHosted
	rate, burst int
	err         error
	lookups     atomic.Int32
}

func (p *ratePlan) Plan(context.Context, string) (limits.Plan, error) {
	p.lookups.Add(1)
	if p.err != nil {
		return limits.Plan{}, p.err
	}
	return limits.Plan{Name: "rated", IngestRate: p.rate, IngestBurst: p.burst}, nil
}

// batchOf is a POST /logs/batch body of n info events.
func batchOf(n int) string {
	return "[" + strings.TrimSuffix(strings.Repeat(`{"name":"x","severity":"info"},`, n), ",") + "]"
}

func TestIngestBucket_TakesAndRefills(t *testing.T) {
	now := time.Now()
	b := newIngestBucket(10, 20, now)

	if ok, _ := b.take(20, now); !ok {
		t.Fatal("a full bucket refused its burst")
	}
	ok, wait := b.take(5, now)
	if ok {
		t.Fatal("an empty bucket took 5 events")
	}
	if wait != 500*time.Millisecond {
		t.Errorf("wait = %v, want 500ms: 5 events at 10 a second", wait)
	}

	// Half a second later the 5 have been earned, and no more.
	now = now.Add(500 * time.Millisecond)
	if ok, _ := b.take(5, now); !ok {
		t.Error("refused 5 events after earning them")
	}
	if ok, _ := b.take(1, now); ok {
		t.Error("took an event it had not earned")
	}

	// A long quiet spell fills the bucket, never past the burst.
	now = now.Add(time.Hour)
	if ok, _ := b.take(20, now); !ok {
		t.Error("refused the burst after an hour")
	}
	if ok, _ := b.take(1, now); ok {
		t.Error("the bucket held more than its burst")
	}
}

// A refused request spends nothing, so the client that waits as long as it was
// told gets in.
func TestIngestBucket_RefusalSpendsNothing(t *testing.T) {
	now := time.Now()
	b := newIngestBucket(1, 3, now)
	b.take(2, now)

	for range 5 {
		if ok, _ := b.take(2, now); ok {
			t.Fatal("took 2 events out of 1")
		}
	}
	_, wait := b.take(2, now)
	if ok, _ := b.take(2, now.Add(wait)); !ok {
		t.Errorf("still refused after the %v it was told to wait", wait)
	}
}

// More events than the bucket holds need a full one, rather than being refused
// whatever the client does.
func TestIngestBucket_OversizedRequestNeedsAFullBucket(t *testing.T) {
	now := time.Now()
	b := newIngestBucket(1, 3, now)
	b.take(1, now)

	ok, wait := b.take(10, now)
	if ok || wait != time.Second {
		t.Errorf("10 events from a bucket of 2 of 3: %v, wait %v; want refused, 1s to fill", ok, wait)
	}
	if ok, _ := b.take(10, now.Add(wait)); !ok {
		t.Error("10 events refused by a full bucket of 3")
	}
	if ok, _ := b.take(1, now.Add(wait)); ok {
		t.Error("the oversized request left tokens behind")
	}
}

func TestIngestBucket_Unlimited(t *testing.T) {
	now := time.Now()
	b := newIngestBucket(limits.Unlimited, limits.Unlimited, now)
	for range 3 {
		if ok, _ := b.take(1_000_000, now); !ok {
			t.Fatal("a plan without a rate refused events")
		}
	}
	if !b.full(now) {
		t.Error("an unlimited bucket is not full: the sweep would keep it forever")
	}
}

// A new size keeps what the bucket has earned, up to the new burst.
func TestIngestBucket_Resize(t *testing.T) {
	now := time.Now()
	b := newIngestBucket(10, 100, now)
	b.take(40, now)

	b.resize(10, 50, now)
	if b.tokens != 50 {
		t.Errorf("tokens = %v after shrinking the burst to 50, want 50", b.tokens)
	}
	b.resize(1000, 5000, now)
	if b.tokens != 50 {
		t.Errorf("tokens = %v after growing the burst, want the 50 it had", b.tokens)
	}
	if !b.sizedAt.Equal(now) {
		t.Errorf("sizedAt = %v, want %v", b.sizedAt, now)
	}
}

func TestSetRetryAfter_RoundsUpToWholeSeconds(t *testing.T) {
	for wait, want := range map[time.Duration]string{
		0:                       "1",
		time.Millisecond:        "1",
		time.Second:             "1",
		1001 * time.Millisecond: "2",
		59 * time.Second:        "59",
	} {
		w := httptest.NewRecorder()
		setRetryAfter(w, wait)
		if got := w.Header().Get("Retry-After"); got != want {
			t.Errorf("wait %v: Retry-After = %q, want %q", wait, got, want)
		}
	}
}

// The issue's acceptance criterion: once a key has sent its burst, its next
// events are refused with 429 and a Retry-After, and none of them is queued.
func TestIngest_OverTheKeysRateIs429(t *testing.T) {
	resetAuthCaches(t)
	pub := &fakePublisher{}
	lim := &ratePlan{rate: 1, burst: 3}
	h := (&Config{Events: pub, Limits: lim}).routes()
	key := seedKey(t, projAlpha, "ingest")

	if w := do(h, keyRequest(http.MethodPost, "/logs/batch", key, batchOf(3))); w.Code != http.StatusAccepted {
		t.Fatalf("the burst: got %d, want 202 (body: %s)", w.Code, w.Body.String())
	}
	for _, r := range []*http.Request{
		keyRequest(http.MethodPost, "/logs", key, `{"name":"x","severity":"info"}`),
		keyRequest(http.MethodPost, "/logs/batch", key, batchOf(2)),
	} {
		w := do(h, r)
		if w.Code != http.StatusTooManyRequests {
			t.Fatalf("%s past the burst: got %d, want 429 (body: %s)", r.URL.Path, w.Code, w.Body.String())
		}
		retry, err := strconv.Atoi(w.Header().Get("Retry-After"))
		if err != nil || retry < 1 || retry > 2 {
			t.Errorf("%s: Retry-After = %q, want the 1 or 2 seconds the events take to earn", r.URL.Path, w.Header().Get("Retry-After"))
		}
	}

	pub.mu.Lock()
	defer pub.mu.Unlock()
	if len(pub.calls) != 1 || len(pub.calls[0]) != 3 {
		t.Errorf("published %d batches, want only the first, of 3 events", len(pub.calls))
	}
	if n := lim.lookups.Load(); n != 1 {
		t.Errorf("the plan was looked up %d times, want once for the key", n)
	}
}

// Each key has its own bucket: one key's flood leaves another's, even in the
// same project, untouched.
func TestIngest_RateIsPerKey(t *testing.T) {
	resetAuthCaches(t)
	h := (&Config{Events: &fakePublisher{}, Limits: &ratePlan{rate: 1, burst: 1}}).routes()
	flood, quiet := seedKey(t, projAlpha, "ingest"), seedKey(t, projAlpha, "ingest")

	do(h, keyRequest(http.MethodPost, "/logs/batch", flood, batchOf(1)))
	if w := do(h, keyRequest(http.MethodPost, "/logs/batch", flood, batchOf(1))); w.Code != http.StatusTooManyRequests {
		t.Fatalf("flooding key: got %d, want 429", w.Code)
	}
	if w := do(h, keyRequest(http.MethodPost, "/logs/batch", quiet, batchOf(1))); w.Code != http.StatusAccepted {
		t.Errorf("another key of the project: got %d, want 202 (body: %s)", w.Code, w.Body.String())
	}
}

// Self-hosted, the default, limits nothing.
func TestIngest_SelfHostedIsUnlimited(t *testing.T) {
	resetAuthCaches(t)
	h := (&Config{Events: &fakePublisher{}}).routes()
	key := seedKey(t, projAlpha, "ingest")

	for i := range 50 {
		if w := do(h, keyRequest(http.MethodPost, "/logs/batch", key, batchOf(maxBatchSize))); w.Code != http.StatusAccepted {
			t.Fatalf("batch %d: got %d, want 202 (body: %s)", i, w.Code, w.Body.String())
		}
	}
}

// A key whose plan cannot be told is not let through unlimited: 500, the
// answer for a logger that cannot be reached, and nothing queued.
func TestIngest_UnknownPlanIs500(t *testing.T) {
	resetAuthCaches(t)
	pub := &fakePublisher{}
	h := (&Config{Events: pub, Limits: &ratePlan{err: errors.New("logger down")}}).routes()
	key := seedKey(t, projAlpha, "ingest")

	if w := do(h, keyRequest(http.MethodPost, "/logs", key, `{"name":"x","severity":"info"}`)); w.Code != http.StatusInternalServerError {
		t.Errorf("got %d, want 500 (body: %s)", w.Code, w.Body.String())
	}
	if len(pub.calls) != 0 {
		t.Errorf("published %d batches without knowing the plan", len(pub.calls))
	}
}

// Once a key has a bucket, a failed lookup keeps its size, and the plan is not
// asked again on every request.
func TestAllowIngest_FailedLookupKeepsTheOldSize(t *testing.T) {
	resetAuthCaches(t)
	lim := &ratePlan{rate: 1, burst: 1}
	app := &Config{Limits: lim}
	ctx := context.Background()

	if ok, _, err := app.allowIngest(ctx, "k", projAlpha, 1); !ok || err != nil {
		t.Fatalf("first event: %v, %v", ok, err)
	}
	ingestBucketsMu.Lock()
	ingestBuckets["k"].sizedAt = time.Now().Add(-ingestPlanTTL)
	ingestBucketsMu.Unlock()

	lim.err = errors.New("logger down")
	for range 3 {
		ok, wait, err := app.allowIngest(ctx, "k", projAlpha, 1)
		if ok || wait <= 0 || err != nil {
			t.Fatalf("over the old rate with the plan unknown: %v, %v, %v; want refused under the old size", ok, wait, err)
		}
	}
	if n := lim.lookups.Load(); n != 2 {
		t.Errorf("%d lookups, want 2: the first, and one retry for the stale size", n)
	}
}

// A plan change reaches a key within ingestPlanTTL.
func TestAllowIngest_ResizesFromThePlan(t *testing.T) {
	resetAuthCaches(t)
	lim := &ratePlan{rate: 1, burst: 1}
	app := &Config{Limits: lim}
	ctx := context.Background()

	app.allowIngest(ctx, "k", projAlpha, 1)
	lim.rate, lim.burst = 100, 100
	if ok, _, _ := app.allowIngest(ctx, "k", projAlpha, 1); ok {
		t.Fatal("the new plan applied before the old size went stale")
	}

	// Once the size is stale the next request takes the new one. It keeps the
	// tokens it had, none, so it is refused, but told to wait a 100th of a second.
	ingestBucketsMu.Lock()
	ingestBuckets["k"].sizedAt = time.Now().Add(-ingestPlanTTL)
	ingestBucketsMu.Unlock()
	_, wait, _ := app.allowIngest(ctx, "k", projAlpha, 1)
	if wait > 10*time.Millisecond {
		t.Errorf("told to wait %v after ingestPlanTTL, want at most the new rate's 10ms", wait)
	}
	ingestBucketsMu.Lock()
	defer ingestBucketsMu.Unlock()
	if b := ingestBuckets["k"]; b.rate != 100 || b.burst != 100 {
		t.Errorf("bucket sized %v/s, burst %v; want the new plan's 100, 100", b.rate, b.burst)
	}
}

// The dashboard's own events have no key, and no rate.
func TestIngest_DashboardEventsAreNotLimited(t *testing.T) {
	newInternalTestServer(t)
	resetAuthCaches(t)
	h := (&Config{Events: &fakePublisher{}, Limits: &ratePlan{rate: 1, burst: 1}}).routes()

	for i := range 3 {
		w := do(h, internalRequest(http.MethodPost, "/projects/"+projAlpha+"/logs", "member-a", map[string]any{"name": "x", "severity": "info"}))
		if w.Code != http.StatusAccepted {
			t.Fatalf("event %d: got %d, want 202 (body: %s)", i, w.Code, w.Body.String())
		}
	}
}

// Every hosted plan lets a key send a full batch at once: a bucket smaller than
// maxBatchSize would make the largest batches wait for a full bucket each.
func TestPlans_BurstHoldsAFullBatch(t *testing.T) {
	for _, p := range limits.Plans() {
		if p.IngestBurst != limits.Unlimited && p.IngestBurst < maxBatchSize {
			t.Errorf("%s: IngestBurst = %d, want at least maxBatchSize = %d", p.Name, p.IngestBurst, maxBatchSize)
		}
	}
}

// On the hosted edition the rate is the organization's plan's, which the
// broker asks the logger for.
func TestIngest_CloudRateIsTheOrganizationsPlan(t *testing.T) {
	_, fake := newInternalTestServer(t)
	resetAuthCaches(t)
	fake.setPlan(projAlpha, limits.PlanFree)
	free, _ := limits.PlanByName(limits.PlanFree)
	h := (&Config{Events: &fakePublisher{}, Limits: limits.Organizations{PlanOf: projectPlan, QuotaOf: projectQuota}}).routes()
	key := seedKey(t, projAlpha, "ingest")

	for sent := 0; sent < free.IngestBurst; sent += maxBatchSize {
		if w := do(h, keyRequest(http.MethodPost, "/logs/batch", key, batchOf(min(maxBatchSize, free.IngestBurst-sent)))); w.Code != http.StatusAccepted {
			t.Fatalf("within the free plan's burst: got %d, want 202 (body: %s)", w.Code, w.Body.String())
		}
	}
	// A refill may have earned a few events while the burst went in; ask for
	// more than that.
	if w := do(h, keyRequest(http.MethodPost, "/logs/batch", key, batchOf(free.IngestRate))); w.Code != http.StatusTooManyRequests {
		t.Errorf("past the free plan's burst: got %d, want 429 (body: %s)", w.Code, w.Body.String())
	}
}

func TestSweep_ForgetsOnlyIdleFullBuckets(t *testing.T) {
	resetAuthCaches(t)
	now := time.Now()
	stale := now.Add(-ingestPlanTTL)

	ingestBucketsMu.Lock()
	ingestBuckets["idle"] = &ingestBucket{rate: 1, burst: 10, tokens: 10, last: stale, sizedAt: stale}
	ingestBuckets["refilled"] = &ingestBucket{rate: 1, burst: 10, tokens: 0, last: now.Add(-11 * time.Second), sizedAt: stale}
	ingestBuckets["draining"] = &ingestBucket{rate: 1, burst: 10, tokens: 0, last: now.Add(-time.Second), sizedAt: stale}
	ingestBuckets["fresh"] = &ingestBucket{rate: 1, burst: 10, tokens: 10, last: now, sizedAt: now}
	ingestBucketsMu.Unlock()

	sweepAuthCaches(now)

	ingestBucketsMu.Lock()
	defer ingestBucketsMu.Unlock()
	for k, want := range map[string]bool{"idle": false, "refilled": false, "draining": true, "fresh": true} {
		if _, kept := ingestBuckets[k]; kept != want {
			t.Errorf("bucket %q kept = %v, want %v", k, kept, want)
		}
	}
}

func TestAllowIngest_CapsTheBuckets(t *testing.T) {
	resetAuthCaches(t)
	old := maxIngestBuckets
	maxIngestBuckets = 3
	t.Cleanup(func() { maxIngestBuckets = old })
	app := &Config{Limits: &ratePlan{rate: 1, burst: 1}}

	for _, k := range []string{"a", "b", "c", "d", "e"} {
		app.allowIngest(context.Background(), k, projAlpha, 1)
	}
	ingestBucketsMu.Lock()
	defer ingestBucketsMu.Unlock()
	if len(ingestBuckets) != 3 {
		t.Errorf("%d buckets, want the cap of 3", len(ingestBuckets))
	}
	if _, ok := ingestBuckets["e"]; !ok {
		t.Error("the newest key has no bucket")
	}
}
