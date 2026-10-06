package limits

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"logwolf-toolbox/data"
)

const project = "aaaaaaaaaaaaaaaaaaaaaa01"

func TestSelfHosted_HasNoQuota(t *testing.T) {
	q, err := SelfHosted{}.MonthlyQuota(context.Background(), project)
	if err != nil || q.Limit != Unlimited {
		t.Errorf("MonthlyQuota = %+v, %v; want no limit", q, err)
	}
}

// Self-hosted offers exactly what the logger stores, in the order the
// dashboard always listed them.
func TestSelfHosted_RetentionChoicesAreEveryValidValue(t *testing.T) {
	got, err := SelfHosted{}.RetentionChoices(context.Background(), project)
	if err != nil {
		t.Fatalf("RetentionChoices: %v", err)
	}
	if want := []int{0, 30, 60, 90, 180, 365}; !slices.Equal(got, want) {
		t.Errorf("RetentionChoices = %v, want %v", got, want)
	}

	valid := slices.Sorted(maps.Keys(data.ValidRetentionDays))
	if !slices.Equal(slices.Sorted(slices.Values(got)), valid) {
		t.Errorf("RetentionChoices = %v, want every one of data.ValidRetentionDays %v", got, valid)
	}
}

// A caller may do what it likes with the slice it got without changing what
// the next caller gets.
func TestSelfHosted_RetentionChoicesAreACopy(t *testing.T) {
	first, _ := SelfHosted{}.RetentionChoices(context.Background(), project)
	first[0] = 7

	second, _ := SelfHosted{}.RetentionChoices(context.Background(), project)
	if second[0] != 0 {
		t.Errorf("RetentionChoices changed under its caller: %v", second)
	}
}

func TestEditionFromEnv(t *testing.T) {
	cases := []struct {
		env  string
		want string
	}{
		{"", EditionSelfHosted},
		{"  ", EditionSelfHosted},
		{"selfhosted", EditionSelfHosted},
		{" SelfHosted ", EditionSelfHosted},
		{"CLOUD", EditionCloud},
		{"enterprise", "enterprise"},
	}
	for _, tc := range cases {
		t.Setenv("LOGWOLF_EDITION", tc.env)
		if got := EditionFromEnv(); got != tc.want {
			t.Errorf("LOGWOLF_EDITION=%q: EditionFromEnv() = %q, want %q", tc.env, got, tc.want)
		}
	}
}

func TestForEdition(t *testing.T) {
	// Self-hosted never looks a plan up, so it needs no lookup.
	p, err := ForEdition(EditionSelfHosted, Lookups{})
	if err != nil {
		t.Fatalf("ForEdition(selfhosted): %v", err)
	}
	if _, ok := p.(SelfHosted); !ok {
		t.Errorf("ForEdition(selfhosted) = %T, want SelfHosted", p)
	}

	p, err = ForEdition(EditionCloud, Lookups{Plan: planNamed(PlanFree), Quota: quotaOf(PlanFree, 0)})
	if err != nil {
		t.Fatalf("ForEdition(cloud): %v", err)
	}
	if _, ok := p.(Organizations); !ok {
		t.Errorf("ForEdition(cloud) = %T, want Organizations", p)
	}

	// Running a cloud deployment on self-hosted limits would give every
	// project everything, so cloud without a way to find a project's plan or
	// quota is an error, as is an edition with no provider at all.
	if p, err := ForEdition(EditionCloud, Lookups{}); err == nil {
		t.Errorf("ForEdition(cloud, no lookups) = %T, want an error", p)
	}
	if p, err := ForEdition(EditionCloud, Lookups{Plan: planNamed(PlanFree)}); err == nil {
		t.Errorf("ForEdition(cloud, no quota lookup) = %T, want an error", p)
	}
	if p, err := ForEdition(EditionCloud, Lookups{Quota: quotaOf(PlanFree, 0)}); err == nil {
		t.Errorf("ForEdition(cloud, no plan lookup) = %T, want an error", p)
	}
	if p, err := ForEdition("enterprise", Lookups{Plan: planNamed(PlanFree), Quota: quotaOf(PlanFree, 0)}); err == nil {
		t.Errorf("ForEdition(enterprise) = %T, want an error", p)
	}
}

func TestFromEnv_DefaultsToSelfHosted(t *testing.T) {
	t.Setenv("LOGWOLF_EDITION", "")
	p, err := FromEnv(Lookups{})
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if _, ok := p.(SelfHosted); !ok {
		t.Errorf("FromEnv() = %T, want SelfHosted", p)
	}
}

func TestSelfHosted_PlanIsTheSelfHostedPlan(t *testing.T) {
	got, err := SelfHosted{}.Plan(context.Background(), project)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got != SelfHostedPlan() {
		t.Errorf("Plan = %+v, want %+v", got, SelfHostedPlan())
	}
}

// planNamed is a PlanLookup that puts every project on the plan called name.
func planNamed(name string) PlanLookup {
	return func(context.Context, string) (string, error) { return name, nil }
}

const organization = "bbbbbbbbbbbbbbbbbbbbbb01"

// quotaOf is a QuotaLookup that puts every project in organization, on the plan
// called name, with events used this month.
func quotaOf(name string, events int64) QuotaLookup {
	return func(context.Context, string) (data.ProjectQuota, error) {
		return data.ProjectQuota{OrganizationID: organization, Plan: name, Month: data.UsageMonth(time.Now()), Events: events}, nil
	}
}

// The hosted edition's limits are those of the plan the project's organization
// is on, asked about the project in question.
func TestOrganizations_ResolvesTheOrganizationsPlan(t *testing.T) {
	var asked []string
	o := Organizations{PlanOf: func(_ context.Context, projectID string) (string, error) {
		asked = append(asked, projectID)
		return PlanPro, nil
	}}

	got, err := o.Plan(context.Background(), project)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if want, _ := PlanByName(PlanPro); got != want {
		t.Errorf("Plan = %+v, want %+v", got, want)
	}

	choices, err := o.RetentionChoices(context.Background(), project)
	if err != nil {
		t.Fatalf("RetentionChoices: %v", err)
	}
	if want := []int{30, 60, 90}; !slices.Equal(choices, want) {
		t.Errorf("RetentionChoices = %v, want %v", choices, want)
	}

	if want := []string{project, project}; !slices.Equal(asked, want) {
		t.Errorf("PlanOf asked about %v, want %v", asked, want)
	}
}

// An organization on the self-hosted plan gets its limits, none, under either
// edition.
func TestOrganizations_SelfHostedPlan(t *testing.T) {
	choices, err := Organizations{PlanOf: planNamed(PlanSelfHosted)}.RetentionChoices(context.Background(), project)
	if err != nil {
		t.Fatalf("RetentionChoices: %v", err)
	}
	if want := []int{0, 30, 60, 90, 180, 365}; !slices.Equal(choices, want) {
		t.Errorf("RetentionChoices = %v, want %v", choices, want)
	}
}

// A plan name the table does not know must not read as a plan without limits.
func TestOrganizations_UnknownPlanIsAnError(t *testing.T) {
	o := Organizations{PlanOf: planNamed("enterprise")}

	if p, err := o.Plan(context.Background(), project); err == nil || !strings.Contains(err.Error(), `"enterprise"`) {
		t.Errorf("Plan = %+v, %v; want an error naming the plan", p, err)
	}
	if choices, err := o.RetentionChoices(context.Background(), project); err == nil {
		t.Errorf("RetentionChoices = %v, want an error", choices)
	}
}

func TestOrganizations_LookupFailure(t *testing.T) {
	lookupErr := errors.New("logger unavailable")
	o := Organizations{PlanOf: func(context.Context, string) (string, error) { return "", lookupErr }}

	if _, err := o.Plan(context.Background(), project); !errors.Is(err, lookupErr) {
		t.Errorf("Plan error = %v, want it to wrap %v", err, lookupErr)
	}
	if _, err := o.RetentionChoices(context.Background(), project); !errors.Is(err, lookupErr) {
		t.Errorf("RetentionChoices error = %v, want it to wrap %v", err, lookupErr)
	}
}

// A self-hosted organization is on the plan that limits nothing, whatever name
// it stores, and so is one a user creates.
func TestSelfHosted_OrganizationPlans(t *testing.T) {
	for _, stored := range []string{PlanSelfHosted, PlanFree, "enterprise", ""} {
		got, err := SelfHosted{}.OrganizationPlan(stored)
		if err != nil || got != SelfHostedPlan() {
			t.Errorf("OrganizationPlan(%q) = %+v, %v; want %+v", stored, got, err, SelfHostedPlan())
		}
	}
	if got := (SelfHosted{}).NewOrganizationPlan(); got != SelfHostedPlan() {
		t.Errorf("NewOrganizationPlan = %+v, want %+v", got, SelfHostedPlan())
	}
}

// A hosted organization is on the plan it names, and starts on the free one.
func TestOrganizations_OrganizationPlans(t *testing.T) {
	o := Organizations{PlanOf: planNamed(PlanPro)}

	got, err := o.OrganizationPlan(PlanTeam)
	if want, _ := PlanByName(PlanTeam); err != nil || got != want {
		t.Errorf("OrganizationPlan(team) = %+v, %v; want %+v", got, err, want)
	}
	if p, err := o.OrganizationPlan("enterprise"); err == nil || !strings.Contains(err.Error(), `"enterprise"`) {
		t.Errorf("OrganizationPlan(enterprise) = %+v, %v; want an error naming the plan", p, err)
	}
	if got, want := o.NewOrganizationPlan(), plans[PlanFree]; got != want {
		t.Errorf("NewOrganizationPlan = %+v, want %+v", got, want)
	}
}

// A hosted project's quota is its organization's plan's monthly events, with
// what the organization has used of them.
func TestOrganizations_MonthlyQuotaIsThePlans(t *testing.T) {
	var asked []string
	month := time.Date(2026, time.October, 1, 0, 0, 0, 0, time.UTC)
	o := Organizations{QuotaOf: func(_ context.Context, projectID string) (data.ProjectQuota, error) {
		asked = append(asked, projectID)
		return data.ProjectQuota{OrganizationID: organization, Plan: PlanFree, Month: month, Events: 42}, nil
	}}

	got, err := o.MonthlyQuota(context.Background(), project)
	if err != nil {
		t.Fatalf("MonthlyQuota: %v", err)
	}
	want := Quota{OrganizationID: organization, Limit: plans[PlanFree].MonthlyEvents, Month: month, Used: 42}
	if got != want {
		t.Errorf("MonthlyQuota = %+v, want %+v", got, want)
	}
	if !slices.Equal(asked, []string{project}) {
		t.Errorf("QuotaOf asked about %v, want %v", asked, []string{project})
	}

	// An organization on the self-hosted plan has no quota under either
	// edition.
	got, err = Organizations{QuotaOf: quotaOf(PlanSelfHosted, 1<<40)}.MonthlyQuota(context.Background(), project)
	if err != nil || got.Limit != Unlimited {
		t.Errorf("MonthlyQuota on the self-hosted plan = %+v, %v; want no limit", got, err)
	}
}

// A quota that cannot be told is an error, never a quota without a limit.
func TestOrganizations_MonthlyQuotaFailures(t *testing.T) {
	if q, err := (Organizations{QuotaOf: quotaOf("enterprise", 0)}).MonthlyQuota(context.Background(), project); err == nil || !strings.Contains(err.Error(), `"enterprise"`) {
		t.Errorf("MonthlyQuota on an unknown plan = %+v, %v; want an error naming the plan", q, err)
	}

	lookupErr := errors.New("logger unavailable")
	o := Organizations{QuotaOf: func(context.Context, string) (data.ProjectQuota, error) { return data.ProjectQuota{}, lookupErr }}
	if _, err := o.MonthlyQuota(context.Background(), project); !errors.Is(err, lookupErr) {
		t.Errorf("MonthlyQuota error = %v, want it to wrap %v", err, lookupErr)
	}
}
