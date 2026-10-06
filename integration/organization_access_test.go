//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"

	"logwolf-toolbox/data"
)

// --- Data layer ---

// TestAccessToProject_OrganizationOwners: an organization owner is owner of
// every project in it without a membership of their own; its admins and
// members get nothing from the organization, only from their own memberships.
func TestAccessToProject_OrganizationOwners(t *testing.T) {
	m, _ := setupOrganizationModels(t)

	org := createOrganization(t, m, "Owned", 3001, "org-owner")
	addOrganizationMember(t, m, org.ID, 3002, "org-admin", data.RoleAdmin)
	addOrganizationMember(t, m, org.ID, 3003, "org-member", data.RoleMember)
	other := createOrganization(t, m, "Other", 3004, "other-owner")

	project, err := m.InsertProject(data.Project{Name: "Inside", Slug: "inside", OrganizationID: org.ID})
	if err != nil {
		t.Fatalf("InsertProject: %v", err)
	}
	if _, err := m.InsertProjectMember(data.ProjectMember{ProjectID: project.ID, UserID: 3003, GithubLogin: "org-member", Role: data.RoleMember}); err != nil {
		t.Fatalf("InsertProjectMember: %v", err)
	}
	elsewhere, err := m.InsertProject(data.Project{Name: "Elsewhere", Slug: "elsewhere", OrganizationID: other.ID})
	if err != nil {
		t.Fatalf("InsertProject: %v", err)
	}

	cases := []struct {
		name    string
		project primitive.ObjectID
		userID  int64
		login   string
		want    data.ProjectAccess
	}{
		{"organization owner", project.ID, 3001, "org-owner", data.ProjectAccess{Exists: true, Role: data.RoleOwner}},
		{"organization admin", project.ID, 3002, "org-admin", data.ProjectAccess{Exists: true}},
		{"organization member with a project membership", project.ID, 3003, "org-member", data.ProjectAccess{Exists: true, Role: data.RoleMember}},
		{"outsider", project.ID, 3005, "outsider", data.ProjectAccess{Exists: true}},
		{"owner of another organization", elsewhere.ID, 3001, "org-owner", data.ProjectAccess{Exists: true}},
		{"project that does not exist", primitive.NewObjectID(), 3001, "org-owner", data.ProjectAccess{}},
	}
	for _, tc := range cases {
		got, err := m.AccessToProject(tc.project, tc.userID, tc.login)
		if err != nil || got != tc.want {
			t.Errorf("%s: AccessToProject = %+v, %v; want %+v", tc.name, got, err, tc.want)
		}
	}

	// The project list agrees with the access check.
	roles := func(userID int64, login string) map[primitive.ObjectID]string {
		projects, err := m.GetProjectsForUser(userID, login)
		if err != nil {
			t.Fatalf("GetProjectsForUser %s: %v", login, err)
		}
		out := map[primitive.ObjectID]string{}
		for _, p := range projects {
			out[p.ID] = p.Role
		}
		return out
	}
	if got := roles(3001, "org-owner"); got[project.ID] != data.RoleOwner || got[elsewhere.ID] != "" {
		t.Errorf("org-owner's projects = %v, want %s as owner and not %s", got, project.ID.Hex(), elsewhere.ID.Hex())
	}
	if got := roles(3002, "org-admin"); len(got) != 0 {
		t.Errorf("org-admin's projects = %v, want none", got)
	}
	if got := roles(3003, "org-member"); len(got) != 1 || got[project.ID] != data.RoleMember {
		t.Errorf("org-member's projects = %v, want %s as member", got, project.ID.Hex())
	}

	// A project owner who also owns the organization is listed once.
	if _, err := m.InsertProjectMember(data.ProjectMember{ProjectID: project.ID, UserID: 3001, GithubLogin: "org-owner", Role: data.RoleOwner}); err != nil {
		t.Fatalf("InsertProjectMember: %v", err)
	}
	projects, err := m.GetProjectsForUser(3001, "org-owner")
	if err != nil || len(projects) != 1 {
		t.Errorf("org-owner's projects with a membership too = %+v, %v; want the one project", projects, err)
	}
}

// TestOrganizationMembers_OwnerRequired: an admin changes members, but adding
// to or taking from the owners is an owner's call.
func TestOrganizationMembers_OwnerRequired(t *testing.T) {
	m, _ := setupOrganizationModels(t)

	org := createOrganization(t, m, "Guarded", 3101, "guard-owner")
	owner := organizationMemberID(t, m, org.ID, 3101)
	addOrganizationMember(t, m, org.ID, 3102, "guard-admin", data.RoleAdmin)
	member := addOrganizationMember(t, m, org.ID, 3103, "guard-member", data.RoleMember)
	second := addOrganizationMember(t, m, org.ID, 3104, "guard-second", data.RoleOwner)

	for _, actor := range []string{data.RoleAdmin, data.RoleMember, ""} {
		if err := m.RemoveOrganizationMember(org.ID, second, actor); !errors.Is(err, data.ErrOwnerRequired) {
			t.Errorf("%q removes an owner: want ErrOwnerRequired, got %v", actor, err)
		}
		if err := m.UpdateOrganizationMemberRole(org.ID, owner, data.RoleAdmin, actor); !errors.Is(err, data.ErrOwnerRequired) {
			t.Errorf("%q demotes an owner: want ErrOwnerRequired, got %v", actor, err)
		}
		if err := m.UpdateOrganizationMemberRole(org.ID, member, data.RoleOwner, actor); !errors.Is(err, data.ErrOwnerRequired) {
			t.Errorf("%q promotes to owner: want ErrOwnerRequired, got %v", actor, err)
		}
	}
	// Setting an owner's own role changes nothing, whoever asks.
	if err := m.UpdateOrganizationMemberRole(org.ID, owner, data.RoleOwner, data.RoleAdmin); err != nil {
		t.Errorf("admin sets an owner's own role: %v", err)
	}

	if err := m.UpdateOrganizationMemberRole(org.ID, member, data.RoleAdmin, data.RoleAdmin); err != nil {
		t.Errorf("admin promotes a member to admin: %v", err)
	}
	if err := m.RemoveOrganizationMember(org.ID, member, data.RoleAdmin); err != nil {
		t.Errorf("admin removes an admin: %v", err)
	}
	if err := m.RemoveOrganizationMember(org.ID, second, data.RoleOwner); err != nil {
		t.Errorf("owner removes an owner beside another: %v", err)
	}

	members, err := m.GetOrganizationMembers(org.ID)
	if err != nil {
		t.Fatalf("GetOrganizationMembers: %v", err)
	}
	if len(members) != 2 {
		t.Errorf("members = %+v, want guard-owner and guard-admin", members)
	}
}

func TestGetOrganizationUsage(t *testing.T) {
	m, _ := setupOrganizationModels(t)
	ctx := context.Background()

	org := createOrganization(t, m, "Counted", 3201, "counter")
	addOrganizationMember(t, m, org.ID, 3202, "counted", data.RoleMember)
	other := createOrganization(t, m, "Uncounted", 3203, "elsewhere")
	for _, o := range []*data.Organization{org, org, other} {
		if _, err := m.InsertProject(data.Project{Name: "P", Slug: "p", OrganizationID: o.ID}); err != nil {
			t.Fatalf("InsertProject: %v", err)
		}
	}

	got, err := m.GetOrganizationUsage(ctx, org.ID)
	if want := (data.OrganizationUsage{Projects: 2, Members: 2}); err != nil || got != want {
		t.Errorf("GetOrganizationUsage = %+v, %v; want %+v", got, err, want)
	}
	got, err = m.GetOrganizationUsage(ctx, primitive.NewObjectID())
	if err != nil || got != (data.OrganizationUsage{}) {
		t.Errorf("GetOrganizationUsage of no organization = %+v, %v; want zero", got, err)
	}
}

// --- Through the broker ---

// TestOrganizationRoutes_EndToEnd: the dashboard's organization routes through
// the broker, the logger and MongoDB of the shared stack.
func TestOrganizationRoutes_EndToEnd(t *testing.T) {
	stack := sharedStack(t)
	users := identityUsers(4)
	owner, admin, outsider, creator := users[0], users[1], users[2], users[3]

	status, raw := owner.call(t, stack, http.MethodPost, "/organizations", map[string]any{"name": "E2E Org"})
	if status != http.StatusCreated {
		t.Fatalf("create organization = %d, want 201 (data: %s)", status, raw)
	}
	var org data.UserOrganization
	if err := json.Unmarshal(raw, &org); err != nil {
		t.Fatalf("decode organization: %v", err)
	}
	if org.Name != "E2E Org" || org.Plan != data.SelfHostedPlan || org.Role != data.RoleOwner {
		t.Fatalf("created %+v, want E2E Org on %s, owned by its creator", org, data.SelfHostedPlan)
	}
	base := "/organizations/" + org.ID.Hex()

	status, raw = owner.call(t, stack, http.MethodPost, base+"/members",
		map[string]any{"login": admin.login, "user_id": admin.id, "role": data.RoleAdmin})
	if status != http.StatusCreated {
		t.Fatalf("add admin = %d, want 201 (data: %s)", status, raw)
	}

	status, raw = admin.call(t, stack, http.MethodGet, "/organizations", nil)
	var orgs []data.UserOrganization
	if err := json.Unmarshal(raw, &orgs); status != http.StatusOK || err != nil || len(orgs) != 1 || orgs[0].Role != data.RoleAdmin {
		t.Errorf("admin's organizations = %d %s, want E2E Org as admin", status, raw)
	}

	// Access: a non-member is refused, an organization that does not exist is
	// not found, and an admin cannot touch the owner.
	if status, raw := outsider.call(t, stack, http.MethodGet, base, nil); status != http.StatusForbidden {
		t.Errorf("outsider GET = %d, want 403 (data: %s)", status, raw)
	}
	if status, raw := owner.call(t, stack, http.MethodGet, "/organizations/"+primitive.NewObjectID().Hex(), nil); status != http.StatusNotFound {
		t.Errorf("GET of a missing organization = %d, want 404 (data: %s)", status, raw)
	}
	status, raw = owner.call(t, stack, http.MethodGet, base+"/members", nil)
	var members []data.OrganizationMember
	if err := json.Unmarshal(raw, &members); status != http.StatusOK || err != nil || len(members) != 2 {
		t.Fatalf("members = %d %s, want the owner and the admin", status, raw)
	}
	var ownerMembership string
	for _, mb := range members {
		if mb.UserID == owner.id {
			ownerMembership = mb.ID.Hex()
		}
	}
	if status, raw := admin.call(t, stack, http.MethodDelete, base+"/members/"+ownerMembership, nil); status != http.StatusForbidden {
		t.Errorf("admin removes the owner = %d, want 403 (data: %s)", status, raw)
	}
	if status, raw := owner.call(t, stack, http.MethodDelete, base+"/members/"+ownerMembership, nil); status != http.StatusBadRequest {
		t.Errorf("owner removes themselves, the last owner = %d, want 400 (data: %s)", status, raw)
	}
	if status, raw := admin.call(t, stack, http.MethodPatch, base, map[string]any{"name": "E2E Renamed"}); status != http.StatusOK {
		t.Errorf("admin renames = %d, want 200 (data: %s)", status, raw)
	}

	// A project moved into the organization is the owner's, member or not.
	projectID := createProjectAs(t, stack, creator, "e2e-org-project")
	client := testMongo(t, stack.mongoURI)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := client.Database("logs").Collection("projects").UpdateOne(ctx,
		bson.M{"_id": oid(t, projectID)}, bson.M{"$set": bson.M{"organization_id": org.ID}}); err != nil {
		t.Fatalf("move project into the organization: %v", err)
	}
	assertProjectAccess(t, stack, "organization owner", owner, projectID, data.RoleOwner)
	assertProjectAccess(t, stack, "organization admin", admin, projectID, "")

	status, raw = admin.call(t, stack, http.MethodGet, base+"/plan", nil)
	var plan struct {
		Plan struct {
			Name        string `json:"name"`
			MaxProjects int    `json:"max_projects"`
		} `json:"plan"`
		Usage data.OrganizationUsage `json:"usage"`
	}
	if err := json.Unmarshal(raw, &plan); status != http.StatusOK || err != nil {
		t.Fatalf("plan = %d %s, %v", status, raw, err)
	}
	if plan.Plan.Name != data.SelfHostedPlan || plan.Plan.MaxProjects != 0 || plan.Usage != (data.OrganizationUsage{Projects: 1, Members: 2}) {
		t.Errorf("plan = %+v, want the self-hosted plan, no limits, 1 project and 2 members", plan)
	}
}

// TestOrganizationProjects_EndToEnd: a member of an organization creates a
// project in it, through the broker, and owns it; an outsider cannot.
func TestOrganizationProjects_EndToEnd(t *testing.T) {
	stack := sharedStack(t)
	users := identityUsers(3)
	owner, member, outsider := users[0], users[1], users[2]

	status, raw := owner.call(t, stack, http.MethodPost, "/organizations", map[string]any{"name": "Project Org"})
	if status != http.StatusCreated {
		t.Fatalf("create organization = %d, want 201 (data: %s)", status, raw)
	}
	var org data.UserOrganization
	if err := json.Unmarshal(raw, &org); err != nil {
		t.Fatalf("decode organization: %v", err)
	}
	base := "/organizations/" + org.ID.Hex()

	if status, raw := owner.call(t, stack, http.MethodPost, base+"/members",
		map[string]any{"login": member.login, "user_id": member.id, "role": data.RoleMember}); status != http.StatusCreated {
		t.Fatalf("add member = %d, want 201 (data: %s)", status, raw)
	}

	if status, raw := outsider.call(t, stack, http.MethodPost, base+"/projects",
		map[string]any{"name": "Intruder", "slug": "intruder"}); status != http.StatusForbidden {
		t.Errorf("outsider creates a project = %d, want 403 (data: %s)", status, raw)
	}

	status, raw = member.call(t, stack, http.MethodPost, base+"/projects", map[string]any{"name": "In Org", "slug": "in-org"})
	if status != http.StatusCreated {
		t.Fatalf("member creates a project = %d, want 201 (data: %s)", status, raw)
	}
	var project data.Project
	if err := json.Unmarshal(raw, &project); err != nil {
		t.Fatalf("decode project: %v", err)
	}
	if project.OrganizationID != org.ID {
		t.Errorf("project's organization = %s, want %s", project.OrganizationID.Hex(), org.ID.Hex())
	}

	// The creator owns it; the organization's owner does too, through the
	// organization.
	assertProjectAccess(t, stack, "creator", member, project.ID.Hex(), data.RoleOwner)
	assertProjectAccess(t, stack, "organization owner", owner, project.ID.Hex(), data.RoleOwner)
	assertProjectAccess(t, stack, "outsider", outsider, project.ID.Hex(), "")

	status, raw = member.call(t, stack, http.MethodGet, base+"/plan", nil)
	var plan struct {
		Usage data.OrganizationUsage `json:"usage"`
	}
	if err := json.Unmarshal(raw, &plan); status != http.StatusOK || err != nil || plan.Usage.Projects != 1 {
		t.Errorf("plan = %d %s, want 1 project in use", status, raw)
	}
}
