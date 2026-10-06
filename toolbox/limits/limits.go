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
	"time"

	"logwolf-toolbox/data"
)

// Provider answers the questions a hosted plan would limit. Project ids are
// hex strings, the way services pass them to each other.
type Provider interface {
	// Plan returns the plan whose limits apply to the project: its
	// organization's.
	Plan(ctx context.Context, projectID string) (Plan, error)

	// MonthlyQuota returns the monthly event quota the project's events count
	// toward, its organization's, and how much of it the month has used. A
	// Quota whose Limit is Unlimited holds the project to nothing.
	MonthlyQuota(ctx context.Context, projectID string) (Quota, error)

	// RetentionChoices lists the retention values, in days, the project may
	// pick, in the order the dashboard shows them: its plan's
	// (Plan.RetentionChoices). 0 is forever. Every value must be one of
	// data.ValidRetentionDays, the only ones the logger stores.
	RetentionChoices(ctx context.Context, projectID string) ([]int, error)

	// OrganizationPlan returns the plan of an organization that stores plan as
	// its plan's name (data.Organization.Plan).
	OrganizationPlan(plan string) (Plan, error)

	// NewOrganizationPlan is the plan an organization a user creates starts on.
	NewOrganizationPlan() Plan
}

// PlanLookup returns the name of the plan of the organization a project is in.
// The hosted edition's Provider resolves plans through it; the service running
// the Provider supplies it, since only the logger can read organizations.
type PlanLookup func(ctx context.Context, projectID string) (string, error)

// QuotaLookup returns the organization a project is in, the name of its plan,
// and the events it has ingested this month (data.Models.ProjectQuota). The
// hosted edition's Provider builds a project's Quota from it.
type QuotaLookup func(ctx context.Context, projectID string) (data.ProjectQuota, error)

// Lookups are what the hosted edition's Provider asks the logger through.
// Self-hosted needs neither.
type Lookups struct {
	Plan  PlanLookup
	Quota QuotaLookup
}

// Quota is a monthly event quota: how many events the organization OrganizationID
// may ingest in Month, and how many it has.
type Quota struct {
	// OrganizationID is the hex id of the organization whose projects share
	// the quota; empty when there is none.
	OrganizationID string
	// Limit is the plan's MonthlyEvents; Unlimited sets none.
	Limit int64
	// Month is the first instant of the calendar month, in UTC
	// (data.UsageMonth).
	Month time.Time
	// Used is the events the organization's projects have ingested in Month,
	// as far as the brokers have flushed them.
	Used int64
}

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
// project's plan and quota through lookups, and is refused without either
// rather than run with self-hosted limits, as is any name it does not know.
// Self-hosted ignores lookups.
func ForEdition(edition string, lookups Lookups) (Provider, error) {
	switch edition {
	case EditionSelfHosted:
		return SelfHosted{}, nil
	case EditionCloud:
		if lookups.Plan == nil {
			return nil, fmt.Errorf("LOGWOLF_EDITION=%s: no way to look up a project's plan", edition)
		}
		if lookups.Quota == nil {
			return nil, fmt.Errorf("LOGWOLF_EDITION=%s: no way to look up a project's quota", edition)
		}
		return Organizations{PlanOf: lookups.Plan, QuotaOf: lookups.Quota}, nil
	default:
		return nil, fmt.Errorf("LOGWOLF_EDITION=%s: unknown edition, want %s or %s", edition, EditionSelfHosted, EditionCloud)
	}
}

// FromEnv is ForEdition(EditionFromEnv(), lookups).
func FromEnv(lookups Lookups) (Provider, error) {
	return ForEdition(EditionFromEnv(), lookups)
}

// SelfHosted is the Provider of a self-hosted install: every project is on
// SelfHostedPlan, which has no limits at all.
type SelfHosted struct{}

// Plan is SelfHostedPlan, whatever the project.
func (SelfHosted) Plan(context.Context, string) (Plan, error) {
	return SelfHostedPlan(), nil
}

// MonthlyQuota sets no limit, and asks nothing: a self-hosted install has no
// quota.
func (SelfHosted) MonthlyQuota(context.Context, string) (Quota, error) {
	return Quota{Limit: Unlimited}, nil
}

// RetentionChoices offers every supported retention to every project.
func (SelfHosted) RetentionChoices(context.Context, string) ([]int, error) {
	return SelfHostedPlan().RetentionChoices(), nil
}

// OrganizationPlan is SelfHostedPlan, whatever the organization stores.
func (SelfHosted) OrganizationPlan(string) (Plan, error) {
	return SelfHostedPlan(), nil
}

// NewOrganizationPlan is SelfHostedPlan: every organization of a self-hosted
// install is on it.
func (SelfHosted) NewOrganizationPlan() Plan {
	return SelfHostedPlan()
}

// Organizations is the hosted edition's Provider: a project's limits are those
// of its organization's plan, which PlanOf names. QuotaOf tells how much of the
// plan's monthly events the organization has used.
type Organizations struct {
	PlanOf  PlanLookup
	QuotaOf QuotaLookup
}

// Plan looks up the plan of the project's organization. A plan name that is not
// in the table is an error, not a plan without limits.
func (o Organizations) Plan(ctx context.Context, projectID string) (Plan, error) {
	name, err := o.PlanOf(ctx, projectID)
	if err != nil {
		return Plan{}, fmt.Errorf("plan of project %s: %w", projectID, err)
	}
	p, err := o.OrganizationPlan(name)
	if err != nil {
		return Plan{}, fmt.Errorf("plan of project %s: %w", projectID, err)
	}
	return p, nil
}

// OrganizationPlan is the plan named plan. A name that is not in the table is
// an error, not a plan without limits.
func (Organizations) OrganizationPlan(plan string) (Plan, error) {
	p, ok := PlanByName(plan)
	if !ok {
		return Plan{}, fmt.Errorf("unknown plan %q", plan)
	}
	return p, nil
}

// NewOrganizationPlan is the free plan: a hosted organization is on it until
// it subscribes.
func (Organizations) NewOrganizationPlan() Plan {
	return plans[PlanFree]
}

// MonthlyQuota is the plan's MonthlyEvents for the project's organization, with
// the events it has ingested this month. A plan name that is not in the table
// is an error, not a quota without a limit.
func (o Organizations) MonthlyQuota(ctx context.Context, projectID string) (Quota, error) {
	q, err := o.QuotaOf(ctx, projectID)
	if err != nil {
		return Quota{}, fmt.Errorf("quota of project %s: %w", projectID, err)
	}
	p, err := o.OrganizationPlan(q.Plan)
	if err != nil {
		return Quota{}, fmt.Errorf("quota of project %s: %w", projectID, err)
	}
	return Quota{OrganizationID: q.OrganizationID, Limit: p.MonthlyEvents, Month: q.Month, Used: q.Events}, nil
}

// RetentionChoices are those of the project's plan.
func (o Organizations) RetentionChoices(ctx context.Context, projectID string) ([]int, error) {
	p, err := o.Plan(ctx, projectID)
	if err != nil {
		return nil, err
	}
	return p.RetentionChoices(), nil
}
