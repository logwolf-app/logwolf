//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"

	"logwolf-toolbox/data"
)

// setupProjectModels returns a data.Models backed by the MongoDB the data-layer
// tests share, with the collections they touch cleared and the project indexes
// recreated. One container serves every test in this file; a clean database per
// test comes from the drop, not from a new container.
func setupProjectModels(t *testing.T) data.Models {
	t.Helper()

	client := testMongo(t, sharedModelsMongo(t))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for _, name := range []string{"projects", "project_members", "logs", "api_keys", "settings"} {
		if err := client.Database("logs").Collection(name).Drop(ctx); err != nil {
			t.Fatalf("setupProjectModels: drop %s: %v", name, err)
		}
	}

	m := data.New(client)
	if err := m.EnsureProjectIndexes(); err != nil {
		t.Fatalf("setupProjectModels: indexes: %v", err)
	}
	return m
}

// testOrganization creates an organization for a test's projects to be created
// in: every project belongs to one.
func testOrganization(t *testing.T, m data.Models) primitive.ObjectID {
	t.Helper()

	org, err := m.CreateOrganizationWithOwner(data.Organization{Name: "Projects", Plan: "free"}, testUserID("org-owner"), "org-owner")
	if err != nil {
		t.Fatalf("testOrganization: %v", err)
	}
	return org.ID
}

// --- Project CRUD ---

func TestInsertAndGetProject(t *testing.T) {
	m := setupProjectModels(t)

	created, err := m.InsertProject(data.Project{Name: "Alpha", Slug: "alpha"})
	if err != nil {
		t.Fatalf("InsertProject: %v", err)
	}
	if created.ID.IsZero() {
		t.Fatal("InsertProject: ID should be set")
	}

	got, err := m.GetProject(created.ID)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if got.Name != "Alpha" || got.Slug != "alpha" {
		t.Errorf("GetProject: got %+v", got)
	}
}

func TestGetProject_NotFound(t *testing.T) {
	m := setupProjectModels(t)

	_, err := m.GetProject(newOID())
	if !errors.Is(err, mongo.ErrNoDocuments) {
		t.Errorf("GetProject missing: want mongo.ErrNoDocuments, got %v", err)
	}
}

// Slugs are labels, not identifiers: two users naming their projects alike
// must both succeed, and neither learns that the other's project exists.
func TestProjectSlugsAreNotUnique(t *testing.T) {
	m := setupProjectModels(t)
	org := testOrganization(t, m)

	first, err := m.CreateProjectWithOwner(data.Project{Name: "App", Slug: "app", OrganizationID: org}, testUserID("alice"), "alice")
	if err != nil {
		t.Fatalf("first CreateProjectWithOwner: %v", err)
	}
	second, err := m.CreateProjectWithOwner(data.Project{Name: "App", Slug: "app", OrganizationID: org}, testUserID("bob"), "bob")
	if err != nil {
		t.Fatalf("second CreateProjectWithOwner with the same slug: %v", err)
	}
	if first.ID == second.ID {
		t.Fatal("both creates returned the same project")
	}

	// Default is an ordinary slug too: the migration's project is found by its
	// flag, never by this.
	if _, err := m.CreateProjectWithOwner(data.Project{Name: "Default", Slug: data.DefaultProjectSlug, OrganizationID: org}, testUserID("carol"), "carol"); err != nil {
		t.Errorf("CreateProjectWithOwner with slug %q: %v", data.DefaultProjectSlug, err)
	}
}

func TestRenameProject(t *testing.T) {
	m := setupProjectModels(t)

	p, err := m.InsertProject(data.Project{Name: "Old", Slug: "old-slug"})
	if err != nil {
		t.Fatalf("InsertProject: %v", err)
	}

	updated, err := m.RenameProject(p.ID, "New Name")
	if err != nil {
		t.Fatalf("RenameProject: %v", err)
	}
	if updated.Name != "New Name" || updated.Slug != "old-slug" {
		t.Errorf("RenameProject: got %+v, want the new name and the old slug", updated)
	}

	// Verify persistence via a fresh read.
	got, _ := m.GetProject(p.ID)
	if got.Name != "New Name" || got.Slug != "old-slug" {
		t.Errorf("RenameProject not persisted: got %+v", got)
	}
}

func TestRenameProject_NotFound(t *testing.T) {
	m := setupProjectModels(t)

	_, err := m.RenameProject(newOID(), "X")
	if !errors.Is(err, mongo.ErrNoDocuments) {
		t.Errorf("RenameProject missing: want mongo.ErrNoDocuments, got %v", err)
	}
}

func TestCreateProjectWithOwner(t *testing.T) {
	m := setupProjectModels(t)
	org := testOrganization(t, m)

	p, err := m.CreateProjectWithOwner(data.Project{Name: "Fresh", Slug: "fresh", OrganizationID: org}, 583231, "  JDoe ")
	if err != nil {
		t.Fatalf("CreateProjectWithOwner: %v", err)
	}

	got, err := m.GetProject(p.ID)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if got.Name != "Fresh" || got.Slug != "fresh" || got.Default || got.OrganizationID != org {
		t.Errorf("GetProject: got %+v, want it in organization %s", got, org.Hex())
	}

	members, err := m.GetProjectMembers(p.ID)
	if err != nil {
		t.Fatalf("GetProjectMembers: %v", err)
	}
	if len(members) != 1 || members[0].UserID != 583231 || members[0].GithubLogin != "jdoe" || members[0].Role != data.RoleOwner {
		t.Errorf("members = %+v, want user 583231 (jdoe) as the only owner", members)
	}
}

func TestCreateProjectWithOwner_RequiresOwner(t *testing.T) {
	m := setupProjectModels(t)
	db := testMongo(t, sharedModelsMongo(t)).Database("logs")
	org := testOrganization(t, m)

	if _, err := m.CreateProjectWithOwner(data.Project{Name: "Ownerless", Slug: "ownerless", OrganizationID: org}, 583231, " "); err == nil {
		t.Fatal("CreateProjectWithOwner with a blank login: want an error, got nil")
	}
	if _, err := m.CreateProjectWithOwner(data.Project{Name: "Ownerless", Slug: "ownerless", OrganizationID: org}, 0, "jdoe"); err == nil {
		t.Fatal("CreateProjectWithOwner without a user ID: want an error, got nil")
	}
	if n := countDocs(t, db.Collection("projects"), bson.M{}); n != 0 {
		t.Errorf("projects: %d created without an owner, want 0", n)
	}
}

// Every project is created inside an organization, and one that exists.
func TestCreateProjectWithOwner_RequiresOrganization(t *testing.T) {
	m := setupProjectModels(t)
	db := testMongo(t, sharedModelsMongo(t)).Database("logs")

	if _, err := m.CreateProjectWithOwner(data.Project{Name: "Loose", Slug: "loose"}, 583231, "jdoe"); err == nil {
		t.Fatal("CreateProjectWithOwner without an organization: want an error, got nil")
	}
	_, err := m.CreateProjectWithOwner(data.Project{Name: "Lost", Slug: "lost", OrganizationID: newOID()}, 583231, "jdoe")
	if !errors.Is(err, data.ErrUnknownOrganization) {
		t.Fatalf("CreateProjectWithOwner in a missing organization: want ErrUnknownOrganization, got %v", err)
	}
	if n := countDocs(t, db.Collection("projects"), bson.M{}); n != 0 {
		t.Errorf("projects: %d created outside an organization, want 0", n)
	}
	if n := countDocs(t, db.Collection("project_members"), bson.M{}); n != 0 {
		t.Errorf("project_members: %d left behind, want 0", n)
	}
}

// TestCreateProjectWithOwner_RollsBackWhenOwnerInsertFails lets the project
// insert through and fails the owner's, and checks the project goes with it: a
// project without an owner could never be reached by anyone.
func TestCreateProjectWithOwner_RollsBackWhenOwnerInsertFails(t *testing.T) {
	m := setupProjectModels(t)
	client := testMongo(t, sharedModelsMongo(t))
	db := client.Database("logs")
	org := testOrganization(t, m)

	// Let the project insert through, then fail every insert after with a
	// non-transient error, so WithTransaction gives up instead of retrying.
	setFailPoint(t, client, bson.M{"skip": 1}, bson.M{"failCommands": bson.A{"insert"}, "errorCode": 2})

	_, err := m.CreateProjectWithOwner(data.Project{Name: "Orphan", Slug: "orphan", OrganizationID: org}, testUserID("alice"), "alice")
	clearFailPoint(t, client)

	if err == nil || !strings.Contains(err.Error(), "owner") {
		t.Fatalf("CreateProjectWithOwner: want a failure at the owner insert, got %v", err)
	}
	if n := countDocs(t, db.Collection("projects"), bson.M{}); n != 0 {
		t.Errorf("projects: %d left behind by the failed create, want 0", n)
	}
	if n := countDocs(t, db.Collection("project_members"), bson.M{}); n != 0 {
		t.Errorf("project_members: %d left behind by the failed create, want 0", n)
	}
}

func TestDeleteProject_Cascade(t *testing.T) {
	m := setupProjectModels(t)

	p, _ := m.InsertProject(data.Project{Name: "Doomed", Slug: "doomed"})

	// Seed related data.
	if err := m.Insert(data.LogEntry{ProjectID: p.ID, Name: "e", Data: "{}", Severity: "info", Tags: []string{}}); err != nil {
		t.Fatalf("seed log: %v", err)
	}
	plaintext, key, err := data.GenerateAPIKey(p.ID, nil)
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}
	_ = plaintext
	if err := m.SaveAPIKey(&key); err != nil {
		t.Fatalf("SaveAPIKey: %v", err)
	}
	if err := m.Settings.SetRetentionDays(p.ID, 30); err != nil {
		t.Fatalf("SetRetentionDays: %v", err)
	}
	if _, err := m.InsertProjectMember(data.ProjectMember{
		ProjectID: p.ID, GithubLogin: "owner1", Role: data.RoleOwner,
	}); err != nil {
		t.Fatalf("InsertProjectMember: %v", err)
	}

	if err := m.DeleteProject(p.ID); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}

	// Project itself must be gone.
	if _, err := m.GetProject(p.ID); !errors.Is(err, mongo.ErrNoDocuments) {
		t.Errorf("project still present after delete: %v", err)
	}
	// Members must be gone.
	members, _ := m.GetProjectMembers(p.ID)
	if len(members) != 0 {
		t.Errorf("members still present: %d", len(members))
	}
	// Logs are left behind for PurgeProjectLogs, which removes them.
	purged, err := m.PurgeProjectLogs(context.Background(), p.ID)
	if err != nil {
		t.Fatalf("PurgeProjectLogs: %v", err)
	}
	if purged != 1 {
		t.Errorf("PurgeProjectLogs deleted %d log(s), want the 1 seeded", purged)
	}
	logs, err := m.AllLogs(p.ID, data.PaginationParams{Page: 1, PageSize: 100})
	if err != nil {
		t.Fatalf("AllLogs after delete: %v", err)
	}
	if len(logs) != 0 {
		t.Errorf("logs still present: %d", len(logs))
	}
	// API keys must be gone.
	keys, err := m.ListAPIKeysByProject(p.ID)
	if err != nil {
		t.Fatalf("ListAPIKeysByProject after delete: %v", err)
	}
	if len(keys) != 0 {
		t.Errorf("api_keys still present for deleted project: %d key(s)", len(keys))
	}
	// Settings must be gone — GetRetentionDays falls back to the default (90)
	// when no document exists, so verify directly that no settings doc survives.
	days, err := m.Settings.GetRetentionDays(context.Background(), p.ID)
	if err != nil {
		t.Fatalf("GetRetentionDays after delete: %v", err)
	}
	if days != 90 {
		t.Errorf("settings still present for deleted project: retention=%d (want default 90)", days)
	}
}

// TestDeleteProject_RollsBackOnFailure fails the cascade part-way through — the
// third delete, project_members, after api_keys and settings have gone — and
// checks that the transaction takes those two back with it. The seeded log is
// outside the transaction and must simply be untouched.
func TestDeleteProject_RollsBackOnFailure(t *testing.T) {
	m := setupProjectModels(t)
	client := testMongo(t, sharedModelsMongo(t))
	db := client.Database("logs")

	p, _ := m.InsertProject(data.Project{Name: "Survivor", Slug: "survivor"})
	projectID := p.ID

	if err := m.Insert(data.LogEntry{ProjectID: projectID, Name: "e", Data: "{}", Severity: "info", Tags: []string{}}); err != nil {
		t.Fatalf("seed log: %v", err)
	}
	_, key, err := data.GenerateAPIKey(projectID, nil)
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}
	if err := m.SaveAPIKey(&key); err != nil {
		t.Fatalf("SaveAPIKey: %v", err)
	}
	if err := m.Settings.SetRetentionDays(projectID, 30); err != nil {
		t.Fatalf("SetRetentionDays: %v", err)
	}
	if _, err := m.InsertProjectMember(data.ProjectMember{
		ProjectID: p.ID, GithubLogin: "owner1", Role: data.RoleOwner,
	}); err != nil {
		t.Fatalf("InsertProjectMember: %v", err)
	}

	// Let two deletes through, then fail every one after with a non-transient
	// error, so WithTransaction gives up instead of retrying.
	setFailPoint(t, client, bson.M{"skip": 2}, bson.M{"failCommands": bson.A{"delete"}, "errorCode": 2})

	err = m.DeleteProject(p.ID)
	clearFailPoint(t, client)

	if err == nil || !strings.Contains(err.Error(), "project_members") {
		t.Fatalf("DeleteProject: want a failure at project_members, got %v", err)
	}

	for coll, filter := range map[string]bson.M{
		"logs":            {"project_id": projectID},
		"api_keys":        {"project_id": projectID},
		"settings":        {"project_id": projectID},
		"project_members": {"project_id": p.ID},
		"projects":        {"_id": p.ID},
	} {
		if n := countDocs(t, db.Collection(coll), filter); n != 1 {
			t.Errorf("%s: want the 1 seeded document back after the rollback, got %d", coll, n)
		}
	}
}

// setFailPoint turns on MongoDB's failCommand fail point, which makes the
// server fail the commands named in data without running them.
func setFailPoint(t *testing.T, client *mongo.Client, mode, data bson.M) {
	t.Helper()

	// Clear it even if the test stops early: a fail point left on would break
	// every test after this one that shares the container.
	t.Cleanup(func() { clearFailPoint(t, client) })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cmd := bson.D{
		{Key: "configureFailPoint", Value: "failCommand"},
		{Key: "mode", Value: mode},
		{Key: "data", Value: data},
	}
	if err := client.Database("admin").RunCommand(ctx, cmd).Err(); err != nil {
		t.Fatalf("configureFailPoint: %v", err)
	}
}

func clearFailPoint(t *testing.T, client *mongo.Client) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cmd := bson.D{{Key: "configureFailPoint", Value: "failCommand"}, {Key: "mode", Value: "off"}}
	if err := client.Database("admin").RunCommand(ctx, cmd).Err(); err != nil {
		t.Errorf("clear fail point: %v", err)
	}
}

// --- Member helpers ---

func TestInsertProjectMember_Duplicate(t *testing.T) {
	m := setupProjectModels(t)

	p, _ := m.InsertProject(data.Project{Name: "Dup", Slug: "dup"})
	pm := data.ProjectMember{ProjectID: p.ID, GithubLogin: "alice", Role: data.RoleOwner}

	if _, err := m.InsertProjectMember(pm); err != nil {
		t.Fatalf("first InsertProjectMember: %v", err)
	}
	if _, err := m.InsertProjectMember(pm); err == nil {
		t.Error("second InsertProjectMember: expected duplicate key error, got nil")
	}
}

func TestRemoveProjectMember_LastOwner(t *testing.T) {
	m := setupProjectModels(t)

	p, _ := m.InsertProject(data.Project{Name: "Solo", Slug: "solo"})
	m.InsertProjectMember(data.ProjectMember{ProjectID: p.ID, GithubLogin: "only-owner", Role: data.RoleOwner})

	err := m.RemoveProjectMember(p.ID, memberID(t, m, p.ID, "only-owner"))
	if !errors.Is(err, data.ErrLastOwner) {
		t.Errorf("RemoveProjectMember last owner: want ErrLastOwner, got %v", err)
	}
}

func TestRemoveProjectMember_SecondOwnerAllowed(t *testing.T) {
	m := setupProjectModels(t)

	p, _ := m.InsertProject(data.Project{Name: "Multi", Slug: "multi"})
	m.InsertProjectMember(data.ProjectMember{ProjectID: p.ID, GithubLogin: "owner1", Role: data.RoleOwner})
	m.InsertProjectMember(data.ProjectMember{ProjectID: p.ID, GithubLogin: "owner2", Role: data.RoleOwner})

	if err := m.RemoveProjectMember(p.ID, memberID(t, m, p.ID, "owner2")); err != nil {
		t.Errorf("RemoveProjectMember second owner: %v", err)
	}
}

func TestRemoveProjectMember_RegularMember(t *testing.T) {
	m := setupProjectModels(t)

	p, _ := m.InsertProject(data.Project{Name: "Reg", Slug: "reg"})
	m.InsertProjectMember(data.ProjectMember{ProjectID: p.ID, GithubLogin: "owner", Role: data.RoleOwner})
	m.InsertProjectMember(data.ProjectMember{ProjectID: p.ID, GithubLogin: "bob", Role: data.RoleMember})

	if err := m.RemoveProjectMember(p.ID, memberID(t, m, p.ID, "bob")); err != nil {
		t.Errorf("RemoveProjectMember member: %v", err)
	}
}

// TestRemoveProjectMember_ConcurrentOwners removes both owners of a project at
// the same moment. Each removal on its own is allowed — there is another owner —
// but only one of them may win, or the project is left with nobody to own it.
// A single round rarely hits the window, so it runs many.
func TestRemoveProjectMember_ConcurrentOwners(t *testing.T) {
	m := setupProjectModels(t)

	for round := 0; round < 25; round++ {
		p, err := m.InsertProject(data.Project{Name: "Race", Slug: fmt.Sprintf("race-%d", round)})
		if err != nil {
			t.Fatalf("round %d: InsertProject: %v", round, err)
		}
		var owners []primitive.ObjectID
		for _, login := range []string{"owner-a", "owner-b"} {
			owners = append(owners, addMember(t, m, p.ID, login, data.RoleOwner))
		}

		start := make(chan struct{})
		errs := make([]error, len(owners))
		var wg sync.WaitGroup
		for i, owner := range owners {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				errs[i] = m.RemoveProjectMember(p.ID, owner)
			}()
		}
		close(start)
		wg.Wait()

		var removed, refused int
		for _, err := range errs {
			switch {
			case err == nil:
				removed++
			case errors.Is(err, data.ErrLastOwner):
				refused++
			default:
				t.Fatalf("round %d: RemoveProjectMember: %v", round, err)
			}
		}
		if removed != 1 || refused != 1 {
			t.Fatalf("round %d: want one removal and one ErrLastOwner, got %d and %d", round, removed, refused)
		}

		members, err := m.GetProjectMembers(p.ID)
		if err != nil {
			t.Fatalf("round %d: GetProjectMembers: %v", round, err)
		}
		if len(members) != 1 || members[0].Role != data.RoleOwner {
			t.Fatalf("round %d: want exactly one owner left, got %+v", round, members)
		}
	}
}

func TestRemoveProjectMember_NotFound(t *testing.T) {
	m := setupProjectModels(t)

	p, _ := m.InsertProject(data.Project{Name: "NF", Slug: "nf"})

	err := m.RemoveProjectMember(p.ID, newOID())
	if !errors.Is(err, mongo.ErrNoDocuments) {
		t.Errorf("RemoveProjectMember missing: want mongo.ErrNoDocuments, got %v", err)
	}
}

// addMember makes login's test user (testUserID) a member of the project, and
// returns the membership's id.
func addMember(t *testing.T, m data.Models, projectID primitive.ObjectID, login, role string) primitive.ObjectID {
	t.Helper()

	pm, err := m.InsertProjectMember(data.ProjectMember{ProjectID: projectID, UserID: testUserID(login), GithubLogin: login, Role: role})
	if err != nil {
		t.Fatalf("InsertProjectMember %s: %v", login, err)
	}
	return pm.ID
}

// memberID returns the id of the project's membership listed under login.
func memberID(t *testing.T, m data.Models, projectID primitive.ObjectID, login string) primitive.ObjectID {
	t.Helper()

	members, err := m.GetProjectMembers(projectID)
	if err != nil {
		t.Fatalf("GetProjectMembers: %v", err)
	}
	for _, mb := range members {
		if mb.GithubLogin == login {
			return mb.ID
		}
	}
	t.Fatalf("no member %s in project %s: %+v", login, projectID.Hex(), members)
	return primitive.NilObjectID
}

// memberRoles maps each member of the project to their role.
func memberRoles(t *testing.T, m data.Models, projectID primitive.ObjectID) map[string]string {
	t.Helper()

	members, err := m.GetProjectMembers(projectID)
	if err != nil {
		t.Fatalf("GetProjectMembers: %v", err)
	}
	roles := make(map[string]string, len(members))
	for _, mb := range members {
		roles[mb.GithubLogin] = mb.Role
	}
	return roles
}

// TestUpdateProjectMemberRole_TransferOwnership walks the handover the role
// change exists for: promote the new owner, then step down.
func TestUpdateProjectMemberRole_TransferOwnership(t *testing.T) {
	m := setupProjectModels(t)

	p, _ := m.InsertProject(data.Project{Name: "Handover", Slug: "handover"})
	m.InsertProjectMember(data.ProjectMember{ProjectID: p.ID, GithubLogin: "old-owner", Role: data.RoleOwner})
	m.InsertProjectMember(data.ProjectMember{ProjectID: p.ID, GithubLogin: "heir", Role: data.RoleMember})

	if err := m.UpdateProjectMemberRole(p.ID, memberID(t, m, p.ID, "heir"), data.RoleOwner); err != nil {
		t.Fatalf("promote heir: %v", err)
	}
	if err := m.UpdateProjectMemberRole(p.ID, memberID(t, m, p.ID, "old-owner"), data.RoleMember); err != nil {
		t.Fatalf("demote old-owner: %v", err)
	}

	roles := memberRoles(t, m, p.ID)
	if roles["heir"] != data.RoleOwner || roles["old-owner"] != data.RoleMember {
		t.Errorf("after the handover: got %v", roles)
	}
}

func TestUpdateProjectMemberRole_LastOwner(t *testing.T) {
	m := setupProjectModels(t)

	p, _ := m.InsertProject(data.Project{Name: "Solo", Slug: "solo"})
	m.InsertProjectMember(data.ProjectMember{ProjectID: p.ID, GithubLogin: "only-owner", Role: data.RoleOwner})
	m.InsertProjectMember(data.ProjectMember{ProjectID: p.ID, GithubLogin: "bob", Role: data.RoleMember})

	err := m.UpdateProjectMemberRole(p.ID, memberID(t, m, p.ID, "only-owner"), data.RoleMember)
	if !errors.Is(err, data.ErrLastOwner) {
		t.Errorf("demote last owner: want ErrLastOwner, got %v", err)
	}
	if roles := memberRoles(t, m, p.ID); roles["only-owner"] != data.RoleOwner {
		t.Errorf("refused demotion still changed the role: %v", roles)
	}

	// Setting the role the last owner already holds is not a demotion.
	if err := m.UpdateProjectMemberRole(p.ID, memberID(t, m, p.ID, "only-owner"), data.RoleOwner); err != nil {
		t.Errorf("owner to owner: %v", err)
	}
}

// A membership id names a row of one project: another project's is not found,
// and changes nothing there.
func TestMemberChanges_StayInTheirProject(t *testing.T) {
	m := setupProjectModels(t)

	p, _ := m.InsertProject(data.Project{Name: "Mine", Slug: "mine"})
	other, _ := m.InsertProject(data.Project{Name: "Theirs", Slug: "theirs"})
	addMember(t, m, p.ID, "owner", data.RoleOwner)
	addMember(t, m, other.ID, "their-owner", data.RoleOwner)
	theirs := addMember(t, m, other.ID, "their-member", data.RoleMember)

	if err := m.UpdateProjectMemberRole(p.ID, theirs, data.RoleOwner); !errors.Is(err, mongo.ErrNoDocuments) {
		t.Errorf("promote another project's member: want mongo.ErrNoDocuments, got %v", err)
	}
	if err := m.RemoveProjectMember(p.ID, theirs); !errors.Is(err, mongo.ErrNoDocuments) {
		t.Errorf("remove another project's member: want mongo.ErrNoDocuments, got %v", err)
	}
	if roles := memberRoles(t, m, other.ID); roles["their-member"] != data.RoleMember {
		t.Errorf("the other project's members changed: %v", roles)
	}
}

func TestUpdateProjectMemberRole_NotFound(t *testing.T) {
	m := setupProjectModels(t)

	p, _ := m.InsertProject(data.Project{Name: "NF", Slug: "nf"})

	err := m.UpdateProjectMemberRole(p.ID, newOID(), data.RoleOwner)
	if !errors.Is(err, mongo.ErrNoDocuments) {
		t.Errorf("UpdateProjectMemberRole missing: want mongo.ErrNoDocuments, got %v", err)
	}
}

func TestUpdateProjectMemberRole_InvalidRole(t *testing.T) {
	m := setupProjectModels(t)

	p, _ := m.InsertProject(data.Project{Name: "Bad", Slug: "bad"})
	m.InsertProjectMember(data.ProjectMember{ProjectID: p.ID, GithubLogin: "bob", Role: data.RoleMember})

	if err := m.UpdateProjectMemberRole(p.ID, memberID(t, m, p.ID, "bob"), "admin"); err == nil {
		t.Error("UpdateProjectMemberRole admin: expected an error, got nil")
	}
	if roles := memberRoles(t, m, p.ID); roles["bob"] != data.RoleMember {
		t.Errorf("invalid role still changed the member: %v", roles)
	}
}

// TestUpdateProjectMemberRole_ConcurrentDemotion demotes both owners of a
// project at the same moment, and in the other half of the rounds demotes one
// while removing the other. Each change on its own is allowed, but only one of
// them may win, or the project is left with nobody to own it. A single round
// rarely hits the window, so it runs many.
func TestUpdateProjectMemberRole_ConcurrentDemotion(t *testing.T) {
	m := setupProjectModels(t)

	for round := 0; round < 50; round++ {
		p, err := m.InsertProject(data.Project{Name: "Race", Slug: fmt.Sprintf("demote-race-%d", round)})
		if err != nil {
			t.Fatalf("round %d: InsertProject: %v", round, err)
		}
		ownerA := addMember(t, m, p.ID, "owner-a", data.RoleOwner)
		ownerB := addMember(t, m, p.ID, "owner-b", data.RoleOwner)

		changes := []func() error{
			func() error { return m.UpdateProjectMemberRole(p.ID, ownerA, data.RoleMember) },
			func() error { return m.UpdateProjectMemberRole(p.ID, ownerB, data.RoleMember) },
		}
		if round%2 == 1 {
			changes[1] = func() error { return m.RemoveProjectMember(p.ID, ownerB) }
		}

		start := make(chan struct{})
		errs := make([]error, len(changes))
		var wg sync.WaitGroup
		for i, change := range changes {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				errs[i] = change()
			}()
		}
		close(start)
		wg.Wait()

		var applied, refused int
		for _, err := range errs {
			switch {
			case err == nil:
				applied++
			case errors.Is(err, data.ErrLastOwner):
				refused++
			default:
				t.Fatalf("round %d: %v", round, err)
			}
		}
		if applied != 1 || refused != 1 {
			t.Fatalf("round %d: want one change and one ErrLastOwner, got %d and %d", round, applied, refused)
		}

		var ownersLeft int
		for _, role := range memberRoles(t, m, p.ID) {
			if role == data.RoleOwner {
				ownersLeft++
			}
		}
		if ownersLeft != 1 {
			t.Fatalf("round %d: want exactly one owner left, got %d", round, ownersLeft)
		}
	}
}

func TestGetProjectMembers(t *testing.T) {
	m := setupProjectModels(t)

	p, _ := m.InsertProject(data.Project{Name: "Listed", Slug: "listed"})
	other, _ := m.InsertProject(data.Project{Name: "Other", Slug: "other"})

	m.InsertProjectMember(data.ProjectMember{ProjectID: p.ID, GithubLogin: "erin", Role: data.RoleOwner})
	m.InsertProjectMember(data.ProjectMember{ProjectID: p.ID, GithubLogin: "frank", Role: data.RoleMember})
	m.InsertProjectMember(data.ProjectMember{ProjectID: other.ID, GithubLogin: "grace", Role: data.RoleOwner})

	members, err := m.GetProjectMembers(p.ID)
	if err != nil {
		t.Fatalf("GetProjectMembers: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("GetProjectMembers: want 2 members, got %d: %+v", len(members), members)
	}

	roles := map[string]string{}
	for _, member := range members {
		if member.ProjectID != p.ID {
			t.Errorf("GetProjectMembers returned a member of project %s", member.ProjectID.Hex())
		}
		roles[member.GithubLogin] = member.Role
	}
	if roles["erin"] != data.RoleOwner {
		t.Errorf("erin role = %q, want %q", roles["erin"], data.RoleOwner)
	}
	if roles["frank"] != data.RoleMember {
		t.Errorf("frank role = %q, want %q", roles["frank"], data.RoleMember)
	}
	if _, leaked := roles["grace"]; leaked {
		t.Error("GetProjectMembers leaked a member of another project")
	}
}

func TestGetProjectMembers_NoMembers(t *testing.T) {
	m := setupProjectModels(t)

	p, _ := m.InsertProject(data.Project{Name: "Empty", Slug: "empty"})

	members, err := m.GetProjectMembers(p.ID)
	if err != nil {
		t.Fatalf("GetProjectMembers: %v", err)
	}
	if len(members) != 0 {
		t.Errorf("GetProjectMembers on a memberless project: got %d", len(members))
	}
}

func TestMemberRole(t *testing.T) {
	m := setupProjectModels(t)

	p, _ := m.InsertProject(data.Project{Name: "Check", Slug: "check"})
	m.InsertProjectMember(data.ProjectMember{ProjectID: p.ID, GithubLogin: "carol", Role: data.RoleMember})
	m.InsertProjectMember(data.ProjectMember{ProjectID: p.ID, GithubLogin: "olive", Role: data.RoleOwner})

	for login, want := range map[string]string{"carol": data.RoleMember, "olive": data.RoleOwner, "stranger": ""} {
		role, err := m.MemberRole(p.ID, testUserID(login), login)
		if err != nil || role != want {
			t.Errorf("MemberRole %s: role=%q err=%v, want %q", login, role, err, want)
		}
	}

	if role, err := m.MemberRole(primitive.NewObjectID(), testUserID("carol"), "carol"); err != nil || role != "" {
		t.Errorf("MemberRole in a project that does not exist: role=%q err=%v, want none", role, err)
	}
}

// TestMemberRole_ByUserID: a membership linked to a user is theirs under any
// login they sign in with, and nobody else's, even someone who takes the login
// it was added under. A membership stored before user IDs has a login alone,
// and is matched by it.
func TestMemberRole_ByUserID(t *testing.T) {
	m := setupProjectModels(t)

	p, _ := m.InsertProject(data.Project{Name: "Linked", Slug: "linked"})
	addMember(t, m, p.ID, "erin", data.RoleOwner)
	if _, err := m.InsertProjectMember(data.ProjectMember{ProjectID: p.ID, GithubLogin: "frank", Role: data.RoleMember}); err != nil {
		t.Fatalf("InsertProjectMember frank, unlinked: %v", err)
	}

	cases := []struct {
		name   string
		userID int64
		login  string
		want   string
	}{
		{"erin", testUserID("erin"), "erin", data.RoleOwner},
		{"erin after a rename", testUserID("erin"), "erin-renamed", data.RoleOwner},
		{"another account with erin's login", testUserID("erin") + 1, "erin", ""},
		{"no user ID with erin's login", 0, "erin", ""},
		{"unlinked frank, by login", testUserID("frank"), "Frank", data.RoleMember},
		{"unlinked frank's login, any user ID", 42, "frank", data.RoleMember},
	}
	for _, tc := range cases {
		role, err := m.MemberRole(p.ID, tc.userID, tc.login)
		if err != nil || role != tc.want {
			t.Errorf("%s: role=%q err=%v, want %q", tc.name, role, err, tc.want)
		}
	}

	projects, err := m.GetProjectsForUser(testUserID("erin"), "erin-renamed")
	if err != nil || len(projects) != 1 || projects[0].Role != data.RoleOwner {
		t.Errorf("GetProjectsForUser erin after a rename: %+v, err=%v, want the project as owner", projects, err)
	}
	projects, err = m.GetProjectsForUser(testUserID("erin")+1, "erin")
	if err != nil || len(projects) != 0 {
		t.Errorf("GetProjectsForUser another account with erin's login: %+v, err=%v, want none", projects, err)
	}
}

// TestInsertProjectMember_OneMembershipPerUser: a user is a member of a project
// once, whatever login they are added under.
func TestInsertProjectMember_OneMembershipPerUser(t *testing.T) {
	m := setupProjectModels(t)

	p, _ := m.InsertProject(data.Project{Name: "Once", Slug: "once"})
	addMember(t, m, p.ID, "erin", data.RoleMember)

	_, err := m.InsertProjectMember(data.ProjectMember{ProjectID: p.ID, UserID: testUserID("erin"), GithubLogin: "erin-renamed", Role: data.RoleOwner})
	if !mongo.IsDuplicateKeyError(err) {
		t.Errorf("InsertProjectMember erin again under a new login: err=%v, want a duplicate key error", err)
	}

	// Another project is another membership.
	other, _ := m.InsertProject(data.Project{Name: "Twice", Slug: "twice"})
	addMember(t, m, other.ID, "erin", data.RoleMember)
}

// TestGetProjectMembers_ShowsTheLoginOfTheLastSignIn: a linked member is listed
// under the login they last signed in with, so a GitHub rename shows; the
// membership keeps the login it was added under.
func TestGetProjectMembers_ShowsTheLoginOfTheLastSignIn(t *testing.T) {
	m := setupProjectModels(t)

	p, _ := m.InsertProject(data.Project{Name: "Renames", Slug: "renames"})
	addMember(t, m, p.ID, "erin", data.RoleOwner)
	addMember(t, m, p.ID, "never-signed-in", data.RoleMember)
	if _, err := m.InsertProjectMember(data.ProjectMember{ProjectID: p.ID, GithubLogin: "unlinked", Role: data.RoleMember}); err != nil {
		t.Fatalf("InsertProjectMember unlinked: %v", err)
	}
	if _, err := m.UpsertUser(testUserID("erin"), "Erin-Renamed", ""); err != nil {
		t.Fatalf("UpsertUser: %v", err)
	}

	roles := memberRoles(t, m, p.ID)
	want := map[string]string{"erin-renamed": data.RoleOwner, "never-signed-in": data.RoleMember, "unlinked": data.RoleMember}
	if !maps.Equal(roles, want) {
		t.Errorf("members = %v, want %v", roles, want)
	}
}

// TestProjectMembers_CaseInsensitiveLogin: an owner types "jdoe" on the settings
// page, GitHub signs the user in as "JDoe". GitHub logins are case-insensitive,
// so every membership lookup must treat the two as the same person — and the
// unique index must not admit them as two members.
func TestProjectMembers_CaseInsensitiveLogin(t *testing.T) {
	m := setupProjectModels(t)

	p, _ := m.InsertProject(data.Project{Name: "Case", Slug: "case"})
	m.InsertProjectMember(data.ProjectMember{ProjectID: p.ID, GithubLogin: "owner", Role: data.RoleOwner})
	if _, err := m.InsertProjectMember(data.ProjectMember{ProjectID: p.ID, GithubLogin: "jdoe", Role: data.RoleMember}); err != nil {
		t.Fatalf("InsertProjectMember: %v", err)
	}

	// The membership was added before user IDs, so it is matched by login.
	role, err := m.MemberRole(p.ID, testUserID("jdoe"), "JDoe")
	if err != nil || role != data.RoleMember {
		t.Errorf("MemberRole JDoe: role=%q err=%v, want the membership added as jdoe", role, err)
	}

	projects, err := m.GetProjectsForUser(testUserID("jdoe"), "JDoe")
	if err != nil || len(projects) != 1 {
		t.Errorf("GetProjectsForUser JDoe: got %d projects, err=%v, want 1", len(projects), err)
	}

	_, err = m.InsertProjectMember(data.ProjectMember{ProjectID: p.ID, GithubLogin: "JDOE", Role: data.RoleOwner})
	if !mongo.IsDuplicateKeyError(err) {
		t.Errorf("InsertProjectMember JDOE next to jdoe: err=%v, want a duplicate key error", err)
	}

	if err := m.RemoveProjectMember(p.ID, memberID(t, m, p.ID, "jdoe")); err != nil {
		t.Fatalf("RemoveProjectMember jdoe: %v", err)
	}
	if role, _ := m.MemberRole(p.ID, testUserID("jdoe"), "JDoe"); role != "" {
		t.Error("JDoe is still a member after removing jdoe")
	}
}

func TestGetProjectsForUser(t *testing.T) {
	m := setupProjectModels(t)

	p1, _ := m.InsertProject(data.Project{Name: "P1", Slug: "p1"})
	p2, _ := m.InsertProject(data.Project{Name: "P2", Slug: "p2"})
	m.InsertProject(data.Project{Name: "P3", Slug: "p3"}) // dave is NOT a member

	addMember(t, m, p1.ID, "dave", data.RoleOwner)
	addMember(t, m, p2.ID, "dave", data.RoleMember)

	projects, err := m.GetProjectsForUser(testUserID("dave"), "dave")
	if err != nil {
		t.Fatalf("GetProjectsForUser: %v", err)
	}
	if len(projects) != 2 {
		t.Errorf("GetProjectsForUser: want 2 projects, got %d", len(projects))
	}

	// The role travels with the project so the dashboard can label each one
	// without a second round trip per project.
	roles := map[string]string{}
	for _, p := range projects {
		roles[p.Slug] = p.Role
	}
	if roles["p1"] != data.RoleOwner {
		t.Errorf("GetProjectsForUser: p1 role = %q, want %q", roles["p1"], data.RoleOwner)
	}
	if roles["p2"] != data.RoleMember {
		t.Errorf("GetProjectsForUser: p2 role = %q, want %q", roles["p2"], data.RoleMember)
	}
}

func TestGetProjectsForUser_NoMemberships(t *testing.T) {
	m := setupProjectModels(t)

	projects, err := m.GetProjectsForUser(testUserID("nobody"), "nobody")
	if err != nil {
		t.Fatalf("GetProjectsForUser no memberships: %v", err)
	}
	if projects == nil {
		t.Error("GetProjectsForUser: must return non-nil slice for user with no projects")
	}
	if len(projects) != 0 {
		t.Errorf("GetProjectsForUser: want 0, got %d", len(projects))
	}
}

// newOID returns a fresh ObjectID guaranteed not to exist in any collection.
func newOID() primitive.ObjectID { return primitive.NewObjectID() }

// --- Linking memberships to user IDs ---

// memberDoc reads one membership straight from the collection, user_id included.
func memberDoc(t *testing.T, id primitive.ObjectID) data.ProjectMember {
	t.Helper()

	client := testMongo(t, sharedModelsMongo(t))
	var mb data.ProjectMember
	if err := client.Database("logs").Collection("project_members").FindOne(context.Background(), bson.M{"_id": id}).Decode(&mb); err != nil {
		t.Fatalf("membership %s: %v", id.Hex(), err)
	}
	return mb
}

// insertUnlinked stores a membership the way builds before user IDs did: a
// login and no user_id.
func insertUnlinked(t *testing.T, m data.Models, projectID primitive.ObjectID, login, role string) primitive.ObjectID {
	t.Helper()

	pm, err := m.InsertProjectMember(data.ProjectMember{ProjectID: projectID, GithubLogin: login, Role: role})
	if err != nil {
		t.Fatalf("InsertProjectMember %s, unlinked: %v", login, err)
	}
	return pm.ID
}

// TestLinkMemberships: a sign-in links every login-only membership under the
// user's current login to their user ID, whatever its casing, and nothing else:
// not another login's, and not one linked to someone else that still stores
// the login. Once linked, the membership follows the user and the login alone
// no longer opens it.
func TestLinkMemberships(t *testing.T) {
	m := setupProjectModels(t)
	erin := testUserID("erin")

	asMember, _ := m.InsertProject(data.Project{Name: "As member", Slug: "as-member"})
	asOwner, _ := m.InsertProject(data.Project{Name: "As owner", Slug: "as-owner"})
	others, _ := m.InsertProject(data.Project{Name: "Others", Slug: "others"})

	memberRow := insertUnlinked(t, m, asMember.ID, "erin", data.RoleMember)
	ownerRow := insertUnlinked(t, m, asOwner.ID, "erin", data.RoleOwner)
	frankRow := insertUnlinked(t, m, others.ID, "frank", data.RoleMember)
	// Someone who was erin once, linked before renaming.
	formerErin, err := m.InsertProjectMember(data.ProjectMember{ProjectID: others.ID, UserID: 77, GithubLogin: "erin", Role: data.RoleOwner})
	if err != nil {
		t.Fatalf("InsertProjectMember former erin: %v", err)
	}

	links, err := m.LinkMemberships(erin, "Erin")
	if err != nil {
		t.Fatalf("LinkMemberships: %v", err)
	}
	if links != (data.MembershipLinks{Linked: 2}) {
		t.Errorf("LinkMemberships = %+v, want 2 linked", links)
	}

	for _, tc := range []struct {
		name string
		id   primitive.ObjectID
		want int64
	}{
		{"erin's membership", memberRow, erin},
		{"erin's ownership", ownerRow, erin},
		{"frank's membership", frankRow, 0},
		{"the former erin's membership", formerErin.ID, 77},
	} {
		if got := memberDoc(t, tc.id).UserID; got != tc.want {
			t.Errorf("%s: user_id = %d, want %d", tc.name, got, tc.want)
		}
	}
	if role := memberDoc(t, ownerRow).Role; role != data.RoleOwner {
		t.Errorf("linking changed erin's role to %q", role)
	}

	for _, tc := range []struct {
		name   string
		userID int64
		login  string
		want   string
	}{
		{"erin after a rename", erin, "erin-renamed", data.RoleMember},
		{"another account with erin's login", erin + 1, "erin", ""},
	} {
		if role, err := m.MemberRole(asMember.ID, tc.userID, tc.login); err != nil || role != tc.want {
			t.Errorf("%s: role=%q err=%v, want %q", tc.name, role, err, tc.want)
		}
	}

	// A second sign-in has nothing left to link.
	if links, err := m.LinkMemberships(erin, "erin"); err != nil || links != (data.MembershipLinks{}) {
		t.Errorf("LinkMemberships again = %+v, %v; want nothing linked", links, err)
	}
}

// TestLinkMemberships_MergesIntoTheUsersMembership: a user already linked to a
// project under another login, who also holds a login-only membership there,
// ends up with one membership: the linked one, with the higher role and the
// older join date of the two.
func TestLinkMemberships_MergesIntoTheUsersMembership(t *testing.T) {
	m := setupProjectModels(t)
	erin := testUserID("erin")

	p, _ := m.InsertProject(data.Project{Name: "Both", Slug: "both"})
	addMember(t, m, p.ID, "olivia", data.RoleOwner)
	unlinked := insertUnlinked(t, m, p.ID, "erin", data.RoleOwner)
	linked, err := m.InsertProjectMember(data.ProjectMember{ProjectID: p.ID, UserID: erin, GithubLogin: "erin-before", Role: data.RoleMember})
	if err != nil {
		t.Fatalf("InsertProjectMember erin, linked: %v", err)
	}
	joined := memberDoc(t, unlinked).CreatedAt

	links, err := m.LinkMemberships(erin, "erin")
	if err != nil {
		t.Fatalf("LinkMemberships: %v", err)
	}
	if links != (data.MembershipLinks{Merged: 1}) {
		t.Errorf("LinkMemberships = %+v, want 1 merged", links)
	}

	members, err := m.GetProjectMembers(p.ID)
	if err != nil {
		t.Fatalf("GetProjectMembers: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("members = %+v, want olivia and one membership for erin", members)
	}
	got := memberDoc(t, linked.ID)
	if got.UserID != erin || got.Role != data.RoleOwner || !got.CreatedAt.Equal(joined) {
		t.Errorf("erin's membership = %+v, want user %d, owner, joined %v", got, erin, joined)
	}
}

// TestLinkMemberships_ConcurrentSignIns: sign-ins of one user at once link each
// membership once, and leave one membership per project.
func TestLinkMemberships_ConcurrentSignIns(t *testing.T) {
	m := setupProjectModels(t)
	erin := testUserID("erin")

	const projects = 5
	for i := range projects {
		p, _ := m.InsertProject(data.Project{Name: fmt.Sprintf("P%d", i), Slug: fmt.Sprintf("p%d", i)})
		insertUnlinked(t, m, p.ID, "erin", data.RoleMember)
	}

	const n = 10
	var wg sync.WaitGroup
	var mu sync.Mutex
	var total int64
	errs := make(chan error, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			links, err := m.LinkMemberships(erin, "erin")
			if err != nil {
				errs <- err
				return
			}
			mu.Lock()
			total += links.Linked + links.Merged
			mu.Unlock()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent LinkMemberships: %v", err)
	}
	if total != projects {
		t.Errorf("concurrent sign-ins linked %d memberships, want %d", total, projects)
	}

	userProjects, err := m.GetProjectsForUser(erin, "erin-renamed")
	if err != nil || len(userProjects) != projects {
		t.Errorf("GetProjectsForUser after a rename = %d projects, %v; want %d", len(userProjects), err, projects)
	}
}
