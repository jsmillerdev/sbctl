package secrets

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/supavise/supavise/internal/config"
)

// Names of the per-project secrets stored (sealed) in supavise.project_secrets.
const (
	NameJWTSecret      = "jwt_secret"       // HS256 secret shared by GoTrue, PostgREST, Realtime, Storage
	NameAnonKey        = "anon_key"         // legacy anon JWT
	NameServiceRoleKey = "service_role_key" // legacy service_role JWT
	NamePublishableKey = "publishable_key"  // sb_publishable_..., maps to anon
	NameSecretKey      = "secret_key"       // sb_secret_..., maps to service_role
	NameDBPassword     = "db_password"      // password of the "postgres" role
	// NameAdminPassword is the password of supabase_admin, used by supavise, pgmeta and the fleet.
	NameAdminPassword = "admin_password"
	// NameAuthenticatorPassword etc. are the passwords of the service login roles.
	NameAuthenticatorPassword = "authenticator_password"
	NameAuthAdminPassword     = "auth_admin_password"
	NameStorageAdminPassword  = "storage_admin_password"
	NameReplicationPassword   = "replication_password" // role used by pg_basebackup
	NamePGSodiumRootKey       = "pgsodium_root_key"    // 32 bytes hex, per cluster
)

// Prefixes of opaque keys and tokens.
const (
	PrefixPublishable = "sb_publishable_"
	PrefixSecret      = "sb_secret_"
	PrefixPAT         = "sbp_"
)

const (
	lowerAlpha = "abcdefghijklmnopqrstuvwxyz"
	alnum      = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	b58        = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"
)

// RandomString returns n characters drawn uniformly from alphabet using crypto/rand.
func RandomString(n int, alphabet string) string {
	max := big.NewInt(int64(len(alphabet)))
	var sb strings.Builder
	sb.Grow(n)
	for i := 0; i < n; i++ {
		x, err := rand.Int(rand.Reader, max)
		if err != nil {
			panic(err) // crypto/rand never fails on supported platforms
		}
		sb.WriteByte(alphabet[x.Int64()])
	}
	return sb.String()
}

// RandomBytes returns n bytes from crypto/rand.
func RandomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

// NewRef returns a 20-letter lowercase project ref. It never returns "system".
func NewRef() string { return RandomString(20, lowerAlpha) }

// ValidRef reports whether s is a well-formed user project ref.
func ValidRef(s string) bool {
	if len(s) != 20 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 'a' || s[i] > 'z' {
			return false
		}
	}
	return true
}

// ValidProjectRef reports whether s names a project on disk and in the registry: the system
// cluster or a well-formed user project ref. Code that builds a path from a ref it was handed checks
// it with this.
func ValidProjectRef(s string) bool { return s == config.SystemRef || ValidRef(s) }

// NewUUID returns a random (version 4) UUID from crypto/rand in its canonical text form.
func NewUUID() string {
	b := RandomBytes(16)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// NewJWTSecret returns a 40-character secret usable verbatim as an env value by every service.
func NewJWTSecret() string { return RandomString(40, alnum) }

// NewPassword returns a 32-character alphanumeric password (safe in DSNs and env files).
func NewPassword() string { return RandomString(32, alnum) }

// NewPGSodiumRootKey returns a 32-byte key, hex encoded, as pgsodium's getkey script prints it.
func NewPGSodiumRootKey() string { return hex.EncodeToString(RandomBytes(32)) }

// NewPublishableKey and NewSecretKey return opaque API keys.
func NewPublishableKey() string { return PrefixPublishable + Base58(RandomBytes(24)) }
func NewSecretKey() string      { return PrefixSecret + Base58(RandomBytes(24)) }

// NewPAT returns a personal access token: "sbp_" + 40 lowercase hex.
func NewPAT() string { return PrefixPAT + hex.EncodeToString(RandomBytes(20)) }

// HashToken is how PATs and opaque keys are stored and looked up.
func HashToken(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// Base58 encodes b with the Bitcoin alphabet.
func Base58(b []byte) string {
	x := new(big.Int).SetBytes(b)
	base, mod := big.NewInt(58), new(big.Int)
	var out []byte
	for x.Sign() > 0 {
		x.DivMod(x, base, mod)
		out = append(out, b58[mod.Int64()])
	}
	for _, c := range b {
		if c != 0 {
			break
		}
		out = append(out, b58[0])
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return string(out)
}

// Roles carried by legacy API-key JWTs.
const (
	RoleAnon        = "anon"
	RoleServiceRole = "service_role"
)

// legacyKeyTTL matches hosted legacy keys (about ten years).
const legacyKeyTTL = 10 * 365 * 24 * time.Hour

// NewLegacyKey signs a hosted-shaped API-key JWT: {"iss":"supabase","ref":ref,"role":role,"iat","exp"}.
func NewLegacyKey(jwtSecret, ref, role string, now time.Time) (string, error) {
	if role != RoleAnon && role != RoleServiceRole {
		return "", fmt.Errorf("secrets: unknown key role %q", role)
	}
	claims := jwt.MapClaims{
		"iss":  "supabase",
		"ref":  ref,
		"role": role,
		"iat":  now.Unix(),
		"exp":  now.Add(legacyKeyTTL).Unix(),
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(jwtSecret))
}

// ParseHS256 verifies an HS256 JWT and returns its claims. An exp claim is checked when
// present but not required: legacy API keys made by hand from the self-hosting docs may
// carry none.
func ParseHS256(token, jwtSecret string) (jwt.MapClaims, error) {
	return parseHS256(token, jwtSecret)
}

// ParseSessionHS256 is ParseHS256 for a dashboard session: the exp claim is required.
// GoTrue access tokens always carry one, so a token without it is not a GoTrue session.
func ParseSessionHS256(token, jwtSecret string) (jwt.MapClaims, error) {
	return parseHS256(token, jwtSecret, jwt.WithExpirationRequired())
}

func parseHS256(token, jwtSecret string, opts ...jwt.ParserOption) (jwt.MapClaims, error) {
	claims := jwt.MapClaims{}
	opts = append(opts, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}))
	_, err := jwt.ParseWithClaims(token, claims, func(t *jwt.Token) (any, error) {
		return []byte(jwtSecret), nil
	}, opts...)
	if err != nil {
		return nil, err
	}
	return claims, nil
}

// ProjectKeys is the full credential set of one project, decrypted.
type ProjectKeys struct {
	JWTSecret             string
	AnonKey               string
	ServiceRoleKey        string
	PublishableKey        string
	SecretKey             string
	DBPassword            string
	AdminPassword         string
	AuthenticatorPassword string
	AuthAdminPassword     string
	StorageAdminPassword  string
	ReplicationPassword   string
	PGSodiumRootKey       string

	// Records are the stored opaque-key records (see apikeys.go; a pointer, so the struct
	// stays comparable) and LegacyDisabled is true when the anon and service_role JWTs are
	// switched off. Both come from KeysFromMap and are not part of Map: the Management API
	// owns them.
	Records        *RecordSet `json:"-"`
	LegacyDisabled bool       `json:"-"`
}

// NewProjectKeys generates a complete credential set for ref.
func NewProjectKeys(ref string, now time.Time) (*ProjectKeys, error) {
	k := &ProjectKeys{
		JWTSecret:             NewJWTSecret(),
		PublishableKey:        NewPublishableKey(),
		SecretKey:             NewSecretKey(),
		DBPassword:            NewPassword(),
		AdminPassword:         NewPassword(),
		AuthenticatorPassword: NewPassword(),
		AuthAdminPassword:     NewPassword(),
		StorageAdminPassword:  NewPassword(),
		ReplicationPassword:   NewPassword(),
		PGSodiumRootKey:       NewPGSodiumRootKey(),
	}
	return k, k.ResignLegacy(ref, now)
}

// ResignLegacy re-signs the anon and service_role JWTs with the current JWTSecret.
func (k *ProjectKeys) ResignLegacy(ref string, now time.Time) error {
	var err error
	if k.AnonKey, err = NewLegacyKey(k.JWTSecret, ref, RoleAnon, now); err != nil {
		return err
	}
	k.ServiceRoleKey, err = NewLegacyKey(k.JWTSecret, ref, RoleServiceRole, now)
	return err
}

// Map returns the keys by secret name, for storage.
func (k *ProjectKeys) Map() map[string]string {
	return map[string]string{
		NameJWTSecret: k.JWTSecret, NameAnonKey: k.AnonKey, NameServiceRoleKey: k.ServiceRoleKey,
		NamePublishableKey: k.PublishableKey, NameSecretKey: k.SecretKey, NameDBPassword: k.DBPassword,
		NameAdminPassword: k.AdminPassword, NameAuthenticatorPassword: k.AuthenticatorPassword,
		NameAuthAdminPassword: k.AuthAdminPassword, NameStorageAdminPassword: k.StorageAdminPassword,
		NameReplicationPassword: k.ReplicationPassword, NamePGSodiumRootKey: k.PGSodiumRootKey,
	}
}

// KeysFromMap is the inverse of Map. Missing names stay empty.
func KeysFromMap(m map[string]string) *ProjectKeys {
	k := &ProjectKeys{
		JWTSecret: m[NameJWTSecret], AnonKey: m[NameAnonKey], ServiceRoleKey: m[NameServiceRoleKey],
		PublishableKey: m[NamePublishableKey], SecretKey: m[NameSecretKey], DBPassword: m[NameDBPassword],
		AdminPassword: m[NameAdminPassword], AuthenticatorPassword: m[NameAuthenticatorPassword],
		AuthAdminPassword: m[NameAuthAdminPassword], StorageAdminPassword: m[NameStorageAdminPassword],
		ReplicationPassword: m[NameReplicationPassword], PGSodiumRootKey: m[NamePGSodiumRootKey],
	}
	k.loadKeyRecords(m)
	return k
}
