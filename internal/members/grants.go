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
// field.jwt_secret, preview_branches, ...); the others are sbctl's, for routes Studio does not
// gate by name.
const (
	// ResSubjectRoles and ResUserInvites guard member and invitation management; their
	// checks carry {"resource": {"role_id": n}}.
	ResSubjectRoles = "auth.subject_roles"
	ResUserInvites  = "user_invites"
	ResOrg          = "organizations"
	ResProjects     = "projects"
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
			perm(writeActions, []string{ResUserContent, ResPreviewBranches}, false),
		}
	case RoleReadOnly:
		return []Permission{
			perm(readActions, allActions, false),
			perm(writeActions, []string{ResUserContent}, false),
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
