package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/nodeupgrade"
)

func TestUpgradeCommandsAreRegistered(t *testing.T) {
	for _, path := range [][]string{{"upgrade"}, {"rollback"}, {"release-info"}} {
		c, _, err := rootCmd.Find(path)
		if err != nil || c == nil || c.Name() != path[0] {
			t.Errorf("%v is not a command: %v", path, err)
		}
	}
	up, _, _ := rootCmd.Find([]string{"upgrade"})
	for _, flag := range []string{"check", "plan", "yes", "unattended", "version", "include-postgres"} {
		if up.Flags().Lookup(flag) == nil {
			t.Errorf("upgrade has no --%s", flag)
		}
	}
	if f := up.Flags().Lookup("api-base"); f == nil || !f.Hidden {
		t.Error("--api-base should exist and be hidden (tests only)")
	}
}

// Exit statuses are the contract: a refusal is 2, whatever makes it, and main turns the Failure
// into the process's status.
func TestUpgradeRefusesOutsideALinuxServerWithStatus2(t *testing.T) {
	for _, args := range [][]string{{"upgrade", "--check"}, {"upgrade", "--yes"}, {"rollback", "--yes"}} {
		_, err := runRoot(t, args...)
		if err == nil || nodeupgrade.ExitCode(err) != nodeupgrade.ExitRefused {
			t.Errorf("%v: err = %v (exit %d), want a refusal with status 2", args, err, nodeupgrade.ExitCode(err))
		}
	}
}

func TestReleaseInfoReportsThePins(t *testing.T) {
	out, err := runRoot(t, "release-info", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var i nodeupgrade.Info
	if err := json.Unmarshal([]byte(out), &i); err != nil {
		t.Fatalf("%v in %q", err, out)
	}
	if i.RegistrySchema == "" || i.Pins["gotrue"] == "" || i.Pins["postgres"] == "" || i.Pins["studio"] == "" || i.Pins["supavisor"] == "" {
		t.Fatalf("info = %+v", i)
	}
	if _, err := nodeupgrade.ParseInfo([]byte(out)); err != nil {
		t.Fatalf("ParseInfo cannot read what release-info prints: %v", err)
	}
	if text, err := runRoot(t, "release-info"); err != nil || !strings.Contains(text, "auth-v") {
		t.Fatalf("text form: %q %v", text, err)
	}
}

func TestUpgradeProjectsArgs(t *testing.T) {
	target := map[string]string{"postgrest": "postgrest-v16.4-r0", "gotrue": "auth-v2.195.0-r1"}
	got := strings.Join(upgradeProjectsArgs(target, time.Time{}), " ")
	want := "projects upgrade --all --yes --no-gc --restart-changed --to gotrue=auth-v2.195.0-r1 --to postgrest=postgrest-v16.4-r0"
	if got != want {
		t.Fatalf("args = %q, want %q", got, want)
	}
	since := time.Date(2026, 10, 12, 20, 0, 0, 0, time.FixedZone("x", 3600))
	if got := strings.Join(upgradeProjectsArgs(target, since), " "); !strings.HasSuffix(got, "--reuse-backup-since 2026-10-12T19:00:00Z") {
		t.Fatalf("args with a backup time = %q", got)
	}
	// Without --include-postgres the target has no postgres, so a project keeps its PostgreSQL.
	if strings.Contains(got, "postgres=") {
		t.Fatal("PostgreSQL is in the rollout without being asked")
	}
}

func TestArtifactServicesAreWhatMoves(t *testing.T) {
	p := &nodeupgrade.Plan{
		System:        []nodeupgrade.ServiceMove{{Service: "gotrue"}},
		Shared:        []nodeupgrade.ServiceMove{{Service: "realtime"}, {Service: "studio"}},
		ProjectTarget: map[string]string{"gotrue": "x", "postgrest": "y"},
		Upgrade:       []string{"a"},
	}
	got := strings.Join(artifactServices(p), ",")
	if got != "gotrue,postgrest,realtime" {
		t.Fatalf("services = %s (Studio is fetched from its own URL)", got)
	}
	p.Upgrade = nil
	if got := strings.Join(artifactServices(p), ","); got != "gotrue,realtime" {
		t.Fatalf("with no project to upgrade: %s", got)
	}
}

func TestReleasesDirIsBesideTheBinaryAndOutOfTheStateDirectory(t *testing.T) {
	if got := releasesDir("/usr/local/bin/supavise"); got != "/usr/local/lib/supavise/releases" {
		t.Fatalf("releasesDir = %s", got)
	}
}

func TestPrefixWriterKeepsParallelLinesWhole(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	pw := &prefixWriter{w: &buf, mu: &mu}
	a, b := pw.with("a: "), pw.with("b: ")
	a.Write([]byte("one\ntw"))
	b.Write([]byte("x\n"))
	a.Write([]byte("o\n"))
	if got := buf.String(); got != "a: one\nb: x\na: two\n" {
		t.Fatalf("output = %q", got)
	}
}

func TestFailureReachesMainWithItsStatus(t *testing.T) {
	err := error(&nodeupgrade.Failure{Code: nodeupgrade.ExitRolledBack, Err: errors.New("x")})
	var f *nodeupgrade.Failure
	if !errors.As(err, &f) || f.Code != 3 {
		t.Fatal("not a Failure with status 3")
	}
}

func TestParseStatusReport(t *testing.T) {
	report := func(escrowState, escrowDetail string) []byte {
		b, _ := json.Marshal(map[string]any{"status": "healthy", "summary": "healthy: 2 projects answering",
			"components": []map[string]string{{"name": "disk", "state": "ok"}, {"name": "key escrow", "state": escrowState, "detail": escrowDetail}}})
		return b
	}
	v, s, e := parseStatusReport(report("ok", "an encrypted copy of this node's key is in the backup backend"))
	if v != "healthy" || !strings.HasPrefix(s, "healthy:") || !e.Known || !e.Covered {
		t.Fatalf("covered: %q %q %+v", v, s, e)
	}
	_, _, e = parseStatusReport(report("info", "the master key is not in the backups: run `supavise system escrow-key`"))
	if !e.Known || e.Covered {
		t.Fatalf("not covered: %+v", e)
	}
	_, _, e = parseStatusReport(report("info", "could not check the backup backend: timeout"))
	if e.Known || e.Covered {
		t.Fatalf("an unreachable backend is not an answer: %+v", e)
	}
	// "ok" with nothing looked at yet, a reworded report and a report without the component do not
	// show a copy; only an unattended upgrade needs the difference, and it needs the copy.
	for name, b := range map[string][]byte{
		"not checked yet": report("ok", "not checked yet"),
		"reworded":        report("warn", "no copy"),
		"missing":         []byte(`{"status":"healthy","summary":"healthy","components":[{"name":"disk","state":"ok"}]}`),
	} {
		if _, _, e := parseStatusReport(b); e.Covered {
			t.Errorf("%s counts as covered: %+v", name, e)
		}
	}
	if v, _, _ := parseStatusReport([]byte("not json")); v != nodeupgrade.VerdictUnknown {
		t.Fatalf("garbage reads as %q", v)
	}
	if v, _, _ := parseStatusReport([]byte(`{"summary":"x"}`)); v != nodeupgrade.VerdictUnknown {
		t.Fatalf("a report with no verdict reads as %q", v)
	}
}

// The artifacts of the binaries a rollback can go back to are not garbage, whether or not the
// daemon that ran them recorded its pins.
func TestKeptReleasePinsProtectTheArtifactsOfTheReleasesToRollBackTo(t *testing.T) {
	root := t.TempDir()
	cfg := config.Default()
	cfg.BinPath = filepath.Join(root, "bin", "supavise")
	rel := nodeupgrade.Releases{Dir: releasesDir(cfg.BinPath)}
	bin := filepath.Join(root, "b")
	if err := os.WriteFile(bin, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 12, 20, 0, 0, 0, time.UTC)
	for i, v := range []string{"v1.0.0", "v1.1.0", "v1.2.0", "v1.3.0"} {
		if _, err := rel.Keep(nodeupgrade.Record{Version: v, Pins: map[string]string{"gotrue": "auth-" + v}, InstalledAt: at.Add(time.Duration(i) * time.Hour)}, bin); err != nil {
			t.Fatal(err)
		}
	}
	if err := rel.Withdraw("v1.3.0"); err != nil { // rolled back from
		t.Fatal(err)
	}
	got := keptReleasePins(cfg, 2)
	if len(got) != 2 || got[0]["gotrue"] != "auth-v1.2.0" || got[1]["gotrue"] != "auth-v1.1.0" {
		t.Fatalf("kept pins = %v: the newest two that are not withdrawn", got)
	}
	if got := keptReleasePins(&config.Config{BinPath: filepath.Join(t.TempDir(), "bin", "supavise")}, 3); len(got) != 0 {
		t.Fatalf("a node that kept nothing: %v", got)
	}
}

// Whatever stops the command before it has read the node is a refusal, status 2.
func TestErrorsBeforeTheNodeIsReadAreRefusals(t *testing.T) {
	err := refusedBefore(errors.New("config: no such file"))
	if nodeupgrade.ExitCode(err) != nodeupgrade.ExitRefused {
		t.Fatalf("exit %d", nodeupgrade.ExitCode(err))
	}
	cfgPath := filepath.Join(t.TempDir(), "missing.toml")
	for _, args := range [][]string{{"upgrade", "--check", "--public-key-file", cfgPath}, {"upgrade", "--plan", "--public-key-file", cfgPath}} {
		if _, err := runRoot(t, args...); nodeupgrade.ExitCode(err) != nodeupgrade.ExitRefused {
			t.Errorf("%v: err = %v (exit %d)", args, err, nodeupgrade.ExitCode(err))
		}
	}
}

// A service that was already set to run the release the rollback puts back has nothing to
// restart (the daemon that died on its start never moved it); one that the swap moved does.
func TestMustHaveRestarted(t *testing.T) {
	swap := time.Date(2026, 10, 12, 20, 0, 0, 0, time.UTC)
	before, after := swap.Add(-time.Hour), swap.Add(time.Second)
	for _, tc := range []struct {
		name          string
		since         time.Time
		tagAtSwap, to string
		want          bool
	}{
		{"moved by the swap, not restarted yet", before, "old", "new", true},
		{"moved by the swap, restarted", after, "old", "new", false},
		{"never moved: nothing to restart", before, "old", "old", false},
		{"unknown unit file at the swap", before, "", "new", true},
	} {
		if got := mustHaveRestarted(tc.since, swap, tc.tagAtSwap, tc.to); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}
