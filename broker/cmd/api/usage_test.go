package main

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"logwolf-toolbox/data"
)

// usageClock is a meter's clock the test moves by hand.
type usageClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *usageClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *usageClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

// sentFlushes is a usageSender that records what it is sent and answers err.
type sentFlushes struct {
	mu      sync.Mutex
	flushes []data.RPCRecordUsageArgs
	err     error
}

func (s *sentFlushes) send(_ context.Context, args *data.RPCRecordUsageArgs) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.flushes = append(s.flushes, *args)
	return nil
}

// last is the counts of the latest flush, sorted by project and hour.
func (s *sentFlushes) last(t *testing.T) []data.RPCUsageCount {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.flushes) == 0 {
		t.Fatal("nothing was flushed")
	}
	return sortedCounts(s.flushes[len(s.flushes)-1].Counts)
}

func sortedCounts(counts []data.RPCUsageCount) []data.RPCUsageCount {
	out := slices.Clone(counts)
	slices.SortFunc(out, func(a, b data.RPCUsageCount) int {
		if c := strings.Compare(a.ProjectID, b.ProjectID); c != 0 {
			return c
		}
		return a.Hour.Compare(b.Hour)
	})
	return out
}

var usageStart = time.Date(2026, 10, 5, 14, 20, 0, 0, time.UTC)

func newTestMeter() (*usageMeter, *usageClock) {
	clock := &usageClock{now: usageStart}
	m := newUsageMeter("broker-test")
	m.now = clock.Now
	return m, clock
}

// A flush sends each project's running total for the hour, under the run's
// source, and only what changed since the last one.
func TestUsageMeter_FlushesRunningTotals(t *testing.T) {
	m, _ := newTestMeter()
	s := &sentFlushes{}
	hour := data.UsageHour(usageStart)

	m.record(projAlpha, 1, 100)
	m.record(projAlpha, 3, 300)
	m.record(projBeta, 2, 50)
	if err := m.flush(context.Background(), s.send); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if got := s.flushes[0].Source; got != "broker-test" {
		t.Errorf("source = %q, want broker-test", got)
	}
	want := sortedCounts([]data.RPCUsageCount{
		{ProjectID: projAlpha, Hour: hour, Events: 4, Bytes: 400},
		{ProjectID: projBeta, Hour: hour, Events: 2, Bytes: 50},
	})
	if got := s.last(t); !slices.Equal(got, want) {
		t.Errorf("first flush = %+v, want %+v", got, want)
	}

	// Nothing new: nothing sent.
	if err := m.flush(context.Background(), s.send); err != nil || len(s.flushes) != 1 {
		t.Fatalf("idle flush: err %v, %d flushes; want nothing sent", err, len(s.flushes))
	}

	// More for alpha: its total for the hour, not the increment, and not beta.
	m.record(projAlpha, 1, 10)
	if err := m.flush(context.Background(), s.send); err != nil {
		t.Fatalf("flush: %v", err)
	}
	want = []data.RPCUsageCount{{ProjectID: projAlpha, Hour: hour, Events: 5, Bytes: 410}}
	if got := s.last(t); !slices.Equal(got, want) {
		t.Errorf("second flush = %+v, want %+v", got, want)
	}
}

// A failed flush loses nothing: the next one sends the totals again, with what
// was counted meanwhile.
func TestUsageMeter_FailedFlushIsSentAgain(t *testing.T) {
	m, _ := newTestMeter()
	s := &sentFlushes{err: errors.New("logger unreachable")}

	m.record(projAlpha, 2, 20)
	if err := m.flush(context.Background(), s.send); err == nil {
		t.Fatal("flush succeeded with the logger unreachable")
	}
	m.record(projAlpha, 1, 10)

	s.err = nil
	if err := m.flush(context.Background(), s.send); err != nil {
		t.Fatalf("flush: %v", err)
	}
	want := []data.RPCUsageCount{{ProjectID: projAlpha, Hour: data.UsageHour(usageStart), Events: 3, Bytes: 30}}
	if got := s.last(t); !slices.Equal(got, want) {
		t.Errorf("flush after the failure = %+v, want %+v", got, want)
	}
}

// Counts go in the hour they were accepted in. Once the logger has an hour
// that is over, the meter forgets it; the current one it keeps adding to.
func TestUsageMeter_HourlyBuckets(t *testing.T) {
	m, clock := newTestMeter()
	s := &sentFlushes{}
	first, second := data.UsageHour(usageStart), data.UsageHour(usageStart).Add(time.Hour)

	m.record(projAlpha, 1, 10)
	clock.set(usageStart.Add(time.Hour))
	m.record(projAlpha, 2, 20)

	if err := m.flush(context.Background(), s.send); err != nil {
		t.Fatalf("flush: %v", err)
	}
	want := []data.RPCUsageCount{
		{ProjectID: projAlpha, Hour: first, Events: 1, Bytes: 10},
		{ProjectID: projAlpha, Hour: second, Events: 2, Bytes: 20},
	}
	if got := s.last(t); !slices.Equal(got, want) {
		t.Errorf("flush = %+v, want %+v", got, want)
	}

	m.mu.Lock()
	_, keptFirst := m.counts[usageKey{projAlpha, first}]
	_, keptSecond := m.counts[usageKey{projAlpha, second}]
	m.mu.Unlock()
	if keptFirst || !keptSecond {
		t.Errorf("after the flush: kept the past hour %v, the current one %v; want only the current one", keptFirst, keptSecond)
	}

	m.record(projAlpha, 1, 10)
	if err := m.flush(context.Background(), s.send); err != nil {
		t.Fatalf("flush: %v", err)
	}
	want = []data.RPCUsageCount{{ProjectID: projAlpha, Hour: second, Events: 3, Bytes: 30}}
	if got := s.last(t); !slices.Equal(got, want) {
		t.Errorf("flush in the same hour = %+v, want %+v", got, want)
	}
}

// Events accepted while a flush is out are not marked as sent with it.
func TestUsageMeter_CountsDuringAFlushStayPending(t *testing.T) {
	m, _ := newTestMeter()

	m.record(projAlpha, 1, 10)
	err := m.flush(context.Background(), func(context.Context, *data.RPCRecordUsageArgs) error {
		m.record(projAlpha, 5, 50) // a request served while the RPC is out
		return nil
	})
	if err != nil {
		t.Fatalf("flush: %v", err)
	}

	s := &sentFlushes{}
	if err := m.flush(context.Background(), s.send); err != nil {
		t.Fatalf("flush: %v", err)
	}
	want := []data.RPCUsageCount{{ProjectID: projAlpha, Hour: data.UsageHour(usageStart), Events: 6, Bytes: 60}}
	if got := s.last(t); !slices.Equal(got, want) {
		t.Errorf("next flush = %+v, want %+v", got, want)
	}
}

func TestUsageMeter_NilCountsNothing(t *testing.T) {
	var m *usageMeter
	m.record(projAlpha, 1, 10)
	if err := m.flush(context.Background(), func(context.Context, *data.RPCRecordUsageArgs) error {
		t.Error("a nil meter sent a flush")
		return nil
	}); err != nil {
		t.Errorf("flush = %v, want nil", err)
	}
}

// Every event the broker answers 202 for is counted, with the size of its
// queued message; none it refuses or cannot queue is.
func TestPublishEvents_MetersAcceptedEvents(t *testing.T) {
	resetAuthCaches(t)
	pub := &fakePublisher{}
	m, _ := newTestMeter()
	h := (&Config{Events: pub, Usage: m, Limits: &ratePlan{rate: 1, burst: 3}}).routes()
	key := seedKey(t, projAlpha, data.ScopeIngest)

	for _, tc := range []struct {
		path, body string
		want       int
	}{
		{"/logs/batch", batchOf(2), http.StatusAccepted},
		{"/logs", `{"name":"x","severity":"info"}`, http.StatusAccepted},
		{"/logs", `{"name":"x","severity":"verbose"}`, http.StatusBadRequest},
		{"/logs/batch", batchOf(2), http.StatusTooManyRequests},
	} {
		if w := do(h, keyRequest(http.MethodPost, tc.path, key, tc.body)); w.Code != tc.want {
			t.Fatalf("POST %s = %d, want %d (body: %s)", tc.path, w.Code, tc.want, w.Body.String())
		}
	}

	// Another key: this one's bucket is empty now.
	failing := (&Config{Events: &fakePublisher{err: errors.New("not confirmed")}, Usage: m}).routes()
	unconfirmed := seedKey(t, projAlpha, data.ScopeIngest)
	if w := do(failing, keyRequest(http.MethodPost, "/logs", unconfirmed, `{"name":"x","severity":"info"}`)); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST /logs unconfirmed = %d, want 503", w.Code)
	}

	var size int64
	for _, call := range pub.calls {
		for _, msg := range call {
			size += int64(len(msg.Body))
		}
	}
	s := &sentFlushes{}
	if err := m.flush(context.Background(), s.send); err != nil {
		t.Fatalf("flush: %v", err)
	}
	want := []data.RPCUsageCount{{ProjectID: projAlpha, Hour: data.UsageHour(usageStart), Events: 3, Bytes: size}}
	if got := s.last(t); !slices.Equal(got, want) {
		t.Errorf("metered %+v, want the 3 accepted events, %d bytes", got, size)
	}
}

// The dashboard's events are stored like any other, so they count too.
func TestPublishEvents_MetersDashboardEvents(t *testing.T) {
	newInternalTestServer(t)
	m, _ := newTestMeter()
	h := (&Config{Events: &fakePublisher{}, Usage: m}).routes()

	w := do(h, internalRequest(http.MethodPost, "/projects/"+projAlpha+"/logs", "member-a", map[string]any{"name": "x", "severity": "info"}))
	if w.Code != http.StatusAccepted {
		t.Fatalf("POST /projects/{id}/logs = %d, want 202 (body: %s)", w.Code, w.Body.String())
	}

	s := &sentFlushes{}
	if err := m.flush(context.Background(), s.send); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if got := s.last(t); len(got) != 1 || got[0].ProjectID != projAlpha || got[0].Events != 1 || got[0].Bytes == 0 {
		t.Errorf("metered %+v, want one event for %s", got, projAlpha)
	}
}

// recordUsage carries a flush to the logger's RecordUsage, and its failure back.
func TestRecordUsage_OverRPC(t *testing.T) {
	f := newFakeLogger()
	serveFakeLogger(t, f)
	m, _ := newTestMeter()
	m.record(projAlpha, 2, 20)

	f.mu.Lock()
	f.failRecordUsage = true
	f.mu.Unlock()
	if err := m.flush(context.Background(), recordUsage); err == nil {
		t.Fatal("flush succeeded with RecordUsage failing")
	}

	f.mu.Lock()
	f.failRecordUsage = false
	f.mu.Unlock()
	if err := m.flush(context.Background(), recordUsage); err != nil {
		t.Fatalf("flush: %v", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	want := data.RPCUsageCount{ProjectID: projAlpha, Hour: data.UsageHour(usageStart), Events: 2, Bytes: 20}
	if len(f.usageFlushes) != 1 || f.usageFlushes[0].Source != "broker-test" || len(f.usageFlushes[0].Counts) != 1 {
		t.Fatalf("the logger got %+v, want one flush of one count from broker-test", f.usageFlushes)
	}
	// gob carries the hour as the same instant, not the same time.Time value.
	if got := f.usageFlushes[0].Counts[0]; got.ProjectID != want.ProjectID || !got.Hour.Equal(want.Hour) || got.Events != want.Events || got.Bytes != want.Bytes {
		t.Errorf("the logger got %+v, want %+v", got, want)
	}
}

func TestUsageFlushInterval(t *testing.T) {
	for env, want := range map[string]time.Duration{
		"":      time.Minute,
		"15s":   15 * time.Second,
		"bogus": time.Minute,
		"0s":    time.Minute,
	} {
		t.Setenv("USAGE_FLUSH_INTERVAL", env)
		if got := usageFlushInterval(); got != want {
			t.Errorf("USAGE_FLUSH_INTERVAL=%q: interval = %s, want %s", env, got, want)
		}
	}
}

// Each run has a source of its own, so a restarted broker's counts never mix
// with those of the run before it.
func TestUsageSource_PerRun(t *testing.T) {
	if a, b := usageSource(), usageSource(); a == b || a == "" || a == data.StorageSource {
		t.Errorf("usageSource() = %q, then %q; want two distinct broker sources", a, b)
	}
}
