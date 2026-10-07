package members

// Action strings of hosted's PermissionAction enum (@supabase/shared-types 0.1.96, the
// version Studio pins), which Studio's permission checks and the platform spec's
// AccessControlPermission carry.
const (
	ActAnalyticsAdminRead   = "analytics:Admin:Read"
	ActAnalyticsAdminWrite  = "analytics:Admin:Write"
	ActAnalyticsRead        = "analytics:Read"
	ActAnalyticsWrite       = "analytics:Write"
	ActAuthExecute          = "auth:Execute"
	ActBillingRead          = "billing:Read"
	ActBillingWrite         = "billing:Write"
	ActCreate               = "write:Create"
	ActDelete               = "write:Delete"
	ActFunctionsRead        = "functions:Read"
	ActFunctionsWrite       = "functions:Write"
	ActFunctionsSecretRead  = "functions:Secret:Read"
	ActFunctionsSecretWrite = "functions:Secret:Write"
	ActInfraExecute         = "infra:Execute"
	ActRead                 = "read:Read"
	ActSecretsRead          = "secrets:Read"
	ActSecretsWrite         = "secrets:Write"
	ActStorageAdminRead     = "storage:Admin:Read"
	ActStorageAdminWrite    = "storage:Admin:Write"
	ActStorageRead          = "storage:Read"
	ActStorageWrite         = "storage:Write"
	ActRealtimeAdminRead    = "realtime:Admin:Read"
	ActRealtimeAdminWrite   = "realtime:Admin:Write"
	ActReplicationRead      = "replication:Admin:Read"
	ActReplicationWrite     = "replication:Admin:Write"
	ActSQLAdminRead         = "tenant:Sql:Admin:Read"
	ActSQLAdminWrite        = "tenant:Sql:Admin:Write"
	ActSQLCreateTable       = "tenant:Sql:CreateTable"
	ActSQLDelete            = "tenant:Sql:Write:Delete"
	ActSQLInsert            = "tenant:Sql:Write:Insert"
	ActSQLQuery             = "tenant:Sql:Query"
	ActSQLSelect            = "tenant:Sql:Read:Select"
	ActSQLUpdate            = "tenant:Sql:Write:Update"
	ActUpdate               = "write:Update"
)

// Resources the roles distinguish. Studio's own names are kept where it checks them
// (organizations, projects, user_content, auth.subject_roles, user_invites, service_api_keys,
// field.jwt_secret, preview_branches, ...); the others are supavise's, for routes Studio does not
// gate by name.
const (
	// ResSubjectRoles and ResUserInvites guard member and invitation management; their
	// checks carry {"resource": {"role_id": n}}.
	ResSubjectRoles = "auth.subject_roles"
	ResUserInvites  = "user_invites"
	ResOrg          = "organizations"
	ResProjects     = "projects"
	// ResSSO is an organization's single sign-on: its identity providers, their domains and
	// default roles, and the users waiting for approval. Owners and Administrators manage it
	// (reading it too: the domains and roles are configuration, not something every member
	// needs); the default role a provider hands out is checked against the caller's own right to
	// add members with that role, so an Administrator cannot make Owners through it.
	ResSSO = "organizations.sso"
	// ResProjectTransfer is moving a project to another organization: Owners only.
	ResProjectTransfer = "projects.transfer"
	ResUserContent     = "user_content"
	ResPreviewBranches = "preview_branches"
	ResServiceKeys     = "service_api_keys"
	ResJWTSecret       = "field.jwt_secret"
	ResS3Credentials   = "storage.s3_credentials"
	// ResAny is the literal resource Studio passes for checks that are not about one object.
	ResAny = "*"
)

var allActions = []string{"%"}

func perm(actions []string, resources []string, restrictive bool) Permission {
	return Permission{Actions: actions, Resources: resources, Restrictive: restrictive}
}

// readActions are what a Read-only member may do on every resource: read, query with
// SELECT (the API runs a Read-only member's SQL as the read-only database role), list.
var readActions = []string{
	ActRead, ActAnalyticsRead, ActAnalyticsAdminRead, ActBillingRead, ActFunctionsRead, ActFunctionsSecretRead,
	ActSecretsRead, ActStorageRead, ActStorageAdminRead, ActRealtimeAdminRead, ActReplicationRead,
	ActSQLAdminRead, ActSQLQuery, ActSQLSelect,
}

// contentActions are what a Developer adds: project content (data, users, files, functions,
// schema) but no settings, keys, secrets or lifecycle.
var contentActions = []string{
	ActAuthExecute, ActFunctionsWrite, ActStorageWrite, ActStorageAdminWrite, ActAnalyticsWrite,
	ActSQLAdminWrite, ActSQLCreateTable, ActSQLDelete, ActSQLInsert, ActSQLUpdate,
}

var writeActions = []string{ActCreate, ActUpdate, ActDelete}

// developerInfra are the infra:Execute resources a Developer holds: restarting the project.
// Restoring a backup or a point in time (queue_job.restore.prepare and
// queue_job.walg.prepare_restore, which Studio checks before it offers the buttons) stays with
// Owners and Administrators, because it overwrites the production data.
var developerInfra = []string{"reboot"}

// ownContent conditions a write on saved content to the caller's own items, the check Studio
// makes by passing the item's owner_id and the signed-in profile id as subject.id. extra, when
// not nil, is a further condition that must hold.
func ownContent(p Permission, extra any) Permission {
	own := map[string]any{"==": []any{map[string]any{"var": "resource.owner_id"}, map[string]any{"var": "subject.id"}}}
	if extra == nil {
		p.Condition = own
	} else {
		p.Condition = map[string]any{"and": []any{own, extra}}
	}
	return p
}

// OwnContentData is the condition data of a check on a saved item: the item's type, visibility
// and owner (profile id) and the caller's profile id. Studio passes the same shape.
func OwnContentData(typ, visibility string, ownerID, subjectID int64) map[string]any {
	return map[string]any{
		"resource": map[string]any{"type": typ, "visibility": visibility, "owner_id": ownerID},
		"subject":  map[string]any{"id": subjectID},
	}
}

// secretReads are the resources a Read-only member cannot read: the service key and JWT
// secret (hosted: "Read service key" and "JWT Secret" are Owner, Administrator and Developer
// only) and the Storage S3 credentials.
var secretReads = []string{ResServiceKeys, ResJWTSecret, ResS3Credentials}

// roleEntries returns the permission entries of a base role, without organization or project
// scope. ownerRoleIDs are the ids an Administrator may not manage (the Owner role and the
// project-scoped Owner roles of the organization).
func roleEntries(role int, ownerRoleIDs []int64) []Permission {
	switch role {
	case RoleOwner:
		return []Permission{perm(allActions, allActions, false)}
	case RoleAdministrator:
		ids := make([]any, 0, len(ownerRoleIDs)+1)
		ids = append(ids, float64(RoleOwner))
		for _, id := range ownerRoleIDs {
			ids = append(ids, float64(id))
		}
		owners := perm(writeActions, []string{ResSubjectRoles, ResUserInvites}, true)
		owners.Condition = map[string]any{"in": []any{map[string]any{"var": "resource.role_id"}, ids}}
		return []Permission{
			perm(allActions, allActions, false),
			// Not organization settings, deleting the organization, or owners.
			perm([]string{ActUpdate, ActDelete}, []string{ResOrg}, true),
			perm([]string{ActUpdate}, []string{ResProjectTransfer}, true),
			owners,
		}
	case RoleDeveloper:
		return []Permission{
			perm(readActions, allActions, false),
			perm(contentActions, allActions, false),
			perm([]string{ActCreate}, []string{ResUserContent}, false),
			ownContent(perm([]string{ActUpdate, ActDelete}, []string{ResUserContent}, false), nil),
			perm(writeActions, []string{ResPreviewBranches}, false),
			// Hosted lists Restart for Developers; Pause, Restore and Delete stay with
			// Administrators. Hosted also lists the backup restores for Developers; Supavise
			// keeps them for Owners and Administrators on purpose, because a restore
			// overwrites the project's data.
			perm([]string{ActInfraExecute}, developerInfra, false),
		}
	case RoleReadOnly:
		noReports := map[string]any{"!=": []any{map[string]any{"var": "resource.type"}, "report"}}
		create := perm([]string{ActCreate}, []string{ResUserContent}, false)
		create.Condition = noReports
		return []Permission{
			perm(readActions, allActions, false),
			// Hosted: SQL snippets belong to every role, custom reports to Developers and up.
			create,
			ownContent(perm([]string{ActUpdate, ActDelete}, []string{ResUserContent}, false), noReports),
			perm([]string{ActRead}, secretReads, true),
		}
	}
	return nil
}

// scopedMemberEntries is what every member of an organization may do on the organization
// itself, even one who holds project-scoped roles only: see it and its member list.
func scopedMemberEntries() []Permission {
	return []Permission{perm([]string{ActRead}, []string{ResOrg}, false)}
}
