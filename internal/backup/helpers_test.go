package backup

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OWNER/sbctl/internal/config"
	"github.com/OWNER/sbctl/internal/registry"
	"github.com/OWNER/sbctl/internal/secrets"
)

const (
	testRef  = "abcdefghijklmnopqrst"
	testRef2 = "tsrqponmlkjihgfedcba"
)

// testEnv is a Service over a FileStore and the in-memory registry.
type testEnv struct {
	svc   *Service
	store *FileStore
	root  string
	reg   *registry.Memory
	sec   secrets.Secrets
	cfg   *config.Config
	now   time.Time
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	root := t.TempDir()
	store, err := NewFileStore(filepath.Join(root, "store"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.StateDir = filepath.Join(root, "state")
	cfg.BinPath = "/usr/local/bin/sbctl"
	sec, err := secrets.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	e := &testEnv{store: store, root: root, reg: registry.NewMemory(), sec: sec, cfg: cfg,
		now: time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)}
	e.svc, err = New(Options{Config: cfg, Registry: e.reg, Store: store, Secrets: sec, Now: func() time.Time { return e.now }})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// addProject registers ref with a full sealed credential set and returns the plain keys.
func (e *testEnv) addProject(t *testing.T, ref string) *secrets.ProjectKeys {
	t.Helper()
	ctx := context.Background()
	if err := e.reg.CreateProject(ctx, &registry.Project{Ref: ref, Name: "test " + ref, Engine: registry.EnginePostgres,
		Status: registry.StatusActiveHealthy, Region: "local"}); err != nil {
		t.Fatal(err)
	}
	k, err := secrets.NewProjectKeys(ref, e.now)
	if err != nil {
		t.Fatal(err)
	}
	for name, v := range k.Map() {
		sealed, err := e.sec.Seal([]byte(v))
		if err != nil {
			t.Fatal(err)
		}
		if err := e.reg.PutSecret(ctx, ref, name, sealed); err != nil {
			t.Fatal(err)
		}
	}
	return k
}

// walName builds a 24-hex-digit segment name.
func walName(tli, seg uint32) string {
	return strings.ToUpper(hex8(tli)) + "00000000" + strings.ToUpper(hex8(seg))
}

func hex8(v uint32) string {
	const digits = "0123456789abcdef"
	b := make([]byte, 8)
	for i := 7; i >= 0; i-- {
		b[i] = digits[v&0xf]
		v >>= 4
	}
	return string(b)
}

func writeFile(t *testing.T, p string, b []byte) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func readAll(t *testing.T, st Store, key string) []byte {
	t.Helper()
	rc, err := st.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func cfgWithBackend(backend string) config.Backup {
	return config.Backup{Backend: backend}
}
