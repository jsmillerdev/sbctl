//go:build fleetfollower

package fleet_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/supavise/supavise/internal/api"
	"github.com/supavise/supavise/internal/artifacts"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/fleet"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/proxy"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
	"github.com/supavise/supavise/internal/units"
)

// TestLinuxFollower is the follower half of tests/linux/fleet-follower.sh, which builds it with
// -tags fleetfollower and runs it as the supavise user on the CI runner. The runner holds the
// leader (a node installed the usual way, its project, its Supavisor under systemd) and, built by
// the script, hot standbys of the leader's system cluster and of the project. This test is the
// daemon of a second node whose system cluster is the standby: it opens that registry read-only,
// starts the shared services as that node would (fleet.Setup with Start, under the exec
// supervisor so that nothing clashes with the leader's units), and checks what design 2.6 and
// spike S1 promise. It ends with the remote MCP endpoint on that node (followerMCP).
func TestLinuxFollower(t *testing.T) {
	followerConf, leaderConf := os.Getenv("FLEET_FOLLOWER_CONFIG"), os.Getenv("FLEET_LEADER_CONFIG")
	if followerConf == "" || leaderConf == "" {
		t.Skip("run by tests/linux/fleet-follower.sh")
	}
	ref, password := os.Getenv("FLEET_REF"), os.Getenv("FLEET_DB_PASSWORD")
	replicaPort, err := strconv.Atoi(os.Getenv("FLEET_REPLICA_PORT"))
	if err != nil || ref == "" || password == "" {
		t.Fatalf("FLEET_REF, FLEET_DB_PASSWORD and FLEET_REPLICA_PORT must be set: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	lcfg, err := config.Load(leaderConf)
	if err != nil {
		t.Fatal(err)
	}
	fcfg, err := config.Load(followerConf)
	if err != nil {
		t.Fatal(err)
	}
	kb, err := os.ReadFile(fcfg.KeyPath)
	if err != nil {
		t.Fatal(err)
	}
	sec, err := secrets.Load(kb) // the follower holds the cluster's master key
	if err != nil {
		t.Fatal(err)
	}

	// The follower's registry is the standby, opened the way a standby's daemon must.
	freg, err := registry.OpenReadOnly(ctx, lifecycle.RegistryDSN(fcfg))
	if err != nil {
		t.Fatalf("the standby's registry: %v", err)
	}
	t.Cleanup(freg.Close) // after the cleanups registered below, which still use the registries
	var inRecovery bool
	if err := freg.Pool().QueryRow(ctx, `select pg_is_in_recovery()`).Scan(&inRecovery); err != nil || !inRecovery {
		t.Fatalf("the follower's system cluster is not a standby: %v, %v", inRecovery, err)
	}
	lreg, err := registry.Open(ctx, lifecycle.RegistryDSN(lcfg))
	if err != nil {
		t.Fatalf("the leader's registry: %v", err)
	}
	t.Cleanup(lreg.Close)

	sup := units.NewExec(fcfg, log)
	arts, err := artifacts.New(fcfg, artifacts.WithLogger(log))
	if err != nil {
		t.Fatal(err)
	}
	// No Deps.Follower: the fleet reads from the system cluster that it is a follower.
	deps := fleet.Deps{Cfg: fcfg, Log: log, Registry: freg, Secrets: sec, Supervisor: sup, Artifacts: arts,
		Skip: []string{config.SvcStudio}, ReadyTimeout: 4 * time.Minute, Start: true}
	mgr, err := fleet.NewManager(deps)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := mgr.Stop(context.Background()); err != nil {
			t.Errorf("stop the follower's services: %v", err)
		}
	})
	ffleet, err := fleet.Setup(ctx, deps)
	if err != nil {
		t.Fatalf("the follower's shared services: %v", err)
	}

	t.Log("profile: Supavisor runs without bin/prepare, the other services are rendered and stopped")
	script := func(cfg *config.Config, svc string) string {
		b, err := os.ReadFile(units.FilesFor(cfg, units.Spec{Service: svc}).Run)
		if err != nil {
			t.Fatalf("%s is not rendered on %s: %v", svc, cfg.StateDir, err)
		}
		return string(b)
	}
	if !strings.Contains(script(lcfg, config.SvcSupavisor), "prepare") {
		t.Fatal("control: the leader's Supavisor has no bin/prepare in its launcher, so the check below could not tell")
	}
	for _, svc := range []string{config.SvcSupavisor, config.SvcRealtime} {
		if s := script(fcfg, svc); strings.Contains(s, "prepare") {
			t.Errorf("the follower's %s still runs bin/prepare:\n%s", svc, s)
		}
	}
	for _, svc := range []string{config.SvcPGMeta, config.SvcRealtime, config.SvcStorage} {
		script(fcfg, svc) // rendered
		st, err := sup.Status(ctx, "supavise-"+svc+".service")
		if err == nil && st.State == units.StateActive {
			t.Errorf("%s runs on the follower", svc)
		}
		// The port is free for the mesh's forwarder.
		l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(fleet.Port(fcfg, svc))))
		if err != nil {
			t.Errorf("%s: its port is taken on the follower: %v", svc, err)
			continue
		}
		l.Close()
	}
	hs := mgr.Status(ctx)
	if !fleet.AllHealthy(hs) {
		t.Errorf("the follower's services are not healthy: %+v", hs)
	}

	t.Log("tenant: the replicated tenant row of the project is served by the follower's Supavisor")
	primaryPort := lcfg.PortsFor(ref, mustProject(t, lreg, ref).Seq).Postgres
	login := func(port int, user string) (string, error) {
		u := url.URL{Scheme: "postgres", User: url.UserPassword(user, password), Host: net.JoinHostPort("127.0.0.1", strconv.Itoa(port)),
			Path: "/postgres", RawQuery: "sslmode=require&connect_timeout=10&default_query_exec_mode=simple_protocol"}
		c, err := pgx.Connect(ctx, u.String())
		if err != nil {
			return "", err
		}
		defer c.Close(context.WithoutCancel(ctx))
		var out string
		err = c.QueryRow(ctx, `select pg_is_in_recovery()::text || '|' || inet_server_port()::text`).Scan(&out)
		return out, err
	}
	eventually := func(what, want string, port int, user string) {
		t.Helper()
		var got string
		var err error
		for deadline := time.Now().Add(90 * time.Second); time.Now().Before(deadline); time.Sleep(time.Second) {
			if got, err = login(port, user); err == nil && got == want {
				t.Logf("%s: %s on port %d answers %s (in recovery|server port)", what, user, port, got)
				return
			}
		}
		t.Fatalf("%s: logged in as %s on port %d: %q, %v; want %q", what, user, port, got, err, want)
	}
	for _, port := range []int{fcfg.Ports.SupavisorSession, fcfg.Ports.SupavisorTransaction} {
		eventually("the project's own tenant", fmt.Sprintf("false|%d", primaryPort), port, "postgres."+ref)
	}

	t.Log("a follower leaves the tenant calls to the leader")
	spec, err := fleet.LoadTenantSpec(ctx, fleet.Deps{Cfg: lcfg, Registry: lreg, Secrets: sec}, ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := ffleet.EnsureTenant(ctx, spec); err != nil {
		t.Errorf("EnsureTenant on a follower: %v", err)
	}
	if err := ffleet.RemoveTenant(ctx, ref); err != nil {
		t.Errorf("RemoveTenant on a follower: %v", err)
	}
	eventually("the tenant after the follower's no-op calls", fmt.Sprintf("false|%d", primaryPort), fcfg.Ports.SupavisorTransaction, "postgres."+ref)

	t.Log("replica tenant: the leader writes it, the follower refreshes after replay")
	lfleet, err := fleet.Setup(ctx, fleet.Deps{Cfg: lcfg, Log: log, Registry: lreg, Secrets: sec})
	if err != nil {
		t.Fatal(err)
	}
	id := registry.ReplicaIdentifier(ref, "us-east-1", "abc123")
	t.Cleanup(func() {
		if err := lfleet.RemoveReplicaTenant(context.Background(), id); err != nil {
			t.Errorf("remove the replica tenant: %v", err)
		}
	})
	leaderPos, followerReplay := fleet.PGReplay{Pool: lreg.Pool()}, fleet.PGReplay{Pool: freg.Pool()}
	refresh := fleet.PeerRefresh{Replay: followerReplay, Refresh: ffleet, Wait: 30 * time.Second}
	// publish writes the replica tenant on the leader, waits for the follower's standby to replay it,
	// calls between (it sees the follower's Supavisor before the refresh), and refreshes.
	publish := func(port int, between func()) {
		t.Helper()
		spec.ReplicaID, spec.DBPort = id, port
		if err := lfleet.EnsureReplicaTenant(ctx, spec); err != nil {
			t.Fatal(err)
		}
		lsn, err := leaderPos.CurrentLSN(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(50 * time.Millisecond) {
			if past, err := followerReplay.ReplayedPast(ctx, lsn); err != nil || past {
				if err != nil {
					t.Fatal(err)
				}
				break
			} else if time.Now().After(deadline) {
				t.Fatalf("the follower's standby did not replay %s", lsn)
			}
		}
		if between != nil {
			between()
		}
		if done, err := refresh.Do(ctx, id, lsn); err != nil || !done {
			t.Fatalf("refresh of %s after replay of %s: %v, %v", id, lsn, done, err)
		}
	}
	publish(primaryPort, nil) // a tenant that points at the primary: a login lands there
	eventually("the replica tenant on its first port", fmt.Sprintf("false|%d", primaryPort), fcfg.Ports.SupavisorTransaction, "postgres."+id)
	publish(replicaPort, func() {
		// The row on the standby already names the replica; the follower's Supavisor may still pool to the
		// old target until it is told (spike S1). What it answers here is recorded, not required.
		got, err := login(fcfg.Ports.SupavisorTransaction, "postgres."+id)
		t.Logf("after replay, before the refresh: %q, %v", got, err)
	})
	for _, port := range []int{fcfg.Ports.SupavisorSession, fcfg.Ports.SupavisorTransaction} {
		eventually("the replica tenant after the refresh", fmt.Sprintf("true|%d", replicaPort), port, "postgres."+id)
	}
	// The project's own tenant still goes to the primary.
	eventually("the project's tenant next to the replica's", fmt.Sprintf("false|%d", primaryPort), fcfg.Ports.SupavisorTransaction, "postgres."+ref)
	if want := lcfg.ReplicaPorts(ref, mustProject(t, lreg, ref).Seq).Postgres; want != replicaPort {
		t.Errorf("the script started the replica on %d; the configuration puts it on %d", replicaPort, want)
	}

	t.Log("remote MCP: the follower's edge gates /mcp against its standby and forwards it to Studio's port")
	followerMCP(t, log, fcfg, freg, sec, ref, os.Getenv("FLEET_PAT"))
}

// noManager is the lifecycle.Manager of an API server that never manages a project: the follower's
// gate reads the registry and nothing else. Any call of the Manager panics on the nil interface.
type noManager struct{ lifecycle.Manager }

// followerMCP checks the remote MCP endpoint on a follower. The
// follower's edge answers api.<domain>/mcp itself, with the Management API's gate reading the
// follower's registry, which is the standby: a read-only database that a write to would log an
// error. A personal access token created on the leader is good there once the standby has replayed
// it; nothing is written (the script looks at the standby's log afterwards). What the gate lets
// through goes to Studio's port, which on a follower is the mesh's forwarder to the leader's Studio
// (mesh.KindStudio) and here, where the mesh is not running, a stand-in that records the request.
func followerMCP(t *testing.T, log *slog.Logger, fcfg *config.Config, freg *registry.Postgres, sec secrets.Secrets, ref, pat string) {
	t.Helper()
	if pat == "" {
		t.Fatal("FLEET_PAT must be set to a personal access token of the leader")
	}
	var mu sync.Mutex
	var seen []string
	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(fcfg.Ports.Studio)))
	if err != nil {
		t.Fatalf("Studio's port on the follower is not free for the forwarder: %v", err)
	}
	studio := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery+" auth="+strconv.FormatBool(r.Header.Get("Authorization") == "Bearer "+pat))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	studio.Listener.Close()
	studio.Listener = l
	studio.Start()
	t.Cleanup(studio.Close)

	apiH, err := api.NewServer(api.Deps{Registry: freg, Secrets: sec, Manager: noManager{}, Config: fcfg, Logger: log.With("component", "api")})
	if err != nil {
		t.Fatalf("the Management API on the standby: %v", err)
	}
	edge, err := proxy.New(proxy.Options{
		Config: fcfg, Registry: freg, Keys: proxy.RegistryKeys{Registry: freg, Secrets: sec},
		APIHandler: apiH, MCPGate: apiH.MCPGate, Logger: log.With("component", "proxy"),
		Cluster: &proxy.Cluster{Leader: func() bool { return false }},
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(edge.Handler())
	t.Cleanup(ts.Close)
	call := func(token string) (*http.Response, string) {
		t.Helper()
		req, err := http.NewRequest("POST", ts.URL+"/mcp?project_ref="+ref+"&access_token=dropped", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Host = fcfg.APIHost()
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, strings.TrimSpace(string(b))
	}

	resp, body := call("")
	if want := `Bearer resource_metadata="` + fcfg.APIURL() + `/.well-known/oauth-protected-resource/mcp"`; resp.StatusCode != 401 ||
		resp.Header.Get("WWW-Authenticate") != want || body != `{"message":"No access token provided"}` {
		t.Errorf("no token: %d %q %q, want the discovery challenge %q", resp.StatusCode, body, resp.Header.Get("WWW-Authenticate"), want)
	}
	if resp, _ := call("sbp_" + strings.Repeat("0", 40)); resp.StatusCode != 401 || !strings.Contains(resp.Header.Get("WWW-Authenticate"), `error="invalid_token"`) {
		t.Errorf("an unknown token: %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}
	mu.Lock()
	n := len(seen)
	mu.Unlock()
	if n != 0 {
		t.Fatalf("Studio's port received %v before a good token", seen)
	}

	resp, body = call(pat)
	if resp.StatusCode != 200 || !strings.Contains(body, `"result"`) {
		t.Fatalf("the leader's personal access token on the follower: %d %q (the standby may not have it; is the token older than the base backup?)", resp.StatusCode, body)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := "POST /api/mcp?project_ref=" + ref + " auth=true"; len(seen) != 1 || seen[0] != want {
		t.Errorf("Studio's port saw %q, want [%q]", seen, want)
	}
}

func mustProject(t *testing.T, reg registry.Registry, ref string) *registry.Project {
	t.Helper()
	p, err := reg.GetProject(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
