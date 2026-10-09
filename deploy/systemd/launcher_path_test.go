package systemd_test

import (
	"strings"
	"testing"

	"github.com/supavise/supavise/deploy/systemd"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/units"
)

// supavise-postgres@<ref> starts only while projects/<ref>/postgres.run exists, and a fence removes
// that file so that systemd does not start the cluster again, at boot or after a restart. The unit
// file names the path with %i; the code that removes the file (units.FilesFor) computes it from the
// configuration. They have to name the same file for the default state directory, or a fence would
// remove a file the unit never looked at.
func TestPostgresUnitConditionNamesTheLauncherTheFenceRemoves(t *testing.T) {
	b, err := systemd.Read("supavise-postgres@.service")
	if err != nil {
		t.Fatal(err)
	}
	var cond, exec string
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "ConditionPathExists="); ok {
			cond = v
		}
		if v, ok := strings.CutPrefix(l, "ExecStart="); ok {
			exec = v
		}
	}
	if cond == "" {
		t.Fatal("the unit has no ConditionPathExists: a fence that removes postgres.run would not keep the cluster from starting")
	}
	cfg := config.Default()
	for _, ref := range []string{"system", "abcdefghijklmnopqrst"} {
		run := units.FilesFor(cfg, units.Spec{Service: config.SvcPostgres, Ref: ref}).Run
		if got := strings.ReplaceAll(cond, "%i", ref); got != run {
			t.Errorf("ref %s: the unit's ConditionPathExists is %s, the fence removes %s", ref, got, run)
		}
		if got := strings.ReplaceAll(exec, "%i", ref); got != run {
			t.Errorf("ref %s: the unit runs %s, the fence removes %s", ref, got, run)
		}
	}
}

// The daemon runs under ProtectSystem=strict. In a cluster it renews its node certificate in the cluster
// directory, and a retirement (a removed node, a rejoin) deletes the identity there and the cluster settings
// in config.d. Without those two directories among its ReadWritePaths the daemon of a joined server cannot
// renew its certificate and cannot retire, and nothing says so until the certificate expires. The leading "-"
// leaves out a directory that does not exist yet, which a single server never has.
func TestDaemonUnitCanWriteWhatAClusterNodeKeeps(t *testing.T) {
	b, err := systemd.Read("supavise.service")
	if err != nil {
		t.Fatal(err)
	}
	var strict bool
	writable := map[string]bool{}
	for _, l := range strings.Split(string(b), "\n") {
		if l == "ProtectSystem=strict" {
			strict = true
		}
		if v, ok := strings.CutPrefix(l, "ReadWritePaths="); ok {
			for _, p := range strings.Fields(v) {
				writable[strings.TrimPrefix(p, "-")] = true
			}
		}
	}
	if !strict {
		t.Fatal("the daemon unit no longer sets ProtectSystem=strict: this test's premise changed")
	}
	cfg := config.Default()
	for what, dir := range map[string]string{
		"the state directory":    cfg.StateDir,
		"the node's certificate": config.ClusterDir(config.DefaultPath),
		"the cluster settings":   config.ConfigDDir(config.DefaultPath),
	} {
		if !writable[dir] {
			t.Errorf("%s (%s) is not among the daemon's ReadWritePaths %v", what, dir, writable)
		}
	}
}
