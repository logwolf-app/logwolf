//go:build integration

package integration

import (
	"context"
	"net/rpc"
	"slices"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"

	"logwolf-toolbox/data"
)

// TestStartupMigration_PutsEveryProjectInAnOrganization seeds a database in the
// shape a build from before organizations left behind: the Default project,
// owned by one user linked to a user ID and one login-only, and two more
// projects of other users, none in an organization. After the first boot one
// organization holds every project, owned by Default's owners: the linked one
// at once, the login-only one at their first sign-in.
func TestStartupMigration_PutsEveryProjectInAnOrganization(t *testing.T) {
	mongoURI, client := migrationMongo(t)
	db := client.Database("logs")

	defaultID := seedLegacyProject(t, db, "Default", data.DefaultProjectSlug, true)
	appID := seedLegacyProject(t, db, "App", "app", false)
	toolsID := seedLegacyProject(t, db, "Tools", "tools", false)

	seedLegacyMember(t, db, defaultID, 101, "alice", data.RoleOwner)
	seedLegacyMember(t, db, defaultID, 0, "bob", data.RoleOwner)
	seedLegacyMember(t, db, defaultID, 303, "carol", data.RoleMember)
	seedLegacyMember(t, db, appID, 303, "carol", data.RoleOwner)
	seedLegacyMember(t, db, toolsID, 0, "dave", data.RoleOwner)

	// The allowlist names more users than own Default: only Default's owners
	// own the organization.
	rpcAddr := startLogger(t, mongoURI, "alice,bob,carol,dave")

	org := requireDefaultOrganization(t, db)
	if org.Name != data.DefaultOrganizationName || org.Plan != data.SelfHostedPlan || org.BillingCustomerID != "" {
		t.Errorf("organization = %+v, want %q on plan %q, billed to nobody", org, data.DefaultOrganizationName, data.SelfHostedPlan)
	}
	assertCount(t, db, "projects", bson.M{}, 3)
	assertCount(t, db, "projects", bson.M{"organization_id": org.ID}, 3)

	assertCount(t, db, "organization_members", bson.M{}, 1)
	assertOrganizationOwner(t, db, org.ID, 101)
	if !slices.Equal(org.PendingOwners, []string{"bob"}) {
		t.Errorf("pending owners = %v, want [bob], Default's login-only owner", org.PendingOwners)
	}

	// --- bob signs in: the ownership waiting for his login becomes his ---

	conn, err := rpc.Dial("tcp", rpcAddr)
	if err != nil {
		t.Fatalf("dial logger RPC: %v", err)
	}
	defer conn.Close()

	var user data.User
	if err := conn.Call("RPCServer.UpsertUser", data.RPCUpsertUserArgs{GithubID: 202, GithubLogin: "Bob"}, &user); err != nil {
		t.Fatalf("sign in as Bob: %v", err)
	}
	assertOrganizationOwner(t, db, org.ID, 202)
	if org := requireDefaultOrganization(t, db); len(org.PendingOwners) != 0 {
		t.Errorf("pending owners after bob's sign-in = %v, want none", org.PendingOwners)
	}

	// A second sign-in claims nothing more.
	if err := conn.Call("RPCServer.UpsertUser", data.RPCUpsertUserArgs{GithubID: 202, GithubLogin: "bob"}, &user); err != nil {
		t.Fatalf("sign in as bob again: %v", err)
	}
	assertCount(t, db, "organization_members", bson.M{"organization_id": org.ID}, 2)

	// --- Second boot: idempotent ---

	startLogger(t, mongoURI, "alice,bob,carol,dave,erin")

	assertCount(t, db, "organizations", bson.M{}, 1)
	assertCount(t, db, "projects", bson.M{"organization_id": org.ID}, 3)
	assertCount(t, db, "organization_members", bson.M{"organization_id": org.ID}, 2)
	if org := requireDefaultOrganization(t, db); len(org.PendingOwners) != 0 {
		t.Errorf("pending owners after the second boot = %v, want none: the organization has owners", org.PendingOwners)
	}
}

// TestStartupMigration_LegacyDataEndsInTheOrganization: a deployment upgraded
// from before multi-tenancy gets its Default project and its organization in
// the same start, the project inside the organization, and the configured
// owners, login-only, pending on both.
func TestStartupMigration_LegacyDataEndsInTheOrganization(t *testing.T) {
	mongoURI, client := migrationMongo(t)
	db := client.Database("logs")

	seedOrphanLog(t, db)

	startLogger(t, mongoURI, "alice,Bob")

	project := requireDefaultProject(t, db)
	org := requireDefaultOrganization(t, db)
	if project.OrganizationID != org.ID {
		t.Errorf("Default project organization = %s, want %s", project.OrganizationID.Hex(), org.ID.Hex())
	}
	if !slices.Equal(org.PendingOwners, []string{"alice", "bob"}) {
		t.Errorf("pending owners = %v, want [alice bob]", org.PendingOwners)
	}
	assertCount(t, db, "organization_members", bson.M{}, 0)
}

// TestStartupMigration_NewProjectsJoinTheOrganization: a fresh install gets its
// organization at the first start, before it has any project, so the first one
// created has an organization to go in.
func TestStartupMigration_NewProjectsJoinTheOrganization(t *testing.T) {
	mongoURI, client := migrationMongo(t)
	db := client.Database("logs")

	rpcAddr := startLogger(t, mongoURI, "alice")

	org := requireDefaultOrganization(t, db)
	if !slices.Equal(org.PendingOwners, []string{"alice"}) {
		t.Errorf("pending owners = %v, want [alice], the configured owner", org.PendingOwners)
	}

	conn, err := rpc.Dial("tcp", rpcAddr)
	if err != nil {
		t.Fatalf("dial logger RPC: %v", err)
	}
	defer conn.Close()

	var project data.Project
	if err := conn.Call("RPCServer.CreateProject", data.RPCCreateProjectArgs{
		Name: "Fresh", Slug: "fresh", OwnerID: 707, Owner: "frank",
	}, &project); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if project.OrganizationID != org.ID {
		t.Errorf("new project organization = %s, want %s", project.OrganizationID.Hex(), org.ID.Hex())
	}
	assertCount(t, db, "projects", bson.M{"_id": project.ID, "organization_id": org.ID}, 1)
}

// TestStartupMigration_OrganizationOwnersConfiguredLater: an organization
// created with nobody to own it gets owners once they are configured, and keeps
// them once it has any.
func TestStartupMigration_OrganizationOwnersConfiguredLater(t *testing.T) {
	mongoURI, client := migrationMongo(t)
	db := client.Database("logs")

	startLogger(t, mongoURI, "")

	org := requireDefaultOrganization(t, db)
	if len(org.PendingOwners) != 0 {
		t.Fatalf("pending owners = %v, want none: nobody is configured", org.PendingOwners)
	}

	startLoggerWithOwners(t, mongoURI, "", "dave")

	if org := requireDefaultOrganization(t, db); !slices.Equal(org.PendingOwners, []string{"dave"}) {
		t.Errorf("pending owners = %v, want [dave]", org.PendingOwners)
	}

	startLoggerWithOwners(t, mongoURI, "alice", "dave")

	if org := requireDefaultOrganization(t, db); !slices.Equal(org.PendingOwners, []string{"dave"}) {
		t.Errorf("pending owners = %v, want [dave]: an organization with owners keeps them", org.PendingOwners)
	}
	assertCount(t, db, "organizations", bson.M{}, 1)
}

// seedLegacyProject inserts a project as a build from before organizations
// stored it: no organization_id.
func seedLegacyProject(t *testing.T, db *mongo.Database, name, slug string, isDefault bool) primitive.ObjectID {
	t.Helper()

	doc := bson.M{"_id": primitive.NewObjectID(), "name": name, "slug": slug, "created_at": time.Now()}
	if isDefault {
		doc["default"] = true
	}
	if _, err := db.Collection("projects").InsertOne(context.Background(), doc); err != nil {
		t.Fatalf("seed project %s: %v", slug, err)
	}
	return doc["_id"].(primitive.ObjectID)
}

// seedLegacyMember inserts a project membership; a userID of 0 leaves it
// login-only, as memberships stored before user IDs are.
func seedLegacyMember(t *testing.T, db *mongo.Database, projectID primitive.ObjectID, userID int64, login, role string) {
	t.Helper()

	doc := bson.M{"project_id": projectID, "github_login": login, "role": role, "created_at": time.Now()}
	if userID > 0 {
		doc["user_id"] = userID
	}
	if _, err := db.Collection("project_members").InsertOne(context.Background(), doc); err != nil {
		t.Fatalf("seed member %s: %v", login, err)
	}
}

func requireDefaultOrganization(t *testing.T, db *mongo.Database) data.Organization {
	t.Helper()

	var org data.Organization
	if err := db.Collection("organizations").FindOne(context.Background(), bson.M{"default": true}).Decode(&org); err != nil {
		t.Fatalf("the migration should have created the %q organization: %v", data.DefaultOrganizationName, err)
	}
	return org
}

// assertOrganizationOwner fails unless the user with this GitHub user ID holds
// an owner membership of the organization.
func assertOrganizationOwner(t *testing.T, db *mongo.Database, orgID primitive.ObjectID, userID int64) {
	t.Helper()

	var member data.OrganizationMember
	err := db.Collection("organization_members").FindOne(context.Background(),
		bson.M{"organization_id": orgID, "user_id": userID}).Decode(&member)
	if err != nil {
		t.Fatalf("organization membership of user %d: %v", userID, err)
	}
	if member.Role != data.RoleOwner {
		t.Errorf("user %d organization role = %q, want %q", userID, member.Role, data.RoleOwner)
	}
}
