package limits

import (
	"maps"
	"slices"
	"testing"

	"logwolf-toolbox/data"
)

// The table names every plan by the name organizations store, and the
// self-hosted one by the name the startup migration gives the Default
// organization.
func TestPlans_NamedAsStored(t *testing.T) {
	for name, p := range plans {
		if p.Name != name {
			t.Errorf("plans[%q].Name = %q", name, p.Name)
		}
		if got, ok := PlanByName(name); !ok || got != p {
			t.Errorf("PlanByName(%q) = %+v, %v; want %+v, true", name, got, ok, p)
		}
	}
	if _, ok := PlanByName(data.SelfHostedPlan); !ok {
		t.Errorf("no plan named %q, the Default organization's", data.SelfHostedPlan)
	}
	for _, name := range []string{"", "Free", "enterprise"} {
		if p, ok := PlanByName(name); ok {
			t.Errorf("PlanByName(%q) = %+v, want no plan", name, p)
		}
	}
}

func TestSelfHostedPlan_IsUnlimited(t *testing.T) {
	want := Plan{
		Name:             data.SelfHostedPlan,
		MonthlyEvents:    Unlimited,
		MaxRetentionDays: Unlimited,
		MaxProjects:      Unlimited,
		MaxMembers:       Unlimited,
		IngestRate:       Unlimited,
		IngestBurst:      Unlimited,
	}
	if got := SelfHostedPlan(); got != want {
		t.Errorf("SelfHostedPlan() = %+v, want %+v", got, want)
	}
}

// Only self-hosted goes without an event quota: a hosted plan that left it at
// Unlimited by mistake would give ingestion away. Every plan's retention cap
// must be a value the logger stores.
func TestHostedPlans_AreLimited(t *testing.T) {
	for name, p := range plans {
		if !data.ValidRetentionDays[p.MaxRetentionDays] {
			t.Errorf("%s: MaxRetentionDays = %d, want one of data.ValidRetentionDays", name, p.MaxRetentionDays)
		}
		if p.MaxProjects < 0 || p.MaxMembers < 0 {
			t.Errorf("%s: MaxProjects = %d, MaxMembers = %d; want Unlimited or a positive limit", name, p.MaxProjects, p.MaxMembers)
		}
		if name != PlanSelfHosted && p.MonthlyEvents <= 0 {
			t.Errorf("%s: MonthlyEvents = %d, want a quota", name, p.MonthlyEvents)
		}
		if name != PlanSelfHosted && p.IngestRate <= 0 {
			t.Errorf("%s: IngestRate = %d, want a rate", name, p.IngestRate)
		}
	}
}

func TestPlan_RetentionChoices(t *testing.T) {
	cases := []struct {
		max  int
		want []int
	}{
		{Unlimited, []int{0, 30, 60, 90, 180, 365}},
		{30, []int{30}},
		{90, []int{30, 60, 90}},
		{180, []int{30, 60, 90, 180}},
		{365, []int{30, 60, 90, 180, 365}},
	}
	for _, tc := range cases {
		if got := (Plan{MaxRetentionDays: tc.max}).RetentionChoices(); !slices.Equal(got, tc.want) {
			t.Errorf("max %d: RetentionChoices() = %v, want %v", tc.max, got, tc.want)
		}
	}
}

// Every plan offers at least one retention, and only ones the logger stores.
func TestPlans_RetentionChoicesAreValid(t *testing.T) {
	valid := slices.Sorted(maps.Keys(data.ValidRetentionDays))
	for name, p := range plans {
		choices := p.RetentionChoices()
		if len(choices) == 0 {
			t.Errorf("%s: no retention choices", name)
		}
		for _, days := range choices {
			if !data.ValidRetentionDays[days] {
				t.Errorf("%s: choice %d is not one of data.ValidRetentionDays %v", name, days, valid)
			}
		}
	}
}

// A caller may do what it likes with the slice it got without changing what
// the next caller gets.
func TestPlan_RetentionChoicesAreACopy(t *testing.T) {
	first := SelfHostedPlan().RetentionChoices()
	first[0] = 7
	if second := SelfHostedPlan().RetentionChoices(); second[0] != 0 {
		t.Errorf("RetentionChoices changed under its caller: %v", second)
	}
}

// A rate goes with a bucket that holds at least a second of it, and no rate
// with no bucket: a plan with one and not the other would refuse every event or
// refill nothing.
func TestPlans_IngestBucketFitsTheRate(t *testing.T) {
	for name, p := range plans {
		if p.IngestRate < 0 || p.IngestBurst < 0 {
			t.Errorf("%s: IngestRate = %d, IngestBurst = %d; want Unlimited or positive", name, p.IngestRate, p.IngestBurst)
		}
		if (p.IngestRate == Unlimited) != (p.IngestBurst == Unlimited) {
			t.Errorf("%s: IngestRate = %d, IngestBurst = %d; want both Unlimited or neither", name, p.IngestRate, p.IngestBurst)
		}
		if p.IngestBurst < p.IngestRate {
			t.Errorf("%s: IngestBurst = %d holds less than a second of IngestRate = %d", name, p.IngestBurst, p.IngestRate)
		}
	}
}

func TestPlans_ListsTheTable(t *testing.T) {
	got := Plans()
	if len(got) != len(plans) {
		t.Fatalf("Plans() has %d plans, want %d", len(got), len(plans))
	}
	for _, p := range got {
		if plans[p.Name] != p {
			t.Errorf("Plans() has %+v, the table %+v", p, plans[p.Name])
		}
	}
}
