// Package members is sbctl's organization membership model: who belongs to which
// organization with which role, project-scoped role assignments, invitations, and the
// permissions those roles grant, in the shape Studio and the Management API use (hosted's
// PermissionAction model). The API server evaluates the permissions on every /platform and
// /v1 request; GET /platform/profile/permissions reports the same entries to Studio, so
// what the dashboard hides and what the server refuses cannot drift apart.
package members

import (
	"fmt"
	"strings"
)

// The four organization roles of hosted Supabase. The ids are sbctl's own (the specs only
// say "number") and are stable: they are stored in the registry.
const (
	RoleOwner         = 1
	RoleAdministrator = 2
	RoleDeveloper     = 3
	RoleReadOnly      = 4
)

// ProjectRoleIDBase is the first id of a project-scoped role. Studio lists these next to the
// four organization roles and refers to both in a member's role_ids, so the ranges must not
// overlap. Pending project-scoped invitations are listed under InvitationRoleIDBase + their id.
const (
	ProjectRoleIDBase    = 1000
	InvitationRoleIDBase = 2_000_000_000
)

// Role describes one of the organization roles.
type Role struct {
	ID int
	// Name is the display name Studio sorts and filters by ("Read-only").
	Name string
	// Slug is the name the Management API v2 and the CLI use ("read-only").
	Slug        string
	Description string
}

// Roles lists the organization roles from the most to the least privileged.
var Roles = []Role{
	{RoleOwner, "Owner", "owner",
		"Full access, including removing you or any other owner, deleting the organization, and transferring or deleting projects."},
	{RoleAdministrator, "Administrator", "administrator",
		"Manage members, billing, and project settings, including removing members and deleting projects. Cannot manage organization settings or owners."},
	{RoleDeveloper, "Developer", "developer",
		"Manage project content, including deleting data, users, files, and Edge Functions. Cannot change settings or delete projects."},
	{RoleReadOnly, "Read-only", "read-only",
		"View resources without modifying or deleting them. SQL Editor access is limited to SELECT queries."},
}

// RoleByID returns the organization role with this id.
func RoleByID(id int) (Role, bool) {
	for _, r := range Roles {
		if r.ID == id {
			return r, true
		}
	}
	return Role{}, false
}

// ParseRole accepts a role name or slug in any case: "Owner", "read-only", "Read-only",
// "readonly", "read_only".
func ParseRole(s string) (Role, error) {
	n := strings.ToLower(strings.TrimSpace(s))
	n = strings.NewReplacer("_", "-", " ", "-").Replace(n)
	if n == "readonly" {
		n = "read-only"
	}
	for _, r := range Roles {
		if r.Slug == n {
			return r, nil
		}
	}
	return Role{}, fmt.Errorf("unknown role %q (owner, administrator, developer or read-only)", s)
}

// RoleName is the display name of a base role id ("" when unknown).
func RoleName(id int) string {
	r, _ := RoleByID(id)
	return r.Name
}

// ValidRoleID reports whether id is one of the four organization roles.
func ValidRoleID(id int) bool { _, ok := RoleByID(id); return ok }
