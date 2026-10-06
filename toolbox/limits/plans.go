package limits

import (
	"slices"

	"logwolf-toolbox/data"
)

// Unlimited, in any of a Plan's limits, means the plan sets none. For
// MaxRetentionDays it is also the retention it reads as: 0 days is forever.
const Unlimited = 0

// Plan is what an organization's plan lets it do. Plans live in code, not in the
// database: an organization stores only its plan's name (data.Organization.Plan),
// and PlanByName turns that into the limits.
type Plan struct {
	// Name is the name organizations store.
	Name string
	// MonthlyEvents is how many events the organization's projects may ingest
	// between them in a calendar month.
	MonthlyEvents int64
	// MaxRetentionDays is the longest retention a project may pick; Unlimited
	// allows forever.
	MaxRetentionDays int
	// MaxProjects is how many projects the organization may have.
	MaxProjects int
	// MaxMembers is how many members the organization may have.
	MaxMembers int
}

// Names of the plans organizations may be on.
const (
	PlanSelfHosted = data.SelfHostedPlan
	PlanFree       = "free"
	PlanPro        = "pro"
	PlanTeam       = "team"
)

// plans is every plan there is, by name. Self-hosted is the only one without a
// limit; the others are the hosted edition's.
var plans = map[string]Plan{
	PlanSelfHosted: {
		Name:             PlanSelfHosted,
		MonthlyEvents:    Unlimited,
		MaxRetentionDays: Unlimited,
		MaxProjects:      Unlimited,
		MaxMembers:       Unlimited,
	},
	PlanFree: {
		Name:             PlanFree,
		MonthlyEvents:    100_000,
		MaxRetentionDays: 30,
		MaxProjects:      3,
		MaxMembers:       3,
	},
	PlanPro: {
		Name:             PlanPro,
		MonthlyEvents:    5_000_000,
		MaxRetentionDays: 90,
		MaxProjects:      20,
		MaxMembers:       20,
	},
	PlanTeam: {
		Name:             PlanTeam,
		MonthlyEvents:    25_000_000,
		MaxRetentionDays: 365,
		MaxProjects:      Unlimited,
		MaxMembers:       Unlimited,
	},
}

// PlanByName returns the plan an organization names, and false when no plan
// has that name.
func PlanByName(name string) (Plan, bool) {
	p, ok := plans[name]
	return p, ok
}

// SelfHostedPlan is the one plan of a self-hosted install, which limits nothing.
func SelfHostedPlan() Plan {
	return plans[PlanSelfHosted]
}

// retentionOrder is every retention Logwolf supports, forever first, as the
// dashboard has always listed them.
var retentionOrder = []int{0, 30, 60, 90, 180, 365}

// RetentionChoices lists the retention values, in days, a project on the plan
// may pick, in the order the dashboard shows them: every supported value up to
// MaxRetentionDays, and forever only when the plan has no maximum.
func (p Plan) RetentionChoices() []int {
	if p.MaxRetentionDays == Unlimited {
		return slices.Clone(retentionOrder)
	}
	var choices []int
	for _, days := range retentionOrder {
		if days != 0 && days <= p.MaxRetentionDays {
			choices = append(choices, days)
		}
	}
	return choices
}
