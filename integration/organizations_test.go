//go:build integration

package integration

import (
	"context"
	"errors"
	"net/rpc"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"

	"logwolf-toolbox/data"
)

// setupOrganizationModels returns a data.Models on the shared data-layer MongoDB
// with the organization collections and users cleared, and the organization
// indexes recreated.
func setupOrganizationModels(t *testing.T) (data.Models, *mongo.Database) {
	t.Helper()

	client := testMongo(t, sharedModelsMongo(t))
	db := client.Database("logs")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for _, name := range []string{"organizations", "organization_members", "users"} {
		if err := db.Collection(name).Drop(ctx); err != nil {
			t.Fatalf("setupOrganizationModels: drop %s: %v", name, err)
		}
	}

	m := data.New(client)
	if err := m.EnsureOrganizationIndexes(); err != nil {
		t.Fatalf("setupOrganizationModels: indexes: %v", err)
	}
	if err := m.EnsureUserIndexes(); err != nil {
		t.Fatalf("setupOrganizationModels: user indexes: %v", err)
	}
	return m, db
}

func createOrganization(t *testing.T, m data.Models, name string, ownerID int64, owner string) *data.Organization {
	t.Helper()
	org, err := m.CreateOrganizationWithOwner(data.Organization{Name: name, Plan: "free"}, ownerID, owner)
	if err != nil {
		t.Fatalf("CreateOrganizationWithOwner %s: %v", name, err)
	}
	return org
}

func addOrganizationMember(t *testing.T, m data.Models, orgID primitive.ObjectID, userID int64, login, role string) primitive.ObjectID {
	t.Helper()
	mb, err := m.InsertOrganizationMember(data.OrganizationMember{OrganizationID: orgID, UserID: userID, GithubLogin: login, Role: role})
	if err != nil {
		t.Fatalf("InsertOrganizationMember %s: %v", login, err)
	}
	return mb.ID
}

// organizationMemberID returns the id of the membership of userID in the
// organization.
func organizationMemberID(t *testing.T, m data.Models, orgID primitive.ObjectID, userID int64) primitive.ObjectID {
	t.Helper()
	members, err := m.GetOrganizationMembers(orgID)
	if err != nil {
		t.Fatalf("GetOrganizationMembers: %v", err)
	}
	for _, mb := range members {
		if mb.UserID == userID {
			return mb.ID
		}
	}
	t.Fatalf("user %d is not a member of organization %s", userID, orgID.Hex())
	return primitive.NilObjectID
}

func TestCreateOrganizationWithOwner(t *testing.T) {
	m, db := setupOrganizationModels(t)

	org := createOrganization(t, m, "Acme", 2001, "Alice")
	if org.ID.IsZero() || org.CreatedAt.IsZero() || org.Plan != "free" {
		t.Fatalf("CreateOrganizationWithOwner returned %+v", org)
	}

	got, err := m.GetOrganization(org.ID)
	if err != nil {
		t.Fatalf("GetOrganization: %v", err)
	}
	if got.Name != "Acme" || got.Plan != "free" || got.BillingCustomerID != "" {
		t.Errorf("GetOrganization = %+v, want Acme on free with no billing customer", got)
	}

	// The fields are stored under the names the issue fixes, billing_customer_id
	// included, empty, so a later query on it needs no $exists.
	var raw bson.M
	if err := db.Collection("organizations").FindOne(context.Background(), bson.M{"_id": org.ID}).Decode(&raw); err != nil {
		t.Fatalf("raw organization: %v", err)
	}
	for _, field := range []string{"name", "plan", "billing_customer_id", "created_at"} {
		if _, ok := raw[field]; !ok {
			t.Errorf("stored organization has no %s: %v", field, raw)
		}
	}

	members, err := m.GetOrganizationMembers(org.ID)
	if err != nil {
		t.Fatalf("GetOrganizationMembers: %v", err)
	}
	if len(members) != 1 || members[0].UserID != 2001 || members[0].Role != data.RoleOwner || members[0].GithubLogin != "alice" {
		t.Errorf("members = %+v, want alice (2001) alone, as owner", members)
	}
	if members[0].OrganizationID != org.ID {
		t.Errorf("owner membership filed under %s, want %s", members[0].OrganizationID.Hex(), org.ID.Hex())
	}
}

// The owner membership is part of the organization's transaction: if it cannot
// be written, the organization is not written either.
func TestCreateOrganizationWithOwner_AllOrNothing(t *testing.T) {
	m, db := setupOrganizationModels(t)
	ctx := context.Background()

	// A validator that refuses every membership makes the second insert fail.
	if err := db.RunCommand(ctx, bson.D{
		{Key: "create", Value: "organization_members"},
		{Key: "validator", Value: bson.M{"role": "never"}},
	}).Err(); err != nil {
		// EnsureOrganizationIndexes has created the collection already.
		if err := db.RunCommand(ctx, bson.D{
			{Key: "collMod", Value: "organization_members"},
			{Key: "validator", Value: bson.M{"role": "never"}},
		}).Err(); err != nil {
			t.Fatalf("set validator: %v", err)
		}
	}
	t.Cleanup(func() { db.Collection("organization_members").Drop(context.Background()) })

	if _, err := m.CreateOrganizationWithOwner(data.Organization{Name: "Doomed", Plan: "free"}, 2002, "bob"); err == nil {
		t.Fatal("CreateOrganizationWithOwner succeeded though the owner could not be written")
	}
	n, err := db.Collection("organizations").CountDocuments(ctx, bson.M{"name": "Doomed"})
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("an organization was left without its owner")
	}
}

func TestGetOrganization_NotFound(t *testing.T) {
	m, _ := setupOrganizationModels(t)

	if _, err := m.GetOrganization(primitive.NewObjectID()); !errors.Is(err, mongo.ErrNoDocuments) {
		t.Errorf("GetOrganization of an unknown id: want mongo.ErrNoDocuments, got %v", err)
	}
	exists, err := m.OrganizationExists(context.Background(), primitive.NewObjectID())
	if err != nil || exists {
		t.Errorf("OrganizationExists of an unknown id = %v, %v; want false", exists, err)
	}
}

func TestRenameOrganization(t *testing.T) {
	m, _ := setupOrganizationModels(t)

	created := createOrganization(t, m, "Old", 2003, "carol")
	// As stored: MongoDB keeps created_at to the millisecond.
	org, err := m.GetOrganization(created.ID)
	if err != nil {
		t.Fatalf("GetOrganization: %v", err)
	}
	renamed, err := m.RenameOrganization(org.ID, " New ")
	if err != nil {
		t.Fatalf("RenameOrganization: %v", err)
	}
	if renamed.Name != "New" || renamed.Plan != org.Plan || !renamed.CreatedAt.Equal(org.CreatedAt) {
		t.Errorf("RenameOrganization = %+v, want the same organization named New", renamed)
	}

	if _, err := m.RenameOrganization(primitive.NewObjectID(), "Ghost"); !errors.Is(err, mongo.ErrNoDocuments) {
		t.Errorf("RenameOrganization of an unknown id: want mongo.ErrNoDocuments, got %v", err)
	}
}

// One membership per user per organization, whatever the role or login, and
// even for writes that bypass InsertOrganizationMember.
func TestOrganizationMembers_UniquePerUser(t *testing.T) {
	m, db := setupOrganizationModels(t)

	org := createOrganization(t, m, "Uniq", 2004, "dave")
	addOrganizationMember(t, m, org.ID, 2005, "erin", data.RoleMember)

	_, err := m.InsertOrganizationMember(data.OrganizationMember{OrganizationID: org.ID, UserID: 2005, GithubLogin: "erin-renamed", Role: data.RoleAdmin})
	if !mongo.IsDuplicateKeyError(err) {
		t.Errorf("second membership of user 2005: want a duplicate key error, got %v", err)
	}

	_, err = db.Collection("organization_members").InsertOne(context.Background(),
		bson.M{"organization_id": org.ID, "user_id": int64(2004), "role": data.RoleMember})
	if !mongo.IsDuplicateKeyError(err) {
		t.Errorf("raw second membership of the owner: want a duplicate key error, got %v", err)
	}

	// The same user in another organization is a different membership.
	other := createOrganization(t, m, "Other", 2006, "frank")
	addOrganizationMember(t, m, other.ID, 2005, "erin", data.RoleMember)
}

func TestOrganizationMembers_LastOwner(t *testing.T) {
	m, _ := setupOrganizationModels(t)

	org := createOrganization(t, m, "Solo", 2007, "grace")
	owner := organizationMemberID(t, m, org.ID, 2007)

	if err := m.RemoveOrganizationMember(org.ID, owner, data.RoleOwner); !errors.Is(err, data.ErrLastOrganizationOwner) {
		t.Errorf("removing the last owner: want ErrLastOrganizationOwner, got %v", err)
	}
	for _, role := range []string{data.RoleAdmin, data.RoleMember} {
		if err := m.UpdateOrganizationMemberRole(org.ID, owner, role, data.RoleOwner); !errors.Is(err, data.ErrLastOrganizationOwner) {
			t.Errorf("demoting the last owner to %s: want ErrLastOrganizationOwner, got %v", role, err)
		}
	}
	// Setting the role they hold changes nothing, and is no error.
	if err := m.UpdateOrganizationMemberRole(org.ID, owner, data.RoleOwner, data.RoleOwner); err != nil {
		t.Errorf("setting the last owner's own role: %v", err)
	}

	// An admin is not an owner: the organization still has just one.
	admin := addOrganizationMember(t, m, org.ID, 2008, "heidi", data.RoleAdmin)
	if err := m.RemoveOrganizationMember(org.ID, owner, data.RoleOwner); !errors.Is(err, data.ErrLastOrganizationOwner) {
		t.Errorf("removing the last owner beside an admin: want ErrLastOrganizationOwner, got %v", err)
	}

	// With a second owner, the first may go.
	if err := m.UpdateOrganizationMemberRole(org.ID, admin, data.RoleOwner, data.RoleOwner); err != nil {
		t.Fatalf("promoting the admin: %v", err)
	}
	if err := m.RemoveOrganizationMember(org.ID, owner, data.RoleOwner); err != nil {
		t.Errorf("removing an owner beside another: %v", err)
	}

	members, err := m.GetOrganizationMembers(org.ID)
	if err != nil {
		t.Fatalf("GetOrganizationMembers: %v", err)
	}
	if len(members) != 1 || members[0].UserID != 2008 || members[0].Role != data.RoleOwner {
		t.Errorf("members = %+v, want heidi alone, as owner", members)
	}
}

func TestOrganizationMembers_NotFound(t *testing.T) {
	m, _ := setupOrganizationModels(t)

	org := createOrganization(t, m, "NF", 2009, "ivan")
	other := createOrganization(t, m, "Elsewhere", 2010, "judy")
	foreign := organizationMemberID(t, m, other.ID, 2010)

	// A membership of another organization is no membership of this one.
	for name, id := range map[string]primitive.ObjectID{"unknown": primitive.NewObjectID(), "foreign": foreign} {
		if err := m.RemoveOrganizationMember(org.ID, id, data.RoleOwner); !errors.Is(err, mongo.ErrNoDocuments) {
			t.Errorf("RemoveOrganizationMember %s: want mongo.ErrNoDocuments, got %v", name, err)
		}
		if err := m.UpdateOrganizationMemberRole(org.ID, id, data.RoleAdmin, data.RoleOwner); !errors.Is(err, mongo.ErrNoDocuments) {
			t.Errorf("UpdateOrganizationMemberRole %s: want mongo.ErrNoDocuments, got %v", name, err)
		}
	}
}

// Removing both owners of an organization at once: each removal is allowed on
// its own, but only one of them may win. A single round rarely hits the window,
// so it runs many.
func TestRemoveOrganizationMember_ConcurrentOwners(t *testing.T) {
	m, _ := setupOrganizationModels(t)

	for round := 0; round < 25; round++ {
		org := createOrganization(t, m, "Race", 2100, "owner-a")
		owners := []primitive.ObjectID{
			organizationMemberID(t, m, org.ID, 2100),
			addOrganizationMember(t, m, org.ID, 2101, "owner-b", data.RoleOwner),
		}

		start := make(chan struct{})
		errs := make([]error, len(owners))
		var wg sync.WaitGroup
		for i, owner := range owners {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				errs[i] = m.RemoveOrganizationMember(org.ID, owner, data.RoleOwner)
			}()
		}
		close(start)
		wg.Wait()

		var removed, refused int
		for _, err := range errs {
			switch {
			case err == nil:
				removed++
			case errors.Is(err, data.ErrLastOrganizationOwner):
				refused++
			default:
				t.Fatalf("round %d: RemoveOrganizationMember: %v", round, err)
			}
		}
		if removed != 1 || refused != 1 {
			t.Fatalf("round %d: want one removal and one ErrLastOrganizationOwner, got %d and %d", round, removed, refused)
		}
	}
}

// A member who has signed in is listed under the login of their last sign-in;
// one who has not keeps the login they were added under.
func TestGetOrganizationMembers_CurrentLogins(t *testing.T) {
	m, _ := setupOrganizationModels(t)

	org := createOrganization(t, m, "Logins", 2011, "kim")
	addOrganizationMember(t, m, org.ID, 2012, "never-signed-in", data.RoleMember)
	if _, err := m.UpsertUser(2011, "Kim-Renamed", ""); err != nil {
		t.Fatalf("UpsertUser: %v", err)
	}

	members, err := m.GetOrganizationMembers(org.ID)
	if err != nil {
		t.Fatalf("GetOrganizationMembers: %v", err)
	}
	logins := map[int64]string{}
	for _, mb := range members {
		logins[mb.UserID] = mb.GithubLogin
	}
	if logins[2011] != "kim-renamed" || logins[2012] != "never-signed-in" {
		t.Errorf("logins = %v, want 2011 kim-renamed and 2012 never-signed-in", logins)
	}

	empty, err := m.GetOrganizationMembers(primitive.NewObjectID())
	if err != nil || empty == nil || len(empty) != 0 {
		t.Errorf("GetOrganizationMembers of an unknown organization = %#v, %v; want an empty list", empty, err)
	}
}

func TestOrganizationRoleAndUserOrganizations(t *testing.T) {
	m, db := setupOrganizationModels(t)

	first := createOrganization(t, m, "First", 2013, "leo")
	second := createOrganization(t, m, "Second", 2014, "mia")
	addOrganizationMember(t, m, second.ID, 2013, "leo", data.RoleAdmin)
	createOrganization(t, m, "Unrelated", 2015, "ned")

	for _, tc := range []struct {
		org  primitive.ObjectID
		user int64
		want string
	}{
		{first.ID, 2013, data.RoleOwner},
		{second.ID, 2013, data.RoleAdmin},
		{second.ID, 2015, ""},
		{primitive.NewObjectID(), 2013, ""},
	} {
		role, err := m.OrganizationRole(tc.org, tc.user)
		if err != nil || role != tc.want {
			t.Errorf("OrganizationRole(%s, %d) = %q, %v; want %q", tc.org.Hex(), tc.user, role, err, tc.want)
		}
	}

	orgs, err := m.GetOrganizationsForUser(2013)
	if err != nil {
		t.Fatalf("GetOrganizationsForUser: %v", err)
	}
	if len(orgs) != 2 || orgs[0].ID != first.ID || orgs[0].Role != data.RoleOwner ||
		orgs[1].ID != second.ID || orgs[1].Role != data.RoleAdmin {
		t.Errorf("GetOrganizationsForUser(2013) = %+v, want First as owner then Second as admin", orgs)
	}

	// A membership that outlived its organization lists nothing.
	if _, err := db.Collection("organizations").DeleteOne(context.Background(), bson.M{"_id": second.ID}); err != nil {
		t.Fatalf("delete organization: %v", err)
	}
	orgs, err = m.GetOrganizationsForUser(2013)
	if err != nil || len(orgs) != 1 || orgs[0].ID != first.ID {
		t.Errorf("GetOrganizationsForUser after a deletion = %+v, %v; want First alone", orgs, err)
	}

	none, err := m.GetOrganizationsForUser(2999)
	if err != nil || none == nil || len(none) != 0 {
		t.Errorf("GetOrganizationsForUser of a stranger = %#v, %v; want an empty list", none, err)
	}
}

// TestOrganizationRPC goes through the logger's RPC server, which also proves
// the logger created the organization indexes at startup.
func TestOrganizationRPC(t *testing.T) {
	stack := sharedStack(t)

	conn, err := rpc.Dial("tcp", stack.loggerRPCAddr)
	if err != nil {
		t.Fatalf("dial logger RPC: %v", err)
	}
	defer conn.Close()

	// The stack's database is shared, so the ids are ones no other test uses.
	owner := time.Now().UnixNano()
	member := owner + 1

	var org data.Organization
	if err := conn.Call("RPCServer.CreateOrganization", data.RPCCreateOrganizationArgs{
		Name: "RPC Org", Plan: "free", OwnerID: owner, Owner: "RPC-Owner",
	}, &org); err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}
	if org.ID.IsZero() || org.Name != "RPC Org" || org.Plan != "free" {
		t.Fatalf("CreateOrganization reply: %+v", org)
	}
	orgID := org.ID.Hex()

	access := func(userID int64, id string) data.OrganizationAccess {
		t.Helper()
		var reply data.OrganizationAccess
		if err := conn.Call("RPCServer.OrganizationAccess", data.RPCOrganizationAccessArgs{OrganizationID: id, UserID: userID}, &reply); err != nil {
			t.Fatalf("OrganizationAccess(%d, %s): %v", userID, id, err)
		}
		return reply
	}
	if got := access(owner, orgID); got != (data.OrganizationAccess{Exists: true, Role: data.RoleOwner}) {
		t.Errorf("owner's access = %+v", got)
	}
	if got := access(member, orgID); got != (data.OrganizationAccess{Exists: true}) {
		t.Errorf("non-member's access = %+v, want the organization to exist with no role", got)
	}
	if got := access(owner, primitive.NewObjectID().Hex()); got != (data.OrganizationAccess{}) {
		t.Errorf("access to an unknown organization = %+v, want it not to exist", got)
	}

	var ok string
	if err := conn.Call("RPCServer.AddOrganizationMember", data.RPCAddOrganizationMemberArgs{
		OrganizationID: orgID, UserID: member, GithubLogin: "RPC-Member", Role: data.RoleMember,
	}, &ok); err != nil {
		t.Fatalf("AddOrganizationMember: %v", err)
	}
	err = conn.Call("RPCServer.AddOrganizationMember", data.RPCAddOrganizationMemberArgs{
		OrganizationID: orgID, UserID: member, GithubLogin: "rpc-member", Role: data.RoleAdmin,
	}, &ok)
	if err == nil {
		t.Error("AddOrganizationMember accepted a second membership of one user")
	}

	var members []data.OrganizationMember
	if err := conn.Call("RPCServer.ListOrganizationMembers", data.RPCOrganizationIDArgs{ID: orgID}, &members); err != nil {
		t.Fatalf("ListOrganizationMembers: %v", err)
	}
	var memberID string
	for _, mb := range members {
		if mb.UserID == member {
			memberID = mb.ID.Hex()
		}
	}
	if len(members) != 2 || memberID == "" {
		t.Fatalf("ListOrganizationMembers = %+v, want the owner and the member", members)
	}

	if err := conn.Call("RPCServer.UpdateOrganizationMemberRole", data.RPCUpdateOrganizationMemberRoleArgs{
		OrganizationID: orgID, MemberID: memberID, Role: data.RoleAdmin,
	}, &ok); err != nil {
		t.Fatalf("UpdateOrganizationMemberRole: %v", err)
	}
	if got := access(member, orgID); got.Role != data.RoleAdmin {
		t.Errorf("member's role after the change = %q, want admin", got.Role)
	}

	var orgs []data.UserOrganization
	if err := conn.Call("RPCServer.ListUserOrganizations", data.RPCUserOrganizationsArgs{UserID: member}, &orgs); err != nil {
		t.Fatalf("ListUserOrganizations: %v", err)
	}
	if len(orgs) != 1 || orgs[0].ID != org.ID || orgs[0].Role != data.RoleAdmin {
		t.Errorf("ListUserOrganizations = %+v, want RPC Org as admin", orgs)
	}

	var renamed data.Organization
	if err := conn.Call("RPCServer.UpdateOrganization", data.RPCUpdateOrganizationArgs{ID: orgID, Name: "Renamed"}, &renamed); err != nil {
		t.Fatalf("UpdateOrganization: %v", err)
	}
	var got data.Organization
	if err := conn.Call("RPCServer.GetOrganization", data.RPCOrganizationIDArgs{ID: orgID}, &got); err != nil {
		t.Fatalf("GetOrganization: %v", err)
	}
	if got.Name != "Renamed" || got.Plan != "free" {
		t.Errorf("GetOrganization after the rename = %+v", got)
	}

	if err := conn.Call("RPCServer.RemoveOrganizationMember", data.RPCRemoveOrganizationMemberArgs{
		OrganizationID: orgID, MemberID: memberID,
	}, &ok); err != nil {
		t.Fatalf("RemoveOrganizationMember: %v", err)
	}
	if got := access(member, orgID); got.Role != "" {
		t.Errorf("removed member still has role %q", got.Role)
	}

	client := testMongo(t, stack.mongoURI)
	indexes, err := client.Database("logs").Collection("organization_members").Indexes().ListSpecifications(context.Background())
	if err != nil {
		t.Fatalf("list organization_members indexes: %v", err)
	}
	unique := false
	for _, ix := range indexes {
		if ix.Name == "unique_organization_member" && ix.Unique != nil && *ix.Unique {
			unique = true
		}
	}
	if !unique {
		t.Errorf("logger did not create the unique (organization_id, user_id) index; indexes: %+v", indexes)
	}
}

// TestProjectPlan: a project's plan is its organization's, read afresh on each
// call, so a plan change shows at once. A project outside any organization has
// no plan, rather than an empty one.
func TestProjectPlan(t *testing.T) {
	m, db := setupOrganizationModels(t)
	ctx := context.Background()

	org := createOrganization(t, m, "Planned", 2101, "planner")
	project, err := m.InsertProject(data.Project{Name: "Planned", Slug: "planned", OrganizationID: org.ID})
	if err != nil {
		t.Fatalf("InsertProject: %v", err)
	}
	if plan, err := m.ProjectPlan(ctx, project.ID); err != nil || plan != "free" {
		t.Errorf("ProjectPlan = %q, %v; want free, the organization's", plan, err)
	}

	if _, err := db.Collection("organizations").UpdateOne(ctx, bson.M{"_id": org.ID}, bson.M{"$set": bson.M{"plan": "pro"}}); err != nil {
		t.Fatalf("change plan: %v", err)
	}
	if plan, err := m.ProjectPlan(ctx, project.ID); err != nil || plan != "pro" {
		t.Errorf("ProjectPlan after a plan change = %q, %v; want pro", plan, err)
	}

	loose, err := m.InsertProject(data.Project{Name: "Loose", Slug: "loose"})
	if err != nil {
		t.Fatalf("InsertProject: %v", err)
	}
	orphan, err := m.InsertProject(data.Project{Name: "Orphan", Slug: "orphan", OrganizationID: primitive.NewObjectID()})
	if err != nil {
		t.Fatalf("InsertProject: %v", err)
	}
	for name, id := range map[string]primitive.ObjectID{"no organization": loose.ID, "a missing organization": orphan.ID} {
		if plan, err := m.ProjectPlan(ctx, id); !errors.Is(err, data.ErrUnknownOrganization) {
			t.Errorf("ProjectPlan of a project in %s = %q, %v; want ErrUnknownOrganization", name, plan, err)
		}
	}

	if plan, err := m.ProjectPlan(ctx, primitive.NewObjectID()); !errors.Is(err, mongo.ErrNoDocuments) {
		t.Errorf("ProjectPlan of an unknown project = %q, %v; want mongo.ErrNoDocuments", plan, err)
	}
}

// TestProjectPlanRPC: a project created through the logger is in the Default
// organization, so on a self-hosted deployment its plan is the self-hosted one.
func TestProjectPlanRPC(t *testing.T) {
	stack := sharedStack(t)

	conn, err := rpc.Dial("tcp", stack.loggerRPCAddr)
	if err != nil {
		t.Fatalf("dial logger RPC: %v", err)
	}
	defer conn.Close()

	var project data.Project
	if err := conn.Call("RPCServer.CreateProject", data.RPCCreateProjectArgs{
		Name: "Plan RPC", Slug: "plan-rpc", OwnerID: time.Now().UnixNano(), Owner: "plan-rpc-owner",
	}, &project); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	var plan string
	if err := conn.Call("RPCServer.ProjectPlan", data.RPCProjectIDArgs{ID: project.ID.Hex()}, &plan); err != nil {
		t.Fatalf("ProjectPlan: %v", err)
	}
	if plan != data.SelfHostedPlan {
		t.Errorf("ProjectPlan = %q, want %q", plan, data.SelfHostedPlan)
	}

	if err := conn.Call("RPCServer.ProjectPlan", data.RPCProjectIDArgs{ID: "not-a-project"}, &plan); err == nil {
		t.Error("ProjectPlan accepted a malformed project id")
	}
}
