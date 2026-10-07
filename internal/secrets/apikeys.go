package secrets

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Opaque API keys beyond the first pair. A project always has one publishable and one
// secret key named "default" whose values live in the NamePublishableKey and NameSecretKey
// secrets (key rotation replaces them). Keys created through the Management API, and the
// revocation of any key (the defaults included), are records stored as one sealed secret
// each, named NamePrefixAPIKey + <id>, so the proxy's key cache (invalidated by
// project_secrets changes) and every KeySource see them without another table.

const (
	// NamePrefixAPIKey starts the name of a key record secret.
	NamePrefixAPIKey = "apikey_"
	// NameLegacyKeys holds LegacyKeysState: whether the anon and service_role JWTs are
	// accepted by the proxy. Absent means enabled.
	NameLegacyKeys = "api_keys_legacy"
)

// Key types of opaque keys.
const (
	KeyTypePublishable = "publishable"
	KeyTypeSecret      = "secret"
)

// APIKeyRecord is one stored opaque key. Key is empty for a default key (its value is
// the project's publishable or secret key secret) and for a revoked key (the value is
// erased on revocation).
type APIKeyRecord struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	Type        string         `json:"type"`
	Description string         `json:"description,omitempty"`
	Key         string         `json:"key,omitempty"`
	Template    map[string]any `json:"secret_jwt_template,omitempty"`
	Default     bool           `json:"default,omitempty"`
	Revoked     bool           `json:"revoked,omitempty"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
}

// LegacyKeysState is the stored value of NameLegacyKeys.
type LegacyKeysState struct {
	Enabled bool `json:"enabled"`
}

// OpaqueKey is an active opaque key with its value resolved.
type OpaqueKey struct {
	APIKeyRecord
	Key string `json:"-"`
}

// KeyID is the stable uuid-shaped id of the default key of a type in a project.
func KeyID(ref, name, typ string) string {
	h := sha256.Sum256([]byte(ref + "/" + typ + "/" + name))
	return fmt.Sprintf("%x-%x-%x-%x-%x", h[0:4], h[4:6], h[6:8], h[8:10], h[10:16])
}

// DefaultKeyName is the name of the keys a project starts with.
const DefaultKeyName = "default"

// RecordSecretName is the secret name of the record with this id.
func RecordSecretName(id string) string { return NamePrefixAPIKey + id }

// RecordSet is the stored key records of a project by id.
type RecordSet struct{ ByID map[string]APIKeyRecord }

// Record returns the record with id.
func (k *ProjectKeys) Record(id string) (APIKeyRecord, bool) {
	if k.Records == nil {
		return APIKeyRecord{}, false
	}
	r, ok := k.Records.ByID[id]
	return r, ok
}

// SetRecord adds or replaces a record in this in-memory copy.
func (k *ProjectKeys) SetRecord(r APIKeyRecord) {
	if k.Records == nil {
		k.Records = &RecordSet{ByID: map[string]APIKeyRecord{}}
	}
	k.Records.ByID[r.ID] = r
}

// loadKeyRecords decodes the record and legacy-state secrets of a plaintext secret map.
func (k *ProjectKeys) loadKeyRecords(m map[string]string) {
	k.Records = nil
	for name, v := range m {
		switch {
		case name == NameLegacyKeys:
			var st LegacyKeysState
			if json.Unmarshal([]byte(v), &st) == nil {
				k.LegacyDisabled = !st.Enabled
			}
		case strings.HasPrefix(name, NamePrefixAPIKey):
			var r APIKeyRecord
			if json.Unmarshal([]byte(v), &r) != nil || r.ID == "" {
				continue
			}
			k.SetRecord(r)
		}
	}
}

// OpaqueKeys returns the active opaque keys: the two default keys unless revoked, and
// every unrevoked record, ordered by creation time then id. The proxy accepts exactly
// these as sb_publishable_* and sb_secret_* credentials.
func (k *ProjectKeys) OpaqueKeys(ref string) []OpaqueKey {
	var out []OpaqueKey
	for _, d := range []struct{ typ, val string }{{KeyTypePublishable, k.PublishableKey}, {KeyTypeSecret, k.SecretKey}} {
		if d.val == "" {
			continue
		}
		id := KeyID(ref, DefaultKeyName, d.typ)
		rec, has := k.Record(id)
		if has && rec.Revoked {
			continue
		}
		if !has {
			rec = APIKeyRecord{ID: id, Name: DefaultKeyName, Type: d.typ, Default: true}
		}
		if d.typ == KeyTypeSecret && rec.Template == nil {
			rec.Template = map[string]any{"role": RoleServiceRole}
		}
		out = append(out, OpaqueKey{APIKeyRecord: rec, Key: d.val})
	}
	for _, r := range k.allRecords() {
		if r.Revoked || r.Default || r.Key == "" {
			continue
		}
		out = append(out, OpaqueKey{APIKeyRecord: r, Key: r.Key})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// MarshalRecord is the plaintext stored (then sealed) for r.
func MarshalRecord(r APIKeyRecord) ([]byte, error) { return json.Marshal(r) }

// MarshalLegacyState is the plaintext stored for the legacy-keys switch.
func MarshalLegacyState(enabled bool) []byte {
	b, _ := json.Marshal(LegacyKeysState{Enabled: enabled})
	return b
}

// TemporaryClaim marks the short-lived service_role JWTs the Management API issues for
// the dashboard's own calls (api-keys/temporary). The proxy keeps accepting them while
// the legacy keys are disabled.
const TemporaryClaim = "sbctl_tmp"

func (k *ProjectKeys) allRecords() []APIKeyRecord {
	if k.Records == nil {
		return nil
	}
	out := make([]APIKeyRecord, 0, len(k.Records.ByID))
	for _, r := range k.Records.ByID {
		out = append(out, r)
	}
	return out
}

// AllRecords returns every stored record, revoked ones included, ordered by id.
func (k *ProjectKeys) AllRecords() []APIKeyRecord {
	out := k.allRecords()
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
