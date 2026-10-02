package main

import (
	"net/http"
	"testing"

	"logwolf-toolbox/data"
)

// TestUpsertCurrentUser_RecordsTheSignIn: the dashboard sends the GitHub ID and
// email, the login comes from X-User-Login, and the user comes back as stored.
func TestUpsertCurrentUser_RecordsTheSignIn(t *testing.T) {
	h, f := newInternalTestServer(t)

	w := do(h, internalRequest(http.MethodPut, "/users/me", "Octocat", map[string]any{"github_id": 583231, "email": "octo@example.com"}))
	if w.Code != http.StatusOK {
		t.Fatalf("PUT /users/me = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}

	user := decodeData[data.User](t, w)
	if user.GithubID != 583231 || user.GithubLogin != "octocat" || user.Email != "octo@example.com" || user.ID.IsZero() {
		t.Errorf("reply = %+v, want the stored user, login normalized", user)
	}
	f.snapshot(func(f *fakeLogger) {
		want := data.RPCUpsertUserArgs{GithubID: 583231, GithubLogin: "octocat", Email: "octo@example.com"}
		if len(f.upsertedUsers) != 1 || f.upsertedUsers[0] != want {
			t.Errorf("forwarded %+v, want one call with %+v", f.upsertedUsers, want)
		}
	})
}

// TestUpsertCurrentUser_FollowsARename: the same GitHub ID signing in under a
// new login is the same user, with the new login.
func TestUpsertCurrentUser_FollowsARename(t *testing.T) {
	h, _ := newInternalTestServer(t)

	first := decodeData[data.User](t, do(h, internalRequest(http.MethodPut, "/users/me", "octocat", map[string]any{"github_id": 583231})))
	w := do(h, internalRequest(http.MethodPut, "/users/me", "octocat-renamed", map[string]any{"github_id": 583231}))
	if w.Code != http.StatusOK {
		t.Fatalf("PUT /users/me after a rename = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}

	renamed := decodeData[data.User](t, w)
	if renamed.ID != first.ID || renamed.GithubLogin != "octocat-renamed" {
		t.Errorf("after a rename got %+v, want user %s with the new login", renamed, first.ID.Hex())
	}
}

// TestUpsertCurrentUser_RefusesAUserWithoutAGithubID: a user is keyed by GitHub
// ID, so the broker refuses one without a usable ID before calling the logger.
func TestUpsertCurrentUser_RefusesAUserWithoutAGithubID(t *testing.T) {
	h, f := newInternalTestServer(t)

	for _, body := range []map[string]any{{}, {"github_id": 0}, {"github_id": -1}, {"email": "octo@example.com"}} {
		if w := do(h, internalRequest(http.MethodPut, "/users/me", "octocat", body)); w.Code != http.StatusBadRequest {
			t.Errorf("PUT /users/me with %v = %d, want 400", body, w.Code)
		}
	}
	if w := do(h, internalRequest(http.MethodPut, "/users/me", "octocat", map[string]any{"github_id": "583231"})); w.Code != http.StatusBadRequest {
		t.Errorf("PUT /users/me with a string id = %d, want 400", w.Code)
	}
	f.snapshot(func(f *fakeLogger) {
		if len(f.upsertedUsers) != 0 {
			t.Errorf("forwarded %+v, want no logger call", f.upsertedUsers)
		}
	})
}

// TestUpsertCurrentUser_NeedsASignedInLogin: like every dashboard route, it
// takes the login from X-User-Login, never from the body.
func TestUpsertCurrentUser_NeedsASignedInLogin(t *testing.T) {
	h, f := newInternalTestServer(t)

	w := do(h, internalRequest(http.MethodPut, "/users/me", "", map[string]any{"github_id": 583231, "login": "octocat"}))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("PUT /users/me without X-User-Login = %d, want 401", w.Code)
	}
	f.snapshot(func(f *fakeLogger) {
		if len(f.upsertedUsers) != 0 {
			t.Errorf("forwarded %+v, want no logger call", f.upsertedUsers)
		}
	})
}
