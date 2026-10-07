package projectconfig

import (
	"fmt"
	"regexp"
	"strings"
)

// PostgRESTSchema is the settings of a project's PostgREST (V1UpdatePostgrestConfigBody).
var PostgRESTSchema = buildPostgRESTSchema()

func schemaList(v any) any {
	parts := strings.Split(v.(string), ",")
	out := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, ",")
}

func buildPostgRESTSchema() *Schema {
	schemas := Field{Name: "db_schema", Kind: String, Env: "PGRST_DB_SCHEMAS", Default: "public,graphql_public", Normalize: schemaList, MaxLen: 2048,
		Check: func(v any) error {
			n := 0
			for _, s := range strings.Split(v.(string), ",") {
				if s = strings.TrimSpace(s); s == "" {
					continue
				}
				n++
				if strings.ContainsAny(s, "\"'\\") {
					return fmt.Errorf("schema name %q contains a quote or backslash", s)
				}
			}
			if n == 0 {
				// PostgREST falls back to "public" for an empty list, so "off" would not be
				// what runs; the Data API cannot be switched off on a node.
				return fmt.Errorf("must list at least one schema (the Data API cannot be switched off)")
			}
			return nil
		}}
	path := Field{Name: "db_extra_search_path", Kind: String, Env: "PGRST_DB_EXTRA_SEARCH_PATH", Default: "public,extensions", Normalize: schemaList, MaxLen: 2048,
		Check: func(v any) error {
			if strings.TrimSpace(v.(string)) == "" {
				return nil
			}
			return schemas.Check(v)
		}}
	return NewSchema(PostgREST, []Field{
		schemas, path,
		{Name: "max_rows", Kind: Int, Env: "PGRST_DB_MAX_ROWS", Default: int64(1000), Min: 0, Max: 1000000, HasRange: true},
		{Name: "db_pool", Kind: Int, Env: "PGRST_DB_POOL", Default: int64(5), Min: 0, Max: 1000, HasRange: true},
		{Name: "db_pool_acquisition_timeout", Kind: Int, Env: "PGRST_DB_POOL_ACQUISITION_TIMEOUT", Default: int64(10), Min: 0, Max: 60, HasRange: true},
	})
}

// RealtimeSchema is the settings of a project's Realtime tenant (UpdateRealtimeConfigBody).
// Defaults are the server's own TENANT_MAX_* defaults (config/runtime.exs of v2.140.10).
var RealtimeSchema = NewSchema(Realtime, []Field{
	{Name: "private_only", Kind: Bool, Default: false},
	{Name: "connection_pool", Kind: Int, Default: int64(1), Min: 1, Max: 100, HasRange: true},
	{Name: "postgres_changes_pool", Kind: Int, Default: int64(4), Min: 1, Max: 100, HasRange: true},
	{Name: "max_concurrent_users", Kind: Int, Default: int64(200), Min: 1, Max: 300000, HasRange: true},
	{Name: "max_events_per_second", Kind: Int, Default: int64(100), Min: 1, Max: 50000, HasRange: true},
	{Name: "max_bytes_per_second", Kind: Int, Default: int64(100000), Min: 1, Max: 10000000, HasRange: true},
	{Name: "max_channels_per_client", Kind: Int, Default: int64(100), Min: 1, Max: 10000, HasRange: true},
	{Name: "max_joins_per_second", Kind: Int, Default: int64(100), Min: 1, Max: 5000, HasRange: true},
	{Name: "max_presence_events_per_second", Kind: Int, Default: int64(1000), Min: 1, Max: 5000, HasRange: true},
	{Name: "max_payload_size_in_kb", Kind: Int, Default: int64(3000), Min: 1, Max: 10000, HasRange: true},
	{Name: "suspend", Kind: Bool, Default: false},
	{Name: "presence_enabled", Kind: Bool, Default: false},
})

// StorageSchema is the settings of a project's Storage tenant (UpdateStorageConfigBody).
var StorageSchema = buildStorageSchema()

// DefaultStorageFeatures is what Storage reports before any change: the features the
// self-hosted fleet runs (the S3 protocol endpoint) and the ones it does not (image
// transformation needs imgproxy, which is optional; Iceberg and vector buckets need
// services sbctl does not run).
func DefaultStorageFeatures() map[string]any {
	return map[string]any{
		"imageTransformation": map[string]any{"enabled": false},
		"s3Protocol":          map[string]any{"enabled": true},
		"purgeCache":          map[string]any{"enabled": false},
		"icebergCatalog":      map[string]any{"enabled": false, "maxNamespaces": 10, "maxTables": 10, "maxCatalogs": 2},
		"vectorBuckets":       map[string]any{"enabled": false, "maxBuckets": 10, "maxIndexes": 5},
	}
}

// storageFeatureKeys are the features and numeric limits of Storage's tenant API
// (src/http/routes/admin/tenants.ts of v1.79.36).
var storageFeatureKeys = map[string]map[string]Kind{
	"imageTransformation": {"enabled": Bool, "maxResolution": Number},
	"purgeCache":          {"enabled": Bool},
	"s3Protocol":          {"enabled": Bool},
	"icebergCatalog":      {"enabled": Bool, "maxNamespaces": Number, "maxTables": Number, "maxCatalogs": Number},
	"vectorBuckets":       {"enabled": Bool, "maxBuckets": Number, "maxIndexes": Number},
}

// storageFeatures checks the types of the features it knows; a feature or a setting it does
// not know is not an error (the tenant API grows, and Studio saves whole objects) and is
// dropped by normalizeStorageFeatures.
func storageFeatures(v any) error {
	for name, raw := range v.(map[string]any) {
		keys, ok := storageFeatureKeys[name]
		if !ok {
			continue
		}
		obj, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("feature %s must be an object", name)
		}
		for k, x := range obj {
			kind, known := keys[k]
			if !known {
				continue
			}
			switch kind {
			case Bool:
				if _, isBool := x.(bool); !isBool {
					return fmt.Errorf("%s.%s must be a boolean", name, k)
				}
			case Number:
				if n, isNum := x.(float64); !isNum || n < 0 {
					return fmt.Errorf("%s.%s must be a non-negative number", name, k)
				}
			}
		}
	}
	return nil
}

func hasKey(m map[string]Kind, k string) bool { _, ok := m[k]; return ok }

// normalizeStorageFeatures keeps the features and settings Storage's tenant API knows.
func normalizeStorageFeatures(v any) any {
	out := map[string]any{}
	for name, raw := range v.(map[string]any) {
		keys, ok := storageFeatureKeys[name]
		obj, isObj := raw.(map[string]any)
		if !ok || !isObj {
			continue
		}
		kept := map[string]any{}
		for k, x := range obj {
			if hasKey(keys, k) {
				kept[k] = x
			}
		}
		out[name] = kept
	}
	return out
}

func buildStorageSchema() *Schema {
	return NewSchema(Storage, []Field{
		{Name: "fileSizeLimit", Kind: Int, Default: int64(50 * 1024 * 1024), Min: 0, Max: 536870912000, HasRange: true},
		{Name: "features", Kind: Object, Check: storageFeatures, Normalize: normalizeStorageFeatures},
		{Name: "external", Kind: Object, Check: func(v any) error {
			if x, ok := v.(map[string]any)["upstreamTarget"]; ok {
				if s, ok := x.(string); !ok || (s != "main" && s != "canary") {
					return fmt.Errorf("upstreamTarget must be main or canary")
				}
			}
			return nil
		}, Normalize: func(v any) any {
			out := map[string]any{}
			if x, ok := v.(map[string]any)["upstreamTarget"]; ok {
				out["upstreamTarget"] = x
			}
			return out
		}},
	})
}

// PostgresSchema is the settings hosted lets a project change (UpdatePostgresConfigBody),
// rendered as server arguments after the project class's sizing. Values are Postgres
// settings in the form the API uses (sizes and durations as strings with units).
var PostgresSchema = buildPostgresSchema()

var (
	pgSize = regexp.MustCompile(`^(-1|[0-9]+( ?(B|kB|MB|GB|TB))?)$`)
	pgTime = regexp.MustCompile(`^(-1|[0-9]+( ?(us|ms|s|min|h|d))?)$`)
)

// pgRestart lists the settings Postgres applies only at start (context postmaster), plus
// the ones the class puts on the command line, which a reload cannot override.
var pgRestart = map[string]bool{
	"max_connections": true, "max_locks_per_transaction": true, "max_logical_replication_workers": true,
	"max_replication_slots": true, "max_wal_senders": true,
	"max_worker_processes": true, "shared_buffers": true, "track_commit_timestamp": true,
	"track_activity_query_size": true, "effective_cache_size": true, "maintenance_work_mem": true, "max_wal_size": true,
}

// PostgresNeedsRestart reports whether changing the setting needs a restart of Postgres.
func PostgresNeedsRestart(name string) bool { return pgRestart[name] }

func buildPostgresSchema() *Schema {
	size := func(name string) Field {
		return Field{Name: name, Kind: String, Pattern: pgSize, MaxLen: 32}
	}
	dur := func(name string) Field { return Field{Name: name, Kind: String, Pattern: pgTime, MaxLen: 32} }
	boolean := func(name string) Field { return Field{Name: name, Kind: Bool} }
	num := func(name string, min, max float64) Field {
		return Field{Name: name, Kind: Int, Min: min, Max: max, HasRange: true}
	}
	return NewSchema(Postgres, []Field{
		size("effective_cache_size"), size("logical_decoding_work_mem"), boolean("cron.log_statement"),
		dur("log_autovacuum_min_duration"), boolean("log_checkpoints"), boolean("log_connections"),
		boolean("log_disconnections"), boolean("log_duration"), boolean("log_lock_waits"),
		boolean("log_recovery_conflict_waits"), boolean("log_replication_commands"), dur("log_startup_progress_interval"),
		size("log_temp_files"), size("maintenance_work_mem"), size("track_activity_query_size"),
		num("max_connections", 1, 262143), num("max_locks_per_transaction", 10, 2147483640),
		num("max_logical_replication_workers", 0, 262143), num("max_parallel_maintenance_workers", 0, 1024),
		num("max_parallel_workers", 0, 1024), num("max_parallel_workers_per_gather", 0, 1024),
		num("max_replication_slots", 0, 262143), size("max_slot_wal_keep_size"),
		dur("max_standby_archive_delay"), dur("max_standby_streaming_delay"),
		num("max_sync_workers_per_subscription", 0, 262143), size("max_wal_size"), num("max_wal_senders", 0, 262143),
		num("max_worker_processes", 0, 262143),
		{Name: "session_replication_role", Kind: Enum, Enum: []string{"origin", "replica", "local"}},
		size("shared_buffers"), dur("statement_timeout"), boolean("track_commit_timestamp"), size("wal_keep_size"),
		dur("wal_sender_timeout"), size("work_mem"), dur("checkpoint_timeout"), boolean("hot_standby_feedback"),
	})
}

// StorageFeatures is the features object of a storage State: the defaults with the saved
// features laid over them.
func (st *State) StorageFeatures() map[string]any {
	out := DefaultStorageFeatures()
	if saved, ok := st.Set["features"].(map[string]any); ok {
		return mergeObjects(out, saved)
	}
	return out
}

// StorageExternal is the external object of a storage State.
func (st *State) StorageExternal() map[string]any {
	out := map[string]any{"upstreamTarget": "main"}
	if saved, ok := st.Set["external"].(map[string]any); ok {
		return mergeObjects(out, saved)
	}
	return out
}
