package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"logwolf-toolbox/data"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

type RPCServer struct {
	models   data.Models
	projects *projectCache

	// purges hands deleted projects to the cleanup loop; nil hands them to no
	// one, and the orphan sweep deletes their logs instead.
	purges chan<- primitive.ObjectID

	// startup is what Status reports; nil reports ready.
	startup *startupState
}

// Status reports whether the logger's startup tasks have all succeeded. The
// broker's /health asks for it. The argument is unused: gob cannot encode an
// empty struct.
func (r *RPCServer) Status(_ string, reply *data.LoggerStatus) error {
	if r.startup == nil {
		*reply = data.LoggerStatus{Ready: true, RetentionCleanup: true}
		return nil
	}
	*reply = r.startup.status()
	return nil
}

// parseProjectID turns the hex project id an RPC argument carries into the
// ObjectID every project_id is stored as. A malformed one names no project; the
// error says "not a valid ObjectID", which the broker answers as not found.
func parseProjectID(op, hex string) (primitive.ObjectID, error) {
	id, err := primitive.ObjectIDFromHex(hex)
	if err != nil {
		return primitive.NilObjectID, fmt.Errorf("%s: invalid project ID: %w", op, err)
	}
	return id, nil
}

// projectExists is the projectCache lookup. A string that is not an ObjectID
// names no project, so it answers false without a query.
func (r *RPCServer) projectExists(projectID string) (bool, error) {
	id, err := primitive.ObjectIDFromHex(projectID)
	if err != nil {
		return false, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return r.models.ProjectExists(ctx, id)
}

// LogInfo inserts an event. One filed under a project that does not exist is
// dropped with an error instead: nothing could ever read it, and the retention
// cleanup, which goes project by project, would never delete it.
func (r *RPCServer) LogInfo(p data.RPCLogPayload, resp *string) error {
	log.Printf("Logging info: %s", p.Name)

	exists, err := r.projects.exists(p.ProjectID, r.projectExists)
	if err != nil {
		log.Println("Error checking the event's project:", err)
		return err
	}
	if !exists {
		log.Printf("Dropping event %q: project %q does not exist", p.Name, p.ProjectID)
		return fmt.Errorf("LogInfo: %w: %q", data.ErrUnknownProject, p.ProjectID)
	}
	// projectExists answers true only for a valid ObjectID.
	projectID, _ := primitive.ObjectIDFromHex(p.ProjectID)

	err = r.models.Insert(data.LogEntry{
		ProjectID: projectID,
		Name:      p.Name,
		Data:      p.Data,
		Severity:  p.Severity,
		Tags:      p.Tags,
		Duration:  p.Duration,
	})
	if err != nil {
		log.Println("Error inserting into logs:", err)
		return err
	}

	*resp = fmt.Sprintf("Processed payload via RPC: %s", p.Name)
	return nil
}

func (r *RPCServer) GetLogs(p data.QueryParams, resp *[]data.LogEntry) error {
	log.Printf("Getting logs with params %+v...\n", p)

	projectID, err := parseProjectID("GetLogs", p.ProjectID)
	if err != nil {
		return err
	}

	result, err := r.models.AllLogs(projectID, p.Pagination)
	if err != nil {
		log.Println("Error getting logs:", err)
		return err
	}

	for _, doc := range result {
		*resp = append(*resp, *doc)
	}

	log.Printf("Logs found via RPC: %d\n", len(*resp))
	return nil
}

// GetLog fetches a single entry by id. The project is part of the query rather
// than a check layered on top of it, so an id belonging to another project
// comes back as "no documents in result" — the same as one that never existed.
func (r *RPCServer) GetLog(f data.RPCLogEntryFilter, resp *data.LogEntry) error {
	log.Printf("Getting log %s of project %s...\n", f.ID, f.ProjectID)

	projectID, err := parseProjectID("GetLog", f.ProjectID)
	if err != nil {
		return err
	}

	entry, err := r.models.GetLog(f.ID, projectID)
	if err != nil {
		log.Println("Error getting log:", err)
		return err
	}

	*resp = *entry
	return nil
}

func (r *RPCServer) DeleteLog(f data.RPCLogEntryFilter, resp *int64) error {
	log.Printf("Deleting log %+v...\n", f)

	projectID, err := parseProjectID("DeleteLog", f.ProjectID)
	if err != nil {
		return err
	}

	result, err := r.models.DeleteLog(f.ID, projectID)
	if err != nil {
		log.Println("Error deleting document:", err)
		return err
	}

	*resp = result.DeletedCount
	log.Printf("Deleted: %d!", result.DeletedCount)

	return nil
}

func (r *RPCServer) GetRetention(args *data.RetentionArgs, reply *int) error {
	projectID, err := parseProjectID("GetRetention", args.ProjectID)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	days, err := r.models.Settings.GetRetentionDays(ctx, projectID)
	if err != nil {
		return err
	}
	*reply = days
	return nil
}

func (r *RPCServer) UpdateRetention(args *data.RetentionArgs, reply *string) error {
	projectID, err := parseProjectID("UpdateRetention", args.ProjectID)
	if err != nil {
		return err
	}
	if err := r.models.Settings.SetRetentionDays(projectID, args.Days); err != nil {
		return err
	}
	*reply = "ok"
	return nil
}

func (r *RPCServer) GetMetrics(args *data.ProjectArgs, reply *data.Metrics) error {
	projectID, err := parseProjectID("GetMetrics", args.ProjectID)
	if err != nil {
		return err
	}
	metrics, err := r.models.GetMetrics(projectID)
	if err != nil {
		return err
	}
	*reply = *metrics
	return nil
}

// CreateProject creates a project owned by the user args.OwnerID, whose login is
// args.Owner. The project and the owner membership are written in one
// transaction, so a failure leaves neither.
func (r *RPCServer) CreateProject(args *data.RPCCreateProjectArgs, reply *data.Project) error {
	if args.Name == "" {
		return fmt.Errorf("CreateProject: name is required")
	}
	if !data.ValidSlug(args.Slug) {
		return fmt.Errorf("CreateProject: invalid slug %q", args.Slug)
	}
	if args.OwnerID <= 0 || data.NormalizeGithubLogin(args.Owner) == "" {
		return fmt.Errorf("CreateProject: owner is required")
	}
	log.Printf("Creating project: %s (%s) owned by %s (%d)", args.Name, args.Slug, args.Owner, args.OwnerID)
	project, err := r.models.CreateProjectWithOwner(data.Project{Name: args.Name, Slug: args.Slug}, args.OwnerID, args.Owner)
	if err != nil {
		log.Println("Error creating project:", err)
		return err
	}
	*reply = *project
	return nil
}

func (r *RPCServer) GetProject(args *data.RPCProjectIDArgs, reply *data.Project) error {
	log.Printf("Getting project: %s", args.ID)
	id, err := primitive.ObjectIDFromHex(args.ID)
	if err != nil {
		return fmt.Errorf("GetProject: invalid ID: %w", err)
	}
	project, err := r.models.GetProject(id)
	if err != nil {
		log.Println("Error getting project:", err)
		return err
	}
	*reply = *project
	return nil
}

// UpdateProject renames a project. The slug is fixed at creation.
func (r *RPCServer) UpdateProject(args *data.RPCUpdateProjectArgs, reply *data.Project) error {
	if args.Name == "" {
		return fmt.Errorf("UpdateProject: name is required")
	}
	log.Printf("Updating project: %s", args.ID)
	id, err := primitive.ObjectIDFromHex(args.ID)
	if err != nil {
		return fmt.Errorf("UpdateProject: invalid ID: %w", err)
	}
	project, err := r.models.RenameProject(id, args.Name)
	if err != nil {
		log.Println("Error updating project:", err)
		return err
	}
	*reply = *project
	return nil
}

func (r *RPCServer) DeleteProject(args *data.RPCProjectIDArgs, reply *string) error {
	log.Printf("Deleting project: %s", args.ID)
	id, err := primitive.ObjectIDFromHex(args.ID)
	if err != nil {
		return fmt.Errorf("DeleteProject: invalid ID: %w", err)
	}
	if err := r.models.DeleteProject(id); err != nil {
		log.Println("Error deleting project:", err)
		return err
	}
	r.projects.forget(args.ID)
	r.requestPurge(id)
	*reply = "ok"
	return nil
}

// requestPurge asks the cleanup loop to delete a deleted project's logs, which
// DeleteProject leaves behind. It never blocks the RPC: if the queue is full the
// request is dropped, and the next orphan sweep deletes those logs anyway.
func (r *RPCServer) requestPurge(projectID primitive.ObjectID) {
	if r.purges == nil {
		return
	}
	select {
	case r.purges <- projectID:
	default:
		log.Printf("Purge queue full: logs of deleted project %s are left for the next cleanup pass", projectID.Hex())
	}
}

// ListUserProjects lists the projects of the user args.UserID, with the
// memberships not yet linked to a user found by their current login.
func (r *RPCServer) ListUserProjects(args *data.RPCUserProjectsArgs, reply *[]data.UserProject) error {
	log.Printf("Listing projects for user: %d (%s)", args.UserID, args.GithubLogin)
	projects, err := r.models.GetProjectsForUser(args.UserID, args.GithubLogin)
	if err != nil {
		log.Println("Error listing user projects:", err)
		return err
	}
	*reply = projects
	return nil
}

// AddMember adds the user args.UserID to a project. A membership always names
// its user: only those stored before user IDs existed have none.
func (r *RPCServer) AddMember(args *data.RPCAddMemberArgs, reply *string) error {
	if !data.ValidRole(args.Role) {
		return fmt.Errorf("AddMember: invalid role %q", args.Role)
	}
	if args.UserID <= 0 {
		return fmt.Errorf("AddMember: %w: GitHub user ID must be positive, got %d", data.ErrInvalidUser, args.UserID)
	}
	if data.NormalizeGithubLogin(args.GithubLogin) == "" {
		return fmt.Errorf("AddMember: %w: login is required", data.ErrInvalidUser)
	}
	log.Printf("Adding member %s (%d) to project %s", args.GithubLogin, args.UserID, args.ProjectID)
	projectID, err := primitive.ObjectIDFromHex(args.ProjectID)
	if err != nil {
		return fmt.Errorf("AddMember: invalid project ID: %w", err)
	}
	_, err = r.models.InsertProjectMember(data.ProjectMember{
		ProjectID:   projectID,
		UserID:      args.UserID,
		GithubLogin: args.GithubLogin,
		Role:        args.Role,
	})
	if err != nil {
		log.Println("Error adding member:", err)
		return err
	}
	*reply = "ok"
	return nil
}

// parseMemberID parses a membership id. One that is not an ObjectID names no
// membership, and the error says so the way the broker reads as not found.
func parseMemberID(op, hex string) (primitive.ObjectID, error) {
	id, err := primitive.ObjectIDFromHex(hex)
	if err != nil {
		return primitive.NilObjectID, fmt.Errorf("%s: invalid member ID: %w", op, err)
	}
	return id, nil
}

func (r *RPCServer) RemoveMember(args *data.RPCRemoveMemberArgs, reply *string) error {
	log.Printf("Removing member %s from project %s", args.MemberID, args.ProjectID)
	projectID, err := primitive.ObjectIDFromHex(args.ProjectID)
	if err != nil {
		return fmt.Errorf("RemoveMember: invalid project ID: %w", err)
	}
	memberID, err := parseMemberID("RemoveMember", args.MemberID)
	if err != nil {
		return err
	}
	if err := r.models.RemoveProjectMember(projectID, memberID); err != nil {
		log.Println("Error removing member:", err)
		return err
	}
	*reply = "ok"
	return nil
}

func (r *RPCServer) UpdateMemberRole(args *data.RPCUpdateMemberRoleArgs, reply *string) error {
	if !data.ValidRole(args.Role) {
		return fmt.Errorf("UpdateMemberRole: invalid role %q", args.Role)
	}
	log.Printf("Setting role of member %s in project %s to %s", args.MemberID, args.ProjectID, args.Role)
	projectID, err := primitive.ObjectIDFromHex(args.ProjectID)
	if err != nil {
		return fmt.Errorf("UpdateMemberRole: invalid project ID: %w", err)
	}
	memberID, err := parseMemberID("UpdateMemberRole", args.MemberID)
	if err != nil {
		return err
	}
	if err := r.models.UpdateProjectMemberRole(projectID, memberID, args.Role); err != nil {
		log.Println("Error updating member role:", err)
		return err
	}
	*reply = "ok"
	return nil
}

// ProjectAccess reports whether the project exists and the caller's role in it,
// the caller being a GitHub user ID with, for memberships not yet linked to a
// user, their current login. A member's project exists, so only a non-member
// costs a second query.
func (r *RPCServer) ProjectAccess(args *data.RPCProjectAccessArgs, reply *data.ProjectAccess) error {
	projectID, err := parseProjectID("ProjectAccess", args.ProjectID)
	if err != nil {
		return err
	}
	role, err := r.models.MemberRole(projectID, args.UserID, args.GithubLogin)
	if err != nil {
		return err
	}
	if role != "" {
		*reply = data.ProjectAccess{Exists: true, Role: role}
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	exists, err := r.models.ProjectExists(ctx, projectID)
	if err != nil {
		return err
	}
	*reply = data.ProjectAccess{Exists: exists}
	return nil
}

func (r *RPCServer) ListMembers(args *data.ProjectArgs, reply *[]data.ProjectMember) error {
	log.Printf("Listing members for project: %s", args.ProjectID)
	projectID, err := primitive.ObjectIDFromHex(args.ProjectID)
	if err != nil {
		return fmt.Errorf("ListMembers: invalid project ID: %w", err)
	}
	members, err := r.models.GetProjectMembers(projectID)
	if err != nil {
		log.Println("Error listing members:", err)
		return err
	}
	*reply = members
	return nil
}

// --- Organizations ---
//
// An organization sits above projects and holds the plan. Its id travels as a
// hex string like a project's, and is parsed once here.

// parseOrganizationID turns the hex organization id an RPC argument carries into
// an ObjectID. A malformed one names no organization; the error says "invalid
// organization ID", for the broker to answer as not found.
func parseOrganizationID(op, hex string) (primitive.ObjectID, error) {
	id, err := primitive.ObjectIDFromHex(hex)
	if err != nil {
		return primitive.NilObjectID, fmt.Errorf("%s: invalid organization ID: %w", op, err)
	}
	return id, nil
}

// CreateOrganization creates an organization on args.Plan owned by the user
// args.OwnerID, whose login is args.Owner. The organization and the owner
// membership are written in one transaction, so a failure leaves neither.
func (r *RPCServer) CreateOrganization(args *data.RPCCreateOrganizationArgs, reply *data.Organization) error {
	log.Printf("Creating organization: %s (%s) owned by %s (%d)", args.Name, args.Plan, args.Owner, args.OwnerID)
	org, err := r.models.CreateOrganizationWithOwner(data.Organization{Name: args.Name, Plan: args.Plan}, args.OwnerID, args.Owner)
	if err != nil {
		log.Println("Error creating organization:", err)
		return err
	}
	*reply = *org
	return nil
}

func (r *RPCServer) GetOrganization(args *data.RPCOrganizationIDArgs, reply *data.Organization) error {
	id, err := parseOrganizationID("GetOrganization", args.ID)
	if err != nil {
		return err
	}
	org, err := r.models.GetOrganization(id)
	if err != nil {
		log.Println("Error getting organization:", err)
		return err
	}
	*reply = *org
	return nil
}

// UpdateOrganization renames an organization. Its plan is not changed here.
func (r *RPCServer) UpdateOrganization(args *data.RPCUpdateOrganizationArgs, reply *data.Organization) error {
	log.Printf("Renaming organization: %s", args.ID)
	id, err := parseOrganizationID("UpdateOrganization", args.ID)
	if err != nil {
		return err
	}
	org, err := r.models.RenameOrganization(id, args.Name)
	if err != nil {
		log.Println("Error renaming organization:", err)
		return err
	}
	*reply = *org
	return nil
}

// ListUserOrganizations lists the organizations of the user args.UserID, each
// with the role they hold in it.
func (r *RPCServer) ListUserOrganizations(args *data.RPCUserOrganizationsArgs, reply *[]data.UserOrganization) error {
	orgs, err := r.models.GetOrganizationsForUser(args.UserID)
	if err != nil {
		log.Println("Error listing user organizations:", err)
		return err
	}
	*reply = orgs
	return nil
}

// OrganizationAccess reports whether the organization exists and the caller's
// role in it. A member's organization exists, so only a non-member costs a
// second query.
func (r *RPCServer) OrganizationAccess(args *data.RPCOrganizationAccessArgs, reply *data.OrganizationAccess) error {
	orgID, err := parseOrganizationID("OrganizationAccess", args.OrganizationID)
	if err != nil {
		return err
	}
	role, err := r.models.OrganizationRole(orgID, args.UserID)
	if err != nil {
		return err
	}
	if role != "" {
		*reply = data.OrganizationAccess{Exists: true, Role: role}
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	exists, err := r.models.OrganizationExists(ctx, orgID)
	if err != nil {
		return err
	}
	*reply = data.OrganizationAccess{Exists: exists}
	return nil
}

func (r *RPCServer) ListOrganizationMembers(args *data.RPCOrganizationIDArgs, reply *[]data.OrganizationMember) error {
	orgID, err := parseOrganizationID("ListOrganizationMembers", args.ID)
	if err != nil {
		return err
	}
	members, err := r.models.GetOrganizationMembers(orgID)
	if err != nil {
		log.Println("Error listing organization members:", err)
		return err
	}
	*reply = members
	return nil
}

// AddOrganizationMember adds the user args.UserID to an organization. A user
// who is a member already is refused with a duplicate key error.
func (r *RPCServer) AddOrganizationMember(args *data.RPCAddOrganizationMemberArgs, reply *string) error {
	orgID, err := parseOrganizationID("AddOrganizationMember", args.OrganizationID)
	if err != nil {
		return err
	}
	log.Printf("Adding member %s (%d) to organization %s as %s", args.GithubLogin, args.UserID, args.OrganizationID, args.Role)
	_, err = r.models.InsertOrganizationMember(data.OrganizationMember{
		OrganizationID: orgID,
		UserID:         args.UserID,
		GithubLogin:    args.GithubLogin,
		Role:           args.Role,
	})
	if err != nil {
		log.Println("Error adding organization member:", err)
		return err
	}
	*reply = "ok"
	return nil
}

func (r *RPCServer) RemoveOrganizationMember(args *data.RPCRemoveOrganizationMemberArgs, reply *string) error {
	log.Printf("Removing member %s from organization %s", args.MemberID, args.OrganizationID)
	orgID, err := parseOrganizationID("RemoveOrganizationMember", args.OrganizationID)
	if err != nil {
		return err
	}
	memberID, err := parseMemberID("RemoveOrganizationMember", args.MemberID)
	if err != nil {
		return err
	}
	if err := r.models.RemoveOrganizationMember(orgID, memberID); err != nil {
		log.Println("Error removing organization member:", err)
		return err
	}
	*reply = "ok"
	return nil
}

func (r *RPCServer) UpdateOrganizationMemberRole(args *data.RPCUpdateOrganizationMemberRoleArgs, reply *string) error {
	if !data.ValidOrganizationRole(args.Role) {
		return fmt.Errorf("UpdateOrganizationMemberRole: invalid role %q", args.Role)
	}
	log.Printf("Setting role of member %s in organization %s to %s", args.MemberID, args.OrganizationID, args.Role)
	orgID, err := parseOrganizationID("UpdateOrganizationMemberRole", args.OrganizationID)
	if err != nil {
		return err
	}
	memberID, err := parseMemberID("UpdateOrganizationMemberRole", args.MemberID)
	if err != nil {
		return err
	}
	if err := r.models.UpdateOrganizationMemberRole(orgID, memberID, args.Role); err != nil {
		log.Println("Error updating organization member role:", err)
		return err
	}
	*reply = "ok"
	return nil
}

// --- Users ---
//
// A user is keyed by their GitHub user ID, which survives a rename; the login
// is only what they were called when they last signed in.

// UpsertUser records a sign-in: it creates the user with this GitHub ID, or
// refreshes the login and email of the existing one, links the memberships
// stored before user IDs under their login to them (data.LinkMemberships), and
// replies with the user as stored. A failure in either step fails the sign-in;
// both are idempotent, so the next one finishes the job.
func (r *RPCServer) UpsertUser(args *data.RPCUpsertUserArgs, reply *data.User) error {
	log.Printf("Upserting user %d (%s)", args.GithubID, args.GithubLogin)
	user, err := r.models.UpsertUser(args.GithubID, args.GithubLogin, args.Email)
	if err != nil {
		log.Println("Error upserting user:", err)
		return err
	}

	links, err := r.models.LinkMemberships(user.GithubID, user.GithubLogin)
	if err != nil {
		log.Println("Error linking memberships:", err)
		return err
	}
	if links.Linked > 0 || links.Merged > 0 {
		log.Printf("Linked memberships of %s to user %d: linked=%d merged=%d",
			user.GithubLogin, user.GithubID, links.Linked, links.Merged)
	}

	*reply = *user
	return nil
}

// GetUser looks a user up by GitHub user ID. One who has never signed in is not
// an error: it is Found false.
func (r *RPCServer) GetUser(args *data.RPCGetUserArgs, reply *data.RPCGetUserReply) error {
	if args.GithubID <= 0 {
		return fmt.Errorf("GetUser: %w: GitHub user ID must be positive, got %d", data.ErrInvalidUser, args.GithubID)
	}
	user, err := r.models.GetUserByGithubID(args.GithubID)
	if errors.Is(err, mongo.ErrNoDocuments) {
		*reply = data.RPCGetUserReply{}
		return nil
	}
	if err != nil {
		log.Println("Error getting user:", err)
		return err
	}
	*reply = data.RPCGetUserReply{Found: true, User: *user}
	return nil
}

// --- API keys ---
//
// The broker authenticates SDK clients and manages keys for the dashboard, but
// only the logger touches MongoDB, so every key operation is one of these.
// Replies never carry a key's hash: gob sends every exported field, whatever
// its json tag says.

// ValidateAPIKey resolves a plaintext key to the active key it belongs to. An
// unknown or revoked key is not an error: it is Valid false.
func (r *RPCServer) ValidateAPIKey(args *data.RPCValidateAPIKeyArgs, reply *data.RPCValidateAPIKeyReply) error {
	valid, key, err := r.models.ValidateAPIKey(args.Plaintext)
	if err != nil {
		log.Println("Error validating API key:", err)
		return err
	}
	reply.Valid = valid
	if key != nil {
		reply.Key = withoutHash(*key)
	}
	return nil
}

func (r *RPCServer) ListAPIKeys(args *data.ProjectArgs, reply *[]data.APIKey) error {
	log.Printf("Listing API keys for project: %s", args.ProjectID)
	projectID, err := parseProjectID("ListAPIKeys", args.ProjectID)
	if err != nil {
		return err
	}
	keys, err := r.models.ListAPIKeysByProject(projectID)
	if err != nil {
		log.Println("Error listing API keys:", err)
		return err
	}
	for i := range keys {
		keys[i] = withoutHash(keys[i])
	}
	*reply = keys
	return nil
}

// CreateAPIKey generates a key for the project and stores it. The plaintext
// goes back to the caller once and is never stored.
func (r *RPCServer) CreateAPIKey(args *data.RPCCreateAPIKeyArgs, reply *data.RPCCreateAPIKeyReply) error {
	log.Printf("Creating API key for project: %s", args.ProjectID)
	projectID, err := parseProjectID("CreateAPIKey", args.ProjectID)
	if err != nil {
		return err
	}
	plaintext, key, err := data.GenerateAPIKey(projectID, args.Scopes)
	if err != nil {
		return fmt.Errorf("CreateAPIKey: %w", err)
	}
	if err := r.models.SaveAPIKey(&key); err != nil {
		log.Println("Error saving API key:", err)
		return err
	}
	reply.Plaintext = plaintext
	reply.Key = withoutHash(key)
	return nil
}

// RevokeAPIKey deactivates a key of a project. An id that names no key of that
// project is data.ErrKeyNotFound.
func (r *RPCServer) RevokeAPIKey(args *data.RPCRevokeAPIKeyArgs, reply *string) error {
	log.Printf("Revoking API key %s of project %s", args.ID, args.ProjectID)
	projectID, err := parseProjectID("RevokeAPIKey", args.ProjectID)
	if err != nil {
		return err
	}
	if err := r.models.RevokeAPIKey(projectID, args.ID); err != nil {
		log.Println("Error revoking API key:", err)
		return err
	}
	*reply = "ok"
	return nil
}

func withoutHash(key data.APIKey) data.APIKey {
	key.Hash = ""
	return key
}
