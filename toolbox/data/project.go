package data

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// ErrLastOwner is returned when an operation would remove or demote the last
// owner of a project.
var ErrLastOwner = errors.New("cannot remove the last owner of a project")

// ErrProjectExists is returned by PurgeProjectLogs for a project that has not
// been deleted.
var ErrProjectExists = errors.New("project still exists")

// ErrUnknownProject is what the logger's LogInfo answers for an event whose
// project does not exist, usually one deleted while the event sat in RabbitMQ.
// The listener reads it back out of the RPC error and drops the event rather
// than retrying it.
var ErrUnknownProject = errors.New("project does not exist")

const (
	RoleOwner  = "owner"
	RoleMember = "member"
)

var slugRe = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// Project is a tenant: logs, API keys, settings and members all belong to one.
//
// Its Slug is a label derived from the name when the project is created, for
// display only. Nothing looks a project up by it and it is not unique: projects
// of different users may share one, so creating a project never tells anyone
// that another user's project exists. The Default project that holds
// pre-multi-tenancy data is found by its Default flag instead.
//
// OrganizationID is the organization the project belongs to. Every project is
// created inside one (CreateProjectWithOwner); those stored before
// organizations existed are moved into the Default organization by the startup
// migration (EnsureDefaultOrganization), and have none until then.
type Project struct {
	ID             primitive.ObjectID `bson:"_id,omitempty" json:"id,omitempty"`
	Name           string             `bson:"name" json:"name"`
	Slug           string             `bson:"slug" json:"slug"`
	OrganizationID primitive.ObjectID `bson:"organization_id,omitempty" json:"organization_id,omitempty"`
	CreatedAt      time.Time          `bson:"created_at" json:"created_at"`
	// Default marks the project the startup migration adopts project-less data
	// into. At most one project has it; see EnsureProjectIndexes.
	Default bool `bson:"default,omitempty" json:"-"`
}

// ProjectMember is one user's membership of one project.
//
// UserID is the member's GitHub user ID, the key of the users collection, and
// it is who the membership belongs to: access is decided by it, never by the
// login. GithubLogin is the login the member was added under, normalized, kept
// for display. Memberships stored before user IDs have none (UserID is 0, and
// the field is absent in the database); until they are linked to a user they
// are matched by login, as they always were. See MemberFilter.
type ProjectMember struct {
	ID          primitive.ObjectID `bson:"_id,omitempty" json:"id,omitempty"`
	ProjectID   primitive.ObjectID `bson:"project_id" json:"project_id"`
	UserID      int64              `bson:"user_id,omitempty" json:"user_id,omitempty"`
	GithubLogin string             `bson:"github_login" json:"github_login"`
	Role        string             `bson:"role" json:"role"`
	CreatedAt   time.Time          `bson:"created_at" json:"created_at"`
}

// UserProject is a project as seen by one user: the project itself plus the
// role that user holds in it. Callers listing "my projects" need both, and the
// role is never a property of the project on its own.
type UserProject struct {
	Project
	Role string `json:"role"`
}

// RPC argument types for project and member operations.

// RPCCreateProjectArgs is the RPC argument for CreateProject. OwnerID and Owner
// are the GitHub user ID and login of the user who gets the owner membership,
// created with the project in one transaction.
type RPCCreateProjectArgs struct {
	Name    string
	Slug    string
	OwnerID int64
	Owner   string
}

// RPCProjectIDArgs is the RPC argument for calls that take only a project ID.
type RPCProjectIDArgs struct {
	ID string
}

// RPCUpdateProjectArgs is the RPC argument for UpdateProject. Only the name
// changes: the slug is fixed when the project is created.
type RPCUpdateProjectArgs struct {
	ID   string
	Name string
}

// RPCUserProjectsArgs is the RPC argument for ListUserProjects: the user, by
// GitHub user ID, and their current login, which finds memberships not yet
// linked to a user ID (see MemberFilter).
type RPCUserProjectsArgs struct {
	UserID      int64
	GithubLogin string
}

// RPCAddMemberArgs is the RPC argument for AddMember. UserID is the new
// member's GitHub user ID, which the dashboard resolves from the login the
// owner typed; the login is kept for display.
type RPCAddMemberArgs struct {
	ProjectID   string
	UserID      int64
	GithubLogin string
	Role        string
}

// RPCRemoveMemberArgs is the RPC argument for RemoveMember. MemberID is the
// membership's own id (ProjectMember.ID), which names one row whether or not
// it is linked to a user yet.
type RPCRemoveMemberArgs struct {
	ProjectID string
	MemberID  string
}

// RPCUpdateMemberRoleArgs is the RPC argument for UpdateMemberRole. MemberID is
// the membership's own id, as in RPCRemoveMemberArgs.
type RPCUpdateMemberRoleArgs struct {
	ProjectID string
	MemberID  string
	Role      string
}

// RPCProjectAccessArgs is the RPC argument for ProjectAccess: the project, and
// the caller by GitHub user ID and current login (see MemberFilter).
type RPCProjectAccessArgs struct {
	ProjectID   string
	UserID      int64
	GithubLogin string
}

// ProjectAccess is the reply of the logger's ProjectAccess RPC: whether the
// project exists, and the caller's role in it, empty for a non-member. The
// broker answers every project route from it, in one round trip.
type ProjectAccess struct {
	Exists bool
	Role   string
}

// ValidSlug reports whether s is a valid URL-safe slug.
func ValidSlug(s string) bool {
	return slugRe.MatchString(s)
}

// ValidRole reports whether r is a recognised project member role.
func ValidRole(r string) bool {
	return r == RoleOwner || r == RoleMember
}

// NormalizeGithubLogin returns the form a GitHub login is stored and compared
// in. GitHub logins are case-insensitive, and GitHub hands back its own casing
// at sign-in (JDoe) whatever an owner typed on the settings page (jdoe), so
// every membership read and write goes through this: otherwise the two never
// match, and the unique (project_id, github_login) index lets both in as
// separate members.
func NormalizeGithubLogin(login string) string {
	return strings.ToLower(strings.TrimSpace(login))
}

// MemberFilter matches the memberships that belong to a user: those linked to
// their GitHub user ID, and those stored before user IDs, which carry only a
// login, under their current login. A membership linked to a user ID is never
// matched by login, so a login someone renamed away from, and another account
// then took, does not carry the first user's memberships with it.
//
// A userID that is not positive names no user, and matches only by login.
func MemberFilter(userID int64, githubLogin string) bson.M {
	unlinked := bson.M{"user_id": bson.M{"$exists": false}, "github_login": NormalizeGithubLogin(githubLogin)}
	if userID <= 0 {
		return unlinked
	}
	return bson.M{"$or": bson.A{bson.M{"user_id": userID}, unlinked}}
}

// EnsureProjectIndexes creates the required indexes for projects and project_members.
// Safe to call on startup — CreateOne is idempotent for identical index definitions.
//
// Slugs used to have a unique index of their own; MarkDefaultProject retires it
// during the startup migration, once it has done its last job.
func (m *Models) EnsureProjectIndexes() error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Unique over the projects that have the flag, and only those: every other
	// project leaves it out, so this admits one Default project and any number
	// of others.
	projects := m.client.Database("logs").Collection("projects")
	if _, err := projects.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "default", Value: 1}},
		Options: options.Index().SetUnique(true).SetName(defaultProjectIndexName).
			SetPartialFilterExpression(bson.M{"default": true}),
	}); err != nil {
		return fmt.Errorf("EnsureProjectIndexes projects.default: %w", err)
	}

	// An organization's projects are looked up by it, as are the projects the
	// startup migration has yet to put in one.
	if _, err := projects.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "organization_id", Value: 1}},
		Options: options.Index().SetName("project_organization_id"),
	}); err != nil {
		return fmt.Errorf("EnsureProjectIndexes projects.organization_id: %w", err)
	}

	members := m.client.Database("logs").Collection("project_members")
	if _, err := members.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "project_id", Value: 1}, {Key: "github_login", Value: 1}},
		Options: options.Index().SetUnique(true).SetName("unique_project_member"),
	}); err != nil {
		return fmt.Errorf("EnsureProjectIndexes project_members.(project_id,github_login): %w", err)
	}

	// One membership per user per project. Memberships stored before user IDs
	// have none and stay out of it.
	if _, err := members.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "project_id", Value: 1}, {Key: "user_id", Value: 1}},
		Options: options.Index().SetUnique(true).SetName("unique_project_member_user").
			SetPartialFilterExpression(bson.M{"user_id": bson.M{"$exists": true}}),
	}); err != nil {
		return fmt.Errorf("EnsureProjectIndexes project_members.(project_id,user_id): %w", err)
	}

	// Listing a user's projects looks their memberships up by user ID alone.
	if _, err := members.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "user_id", Value: 1}},
		Options: options.Index().SetName("member_user_id"),
	}); err != nil {
		return fmt.Errorf("EnsureProjectIndexes project_members.user_id: %w", err)
	}

	// ...and the ones not linked yet by login alone, as does LinkMemberships at
	// every sign-in.
	if _, err := members.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "github_login", Value: 1}},
		Options: options.Index().SetName("member_github_login"),
	}); err != nil {
		return fmt.Errorf("EnsureProjectIndexes project_members.github_login: %w", err)
	}

	return nil
}

// MembershipLinks summarises one run of LinkMemberships: how many memberships
// it linked to the user, and how many it merged into a membership the user
// already held in the same project.
type MembershipLinks struct {
	Linked int64
	Merged int64
}

// LinkMemberships attaches the GitHub user ID githubID to every membership
// stored before user IDs under login, the user's current login. It runs at each
// sign-in, the one moment GitHub vouches for which account holds a login: the
// logger holds no GitHub token, so it cannot resolve these logins by itself.
// From then on the membership is the user's, follows them through renames, and
// is never matched by login again (MemberFilter). Memberships of people who
// never sign in again stay login-only.
//
// A membership already linked to anyone, this user or another, is left alone,
// whatever login it stores. Where the user already holds a membership in the
// project, a second one would break the one-membership-per-user rule, so the
// login-only one is merged into it instead: the higher role and the older join
// date survive, as in NormalizeMemberLogins.
//
// It is idempotent, and does nothing for a user with no login-only memberships.
func (m *Models) LinkMemberships(githubID int64, login string) (MembershipLinks, error) {
	var report MembershipLinks
	login = NormalizeGithubLogin(login)
	if githubID <= 0 {
		return report, fmt.Errorf("LinkMemberships: %w: GitHub user ID must be positive, got %d", ErrInvalidUser, githubID)
	}
	if login == "" {
		return report, fmt.Errorf("LinkMemberships: %w: login is required", ErrInvalidUser)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	coll := m.client.Database("logs").Collection("project_members")
	unlinked := bson.M{"user_id": bson.M{"$exists": false}, "github_login": login}

	cursor, err := coll.Find(ctx, unlinked, options.Find().SetProjection(bson.M{"_id": 1, "project_id": 1}))
	if err != nil {
		return report, fmt.Errorf("LinkMemberships: %w", err)
	}
	var found []ProjectMember
	if err := cursor.All(ctx, &found); err != nil {
		return report, fmt.Errorf("LinkMemberships decode: %w", err)
	}

	for _, mb := range found {
		// Matching user_id's absence again means a membership linked in the
		// meantime, by a sign-in running alongside this one, is left alone.
		result, err := coll.UpdateOne(ctx,
			bson.M{"_id": mb.ID, "user_id": bson.M{"$exists": false}},
			bson.M{"$set": bson.M{"user_id": githubID}},
		)
		// Only user_id changes, so only the (project_id, user_id) index can refuse
		// it: the user is a member of this project already.
		if mongo.IsDuplicateKeyError(err) {
			merged, err := m.mergeIntoLinkedMembership(mb.ProjectID, mb.ID, githubID)
			if err != nil {
				return report, err
			}
			if merged {
				report.Merged++
			}
			continue
		}
		if err != nil {
			return report, fmt.Errorf("LinkMemberships %s: %w", mb.ID.Hex(), err)
		}
		report.Linked += result.ModifiedCount
	}
	return report, nil
}

// mergeIntoLinkedMembership folds the login-only membership unlinkedID into the
// membership githubID already holds in the project, and reports whether it did.
// It goes through changeMembers, so it never races a removal or a role change
// on the same project; if the linked membership is gone by then, the login-only
// one is simply linked.
func (m *Models) mergeIntoLinkedMembership(projectID, unlinkedID primitive.ObjectID, githubID int64) (bool, error) {
	var merged bool
	err := m.changeMembers("LinkMemberships", projectID, func(sc mongo.SessionContext, coll *mongo.Collection) error {
		merged = false

		var unlinked ProjectMember
		err := coll.FindOne(sc, bson.M{"_id": unlinkedID, "user_id": bson.M{"$exists": false}}).Decode(&unlinked)
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil // removed or linked in the meantime
		}
		if err != nil {
			return fmt.Errorf("LinkMemberships find %s: %w", unlinkedID.Hex(), err)
		}

		var linked ProjectMember
		err = coll.FindOne(sc, bson.M{"project_id": projectID, "user_id": githubID}).Decode(&linked)
		if errors.Is(err, mongo.ErrNoDocuments) {
			if _, err := coll.UpdateOne(sc, bson.M{"_id": unlinked.ID}, bson.M{"$set": bson.M{"user_id": githubID}}); err != nil {
				return fmt.Errorf("LinkMemberships link %s: %w", unlinked.ID.Hex(), err)
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("LinkMemberships find linked: %w", err)
		}

		// The user is an owner if either membership made them one, so the
		// project keeps at least the owners it had, counted as people.
		set := bson.M{}
		if unlinked.Role == RoleOwner && linked.Role != RoleOwner {
			set["role"] = RoleOwner
		}
		if unlinked.CreatedAt.Before(linked.CreatedAt) {
			set["created_at"] = unlinked.CreatedAt
		}
		if len(set) > 0 {
			if _, err := coll.UpdateOne(sc, bson.M{"_id": linked.ID}, bson.M{"$set": set}); err != nil {
				return fmt.Errorf("LinkMemberships merge into %s: %w", linked.ID.Hex(), err)
			}
		}
		if _, err := coll.DeleteOne(sc, bson.M{"_id": unlinked.ID}); err != nil {
			return fmt.Errorf("LinkMemberships delete %s: %w", unlinked.ID.Hex(), err)
		}
		merged = true
		return nil
	})
	return merged, err
}

func (m *Models) InsertProject(p Project) (*Project, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	p.ID = primitive.NewObjectID()
	p.CreatedAt = time.Now()

	if _, err := m.client.Database("logs").Collection("projects").InsertOne(ctx, p); err != nil {
		return nil, fmt.Errorf("InsertProject: %w", err)
	}
	return &p, nil
}

// CreateProjectWithOwner inserts a project and an owner membership for the user
// with this GitHub user ID and login in one transaction. Only an owner can add
// members, so a project that exists without one is unreachable for good; here
// either both documents are written or neither is.
//
// The project is created inside the organization p.OrganizationID, which must
// exist: ErrUnknownOrganization otherwise.
func (m *Models) CreateProjectWithOwner(p Project, userID int64, login string) (*Project, error) {
	login = NormalizeGithubLogin(login)
	if userID <= 0 {
		return nil, errors.New("CreateProjectWithOwner: owner user ID is required")
	}
	if login == "" {
		return nil, errors.New("CreateProjectWithOwner: owner login is required")
	}
	if p.OrganizationID.IsZero() {
		return nil, errors.New("CreateProjectWithOwner: organization is required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	session, err := m.client.StartSession()
	if err != nil {
		return nil, fmt.Errorf("CreateProjectWithOwner start session: %w", err)
	}
	defer session.EndSession(ctx)

	p.ID = primitive.NewObjectID()
	p.CreatedAt = time.Now()
	owner := ProjectMember{
		ID:          primitive.NewObjectID(),
		ProjectID:   p.ID,
		UserID:      userID,
		GithubLogin: login,
		Role:        RoleOwner,
		CreatedAt:   p.CreatedAt,
	}

	// The ids are fixed before the transaction, so a retry by WithTransaction
	// writes the same two documents again rather than a second project.
	db := m.client.Database("logs")
	_, err = session.WithTransaction(ctx, func(sc mongo.SessionContext) (any, error) {
		exists, err := m.OrganizationExists(sc, p.OrganizationID)
		if err != nil {
			return nil, fmt.Errorf("CreateProjectWithOwner: %w", err)
		}
		if !exists {
			return nil, fmt.Errorf("CreateProjectWithOwner: %w: %s", ErrUnknownOrganization, p.OrganizationID.Hex())
		}
		if _, err := db.Collection("projects").InsertOne(sc, p); err != nil {
			return nil, fmt.Errorf("CreateProjectWithOwner project: %w", err)
		}
		if _, err := db.Collection("project_members").InsertOne(sc, owner); err != nil {
			return nil, fmt.Errorf("CreateProjectWithOwner owner: %w", err)
		}
		return nil, nil
	})
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (m *Models) GetProject(id primitive.ObjectID) (*Project, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var p Project
	err := m.client.Database("logs").Collection("projects").FindOne(ctx, bson.M{"_id": id}).Decode(&p)
	if err != nil {
		return nil, fmt.Errorf("GetProject: %w", err)
	}
	return &p, nil
}

// ProjectExists reports whether a project with the given id exists.
func (m *Models) ProjectExists(ctx context.Context, id primitive.ObjectID) (bool, error) {
	n, err := m.client.Database("logs").Collection("projects").CountDocuments(ctx, bson.M{"_id": id}, options.Count().SetLimit(1))
	if err != nil {
		return false, fmt.Errorf("ProjectExists: %w", err)
	}
	return n > 0, nil
}

func (m *Models) InsertProjectMember(pm ProjectMember) (*ProjectMember, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pm.ID = primitive.NewObjectID()
	pm.GithubLogin = NormalizeGithubLogin(pm.GithubLogin)
	pm.CreatedAt = time.Now()

	if _, err := m.client.Database("logs").Collection("project_members").InsertOne(ctx, pm); err != nil {
		return nil, fmt.Errorf("InsertProjectMember: %w", err)
	}
	return &pm, nil
}

// GetProjectMembers returns the project's memberships. A member linked to a
// user who has signed in is shown under the login of their last sign-in, so a
// GitHub rename shows up here too; the others keep the login they were added
// under.
func (m *Models) GetProjectMembers(projectID primitive.ObjectID) ([]ProjectMember, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cursor, err := m.client.Database("logs").Collection("project_members").Find(ctx, bson.M{"project_id": projectID})
	if err != nil {
		return nil, fmt.Errorf("GetProjectMembers: %w", err)
	}
	defer cursor.Close(ctx)

	var members []ProjectMember
	if err := cursor.All(ctx, &members); err != nil {
		return nil, fmt.Errorf("GetProjectMembers decode: %w", err)
	}

	var userIDs []int64
	for _, mb := range members {
		if mb.UserID > 0 {
			userIDs = append(userIDs, mb.UserID)
		}
	}
	logins, err := m.currentLogins(ctx, userIDs)
	if err != nil {
		return nil, fmt.Errorf("GetProjectMembers: %w", err)
	}
	for i, mb := range members {
		if login := logins[mb.UserID]; login != "" {
			members[i].GithubLogin = login
		}
	}
	return members, nil
}

// currentLogins returns the login each of these users signed in under last,
// keyed by GitHub user ID. A user who has never signed in is missing from it.
func (m *Models) currentLogins(ctx context.Context, userIDs []int64) (map[int64]string, error) {
	logins := map[int64]string{}
	if len(userIDs) == 0 {
		return logins, nil
	}

	cursor, err := m.users().Find(ctx, bson.M{"github_id": bson.M{"$in": userIDs}},
		options.Find().SetProjection(bson.M{"github_id": 1, "github_login": 1}))
	if err != nil {
		return nil, fmt.Errorf("users: %w", err)
	}
	defer cursor.Close(ctx)

	var users []User
	if err := cursor.All(ctx, &users); err != nil {
		return nil, fmt.Errorf("users decode: %w", err)
	}
	for _, u := range users {
		logins[u.GithubID] = u.GithubLogin
	}
	return logins, nil
}

// RenameProject changes a project's name. Its slug stays what it was when the
// project was created.
func (m *Models) RenameProject(id primitive.ObjectID, name string) (*Project, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sr := m.client.Database("logs").Collection("projects").FindOneAndUpdate(
		ctx,
		bson.M{"_id": id},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "name", Value: name},
		}}},
		options.FindOneAndUpdate().SetReturnDocument(options.After),
	)
	if sr.Err() != nil {
		return nil, fmt.Errorf("RenameProject: %w", sr.Err())
	}
	var p Project
	if err := sr.Decode(&p); err != nil {
		return nil, fmt.Errorf("RenameProject decode: %w", err)
	}
	return &p, nil
}

// DeleteProject removes a project with its API keys, settings, and members in one
// transaction: either every collection loses the project's documents or none
// does, so a failure part-way leaves nothing to clean up by hand. Transactions
// need MongoDB to run as a replica set.
//
// The project's logs are not part of it. There can be millions, and deleting
// them inside the transaction would outlast both the timeout here and MongoDB's
// transaction lifetime limit, so the project could never be deleted. Once this
// returns they belong to no project: nobody can read them, PurgeProjectLogs
// removes them, and DeleteOrphanedLogs catches any it misses.
func (m *Models) DeleteProject(id primitive.ObjectID) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	session, err := m.client.StartSession()
	if err != nil {
		return fmt.Errorf("DeleteProject start session: %w", err)
	}
	defer session.EndSession(ctx)

	db := m.client.Database("logs")

	// WithTransaction may run the callback more than once on a transient error;
	// every step is a delete by filter, so a rerun is harmless.
	_, err = session.WithTransaction(ctx, func(sc mongo.SessionContext) (any, error) {
		if _, err := db.Collection("api_keys").DeleteMany(sc, bson.M{"project_id": id}); err != nil {
			return nil, fmt.Errorf("DeleteProject api_keys: %w", err)
		}
		if _, err := db.Collection("settings").DeleteMany(sc, bson.M{"project_id": id}); err != nil {
			return nil, fmt.Errorf("DeleteProject settings: %w", err)
		}
		if _, err := db.Collection("project_members").DeleteMany(sc, bson.M{"project_id": id}); err != nil {
			return nil, fmt.Errorf("DeleteProject project_members: %w", err)
		}
		if _, err := db.Collection("projects").DeleteOne(sc, bson.M{"_id": id}); err != nil {
			return nil, fmt.Errorf("DeleteProject project: %w", err)
		}
		return nil, nil
	})
	return err
}

// RemoveProjectMember removes the membership memberID from a project. Returns
// ErrLastOwner if the member is the sole remaining owner, and
// mongo.ErrNoDocuments if the project has no such membership.
func (m *Models) RemoveProjectMember(projectID, memberID primitive.ObjectID) error {
	return m.changeMembers("RemoveProjectMember", projectID, func(sc mongo.SessionContext, coll *mongo.Collection) error {
		var target ProjectMember
		if err := coll.FindOne(sc, bson.M{"_id": memberID, "project_id": projectID}).Decode(&target); err != nil {
			return fmt.Errorf("RemoveProjectMember: %w", err)
		}

		if target.Role == RoleOwner {
			if err := refuseLastOwner(sc, coll, projectID); err != nil {
				return fmt.Errorf("RemoveProjectMember: %w", err)
			}
		}

		result, err := coll.DeleteOne(sc, bson.M{"_id": target.ID, "project_id": projectID})
		if err != nil {
			return fmt.Errorf("RemoveProjectMember delete: %w", err)
		}
		if result.DeletedCount == 0 {
			return fmt.Errorf("RemoveProjectMember: %w", mongo.ErrNoDocuments)
		}
		return nil
	})
}

// UpdateProjectMemberRole gives the membership memberID a new role. Returns
// ErrLastOwner if that would demote the sole remaining owner, and
// mongo.ErrNoDocuments if the project has no such membership. Setting the role
// a member already holds changes nothing.
func (m *Models) UpdateProjectMemberRole(projectID, memberID primitive.ObjectID, role string) error {
	if !ValidRole(role) {
		return fmt.Errorf("UpdateProjectMemberRole: invalid role %q", role)
	}

	return m.changeMembers("UpdateProjectMemberRole", projectID, func(sc mongo.SessionContext, coll *mongo.Collection) error {
		var target ProjectMember
		if err := coll.FindOne(sc, bson.M{"_id": memberID, "project_id": projectID}).Decode(&target); err != nil {
			return fmt.Errorf("UpdateProjectMemberRole: %w", err)
		}
		if target.Role == role {
			return nil
		}

		if target.Role == RoleOwner {
			if err := refuseLastOwner(sc, coll, projectID); err != nil {
				return fmt.Errorf("UpdateProjectMemberRole: %w", err)
			}
		}

		if _, err := coll.UpdateOne(sc, bson.M{"_id": target.ID}, bson.M{"$set": bson.M{"role": role}}); err != nil {
			return fmt.Errorf("UpdateProjectMemberRole update: %w", err)
		}
		return nil
	})
}

// changeMembers runs fn, a change to a project's members that must never leave
// it without an owner, in one transaction. See changeMembersOf.
func (m *Models) changeMembers(op string, projectID primitive.ObjectID, fn func(sc mongo.SessionContext, coll *mongo.Collection) error) error {
	return m.changeMembersOf(op, "projects", "project_members", projectID, fn)
}

// changeMembersOf runs fn, a change to the members of the document parentID in
// the parents collection that must never leave it without an owner, in one
// transaction. fn gets the members collection.
//
// A transaction alone is not enough: two requests removing or demoting two
// different owners would each read two owners from their own snapshot, write
// different documents, never conflict, and leave the parent with none. So the
// transaction first writes to the parent's own document. The second one to get
// there hits a write conflict, WithTransaction retries it, and the retry counts
// the owners after the first change has committed.
func (m *Models) changeMembersOf(op, parents, members string, parentID primitive.ObjectID, fn func(sc mongo.SessionContext, coll *mongo.Collection) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	session, err := m.client.StartSession()
	if err != nil {
		return fmt.Errorf("%s start session: %w", op, err)
	}
	defer session.EndSession(ctx)

	db := m.client.Database("logs")

	_, err = session.WithTransaction(ctx, func(sc mongo.SessionContext) (any, error) {
		// A membership row can outlive its parent; with no parent document to
		// write to there is nothing to serialize on, and no owner left to protect.
		if _, err := db.Collection(parents).UpdateOne(sc,
			bson.M{"_id": parentID},
			bson.M{"$currentDate": bson.M{"members_updated_at": true}},
		); err != nil {
			return nil, fmt.Errorf("%s lock %s: %w", op, parents, err)
		}
		return nil, fn(sc, db.Collection(members))
	})
	return err
}

// refuseLastOwner returns ErrLastOwner unless the project has an owner besides
// the one about to be removed or demoted. The count is only safe from concurrent
// changes inside changeMembers.
func refuseLastOwner(sc mongo.SessionContext, coll *mongo.Collection, projectID primitive.ObjectID) error {
	return refuseLastOwnerOf(sc, coll, bson.M{"project_id": projectID}, ErrLastOwner)
}

// refuseLastOwnerOf returns last unless the members matching parent include an
// owner besides the one about to be removed or demoted. The count is only safe
// from concurrent changes inside changeMembersOf.
func refuseLastOwnerOf(sc mongo.SessionContext, coll *mongo.Collection, parent bson.M, last error) error {
	owners := bson.M{"role": RoleOwner}
	for k, v := range parent {
		owners[k] = v
	}
	n, err := coll.CountDocuments(sc, owners)
	if err != nil {
		return fmt.Errorf("count owners: %w", err)
	}
	if n <= 1 {
		return last
	}
	return nil
}

// MemberRole returns the role in the project of the user with this GitHub user
// ID and current login, or "" if they are not a member, including when the
// project does not exist. Which memberships are theirs is MemberFilter's rule.
func (m *Models) MemberRole(projectID primitive.ObjectID, userID int64, githubLogin string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	filter := MemberFilter(userID, githubLogin)
	filter["project_id"] = projectID

	var member ProjectMember
	err := m.client.Database("logs").Collection("project_members").FindOne(ctx, filter,
		options.FindOne().SetProjection(bson.M{"role": 1})).Decode(&member)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("MemberRole: %w", err)
	}
	return member.Role, nil
}

func (m *Models) GetAllProjects(ctx context.Context) ([]Project, error) {
	cursor, err := m.client.Database("logs").Collection("projects").Find(ctx, bson.M{})
	if err != nil {
		return nil, fmt.Errorf("GetAllProjects: %w", err)
	}
	defer cursor.Close(ctx)

	var projects []Project
	if err := cursor.All(ctx, &projects); err != nil {
		return nil, fmt.Errorf("GetAllProjects decode: %w", err)
	}
	return projects, nil
}

// GetProjectsForUser returns every project the user with this GitHub user ID
// and current login is a member of, each paired with the role they hold in it.
// Which memberships are theirs is MemberFilter's rule. The projects of the
// organizations they own are theirs too, as owner (EffectiveProjectRole),
// whatever the projects' own memberships say.
func (m *Models) GetProjectsForUser(userID int64, githubLogin string) ([]UserProject, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	memberCursor, err := m.client.Database("logs").Collection("project_members").Find(ctx, MemberFilter(userID, githubLogin))
	if err != nil {
		return nil, fmt.Errorf("GetProjectsForUser members: %w", err)
	}
	defer memberCursor.Close(ctx)

	var members []ProjectMember
	if err := memberCursor.All(ctx, &members); err != nil {
		return nil, fmt.Errorf("GetProjectsForUser members decode: %w", err)
	}

	owned, err := m.ownedOrganizations(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("GetProjectsForUser: %w", err)
	}

	if len(members) == 0 && len(owned) == 0 {
		return []UserProject{}, nil
	}

	ids := make([]primitive.ObjectID, len(members))
	roles := make(map[primitive.ObjectID]string, len(members))
	for i, mb := range members {
		ids[i] = mb.ProjectID
		roles[mb.ProjectID] = mb.Role
	}
	ownedOrg := make(map[primitive.ObjectID]bool, len(owned))
	for _, id := range owned {
		ownedOrg[id] = true
	}

	filter := bson.M{"$or": bson.A{
		bson.M{"_id": bson.M{"$in": ids}},
		bson.M{"organization_id": bson.M{"$in": owned}},
	}}
	projectCursor, err := m.client.Database("logs").Collection("projects").Find(ctx, filter)
	if err != nil {
		return nil, fmt.Errorf("GetProjectsForUser projects: %w", err)
	}
	defer projectCursor.Close(ctx)

	var projects []Project
	if err := projectCursor.All(ctx, &projects); err != nil {
		return nil, fmt.Errorf("GetProjectsForUser projects decode: %w", err)
	}

	// A membership row can outlive its project (the project was deleted while the
	// row lingers); the projects query is what decides which entries survive.
	result := make([]UserProject, 0, len(projects))
	for _, p := range projects {
		var orgRole string
		if ownedOrg[p.OrganizationID] {
			orgRole = RoleOwner
		}
		result = append(result, UserProject{Project: p, Role: EffectiveProjectRole(roles[p.ID], orgRole)})
	}
	return result, nil
}
