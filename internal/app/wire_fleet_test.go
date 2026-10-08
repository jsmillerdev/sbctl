package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/awsapi"
	"github.com/supavise/supavise/internal/awsapi/awsfake"
	"github.com/supavise/supavise/internal/cluster"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/fleet"
	"github.com/supavise/supavise/internal/mesh"
	"github.com/supavise/supavise/internal/mesh/peerapi"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/units"
)

// fakeMesh answers peer API calls from a script and records them.
type fakeMesh struct {
	mu    sync.Mutex
	calls []fakeCall
	fail  map[string]error
}

type fakeCall struct {
	Node, Method, Path string
	In                 peerapi.RefreshRequest
}

func (f *fakeMesh) Call(_ context.Context, node, method, path string, in, out any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := fakeCall{Node: node, Method: method, Path: path}
	if r, ok := in.(peerapi.RefreshRequest); ok {
		c.In = r
	}
	f.calls = append(f.calls, c)
	if err := f.fail[node]; err != nil {
		return err
	}
	if r, ok := out.(*peerapi.RefreshResult); ok {
		r.Refreshed = true
	}
	return nil
}

func (f *fakeMesh) Dial(context.Context, string, mesh.Header) (net.Conn, error) {
	return nil, mesh.ErrNoSession
}
func (f *fakeMesh) Connected(string) bool            { return true }
func (f *fakeMesh) RTT(string) (time.Duration, bool) { return 0, false }
func (f *fakeMesh) Peers() []string                  { return nil }
func (f *fakeMesh) paths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		out = append(out, c.Node+" "+c.Method+" "+c.Path+" lsn="+c.In.LSN)
	}
	return out
}

type nullSupervisor struct{}

func (nullSupervisor) Render(context.Context, units.Spec) error { return nil }
func (nullSupervisor) Start(context.Context, string) error      { return nil }
func (nullSupervisor) Stop(context.Context, string) error       { return nil }
func (nullSupervisor) Remove(context.Context, string) error     { return nil }
func (nullSupervisor) Status(context.Context, string) (units.Status, error) {
	return units.Status{}, nil
}

func nodeRow(id string, state registry.NodeState) registry.Node {
	return registry.Node{ID: id, Name: "node-" + id, State: state}
}

func clusterOf(self string, role cluster.Role, leader string, nodes ...registry.Node) *cluster.Static {
	var me registry.Node
	for _, n := range nodes {
		if n.ID == self {
			me = n
		}
	}
	return cluster.NewStatic(cluster.Snapshot{Self: me, Nodes: nodes, Leader: leader, Epoch: 3, Role: role})
}

func TestWireFleetDoesNothingOnANodeWithoutACluster(t *testing.T) {
	w := testWire(t)
	if err := wireFleet(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	if len(w.runners) != 0 || len(w.stops) != 0 {
		t.Fatalf("started something: %d runners, %d stops", len(w.runners), len(w.stops))
	}
	if _, ok := Get[fleet.Fleet](w); ok {
		t.Error("a Fleet was provided")
	}
	if _, ok := Get[fleet.PeerRefresher](w); ok {
		t.Error("a PeerRefresher was provided")
	}
}

func TestWireFleetInACluster(t *testing.T) {
	w := testWire(t)
	mem := clusterOf("n2", cluster.RoleFollower, "n1", nodeRow("n1", registry.NodeActive), nodeRow("n2", registry.NodeActive))
	Provide[cluster.Membership](w, mem)
	Provide[mesh.Mesh](w, &fakeMesh{})
	w.Cfg.Supervisor = config.SupervisorSystemd
	w.Node.Supervisor = nullSupervisor{}
	if err := wireFleet(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	if f, ok := Get[fleet.Fleet](w); !ok || len(f) != 3 {
		t.Errorf("fleet = %v, %v", f, ok)
	}
	if len(w.runners) != 1 || w.runners[0].name != "fleet role" {
		t.Fatalf("runners = %+v", w.runners)
	}
	found := false
	for _, p := range mesh.DefaultMux.Patterns() {
		found = found || p == "POST "+peerapi.PathFleetRefresh
	}
	if !found {
		t.Errorf("the refresh endpoint is not registered: %v", mesh.DefaultMux.Patterns())
	}
	// A second daemon in the same process registers again without a panic.
	registerFleetRefresh(&fleetRefreshServer{mem: mem, log: slog.New(slog.NewTextHandler(io.Discard, nil))})
}

// ---- the refresh endpoint -----------------------------------------------------------------

type scriptedReplay struct {
	past bool
	err  error
	asks []string
}

func (s *scriptedReplay) ReplayedPast(_ context.Context, lsn string) (bool, error) {
	s.asks = append(s.asks, lsn)
	return s.past, s.err
}

type countingRefresher struct {
	got []string
	err error
}

func (c *countingRefresher) RefreshTenant(_ context.Context, id string) error {
	c.got = append(c.got, id)
	return c.err
}

const tenantRef = "abcdefghijklmnopqrst"

func refreshServer(t *testing.T, leader string, rp *scriptedReplay, rf *countingRefresher) http.Handler {
	t.Helper()
	mem := clusterOf("n2", cluster.RoleFollower, "n1", nodeRow("n1", registry.NodeActive), nodeRow("n2", registry.NodeActive))
	s := &fleetRefreshServer{mem: mem, log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		pr: fleet.PeerRefresh{Replay: rp, Refresh: rf, Wait: 20 * time.Millisecond, Poll: time.Millisecond}}
	if rp == nil {
		s.pr.Replay = nil
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+peerapi.PathFleetRefresh, s.ServeHTTP)
	return mux
}

func refreshCall(h http.Handler, peer, tenant, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", peerapi.FleetRefreshPath(tenant), strings.NewReader(body))
	req = req.WithContext(mesh.WithPeer(req.Context(), mesh.Peer{Node: peer}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func errCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var e peerapi.Error
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("%d %q: %v", rec.Code, rec.Body.String(), err)
	}
	return e.Code
}

func TestFleetRefreshAfterReplay(t *testing.T) {
	rp, rf := &scriptedReplay{past: true}, &countingRefresher{}
	h := refreshServer(t, "n1", rp, rf)
	rec := refreshCall(h, "n1", tenantRef, `{"lsn":"0/3000100"}`)
	var res peerapi.RefreshResult
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &res) != nil || !res.Refreshed {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if len(rp.asks) != 1 || rp.asks[0] != "0/3000100" || len(rf.got) != 1 || rf.got[0] != tenantRef {
		t.Errorf("replay asked %v; refreshed %v", rp.asks, rf.got)
	}
	// No body, no wait: the contract allows a caller that does not send a position.
	rp.asks, rf.got = nil, nil
	rec = refreshCall(h, "n1", tenantRef, "")
	if rec.Code != 200 || len(rp.asks) != 0 || len(rf.got) != 1 {
		t.Fatalf("%d %s; replay asked %v", rec.Code, rec.Body.String(), rp.asks)
	}
	rec = refreshCall(h, "n1", tenantRef+"-rr-eu-west-1-abc123", `{}`)
	if rec.Code != 200 || rf.got[len(rf.got)-1] != tenantRef+"-rr-eu-west-1-abc123" {
		t.Fatalf("a replica tenant: %d %s", rec.Code, rec.Body.String())
	}
}

func TestFleetRefreshIsTheLeadersToAsk(t *testing.T) {
	rf := &countingRefresher{}
	h := refreshServer(t, "n1", &scriptedReplay{past: true}, rf)
	for name, peer := range map[string]string{"another follower": "n3", "this node": "n2", "no certificate": ""} {
		rec := refreshCall(h, peer, tenantRef, "")
		if rec.Code != 403 || errCode(t, rec) != "not_leader" {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	if len(rf.got) != 0 {
		t.Fatalf("refreshed for a caller that is not the leader: %v", rf.got)
	}
}

func TestFleetRefreshAnswersWhyItDidNot(t *testing.T) {
	// Replay has not reached the position: 503 replay_behind, and the tenant is left alone.
	rf := &countingRefresher{}
	rec := refreshCall(refreshServer(t, "n1", &scriptedReplay{}, rf), "n1", tenantRef, `{"lsn":"0/FFFFFFF"}`)
	if rec.Code != 503 || errCode(t, rec) != "replay_behind" || len(rf.got) != 0 {
		t.Errorf("%d %s, refreshed %v", rec.Code, rec.Body.String(), rf.got)
	}
	// Input that is no tenant and no position is refused, and nothing is refreshed.
	rf = &countingRefresher{}
	h := refreshServer(t, "n1", &scriptedReplay{past: true}, rf)
	for name, tc := range map[string]struct{ tenant, body string }{
		"a system tenant":         {"system", ""},
		"a bad position":          {tenantRef, `{"lsn":"x"}`},
		"a body that is not json": {tenantRef, `lsn=0/1`},
	} {
		if rec := refreshCall(h, "n1", tc.tenant, tc.body); rec.Code != 400 || errCode(t, rec) != "bad_request" {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	if len(rf.got) != 0 {
		t.Errorf("refreshed %v for bad input", rf.got)
	}
	// Supavisor refuses to terminate: the leader hears of it.
	rec = refreshCall(refreshServer(t, "n1", &scriptedReplay{past: true}, &countingRefresher{err: errors.New("supavisor answered 500")}), "n1", tenantRef, "")
	if rec.Code != 502 || errCode(t, rec) != "refresh_failed" || !strings.Contains(rec.Body.String(), "supavisor answered 500") {
		t.Errorf("%d %s", rec.Code, rec.Body.String())
	}
	// A node that cannot read its replay position cannot honor a request that names one.
	h = refreshServer(t, "n1", nil, &countingRefresher{})
	if rec := refreshCall(h, "n1", tenantRef, `{"lsn":"0/1"}`); rec.Code != 503 {
		t.Errorf("without a replay: %d", rec.Code)
	}
}

// ---- the leader's side --------------------------------------------------------------------

func TestPeerRefresherAsksEveryOtherActiveNodeWithTheLeadersPosition(t *testing.T) {
	m := &fakeMesh{fail: map[string]error{}}
	mem := clusterOf("n1", cluster.RoleLeader, "n1",
		nodeRow("n1", registry.NodeActive), nodeRow("n2", registry.NodeActive), nodeRow("n3", registry.NodeActive),
		nodeRow("n4", registry.NodeJoining), nodeRow("n5", registry.NodeLeft))
	asked := 0
	p := &peerRefresher{mem: mem, rpc: m, log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		lsn: func(context.Context) (string, error) { asked++; return "0/5000000", nil }}
	if err := p.RefreshPeers(context.Background(), tenantRef); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(sortedStrings(m.paths()), "|")
	want := "n2 POST /peer/v1/fleet/refresh/" + tenantRef + " lsn=0/5000000|n3 POST /peer/v1/fleet/refresh/" + tenantRef + " lsn=0/5000000"
	if got != want || asked != 1 {
		t.Fatalf("calls = %s, position read %d times", got, asked)
	}
	// One node is down: the others are still asked, and the failure is the caller's to see.
	m.fail["n2"] = mesh.ErrNoSession
	m.calls = nil
	err := p.RefreshPeers(context.Background(), tenantRef)
	if !errors.Is(err, mesh.ErrNoSession) || !strings.Contains(err.Error(), "n2") || len(m.calls) != 2 {
		t.Fatalf("err = %v, %d calls", err, len(m.calls))
	}
	// A cluster of one asks nobody, and does not read the position.
	solo := clusterOf("n1", cluster.RoleLeader, "n1", nodeRow("n1", registry.NodeActive))
	asked = 0
	p = &peerRefresher{mem: solo, rpc: m, lsn: func(context.Context) (string, error) { asked++; return "", nil }}
	if err := p.RefreshPeers(context.Background(), tenantRef); err != nil || asked != 0 {
		t.Fatalf("err=%v asked=%d", err, asked)
	}
	// A position that cannot be read stops the refresh: it would be sent without one.
	p = &peerRefresher{mem: mem, rpc: m, lsn: func(context.Context) (string, error) { return "", errors.New("no pool") }}
	m.calls = nil
	if err := p.RefreshPeers(context.Background(), tenantRef); err == nil || len(m.calls) != 0 {
		t.Fatalf("err=%v calls=%d", err, len(m.calls))
	}
}

func sortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// ---- role changes -------------------------------------------------------------------------

type roleHarness struct {
	snaps chan cluster.Snapshot
	mu    sync.Mutex
	modes []fleet.Mode
	fail  int // applies that fail before one works
	done  chan struct{}
}

func (h *roleHarness) apply(_ context.Context, m fleet.Mode) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.fail > 0 {
		h.fail--
		return errors.New("realtime: address already in use")
	}
	h.modes = append(h.modes, m)
	select {
	case h.done <- struct{}{}:
	default:
	}
	return nil
}

func (h *roleHarness) applied() []fleet.Mode {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]fleet.Mode(nil), h.modes...)
}

func runRoles(t *testing.T, h *roleHarness) (stop func()) {
	t.Helper()
	h.snaps, h.done = make(chan cluster.Snapshot, 8), make(chan struct{}, 8)
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() {
		followRole(ctx, h.snaps, h.apply, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Millisecond)
		close(finished)
	}()
	return func() {
		cancel()
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Fatal("followRole did not return when its context ended")
		}
	}
}

func (h *roleHarness) waitApplied(t *testing.T, n int) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for len(h.applied()) < n {
		select {
		case <-h.done:
		case <-deadline:
			t.Fatalf("applied %v, want %d", h.applied(), n)
		}
	}
}

func snap(r cluster.Role) cluster.Snapshot { return cluster.Snapshot{Role: r} }

func TestFollowRoleLeavesTheBootRoleAlone(t *testing.T) {
	h := &roleHarness{}
	stop := runRoles(t, h)
	h.snaps <- snap(cluster.RoleFollower) // the role the daemon booted in
	h.snaps <- snap(cluster.RoleFollower) // a node row changed
	time.Sleep(30 * time.Millisecond)
	stop()
	if got := h.applied(); len(got) != 0 {
		t.Fatalf("applied %v at boot", got)
	}
}

func TestFollowRolePromotionDemotionAndFence(t *testing.T) {
	h := &roleHarness{}
	stop := runRoles(t, h)
	defer stop()
	h.snaps <- snap(cluster.RoleFollower)
	h.snaps <- snap(cluster.RoleLeader) // promotion
	h.waitApplied(t, 1)
	h.snaps <- snap(cluster.RoleLeader)   // same role again: nothing
	h.snaps <- snap(cluster.RoleFollower) // demotion
	h.waitApplied(t, 2)
	h.snaps <- snap(cluster.RoleFenced)
	h.waitApplied(t, 3)
	h.snaps <- snap(cluster.RoleFollower) // back from the fence
	h.waitApplied(t, 4)
	want := []fleet.Mode{fleet.ModeLeader, fleet.ModeFollower, fleet.ModeStopped, fleet.ModeFollower}
	got := h.applied()
	if len(got) != len(want) {
		t.Fatalf("applied %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("applied %v, want %v", got, want)
		}
	}
}

// The forwarder still holds a service's port when the node is promoted: the start fails, and is
// tried again until it works.
func TestFollowRoleRetriesAFailedApply(t *testing.T) {
	h := &roleHarness{fail: 2}
	stop := runRoles(t, h)
	defer stop()
	h.snaps <- snap(cluster.RoleFollower)
	h.snaps <- snap(cluster.RoleLeader)
	h.waitApplied(t, 1)
	if got := h.applied(); len(got) != 1 || got[0] != fleet.ModeLeader || h.fail != 0 {
		t.Fatalf("applied %v, failures left %d", got, h.fail)
	}
}

// A promotion that half-worked and was reverted before the retry still leaves services running
// that the follower must not run: the revert is applied, though the role is the booted one.
func TestFollowRoleAppliesTheRoleAgainAfterAFailureEvenIfItReverted(t *testing.T) {
	h := &roleHarness{fail: 1}
	h.snaps, h.done = make(chan cluster.Snapshot, 8), make(chan struct{}, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go followRole(ctx, h.snaps, h.apply, slog.New(slog.NewTextHandler(io.Discard, nil)), 50*time.Millisecond)
	h.snaps <- snap(cluster.RoleFollower)
	h.snaps <- snap(cluster.RoleLeader)   // the apply fails
	h.snaps <- snap(cluster.RoleFollower) // and the node is a follower again before the retry
	h.waitApplied(t, 1)
	if got := h.applied(); got[0] != fleet.ModeFollower {
		t.Fatalf("applied %v; the follower profile must be applied to undo the half-started leader", got)
	}
}

func TestFollowRoleEndsWithItsContextOrItsChannel(t *testing.T) {
	h := &roleHarness{}
	stop := runRoles(t, h)
	stop()
	h2 := &roleHarness{}
	h2.snaps, h2.done = make(chan cluster.Snapshot), make(chan struct{})
	finished := make(chan struct{})
	go func() {
		followRole(context.Background(), h2.snaps, h2.apply, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Millisecond)
		close(finished)
	}()
	close(h2.snaps)
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("followRole did not return when Watch closed")
	}
}

// ---- Storage credentials ------------------------------------------------------------------

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func credsWire(t *testing.T) (*Wire, *awsfake.Server) {
	t.Helper()
	w := testWire(t)
	w.Cfg.StateDir = t.TempDir()
	w.Cfg.Fleet.StorageS3RoleARN = "arn:aws:iam::123456789012:role/supavise-storage"
	w.Cfg.Fleet.StorageCredentialsPort = freePort(t)
	fake := awsfake.New(t)
	fake.AddRole(w.Cfg.Fleet.StorageS3RoleARN)
	old := newAWS
	newAWS = func() (*awsapi.Client, error) { return fake.Client(), nil }
	t.Cleanup(func() { newAWS = old })
	return w, fake
}

func TestWireServesStorageItsRoleOnLoopback(t *testing.T) {
	w, fake := credsWire(t)
	if err := wireFleet(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	if len(w.runners) != 1 || w.runners[0].name != "storage credentials" {
		t.Fatalf("runners = %+v", w.runners)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.runners[0].fn(ctx) }()
	token, err := fleet.StorageCredentialToken(w.Cfg)
	if err != nil {
		t.Fatal(err)
	}
	url := fleet.StorageCredentialsURL(w.Cfg)
	if want := "http://127.0.0.1:" + strconv.Itoa(w.Cfg.Fleet.StorageCredentialsPort) + "/credentials"; url != want {
		t.Fatalf("url = %s, want %s", url, want)
	}
	do := func(tok string) (int, string) {
		req, _ := http.NewRequest("GET", url, nil)
		if tok != "" {
			req.Header.Set("Authorization", tok)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := do(""); code != 403 || body != "" {
		t.Errorf("without the token: %d %q", code, body)
	}
	code, body := do(token)
	if code != 200 || !strings.Contains(body, `"AccessKeyId":"ASIAFAKEASSUMED`) || !strings.Contains(body, `"Token":"fake-assumed-token`) {
		t.Fatalf("%d %s", code, body)
	}
	if order := fake.Order("sts"); len(order) != 1 || order[0] != "sts:AssumeRole" {
		t.Errorf("sts calls = %v", order)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestWireStorageCredentialsDoNotStopTheDaemonWhenTheyCannotRun(t *testing.T) {
	// The port is taken.
	w, _ := credsWire(t)
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(w.Cfg.Fleet.StorageCredentialsPort))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if err := wireFleet(context.Background(), w); err != nil || len(w.runners) != 0 {
		t.Fatalf("err=%v runners=%d", err, len(w.runners))
	}
	// There is no AWS client.
	w, _ = credsWire(t)
	newAWS = func() (*awsapi.Client, error) { return nil, errors.New("no region") }
	if err := wireFleet(context.Background(), w); err != nil || len(w.runners) != 0 {
		t.Fatalf("err=%v runners=%d", err, len(w.runners))
	}
	// There is no role: nothing to serve.
	w, _ = credsWire(t)
	w.Cfg.Fleet.StorageS3RoleARN = ""
	if err := wireFleet(context.Background(), w); err != nil || len(w.runners) != 0 {
		t.Fatalf("err=%v runners=%d", err, len(w.runners))
	}
}
