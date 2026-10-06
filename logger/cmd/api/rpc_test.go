package main

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"logwolf-toolbox/data"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// TestProjectAccess_MalformedProjectID verifies that ProjectAccess answers a
// project id that is not an ObjectID hex, or an empty one, with the "invalid
// project ID" error the broker turns into a 404, rather than "not a member".
// That path needs no database, so the zero-value server is enough.
func TestProjectAccess_MalformedProjectID(t *testing.T) {
	srv := &RPCServer{}

	for _, id := range []string{"not-a-valid-object-id", ""} {
		var reply data.ProjectAccess
		err := srv.ProjectAccess(&data.RPCProjectAccessArgs{ProjectID: id, UserID: 583231, GithubLogin: "jpricardo"}, &reply)
		if err == nil || !strings.Contains(err.Error(), "invalid project ID") {
			t.Errorf("ProjectAccess(%q): want an invalid project ID error, got %v", id, err)
		}
		if reply != (data.ProjectAccess{}) {
			t.Errorf("ProjectAccess(%q): reply should stay empty on error, got %+v", id, reply)
		}
	}
}

// TestLogInfo_UnknownProject verifies that an event whose project id names no
// project is refused rather than inserted. An id that is not an ObjectID is
// answered without a database, so the zero-value server is enough here.
func TestLogInfo_UnknownProject(t *testing.T) {
	srv := &RPCServer{}

	var reply string
	err := srv.LogInfo(data.RPCLogPayload{ProjectID: "not-a-project", Name: "stray"}, &reply)
	if !errors.Is(err, data.ErrUnknownProject) {
		t.Errorf("LogInfo for an unknown project: want ErrUnknownProject, got %v", err)
	}
	if reply != "" {
		t.Errorf("reply should stay empty when the event is dropped, got %q", reply)
	}
}

// TestRequestPurge_Queues checks that a deleted project reaches the cleanup
// loop's queue.
func TestRequestPurge_Queues(t *testing.T) {
	purges := make(chan primitive.ObjectID, 1)
	srv := &RPCServer{purges: purges}

	doomed := primitive.NewObjectID()
	srv.requestPurge(doomed)

	select {
	case got := <-purges:
		if got != doomed {
			t.Errorf("queued %s, want %s", got.Hex(), doomed.Hex())
		}
	default:
		t.Fatal("requestPurge queued nothing")
	}
}

// TestRequestPurge_NeverBlocks checks that DeleteProject cannot hang on the
// purge queue: a full queue drops the request (the orphan sweep deletes those
// logs later), and a server with no queue at all asks no one.
func TestRequestPurge_NeverBlocks(t *testing.T) {
	earlier := primitive.NewObjectID()
	full := make(chan primitive.ObjectID, 1)
	full <- earlier

	for name, srv := range map[string]*RPCServer{
		"full queue": {purges: full},
		"no queue":   {},
	} {
		done := make(chan struct{})
		go func() {
			srv.requestPurge(primitive.NewObjectID())
			close(done)
		}()

		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatalf("%s: requestPurge blocked", name)
		}
	}

	if got := <-full; got != earlier {
		t.Errorf("full queue: now holds %s, want the earlier %s", got.Hex(), earlier.Hex())
	}
}

// The API key methods refuse input that could never name a key before they
// touch the database, so the zero-value server answers these.

func TestValidateAPIKey_MalformedKeyIsInvalid(t *testing.T) {
	srv := &RPCServer{}

	var reply data.RPCValidateAPIKeyReply
	if err := srv.ValidateAPIKey(&data.RPCValidateAPIKeyArgs{Plaintext: "lw_short"}, &reply); err != nil {
		t.Fatalf("ValidateAPIKey: %v", err)
	}
	if reply.Valid {
		t.Error("a malformed key was accepted")
	}
}

func TestCreateAPIKey_UnknownScope(t *testing.T) {
	srv := &RPCServer{}

	var reply data.RPCCreateAPIKeyReply
	err := srv.CreateAPIKey(&data.RPCCreateAPIKeyArgs{ProjectID: primitive.NewObjectID().Hex(), Scopes: []string{"admin"}}, &reply)
	if !errors.Is(err, data.ErrInvalidScope) {
		t.Errorf("CreateAPIKey with an unknown scope: want ErrInvalidScope, got %v", err)
	}
	if reply.Plaintext != "" {
		t.Error("a key was handed out despite the error")
	}
}

func TestAPIKeyMethods_MalformedID(t *testing.T) {
	srv := &RPCServer{}

	var reply string
	if err := srv.RevokeAPIKey(&data.RPCRevokeAPIKeyArgs{ProjectID: primitive.NewObjectID().Hex(), ID: "not-an-id"}, &reply); err == nil {
		t.Error("RevokeAPIKey accepted a malformed id")
	}
}

// TestProjectScopedMethods_MalformedProjectID checks that every method taking a
// project id refuses one that is not an ObjectID before touching the database,
// and says so in the words the broker answers with 404.
func TestProjectScopedMethods_MalformedProjectID(t *testing.T) {
	srv := &RPCServer{}
	const bad = "not-a-project"

	for name, call := range map[string]func() error{
		"GetLogs": func() error {
			var reply []data.LogEntry
			return srv.GetLogs(data.QueryParams{ProjectID: bad}, &reply)
		},
		"GetLog": func() error {
			var reply data.LogEntry
			return srv.GetLog(data.RPCLogEntryFilter{ID: primitive.NewObjectID().Hex(), ProjectID: bad}, &reply)
		},
		"DeleteLog": func() error {
			var reply int64
			return srv.DeleteLog(data.RPCLogEntryFilter{ID: primitive.NewObjectID().Hex(), ProjectID: bad}, &reply)
		},
		"GetRetention": func() error {
			var reply int
			return srv.GetRetention(&data.RetentionArgs{ProjectID: bad}, &reply)
		},
		"UpdateRetention": func() error {
			var reply string
			return srv.UpdateRetention(&data.RetentionArgs{ProjectID: bad, Days: 30}, &reply)
		},
		"GetMetrics": func() error {
			var reply data.Metrics
			return srv.GetMetrics(&data.ProjectArgs{ProjectID: bad}, &reply)
		},
		"ListAPIKeys": func() error {
			var reply []data.APIKey
			return srv.ListAPIKeys(&data.ProjectArgs{ProjectID: bad}, &reply)
		},
		"CreateAPIKey": func() error {
			var reply data.RPCCreateAPIKeyReply
			return srv.CreateAPIKey(&data.RPCCreateAPIKeyArgs{ProjectID: bad}, &reply)
		},
		"RevokeAPIKey": func() error {
			var reply string
			return srv.RevokeAPIKey(&data.RPCRevokeAPIKeyArgs{ProjectID: bad, ID: primitive.NewObjectID().Hex()}, &reply)
		},
		"ProjectPlan": func() error {
			var reply string
			return srv.ProjectPlan(&data.RPCProjectIDArgs{ID: bad}, &reply)
		},
		"ProjectQuota": func() error {
			var reply data.ProjectQuota
			return srv.ProjectQuota(&data.RPCProjectIDArgs{ID: bad}, &reply)
		},
	} {
		err := call()
		if err == nil || !strings.Contains(err.Error(), "not a valid ObjectID") {
			t.Errorf("%s: want a \"not a valid ObjectID\" error, got %v", name, err)
		}
	}
}

// TestCreateProject_RequiresOwner checks that no project is created without the
// owner it is created with: one without an owner could never be reached. The
// owner is a user, so it needs their GitHub user ID as well as their login.
func TestCreateProject_RequiresOwner(t *testing.T) {
	srv := &RPCServer{} // zero-value models: reaching MongoDB would panic

	for _, owner := range []struct {
		id    int64
		login string
	}{{583231, ""}, {583231, "   "}, {0, "octocat"}, {-1, "octocat"}} {
		var reply data.Project
		err := srv.CreateProject(&data.RPCCreateProjectArgs{Name: "App", Slug: "app", OwnerID: owner.id, Owner: owner.login}, &reply)
		if err == nil || !strings.Contains(err.Error(), "owner is required") {
			t.Errorf("CreateProject with owner %+v: want \"owner is required\", got %v", owner, err)
		}
	}
}

// TestCreateProject_MalformedOrganizationID: an organization id that is not an
// ObjectID names no organization, and the error says so in the words the
// broker reads as a 404.
func TestCreateProject_MalformedOrganizationID(t *testing.T) {
	srv := &RPCServer{} // zero-value models: reaching MongoDB would panic

	var reply data.Project
	err := srv.CreateProject(&data.RPCCreateProjectArgs{Name: "App", Slug: "app", OwnerID: 583231, Owner: "octocat", OrganizationID: "not-an-id"}, &reply)
	if err == nil || !strings.Contains(err.Error(), "invalid organization ID") || !strings.Contains(err.Error(), "not a valid ObjectID") {
		t.Errorf("CreateProject in a malformed organization: want \"invalid organization ID\", got %v", err)
	}
}

// TestAddMember_RequiresAUser: a new membership names its user by GitHub user
// ID, so one without a usable ID, or without a login to show, is refused before
// the database.
func TestAddMember_RequiresAUser(t *testing.T) {
	srv := &RPCServer{} // zero-value models: reaching MongoDB would panic

	project := primitive.NewObjectID().Hex()
	for _, args := range []data.RPCAddMemberArgs{
		{ProjectID: project, UserID: 0, GithubLogin: "octocat", Role: data.RoleMember},
		{ProjectID: project, UserID: -5, GithubLogin: "octocat", Role: data.RoleMember},
		{ProjectID: project, UserID: 583231, GithubLogin: " ", Role: data.RoleMember},
	} {
		var reply string
		if err := srv.AddMember(&args, &reply); !errors.Is(err, data.ErrInvalidUser) {
			t.Errorf("AddMember(%+v): want ErrInvalidUser, got %v", args, err)
		}
	}
}

// TestMemberChanges_MalformedMemberID: a member id that is not an ObjectID names
// no membership, and the error says so in the words the broker reads as a 404.
func TestMemberChanges_MalformedMemberID(t *testing.T) {
	srv := &RPCServer{} // zero-value models: reaching MongoDB would panic

	project := primitive.NewObjectID().Hex()
	var reply string
	if err := srv.RemoveMember(&data.RPCRemoveMemberArgs{ProjectID: project, MemberID: "octocat"}, &reply); err == nil ||
		!strings.Contains(err.Error(), "not a valid ObjectID") {
		t.Errorf("RemoveMember with a login for an id: want a \"not a valid ObjectID\" error, got %v", err)
	}
	if err := srv.UpdateMemberRole(&data.RPCUpdateMemberRoleArgs{ProjectID: project, MemberID: "583231", Role: data.RoleOwner}, &reply); err == nil ||
		!strings.Contains(err.Error(), "not a valid ObjectID") {
		t.Errorf("UpdateMemberRole with a user ID for an id: want a \"not a valid ObjectID\" error, got %v", err)
	}
}

func TestWithoutHash(t *testing.T) {
	key := data.APIKey{Prefix: "lw_abcdefg", Hash: "$2a$10$secret"}
	if got := withoutHash(key); got.Hash != "" || got.Prefix != key.Prefix {
		t.Errorf("withoutHash = %+v, want the key minus its hash", got)
	}
}

// TestGetLogs_RefusesAnOversizedPage: the logger holds the page bounds itself,
// whoever calls it. The zero-value server has no database, so a page that got
// past the check would panic instead of answering.
func TestGetLogs_RefusesAnOversizedPage(t *testing.T) {
	srv := &RPCServer{}

	var reply []data.LogEntry
	err := srv.GetLogs(data.QueryParams{
		ProjectID:  primitive.NewObjectID().Hex(),
		Pagination: data.PaginationParams{Page: 1, PageSize: data.MaxPageSize + 1},
	}, &reply)
	if !errors.Is(err, data.ErrInvalidPagination) {
		t.Errorf("GetLogs with a page over the cap = %v, want ErrInvalidPagination", err)
	}
}

// TestUserMethods_RefuseAnUnkeyedUser: users are keyed by GitHub user ID, so
// both user methods refuse a call without one before any query. The zero-value
// server has no database, so a call that got past the check would panic.
func TestUserMethods_RefuseAnUnkeyedUser(t *testing.T) {
	srv := &RPCServer{}

	var user data.User
	if err := srv.UpsertUser(&data.RPCUpsertUserArgs{GithubID: 0, GithubLogin: "jdoe"}, &user); !errors.Is(err, data.ErrInvalidUser) {
		t.Errorf("UpsertUser without an id = %v, want ErrInvalidUser", err)
	}
	if err := srv.UpsertUser(&data.RPCUpsertUserArgs{GithubID: 42, GithubLogin: " "}, &user); !errors.Is(err, data.ErrInvalidUser) {
		t.Errorf("UpsertUser without a login = %v, want ErrInvalidUser", err)
	}
	if user != (data.User{}) {
		t.Errorf("reply should stay empty on error, got %+v", user)
	}

	var reply data.RPCGetUserReply
	if err := srv.GetUser(&data.RPCGetUserArgs{GithubID: 0}, &reply); !errors.Is(err, data.ErrInvalidUser) {
		t.Errorf("GetUser without an id = %v, want ErrInvalidUser", err)
	}
	if reply.Found {
		t.Errorf("GetUser without an id should not find anyone, got %+v", reply)
	}
}

// TestOrganizationMethods_MalformedID checks that every method taking an
// organization id refuses one that is not an ObjectID before touching the
// database, and says which id it was, for the broker to answer with 404.
func TestOrganizationMethods_MalformedID(t *testing.T) {
	srv := &RPCServer{} // zero-value models: reaching MongoDB would panic
	const bad = "not-an-organization"
	member := primitive.NewObjectID().Hex()

	for name, call := range map[string]func() error{
		"GetOrganization": func() error {
			var reply data.Organization
			return srv.GetOrganization(&data.RPCOrganizationIDArgs{ID: bad}, &reply)
		},
		"UpdateOrganization": func() error {
			var reply data.Organization
			return srv.UpdateOrganization(&data.RPCUpdateOrganizationArgs{ID: bad, Name: "Acme"}, &reply)
		},
		"OrganizationAccess": func() error {
			var reply data.OrganizationAccess
			return srv.OrganizationAccess(&data.RPCOrganizationAccessArgs{OrganizationID: bad, UserID: 583231}, &reply)
		},
		"ListOrganizationMembers": func() error {
			var reply []data.OrganizationMember
			return srv.ListOrganizationMembers(&data.RPCOrganizationIDArgs{ID: bad}, &reply)
		},
		"OrganizationUsage": func() error {
			var reply data.OrganizationUsage
			return srv.OrganizationUsage(&data.RPCOrganizationIDArgs{ID: bad}, &reply)
		},
		"AddOrganizationMember": func() error {
			var reply string
			return srv.AddOrganizationMember(&data.RPCAddOrganizationMemberArgs{
				OrganizationID: bad, UserID: 583231, GithubLogin: "octocat", Role: data.RoleMember,
			}, &reply)
		},
		"RemoveOrganizationMember": func() error {
			var reply string
			return srv.RemoveOrganizationMember(&data.RPCRemoveOrganizationMemberArgs{OrganizationID: bad, MemberID: member}, &reply)
		},
		"UpdateOrganizationMemberRole": func() error {
			var reply string
			return srv.UpdateOrganizationMemberRole(&data.RPCUpdateOrganizationMemberRoleArgs{
				OrganizationID: bad, MemberID: member, Role: data.RoleAdmin,
			}, &reply)
		},
	} {
		err := call()
		if err == nil || !strings.Contains(err.Error(), "invalid organization ID") {
			t.Errorf("%s: want an \"invalid organization ID\" error, got %v", name, err)
		}
	}
}

// TestOrganizationMemberChanges_RefuseBadInput: a membership names its user by
// GitHub user ID and holds one of the three organization roles; anything else,
// or a member id that is not an ObjectID, is refused before the database.
func TestOrganizationMemberChanges_RefuseBadInput(t *testing.T) {
	srv := &RPCServer{} // zero-value models: reaching MongoDB would panic
	org := primitive.NewObjectID().Hex()

	var reply string
	for _, args := range []data.RPCAddOrganizationMemberArgs{
		{OrganizationID: org, UserID: 0, GithubLogin: "octocat", Role: data.RoleMember},
		{OrganizationID: org, UserID: 583231, GithubLogin: " ", Role: data.RoleMember},
	} {
		if err := srv.AddOrganizationMember(&args, &reply); !errors.Is(err, data.ErrInvalidUser) {
			t.Errorf("AddOrganizationMember(%+v): want ErrInvalidUser, got %v", args, err)
		}
	}
	if err := srv.AddOrganizationMember(&data.RPCAddOrganizationMemberArgs{
		OrganizationID: org, UserID: 583231, GithubLogin: "octocat", Role: "viewer",
	}, &reply); err == nil || !strings.Contains(err.Error(), "invalid role") {
		t.Errorf("AddOrganizationMember with role viewer: want an invalid role error, got %v", err)
	}
	if err := srv.UpdateOrganizationMemberRole(&data.RPCUpdateOrganizationMemberRoleArgs{
		OrganizationID: org, MemberID: primitive.NewObjectID().Hex(), Role: "viewer",
	}, &reply); err == nil || !strings.Contains(err.Error(), "invalid role") {
		t.Errorf("UpdateOrganizationMemberRole to viewer: want an invalid role error, got %v", err)
	}
	if err := srv.RemoveOrganizationMember(&data.RPCRemoveOrganizationMemberArgs{OrganizationID: org, MemberID: "octocat"}, &reply); err == nil ||
		!strings.Contains(err.Error(), "invalid member ID") {
		t.Errorf("RemoveOrganizationMember with a login for an id: want an invalid member ID error, got %v", err)
	}
}

// TestCreateOrganization_RefusesBadInput: an organization needs a name, a plan,
// and the owner it is created with.
func TestCreateOrganization_RefusesBadInput(t *testing.T) {
	srv := &RPCServer{} // zero-value models: reaching MongoDB would panic

	for _, args := range []data.RPCCreateOrganizationArgs{
		{Plan: "free", OwnerID: 583231, Owner: "octocat"},
		{Name: "Acme", OwnerID: 583231, Owner: "octocat"},
		{Name: "Acme", Plan: "free", Owner: "octocat"},
		{Name: "Acme", Plan: "free", OwnerID: 583231},
	} {
		var reply data.Organization
		if err := srv.CreateOrganization(&args, &reply); err == nil {
			t.Errorf("CreateOrganization(%+v) succeeded", args)
		}
		if !reflect.DeepEqual(reply, data.Organization{}) {
			t.Errorf("CreateOrganization(%+v): reply should stay empty on error, got %+v", args, reply)
		}
	}
}
