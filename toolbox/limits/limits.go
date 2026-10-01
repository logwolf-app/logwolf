// Package limits holds what a deployment lets a project do beyond Logwolf's own
// rules: how many events it may ingest and which retention it may pick.
//
// A self-hosted install has no plans, so its Provider allows everything Logwolf
// supports. The hosted edition answers from each project's plan instead. Which
// one a service runs with is picked by LOGWOLF_EDITION (see FromEnv).
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
	// AllowIngest reports whether the project may ingest n more events.
	AllowIngest(ctx context.Context, projectID string, n int) (bool, error)

	// RetentionChoices lists the retention values, in days, the project may
	// pick, in the order the dashboard shows them. 0 is forever. Every value
	// must be one of data.ValidRetentionDays, the only ones the logger stores.
	RetentionChoices(ctx context.Context, projectID string) ([]int, error)
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

// ForEdition returns the Provider of an edition. The cloud edition has none in
// this build yet, so it is refused rather than run with self-hosted limits, as
// is any name it does not know.
func ForEdition(edition string) (Provider, error) {
	switch edition {
	case EditionSelfHosted:
		return SelfHosted{}, nil
	case EditionCloud:
		return nil, fmt.Errorf("LOGWOLF_EDITION=%s: the cloud edition is not available in this build", edition)
	default:
		return nil, fmt.Errorf("LOGWOLF_EDITION=%s: unknown edition, want %s or %s", edition, EditionSelfHosted, EditionCloud)
	}
}

// FromEnv is ForEdition(EditionFromEnv()).
func FromEnv() (Provider, error) {
	return ForEdition(EditionFromEnv())
}

// selfHostedRetention is every retention Logwolf supports, forever first, as
// the dashboard has always listed them.
var selfHostedRetention = []int{0, 30, 60, 90, 180, 365}

// SelfHosted is the Provider of a self-hosted install: no limits at all.
type SelfHosted struct{}

// AllowIngest always allows: a self-hosted install has no quota.
func (SelfHosted) AllowIngest(context.Context, string, int) (bool, error) {
	return true, nil
}

// RetentionChoices offers every supported retention to every project.
func (SelfHosted) RetentionChoices(context.Context, string) ([]int, error) {
	return append([]int(nil), selfHostedRetention...), nil
}
