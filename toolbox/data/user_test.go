package data

import (
	"errors"
	"testing"
)

// TestUpsertUser_RefusesAnUnkeyedUser: a user is keyed by their GitHub ID, so
// one without it, or without a login to show, is refused before any query. The
// zero-value Models has no database, so a call that got past the check would
// panic instead of answering.
func TestUpsertUser_RefusesAnUnkeyedUser(t *testing.T) {
	var m Models

	cases := []struct {
		name  string
		id    int64
		login string
	}{
		{"zero id", 0, "jdoe"},
		{"negative id", -1, "jdoe"},
		{"empty login", 42, ""},
		{"blank login", 42, "   "},
	}
	for _, tc := range cases {
		user, err := m.UpsertUser(tc.id, tc.login, "jdoe@example.com")
		if !errors.Is(err, ErrInvalidUser) {
			t.Errorf("%s: UpsertUser(%d, %q) = %v, want ErrInvalidUser", tc.name, tc.id, tc.login, err)
		}
		if user != nil {
			t.Errorf("%s: UpsertUser returned a user alongside the error: %+v", tc.name, user)
		}
	}
}
