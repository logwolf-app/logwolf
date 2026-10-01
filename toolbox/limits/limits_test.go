package limits

import (
	"context"
	"maps"
	"slices"
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
	p, err := ForEdition(EditionSelfHosted)
	if err != nil {
		t.Fatalf("ForEdition(selfhosted): %v", err)
	}
	if _, ok := p.(SelfHosted); !ok {
		t.Errorf("ForEdition(selfhosted) = %T, want SelfHosted", p)
	}

	// Running a cloud deployment on self-hosted limits would give every
	// project everything, so an edition without a provider is an error.
	for _, edition := range []string{EditionCloud, "enterprise"} {
		if p, err := ForEdition(edition); err == nil {
			t.Errorf("ForEdition(%q) = %T, want an error", edition, p)
		}
	}
}

func TestFromEnv_DefaultsToSelfHosted(t *testing.T) {
	t.Setenv("LOGWOLF_EDITION", "")
	p, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if _, ok := p.(SelfHosted); !ok {
		t.Errorf("FromEnv() = %T, want SelfHosted", p)
	}
}
