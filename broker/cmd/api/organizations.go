package main

import (
	"context"
	"fmt"
	"log"
	"logwolf-toolbox/data"
	"logwolf-toolbox/limits"
	"net/http"
	"net/rpc"
	"strings"

	"github.com/go-chi/chi/v5"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// --- Organization access ---
//
// Every dashboard route that acts on one organization names it in the path, as
// /organizations/{id}/..., and denies the way project routes do: 404 if the
// organization does not exist (or the id cannot name one), 403 if it exists
// and the caller is not a member, or holds a role below the route's.
// authorizeOrganization is that rule; requireOrganization applies it to the
// routes.

// organizationContext is what requireOrganization hands the handler: the
// organization it checked, the caller's role in it, and the logger connection
// it checked over, open for the handler's own calls.
type organizationContext struct {
	id     string // canonical hex, as ObjectID.Hex writes it
	role   string
	client *rpc.Client
}

const organizationContextKey contextKey = "organization"

// organizationFromContext returns what requireOrganization stored. Handlers
// behind requireOrganization can rely on it being there.
func organizationFromContext(r *http.Request) *organizationContext {
	o, _ := r.Context().Value(organizationContextKey).(*organizationContext)
	return o
}

// organizationRoleMeets reports whether an organization role is enough for
// need: any role for anyMember, admin or owner for adminOnly, owner for
// ownerOnly.
func organizationRoleMeets(role string, need accessLevel) bool {
	switch need {
	case anyMember:
		return data.ValidOrganizationRole(role)
	case adminOnly:
		return role == data.RoleOwner || role == data.RoleAdmin
	default:
		return role == data.RoleOwner
	}
}

// authorizeOrganization checks the caller's access to orgID over client, and
// answers the request itself when it falls short of need. It returns the
// caller's role and whether the handler may go on.
//
// One RPC answers both questions, whether the organization exists and what the
// caller's role in it is. Organization memberships always name a user ID, so
// the login plays no part.
func (app *Config) authorizeOrganization(w http.ResponseWriter, r *http.Request, client *rpc.Client, orgID string, need accessLevel) (string, bool) {
	var access data.OrganizationAccess
	args := data.RPCOrganizationAccessArgs{OrganizationID: orgID, UserID: userIDFromContext(r)}
	if err := client.Call("RPCServer.OrganizationAccess", &args, &access); err != nil {
		app.rpcErrorJSON(w, err, organizationNotFound)
		return "", false
	}

	switch {
	case !access.Exists:
		app.errorJSON(w, fmt.Errorf("organization not found"), http.StatusNotFound)
		return "", false
	case access.Role == "":
		app.errorJSON(w, fmt.Errorf("forbidden"), http.StatusForbidden)
		return "", false
	case !organizationRoleMeets(access.Role, need):
		if need == adminOnly {
			app.errorJSON(w, fmt.Errorf("only an owner or an admin can do this"), http.StatusForbidden)
		} else {
			app.errorJSON(w, fmt.Errorf("only an owner can do this"), http.StatusForbidden)
		}
		return "", false
	}
	return access.Role, true
}

// requireOrganization guards a route whose path names the organization as
// {id}. It checks the caller's access with authorizeOrganization, then hands
// the handler an organizationContext; the logger connection in it is closed
// once the handler returns.
//
// A path id that is not an ObjectID names no organization, so it is a 404
// without a call to the logger.
func (app *Config) requireOrganization(need accessLevel) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id, err := primitive.ObjectIDFromHex(chi.URLParam(r, "id"))
			if err != nil {
				app.errorJSON(w, fmt.Errorf("organization not found"), http.StatusNotFound)
				return
			}

			client, ok := app.dialLogger(w)
			if !ok {
				return
			}
			defer client.Close()

			role, ok := app.authorizeOrganization(w, r, client, id.Hex(), need)
			if !ok {
				return
			}

			o := &organizationContext{id: id.Hex(), role: role, client: client}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), organizationContextKey, o)))
		})
	}
}

// --- Organizations ---

// ListOrganizations lists the caller's organizations, each with their role in it.
func (app *Config) ListOrganizations(w http.ResponseWriter, r *http.Request) {
	client, ok := app.dialLogger(w)
	if !ok {
		return
	}
	defer client.Close()

	var orgs []data.UserOrganization
	args := data.RPCUserOrganizationsArgs{UserID: userIDFromContext(r)}
	if err := client.Call("RPCServer.ListUserOrganizations", &args, &orgs); err != nil {
		app.rpcErrorJSON(w, err, nil)
		return
	}

	if orgs == nil {
		orgs = []data.UserOrganization{}
	}

	app.writeJSON(w, http.StatusOK, jsonResponse{Error: false, Message: "OK!", Data: orgs})
}

// CreateOrganization creates an organization owned by the caller. Which plan it
// starts on is the edition's call (limits.Provider.NewOrganizationPlan), not the
// caller's.
func (app *Config) CreateOrganization(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := app.readJSON(w, r, &body); err != nil {
		app.errorJSON(w, err)
		return
	}

	name := strings.TrimSpace(body.Name)
	if name == "" {
		app.errorJSON(w, fmt.Errorf("name is required"), http.StatusBadRequest)
		return
	}

	client, ok := app.dialLogger(w)
	if !ok {
		return
	}
	defer client.Close()

	// One call: the logger writes the organization and the caller's owner
	// membership in one transaction.
	var org data.Organization
	args := data.RPCCreateOrganizationArgs{
		Name:    name,
		Plan:    app.limitsProvider().NewOrganizationPlan().Name,
		OwnerID: userIDFromContext(r),
		Owner:   userLoginFromContext(r),
	}
	if err := client.Call("RPCServer.CreateOrganization", &args, &org); err != nil {
		app.rpcErrorJSON(w, err, nil)
		return
	}

	app.writeJSON(w, http.StatusCreated, jsonResponse{
		Error:   false,
		Message: "Organization created.",
		Data:    data.UserOrganization{Organization: org, Role: data.RoleOwner},
	})
}

// The handlers from here on sit behind requireOrganization, which has checked
// the caller's access to the organization in the path (routes.go says to which
// level) and hands them the organization and an open logger connection.

// GetOrganization returns the organization with the caller's role in it.
func (app *Config) GetOrganization(w http.ResponseWriter, r *http.Request) {
	o := organizationFromContext(r)

	var org data.Organization
	if err := o.client.Call("RPCServer.GetOrganization", &data.RPCOrganizationIDArgs{ID: o.id}, &org); err != nil {
		app.rpcErrorJSON(w, err, organizationNotFound)
		return
	}

	app.writeJSON(w, http.StatusOK, jsonResponse{Error: false, Message: "OK!", Data: data.UserOrganization{Organization: org, Role: o.role}})
}

// UpdateOrganization renames the organization. Its plan is not the members' to
// change.
func (app *Config) UpdateOrganization(w http.ResponseWriter, r *http.Request) {
	o := organizationFromContext(r)

	var body struct {
		Name string `json:"name"`
	}
	if err := app.readJSON(w, r, &body); err != nil {
		app.errorJSON(w, err)
		return
	}

	name := strings.TrimSpace(body.Name)
	if name == "" {
		app.errorJSON(w, fmt.Errorf("name is required"), http.StatusBadRequest)
		return
	}

	var org data.Organization
	if err := o.client.Call("RPCServer.UpdateOrganization", &data.RPCUpdateOrganizationArgs{ID: o.id, Name: name}, &org); err != nil {
		app.rpcErrorJSON(w, err, organizationNotFound)
		return
	}

	app.writeJSON(w, http.StatusOK, jsonResponse{Error: false, Message: "Organization updated.", Data: data.UserOrganization{Organization: org, Role: o.role}})
}

// CreateOrganizationProject creates a project in the organization, owned by the
// caller. Any member of the organization may: the organization's role grants
// nothing in the project, so the caller's owner membership is what lets them
// in, like a project created with POST /projects.
func (app *Config) CreateOrganizationProject(w http.ResponseWriter, r *http.Request) {
	o := organizationFromContext(r)

	args, ok := app.readNewProject(w, r)
	if !ok {
		return
	}
	args.OrganizationID = o.id

	app.createProject(w, o.client, args)
}

// planResponse is a plan's limits as the dashboard reads them. 0 (Unlimited)
// in any limit means the plan sets none; for max_retention_days, forever.
type planResponse struct {
	Name             string `json:"name"`
	MonthlyEvents    int64  `json:"monthly_events"`
	MaxRetentionDays int    `json:"max_retention_days"`
	MaxProjects      int    `json:"max_projects"`
	MaxMembers       int    `json:"max_members"`
}

type organizationPlanResponse struct {
	Plan  planResponse           `json:"plan"`
	Usage data.OrganizationUsage `json:"usage"`
}

// GetOrganizationPlan answers the organization's plan, resolved by the edition
// (limits.Provider.OrganizationPlan), and how much of it the organization uses.
func (app *Config) GetOrganizationPlan(w http.ResponseWriter, r *http.Request) {
	o := organizationFromContext(r)

	var org data.Organization
	if err := o.client.Call("RPCServer.GetOrganization", &data.RPCOrganizationIDArgs{ID: o.id}, &org); err != nil {
		app.rpcErrorJSON(w, err, organizationNotFound)
		return
	}

	plan, err := app.limitsProvider().OrganizationPlan(org.Plan)
	if err != nil {
		log.Printf(`{"event":"organization_plan","outcome":"error","organization_id":%q,"error":%q}`, o.id, err.Error())
		app.errorJSON(w, fmt.Errorf("could not look up the organization's plan"), http.StatusInternalServerError)
		return
	}

	var usage data.OrganizationUsage
	if err := o.client.Call("RPCServer.OrganizationUsage", &data.RPCOrganizationIDArgs{ID: o.id}, &usage); err != nil {
		app.rpcErrorJSON(w, err, organizationNotFound)
		return
	}

	app.writeJSON(w, http.StatusOK, jsonResponse{Error: false, Message: "OK!", Data: organizationPlanResponse{
		Plan:  planResponseOf(plan),
		Usage: usage,
	}})
}

func planResponseOf(p limits.Plan) planResponse {
	return planResponse{
		Name:             p.Name,
		MonthlyEvents:    p.MonthlyEvents,
		MaxRetentionDays: p.MaxRetentionDays,
		MaxProjects:      p.MaxProjects,
		MaxMembers:       p.MaxMembers,
	}
}

// --- Organization members ---
//
// Admins manage members; only owners decide who the owners are. Adding an
// owner is refused here, from the body alone; removing, demoting or promoting
// one is refused by the logger, which reads the membership's role in the same
// transaction as the change (data.ErrOwnerRequired).

func (app *Config) ListOrganizationMembers(w http.ResponseWriter, r *http.Request) {
	o := organizationFromContext(r)

	var members []data.OrganizationMember
	if err := o.client.Call("RPCServer.ListOrganizationMembers", &data.RPCOrganizationIDArgs{ID: o.id}, &members); err != nil {
		app.rpcErrorJSON(w, err, organizationNotFound)
		return
	}

	if members == nil {
		members = []data.OrganizationMember{}
	}

	app.writeJSON(w, http.StatusOK, jsonResponse{Error: false, Message: "OK!", Data: members})
}

// AddOrganizationMember adds a user to the organization. Like a project invite,
// the body names the login and the GitHub user ID the dashboard resolved it to;
// the membership belongs to the ID.
func (app *Config) AddOrganizationMember(w http.ResponseWriter, r *http.Request) {
	o := organizationFromContext(r)

	var body struct {
		Login  string `json:"login"`
		UserID int64  `json:"user_id"`
		Role   string `json:"role"`
	}
	if err := app.readJSON(w, r, &body); err != nil {
		app.errorJSON(w, err)
		return
	}

	login := data.NormalizeGithubLogin(body.Login)
	if login == "" {
		app.errorJSON(w, fmt.Errorf("login is required"), http.StatusBadRequest)
		return
	}
	if body.UserID <= 0 {
		app.errorJSON(w, fmt.Errorf("user_id must be a positive integer"), http.StatusBadRequest)
		return
	}
	if !data.ValidOrganizationRole(body.Role) {
		app.errorJSON(w, fmt.Errorf("invalid role"), http.StatusBadRequest)
		return
	}
	if body.Role == data.RoleOwner && o.role != data.RoleOwner {
		app.errorJSON(w, fmt.Errorf("only an owner can add an owner"), http.StatusForbidden)
		return
	}

	var reply string
	if err := o.client.Call("RPCServer.AddOrganizationMember", &data.RPCAddOrganizationMemberArgs{
		OrganizationID: o.id,
		UserID:         body.UserID,
		GithubLogin:    login,
		Role:           body.Role,
	}, &reply); err != nil {
		app.rpcErrorJSON(w, err, rpcErrorMessages{
			rpcErrDuplicate: fmt.Sprintf("%s is already a member of this organization", login),
			rpcErrNotFound:  "organization not found",
		})
		return
	}

	app.writeJSON(w, http.StatusCreated, jsonResponse{Error: false, Message: "Member added."})
}

// RemoveOrganizationMember removes the membership named in the path by its own
// id; one of another organization is not found.
func (app *Config) RemoveOrganizationMember(w http.ResponseWriter, r *http.Request) {
	o := organizationFromContext(r)

	var reply string
	if err := o.client.Call("RPCServer.RemoveOrganizationMember", &data.RPCRemoveOrganizationMemberArgs{
		OrganizationID: o.id,
		MemberID:       chi.URLParam(r, "memberID"),
		ActorRole:      o.role,
	}, &reply); err != nil {
		app.rpcErrorJSON(w, err, rpcErrorMessages{
			rpcErrOwnerRequired: "only an owner can remove an owner",
			rpcErrLastOwner:     "cannot remove the last owner",
			rpcErrNotFound:      "member not found",
		})
		return
	}

	app.writeJSON(w, http.StatusOK, jsonResponse{Error: false, Message: "Member removed."})
}

// UpdateOrganizationMemberRole changes the role of the membership named in the
// path. An owner hands the organization over by promoting someone and then
// demoting themselves, so that is allowed as long as another owner remains.
func (app *Config) UpdateOrganizationMemberRole(w http.ResponseWriter, r *http.Request) {
	o := organizationFromContext(r)

	var body struct {
		Role string `json:"role"`
	}
	if err := app.readJSON(w, r, &body); err != nil {
		app.errorJSON(w, err)
		return
	}
	if !data.ValidOrganizationRole(body.Role) {
		app.errorJSON(w, fmt.Errorf("invalid role"), http.StatusBadRequest)
		return
	}

	var reply string
	if err := o.client.Call("RPCServer.UpdateOrganizationMemberRole", &data.RPCUpdateOrganizationMemberRoleArgs{
		OrganizationID: o.id,
		MemberID:       chi.URLParam(r, "memberID"),
		Role:           body.Role,
		ActorRole:      o.role,
	}, &reply); err != nil {
		app.rpcErrorJSON(w, err, rpcErrorMessages{
			rpcErrOwnerRequired: "only an owner can promote or demote an owner",
			rpcErrLastOwner:     "cannot demote the last owner",
			rpcErrNotFound:      "member not found",
		})
		return
	}

	app.writeJSON(w, http.StatusOK, jsonResponse{Error: false, Message: "Role updated."})
}
