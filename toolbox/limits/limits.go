// Package limits holds what a deployment lets a project do beyond Logwolf's own
// rules: how many events it may ingest and which retention it may pick.
//
// A project's limits are its organization's plan (Plan, in plans.go). A
// self-hosted install has one plan, which limits nothing, so its Provider
// allows everything Logwolf supports without asking which organization a
// project is in. The hosted edition looks the plan up (Organizations). Which one
// a service runs with is picked by LOGWOLF_EDITION (see FromEnv).
package limits

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// Provider answers the questions a hosted plan would limit. Project ids are
// hex strings, the way services pass them to each other.
type Provider interface {
	// Plan returns the plan whose limits apply to the project: its
	// organization's.
	Plan(ctx context.Context, projectID string) (Plan, error)

	// AllowIngest reports whether the project may ingest n more events.
	AllowIngest(ctx context.Context, projectID string, n int) (bool, error)

	// RetentionChoices lists the retention values, in days, the project may
	// pick, in the order the dashboard shows them: its plan's
	// (Plan.RetentionChoices). 0 is forever. Every value must be one of
	// data.ValidRetentionDays, the only ones the logger stores.
	RetentionChoices(ctx context.Context, projectID string) ([]int, error)
}

// PlanLookup returns the name of the plan of the organization a project is in.
// The hosted edition's Provider resolves plans through it; the service running
// the Provider supplies it, since only the logger can read organizations.
type PlanLookup func(ctx context.Context, projectID string) (string, error)

// Editions LOGWOLF_EDITION may name.
const (
	EditionSelfHosted = "selfhosted"
	EditionCloud      = "cloud"
)

// EditionFromEnv reads LOGWOLF_EDITION: trimmed and lowercased, and
// EditionSelfHosted when it is unset or blank.
func EditionFromEnv() string {
	e := strings.ToLower(strings.TrimSpace(os.Getenv("LOGWOLF_EDITION")))
	if e == "" {
		return EditionSelfHosted
	}
	return e
}

// ForEdition returns the Provider of an edition. The cloud edition resolves each
// project's plan with planOf, and is refused without one rather than run with
// self-hosted limits, as is any name it does not know. Self-hosted ignores
// planOf.
func ForEdition(edition string, planOf PlanLookup) (Provider, error) {
	switch edition {
	case EditionSelfHosted:
		return SelfHosted{}, nil
	case EditionCloud:
		if planOf == nil {
			return nil, fmt.Errorf("LOGWOLF_EDITION=%s: no way to look up a project's plan", edition)
		}
		return Organizations{PlanOf: planOf}, nil
	default:
		return nil, fmt.Errorf("LOGWOLF_EDITION=%s: unknown edition, want %s or %s", edition, EditionSelfHosted, EditionCloud)
	}
}

// FromEnv is ForEdition(EditionFromEnv(), planOf).
func FromEnv(planOf PlanLookup) (Provider, error) {
	return ForEdition(EditionFromEnv(), planOf)
}

// SelfHosted is the Provider of a self-hosted install: every project is on
// SelfHostedPlan, which has no limits at all.
type SelfHosted struct{}

// Plan is SelfHostedPlan, whatever the project.
func (SelfHosted) Plan(context.Context, string) (Plan, error) {
	return SelfHostedPlan(), nil
}

// AllowIngest always allows: a self-hosted install has no quota.
func (SelfHosted) AllowIngest(context.Context, string, int) (bool, error) {
	return true, nil
}

// RetentionChoices offers every supported retention to every project.
func (SelfHosted) RetentionChoices(context.Context, string) ([]int, error) {
	return SelfHostedPlan().RetentionChoices(), nil
}

// Organizations is the hosted edition's Provider: a project's limits are those
// of its organization's plan, which PlanOf names.
type Organizations struct {
	PlanOf PlanLookup
}

// Plan looks up the plan of the project's organization. A plan name that is not
// in the table is an error, not a plan without limits.
func (o Organizations) Plan(ctx context.Context, projectID string) (Plan, error) {
	name, err := o.PlanOf(ctx, projectID)
	if err != nil {
		return Plan{}, fmt.Errorf("plan of project %s: %w", projectID, err)
	}
	p, ok := PlanByName(name)
	if !ok {
		return Plan{}, fmt.Errorf("plan of project %s: unknown plan %q", projectID, name)
	}
	return p, nil
}

// AllowIngest allows every event for now: events are not counted yet, so there
// is nothing to hold against the plan's MonthlyEvents.
func (Organizations) AllowIngest(context.Context, string, int) (bool, error) {
	return true, nil
}

// RetentionChoices are those of the project's plan.
func (o Organizations) RetentionChoices(ctx context.Context, projectID string) ([]int, error) {
	p, err := o.Plan(ctx, projectID)
	if err != nil {
		return nil, err
	}
	return p.RetentionChoices(), nil
}
