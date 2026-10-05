package data

import (
	"errors"
	"testing"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestValidOrganizationRole(t *testing.T) {
	for _, role := range []string{RoleOwner, RoleAdmin, RoleMember} {
		if !ValidOrganizationRole(role) {
			t.Errorf("ValidOrganizationRole(%q) = false, want true", role)
		}
	}
	for _, role := range []string{"", "Owner", "viewer", " admin"} {
		if ValidOrganizationRole(role) {
			t.Errorf("ValidOrganizationRole(%q) = true, want false", role)
		}
	}
}

// Admin is an organization role only: project roles stay owner and member.
func TestValidRole_NoProjectAdmins(t *testing.T) {
	if ValidRole(RoleAdmin) {
		t.Error("ValidRole(admin) = true; projects have no admins")
	}
}

// An organization owner is owner of every project in the organization; no
// other organization role changes what the project's own membership says.
func TestEffectiveProjectRole(t *testing.T) {
	cases := []struct {
		project, org, want string
	}{
		{"", "", ""},
		{RoleMember, "", RoleMember},
		{RoleOwner, "", RoleOwner},
		{"", RoleOwner, RoleOwner},
		{RoleMember, RoleOwner, RoleOwner},
		{RoleOwner, RoleOwner, RoleOwner},
		{"", RoleAdmin, ""},
		{RoleMember, RoleAdmin, RoleMember},
		{"", RoleMember, ""},
		{RoleOwner, RoleMember, RoleOwner},
	}
	for _, tc := range cases {
		if got := EffectiveProjectRole(tc.project, tc.org); got != tc.want {
			t.Errorf("EffectiveProjectRole(%q, %q) = %q, want %q", tc.project, tc.org, got, tc.want)
		}
	}
}

// The checks below run before any query. The zero-value Models has no database,
// so a call that got past them would panic instead of answering.

func TestCreateOrganizationWithOwner_RefusesBadInput(t *testing.T) {
	var m Models

	cases := []struct {
		name    string
		org     Organization
		userID  int64
		login   string
		wantErr error
	}{
		{"no name", Organization{Plan: "free"}, 42, "jdoe", ErrInvalidOrganization},
		{"blank name", Organization{Name: "  ", Plan: "free"}, 42, "jdoe", ErrInvalidOrganization},
		{"no plan", Organization{Name: "Acme"}, 42, "jdoe", ErrInvalidOrganization},
		{"zero owner id", Organization{Name: "Acme", Plan: "free"}, 0, "jdoe", ErrInvalidUser},
		{"blank owner login", Organization{Name: "Acme", Plan: "free"}, 42, " ", ErrInvalidUser},
	}
	for _, tc := range cases {
		org, err := m.CreateOrganizationWithOwner(tc.org, tc.userID, tc.login)
		if !errors.Is(err, tc.wantErr) {
			t.Errorf("%s: want %v, got %v", tc.name, tc.wantErr, err)
		}
		if org != nil {
			t.Errorf("%s: returned an organization alongside the error: %+v", tc.name, org)
		}
	}
}

func TestRenameOrganization_RefusesBlankName(t *testing.T) {
	var m Models

	if _, err := m.RenameOrganization(primitive.NewObjectID(), "   "); !errors.Is(err, ErrInvalidOrganization) {
		t.Errorf("RenameOrganization to a blank name: want ErrInvalidOrganization, got %v", err)
	}
}

func TestInsertOrganizationMember_RefusesBadInput(t *testing.T) {
	var m Models
	org := primitive.NewObjectID()

	cases := []struct {
		name string
		om   OrganizationMember
	}{
		{"unknown role", OrganizationMember{OrganizationID: org, UserID: 42, GithubLogin: "jdoe", Role: "viewer"}},
		{"zero user id", OrganizationMember{OrganizationID: org, GithubLogin: "jdoe", Role: RoleMember}},
		{"blank login", OrganizationMember{OrganizationID: org, UserID: 42, GithubLogin: " ", Role: RoleMember}},
	}
	for _, tc := range cases {
		if om, err := m.InsertOrganizationMember(tc.om); err == nil {
			t.Errorf("%s: InsertOrganizationMember accepted %+v", tc.name, om)
		}
	}
}

func TestUpdateOrganizationMemberRole_RefusesUnknownRole(t *testing.T) {
	var m Models

	if err := m.UpdateOrganizationMemberRole(primitive.NewObjectID(), primitive.NewObjectID(), "viewer", RoleOwner); err == nil {
		t.Error("UpdateOrganizationMemberRole accepted the role viewer")
	}
}

// A user ID that names no user holds no role and belongs to no organization.
func TestOrganizationLookups_NoUser(t *testing.T) {
	var m Models

	role, err := m.OrganizationRole(primitive.NewObjectID(), 0)
	if err != nil || role != "" {
		t.Errorf("OrganizationRole for user 0 = %q, %v; want no role", role, err)
	}

	orgs, err := m.GetOrganizationsForUser(0)
	if err != nil || orgs == nil || len(orgs) != 0 {
		t.Errorf("GetOrganizationsForUser(0) = %#v, %v; want an empty list", orgs, err)
	}
}
