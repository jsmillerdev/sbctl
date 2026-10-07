package diskquota

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/jsmillerdev/supavise/internal/config"
	"github.com/jsmillerdev/supavise/internal/units"
)

type fakeSup struct{ started []string }

func (f *fakeSup) Render(context.Context, units.Spec) error { return nil }
func (f *fakeSup) Start(_ context.Context, u string) error {
	f.started = append(f.started, u)
	return nil
}
func (f *fakeSup) Stop(context.Context, string) error { return nil }
func (f *fakeSup) Status(context.Context, string) (units.Status, error) {
	return units.Status{}, nil
}
func (f *fakeSup) Remove(context.Context, string) error { return nil }

func testManager(t *testing.T, fstype string, opts ...string) (*Manager, *fakeSup, string) {
	t.Helper()
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	ref := "abcdefghijklmnopqrst"
	if err := os.MkdirAll(cfg.Paths().Project(ref), 0o755); err != nil {
		t.Fatal(err)
	}
	sup := &fakeSup{}
	m := New(cfg, sup)
	m.InspectFn = func(string) (Volume, error) {
		return Volume{Mount: cfg.StateDir, FSType: fstype, Options: opts, TotalBytes: 100 << 30, FreeBytes: 50 << 30}, nil
	}
	return m, sup, ref
}

// An unprivileged daemon writes the wanted limit and starts the root unit, which applies it.
func TestSetStartsTheRootUnit(t *testing.T) {
	m, sup, ref := testManager(t, "xfs", "rw", "prjquota")
	m.Euid = func() int { return 1000 }
	if _, ok := m.Limit(ref); ok {
		t.Fatal("a limit before any was set")
	}
	if err := m.Set(context.Background(), ref, 7, 20); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(sup.started, ","); got != "supavise-diskquota@"+ref+".service" {
		t.Fatalf("started %q", got)
	}
	if gb, ok := m.Limit(ref); !ok || gb != 20 {
		t.Fatalf("limit = %d %v", gb, ok)
	}
	s, _, _ := ReadSetting(SettingFile(m.Cfg.Paths().Project(ref)))
	if s.ProjectID != ProjectID(7) {
		t.Fatalf("setting = %+v", s)
	}

	// What the unit runs: the stored setting goes to xfs_quota.
	var calls []string
	m.Run = func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return nil, nil
	}
	if err := m.ApplyStored(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || !strings.Contains(calls[1], "bhard=20g 100007") {
		t.Fatalf("xfs_quota calls = %q", calls)
	}
}

// Run as root (the exec backend of a development machine) there is no unit: xfs_quota runs at once.
func TestSetAsRootRunsXFSQuotaDirectly(t *testing.T) {
	m, sup, ref := testManager(t, "xfs", "prjquota")
	m.Euid = func() int { return 0 }
	var calls int
	m.Run = func(context.Context, string, ...string) ([]byte, error) { calls++; return nil, nil }
	if err := m.Set(context.Background(), ref, 1, 5); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(sup.started) != 0 {
		t.Fatalf("calls %d, units %v", calls, sup.started)
	}
}

func TestSetRefusesAVolumeThatCannotEnforce(t *testing.T) {
	for name, tc := range map[string]struct {
		fs   string
		opts []string
	}{"ext4": {"ext4", nil}, "xfs without quota": {"xfs", []string{"rw"}}, "xfs without enforcement": {"xfs", []string{"pqnoenforce"}}} {
		m, sup, ref := testManager(t, tc.fs, tc.opts...)
		m.Euid = func() int { return 1000 }
		if err := m.Set(context.Background(), ref, 1, 5); !errors.Is(err, ErrNotEnforceable) {
			t.Errorf("%s: err = %v", name, err)
		}
		if _, ok := m.Limit(ref); ok || len(sup.started) != 0 {
			t.Errorf("%s: a refused set left a limit or started a unit", name)
		}
	}
}

// The root unit trusts nothing it is handed: a link in place of a project directory, a ref that is
// no project, a project with no stored limit.
func TestApplyStoredChecksWhatItActsOn(t *testing.T) {
	m, _, ref := testManager(t, "xfs", "prjquota")
	m.Run = func(context.Context, string, ...string) ([]byte, error) { return nil, nil }
	if err := m.ApplyStored(context.Background(), ref); err == nil || !strings.Contains(err.Error(), "no disk limit") {
		t.Fatalf("no setting: %v", err)
	}
	if err := m.ApplyStored(context.Background(), "bbbbbbbbbbbbbbbbbbbb"); err == nil {
		t.Fatal("a project that does not exist")
	}
	other := t.TempDir()
	link := m.Cfg.Paths().Project("cccccccccccccccccccc")
	if err := os.Symlink(other, link); err != nil {
		t.Fatal(err)
	}
	if err := WriteSetting(SettingFile(other), Setting{ProjectID: ProjectID(2), SizeGB: 1}); err != nil {
		t.Fatal(err)
	}
	if err := m.ApplyStored(context.Background(), "cccccccccccccccccccc"); err == nil {
		t.Fatal("a symbolic link was followed")
	}
}
