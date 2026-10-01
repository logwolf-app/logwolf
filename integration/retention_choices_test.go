//go:build integration

package integration

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"logwolf-toolbox/limits"
)

// TestRetentionChoices_SelfHosted checks that a broker started with the
// self-hosted edition, the default, offers a project every retention it always
// has, and stores any one of them, forever included.
func TestRetentionChoices_SelfHosted(t *testing.T) {
	if edition := limits.EditionFromEnv(); edition != limits.EditionSelfHosted {
		t.Skipf("LOGWOLF_EDITION=%s: these are the self-hosted choices", edition)
	}

	stack := sharedStack(t)
	const owner = "retention-choices-owner"

	created := mustInternalCall(t, stack.brokerURL, http.MethodPost, "/projects", owner,
		map[string]string{"name": "Retention choices", "slug": "retention-choices"}, http.StatusCreated)
	var project struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(created, &project); err != nil || project.ID == "" {
		t.Fatalf("decode created project %s: %v", created, err)
	}
	path := "/projects/" + project.ID + "/retention"

	var retention struct {
		Days    int   `json:"days"`
		Choices []int `json:"choices"`
	}
	if err := json.Unmarshal(mustInternalCall(t, stack.brokerURL, http.MethodGet, path, owner, nil, http.StatusOK), &retention); err != nil {
		t.Fatalf("decode retention: %v", err)
	}
	if retention.Days != 90 {
		t.Errorf("days = %d, want the default 90", retention.Days)
	}
	want := []int{0, 30, 60, 90, 180, 365}
	if !slices.Equal(retention.Choices, want) {
		t.Fatalf("choices = %v, want %v", retention.Choices, want)
	}

	// The logger has to store every one of them.
	for _, days := range want {
		mustInternalCall(t, stack.brokerURL, http.MethodPatch, path, owner, map[string]any{"days": days}, http.StatusOK)
	}
	mustInternalCall(t, stack.brokerURL, http.MethodPatch, path, owner, map[string]any{"days": 7}, http.StatusBadRequest)
}
