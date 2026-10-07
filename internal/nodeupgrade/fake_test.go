package nodeupgrade

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

var t0 = time.Date(2026, 10, 12, 20, 0, 0, 0, time.UTC)

const (
	authOld = "auth-v2.100.0-r1"
	authNew = "auth-v2.195.0-r1"
	restOld = "postgrest-v12.0-r0"
	restNew = "postgrest-v16.4-r0"
	pgOld   = "postgres-17.1.0-r1"
	pgNew   = "postgres-17.11.0.004-r1"
	rtOld   = "realtime-v2.100.0-r0"
	rtNew   = "realtime-v2.140.10-r0"
	stOld   = "storage-v1.70.0-r0"
	stNew   = "storage-v1.79.36-r0"
)

// The migrations a release knows: v1 is what the node has, v2 adds one (a newer release's).
var (
	migsV1 = []string{"0001_init.sql", "1100_project_upgrades.sql", "1190_custom_domains.sql"}
	migsV2 = []string{"0001_init.sql", "1100_project_upgrades.sql", "1190_custom_domains.sql", "1200_next.sql"}
)

func oldPins() map[string]string {
	return map[string]string{
		"postgres": pgOld, "gotrue": authOld, "postgrest": restOld,
		"pgmeta": "pgmeta-v0.100.0-r0", "supavisor": "pooler-v2.9.13-r1", "realtime": rtOld, "storage": stOld, "studio": "2026.10.01-sha-aaaaaaa",
	}
}

func newPins() map[string]string {
	p := oldPins()
	p["gotrue"], p["postgrest"], p["realtime"], p["storage"] = authNew, restNew, rtNew, stNew
	p["studio"] = "2026.10.05-sha-94b8b06"
	return p
}

// testNode is a healthy node with three projects: a, b active on the old releases and c paused.
func testNode() *Node {
	old := func() map[string]string {
		return map[string]string{"gotrue": authOld, "postgrest": restOld, "postgres": pgOld}
	}
	return &Node{
		Version: "v1.0.0", Pins: oldPins(), AppliedMigrations: migsV1, Platform: "linux-amd64",
		Verdict: VerdictHealthy, Summary: "healthy: 2 projects answering, 1 paused",
		Escrow: Escrow{Known: true, Covered: true}, DiskFree: 100 << 30, DiskPath: "/var/lib/supavise",
		Projects: []Project{
			{Ref: "system", Name: "system", Status: "ACTIVE_HEALTHY", Versions: map[string]string{}, LastBackup: t0.Add(-2 * time.Hour)},
			{Ref: "aaaaaaaaaaaaaaaaaaaa", Name: "a", Status: "ACTIVE_HEALTHY", Versions: old(), LastBackup: t0.Add(-3 * time.Hour)},
			{Ref: "bbbbbbbbbbbbbbbbbbbb", Name: "b", Status: "ACTIVE_HEALTHY", Versions: old(), LastBackup: t0.Add(-5 * time.Hour)},
			{Ref: "cccccccccccccccccccc", Name: "c", Status: "INACTIVE", Versions: old()},
		},
	}
}

func newInfo() *Info {
	return &Info{Version: "v1.1.0", Platform: "linux-amd64", Pins: newPins(), RegistrySchema: "1190_custom_domains.sql", RegistryMigrations: migsV1}
}

// fakeHost records what Run asks of the machine, in order, and fails the calls it is told to.
type fakeHost struct {
	mu    sync.Mutex
	calls []string
	node  *Node
	info  *Info
	out   *bytes.Buffer

	tag           string // the release Resolve finds; empty: info.Version
	resolveErr    error
	stageErr      error
	prefetchErr   error
	backupErr     error
	installErr    error
	installSwaps  bool // Install replaced the binary before it failed
	sharedErr     error
	sharedErrOnce bool // fail WaitShared once (the way forward), not on the way back
	projectsErr   error
	moves         []ProjectMove
	verdicts      []string // successive answers of Status; the last repeats
	statusErr     error
	applied       []string
	appliedErr    error
	revertErr     error
	restoreErr    error
	prev, cur     *Record
	since         []ProjectMove
	confirm       bool
	confirmErr    error
	marks         []string
	restored      bool // after Restore the node reports the verdict it had before
}

func newFakeHost() *fakeHost {
	return &fakeHost{node: testNode(), info: newInfo(), out: &bytes.Buffer{}, applied: migsV1, verdicts: []string{VerdictHealthy}, confirm: true}
}

func (f *fakeHost) rec(format string, a ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf(format, a...))
}

// order returns the calls joined, for comparisons of sequence.
func (f *fakeHost) order() string { return strings.Join(f.calls, " | ") }

func (f *fakeHost) has(prefix string) bool {
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func (f *fakeHost) Inspect(context.Context) (*Node, error) {
	f.rec("inspect")
	n := *f.node
	return &n, nil
}

func (f *fakeHost) Resolve(_ context.Context, tag string, _ *Node) (*Candidate, error) {
	f.rec("resolve %s", tag)
	if f.resolveErr != nil {
		return nil, f.resolveErr
	}
	t := f.tag
	if t == "" {
		t = f.info.Version
	}
	return &Candidate{Tag: t}, nil
}

func (f *fakeHost) Stage(context.Context, *Candidate) (*Staged, error) {
	f.rec("stage")
	if f.stageErr != nil {
		return nil, f.stageErr
	}
	return &Staged{Info: f.info}, nil
}

func (f *fakeHost) Discard(*Staged) { f.rec("discard") }

func (f *fakeHost) Prefetch(context.Context, *Staged, *Node, *Plan) error {
	f.rec("prefetch")
	return f.prefetchErr
}

func (f *fakeHost) Backup(_ context.Context, refs []string, parallel int) error {
	f.rec("backup %s x%d", strings.Join(refs, ","), parallel)
	return f.backupErr
}

func (f *fakeHost) Mark(phase, detail string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.marks = append(f.marks, phase)
}

func (f *fakeHost) Install(_ context.Context, _ *Staged, prev, next Record) (bool, error) {
	f.rec("install %s->%s migrations %d/%s", prev.Version, next.Version, len(prev.Migrations), prev.MigrationsFrom)
	if f.installErr != nil {
		return f.installSwaps, f.installErr
	}
	return true, nil
}

func (f *fakeHost) WaitShared(_ context.Context, moves []ServiceMove) error {
	names := make([]string, len(moves))
	for i, m := range moves {
		names[i] = m.Service
	}
	f.rec("wait %s", strings.Join(names, ","))
	if f.sharedErr != nil {
		err := f.sharedErr
		if f.sharedErrOnce {
			f.sharedErr = nil
		}
		return err
	}
	return nil
}

func (f *fakeHost) UpgradeProjects(_ context.Context, target map[string]string, _ time.Time) ([]ProjectMove, error) {
	f.rec("projects gotrue=%s postgres=%s", short("gotrue", target["gotrue"]), short("postgres", target["postgres"]))
	return f.moves, f.projectsErr
}

func (f *fakeHost) Status(context.Context) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "status")
	if f.statusErr != nil {
		return "", "", f.statusErr
	}
	if f.restored {
		return f.node.Verdict, f.node.Verdict + " (summary)", nil
	}
	v := f.verdicts[0]
	if len(f.verdicts) > 1 {
		f.verdicts = f.verdicts[1:]
	}
	return v, v + " (summary)", nil
}

func (f *fakeHost) Cleanup(_ context.Context, keep int, current string) {
	f.rec("cleanup keep=%d current=%s", keep, current)
}

func (f *fakeHost) AppliedMigrations(context.Context) ([]string, error) {
	return f.applied, f.appliedErr
}

func (f *fakeHost) PreviousRelease(context.Context, string) (*Record, *Record, error) {
	return f.prev, f.cur, nil
}

func (f *fakeHost) MovesSince(context.Context, time.Time) ([]ProjectMove, error) {
	return f.since, nil
}

func (f *fakeHost) RevertProjects(_ context.Context, moves []ProjectMove) error {
	refs := make([]string, len(moves))
	for i, m := range moves {
		refs[i] = m.Ref[:1]
	}
	f.rec("revert %s", strings.Join(refs, ","))
	return f.revertErr
}

func (f *fakeHost) Restore(_ context.Context, from string, rec Record) error {
	f.rec("restore %s (from %s)", rec.Version, from)
	f.mu.Lock()
	f.restored = f.restoreErr == nil
	f.mu.Unlock()
	return f.restoreErr
}

func (f *fakeHost) Confirm(q string) (bool, error) {
	f.rec("confirm")
	return f.confirm, f.confirmErr
}

var errBoom = errors.New("boom")
