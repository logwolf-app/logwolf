package limits

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"logwolf-toolbox/data"
)

const project = "aaaaaaaaaaaaaaaaaaaaaa01"

func TestSelfHosted_AllowsAnyIngest(t *testing.T) {
	for _, n := range []int{0, 1, 1000, 1 << 30} {
		ok, err := SelfHosted{}.AllowIngest(context.Background(), project, n)
		if err != nil || !ok {
			t.Errorf("AllowIngest(%d) = %v, %v; want true, nil", n, ok, err)
		}
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
	p, err := ForEdition(EditionSelfHosted, nil)
	if err != nil {
		t.Fatalf("ForEdition(selfhosted): %v", err)
	}
	if _, ok := p.(SelfHosted); !ok {
		t.Errorf("ForEdition(selfhosted) = %T, want SelfHosted", p)
	}

	p, err = ForEdition(EditionCloud, planNamed(PlanFree))
	if err != nil {
		t.Fatalf("ForEdition(cloud): %v", err)
	}
	if _, ok := p.(Organizations); !ok {
		t.Errorf("ForEdition(cloud) = %T, want Organizations", p)
	}

	// Running a cloud deployment on self-hosted limits would give every
	// project everything, so cloud without a way to find a project's plan is
	// an error, as is an edition with no provider at all.
	if p, err := ForEdition(EditionCloud, nil); err == nil {
		t.Errorf("ForEdition(cloud, nil) = %T, want an error", p)
	}
	if p, err := ForEdition("enterprise", planNamed(PlanFree)); err == nil {
		t.Errorf("ForEdition(enterprise) = %T, want an error", p)
	}
}

func TestFromEnv_DefaultsToSelfHosted(t *testing.T) {
	t.Setenv("LOGWOLF_EDITION", "")
	p, err := FromEnv(nil)
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
