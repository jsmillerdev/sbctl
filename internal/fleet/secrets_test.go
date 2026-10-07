package fleet

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/jsmillerdev/supavise/internal/config"
	"github.com/jsmillerdev/supavise/internal/registry"
)

func TestLoadCredsGeneratesOnceAndKeepsThem(t *testing.T) {
	n := newTestNode(t)
	ctx := context.Background()
	a, err := loadCreds(ctx, n.deps(), true)
	if err != nil {
		t.Fatal(err)
	}
	b, err := loadCreds(ctx, n.deps(), true)
	if err != nil {
		t.Fatal(err)
	}
	if a.supavisorVaultKey != b.supavisorVaultKey || a.realtimeDBEncKeyGCM != b.realtimeDBEncKeyGCM || a.storageEncKey != b.storageEncKey || a.pgmetaKey != b.pgmetaKey {
		t.Fatal("secrets changed between calls; stored tenants would become unreadable")
	}
	// Lengths the services insist on.
	for name, c := range map[string]struct {
		got  string
		want int
	}{
		"supavisor vault key (Cloak AES-256-GCM)": {a.supavisorVaultKey, 32},
		"realtime DB_ENC_KEY (AES-128-ECB)":       {a.realtimeDBEncKey, 16},
		"realtime DB_ENC_KEY_GCM (AES-256-GCM)":   {a.realtimeDBEncKeyGCM, 32},
		"realtime secret_key_base":                {a.realtimeSecretBase, 64},
		"supavisor secret_key_base":               {a.supavisorSecretBase, 64},
		"storage encryption key":                  {a.storageEncKey, 32},
	} {
		if len(c.got) != c.want {
			t.Errorf("%s: %d bytes, want %d", name, len(c.got), c.want)
		}
	}
	seen := map[string]string{}
	for name, v := range map[string]string{
		"a": a.supavisorAPIJWT, "b": a.supavisorMetricsJWT, "c": a.realtimeAPIJWT, "d": a.realtimeMetricsJWT,
		"e": a.storageAdminKey, "f": a.pgmetaKey, "g": a.supavisorSecretBase, "h": a.realtimeSecretBase,
	} {
		if v == "" {
			t.Errorf("secret %s is empty", name)
		}
		if prev, dup := seen[v]; dup {
			t.Errorf("secrets %s and %s are the same value", prev, name)
		}
		seen[v] = name
	}
	if got := a.logins[config.SvcStorage]; got.User != "supavise_storage" || got.Database != "_storage" || got.Password != "pw-storage" {
		t.Fatalf("storage login = %+v", got)
	}
}

func TestLoadCredsWithoutCreateReportsMissing(t *testing.T) {
	n := newTestNode(t)
	_, err := loadCreds(context.Background(), n.deps(), false)
	if err == nil || !strings.Contains(err.Error(), SecretSupavisorAPIJWT) {
		t.Fatalf("err = %v, want it to name the first missing secret", err)
	}
}

func TestLoadCredsNeedsSystemLogins(t *testing.T) {
	n := newTestNode(t)
	n.reg = registry.NewMemory() // an empty registry: system init never ran
	_, err := loadCreds(context.Background(), n.deps(), true)
	if err == nil || !strings.Contains(err.Error(), "system init") {
		t.Fatalf("err = %v, want a hint to run system init", err)
	}
}

func TestPGMetaKeyConfigOverridesRegistry(t *testing.T) {
	n := newTestNode(t)
	n.cfg.API.PGMetaCryptoKey = "from-config"
	c, err := loadCreds(context.Background(), n.deps(), true)
	if err != nil {
		t.Fatal(err)
	}
	if c.pgmetaKey != "from-config" {
		t.Fatalf("pgmeta key = %q", c.pgmetaKey)
	}
	if _, err := n.reg.GetSecret(context.Background(), config.SystemRef, SecretPGMetaCryptoKey); err == nil {
		t.Fatal("a configured key must not also be generated into the registry")
	}
}

func TestEnsureSecretConcurrentCallersAgree(t *testing.T) {
	n := newTestNode(t)
	var wg sync.WaitGroup
	got := make([]string, 8)
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := ensureSecret(context.Background(), n.reg, n.sec, "fleet_test_secret", func() string { return strings.Repeat(string(rune('a'+i)), 10) })
			if err != nil {
				t.Error(err)
			}
			got[i] = v
		}()
	}
	wg.Wait()
	for _, v := range got {
		if v != got[0] {
			t.Fatalf("callers disagree: %v", got)
		}
	}
}
