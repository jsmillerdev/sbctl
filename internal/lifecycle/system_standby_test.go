package lifecycle

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/registry"
)

func systemStandbyPlan() SystemStandbyPlan {
	return SystemStandbyPlan{Identifier: registry.ReplicaIdentifier(config.SystemRef, "us-east-1", "abc123"), BackupID: "20261001T000000Z-ab12cd", ReplicationPassword: "leader-secret"}
}

// systemSeeder is a seeder that fills the standby's directory the way the backup service does.
func systemSeeder(plans *[]ReplicaSeedPlan, id string) ReplicaSeeder {
	return func(_ context.Context, plan ReplicaSeedPlan) error {
		*plans = append(*plans, plan)
		if err := os.MkdirAll(plan.DataDir, 0o700); err != nil {
			return err
		}
		for name, body := range map[string]string{"PG_VERSION": "17\n", "standby.signal": "",
			"postgresql.auto.conf": "primary_conninfo = 'host=127.0.0.1 port=5433 application_name=" + id + "'\n"} {
			if err := os.WriteFile(filepath.Join(plan.DataDir, name), []byte(body), 0o600); err != nil {
				return err
			}
		}
		return nil
	}
}

func TestSeedSystemStandbyBuildsAndStartsTheStandbyAndCanBeRepeated(t *testing.T) {
	ctx := context.Background()
	f := newReplicaFixture(t)
	plan := systemStandbyPlan()
	var plans []ReplicaSeedPlan
	seed := systemSeeder(&plans, plan.Identifier)

	if err := f.pl.SeedSystemStandby(ctx, plan, seed); err != nil {
		t.Fatal(err)
	}
	sys := &registry.Project{Ref: config.SystemRef}
	rp := f.pl.replicaPaths(sys)
	if len(plans) != 1 || plans[0].Ref != config.SystemRef || plans[0].Identifier != plan.Identifier || plans[0].DataDir != rp.Data ||
		plans[0].BackupID != plan.BackupID || plans[0].PrimaryPort != f.cfg.PortsFor(config.SystemRef, 0).Postgres || plans[0].ReplicationPassword != "leader-secret" {
		t.Fatalf("the seed plan = %+v", plans)
	}
	// The unit is the system cluster's own, rendered from the replica's spec, with a key file to start on.
	if ops := f.sup.ops(); !strings.Contains(ops, "start supavise-postgres@system.service") || strings.Contains(ops, "gotrue") {
		t.Fatalf("ops:\n%s", ops)
	}
	if fi, err := os.Stat(rp.RootKey); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("pgsodium root key: %v %v", fi, err)
	}

	// A resumed join runs it again: the standby is there, it only has to run.
	plans = nil
	f.sup.mu.Lock()
	f.sup.log = nil
	f.sup.mu.Unlock()
	if err := f.pl.SeedSystemStandby(ctx, plan, seed); err != nil {
		t.Fatal(err)
	}
	if len(plans) != 0 || !strings.Contains(f.sup.ops(), "start supavise-postgres@system.service") {
		t.Fatalf("a repeated seeding: plans %v, ops %s", plans, f.sup.ops())
	}
}

func TestSeedSystemStandbyFinishesWhatAnEarlierAttemptCutOffAndRefusesOtherData(t *testing.T) {
	ctx := context.Background()
	plan := systemStandbyPlan()
	sys := &registry.Project{Ref: config.SystemRef}

	// A seed that was cut off: its marker is there, and the partial copy goes.
	f := newReplicaFixture(t)
	rp := f.pl.replicaPaths(sys)
	if err := os.MkdirAll(rp.Data, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{SeedMarker, "backup_label", "PG_VERSION"} {
		if err := os.WriteFile(filepath.Join(rp.Data, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var plans []ReplicaSeedPlan
	if err := f.pl.SeedSystemStandby(ctx, plan, systemSeeder(&plans, plan.Identifier)); err != nil {
		t.Fatalf("over a seed that was cut off: %v", err)
	}
	if len(plans) != 1 || fileExists(filepath.Join(rp.Data, "backup_label")) || fileExists(filepath.Join(rp.Data, SeedMarker)) {
		t.Fatalf("plans %d; the partial copy stayed", len(plans))
	}

	// A cluster that is not this standby is not touched.
	for name, files := range map[string]map[string]string{
		"a primary":                   {"PG_VERSION": "17\n"},
		"the standby of another node": {"PG_VERSION": "17\n", "standby.signal": "", "postgresql.auto.conf": "primary_conninfo = 'host=x application_name=system-rr-eu-west-1-zzz999'\n"},
		"something else":              {"notes.txt": "mine"},
	} {
		g := newReplicaFixture(t)
		dir := g.pl.replicaPaths(sys).Data
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		for n, body := range files {
			if err := os.WriteFile(filepath.Join(dir, n), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		plans = nil
		err := g.pl.SeedSystemStandby(ctx, plan, systemSeeder(&plans, plan.Identifier))
		if !errors.Is(err, ErrClusterExists) || len(plans) != 0 || g.sup.ops() != "" {
			t.Errorf("%s: %v (seeds %d, ops %q)", name, err, len(plans), g.sup.ops())
		}
		for n := range files {
			if !fileExists(filepath.Join(dir, n)) {
				t.Errorf("%s: %s was removed", name, n)
			}
		}
	}

	// Not an identifier of the system cluster, and no seeder.
	h := newReplicaFixture(t)
	if err := h.pl.SeedSystemStandby(ctx, SystemStandbyPlan{Identifier: testReplicaID()}, systemSeeder(&plans, "")); err == nil {
		t.Fatal("the standby of a project was built as the system cluster's")
	}
	if err := h.pl.SeedSystemStandby(ctx, plan, nil); err == nil {
		t.Fatal("no seeder")
	}
}

func TestSystemStandbyPreflight(t *testing.T) {
	ctx := context.Background()
	f := newReplicaFixture(t)
	f.cfg.Backup.Backend = "s3://bucket/prefix"
	if err := f.pl.SystemStandbyPreflight(ctx); err != nil {
		t.Fatalf("preflight: %v", err)
	}
	f.cfg.Backup.Backend = "file:///var/lib/supavise/backups"
	if err := f.pl.SystemStandbyPreflight(ctx); err == nil || !strings.Contains(err.Error(), "file://") {
		t.Fatalf("a file:// backend = %v", err)
	}
}
