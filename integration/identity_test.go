//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"

	"logwolf-toolbox/data"
)

// End-to-end checks that the dashboard knows a person by their GitHub user ID,
// never by their login: through the broker, the logger and MongoDB of the
// shared stack, the way the dashboard drives them. A login is a name GitHub
// lets its owner change and lets someone else take afterwards; the ID is
// neither.

// dashboardUser is a signed-in dashboard user as the broker sees them: the
// GitHub user ID and login the frontend sends with every request.
type dashboardUser struct {
	id    int64
	login string
}

// renamed is the same GitHub account under a new login.
func (u dashboardUser) renamed(login string) dashboardUser { return dashboardUser{u.id, login} }

func (u dashboardUser) call(t *testing.T, stack *testStack, method, path string, body any) (int, json.RawMessage) {
	t.Helper()
	return internalCallAs(t, stack.brokerURL, method, path, u.login, u.id, body)
}

// identityUsers returns n users with GitHub IDs and logins no other test on the
// shared stack uses.
func identityUsers(n int) []dashboardUser {
	base := time.Now().UnixNano()
	users := make([]dashboardUser, n)
	for i := range users {
		users[i] = dashboardUser{base + int64(i), fmt.Sprintf("identity-%d-%d", base, i)}
	}
	return users
}

// signIn records a sign-in the way the dashboard does, which also links the
// login-only memberships under the user's current login to their ID.
func signIn(t *testing.T, stack *testStack, u dashboardUser) {
	t.Helper()

	status, raw := u.call(t, stack, http.MethodPut, "/users/me", map[string]any{})
	if status != http.StatusOK {
		t.Fatalf("sign in as %d (%s) = %d, want 200 (data: %s)", u.id, u.login, status, raw)
	}
}

// createProjectAs creates a project owned by u and returns its id.
func createProjectAs(t *testing.T, stack *testStack, u dashboardUser, name string) string {
	t.Helper()

	status, raw := u.call(t, stack, http.MethodPost, "/projects", map[string]any{"name": name, "slug": name})
	if status != http.StatusCreated {
		t.Fatalf("create project %s as %s = %d, want 201 (data: %s)", name, u.login, status, raw)
	}
	var project data.Project
	if err := json.Unmarshal(raw, &project); err != nil {
		t.Fatalf("decode project %s: %v", name, err)
	}
	return project.ID.Hex()
}

// invite adds a member the way the dashboard does once GitHub has resolved the
// login to its user ID: the body carries both.
func invite(t *testing.T, stack *testStack, owner dashboardUser, projectID string, invitee dashboardUser, role string) {
	t.Helper()

	status, raw := owner.call(t, stack, http.MethodPost, "/projects/"+projectID+"/members",
		map[string]any{"login": invitee.login, "user_id": invitee.id, "role": role})
	if status != http.StatusCreated {
		t.Fatalf("invite %s to %s = %d, want 201 (data: %s)", invitee.login, projectID, status, raw)
	}
}

// projectRoles lists u's projects through GET /projects, as project id → role.
func projectRoles(t *testing.T, stack *testStack, u dashboardUser) map[string]string {
	t.Helper()

	status, raw := u.call(t, stack, http.MethodGet, "/projects", nil)
	var projects []data.UserProject
	if err := json.Unmarshal(raw, &projects); status != http.StatusOK || err != nil {
		t.Fatalf("list projects as %s = %d, %v (data: %s)", u.login, status, err, raw)
	}
	roles := make(map[string]string, len(projects))
	for _, p := range projects {
		roles[p.ID.Hex()] = p.Role
	}
	return roles
}

// listMembers lists a project's members through the broker, as u.
func listMembers(t *testing.T, stack *testStack, u dashboardUser, projectID string) []data.ProjectMember {
	t.Helper()

	status, raw := u.call(t, stack, http.MethodGet, "/projects/"+projectID+"/members", nil)
	var members []data.ProjectMember
	if err := json.Unmarshal(raw, &members); status != http.StatusOK || err != nil {
		t.Fatalf("list members of %s as %s = %d, %v (data: %s)", projectID, u.login, status, err, raw)
	}
	return members
}

// seedLoginOnlyMembership stores a membership the way builds before user IDs
// did, a login and no user_id, which the broker no longer lets anyone add.
func seedLoginOnlyMembership(t *testing.T, stack *testStack, projectID, login, role string) primitive.ObjectID {
	t.Helper()

	pid, err := primitive.ObjectIDFromHex(projectID)
	if err != nil {
		t.Fatalf("project id %q: %v", projectID, err)
	}
	res, err := testMongo(t, stack.mongoURI).Database("logs").Collection("project_members").InsertOne(context.Background(), bson.M{
		"project_id": pid, "github_login": login, "role": role, "created_at": time.Now(),
	})
	if err != nil {
		t.Fatalf("seed login-only membership of %s: %v", login, err)
	}
	return res.InsertedID.(primitive.ObjectID)
}

// storedMembership reads a membership straight from MongoDB.
func storedMembership(t *testing.T, stack *testStack, id primitive.ObjectID) data.ProjectMember {
	t.Helper()

	var m data.ProjectMember
	err := testMongo(t, stack.mongoURI).Database("logs").Collection("project_members").
		FindOne(context.Background(), bson.M{"_id": id}).Decode(&m)
	if err != nil {
		t.Fatalf("membership %s: %v", id.Hex(), err)
	}
	return m
}

// projectRoute is one dashboard route under /projects/{id}, and the access
// level routes.go puts it behind.
type projectRoute struct {
	method, path string
	body         any
	ownerOnly    bool
}

// projectRoutes are the routes that open a project to its members, paths
// relative to /projects/{id}. Those that change something are sent with a body
// the handler refuses, or name something that does not exist, so checking
// access changes nothing; past the access check they answer 400 or 404.
var projectRoutes = []projectRoute{
	{http.MethodGet, "", nil, false},
	{http.MethodGet, "/members", nil, false},
	{http.MethodGet, "/logs", nil, false},
	{http.MethodGet, "/logs/" + primitive.NewObjectID().Hex(), nil, false},
	{http.MethodPost, "/logs", map[string]any{"name": "intruder", "data": "{}", "severity": "nope"}, false},
	{http.MethodDelete, "/logs/" + primitive.NewObjectID().Hex(), nil, false},
	{http.MethodGet, "/keys", nil, false},
	{http.MethodPost, "/keys", map[string]any{"scopes": []string{"nope"}}, false},
	{http.MethodDelete, "/keys/" + primitive.NewObjectID().Hex(), nil, false},
	{http.MethodGet, "/retention", nil, false},
	{http.MethodPatch, "/retention", map[string]any{"days": 7}, false},
	{http.MethodGet, "/metrics", nil, false},
	{http.MethodPatch, "", map[string]any{"name": ""}, true},
	{http.MethodPost, "/members", map[string]any{"login": "accomplice", "user_id": 1, "role": "nope"}, true},
	{http.MethodPatch, "/members/" + primitive.NewObjectID().Hex(), map[string]any{"role": data.RoleOwner}, true},
	{http.MethodDelete, "/members/" + primitive.NewObjectID().Hex(), nil, true},
	{http.MethodDelete, "", nil, true},
}

// assertProjectAccess checks u against every project route: a member gets
// past the access check on the member routes and an owner on all of them;
// anyone else is refused each one with a 403.
func assertProjectAccess(t *testing.T, stack *testStack, who string, u dashboardUser, projectID, role string) {
	t.Helper()

	for _, route := range projectRoutes {
		// Deleting the project would end the test; an owner's access to it is
		// what the other owner-only routes already show.
		if route.method == http.MethodDelete && route.path == "" && role == data.RoleOwner {
			continue
		}
		allowed := role == data.RoleOwner || role == data.RoleMember && !route.ownerOnly

		status, raw := u.call(t, stack, route.method, "/projects/"+projectID+route.path, route.body)
		switch {
		case allowed && (status == http.StatusForbidden || status == http.StatusUnauthorized || status >= 500):
			t.Errorf("%s: %s %s = %d, want access as %s (data: %s)", who, route.method, route.path, status, role, raw)
		case !allowed && status != http.StatusForbidden:
			t.Errorf("%s: %s %s = %d, want 403 (data: %s)", who, route.method, route.path, status, raw)
		}
	}

	roles := projectRoles(t, stack, u)
	if got, listed := roles[projectID]; got != role || role == "" && listed {
		t.Errorf("%s: GET /projects lists the project as %q (listed %v), want %q", who, got, listed, role)
	}
}

// TestIdentity_RenameKeepsAccess: a user who renames their GitHub account
// signs in under the new login and finds every project they had, as the same
// role: the ones they created, the ones they were invited to, and those
// they held before user IDs existed, once a sign-in has linked them.
func TestIdentity_RenameKeepsAccess(t *testing.T) {
	stack := sharedStack(t)
	users := identityUsers(2)
	erin, colleague := users[0], users[1]

	owned := createProjectAs(t, stack, erin, "erin-owned")
	invitedTo := createProjectAs(t, stack, colleague, "erin-invited")
	invite(t, stack, colleague, invitedTo, erin, data.RoleMember)
	legacy := createProjectAs(t, stack, colleague, "erin-legacy")
	legacyRow := seedLoginOnlyMembership(t, stack, legacy, erin.login, data.RoleOwner)

	// The last sign-in under the old login links the membership from before
	// user IDs.
	signIn(t, stack, erin)
	if got := storedMembership(t, stack, legacyRow).UserID; got != erin.id {
		t.Fatalf("login-only membership after sign-in: user_id = %d, want %d", got, erin.id)
	}

	renamed := erin.renamed(erin.login + "-renamed")
	signIn(t, stack, renamed)

	want := map[string]string{owned: data.RoleOwner, invitedTo: data.RoleMember, legacy: data.RoleOwner}
	for projectID, role := range want {
		assertProjectAccess(t, stack, "erin after a rename", renamed, projectID, role)
	}

	// The members page lists erin under the login of their last sign-in.
	for projectID := range want {
		for _, m := range listMembers(t, stack, renamed, projectID) {
			if m.UserID == erin.id && m.GithubLogin != renamed.login {
				t.Errorf("project %s lists erin as %q, want %q", projectID, m.GithubLogin, renamed.login)
			}
		}
	}

	// Renaming back is no different.
	signIn(t, stack, erin)
	for projectID, role := range want {
		if got := projectRoles(t, stack, erin)[projectID]; got != role {
			t.Errorf("erin after renaming back: project %s role %q, want %q", projectID, got, role)
		}
	}
}

// TestIdentity_AccessFollowsUserID: a membership linked to one user ID grants
// nothing to another, whatever login either signs in with: not to whoever takes
// a renamed user's old login, not even after they sign in under it, and not by
// sending a member's login with another ID. The member's ID under any login is
// that member, and only that member: never more than their role.
func TestIdentity_AccessFollowsUserID(t *testing.T) {
	stack := sharedStack(t)
	users := identityUsers(4)
	owner, member, legacyMember, impostor := users[0], users[1], users[2], users[3]

	projectID := createProjectAs(t, stack, owner, "by-user-id")
	invite(t, stack, owner, projectID, member, data.RoleMember)
	legacyRow := seedLoginOnlyMembership(t, stack, projectID, legacyMember.login, data.RoleMember)
	signIn(t, stack, legacyMember)

	// The impostor signs in under each login in turn, the way someone who took
	// it after a rename would, and is still nobody here.
	for _, login := range []string{member.login, legacyMember.login, owner.login} {
		as := impostor.renamed(login)
		signIn(t, stack, as)
		assertProjectAccess(t, stack, "another account with "+login, as, projectID, "")
	}

	// Signing in under their login linked nothing to the impostor.
	if got := storedMembership(t, stack, legacyRow).UserID; got != legacyMember.id {
		t.Errorf("legacy member's membership: user_id = %d, want %d", got, legacyMember.id)
	}
	for _, m := range listMembers(t, stack, owner, projectID) {
		if m.UserID == impostor.id {
			t.Errorf("the impostor holds membership %+v", m)
		}
	}

	// Each ID is its own role, whatever login comes with it: a member who sends
	// the owner's login is still a member.
	assertProjectAccess(t, stack, "the member", member, projectID, data.RoleMember)
	assertProjectAccess(t, stack, "the member under the owner's login", member.renamed(owner.login), projectID, data.RoleMember)
	assertProjectAccess(t, stack, "the linked legacy member under a new login", legacyMember.renamed("legacy-renamed"), projectID, data.RoleMember)
	assertProjectAccess(t, stack, "the owner under the member's login", owner.renamed(member.login), projectID, data.RoleOwner)

	// A login without an ID is nobody at all.
	if status, raw := internalCallAs(t, stack.brokerURL, http.MethodGet, "/projects/"+projectID, member.login, 0, nil); status != http.StatusUnauthorized {
		t.Errorf("the member's login without an ID = %d, want 401 (data: %s)", status, raw)
	}
}

// TestIdentity_InvitesResolveLoginAtInviteTime: an invite names a login, which
// the dashboard resolves to its GitHub user ID before calling the broker, and
// the membership belongs to that ID from the start. The invitee need not sign
// in first and need not keep the login; whoever holds it later gets nothing.
// An invite that comes without the ID is refused, so no new membership is ever
// left to be matched by login.
func TestIdentity_InvitesResolveLoginAtInviteTime(t *testing.T) {
	stack := sharedStack(t)
	users := identityUsers(3)
	owner, invitee, successor := users[0], users[1], users[2]

	projectID := createProjectAs(t, stack, owner, "invites")
	membersPath := "/projects/" + projectID + "/members"

	// An invite that was never resolved names nobody.
	for _, body := range []map[string]any{
		{"login": invitee.login, "role": data.RoleMember},
		{"login": invitee.login, "user_id": 0, "role": data.RoleMember},
		{"login": invitee.login, "user_id": -invitee.id, "role": data.RoleMember},
	} {
		if status, raw := owner.call(t, stack, http.MethodPost, membersPath, body); status != http.StatusBadRequest {
			t.Errorf("invite %v = %d, want 400 (data: %s)", body, status, raw)
		}
	}
	if members := listMembers(t, stack, owner, projectID); len(members) != 1 {
		t.Fatalf("members after refused invites = %+v, want the owner alone", members)
	}

	// GitHub's casing, as checkInvitee returns it.
	invite(t, stack, owner, projectID, invitee.renamed("Invitee-"+invitee.login), data.RoleMember)
	invitee = invitee.renamed("invitee-" + invitee.login)

	// The membership is the invitee's at once, before they ever sign in.
	var row data.ProjectMember
	for _, m := range listMembers(t, stack, owner, projectID) {
		if m.UserID == invitee.id {
			row = m
		}
	}
	if row.ID.IsZero() || row.GithubLogin != invitee.login || row.Role != data.RoleMember {
		t.Fatalf("invitee's membership = %+v, want user %d as %s, a member", row, invitee.id, invitee.login)
	}

	// The invitee renames before their first sign-in, and someone else takes
	// the login they were invited under and signs in with it first.
	took := successor.renamed(invitee.login)
	signIn(t, stack, took)
	assertProjectAccess(t, stack, "whoever took the invited login", took, projectID, "")

	renamed := invitee.renamed(invitee.login + "-renamed")
	signIn(t, stack, renamed)
	assertProjectAccess(t, stack, "the invitee under a new login", renamed, projectID, data.RoleMember)

	if got := storedMembership(t, stack, row.ID); got.UserID != invitee.id {
		t.Errorf("invitee's membership after sign-ins: user_id = %d, want %d", got.UserID, invitee.id)
	}

	// One person is one membership: inviting the invitee again under their new
	// login is a duplicate, not a second membership.
	status, raw := owner.call(t, stack, http.MethodPost, membersPath,
		map[string]any{"login": renamed.login, "user_id": invitee.id, "role": data.RoleOwner})
	if status != http.StatusConflict {
		t.Errorf("invite the invitee again under a new login = %d, want 409 (data: %s)", status, raw)
	}
	if members := listMembers(t, stack, owner, projectID); len(members) != 2 {
		t.Errorf("members = %+v, want the owner and the invitee", members)
	}
}
