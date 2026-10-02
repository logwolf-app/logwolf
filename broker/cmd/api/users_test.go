package main

import (
	"net/http"
	"strconv"
	"testing"

	"logwolf-toolbox/data"
)

// signInRequest is the dashboard's PUT /users/me as the user with this login
// and GitHub user ID; an id of 0 leaves X-User-ID out.
func signInRequest(login string, githubID int64, body any) *http.Request {
	r := internalRequest(http.MethodPut, "/users/me", login, body)
	r.Header.Del("X-User-ID")
	if githubID != 0 {
		r.Header.Set("X-User-ID", strconv.FormatInt(githubID, 10))
	}
	return r
}

// TestUpsertCurrentUser_RecordsTheSignIn: the GitHub ID and login come from
// X-User-ID and X-User-Login, the email from the body, and the user comes back
// as stored.
func TestUpsertCurrentUser_RecordsTheSignIn(t *testing.T) {
	h, f := newInternalTestServer(t)

	w := do(h, signInRequest("Octocat", 583231, map[string]any{"email": "octo@example.com"}))
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

	first := decodeData[data.User](t, do(h, signInRequest("octocat", 583231, map[string]any{})))
	w := do(h, signInRequest("octocat-renamed", 583231, map[string]any{}))
	if w.Code != http.StatusOK {
		t.Fatalf("PUT /users/me after a rename = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}

	renamed := decodeData[data.User](t, w)
	if renamed.ID != first.ID || renamed.GithubLogin != "octocat-renamed" {
		t.Errorf("after a rename got %+v, want user %s with the new login", renamed, first.ID.Hex())
	}
}

// TestUpsertCurrentUser_TakesTheIDFromTheHeader: like every dashboard route, it
// takes who the caller is from X-User-ID and X-User-Login. Without a usable ID
// it is refused before the logger; an ID in the body names nobody.
func TestUpsertCurrentUser_TakesTheIDFromTheHeader(t *testing.T) {
	h, f := newInternalTestServer(t)

	for _, id := range []int64{0, -1} {
		if w := do(h, signInRequest("octocat", id, map[string]any{"github_id": 583231})); w.Code != http.StatusUnauthorized {
			t.Errorf("PUT /users/me with X-User-ID %d = %d, want 401", id, w.Code)
		}
	}
	w := do(h, internalRequest(http.MethodPut, "/users/me", "", map[string]any{"github_id": 583231, "login": "octocat"}))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("PUT /users/me without X-User-Login = %d, want 401", w.Code)
	}
	f.snapshot(func(f *fakeLogger) {
		if len(f.upsertedUsers) != 0 {
			t.Errorf("forwarded %+v, want no logger call", f.upsertedUsers)
		}
	})

	w = do(h, signInRequest("octocat", 583231, map[string]any{"github_id": 1}))
	if user := decodeData[data.User](t, w); w.Code != http.StatusOK || user.GithubID != 583231 {
		t.Errorf("PUT /users/me with another ID in the body = %d, user %+v; want 200 for 583231", w.Code, user)
	}
}
