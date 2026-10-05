package main

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"
	"time"

	"logwolf-toolbox/limits"
)

// planLimits is a limits.Provider with fixed answers, standing in for a hosted
// plan. It records the projects it was asked about.
type planLimits struct {
	retention []int
	err       error
	asked     []string
}

func (p *planLimits) Plan(context.Context, string) (limits.Plan, error) {
	return limits.Plan{Name: "fixed"}, p.err
}

func (p *planLimits) AllowIngest(context.Context, string, int) (bool, error) { return true, nil }

func (p *planLimits) RetentionChoices(_ context.Context, projectID string) ([]int, error) {
	p.asked = append(p.asked, projectID)
	return p.retention, p.err
}

// newLimitedTestServer is newInternalTestServer with lim as the edition's limits.
func newLimitedTestServer(t *testing.T, lim limits.Provider) (http.Handler, *fakeLogger) {
	t.Helper()
	_, f := newInternalTestServer(t)
	return (&Config{Limits: lim}).routes(), f
}

type retentionData struct {
	Days    int   `json:"days"`
	Choices []int `json:"choices"`
}

// A broker with no Limits is self-hosted: every supported retention, as the
// dashboard has always offered them.
func TestGetRetention_SelfHostedChoices(t *testing.T) {
	handler, _ := newInternalTestServer(t)

	w := do(handler, internalRequest(http.MethodGet, "/projects/"+projAlpha+"/retention", "member-a", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	got := decodeData[retentionData](t, w)
	if got.Days != 90 {
		t.Errorf("days = %d, want the default 90", got.Days)
	}
	if want := []int{0, 30, 60, 90, 180, 365}; !slices.Equal(got.Choices, want) {
		t.Errorf("choices = %v, want %v", got.Choices, want)
	}
}

func TestRetention_ChoicesComeFromTheProvider(t *testing.T) {
	lim := &planLimits{retention: []int{30, 60}}
	handler, fake := newLimitedTestServer(t, lim)

	w := do(handler, internalRequest(http.MethodGet, "/projects/"+projAlpha+"/retention", "member-a", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET: got %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	if got := decodeData[retentionData](t, w).Choices; !slices.Equal(got, lim.retention) {
		t.Errorf("GET choices = %v, want %v", got, lim.retention)
	}

	// 365 is a retention Logwolf supports, but not one this plan offers.
	w = do(handler, internalRequest(http.MethodPatch, "/projects/"+projAlpha+"/retention", "owner-a",
		map[string]any{"days": 365}))
	if w.Code != http.StatusBadRequest {
		t.Errorf("PATCH 365: got %d, want 400 (body: %s)", w.Code, w.Body.String())
	}
	fake.snapshot(func(f *fakeLogger) {
		if days, set := f.retention[projAlpha]; set {
			t.Errorf("a retention outside the plan was stored: %d", days)
		}
	})

	w = do(handler, internalRequest(http.MethodPatch, "/projects/"+projAlpha+"/retention", "owner-a",
		map[string]any{"days": 60}))
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH 60: got %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	if got := decodeData[retentionData](t, w); got.Days != 60 || !slices.Equal(got.Choices, lim.retention) {
		t.Errorf("PATCH 60 answered %+v", got)
	}

	for _, id := range lim.asked {
		if id != projAlpha {
			t.Errorf("provider asked about project %q, want %q", id, projAlpha)
		}
	}
}

func TestRetention_ProviderFailureIsAnInternalError(t *testing.T) {
	handler, fake := newLimitedTestServer(t, &planLimits{err: errors.New("plans unavailable")})

	if w := do(handler, internalRequest(http.MethodGet, "/projects/"+projAlpha+"/retention", "member-a", nil)); w.Code != http.StatusInternalServerError {
		t.Errorf("GET: got %d, want 500 (body: %s)", w.Code, w.Body.String())
	}

	w := do(handler, internalRequest(http.MethodPatch, "/projects/"+projAlpha+"/retention", "owner-a",
		map[string]any{"days": 30}))
	if w.Code != http.StatusInternalServerError {
		t.Errorf("PATCH: got %d, want 500 (body: %s)", w.Code, w.Body.String())
	}
	fake.snapshot(func(f *fakeLogger) {
		if days, set := f.retention[projAlpha]; set {
			t.Errorf("retention stored without the plan's say: %d", days)
		}
	})
}

// A broker on the hosted edition offers each project the retention of its
// organization's plan, which it asks the logger for, and stores nothing else.
func TestRetention_CloudChoicesComeFromTheOrganizationsPlan(t *testing.T) {
	handler, fake := newLimitedTestServer(t, limits.Organizations{PlanOf: projectPlan})
	fake.setPlan(projAlpha, limits.PlanPro)

	w := do(handler, internalRequest(http.MethodGet, "/projects/"+projAlpha+"/retention", "member-a", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET: got %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	if got, want := decodeData[retentionData](t, w).Choices, []int{30, 60, 90}; !slices.Equal(got, want) {
		t.Errorf("GET choices = %v, want %v, the pro plan's", got, want)
	}

	w = do(handler, internalRequest(http.MethodPatch, "/projects/"+projAlpha+"/retention", "owner-a",
		map[string]any{"days": 180}))
	if w.Code != http.StatusBadRequest {
		t.Errorf("PATCH 180: got %d, want 400 (body: %s)", w.Code, w.Body.String())
	}
	w = do(handler, internalRequest(http.MethodPatch, "/projects/"+projAlpha+"/retention", "owner-a",
		map[string]any{"days": 60}))
	if w.Code != http.StatusOK {
		t.Errorf("PATCH 60: got %d, want 200 (body: %s)", w.Code, w.Body.String())
	}

	// The lookup's own connections are closed, like the handlers'. The fake
	// closes its end once it reads EOF, which takes a moment.
	deadline := time.Now().Add(2 * time.Second)
	for fake.openConns.Load() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%d logger connection(s) left open", fake.openConns.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A project whose plan cannot be told, because its organization is missing or
// on a plan the broker does not know, is offered nothing rather than
// everything.
func TestRetention_CloudWithoutAKnownPlanIsAnInternalError(t *testing.T) {
	handler, fake := newLimitedTestServer(t, limits.Organizations{PlanOf: projectPlan})
	fake.setPlan(projBeta, "enterprise")

	for project, member := range map[string]string{projAlpha: "member-a", projBeta: "owner-b"} {
		w := do(handler, internalRequest(http.MethodGet, "/projects/"+project+"/retention", member, nil))
		if w.Code != http.StatusInternalServerError {
			t.Errorf("GET %s: got %d, want 500 (body: %s)", project, w.Code, w.Body.String())
		}
	}
}

func TestProjectPlan(t *testing.T) {
	_, fake := newInternalTestServer(t)
	fake.setPlan(projAlpha, limits.PlanFree)

	plan, err := projectPlan(context.Background(), projAlpha)
	if err != nil || plan != limits.PlanFree {
		t.Errorf("projectPlan(alpha) = %q, %v; want %q", plan, err, limits.PlanFree)
	}

	if plan, err := projectPlan(context.Background(), projBeta); err == nil {
		t.Errorf("projectPlan of a project in no organization = %q, want an error", plan)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := projectPlan(ctx, projAlpha); !errors.Is(err, context.Canceled) {
		t.Errorf("projectPlan with a done context: %v, want context.Canceled", err)
	}
}
