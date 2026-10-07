package config

// Defaults of the [fleet] section.
const (
	// DefaultSupavisorAPIPort is Supavisor's HTTP API (tenant management and /api/health).
	DefaultSupavisorAPIPort = 4001
	// DefaultStorageFileSizeLimit is Storage's per-object upload limit, in bytes (50 MiB,
	// the upstream compose default).
	DefaultStorageFileSizeLimit int64 = 52428800
	// DefaultPoolerMaxClientConn is the most client connections one project's pooler tenant may
	// be configured to hold (max_client_conn): the shared Supavisor serves every project, and its
	// file descriptors and memory are theirs together.
	DefaultPoolerMaxClientConn = 5000
)

// Fleet is the [fleet] config section: settings of the shared services (Supavisor,
// Realtime, Storage, postgres-meta, Studio) that internal/fleet starts. Everything has a
// working default; the environment overrides are SUPAVISE_FLEET_*.
type Fleet struct {
	// SupavisorAPIPort is the port of Supavisor's HTTP API. The pinned artifact reads
	// SUPAVISOR_BIND_IP (a slim-services addition) for the API and the proxy listeners, but
	// it moves 5432 and 6543 to that address too, and those must stay reachable from
	// outside, so supavise does not set it: the API and Supavisor's ephemeral shard listeners
	// listen on every interface. The API needs a JWT signed with a secret only supavise knows,
	// but the host firewall or security group must still close this port and the ephemeral
	// ones (the installer opens 80, 443, 5432 and 6543 only). Zero means DefaultSupavisorAPIPort.
	SupavisorAPIPort int `toml:"supavisor_api_port"`
	// StorageBackend is "file" (default: objects under <state_dir>/system/storage) or "s3".
	StorageBackend string `toml:"storage_backend"`
	// StorageS3Bucket is the bucket of the s3 backend. Storage keeps every project's
	// objects under the key prefix <ref>/, so one bucket serves all projects.
	StorageS3Bucket string `toml:"storage_s3_bucket"`
	// StorageS3Endpoint is empty for AWS and the endpoint URL of any other S3-compatible service.
	StorageS3Endpoint       string `toml:"storage_s3_endpoint"`
	StorageS3Region         string `toml:"storage_s3_region"`
	StorageS3ForcePathStyle bool   `toml:"storage_s3_force_path_style"`
	// StorageS3AccessKeyID and StorageS3SecretAccessKey are optional static credentials.
	// When empty the AWS default chain applies (instance role). The secret sits in
	// config.toml in plain text, so keep that file 0600 and owned by the supavise user.
	StorageS3AccessKeyID     string `toml:"storage_s3_access_key_id"`
	StorageS3SecretAccessKey string `toml:"storage_s3_secret_access_key"`
	// StorageFileSizeLimit is the per-object upload limit in bytes; zero means
	// DefaultStorageFileSizeLimit.
	StorageFileSizeLimit int64 `toml:"storage_file_size_limit"`
	// PoolerMaxClientConn is the ceiling a project may set for max_client_conn on its Supavisor
	// tenant (the Management API refuses more with 400); zero means DefaultPoolerMaxClientConn.
	// A ceiling below Supavisor's own default (1000) does not lower that default.
	PoolerMaxClientConn int `toml:"pooler_max_client_conn"`
}

// SupavisorAPI returns the Supavisor API port with the default applied.
func (f Fleet) SupavisorAPI() int {
	if f.SupavisorAPIPort > 0 {
		return f.SupavisorAPIPort
	}
	return DefaultSupavisorAPIPort
}

// FileSizeLimit returns the Storage upload limit with the default applied.
func (f Fleet) FileSizeLimit() int64 {
	if f.StorageFileSizeLimit > 0 {
		return f.StorageFileSizeLimit
	}
	return DefaultStorageFileSizeLimit
}

// PoolerMaxClients returns the per-project client connection ceiling with the default applied.
func (f Fleet) PoolerMaxClients() int {
	if f.PoolerMaxClientConn > 0 {
		return f.PoolerMaxClientConn
	}
	return DefaultPoolerMaxClientConn
}
