package main

import (
	"errors"
	"fmt"
	"net"
	"net/rpc"
	"sync"
	"sync/atomic"
	"testing"

	"go.mongodb.org/mongo-driver/bson/primitive"

	"logwolf-toolbox/data"
)

// fakeLogger stands in for the Logger service over a real net/rpc connection.
//
// The broker's project handlers dial the logger themselves — the address comes
// from LOGGER_RPC_ADDR, not from Config — so the only way to exercise the
// membership checks is to put a server on the other end of that dial. Serving
// the same method set over gob keeps the wire contract (including net/rpc's
// flattening of errors into strings, which the broker matches on) intact.
//
// All exported methods must be RPC-shaped; helpers for the tests are unexported
// so net/rpc ignores them.
type fakeLogger struct {
	mu sync.Mutex

	projects  map[string]data.Project         // project id hex -> project
	members   map[string][]data.ProjectMember // project id hex -> members
	logs      map[string][]data.LogEntry      // project id hex -> logs
	retention map[string]int                  // project id hex -> days
	metrics   map[string]data.Metrics         // project id hex -> metrics
	keys      map[string]data.APIKey          // key id hex -> key
	plaintext map[string]string               // plaintext key -> key id hex
	users     map[int64]data.User             // GitHub user ID -> user
	plans     map[string]string               // project id hex -> its organization's plan

	orgs       map[string]data.Organization         // organization id hex -> organization
	orgMembers map[string][]data.OrganizationMember // organization id hex -> members

	// Recorded calls, for asserting what the broker forwarded.
	getLogsParams   []data.QueryParams
	retentionArgs   []data.RetentionArgs
	metricsArgs     []data.ProjectArgs
	createdProjects []data.RPCCreateProjectArgs
	updatedProjects []data.RPCUpdateProjectArgs
	deletedProjects []string
	addedMembers    []data.RPCAddMemberArgs
	removedMembers  []data.RPCRemoveMemberArgs
	roleChanges     []data.RPCUpdateMemberRoleArgs
	revokedKeys     []data.RPCRevokeAPIKeyArgs
	upsertedUsers   []data.RPCUpsertUserArgs
	accessChecks    int // ProjectAccess calls

	createdOrgs       []data.RPCCreateOrganizationArgs
	updatedOrgs       []data.RPCUpdateOrganizationArgs
	addedOrgMembers   []data.RPCAddOrganizationMemberArgs
	removedOrgMembers []data.RPCRemoveOrganizationMemberArgs
	orgRoleChanges    []data.RPCUpdateOrganizationMemberRoleArgs
	orgAccessChecks   int // OrganizationAccess calls
	usageFlushes      []data.RPCRecordUsageArgs

	// Failure injection.
	failCreateProject bool               // CreateProject fails, as its transaction would, and creates nothing
	lastOwnerLogin    string             // RemoveMember and UpdateMemberRole refuse to remove or demote this login
	status            *data.LoggerStatus // what Status answers; nil is ready
	failRecordUsage   bool               // RecordUsage fails, as an unreachable database would

	// openConns counts the broker's connections the fake has not yet seen
	// closed. It goes back to zero only if every handler closed its client.
	openConns atomic.Int64
}

// errNoDocuments mirrors the driver error the logger passes back when a lookup
// misses. The broker only sees the message, so the wording is the contract.
var errNoDocuments = errors.New("mongo: no documents in result set")

// checkObjectID refuses a malformed id the way the logger's RPC methods do,
// before they touch the database.
func checkObjectID(op, id string) error {
	if _, err := primitive.ObjectIDFromHex(id); err != nil {
		return fmt.Errorf("%s: invalid project ID: %w", op, err)
	}
	return nil
}

// errDuplicateKey mirrors the driver's unique-index violation, which the broker
// recognizes by its E11000 code.
func errDuplicateKey(index string) error {
	return fmt.Errorf("E11000 duplicate key error collection: logs index: %s", index)
}

func newFakeLogger() *fakeLogger {
	return &fakeLogger{
		projects:  map[string]data.Project{},
		members:   map[string][]data.ProjectMember{},
		logs:      map[string][]data.LogEntry{},
		retention: map[string]int{},
		metrics:   map[string]data.Metrics{},
		keys:      map[string]data.APIKey{},
		plaintext: map[string]string{},
		users:     map[int64]data.User{},
		plans:     map[string]string{},

		orgs:       map[string]data.Organization{},
		orgMembers: map[string][]data.OrganizationMember{},
	}
}

// --- RPC surface ---

func (f *fakeLogger) GetProject(args *data.RPCProjectIDArgs, reply *data.Project) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := checkObjectID("GetProject", args.ID); err != nil {
		return err
	}
	p, ok := f.projects[args.ID]
	if !ok {
		return errNoDocuments
	}
	*reply = p
	return nil
}

func (f *fakeLogger) CreateProject(args *data.RPCCreateProjectArgs, reply *data.Project) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.createdProjects = append(f.createdProjects, *args)
	if f.failCreateProject {
		return fmt.Errorf("CreateProjectWithOwner owner: injected failure")
	}

	// No organization named is the Default one, which these tests leave out.
	var orgID primitive.ObjectID
	if args.OrganizationID != "" {
		if err := checkOrganizationID("CreateProject", args.OrganizationID); err != nil {
			return err
		}
		if _, ok := f.orgs[args.OrganizationID]; !ok {
			return fmt.Errorf("CreateProjectWithOwner: %w: %s", data.ErrUnknownOrganization, args.OrganizationID)
		}
		orgID = mustObjectID(args.OrganizationID)
	}

	// Like the logger, the project and its owner come into being together.
	id := nextProjectID()
	p := data.Project{ID: mustObjectID(id), Name: args.Name, Slug: args.Slug, OrganizationID: orgID}
	f.projects[id] = p
	f.members[id] = []data.ProjectMember{{
		ID: mustObjectID(testMemberID(id, args.Owner)), ProjectID: p.ID, UserID: args.OwnerID, GithubLogin: args.Owner, Role: data.RoleOwner,
	}}
	*reply = p
	return nil
}

func (f *fakeLogger) UpdateProject(args *data.RPCUpdateProjectArgs, reply *data.Project) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.updatedProjects = append(f.updatedProjects, *args)
	if err := checkObjectID("UpdateProject", args.ID); err != nil {
		return err
	}
	p, ok := f.projects[args.ID]
	if !ok {
		return errNoDocuments
	}
	p.Name = args.Name
	f.projects[args.ID] = p
	*reply = p
	return nil
}

func (f *fakeLogger) Status(_ string, reply *data.LoggerStatus) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.status == nil {
		*reply = data.LoggerStatus{Ready: true, RetentionCleanup: true}
		return nil
	}
	*reply = *f.status
	return nil
}

func (f *fakeLogger) DeleteProject(args *data.RPCProjectIDArgs, reply *string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := checkObjectID("DeleteProject", args.ID); err != nil {
		return err
	}
	f.deletedProjects = append(f.deletedProjects, args.ID)
	delete(f.projects, args.ID)
	delete(f.members, args.ID)
	delete(f.logs, args.ID)
	delete(f.retention, args.ID)
	// The real DeleteProject deletes the project's API keys in its transaction.
	for id, k := range f.keys {
		if k.ProjectID.Hex() == args.ID {
			delete(f.keys, id)
		}
	}
	*reply = "ok"
	return nil
}

func (f *fakeLogger) ListUserProjects(args *data.RPCUserProjectsArgs, reply *[]data.UserProject) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	var out []data.UserProject
	for id, members := range f.members {
		for _, m := range members {
			if !memberIs(m, args.UserID, args.GithubLogin) {
				continue
			}
			if p, ok := f.projects[id]; ok {
				out = append(out, data.UserProject{Project: p, Role: m.Role})
			}
		}
	}
	*reply = out
	return nil
}

func (f *fakeLogger) ListMembers(args *data.ProjectArgs, reply *[]data.ProjectMember) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := checkObjectID("ListMembers", args.ProjectID); err != nil {
		return err
	}
	*reply = append([]data.ProjectMember(nil), f.members[args.ProjectID]...)
	return nil
}

func (f *fakeLogger) ProjectAccess(args *data.RPCProjectAccessArgs, reply *data.ProjectAccess) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.accessChecks++
	if err := checkObjectID("ProjectAccess", args.ProjectID); err != nil {
		return err
	}
	p, exists := f.projects[args.ProjectID]
	*reply = data.ProjectAccess{Exists: exists}
	if !exists {
		return nil
	}
	for _, m := range f.members[args.ProjectID] {
		if memberIs(m, args.UserID, args.GithubLogin) {
			reply.Role = m.Role
		}
	}
	// Like data.AccessToProject: an owner of the project's organization owns it.
	reply.Role = data.EffectiveProjectRole(reply.Role, f.orgRole(p.OrganizationID.Hex(), args.UserID))
	return nil
}

// ProjectPlan answers the plan setPlan gave the project; a project without one
// is in no organization.
func (f *fakeLogger) ProjectPlan(args *data.RPCProjectIDArgs, reply *string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := checkObjectID("ProjectPlan", args.ID); err != nil {
		return err
	}
	if _, ok := f.projects[args.ID]; !ok {
		return errNoDocuments
	}
	plan, ok := f.plans[args.ID]
	if !ok {
		return fmt.Errorf("ProjectPlan: project %s: %w", args.ID, data.ErrUnknownOrganization)
	}
	*reply = plan
	return nil
}

// RecordUsage records the flush, or fails it when failRecordUsage is set.
func (f *fakeLogger) RecordUsage(args *data.RPCRecordUsageArgs, reply *string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.failRecordUsage {
		return errors.New("RecordUsage: server selection timeout")
	}
	f.usageFlushes = append(f.usageFlushes, *args)
	*reply = "OK"
	return nil
}

func (f *fakeLogger) AddMember(args *data.RPCAddMemberArgs, reply *string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.addedMembers = append(f.addedMembers, *args)
	if err := checkObjectID("AddMember", args.ProjectID); err != nil {
		return err
	}
	if args.UserID <= 0 {
		return fmt.Errorf("AddMember: %w", data.ErrInvalidUser)
	}
	// Like the logger's two unique indexes: one membership per user, and per login.
	for _, m := range f.members[args.ProjectID] {
		if m.UserID == args.UserID {
			return fmt.Errorf("InsertProjectMember: %w", errDuplicateKey("unique_project_member_user"))
		}
		if m.GithubLogin == args.GithubLogin {
			return fmt.Errorf("InsertProjectMember: %w", errDuplicateKey("unique_project_member"))
		}
	}
	f.members[args.ProjectID] = append(f.members[args.ProjectID], data.ProjectMember{
		ID:          mustObjectID(testMemberID(args.ProjectID, args.GithubLogin)),
		ProjectID:   mustObjectID(args.ProjectID),
		UserID:      args.UserID,
		GithubLogin: args.GithubLogin,
		Role:        args.Role,
	})
	*reply = "ok"
	return nil
}

// findMember returns the index of the membership memberID in the project, or
// the error the logger would answer. The caller holds f.mu.
func (f *fakeLogger) findMember(op, projectID, memberID string) (int, error) {
	if _, err := primitive.ObjectIDFromHex(memberID); err != nil {
		return 0, fmt.Errorf("%s: invalid member ID: %w", op, err)
	}
	for i, m := range f.members[projectID] {
		if m.ID.Hex() == memberID {
			return i, nil
		}
	}
	return 0, fmt.Errorf("%s: %w", op, errNoDocuments)
}

func (f *fakeLogger) RemoveMember(args *data.RPCRemoveMemberArgs, reply *string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.removedMembers = append(f.removedMembers, *args)
	i, err := f.findMember("RemoveProjectMember", args.ProjectID, args.MemberID)
	if err != nil {
		return err
	}
	members := f.members[args.ProjectID]
	if members[i].GithubLogin == f.lastOwnerLogin {
		// data.ErrLastOwner's message, as the broker sees it over the wire.
		return fmt.Errorf("cannot remove the last owner of a project")
	}
	f.members[args.ProjectID] = append(members[:i:i], members[i+1:]...)
	*reply = "ok"
	return nil
}

func (f *fakeLogger) UpdateMemberRole(args *data.RPCUpdateMemberRoleArgs, reply *string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.roleChanges = append(f.roleChanges, *args)
	i, err := f.findMember("UpdateProjectMemberRole", args.ProjectID, args.MemberID)
	if err != nil {
		return err
	}
	if f.members[args.ProjectID][i].GithubLogin == f.lastOwnerLogin && args.Role != data.RoleOwner {
		return fmt.Errorf("UpdateProjectMemberRole: cannot remove the last owner of a project")
	}
	f.members[args.ProjectID][i].Role = args.Role
	*reply = "ok"
	return nil
}

func (f *fakeLogger) GetLogs(p data.QueryParams, reply *[]data.LogEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.getLogsParams = append(f.getLogsParams, p)
	*reply = append([]data.LogEntry(nil), f.logs[p.ProjectID]...)
	return nil
}

func (f *fakeLogger) GetLog(filter data.RPCLogEntryFilter, reply *data.LogEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	for _, e := range f.logs[filter.ProjectID] {
		if e.ID == filter.ID {
			*reply = e
			return nil
		}
	}
	return errNoDocuments
}

func (f *fakeLogger) DeleteLog(filter data.RPCLogEntryFilter, reply *int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	var deleted int64
	kept := make([]data.LogEntry, 0, len(f.logs[filter.ProjectID]))
	for _, e := range f.logs[filter.ProjectID] {
		if e.ID == filter.ID {
			deleted++
			continue
		}
		kept = append(kept, e)
	}
	f.logs[filter.ProjectID] = kept
	*reply = deleted
	return nil
}

func (f *fakeLogger) GetRetention(args *data.RetentionArgs, reply *int) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.retentionArgs = append(f.retentionArgs, *args)
	days, ok := f.retention[args.ProjectID]
	if !ok {
		days = 90
	}
	*reply = days
	return nil
}

func (f *fakeLogger) UpdateRetention(args *data.RetentionArgs, reply *string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.retentionArgs = append(f.retentionArgs, *args)
	if !data.ValidRetentionDays[args.Days] {
		return fmt.Errorf("SetRetentionDays: %d is not a valid retention value", args.Days)
	}
	f.retention[args.ProjectID] = args.Days
	*reply = "ok"
	return nil
}

func (f *fakeLogger) GetMetrics(args *data.ProjectArgs, reply *data.Metrics) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.metricsArgs = append(f.metricsArgs, *args)
	*reply = f.metrics[args.ProjectID]
	return nil
}

func (f *fakeLogger) ValidateAPIKey(args *data.RPCValidateAPIKeyArgs, reply *data.RPCValidateAPIKeyReply) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	id, ok := f.plaintext[args.Plaintext]
	if !ok || !f.keys[id].Active {
		return nil
	}
	reply.Valid = true
	reply.Key = f.keys[id]
	return nil
}

func (f *fakeLogger) ListAPIKeys(args *data.ProjectArgs, reply *[]data.APIKey) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	var out []data.APIKey
	for _, k := range f.keys {
		if k.ProjectID.Hex() == args.ProjectID {
			out = append(out, k)
		}
	}
	*reply = out
	return nil
}

func (f *fakeLogger) CreateAPIKey(args *data.RPCCreateAPIKeyArgs, reply *data.RPCCreateAPIKeyReply) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	scopes, err := data.NormalizeScopes(args.Scopes)
	if err != nil {
		return fmt.Errorf("CreateAPIKey: %w", err)
	}
	reply.Plaintext, reply.Key = f.storeKey(args.ProjectID, scopes)
	return nil
}

func (f *fakeLogger) RevokeAPIKey(args *data.RPCRevokeAPIKeyArgs, reply *string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.revokedKeys = append(f.revokedKeys, *args)
	k, ok := f.keys[args.ID]
	if !ok || k.ProjectID.Hex() != args.ProjectID {
		return data.ErrKeyNotFound
	}
	k.Active = false
	f.keys[args.ID] = k
	*reply = "ok"
	return nil
}

// UpsertUser keys users by GitHub ID like the logger: a second sign-in keeps
// the user and refreshes their login and email.
func (f *fakeLogger) UpsertUser(args *data.RPCUpsertUserArgs, reply *data.User) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.upsertedUsers = append(f.upsertedUsers, *args)
	login := data.NormalizeGithubLogin(args.GithubLogin)
	if args.GithubID <= 0 || login == "" {
		return fmt.Errorf("UpsertUser: %w", data.ErrInvalidUser)
	}
	u, ok := f.users[args.GithubID]
	if !ok {
		u = data.User{ID: primitive.NewObjectID(), GithubID: args.GithubID}
	}
	u.GithubLogin, u.Email = login, args.Email
	f.users[args.GithubID] = u
	*reply = u
	return nil
}

// --- Organizations ---

// checkOrganizationID refuses a malformed id the way the logger's organization
// RPC methods do.
func checkOrganizationID(op, id string) error {
	if _, err := primitive.ObjectIDFromHex(id); err != nil {
		return fmt.Errorf("%s: invalid organization ID: %w", op, err)
	}
	return nil
}

func (f *fakeLogger) OrganizationAccess(args *data.RPCOrganizationAccessArgs, reply *data.OrganizationAccess) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.orgAccessChecks++
	if err := checkOrganizationID("OrganizationAccess", args.OrganizationID); err != nil {
		return err
	}
	_, exists := f.orgs[args.OrganizationID]
	*reply = data.OrganizationAccess{Exists: exists, Role: f.orgRole(args.OrganizationID, args.UserID)}
	return nil
}

func (f *fakeLogger) GetOrganization(args *data.RPCOrganizationIDArgs, reply *data.Organization) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := checkOrganizationID("GetOrganization", args.ID); err != nil {
		return err
	}
	o, ok := f.orgs[args.ID]
	if !ok {
		return fmt.Errorf("GetOrganization: %w", errNoDocuments)
	}
	*reply = o
	return nil
}

func (f *fakeLogger) CreateOrganization(args *data.RPCCreateOrganizationArgs, reply *data.Organization) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.createdOrgs = append(f.createdOrgs, *args)
	if args.Name == "" || args.Plan == "" {
		return fmt.Errorf("CreateOrganizationWithOwner: %w", data.ErrInvalidOrganization)
	}
	id := nextProjectID()
	o := data.Organization{ID: mustObjectID(id), Name: args.Name, Plan: args.Plan}
	f.orgs[id] = o
	f.orgMembers[id] = []data.OrganizationMember{{
		ID: mustObjectID(testMemberID(id, args.Owner)), OrganizationID: o.ID, UserID: args.OwnerID, GithubLogin: args.Owner, Role: data.RoleOwner,
	}}
	*reply = o
	return nil
}

func (f *fakeLogger) UpdateOrganization(args *data.RPCUpdateOrganizationArgs, reply *data.Organization) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.updatedOrgs = append(f.updatedOrgs, *args)
	if err := checkOrganizationID("UpdateOrganization", args.ID); err != nil {
		return err
	}
	o, ok := f.orgs[args.ID]
	if !ok {
		return fmt.Errorf("RenameOrganization: %w", errNoDocuments)
	}
	o.Name = args.Name
	f.orgs[args.ID] = o
	*reply = o
	return nil
}

func (f *fakeLogger) ListUserOrganizations(args *data.RPCUserOrganizationsArgs, reply *[]data.UserOrganization) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := []data.UserOrganization{}
	for id, o := range f.orgs {
		if role := f.orgRole(id, args.UserID); role != "" {
			out = append(out, data.UserOrganization{Organization: o, Role: role})
		}
	}
	*reply = out
	return nil
}

func (f *fakeLogger) ListOrganizationMembers(args *data.RPCOrganizationIDArgs, reply *[]data.OrganizationMember) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := checkOrganizationID("ListOrganizationMembers", args.ID); err != nil {
		return err
	}
	*reply = append([]data.OrganizationMember{}, f.orgMembers[args.ID]...)
	return nil
}

func (f *fakeLogger) OrganizationUsage(args *data.RPCOrganizationIDArgs, reply *data.OrganizationUsage) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := checkOrganizationID("OrganizationUsage", args.ID); err != nil {
		return err
	}
	usage := data.OrganizationUsage{Members: int64(len(f.orgMembers[args.ID]))}
	for _, p := range f.projects {
		if p.OrganizationID.Hex() == args.ID {
			usage.Projects++
		}
	}
	*reply = usage
	return nil
}

func (f *fakeLogger) AddOrganizationMember(args *data.RPCAddOrganizationMemberArgs, reply *string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.addedOrgMembers = append(f.addedOrgMembers, *args)
	if err := checkOrganizationID("AddOrganizationMember", args.OrganizationID); err != nil {
		return err
	}
	for _, m := range f.orgMembers[args.OrganizationID] {
		if m.UserID == args.UserID {
			return fmt.Errorf("InsertOrganizationMember: %w", errDuplicateKey("unique_organization_member"))
		}
	}
	f.orgMembers[args.OrganizationID] = append(f.orgMembers[args.OrganizationID], data.OrganizationMember{
		ID:             mustObjectID(testMemberID(args.OrganizationID, args.GithubLogin)),
		OrganizationID: mustObjectID(args.OrganizationID),
		UserID:         args.UserID,
		GithubLogin:    args.GithubLogin,
		Role:           args.Role,
	})
	*reply = "ok"
	return nil
}

// findOrgMember returns the index of the membership memberID in the
// organization, or the error the logger would answer. A change that touches an
// owner is refused the way the logger refuses it: ErrOwnerRequired unless the
// actor is an owner, ErrLastOrganizationOwner if it would take away the only
// one. The caller holds f.mu.
func (f *fakeLogger) findOrgMember(op, orgID, memberID string, touchesOwner func(data.OrganizationMember) bool, actorRole string) (int, error) {
	if _, err := primitive.ObjectIDFromHex(memberID); err != nil {
		return 0, fmt.Errorf("%s: invalid member ID: %w", op, err)
	}
	for i, m := range f.orgMembers[orgID] {
		if m.ID.Hex() != memberID {
			continue
		}
		if !touchesOwner(m) {
			return i, nil
		}
		if actorRole != data.RoleOwner {
			return 0, fmt.Errorf("%s: %w", op, data.ErrOwnerRequired)
		}
		if m.Role == data.RoleOwner {
			owners := 0
			for _, other := range f.orgMembers[orgID] {
				if other.Role == data.RoleOwner {
					owners++
				}
			}
			if owners <= 1 {
				return 0, fmt.Errorf("%s: %w", op, data.ErrLastOrganizationOwner)
			}
		}
		return i, nil
	}
	return 0, fmt.Errorf("%s: %w", op, errNoDocuments)
}

func (f *fakeLogger) RemoveOrganizationMember(args *data.RPCRemoveOrganizationMemberArgs, reply *string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.removedOrgMembers = append(f.removedOrgMembers, *args)
	if err := checkOrganizationID("RemoveOrganizationMember", args.OrganizationID); err != nil {
		return err
	}
	isOwner := func(m data.OrganizationMember) bool { return m.Role == data.RoleOwner }
	i, err := f.findOrgMember("RemoveOrganizationMember", args.OrganizationID, args.MemberID, isOwner, args.ActorRole)
	if err != nil {
		return err
	}
	members := f.orgMembers[args.OrganizationID]
	f.orgMembers[args.OrganizationID] = append(members[:i:i], members[i+1:]...)
	*reply = "ok"
	return nil
}

func (f *fakeLogger) UpdateOrganizationMemberRole(args *data.RPCUpdateOrganizationMemberRoleArgs, reply *string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.orgRoleChanges = append(f.orgRoleChanges, *args)
	if err := checkOrganizationID("UpdateOrganizationMemberRole", args.OrganizationID); err != nil {
		return err
	}
	if !data.ValidOrganizationRole(args.Role) {
		return fmt.Errorf("UpdateOrganizationMemberRole: invalid role %q", args.Role)
	}
	touchesOwner := func(m data.OrganizationMember) bool {
		return m.Role != args.Role && (m.Role == data.RoleOwner || args.Role == data.RoleOwner)
	}
	i, err := f.findOrgMember("UpdateOrganizationMemberRole", args.OrganizationID, args.MemberID, touchesOwner, args.ActorRole)
	if err != nil {
		return err
	}
	f.orgMembers[args.OrganizationID][i].Role = args.Role
	*reply = "ok"
	return nil
}

// --- test-side helpers (unexported, so net/rpc ignores them) ---

// orgRole is the user's role in the organization, "" for none. The caller holds
// f.mu.
func (f *fakeLogger) orgRole(orgID string, userID int64) string {
	for _, m := range f.orgMembers[orgID] {
		if userID > 0 && m.UserID == userID {
			return m.Role
		}
	}
	return ""
}

func (f *fakeLogger) addOrganization(id, name, plan string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.orgs[id] = data.Organization{ID: mustObjectID(id), Name: name, Plan: plan}
}

// addOrgMember makes the user testUserID(login) a member of the organization,
// under id testMemberID(orgID, login).
func (f *fakeLogger) addOrgMember(orgID, login, role string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.orgMembers[orgID] = append(f.orgMembers[orgID], data.OrganizationMember{
		ID:             mustObjectID(testMemberID(orgID, login)),
		OrganizationID: mustObjectID(orgID),
		UserID:         testUserID(login),
		GithubLogin:    login,
		Role:           role,
	})
}

// putInOrganization moves the project into the organization.
func (f *fakeLogger) putInOrganization(projectID, orgID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.projects[projectID]
	p.OrganizationID = mustObjectID(orgID)
	f.projects[projectID] = p
}

var keySeq atomic.Int64

// storeKey mints a key the way the logger would, minus the hashing, and keeps
// its plaintext so ValidateAPIKey can find it. The caller holds f.mu.
func (f *fakeLogger) storeKey(projectID string, scopes []string) (string, data.APIKey) {
	n := keySeq.Add(1)
	id := fmt.Sprintf("eeeeeeeeeeeeeeeeeeee%04d", n)
	plaintext := fmt.Sprintf("lw_fakelogger%033d", n)
	k := data.APIKey{
		ID:        mustObjectID(id),
		ProjectID: mustObjectID(projectID),
		Prefix:    plaintext[:10],
		Scopes:    scopes,
		Active:    true,
	}
	f.keys[id] = k
	f.plaintext[plaintext] = id
	return plaintext, k
}

func (f *fakeLogger) addKey(projectID string, scopes ...string) (plaintext string, key data.APIKey) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.storeKey(projectID, scopes)
}

func (f *fakeLogger) addProject(id, name, slug string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.projects[id] = data.Project{ID: mustObjectID(id), Name: name, Slug: slug}
}

// setPlan puts the project in an organization on plan.
func (f *fakeLogger) setPlan(projectID, plan string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.plans[projectID] = plan
}

// addMember makes the user testUserID(login) a member, under id
// testMemberID(projectID, login).
func (f *fakeLogger) addMember(projectID, login, role string) {
	f.addMembership(projectID, testUserID(login), login, role)
}

// addUnlinkedMember adds a membership stored before user IDs: a login alone.
func (f *fakeLogger) addUnlinkedMember(projectID, login, role string) {
	f.addMembership(projectID, 0, login, role)
}

func (f *fakeLogger) addMembership(projectID string, userID int64, login, role string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.members[projectID] = append(f.members[projectID], data.ProjectMember{
		ID:          mustObjectID(testMemberID(projectID, login)),
		ProjectID:   mustObjectID(projectID),
		UserID:      userID,
		GithubLogin: login,
		Role:        role,
	})
}

// memberIs applies data.MemberFilter's rule to one membership: it belongs to the
// user with this ID, or, when it is linked to nobody, to whoever has its login.
func memberIs(m data.ProjectMember, userID int64, login string) bool {
	if m.UserID != 0 {
		return userID > 0 && m.UserID == userID
	}
	return m.GithubLogin == login
}

func (f *fakeLogger) addLog(projectID, logID, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logs[projectID] = append(f.logs[projectID], data.LogEntry{
		ID:        logID,
		ProjectID: mustObjectID(projectID),
		Name:      name,
		Data:      "{}",
		Severity:  "info",
		Tags:      []string{},
	})
}

func (f *fakeLogger) snapshot(read func(*fakeLogger)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	read(f)
}

// serveFakeLogger publishes f on a loopback listener under the name the broker
// calls ("RPCServer") and points LOGGER_RPC_ADDR at it for the duration of t.
func serveFakeLogger(t *testing.T, f *fakeLogger) {
	t.Helper()

	srv := rpc.NewServer()
	if err := srv.RegisterName("RPCServer", f); err != nil {
		t.Fatalf("register fake logger: %v", err)
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { l.Close() })

	// Accept returns as soon as the listener closes, which Cleanup handles.
	go srv.Accept(countingListener{Listener: l, open: &f.openConns})

	t.Setenv("LOGGER_RPC_ADDR", l.Addr().String())
}

// countingListener tracks how many accepted connections are still open. The
// RPC server closes its end once the broker closes the client, so a connection
// the broker leaks stays counted.
type countingListener struct {
	net.Listener
	open *atomic.Int64
}

func (l countingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	l.open.Add(1)
	return &countedConn{Conn: c, open: l.open}, nil
}

type countedConn struct {
	net.Conn
	open   *atomic.Int64
	closed sync.Once
}

func (c *countedConn) Close() error {
	c.closed.Do(func() { c.open.Add(-1) })
	return c.Conn.Close()
}
