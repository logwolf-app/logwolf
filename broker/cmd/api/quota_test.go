package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"logwolf-toolbox/data"
	"logwolf-toolbox/limits"
)

// quotaPlan is a limits.Provider whose projects are each in an organization
// with a monthly quota of limit events, used of them as the logger counts it,
// or whose quota lookups fail with err. It counts the lookups. Projects are in
// the organization org names for them, their own by default.
type quotaPlan struct {
	limits.SelfHosted

	mu      sync.Mutex
	limit   int64
	used    map[string]int64 // organization -> events the logger has counted
	org     map[string]string
	month   time.Time // zero: this month
	err     error
	lookups int
}

func newQuotaPlan(limit int64) *quotaPlan {
	return &quotaPlan{limit: limit, used: map[string]int64{}, org: map[string]string{}}
}

func (p *quotaPlan) orgOf(projectID string) string {
	if org, ok := p.org[projectID]; ok {
		return org
	}
	return "org-of-" + projectID
}

func (p *quotaPlan) MonthlyQuota(_ context.Context, projectID string) (limits.Quota, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lookups++
	if p.err != nil {
		return limits.Quota{}, p.err
	}
	month := p.month
	if month.IsZero() {
		month = data.UsageMonth(time.Now())
	}
	org := p.orgOf(projectID)
	return limits.Quota{OrganizationID: org, Limit: p.limit, Month: month, Used: p.used[org]}, nil
}

func (p *quotaPlan) setUsed(projectID string, n int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.used[p.orgOf(projectID)] = n
}

func (p *quotaPlan) lookupCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lookups
}

// staleQuota makes the broker's quota of every project due for a lookup.
func staleQuota() {
	quotaMu.Lock()
	defer quotaMu.Unlock()
	for _, p := range quotaProjects {
		p.checkedAt = p.checkedAt.Add(-quotaTTL)
	}
}

// errorCode is the code of an error response's body.
func errorCode(t *testing.T, body []byte) string {
	t.Helper()
	var envelope struct {
		Error bool   `json:"error"`
		Code  string `json:"code"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || !envelope.Error {
		t.Fatalf("body %s (%v): want an error envelope", body, err)
	}
	return envelope.Code
}

// The issue's acceptance criteria: past its organization's quota a project's
// events are refused with 429 and the code quota_exceeded, queued nowhere,
// counted nowhere, and told to come back when the month is over.
func TestIngest_OverTheMonthlyQuotaIs429(t *testing.T) {
	resetAuthCaches(t)
	pub := &fakePublisher{}
	m := newUsageMeter("broker-test")
	lim := newQuotaPlan(10)
	lim.setUsed(projAlpha, 7)
	h := (&Config{Events: pub, Usage: m, Limits: lim}).routes()
	key := seedKey(t, projAlpha, "ingest")

	if w := do(h, keyRequest(http.MethodPost, "/logs/batch", key, batchOf(3))); w.Code != http.StatusAccepted {
		t.Fatalf("the last 3 events of the quota: got %d, want 202 (body: %s)", w.Code, w.Body.String())
	}
	for _, r := range []*http.Request{
		keyRequest(http.MethodPost, "/logs", key, `{"name":"x","severity":"info"}`),
		keyRequest(http.MethodPost, "/logs/batch", key, batchOf(2)),
	} {
		w := do(h, r)
		if w.Code != http.StatusTooManyRequests {
			t.Fatalf("%s past the quota: got %d, want 429 (body: %s)", r.URL.Path, w.Code, w.Body.String())
		}
		if code := errorCode(t, w.Body.Bytes()); code != codeQuotaExceeded {
			t.Errorf("%s: code %q, want %q", r.URL.Path, code, codeQuotaExceeded)
		}
		retry, err := strconv.ParseInt(w.Header().Get("Retry-After"), 10, 64)
		untilNextMonth := time.Until(data.NextUsageMonth(time.Now()))
		if err != nil || retry < int64(untilNextMonth.Seconds())-5 || retry > int64(untilNextMonth.Seconds())+5 {
			t.Errorf("%s: Retry-After = %q, want about %v, until the month is over", r.URL.Path, w.Header().Get("Retry-After"), untilNextMonth)
		}
	}

	pub.mu.Lock()
	if len(pub.calls) != 1 || len(pub.calls[0]) != 3 {
		t.Errorf("published %d batches, want only the first, of 3 events", len(pub.calls))
	}
	pub.mu.Unlock()
	m.mu.Lock()
	for k, c := range m.counts {
		if c.events != 3 {
			t.Errorf("metered %d events for %v, want the 3 accepted", c.events, k)
		}
	}
	m.mu.Unlock()
	if n := lim.lookupCount(); n != 1 {
		t.Errorf("the quota was looked up %d times, want once within quotaTTL", n)
	}
}

// A batch that would go past the quota is refused whole, though some of it
// would fit: the rest of the quota is left for requests that fit it.
func TestIngest_ABatchPastTheQuotaIsRefusedWhole(t *testing.T) {
	resetAuthCaches(t)
	pub := &fakePublisher{}
	lim := newQuotaPlan(10)
	lim.setUsed(projAlpha, 8)
	h := (&Config{Events: pub, Limits: lim}).routes()
	key := seedKey(t, projAlpha, "ingest")

	if w := do(h, keyRequest(http.MethodPost, "/logs/batch", key, batchOf(3))); w.Code != http.StatusTooManyRequests {
		t.Fatalf("3 events with 2 left: got %d, want 429 (body: %s)", w.Code, w.Body.String())
	}
	if w := do(h, keyRequest(http.MethodPost, "/logs/batch", key, batchOf(2))); w.Code != http.StatusAccepted {
		t.Errorf("2 events with 2 left: got %d, want 202 (body: %s)", w.Code, w.Body.String())
	}
}

// The SDK tells the two 429s apart by their codes: a key past its rate is
// rate_limited, and so is an address past its failed authentications.
func TestIngest_RateAndQuotaRefusalsHaveDistinctCodes(t *testing.T) {
	resetAuthCaches(t)
	lim := &ratePlan{rate: 1, burst: 1}
	h := (&Config{Events: &fakePublisher{}, Limits: lim}).routes()
	key := seedKey(t, projAlpha, "ingest")

	do(h, keyRequest(http.MethodPost, "/logs", key, `{"name":"x","severity":"info"}`))
	w := do(h, keyRequest(http.MethodPost, "/logs", key, `{"name":"x","severity":"info"}`))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("past the rate: got %d, want 429 (body: %s)", w.Code, w.Body.String())
	}
	if code := errorCode(t, w.Body.Bytes()); code != codeRateLimited {
		t.Errorf("past the rate: code %q, want %q", code, codeRateLimited)
	}

	rateLimited(t, nil)
	w = do(h, keyRequest(http.MethodPost, "/logs", key, `{"name":"x","severity":"info"}`))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("too many failed authentications: got %d, want 429 (body: %s)", w.Code, w.Body.String())
	}
	if code := errorCode(t, w.Body.Bytes()); code != codeRateLimited {
		t.Errorf("too many failed authentications: code %q, want %q", code, codeRateLimited)
	}
}

// A request the quota refuses spends none of the key's rate, and one the rate
// refuses counts nothing against the quota.
func TestIngest_QuotaAndRateDoNotSpendEachOther(t *testing.T) {
	resetAuthCaches(t)
	h := (&Config{Events: &fakePublisher{}, Limits: rateAndQuota{&ratePlan{rate: 1, burst: 2}, newQuotaPlan(2)}}).routes()
	key := seedKey(t, projAlpha, "ingest")

	w := do(h, keyRequest(http.MethodPost, "/logs/batch", key, batchOf(3)))
	if w.Code != http.StatusTooManyRequests || errorCode(t, w.Body.Bytes()) != codeQuotaExceeded {
		t.Fatalf("3 events past a quota of 2: got %d (body: %s), want 429 quota_exceeded", w.Code, w.Body.String())
	}
	if w := do(h, keyRequest(http.MethodPost, "/logs/batch", key, batchOf(2))); w.Code != http.StatusAccepted {
		t.Fatalf("2 events after the quota's refusal: got %d, want 202, the bucket untouched (body: %s)", w.Code, w.Body.String())
	}

	// With room in the quota again, the key's empty bucket refuses the next
	// events, and the quota gets them back.
	quotaMu.Lock()
	quotaCounters["org-of-"+projAlpha].limit = 10
	quotaMu.Unlock()
	w = do(h, keyRequest(http.MethodPost, "/logs/batch", key, batchOf(2)))
	if w.Code != http.StatusTooManyRequests || errorCode(t, w.Body.Bytes()) != codeRateLimited {
		t.Fatalf("2 events past the rate: got %d (body: %s), want 429 rate_limited", w.Code, w.Body.String())
	}
	quotaMu.Lock()
	used := quotaCounters["org-of-"+projAlpha].used
	quotaMu.Unlock()
	if used != 2 {
		t.Errorf("the quota counts %d events, want the 2 accepted", used)
	}
}

// rateAndQuota is a provider with ratePlan's rates and quotaPlan's quotas.
type rateAndQuota struct {
	*ratePlan
	quota *quotaPlan
}

func (p rateAndQuota) MonthlyQuota(ctx context.Context, projectID string) (limits.Quota, error) {
	return p.quota.MonthlyQuota(ctx, projectID)
}

// The quota is the organization's: its projects' events add up on one counter.
func TestIngest_ProjectsOfAnOrganizationShareItsQuota(t *testing.T) {
	resetAuthCaches(t)
	lim := newQuotaPlan(5)
	lim.org[projAlpha], lim.org[projBeta] = "org-1", "org-1"
	h := (&Config{Events: &fakePublisher{}, Limits: lim}).routes()
	alpha, beta := seedKey(t, projAlpha, "ingest"), seedKey(t, projBeta, "ingest")

	if w := do(h, keyRequest(http.MethodPost, "/logs/batch", alpha, batchOf(3))); w.Code != http.StatusAccepted {
		t.Fatalf("alpha's 3: got %d, want 202 (body: %s)", w.Code, w.Body.String())
	}
	if w := do(h, keyRequest(http.MethodPost, "/logs/batch", beta, batchOf(3))); w.Code != http.StatusTooManyRequests {
		t.Errorf("beta's 3 after alpha's 3 of 5: got %d, want 429 (body: %s)", w.Code, w.Body.String())
	}
	if w := do(h, keyRequest(http.MethodPost, "/logs/batch", beta, batchOf(2))); w.Code != http.StatusAccepted {
		t.Errorf("beta's 2: got %d, want 202 (body: %s)", w.Code, w.Body.String())
	}
}

// The dashboard's events count toward the quota, so they are held to it too.
func TestIngest_DashboardEventsAreHeldToTheQuota(t *testing.T) {
	newInternalTestServer(t)
	resetAuthCaches(t)
	pub := &fakePublisher{}
	lim := newQuotaPlan(1)
	h := (&Config{Events: pub, Limits: lim}).routes()

	event := map[string]any{"name": "x", "severity": "info"}
	if w := do(h, internalRequest(http.MethodPost, "/projects/"+projAlpha+"/logs", "member-a", event)); w.Code != http.StatusAccepted {
		t.Fatalf("within the quota: got %d, want 202 (body: %s)", w.Code, w.Body.String())
	}
	w := do(h, internalRequest(http.MethodPost, "/projects/"+projAlpha+"/logs", "member-a", event))
	if w.Code != http.StatusTooManyRequests || errorCode(t, w.Body.Bytes()) != codeQuotaExceeded {
		t.Errorf("past the quota: got %d (body: %s), want 429 quota_exceeded", w.Code, w.Body.String())
	}
	if len(pub.calls) != 1 {
		t.Errorf("published %d events, want 1", len(pub.calls))
	}
}

// Self-hosted, the default, has no quota and keeps no counter.
func TestIngest_SelfHostedHasNoQuota(t *testing.T) {
	resetAuthCaches(t)
	h := (&Config{Events: &fakePublisher{}}).routes()
	key := seedKey(t, projAlpha, "ingest")

	for i := range 20 {
		if w := do(h, keyRequest(http.MethodPost, "/logs/batch", key, batchOf(maxBatchSize))); w.Code != http.StatusAccepted {
			t.Fatalf("batch %d: got %d, want 202 (body: %s)", i, w.Code, w.Body.String())
		}
	}
	quotaMu.Lock()
	defer quotaMu.Unlock()
	if len(quotaCounters) != 0 {
		t.Errorf("%d quota counters kept self-hosted, want none", len(quotaCounters))
	}
}

// A plan without a monthly limit lets everything through.
func TestIngest_UnlimitedPlanHasNoQuota(t *testing.T) {
	resetAuthCaches(t)
	lim := newQuotaPlan(limits.Unlimited)
	lim.setUsed(projAlpha, 1<<40)
	h := (&Config{Events: &fakePublisher{}, Limits: lim}).routes()
	key := seedKey(t, projAlpha, "ingest")

	if w := do(h, keyRequest(http.MethodPost, "/logs/batch", key, batchOf(maxBatchSize))); w.Code != http.StatusAccepted {
		t.Errorf("got %d, want 202 (body: %s)", w.Code, w.Body.String())
	}
}

// A project whose quota cannot be told is not let through unlimited: 500, and
// nothing queued.
func TestIngest_UnknownQuotaIs500(t *testing.T) {
	resetAuthCaches(t)
	pub := &fakePublisher{}
	lim := newQuotaPlan(10)
	lim.err = errors.New("logger down")
	h := (&Config{Events: pub, Limits: lim}).routes()
	key := seedKey(t, projAlpha, "ingest")

	if w := do(h, keyRequest(http.MethodPost, "/logs", key, `{"name":"x","severity":"info"}`)); w.Code != http.StatusInternalServerError {
		t.Errorf("got %d, want 500 (body: %s)", w.Code, w.Body.String())
	}
	if len(pub.calls) != 0 {
		t.Errorf("published %d batches without knowing the quota", len(pub.calls))
	}
}

// Once a project has a quota, a failed lookup keeps it, and the provider is
// not asked again on every request.
func TestReserveQuota_FailedLookupKeepsTheCounter(t *testing.T) {
	resetAuthCaches(t)
	lim := newQuotaPlan(2)
	app := &Config{Limits: lim}
	ctx, now := context.Background(), time.Now()

	if _, ok, _, err := app.reserveQuota(ctx, projAlpha, 2, now); !ok || err != nil {
		t.Fatalf("the quota's 2 events: %v, %v", ok, err)
	}
	staleQuota()
	lim.err = errors.New("logger down")
	for range 3 {
		if _, ok, wait, err := app.reserveQuota(ctx, projAlpha, 1, now); ok || wait <= 0 || err != nil {
			t.Fatalf("past the quota with the lookup failing: %v, %v, %v; want refused under the old counter", ok, wait, err)
		}
	}
	if n := lim.lookupCount(); n != 2 {
		t.Errorf("%d lookups, want 2: the first, and one retry for the stale quota", n)
	}
}

// A lookup brings in what other brokers flushed, but never lowers what this
// one counted: each falls short of the truth on its own.
func TestReserveQuota_LookupsKeepTheLargerCount(t *testing.T) {
	resetAuthCaches(t)
	lim := newQuotaPlan(10)
	app := &Config{Limits: lim}
	ctx, now := context.Background(), time.Now()

	app.reserveQuota(ctx, projAlpha, 4, now) // the logger has none of these yet
	staleQuota()
	if _, ok, _, _ := app.reserveQuota(ctx, projAlpha, 6, now); !ok {
		t.Fatal("6 more of 10 after 4, with the logger at 0: refused, want them to fit")
	}

	// Another broker's 3 events reach the logger; this broker's 10 have not.
	lim.setUsed(projAlpha, 3)
	staleQuota()
	if _, ok, _, _ := app.reserveQuota(ctx, projAlpha, 1, now); ok {
		t.Error("an 11th event of 10 accepted after a lookup that counted 3")
	}

	// They all have, and so have 2 more: 15 in all.
	lim.setUsed(projAlpha, 15)
	staleQuota()
	app.reserveQuota(ctx, projAlpha, 0, now)
	quotaMu.Lock()
	defer quotaMu.Unlock()
	if c := quotaCounters["org-of-"+projAlpha]; c.used != 15 {
		t.Errorf("counter at %d, want the logger's 15", c.used)
	}
}

// A plan change reaches the counter at the next lookup.
func TestReserveQuota_TakesTheNewLimit(t *testing.T) {
	resetAuthCaches(t)
	lim := newQuotaPlan(1)
	app := &Config{Limits: lim}
	ctx, now := context.Background(), time.Now()

	app.reserveQuota(ctx, projAlpha, 1, now)
	lim.limit = 100
	if _, ok, _, _ := app.reserveQuota(ctx, projAlpha, 1, now); ok {
		t.Fatal("the new limit applied before the quota went stale")
	}
	staleQuota()
	if _, ok, _, _ := app.reserveQuota(ctx, projAlpha, 1, now); !ok {
		t.Error("refused after quotaTTL, want the new limit of 100")
	}
}

// When the month turns the quota renews, without waiting for a lookup, and a
// late lookup about the month before changes nothing.
func TestReserveQuota_RenewsWithTheMonth(t *testing.T) {
	resetAuthCaches(t)
	october := time.Date(2026, time.October, 1, 0, 0, 0, 0, time.UTC)
	lim := newQuotaPlan(5)
	lim.month = october
	lim.setUsed(projAlpha, 5)
	app := &Config{Limits: lim}
	ctx := context.Background()

	lastSecond := time.Date(2026, time.October, 31, 23, 59, 59, 0, time.UTC)
	_, ok, wait, _ := app.reserveQuota(ctx, projAlpha, 1, lastSecond)
	if ok || wait != time.Second {
		t.Fatalf("the last second of a used-up October: %v, wait %v; want refused for 1s", ok, wait)
	}

	firstSecond := lastSecond.Add(time.Second)
	if _, ok, _, _ := app.reserveQuota(ctx, projAlpha, 5, firstSecond); !ok {
		t.Fatal("November's first 5 events refused, want a renewed quota")
	}

	// The logger still answers for October, as its clock or a slow flush may.
	staleQuota()
	if _, ok, _, _ := app.reserveQuota(ctx, projAlpha, 1, firstSecond); ok {
		t.Error("a 6th event of November's 5 accepted after a lookup about October")
	}
	quotaMu.Lock()
	defer quotaMu.Unlock()
	if c := quotaCounters["org-of-"+projAlpha]; !c.month.Equal(october.AddDate(0, 1, 0)) || c.used != 5 {
		t.Errorf("counter at %d for %v, want 5 for November", c.used, c.month)
	}
}

// Events that were reserved but never queued are given back.
func TestIngest_UnqueuedEventsAreGivenBack(t *testing.T) {
	resetAuthCaches(t)
	pub := &fakePublisher{err: errors.New("not confirmed")}
	lim := newQuotaPlan(3)
	h := (&Config{Events: pub, Limits: lim}).routes()
	key := seedKey(t, projAlpha, "ingest")

	for range 3 {
		if w := do(h, keyRequest(http.MethodPost, "/logs/batch", key, batchOf(3))); w.Code != http.StatusServiceUnavailable {
			t.Fatalf("got %d, want 503 (body: %s)", w.Code, w.Body.String())
		}
	}
	pub.mu.Lock()
	pub.err = nil
	pub.mu.Unlock()
	if w := do(h, keyRequest(http.MethodPost, "/logs/batch", key, batchOf(3))); w.Code != http.StatusAccepted {
		t.Errorf("the quota's 3 after 3 failed publishes: got %d, want 202 (body: %s)", w.Code, w.Body.String())
	}
}

// On the hosted edition the quota is the organization's plan's, which the
// broker asks the logger for along with what the organization has used.
func TestIngest_CloudQuotaIsTheOrganizationsPlan(t *testing.T) {
	_, fake := newInternalTestServer(t)
	resetAuthCaches(t)
	fake.setPlan(projAlpha, limits.PlanFree)
	free, _ := limits.PlanByName(limits.PlanFree)
	fake.setMonthUsed(projAlpha, free.MonthlyEvents-1)
	h := (&Config{Events: &fakePublisher{}, Limits: limits.Organizations{PlanOf: projectPlan, QuotaOf: projectQuota}}).routes()
	key := seedKey(t, projAlpha, "ingest")

	if w := do(h, keyRequest(http.MethodPost, "/logs", key, `{"name":"x","severity":"info"}`)); w.Code != http.StatusAccepted {
		t.Fatalf("the free plan's last event: got %d, want 202 (body: %s)", w.Code, w.Body.String())
	}
	w := do(h, keyRequest(http.MethodPost, "/logs", key, `{"name":"x","severity":"info"}`))
	if w.Code != http.StatusTooManyRequests || errorCode(t, w.Body.Bytes()) != codeQuotaExceeded {
		t.Errorf("past the free plan's quota: got %d (body: %s), want 429 quota_exceeded", w.Code, w.Body.String())
	}
	fake.snapshot(func(f *fakeLogger) {
		if f.quotaLookups != 1 {
			t.Errorf("the logger was asked %d times, want once", f.quotaLookups)
		}
	})
}

func TestProjectQuota(t *testing.T) {
	_, fake := newInternalTestServer(t)
	fake.setPlan(projAlpha, limits.PlanPro)
	fake.setMonthUsed(projAlpha, 42)

	q, err := projectQuota(context.Background(), projAlpha)
	if err != nil || q.Plan != limits.PlanPro || q.Events != 42 || q.OrganizationID == "" || !q.Month.Equal(data.UsageMonth(time.Now())) {
		t.Errorf("projectQuota(alpha) = %+v, %v; want the pro plan, 42 events this month", q, err)
	}
	if q, err := projectQuota(context.Background(), projBeta); err == nil {
		t.Errorf("projectQuota of a project in no organization = %+v, want an error", q)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := projectQuota(ctx, projAlpha); !errors.Is(err, context.Canceled) {
		t.Errorf("projectQuota with a done context: %v, want context.Canceled", err)
	}
}

// The sweep forgets projects no request has come for in quotaIdle, and the
// counters no project left points to.
func TestSweep_ForgetsIdleQuotas(t *testing.T) {
	resetAuthCaches(t)
	lim := newQuotaPlan(10)
	lim.org[projAlpha], lim.org[projBeta] = "org-1", "org-2"
	app := &Config{Limits: lim}
	now := time.Now()

	app.reserveQuota(context.Background(), projAlpha, 1, now.Add(-quotaIdle))
	app.reserveQuota(context.Background(), projBeta, 1, now)
	sweepAuthCaches(now)

	quotaMu.Lock()
	defer quotaMu.Unlock()
	if _, ok := quotaProjects[projAlpha]; ok {
		t.Error("an idle project's quota was kept")
	}
	if _, ok := quotaCounters["org-1"]; ok {
		t.Error("the counter of an organization no project points to was kept")
	}
	if _, ok := quotaProjects[projBeta]; !ok || quotaCounters["org-2"] == nil {
		t.Error("an active project's quota was swept")
	}
}

func TestReserveQuota_CapsTheProjects(t *testing.T) {
	resetAuthCaches(t)
	old := maxQuotaProjects
	maxQuotaProjects = 3
	t.Cleanup(func() { maxQuotaProjects = old })
	app := &Config{Limits: newQuotaPlan(10)}

	for i := range 5 {
		app.reserveQuota(context.Background(), "project-"+strconv.Itoa(i), 1, time.Now())
	}
	quotaMu.Lock()
	defer quotaMu.Unlock()
	if len(quotaProjects) > maxQuotaProjects {
		t.Errorf("%d projects kept, want at most %d", len(quotaProjects), maxQuotaProjects)
	}
}
