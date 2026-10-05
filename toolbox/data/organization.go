package data

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// ErrLastOrganizationOwner is returned when an operation would remove or demote
// the last owner of an organization.
var ErrLastOrganizationOwner = errors.New("cannot remove the last owner of an organization")

// ErrInvalidOrganization is what CreateOrganizationWithOwner answers for an
// organization it cannot create: no name, or no plan.
var ErrInvalidOrganization = errors.New("invalid organization")

// ErrUnknownOrganization is what CreateProjectWithOwner answers for a project
// whose organization does not exist.
var ErrUnknownOrganization = errors.New("organization does not exist")

// ErrOwnerRequired is returned when a change to an organization's members would
// remove, demote or promote an owner, and whoever makes it is not an owner
// themselves: an admin manages members, but only owners decide who the owners
// are.
var ErrOwnerRequired = errors.New("only an owner can change the owners of an organization")

// RoleAdmin is the organization role between owner and member. Projects have no
// admins: their roles are RoleOwner and RoleMember alone (ValidRole).
const RoleAdmin = "admin"

// Organization sits above projects: it owns them, holds the plan, and so sets
// the limits its projects work within. Every project belongs to one
// (Project.OrganizationID).
//
// Plan names the plan the organization is on. BillingCustomerID is the billing
// provider's id for the organization, empty when nothing bills it, as on every
// self-hosted deployment.
type Organization struct {
	ID                primitive.ObjectID `bson:"_id,omitempty" json:"id,omitempty"`
	Name              string             `bson:"name" json:"name"`
	Plan              string             `bson:"plan" json:"plan"`
	BillingCustomerID string             `bson:"billing_customer_id" json:"billing_customer_id"`
	CreatedAt         time.Time          `bson:"created_at" json:"created_at"`
	// Default marks the deployment's organization, which the startup migration
	// creates and puts every project in. At most one organization has it; see
	// EnsureOrganizationIndexes.
	Default bool `bson:"default,omitempty" json:"-"`
	// PendingOwners are logins that become owners at their next sign-in
	// (ClaimPendingOwnerships). A membership needs a user ID, and the Default
	// organization's owners can include logins nobody has signed in under yet.
	PendingOwners []string `bson:"pending_owners,omitempty" json:"-"`
}

// OrganizationMember is one user's membership of one organization.
//
// UserID is the member's GitHub user ID, the key of the users collection, and
// it is who the membership belongs to. Unlike project memberships there are no
// login-only ones: organizations came after user IDs, so every membership names
// its user. GithubLogin is the login the member was added under, normalized,
// kept for display only; GetOrganizationMembers shows a user who has signed in
// under the login of their last sign-in instead.
//
// An organization role is not a project role. Project roles stay what each
// project's own memberships say, except that an organization owner is owner of
// every project in the organization (EffectiveProjectRole).
type OrganizationMember struct {
	ID             primitive.ObjectID `bson:"_id,omitempty" json:"id,omitempty"`
	OrganizationID primitive.ObjectID `bson:"organization_id" json:"organization_id"`
	UserID         int64              `bson:"user_id" json:"user_id"`
	GithubLogin    string             `bson:"github_login" json:"github_login"`
	Role           string             `bson:"role" json:"role"`
	CreatedAt      time.Time          `bson:"created_at" json:"created_at"`
}

// UserOrganization is an organization as seen by one user: the organization
// plus the role that user holds in it.
type UserOrganization struct {
	Organization
	Role string `json:"role"`
}

// RPC argument types for organization and organization member operations. Like
// every RPC argument they carry ids as hex strings; the logger parses them into
// the ObjectIDs the data functions take.

// RPCCreateOrganizationArgs is the RPC argument for CreateOrganization. OwnerID
// and Owner are the GitHub user ID and login of the user who gets the owner
// membership, created with the organization in one transaction.
type RPCCreateOrganizationArgs struct {
	Name    string
	Plan    string
	OwnerID int64
	Owner   string
}

// RPCOrganizationIDArgs is the RPC argument for calls that take only an
// organization ID.
type RPCOrganizationIDArgs struct {
	ID string
}

// RPCUpdateOrganizationArgs is the RPC argument for UpdateOrganization. Only the
// name changes there: the plan is not the members' to pick.
type RPCUpdateOrganizationArgs struct {
	ID   string
	Name string
}

// RPCUserOrganizationsArgs is the RPC argument for ListUserOrganizations.
type RPCUserOrganizationsArgs struct {
	UserID int64
}

// RPCAddOrganizationMemberArgs is the RPC argument for AddOrganizationMember.
// UserID is the new member's GitHub user ID; the login is kept for display.
type RPCAddOrganizationMemberArgs struct {
	OrganizationID string
	UserID         int64
	GithubLogin    string
	Role           string
}

// RPCRemoveOrganizationMemberArgs is the RPC argument for
// RemoveOrganizationMember. MemberID is the membership's own id
// (OrganizationMember.ID). ActorRole is the role in the organization of whoever
// makes the change: anyone but an owner is refused an owner's membership
// (ErrOwnerRequired).
type RPCRemoveOrganizationMemberArgs struct {
	OrganizationID string
	MemberID       string
	ActorRole      string
}

// RPCUpdateOrganizationMemberRoleArgs is the RPC argument for
// UpdateOrganizationMemberRole. MemberID is the membership's own id. ActorRole
// is the role in the organization of whoever makes the change, as in
// RPCRemoveOrganizationMemberArgs.
type RPCUpdateOrganizationMemberRoleArgs struct {
	OrganizationID string
	MemberID       string
	Role           string
	ActorRole      string
}

// RPCOrganizationAccessArgs is the RPC argument for OrganizationAccess: the
// organization, and the caller by GitHub user ID.
type RPCOrganizationAccessArgs struct {
	OrganizationID string
	UserID         int64
}

// OrganizationAccess is the reply of the logger's OrganizationAccess RPC:
// whether the organization exists, and the caller's role in it, empty for a
// non-member. Like ProjectAccess, it answers an access check in one round trip.
type OrganizationAccess struct {
	Exists bool
	Role   string
}

// OrganizationUsage is how much of its plan an organization uses, counted
// against the plan's limits: its projects and its members. Events are not
// counted yet.
type OrganizationUsage struct {
	Projects int64 `json:"projects"`
	Members  int64 `json:"members"`
}

// ValidOrganizationRole reports whether r is a recognised organization member
// role: owner, admin or member.
func ValidOrganizationRole(r string) bool {
	return r == RoleOwner || r == RoleAdmin || r == RoleMember
}

// EffectiveProjectRole is the role a user holds in a project, given their role
// in the project's own memberships (projectRole, empty for none) and in the
// organization the project belongs to (orgRole, empty for none). An
// organization owner is owner of every project in it, whatever the project's
// memberships say; any other organization role grants nothing in a project, so
// the project's own role stands.
func EffectiveProjectRole(projectRole, orgRole string) string {
	if orgRole == RoleOwner {
		return RoleOwner
	}
	return projectRole
}

func (m *Models) organizations() *mongo.Collection {
	return m.client.Database("logs").Collection("organizations")
}

func (m *Models) organizationMembers() *mongo.Collection {
	return m.client.Database("logs").Collection("organization_members")
}

// EnsureOrganizationIndexes creates the indexes on organizations and
// organization_members: the partial unique one that allows a single Default
// organization; pending_owners, which each sign-in looks its login up in; the
// unique (organization_id, user_id), which keeps one membership per user per
// organization; and user_id, which lists a user's organizations. Safe to call on
// startup — CreateOne is idempotent for identical index definitions.
func (m *Models) EnsureOrganizationIndexes() error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Like the Default project's: unique over the organizations that have the
	// flag, which every other one leaves out.
	orgs := m.organizations()
	if _, err := orgs.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "default", Value: 1}},
		Options: options.Index().SetUnique(true).SetName(defaultOrganizationIndexName).
			SetPartialFilterExpression(bson.M{"default": true}),
	}); err != nil {
		return fmt.Errorf("EnsureOrganizationIndexes organizations.default: %w", err)
	}

	if _, err := orgs.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "pending_owners", Value: 1}},
		Options: options.Index().SetName("organization_pending_owners").
			SetPartialFilterExpression(bson.M{"pending_owners": bson.M{"$exists": true}}),
	}); err != nil {
		return fmt.Errorf("EnsureOrganizationIndexes organizations.pending_owners: %w", err)
	}

	members := m.organizationMembers()
	if _, err := members.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "organization_id", Value: 1}, {Key: "user_id", Value: 1}},
		Options: options.Index().SetUnique(true).SetName("unique_organization_member"),
	}); err != nil {
		return fmt.Errorf("EnsureOrganizationIndexes organization_members.(organization_id,user_id): %w", err)
	}

	if _, err := members.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "user_id", Value: 1}},
		Options: options.Index().SetName("organization_member_user_id"),
	}); err != nil {
		return fmt.Errorf("EnsureOrganizationIndexes organization_members.user_id: %w", err)
	}
	return nil
}

// CreateOrganizationWithOwner inserts an organization and an owner membership
// for the user with this GitHub user ID and login in one transaction: an
// organization without an owner could never be managed, so either both
// documents are written or neither is. The organization needs a name and a
// plan; its id and creation time are set here.
func (m *Models) CreateOrganizationWithOwner(o Organization, userID int64, login string) (*Organization, error) {
	o.Name = strings.TrimSpace(o.Name)
	login = NormalizeGithubLogin(login)
	if o.Name == "" {
		return nil, fmt.Errorf("CreateOrganizationWithOwner: %w: name is required", ErrInvalidOrganization)
	}
	if o.Plan == "" {
		return nil, fmt.Errorf("CreateOrganizationWithOwner: %w: plan is required", ErrInvalidOrganization)
	}
	if userID <= 0 {
		return nil, fmt.Errorf("CreateOrganizationWithOwner: %w: GitHub user ID must be positive, got %d", ErrInvalidUser, userID)
	}
	if login == "" {
		return nil, fmt.Errorf("CreateOrganizationWithOwner: %w: login is required", ErrInvalidUser)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	session, err := m.client.StartSession()
	if err != nil {
		return nil, fmt.Errorf("CreateOrganizationWithOwner start session: %w", err)
	}
	defer session.EndSession(ctx)

	o.ID = primitive.NewObjectID()
	o.CreatedAt = time.Now()
	owner := OrganizationMember{
		ID:             primitive.NewObjectID(),
		OrganizationID: o.ID,
		UserID:         userID,
		GithubLogin:    login,
		Role:           RoleOwner,
		CreatedAt:      o.CreatedAt,
	}

	// The ids are fixed before the transaction, so a retry by WithTransaction
	// writes the same two documents again rather than a second organization.
	_, err = session.WithTransaction(ctx, func(sc mongo.SessionContext) (any, error) {
		if _, err := m.organizations().InsertOne(sc, o); err != nil {
			return nil, fmt.Errorf("CreateOrganizationWithOwner organization: %w", err)
		}
		if _, err := m.organizationMembers().InsertOne(sc, owner); err != nil {
			return nil, fmt.Errorf("CreateOrganizationWithOwner owner: %w", err)
		}
		return nil, nil
	})
	if err != nil {
		return nil, err
	}
	return &o, nil
}

// GetOrganization returns the organization with this id, or
// mongo.ErrNoDocuments (wrapped) if there is none.
func (m *Models) GetOrganization(id primitive.ObjectID) (*Organization, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var o Organization
	if err := m.organizations().FindOne(ctx, bson.M{"_id": id}).Decode(&o); err != nil {
		return nil, fmt.Errorf("GetOrganization: %w", err)
	}
	return &o, nil
}

// OrganizationExists reports whether an organization with the given id exists.
func (m *Models) OrganizationExists(ctx context.Context, id primitive.ObjectID) (bool, error) {
	n, err := m.organizations().CountDocuments(ctx, bson.M{"_id": id}, options.Count().SetLimit(1))
	if err != nil {
		return false, fmt.Errorf("OrganizationExists: %w", err)
	}
	return n > 0, nil
}

// ProjectPlan returns the name of the plan of the organization the project is
// in, which a project's limits are resolved from (limits.Provider). A project
// that does not exist is mongo.ErrNoDocuments (wrapped); one in no
// organization, or in one that does not exist, is ErrUnknownOrganization.
func (m *Models) ProjectPlan(ctx context.Context, projectID primitive.ObjectID) (string, error) {
	var p Project
	err := m.client.Database("logs").Collection("projects").
		FindOne(ctx, bson.M{"_id": projectID}, options.FindOne().SetProjection(bson.M{"organization_id": 1})).
		Decode(&p)
	if err != nil {
		return "", fmt.Errorf("ProjectPlan: %w", err)
	}
	if p.OrganizationID.IsZero() {
		return "", fmt.Errorf("ProjectPlan: project %s: %w", projectID.Hex(), ErrUnknownOrganization)
	}

	var o Organization
	err = m.organizations().
		FindOne(ctx, bson.M{"_id": p.OrganizationID}, options.FindOne().SetProjection(bson.M{"plan": 1})).
		Decode(&o)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return "", fmt.Errorf("ProjectPlan: project %s: %w", projectID.Hex(), ErrUnknownOrganization)
	}
	if err != nil {
		return "", fmt.Errorf("ProjectPlan: %w", err)
	}
	return o.Plan, nil
}

// RenameOrganization changes an organization's name, and returns it as stored,
// or mongo.ErrNoDocuments (wrapped) if there is no such organization.
func (m *Models) RenameOrganization(id primitive.ObjectID, name string) (*Organization, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("RenameOrganization: %w: name is required", ErrInvalidOrganization)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var o Organization
	err := m.organizations().FindOneAndUpdate(ctx,
		bson.M{"_id": id},
		bson.M{"$set": bson.M{"name": name}},
		options.FindOneAndUpdate().SetReturnDocument(options.After),
	).Decode(&o)
	if err != nil {
		return nil, fmt.Errorf("RenameOrganization: %w", err)
	}
	return &o, nil
}

// InsertOrganizationMember adds the user om.UserID to the organization
// om.OrganizationID with om.Role, and returns the membership as stored. A user
// who is a member already is refused by the unique index, with a duplicate key
// error.
func (m *Models) InsertOrganizationMember(om OrganizationMember) (*OrganizationMember, error) {
	om.GithubLogin = NormalizeGithubLogin(om.GithubLogin)
	if !ValidOrganizationRole(om.Role) {
		return nil, fmt.Errorf("InsertOrganizationMember: invalid role %q", om.Role)
	}
	if om.UserID <= 0 {
		return nil, fmt.Errorf("InsertOrganizationMember: %w: GitHub user ID must be positive, got %d", ErrInvalidUser, om.UserID)
	}
	if om.GithubLogin == "" {
		return nil, fmt.Errorf("InsertOrganizationMember: %w: login is required", ErrInvalidUser)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	om.ID = primitive.NewObjectID()
	om.CreatedAt = time.Now()
	if _, err := m.organizationMembers().InsertOne(ctx, om); err != nil {
		return nil, fmt.Errorf("InsertOrganizationMember: %w", err)
	}
	return &om, nil
}

// GetOrganizationMembers returns the organization's memberships. A member who
// has signed in is shown under the login of their last sign-in, so a GitHub
// rename shows up here too; the others keep the login they were added under.
func (m *Models) GetOrganizationMembers(orgID primitive.ObjectID) ([]OrganizationMember, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cursor, err := m.organizationMembers().Find(ctx, bson.M{"organization_id": orgID})
	if err != nil {
		return nil, fmt.Errorf("GetOrganizationMembers: %w", err)
	}
	defer cursor.Close(ctx)

	members := []OrganizationMember{}
	if err := cursor.All(ctx, &members); err != nil {
		return nil, fmt.Errorf("GetOrganizationMembers decode: %w", err)
	}

	userIDs := make([]int64, len(members))
	for i, mb := range members {
		userIDs[i] = mb.UserID
	}
	logins, err := m.currentLogins(ctx, userIDs)
	if err != nil {
		return nil, fmt.Errorf("GetOrganizationMembers: %w", err)
	}
	for i, mb := range members {
		if login := logins[mb.UserID]; login != "" {
			members[i].GithubLogin = login
		}
	}
	return members, nil
}

// RemoveOrganizationMember removes the membership memberID from an
// organization, on behalf of someone whose role in it is actorRole. Returns
// ErrOwnerRequired if the member is an owner and actorRole is not,
// ErrLastOrganizationOwner if the member is the sole remaining owner, and
// mongo.ErrNoDocuments if the organization has no such membership.
func (m *Models) RemoveOrganizationMember(orgID, memberID primitive.ObjectID, actorRole string) error {
	return m.changeOrganizationMembers("RemoveOrganizationMember", orgID, func(sc mongo.SessionContext, coll *mongo.Collection) error {
		var target OrganizationMember
		if err := coll.FindOne(sc, bson.M{"_id": memberID, "organization_id": orgID}).Decode(&target); err != nil {
			return fmt.Errorf("RemoveOrganizationMember: %w", err)
		}

		if target.Role == RoleOwner {
			if actorRole != RoleOwner {
				return fmt.Errorf("RemoveOrganizationMember: %w", ErrOwnerRequired)
			}
			if err := refuseLastOrganizationOwner(sc, coll, orgID); err != nil {
				return fmt.Errorf("RemoveOrganizationMember: %w", err)
			}
		}

		result, err := coll.DeleteOne(sc, bson.M{"_id": target.ID, "organization_id": orgID})
		if err != nil {
			return fmt.Errorf("RemoveOrganizationMember delete: %w", err)
		}
		if result.DeletedCount == 0 {
			return fmt.Errorf("RemoveOrganizationMember: %w", mongo.ErrNoDocuments)
		}
		return nil
	})
}

// UpdateOrganizationMemberRole gives the membership memberID a new role, on
// behalf of someone whose role in the organization is actorRole. Returns
// ErrOwnerRequired if the member is an owner, or would become one, and
// actorRole is not owner; ErrLastOrganizationOwner if that would demote the
// sole remaining owner; and mongo.ErrNoDocuments if the organization has no
// such membership. Setting the role a member already holds changes nothing.
func (m *Models) UpdateOrganizationMemberRole(orgID, memberID primitive.ObjectID, role, actorRole string) error {
	if !ValidOrganizationRole(role) {
		return fmt.Errorf("UpdateOrganizationMemberRole: invalid role %q", role)
	}

	return m.changeOrganizationMembers("UpdateOrganizationMemberRole", orgID, func(sc mongo.SessionContext, coll *mongo.Collection) error {
		var target OrganizationMember
		if err := coll.FindOne(sc, bson.M{"_id": memberID, "organization_id": orgID}).Decode(&target); err != nil {
			return fmt.Errorf("UpdateOrganizationMemberRole: %w", err)
		}
		if target.Role == role {
			return nil
		}
		if (target.Role == RoleOwner || role == RoleOwner) && actorRole != RoleOwner {
			return fmt.Errorf("UpdateOrganizationMemberRole: %w", ErrOwnerRequired)
		}

		if target.Role == RoleOwner {
			if err := refuseLastOrganizationOwner(sc, coll, orgID); err != nil {
				return fmt.Errorf("UpdateOrganizationMemberRole: %w", err)
			}
		}

		if _, err := coll.UpdateOne(sc, bson.M{"_id": target.ID}, bson.M{"$set": bson.M{"role": role}}); err != nil {
			return fmt.Errorf("UpdateOrganizationMemberRole update: %w", err)
		}
		return nil
	})
}

// changeOrganizationMembers runs fn, a change to an organization's members that
// must never leave it without an owner, in one transaction serialized on the
// organization's document, like changeMembers does for projects.
func (m *Models) changeOrganizationMembers(op string, orgID primitive.ObjectID, fn func(sc mongo.SessionContext, coll *mongo.Collection) error) error {
	return m.changeMembersOf(op, "organizations", "organization_members", orgID, fn)
}

// refuseLastOrganizationOwner returns ErrLastOrganizationOwner unless the
// organization has an owner besides the one about to be removed or demoted. The
// count is only safe from concurrent changes inside changeOrganizationMembers.
func refuseLastOrganizationOwner(sc mongo.SessionContext, coll *mongo.Collection, orgID primitive.ObjectID) error {
	return refuseLastOwnerOf(sc, coll, bson.M{"organization_id": orgID}, ErrLastOrganizationOwner)
}

// OrganizationRole returns the role in the organization of the user with this
// GitHub user ID, or "" if they are not a member, including when the
// organization does not exist.
func (m *Models) OrganizationRole(orgID primitive.ObjectID, userID int64) (string, error) {
	if userID <= 0 {
		return "", nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var member OrganizationMember
	err := m.organizationMembers().FindOne(ctx,
		bson.M{"organization_id": orgID, "user_id": userID},
		options.FindOne().SetProjection(bson.M{"role": 1}),
	).Decode(&member)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("OrganizationRole: %w", err)
	}
	return member.Role, nil
}

// GetOrganizationUsage counts what the organization has of what its plan
// limits: its projects and its members.
func (m *Models) GetOrganizationUsage(ctx context.Context, orgID primitive.ObjectID) (OrganizationUsage, error) {
	projects, err := m.client.Database("logs").Collection("projects").CountDocuments(ctx, bson.M{"organization_id": orgID})
	if err != nil {
		return OrganizationUsage{}, fmt.Errorf("GetOrganizationUsage projects: %w", err)
	}
	members, err := m.organizationMembers().CountDocuments(ctx, bson.M{"organization_id": orgID})
	if err != nil {
		return OrganizationUsage{}, fmt.Errorf("GetOrganizationUsage members: %w", err)
	}
	return OrganizationUsage{Projects: projects, Members: members}, nil
}

// AccessToProject reports whether the project exists and the role in it of the
// user with this GitHub user ID and current login: their own membership's
// (MemberRole), or owner if they own the project's organization
// (EffectiveProjectRole). A project owner costs one query; anyone else also
// the project's, and their role in its organization.
func (m *Models) AccessToProject(projectID primitive.ObjectID, userID int64, githubLogin string) (ProjectAccess, error) {
	role, err := m.MemberRole(projectID, userID, githubLogin)
	if err != nil {
		return ProjectAccess{}, err
	}
	if role == RoleOwner {
		return ProjectAccess{Exists: true, Role: role}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var p Project
	err = m.client.Database("logs").Collection("projects").
		FindOne(ctx, bson.M{"_id": projectID}, options.FindOne().SetProjection(bson.M{"organization_id": 1})).
		Decode(&p)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return ProjectAccess{}, nil
	}
	if err != nil {
		return ProjectAccess{}, fmt.Errorf("AccessToProject: %w", err)
	}
	if p.OrganizationID.IsZero() {
		return ProjectAccess{Exists: true, Role: role}, nil
	}

	orgRole, err := m.OrganizationRole(p.OrganizationID, userID)
	if err != nil {
		return ProjectAccess{}, fmt.Errorf("AccessToProject: %w", err)
	}
	return ProjectAccess{Exists: true, Role: EffectiveProjectRole(role, orgRole)}, nil
}

// ownedOrganizations returns the ids of the organizations the user with this
// GitHub user ID owns, never nil, so it can go into an $in as it is.
func (m *Models) ownedOrganizations(ctx context.Context, userID int64) ([]primitive.ObjectID, error) {
	if userID <= 0 {
		return []primitive.ObjectID{}, nil
	}
	cursor, err := m.organizationMembers().Find(ctx, bson.M{"user_id": userID, "role": RoleOwner},
		options.Find().SetProjection(bson.M{"organization_id": 1}))
	if err != nil {
		return nil, fmt.Errorf("ownedOrganizations: %w", err)
	}
	defer cursor.Close(ctx)

	var members []OrganizationMember
	if err := cursor.All(ctx, &members); err != nil {
		return nil, fmt.Errorf("ownedOrganizations decode: %w", err)
	}
	ids := make([]primitive.ObjectID, len(members))
	for i, mb := range members {
		ids[i] = mb.OrganizationID
	}
	return ids, nil
}

// GetOrganizationsForUser returns every organization the user with this GitHub
// user ID is a member of, each paired with the role they hold in it.
func (m *Models) GetOrganizationsForUser(userID int64) ([]UserOrganization, error) {
	if userID <= 0 {
		return []UserOrganization{}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	memberCursor, err := m.organizationMembers().Find(ctx, bson.M{"user_id": userID})
	if err != nil {
		return nil, fmt.Errorf("GetOrganizationsForUser members: %w", err)
	}
	defer memberCursor.Close(ctx)

	var members []OrganizationMember
	if err := memberCursor.All(ctx, &members); err != nil {
		return nil, fmt.Errorf("GetOrganizationsForUser members decode: %w", err)
	}
	if len(members) == 0 {
		return []UserOrganization{}, nil
	}

	ids := make([]primitive.ObjectID, len(members))
	roles := make(map[primitive.ObjectID]string, len(members))
	for i, mb := range members {
		ids[i] = mb.OrganizationID
		roles[mb.OrganizationID] = mb.Role
	}

	orgCursor, err := m.organizations().Find(ctx, bson.M{"_id": bson.M{"$in": ids}},
		options.Find().SetSort(bson.D{{Key: "created_at", Value: 1}}))
	if err != nil {
		return nil, fmt.Errorf("GetOrganizationsForUser organizations: %w", err)
	}
	defer orgCursor.Close(ctx)

	var orgs []Organization
	if err := orgCursor.All(ctx, &orgs); err != nil {
		return nil, fmt.Errorf("GetOrganizationsForUser organizations decode: %w", err)
	}

	// A membership row whose organization is gone is left out: the
	// organizations query decides which entries survive.
	result := make([]UserOrganization, 0, len(orgs))
	for _, o := range orgs {
		result = append(result, UserOrganization{Organization: o, Role: roles[o.ID]})
	}
	return result, nil
}

// ClaimPendingOwnerships makes the user with this GitHub user ID an owner of
// every organization that lists login, their current login, among its pending
// owners, and takes the login off the list. Like LinkMemberships, it runs at
// each sign-in, the one moment GitHub vouches for which account holds a login.
// A user who is a member already is promoted: they were named an owner. Returns
// how many organizations the user became an owner of.
//
// Each organization is claimed in its own transaction, serialized like any
// other change to its members, so a failure leaves the login pending there for
// the next sign-in. It does nothing for a login no organization is waiting for.
func (m *Models) ClaimPendingOwnerships(githubID int64, login string) (int64, error) {
	login = NormalizeGithubLogin(login)
	if githubID <= 0 {
		return 0, fmt.Errorf("ClaimPendingOwnerships: %w: GitHub user ID must be positive, got %d", ErrInvalidUser, githubID)
	}
	if login == "" {
		return 0, fmt.Errorf("ClaimPendingOwnerships: %w: login is required", ErrInvalidUser)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cursor, err := m.organizations().Find(ctx, bson.M{"pending_owners": login}, options.Find().SetProjection(bson.M{"_id": 1}))
	if err != nil {
		return 0, fmt.Errorf("ClaimPendingOwnerships: %w", err)
	}
	var orgs []Organization
	if err := cursor.All(ctx, &orgs); err != nil {
		return 0, fmt.Errorf("ClaimPendingOwnerships decode: %w", err)
	}

	var claimed int64
	for _, o := range orgs {
		var owner bool
		err := m.changeOrganizationMembers("ClaimPendingOwnerships", o.ID, func(sc mongo.SessionContext, coll *mongo.Collection) error {
			owner = false

			// Taking the login off the list first, and matching it, means a sign-in
			// running alongside this one that claimed it already changes nothing.
			result, err := m.organizations().UpdateOne(sc,
				bson.M{"_id": o.ID, "pending_owners": login},
				bson.M{"$pull": bson.M{"pending_owners": login}},
			)
			if err != nil {
				return fmt.Errorf("ClaimPendingOwnerships %s: %w", o.ID.Hex(), err)
			}
			if result.ModifiedCount == 0 {
				return nil
			}

			if _, err := coll.UpdateOne(sc,
				bson.M{"organization_id": o.ID, "user_id": githubID},
				bson.M{
					"$set":         bson.M{"role": RoleOwner},
					"$setOnInsert": bson.M{"github_login": login, "created_at": time.Now()},
				},
				options.Update().SetUpsert(true),
			); err != nil {
				return fmt.Errorf("ClaimPendingOwnerships %s owner: %w", o.ID.Hex(), err)
			}
			owner = true
			return nil
		})
		if err != nil {
			return claimed, err
		}
		if owner {
			claimed++
		}
	}
	return claimed, nil
}
