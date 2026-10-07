package app

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jsmillerdev/supavise/internal/config"
	"github.com/jsmillerdev/supavise/internal/lifecycle"
	"github.com/jsmillerdev/supavise/internal/registry"
)

type dirArts map[string]string

func (d dirArts) Dir(svc string) (string, error) {
	if dir, ok := d[svc]; ok {
		return dir, nil
	}
	return "", fmt.Errorf("no artifact for %s", svc)
}
func (d dirArts) Tag(svc string) (string, error) { return filepath.Base(d[svc]), nil }

// freePorts returns n free loopback ports at or above from, inside the private range.
func freePorts(t *testing.T, from, n int) []int {
	t.Helper()
	var out []int
	for p := from; p < from+300 && len(out) < n; p++ {
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err != nil {
			continue
		}
		l.Close()
		out = append(out, p)
	}
	if len(out) < n {
		t.Fatal("no free ports")
	}
	return out
}

// TestServeIntegration runs the daemon the way supavise.service does, on the real artifacts
// under the exec backend: system init, a project created before the daemon starts, the
// daemon bringing it up again at boot, requests through the proxy and the admin listener,
// and a graceful stop on context cancel. It needs SUPAVISE_TEST_UNPACKED (see the lifecycle
// package); it starts two PostgreSQL clusters, GoTrue and PostgREST.
func TestServeIntegration(t *testing.T) {
	root := os.Getenv("SUPAVISE_TEST_UNPACKED")
	if root == "" {
		t.Skip("SUPAVISE_TEST_UNPACKED not set")
	}
	arts := dirArts{}
	for svc, glob := range map[string]string{config.SvcPostgres: "postgres-17*", config.SvcGoTrue: "auth-*", config.SvcPostgREST: "postgrest-*"} {
		m, _ := filepath.Glob(filepath.Join(root, glob))
		if len(m) == 0 {
			t.Skipf("no %s artifact under %s", svc, root)
		}
		arts[svc] = m[len(m)-1]
	}
	truePath, err := exec.LookPath("true")
	if err != nil {
		t.Skip("no true(1)")
	}
	ports := freePorts(t, 34900, 3)  // system Postgres, system GoTrue, project base (3 ports per project follow it)
	listen := freePorts(t, 35300, 3) // proxy HTTP, proxy HTTPS, admin; clear of the project ports above
	state, err := os.MkdirTemp("/tmp", "sba")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(state) })

	cfg := config.Default()
	cfg.StateDir = state
	cfg.KeyPath = filepath.Join(state, "master.key")
	cfg.Supervisor = config.SupervisorExec
	cfg.Domain = "supavise.test"
	cfg.TLS.Mode = "off"
	cfg.Functions.Enabled = true // the daemon must route /functions/v1 and run the syncer; no runtime runs here
	cfg.BinPath = truePath       // archive_command succeeds, so WAL does not pile up
	cfg.Backup.Backend = "file://" + filepath.Join(state, "backups")
	cfg.Ports.SystemPostgres, cfg.Ports.SystemGoTrue, cfg.Ports.ProjectBase = ports[0], ports[1], 35100 // 35100 + 3n: project ports
	cfg.Listen = config.Listen{HTTP: fmt.Sprintf("127.0.0.1:%d", listen[0]), HTTPS: fmt.Sprintf("127.0.0.1:%d", listen[1]), Admin: fmt.Sprintf("127.0.0.1:%d", listen[2])}
	o := Options{Artifacts: arts}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	t.Cleanup(func() {
		if err := lifecycle.StopAll(context.Background(), cfg, LifecycleOptions(cfg, o)); err != nil {
			t.Errorf("StopAll: %v", err)
		}
	})

	n, err := lifecycle.InitSystem(ctx, cfg, LifecycleOptions(cfg, o), false)
	if err != nil {
		t.Fatal(err)
	}
	p, err := n.Engine.Create(ctx, lifecycle.CreateRequest{Name: "app-test", Class: "micro"})
	if err != nil {
		n.Close()
		t.Fatal(err)
	}
	keys, err := n.Engine.Keys(ctx, p.Ref)
	if err != nil {
		t.Fatal(err)
	}
	// What a reboot does: the project's units are gone when the daemon starts.
	for _, svc := range []string{config.SvcPostgREST, config.SvcGoTrue, config.SvcPostgres} {
		if err := n.Supervisor.Stop(ctx, config.UnitName(svc, p.Ref)); err != nil {
			t.Fatal(err)
		}
	}
	n.Close()

	sctx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	served := make(chan struct{}) // closed when Serve has returned, for any number of waiters
	go func() { done <- Serve(sctx, cfg, o); close(served) }()
	t.Cleanup(func() { stop(); <-served })

	get := func(host, path string, hdr ...string) (int, string) {
		req, _ := http.NewRequest("GET", "http://"+cfg.Listen.HTTP+path, nil)
		req.Host = host
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
		if err != nil {
			return 0, err.Error()
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	waitFor := func(what string, f func() (bool, string)) {
		t.Helper()
		var last string
		for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
			var ok bool
			if ok, last = f(); ok {
				return
			}
		}
		t.Fatalf("%s: %s", what, last)
	}

	host := cfg.ProjectHost(p.Ref)
	// The daemon started the project's units at boot: the project API answers through the proxy.
	waitFor("project API through the proxy after boot", func() (bool, string) {
		code, body := get(host, "/auth/v1/settings", "apikey", keys.PublishableKey)
		return code == 200, fmt.Sprint(code, " ", body)
	})
	if code, _ := get(host, "/rest/v1/", "apikey", "sb_publishable_wrong"); code != 401 {
		t.Errorf("bad key = %d, want 401", code)
	}
	// [functions] enabled reaches the proxy: /functions/v1 is routed to the runtime (which is not
	// running in this test, so the answer is an error, but not the one of a node without Edge Functions).
	if code, body := get(host, "/functions/v1/nothing", "apikey", keys.PublishableKey); strings.Contains(body, "not enabled on this node") {
		t.Errorf("/functions/v1 = %d %s: the daemon did not pass [functions] enabled to the proxy", code, body)
	}
	// api.<domain> reaches the Management API (401 without credentials, not 503) and the
	// dashboard GoTrue under /auth/v1.
	if code, body := get(cfg.APIHost(), "/v1/projects"); code != 401 {
		t.Errorf("management API through the proxy = %d %s, want 401", code, body)
	}
	if code, body := get(cfg.APIHost(), "/auth/v1/settings"); code != 200 || !strings.Contains(body, "external") {
		t.Errorf("dashboard GoTrue through the proxy = %d %s", code, body)
	}
	// The admin listener serves the same API.
	resp, err := http.Get("http://" + cfg.Listen.Admin + "/v1/projects")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("admin listener = %d, want 401", resp.StatusCode)
	}

	// Graceful stop on cancel; the project units keep running (they are systemd's).
	stop()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve returned %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Serve did not return after cancel")
	}
	reg, err := registry.Open(context.Background(), lifecycle.RegistryDSN(cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	got, err := reg.GetProject(context.Background(), p.Ref)
	if err != nil || got.Status != registry.StatusActiveHealthy {
		t.Errorf("project after the daemon stopped = %+v %v", got, err)
	}
}
