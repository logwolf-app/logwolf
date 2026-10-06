package main

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"logwolf-toolbox/data"
	"logwolf-toolbox/limits"
)

// Organization ids are ObjectID hex, like project ids.
const (
	orgAcme    = "0000000000000000000000a1"
	orgGlobex  = "0000000000000000000000b2"
	orgMissing = "0000000000000000000000c3"
)

// newOrganizationTestServer is newInternalTestServer with two organizations:
// Acme (org-owner, org-admin, org-member), which holds project alpha, and
// Globex (owner-b), which holds beta. org-owner is no member of alpha itself.
func newOrganizationTestServer(t *testing.T, lim limits.Provider) (http.Handler, *fakeLogger) {
	t.Helper()
	_, f := newInternalTestServer(t)
	f.addOrganization(orgAcme, "Acme", limits.PlanSelfHosted)
	f.addOrganization(orgGlobex, "Globex", limits.PlanSelfHosted)
	f.addOrgMember(orgAcme, "org-owner", data.RoleOwner)
	f.addOrgMember(orgAcme, "org-admin", data.RoleAdmin)
	f.addOrgMember(orgAcme, "org-member", data.RoleMember)
	f.addOrgMember(orgGlobex, "owner-b", data.RoleOwner)
	f.putInOrganization(projAlpha, orgAcme)
	f.putInOrganization(projBeta, orgGlobex)
	return (&Config{Limits: lim}).routes(), f
}

func orgPath(suffix string) func(string) string {
	return func(id string) string { return "/organizations/" + id + suffix }
}

// orgMemberTarget is the route of login's membership of the organization.
func orgMemberTarget(login string) func(string) string {
	return func(id string) string { return "/organizations/" + id + "/members/" + testMemberID(id, login) }
}

// organizationRoutes lists every dashboard route that acts on one
// organization, with a body its handler accepts. owner, here, marks the routes
// an admin or owner needs.
var organizationRoutes = []projectRoute{
	{http.MethodGet, orgPath(""), noBody, false},
	{http.MethodPatch, orgPath(""), constBody(map[string]string{"name": "Renamed"}), true},
	{http.MethodPost, orgPath("/projects"), constBody(map[string]string{"name": "Fresh", "slug": "fresh"}), false},
	{http.MethodGet, orgPath("/plan"), noBody, false},
	{http.MethodGet, orgPath("/usage"), noBody, true},
	{http.MethodGet, orgPath("/members"), noBody, false},
	{http.MethodPost, orgPath("/members"), constBody(map[string]any{"login": "newcomer", "user_id": testUserID("newcomer"), "role": data.RoleMember}), true},
	{http.MethodPatch, orgMemberTarget("org-member"), constBody(map[string]string{"role": data.RoleAdmin}), true},
	{http.MethodDelete, orgMemberTarget("org-member"), noBody, true},
}

// TestOrganizationAccess_SameAnswerOnEveryRoute: every route that acts on an
// organization denies the same way, and lets in whoever has the role.
func TestOrganizationAccess_SameAnswerOnEveryRoute(t *testing.T) {
	cases := []struct {
		name      string
		orgID     string
		user      string
		want      int
		adminOnly bool // only check the routes an admin needs
	}{
		{"organization that does not exist", orgMissing, "org-owner", http.StatusNotFound, false},
		{"id that is not an ObjectID", "not-an-id", "org-owner", http.StatusNotFound, false},
		{"outsider", orgAcme, "owner-b", http.StatusForbidden, false},
		{"member on an admin route", orgAcme, "org-member", http.StatusForbidden, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newOrganizationTestServer(t, nil)
			for _, route := range organizationRoutes {
				if tc.adminOnly && !route.owner {
					continue
				}
				target := route.target(tc.orgID)
				w := do(h, internalRequest(route.method, target, tc.user, route.body(tc.orgID)))
				if w.Code != tc.want {
					t.Errorf("%s %s as %s = %d, want %d (body: %s)", route.method, target, tc.user, w.Code, tc.want, w.Body.String())
				}
			}
		})
	}

	// Admins and owners get through every route; a fresh server each, since
	// the routes change the members.
	for _, user := range []string{"org-admin", "org-owner"} {
		for _, route := range organizationRoutes {
			h, _ := newOrganizationTestServer(t, nil)
			target := route.target(orgAcme)
			w := do(h, internalRequest(route.method, target, user, route.body(orgAcme)))
			if w.Code >= 300 {
				t.Errorf("%s %s as %s = %d, want success (body: %s)", route.method, target, user, w.Code, w.Body.String())
			}
		}
	}
}

func TestOrganizationAccess_OneLookupPerRequest(t *testing.T) {
	h, f := newOrganizationTestServer(t, nil)

	for _, user := range []string{"org-member", "owner-b"} {
		for _, target := range []string{"/organizations/" + orgAcme, "/organizations/" + orgAcme + "/members"} {
			f.snapshot(func(f *fakeLogger) { f.orgAccessChecks = 0 })
			do(h, internalRequest(http.MethodGet, target, user, nil))
			f.snapshot(func(f *fakeLogger) {
				if f.orgAccessChecks != 1 {
					t.Errorf("GET %s as %s made %d OrganizationAccess calls, want 1", target, user, f.orgAccessChecks)
				}
			})
		}
	}
}

func TestOrganizationAccess_MalformedPathIDSkipsTheLogger(t *testing.T) {
	h, f := newOrganizationTestServer(t, nil)

	w := do(h, internalRequest(http.MethodGet, "/organizations/not-an-id", "org-owner", nil))
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "organization not found") {
		t.Errorf("GET /organizations/not-an-id = %d %s, want 404 organization not found", w.Code, w.Body.String())
	}
	f.snapshot(func(f *fakeLogger) {
		if f.orgAccessChecks != 0 {
			t.Errorf("made %d OrganizationAccess calls for a malformed id, want 0", f.orgAccessChecks)
		}
	})
}

func TestOrganizationRoleMeets(t *testing.T) {
	cases := []struct {
		role string
		need accessLevel
		want bool
	}{
		{data.RoleMember, anyMember, true},
		{data.RoleAdmin, anyMember, true},
		{data.RoleOwner, anyMember, true},
		{"", anyMember, false},
		{"viewer", anyMember, false},
		{data.RoleMember, adminOnly, false},
		{data.RoleAdmin, adminOnly, true},
		{data.RoleOwner, adminOnly, true},
		{data.RoleMember, ownerOnly, false},
		{data.RoleAdmin, ownerOnly, false},
		{data.RoleOwner, ownerOnly, true},
	}
	for _, tc := range cases {
		if got := organizationRoleMeets(tc.role, tc.need); got != tc.want {
			t.Errorf("organizationRoleMeets(%q, %d) = %v, want %v", tc.role, tc.need, got, tc.want)
		}
	}
}

// --- Organizations ---

func TestListOrganizations_ScopedToCaller(t *testing.T) {
	h, _ := newOrganizationTestServer(t, nil)

	w := do(h, internalRequest(http.MethodGet, "/organizations", "org-admin", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	orgs := decodeData[[]data.UserOrganization](t, w)
	if len(orgs) != 1 || orgs[0].ID.Hex() != orgAcme || orgs[0].Role != data.RoleAdmin {
		t.Errorf("org-admin's organizations = %+v, want Acme alone, as admin", orgs)
	}

	w = do(h, internalRequest(http.MethodGet, "/organizations", "nobody", nil))
	if got := decodeData[[]data.UserOrganization](t, w); w.Code != http.StatusOK || got == nil || len(got) != 0 {
		t.Errorf("nobody's organizations = %d %s, want 200 and an empty list", w.Code, w.Body.String())
	}
}

// A new organization belongs to its creator, on the plan the edition starts
// organizations on, whatever the body says.
func TestCreateOrganization_OwnedByCallerOnTheEditionsPlan(t *testing.T) {
	cloud := limits.Organizations{PlanOf: func(context.Context, string) (string, error) { return limits.PlanFree, nil }}
	for _, tc := range []struct {
		name string
		lim  limits.Provider
		plan string
	}{
		{"self-hosted", nil, limits.PlanSelfHosted},
		{"cloud", cloud, limits.PlanFree},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, f := newOrganizationTestServer(t, tc.lim)

			w := do(h, internalRequest(http.MethodPost, "/organizations", "newcomer",
				map[string]string{"name": "  Initech  ", "plan": limits.PlanTeam}))
			if w.Code != http.StatusCreated {
				t.Fatalf("got %d, want 201 (body: %s)", w.Code, w.Body.String())
			}
			got := decodeData[data.UserOrganization](t, w)
			if got.Name != "Initech" || got.Plan != tc.plan || got.Role != data.RoleOwner {
				t.Errorf("created %+v, want Initech on %s, the caller its owner", got, tc.plan)
			}
			f.snapshot(func(f *fakeLogger) {
				want := data.RPCCreateOrganizationArgs{Name: "Initech", Plan: tc.plan, OwnerID: testUserID("newcomer"), Owner: "newcomer"}
				if len(f.createdOrgs) != 1 || f.createdOrgs[0] != want {
					t.Errorf("CreateOrganization calls = %+v, want one with %+v", f.createdOrgs, want)
				}
			})
		})
	}
}

func TestCreateOrganization_RequiresAName(t *testing.T) {
	h, f := newOrganizationTestServer(t, nil)

	for _, name := range []string{"", "   "} {
		w := do(h, internalRequest(http.MethodPost, "/organizations", "newcomer", map[string]string{"name": name}))
		if w.Code != http.StatusBadRequest {
			t.Errorf("name %q: got %d, want 400 (body: %s)", name, w.Code, w.Body.String())
		}
	}
	f.snapshot(func(f *fakeLogger) {
		if len(f.createdOrgs) != 0 {
			t.Errorf("forwarded %d CreateOrganization calls, want none", len(f.createdOrgs))
		}
	})
}

func TestGetOrganization_CarriesTheCallersRole(t *testing.T) {
	h, _ := newOrganizationTestServer(t, nil)

	w := do(h, internalRequest(http.MethodGet, "/organizations/"+strings.ToUpper(orgAcme), "org-member", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	if got := decodeData[data.UserOrganization](t, w); got.ID.Hex() != orgAcme || got.Name != "Acme" || got.Role != data.RoleMember {
		t.Errorf("got %+v, want Acme with role member", got)
	}
}

func TestUpdateOrganization_Renames(t *testing.T) {
	h, f := newOrganizationTestServer(t, nil)

	w := do(h, internalRequest(http.MethodPatch, "/organizations/"+orgAcme, "org-admin", map[string]string{"name": " Acme Corp "}))
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	if got := decodeData[data.UserOrganization](t, w); got.Name != "Acme Corp" || got.Role != data.RoleAdmin {
		t.Errorf("got %+v, want Acme Corp with role admin", got)
	}

	w = do(h, internalRequest(http.MethodPatch, "/organizations/"+orgAcme, "org-admin", map[string]string{"name": " "}))
	if w.Code != http.StatusBadRequest {
		t.Errorf("blank name: got %d, want 400 (body: %s)", w.Code, w.Body.String())
	}
	f.snapshot(func(f *fakeLogger) {
		if len(f.updatedOrgs) != 1 {
			t.Errorf("forwarded %d UpdateOrganization calls, want 1", len(f.updatedOrgs))
		}
	})
}

// --- Plan and usage ---

type planData struct {
	Plan  planResponse           `json:"plan"`
	Usage data.OrganizationUsage `json:"usage"`
}

func TestGetOrganizationPlan_SelfHostedLimitsNothing(t *testing.T) {
	h, _ := newOrganizationTestServer(t, nil)

	w := do(h, internalRequest(http.MethodGet, "/organizations/"+orgAcme+"/plan", "org-member", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	got := decodeData[planData](t, w)
	if want := planResponseOf(limits.SelfHostedPlan()); got.Plan != want {
		t.Errorf("plan = %+v, want %+v", got.Plan, want)
	}
	if want := (data.OrganizationUsage{Projects: 1, Members: 3}); got.Usage != want {
		t.Errorf("usage = %+v, want %+v", got.Usage, want)
	}
}

func TestGetOrganizationPlan_CloudResolvesTheStoredPlan(t *testing.T) {
	cloud := limits.Organizations{PlanOf: func(context.Context, string) (string, error) { return "", nil }}
	h, f := newOrganizationTestServer(t, cloud)
	f.addOrganization(orgAcme, "Acme", limits.PlanPro)

	w := do(h, internalRequest(http.MethodGet, "/organizations/"+orgAcme+"/plan", "org-member", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	pro, _ := limits.PlanByName(limits.PlanPro)
	if got := decodeData[planData](t, w).Plan; got != planResponseOf(pro) {
		t.Errorf("plan = %+v, want %+v", got, planResponseOf(pro))
	}

	// A plan the table does not know is an error, never a plan without limits.
	f.addOrganization(orgAcme, "Acme", "enterprise")
	w = do(h, internalRequest(http.MethodGet, "/organizations/"+orgAcme+"/plan", "org-member", nil))
	if w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), "max_projects") {
		t.Errorf("unknown plan: got %d %s, want 500 and no limits", w.Code, w.Body.String())
	}
}

// --- Members ---

func TestAddOrganizationMember(t *testing.T) {
	h, f := newOrganizationTestServer(t, nil)
	add := func(user, login, role string) *http.Response {
		w := do(h, internalRequest(http.MethodPost, "/organizations/"+orgAcme+"/members", user,
			map[string]any{"login": login, "user_id": testUserID(login), "role": role}))
		return w.Result()
	}

	if got := add("org-admin", "New-Hire", data.RoleAdmin).StatusCode; got != http.StatusCreated {
		t.Errorf("admin adds an admin: got %d, want 201", got)
	}
	if got := add("org-admin", "new-hire", data.RoleMember).StatusCode; got != http.StatusConflict {
		t.Errorf("adding a member twice: got %d, want 409", got)
	}
	if got := add("org-admin", "boss", data.RoleOwner).StatusCode; got != http.StatusForbidden {
		t.Errorf("admin adds an owner: got %d, want 403", got)
	}
	if got := add("org-owner", "boss", data.RoleOwner).StatusCode; got != http.StatusCreated {
		t.Errorf("owner adds an owner: got %d, want 201", got)
	}
	if got := add("org-owner", "someone", "viewer").StatusCode; got != http.StatusBadRequest {
		t.Errorf("invalid role: got %d, want 400", got)
	}

	f.snapshot(func(f *fakeLogger) {
		// The admin's owner was refused before the logger; the bad role too.
		if len(f.addedOrgMembers) != 3 {
			t.Fatalf("forwarded %d AddOrganizationMember calls, want 3: %+v", len(f.addedOrgMembers), f.addedOrgMembers)
		}
		want := data.RPCAddOrganizationMemberArgs{OrganizationID: orgAcme, UserID: testUserID("new-hire"), GithubLogin: "new-hire", Role: data.RoleAdmin}
		if f.addedOrgMembers[0] != want {
			t.Errorf("first add = %+v, want %+v", f.addedOrgMembers[0], want)
		}
	})
}

func TestAddOrganizationMember_RequiresAUserID(t *testing.T) {
	h, _ := newOrganizationTestServer(t, nil)

	for _, body := range []map[string]any{
		{"login": "someone", "role": data.RoleMember},
		{"login": "someone", "user_id": -4, "role": data.RoleMember},
		{"login": " ", "user_id": 7, "role": data.RoleMember},
	} {
		w := do(h, internalRequest(http.MethodPost, "/organizations/"+orgAcme+"/members", "org-owner", body))
		if w.Code != http.StatusBadRequest {
			t.Errorf("%v: got %d, want 400 (body: %s)", body, w.Code, w.Body.String())
		}
	}
}

// Admins manage members, but only owners touch owners; the logger is the one
// to tell, since it reads the member's role in the same transaction.
func TestOrganizationMemberChanges_OnlyOwnersTouchOwners(t *testing.T) {
	cases := []struct {
		name   string
		user   string
		method string
		target string
		body   any
		want   int
	}{
		{"admin promotes a member to admin", "org-admin", http.MethodPatch, "org-member", map[string]string{"role": data.RoleAdmin}, http.StatusOK},
		{"admin promotes a member to owner", "org-admin", http.MethodPatch, "org-member", map[string]string{"role": data.RoleOwner}, http.StatusForbidden},
		{"admin demotes the owner", "org-admin", http.MethodPatch, "org-owner", map[string]string{"role": data.RoleMember}, http.StatusForbidden},
		{"admin removes the owner", "org-admin", http.MethodDelete, "org-owner", nil, http.StatusForbidden},
		{"admin removes a member", "org-admin", http.MethodDelete, "org-member", nil, http.StatusOK},
		{"owner promotes a member to owner", "org-owner", http.MethodPatch, "org-member", map[string]string{"role": data.RoleOwner}, http.StatusOK},
		{"owner demotes themselves, the last owner", "org-owner", http.MethodPatch, "org-owner", map[string]string{"role": data.RoleAdmin}, http.StatusBadRequest},
		{"owner removes themselves, the last owner", "org-owner", http.MethodDelete, "org-owner", nil, http.StatusBadRequest},
		{"owner removes an admin", "org-owner", http.MethodDelete, "org-admin", nil, http.StatusOK},
		{"member of another organization", "org-owner", http.MethodDelete, "owner-b", nil, http.StatusNotFound},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, f := newOrganizationTestServer(t, nil)
			target := "/organizations/" + orgAcme + "/members/" + testMemberID(orgAcme, tc.target)
			if tc.target == "owner-b" {
				target = "/organizations/" + orgAcme + "/members/" + testMemberID(orgGlobex, tc.target)
			}

			w := do(h, internalRequest(tc.method, target, tc.user, tc.body))
			if w.Code != tc.want {
				t.Fatalf("got %d, want %d (body: %s)", w.Code, tc.want, w.Body.String())
			}

			// The actor's role travels with the change, from the access check,
			// never from the request.
			f.snapshot(func(f *fakeLogger) {
				wantRole := data.RoleAdmin
				if tc.user == "org-owner" {
					wantRole = data.RoleOwner
				}
				for _, c := range f.orgRoleChanges {
					if c.ActorRole != wantRole || c.OrganizationID != orgAcme {
						t.Errorf("UpdateOrganizationMemberRole forwarded %+v, want actor %s in %s", c, wantRole, orgAcme)
					}
				}
				for _, c := range f.removedOrgMembers {
					if c.ActorRole != wantRole || c.OrganizationID != orgAcme {
						t.Errorf("RemoveOrganizationMember forwarded %+v, want actor %s in %s", c, wantRole, orgAcme)
					}
				}
			})
		})
	}
}

// --- No organization id outside the path ---

// An organization id in the query or the body changes nothing: the route acts
// on the organization in its path, the one the caller was checked against.
func TestOrganizationRoutes_TakeTheOrganizationFromThePath(t *testing.T) {
	h, f := newOrganizationTestServer(t, nil)

	w := do(h, internalRequest(http.MethodPost, "/organizations/"+orgAcme+"/members?organization_id="+orgGlobex, "org-owner",
		map[string]any{"login": "newcomer", "user_id": testUserID("newcomer"), "role": data.RoleMember, "organization_id": orgGlobex}))
	if w.Code != http.StatusCreated {
		t.Fatalf("got %d, want 201 (body: %s)", w.Code, w.Body.String())
	}
	w = do(h, internalRequest(http.MethodPatch, "/organizations/"+orgAcme+"?id="+orgGlobex, "org-owner",
		map[string]string{"name": "Renamed", "id": orgGlobex}))
	if w.Code != http.StatusOK {
		t.Fatalf("rename: got %d, want 200 (body: %s)", w.Code, w.Body.String())
	}

	f.snapshot(func(f *fakeLogger) {
		if len(f.addedOrgMembers) != 1 || f.addedOrgMembers[0].OrganizationID != orgAcme {
			t.Errorf("AddOrganizationMember calls = %+v, want one in %s", f.addedOrgMembers, orgAcme)
		}
		if len(f.updatedOrgs) != 1 || f.updatedOrgs[0].ID != orgAcme {
			t.Errorf("UpdateOrganization calls = %+v, want one of %s", f.updatedOrgs, orgAcme)
		}
		if f.orgs[orgGlobex].Name != "Globex" {
			t.Errorf("Globex was renamed to %q", f.orgs[orgGlobex].Name)
		}
	})
}

// --- Projects in an organization ---

// Any member of the organization may create a project in it, which they own;
// the organization comes from the path alone.
func TestCreateOrganizationProject_InThePathsOrganizationOwnedByTheCaller(t *testing.T) {
	h, f := newOrganizationTestServer(t, nil)

	w := do(h, internalRequest(http.MethodPost, "/organizations/"+orgAcme+"/projects?organization_id="+orgGlobex, "org-member",
		map[string]string{"name": "Fresh", "slug": "fresh", "organization_id": orgGlobex}))
	if w.Code != http.StatusCreated {
		t.Fatalf("create project: got %d, want 201 (body: %s)", w.Code, w.Body.String())
	}
	created := decodeData[data.Project](t, w)
	if created.OrganizationID.Hex() != orgAcme {
		t.Errorf("created project's organization = %s, want %s", created.OrganizationID.Hex(), orgAcme)
	}

	f.snapshot(func(f *fakeLogger) {
		if len(f.createdProjects) != 1 {
			t.Fatalf("CreateProject called %d times, want 1", len(f.createdProjects))
		}
		args := f.createdProjects[0]
		if args.OrganizationID != orgAcme {
			t.Errorf("CreateProject organization = %q, want %q", args.OrganizationID, orgAcme)
		}
		if args.OwnerID != testUserID("org-member") || args.Owner != "org-member" {
			t.Errorf("CreateProject owner = %d %q, want the caller", args.OwnerID, args.Owner)
		}
	})

	// The new project is the caller's: they reach it as its owner.
	w = do(h, internalRequest(http.MethodGet, "/projects/"+created.ID.Hex(), "org-member", nil))
	if w.Code != http.StatusOK {
		t.Errorf("GET the new project as its creator = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
}

func TestCreateOrganizationProject_RejectsInvalidInput(t *testing.T) {
	h, f := newOrganizationTestServer(t, nil)

	for name, body := range map[string]map[string]string{
		"missing name": {"slug": "ok-slug"},
		"invalid slug": {"name": "X", "slug": "Not A Slug"},
		"missing slug": {"name": "X"},
	} {
		w := do(h, internalRequest(http.MethodPost, "/organizations/"+orgAcme+"/projects", "org-member", body))
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400", name, w.Code)
		}
	}
	f.snapshot(func(f *fakeLogger) {
		if len(f.createdProjects) != 0 {
			t.Errorf("CreateProject called %d times for invalid input, want 0", len(f.createdProjects))
		}
	})
}

// --- Project access for organization owners ---

// An owner of a project's organization is owner of the project, member or not;
// the organization's admins and members get nothing from it.
func TestProjectAccess_AdmitsTheOrganizationsOwners(t *testing.T) {
	h, _ := newOrganizationTestServer(t, nil)

	for _, route := range projectRoutes {
		target := route.target(projAlpha)
		// Publishing an event needs RabbitMQ, which these tests have none of.
		if route.method == http.MethodPost && strings.HasSuffix(target, "/logs") {
			continue
		}
		h, _ := newOrganizationTestServer(t, nil)
		w := do(h, internalRequest(route.method, target, "org-owner", route.body(projAlpha)))
		// alphaKeyID names no key, so revoking it is the logger's 404, past the
		// access check like every other answer here.
		if w.Code >= 300 && !(w.Code == http.StatusNotFound && strings.Contains(target, "/keys/")) {
			t.Errorf("%s %s as the organization's owner = %d, want success (body: %s)", route.method, target, w.Code, w.Body.String())
		}
	}

	for _, user := range []string{"org-admin", "org-member"} {
		w := do(h, internalRequest(http.MethodGet, "/projects/"+projAlpha, user, nil))
		if w.Code != http.StatusForbidden {
			t.Errorf("GET alpha as the organization's %s = %d, want 403", user, w.Code)
		}
	}

	// Owning one organization grants nothing in another's projects.
	w := do(h, internalRequest(http.MethodGet, "/projects/"+projBeta, "org-owner", nil))
	if w.Code != http.StatusForbidden {
		t.Errorf("GET beta as Acme's owner = %d, want 403", w.Code)
	}
}
