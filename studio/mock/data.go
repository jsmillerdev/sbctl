package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/OWNER/sbctl/internal/secrets"
)

const (
	orgSlug    = "mock-org"
	orgID      = 1
	region     = "us-east-1"
	provider   = "AWS"
	statusOK   = "ACTIVE_HEALTHY"
	createdAt  = "2026-01-01T00:00:00.000Z"
	dbVersion  = "17.6.1.074"
	subscrID   = "mock-subscription"
	profileNum = 1
)

// disabledFeatures is what GET /platform/profile tells Studio to hide: billing, which the mock
// answers only with stubs, and organization creation.
var disabledFeatures = []string{"billing:all", "organizations:create"}

// entitlementKeys are all feature keys of the entitlements endpoint in the platform spec.
// A missing key means "no access" in platform mode, so the mock grants every one.
var entitlementKeys = []string{
	"api.members.invitations", "api.members.roles", "assistant.advance_model", "auth.advanced_auth_settings",
	"auth.custom_jwt_template", "auth.custom_oauth.max_providers", "auth.hooks", "auth.leaked_password_protection",
	"auth.mfa_enhanced_security", "auth.mfa_phone", "auth.mfa_web_authn", "auth.password_hibp",
	"auth.performance_settings", "auth.platform.sso", "auth.saml_2", "auth.user_sessions",
	"backup.restore_to_new_project", "backup.retention_days", "backup.schedule", "function.max_count",
	"function.size_limit_mb", "instances.compute_update_available_sizes", "instances.disk_modifications",
	"instances.high_availability", "instances.orioledb", "instances.read_replicas", "integrations.github_connections",
	"integrations.github_push_webhooks_limit", "log.retention_days", "observability.dashboard_advanced_metrics",
	"pitr.available_variants", "realtime.max_bytes_per_second", "realtime.max_channels_per_client",
	"realtime.max_concurrent_users", "realtime.max_events_per_second", "realtime.max_joins_per_second",
	"realtime.max_payload_size_in_kb", "realtime.max_presence_events_per_second", "replication.etl",
	"security.audit_logs_days", "security.enforce_mfa", "security.iso27001_certificate", "security.member_roles",
	"security.private_link", "security.questionnaire", "security.soc2_report", "storage.iceberg_catalog",
	"storage.image_transformations", "storage.max_file_size", "storage.max_file_size.configurable",
	"storage.purge_cache", "storage.vector_buckets", "custom_domain", "vanity_subdomain", "ipv4", "log_drains",
	"audit_log_drains", "branching_limit", "branching_persistent",
}

// numericEntitlement reports keys that carry a number rather than a flag.
func numericEntitlement(key string) bool {
	return strings.Contains(key, ".max_") || strings.HasSuffix(key, "_days") || strings.HasSuffix(key, "_limit") ||
		strings.HasSuffix(key, "_mb") || strings.HasSuffix(key, "_kb") || strings.HasSuffix(key, "max_count")
}

func (s *server) entitlements() []map[string]any {
	out := make([]map[string]any, 0, len(entitlementKeys))
	for _, k := range entitlementKeys {
		e := map[string]any{"feature": map[string]any{"key": k}, "hasAccess": true}
		if numericEntitlement(k) {
			e["type"] = "numeric"
			e["config"] = map[string]any{"enabled": true, "unit": "count", "unlimited": true, "value": 0}
		} else {
			e["type"] = "boolean"
			e["config"] = map[string]any{"enabled": true}
		}
		out = append(out, e)
	}
	return out
}

func (s *server) profile(u *user) map[string]any {
	name, _, _ := strings.Cut(u.Email, "@")
	return map[string]any{
		"id": profileNum, "gotrue_id": u.ID, "auth0_id": "email|" + u.ID, "username": name, "primary_email": u.Email,
		"first_name": "Mock", "last_name": "Admin", "mobile": "", "is_alpha_user": false, "free_project_limit": 100,
		"disabled_features": disabledFeatures,
	}
}

func (s *server) permissions() []map[string]any {
	return []map[string]any{{
		"actions": []string{"%"}, "resources": []string{"%"}, "condition": nil, "restrictive": false,
		"organization_id": orgID, "organization_slug": orgSlug, "project_ids": nil, "project_refs": nil,
	}}
}

func (s *server) organization() map[string]any {
	return map[string]any{
		"id": orgID, "slug": orgSlug, "name": "Mock Org", "billing_email": "billing@example.test", "billing_partner": nil,
		"integration_source": nil, "is_owner": true, "opt_in_tags": []string{}, "organization_missing_address": false,
		"organization_missing_tax_id": false, "organization_requires_mfa": false,
		"plan": map[string]any{"id": "enterprise", "name": "Enterprise"}, "requires_indirect_tax_declaration": false,
		"restriction_data": nil, "restriction_status": nil, "stripe_customer_id": nil, "subscription_id": nil,
		"usage_billing_enabled": false,
	}
}

func (s *server) organizationDetail() map[string]any {
	o := s.organization()
	return map[string]any{
		"id": o["id"], "slug": o["slug"], "name": o["name"], "billing_email": o["billing_email"],
		"billing_partner": nil, "has_oriole_project": false, "integration_source": nil, "opt_in_tags": []string{},
		"plan": o["plan"], "restriction_data": nil, "restriction_status": nil, "usage_billing_enabled": false,
	}
}

func (s *server) project(ref string) (*Project, int) {
	for i := range s.cfg.Projects {
		if s.cfg.Projects[i].Ref == ref {
			return &s.cfg.Projects[i], i + 1
		}
	}
	return nil, 0
}

func (s *server) host(p *Project) string { return p.Ref + "." + s.cfg.ProjectHost }

func (s *server) databases(p *Project) []map[string]any {
	return []map[string]any{{
		"identifier": p.Ref, "type": "PRIMARY", "region": region, "cloud_provider": provider, "status": statusOK,
	}}
}

// projectItem is one entry of the organization project list; withOrg adds the fields of the
// global list.
func (s *server) projectItem(p *Project, n int, withOrg bool) map[string]any {
	m := map[string]any{
		"ref": p.Ref, "name": p.Name, "cloud_provider": provider, "region": region, "status": statusOK,
		"inserted_at": createdAt, "integration_source": nil, "is_branch": false, "databases": s.databases(p),
	}
	if withOrg {
		m["id"] = n
		m["organization_id"] = orgID
		m["organization_slug"] = orgSlug
		m["subscription_id"] = subscrID
		m["is_branch_enabled"] = false
		m["is_physical_backups_enabled"] = false
		m["preview_branch_refs"] = []string{}
		delete(m, "is_branch")
		delete(m, "databases")
		delete(m, "integration_source")
	}
	return m
}

func (s *server) projectDetail(p *Project, n int) map[string]any {
	return map[string]any{
		"id": n, "ref": p.Ref, "name": p.Name, "organization_id": orgID, "cloud_provider": provider, "region": region,
		"status": statusOK, "inserted_at": createdAt, "updated_at": createdAt, "subscription_id": subscrID,
		"db_host": "db." + s.host(p), "dbVersion": dbVersion, "connectionString": s.connectionString(p),
		"restUrl": fmt.Sprintf("%s://%s/rest/v1/", s.cfg.Scheme, s.host(p)), "high_availability": false,
		"integration_source": nil, "is_branch_enabled": false, "is_physical_backups_enabled": false,
		"is_hibernating": false,
	}
}

func (s *server) legacyKey(p *Project, role string) string {
	k, err := secrets.NewLegacyKey(p.JWTSecret, p.Ref, role, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		return ""
	}
	return k
}

func (s *server) projectSettings(p *Project) map[string]any {
	return map[string]any{
		"ref": p.Ref, "name": p.Name, "status": statusOK, "region": region, "cloud_provider": provider,
		"inserted_at": createdAt, "db_host": "db." + s.host(p), "db_dns_name": "db." + s.host(p), "db_name": "postgres",
		"db_user": "postgres", "db_port": 5432, "ssl_enforced": false, "is_sensitive": false, "jwt_secret": p.JWTSecret,
		"app_config": map[string]any{"db_schema": "public", "endpoint": s.host(p), "protocol": s.cfg.Scheme},
		"service_api_keys": []map[string]any{
			{"name": "anon key", "tags": "anon", "api_key": s.legacyKey(p, "anon")},
			{"name": "service_role key", "tags": "service_role", "api_key": s.legacyKey(p, "service_role")},
		},
	}
}

func (s *server) apiKeys(p *Project) []map[string]any {
	mk := func(id, name, role string) map[string]any {
		return map[string]any{
			"id": id, "name": name, "type": "legacy", "api_key": s.legacyKey(p, role), "description": nil,
			"hash": nil, "prefix": nil, "inserted_at": createdAt, "updated_at": createdAt, "secret_jwt_template": map[string]any{"role": role},
		}
	}
	return []map[string]any{mk("anon", "anon", "anon"), mk("service_role", "service_role", "service_role")}
}

// primaryDatabase is the project's only database entry (no replicas).
func (s *server) primaryDatabase(p *Project) map[string]any {
	return map[string]any{
		"identifier": p.Ref, "cloud_provider": provider, "region": region, "status": statusOK, "size": "micro",
		"inserted_at": createdAt, "db_host": "db." + s.host(p), "db_name": "postgres", "db_port": 5432, "db_user": "postgres",
		"restUrl": fmt.Sprintf("%s://%s/rest/v1/", s.cfg.Scheme, s.host(p)), "connectionString": s.connectionString(p),
		"connection_string_read_only": nil,
	}
}
