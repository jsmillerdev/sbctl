package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAPIDisableOAuth(t *testing.T) {
	load := func(t *testing.T, toml string) *Config {
		t.Helper()
		p := filepath.Join(t.TempDir(), "config.toml")
		if err := os.WriteFile(p, []byte(toml), 0o600); err != nil {
			t.Fatal(err)
		}
		c, err := Load(p)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	if load(t, "domain = \"example.com\"\n").API.DisableOAuth {
		t.Error("OAuth is on by default")
	}
	if !load(t, "domain = \"example.com\"\n[api]\ndisable_oauth = true\n").API.DisableOAuth {
		t.Error("[api] disable_oauth is not read")
	}
	t.Setenv("SUPAVISE_API_DISABLE_OAUTH", "true")
	if !load(t, "domain = \"example.com\"\n").API.DisableOAuth {
		t.Error("SUPAVISE_API_DISABLE_OAUTH is not read")
	}
}
