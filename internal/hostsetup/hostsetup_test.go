package hostsetup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/supavise/supavise/deploy/systemd"
)

// fakeRunner answers commands from a function and records them.
type fakeRunner struct {
	mu    sync.Mutex
	calls []string
	// envs holds the extra environment of each call, in the order of calls.
	envs [][]string
	do   func(name string, args []string) ([]byte, error)
}

func (f *fakeRunner) Run(_ context.Context, env []string, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, strings.TrimSpace(name+" "+strings.Join(args, " ")))
	f.envs = append(f.envs, env)
	f.mu.Unlock()
	if f.do == nil {
		return nil, nil
	}
	return f.do(name, args)
}

func (f *fakeRunner) ran(prefix string) int {
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// host is a temporary root: the directories converge works in, and the current user as the
// supavise account.
type host struct {
	state, config, units, polkit string
	runner                       *fakeRunner
	mountinfo                    string
	opts                         Options
}

func newHost(t *testing.T) *host {
	t.Helper()
	root := t.TempDir()
	h := &host{
		state: filepath.Join(root, "var/lib/supavise"), config: filepath.Join(root, "etc/supavise"),
		units: filepath.Join(root, "etc/systemd/system"), polkit: filepath.Join(root, "etc/polkit-1/rules.d"),
		runner: &fakeRunner{},
	}
	for _, d := range []string{h.state, h.config} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	h.opts = Options{
		StateDir: h.state, ConfigDir: h.config, UnitDir: h.units, PolkitDir: h.polkit,
		Owner:    func() (int, int, bool) { return os.Getuid(), os.Getgid(), true },
		PeerPort: 7443, Runner: h.runner, Geteuid: func() int { return 0 },
		LookPath:  func(string) (string, error) { return "/usr/sbin/ufw", nil },
		MountInfo: func() ([]byte, error) { return []byte(h.mountinfo), nil },
	}
	return h
}

func (h *host) converger(out *bytes.Buffer) *Converger {
	c := &Converger{Steps: DefaultSteps(h.opts), StateDir: h.state, Version: "v9.9.9",
		Now: func() time.Time { return time.Date(2026, 10, 12, 20, 0, 0, 0, time.UTC) }}
	if out != nil {
		c.Out = out
	}
	return c
}

func changed(rs []Result) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.Changed...)
	}
	return out
}

// A second converge changes nothing, and `--check` agrees before and after.
func TestConvergeIsIdempotent(t *testing.T) {
	h := newHost(t)
	ctx := context.Background()
	h.mountinfo = "36 35 8:1 / " + h.state + " rw - xfs /dev/nvme1n1 rw\n37 35 8:1 /etc " + h.config + " rw - xfs /dev/nvme1n1 rw\n"
	// ufw is active and does not admit 7443.
	h.runner.do = func(name string, args []string) ([]byte, error) {
		if name == "ufw" && len(args) > 0 && args[0] == "status" {
			return []byte("Status: active\n\nTo   Action   From\n22/tcp  ALLOW  Anywhere\n"), nil
		}
		return nil, nil
	}

	c := h.converger(nil)
	before := c.Check(ctx)
	pending := map[string]bool{}
	for _, r := range before {
		pending[r.ID] = r.Pending
	}
	for _, id := range []string{"units", "directories", "mounts", "ufw", "marker"} {
		if !pending[id] {
			t.Errorf("before the first run %q is not pending: %+v", id, before)
		}
	}

	var out bytes.Buffer
	res, err := h.converger(&out).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(changed(res)) == 0 {
		t.Fatal("the first run changed nothing")
	}
	for _, want := range []string{"installed " + filepath.Join(h.units, "supavise.service"), "created " + filepath.Join(h.config, "cluster"),
		"created " + filepath.Join(h.config, "config.d"), "wrote " + filepath.Join(h.units, "supavise.service.d", "10-supavise-data-mount.conf"),
		"allowed 7443/tcp in ufw", "recorded converge revision 1"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	if m, err := ReadMarker(h.state); err != nil || m.Revision != Revision || m.Version != "v9.9.9" {
		t.Fatalf("marker = %+v, %v", m, err)
	}
	if h.runner.ran("ufw allow 7443/tcp") != 1 {
		t.Errorf("ufw calls: %v", h.runner.calls)
	}

	// ufw now lists the rule.
	h.runner.do = func(name string, args []string) ([]byte, error) {
		if name == "ufw" && len(args) > 0 && args[0] == "status" {
			return []byte("Status: active\n\n7443/tcp  ALLOW  Anywhere\n7443/tcp (v6)  ALLOW  Anywhere (v6)\n"), nil
		}
		return nil, nil
	}
	for _, r := range h.converger(nil).Check(ctx) {
		if r.Pending {
			t.Errorf("pending after a converge: %+v", r)
		}
	}
	out.Reset()
	res, err = h.converger(&out).Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if c := changed(res); len(c) != 0 {
		t.Errorf("a second run changed %v", c)
	}
	if !strings.Contains(out.String(), "units are up to date") || strings.Contains(out.String(), "recorded") {
		t.Errorf("second run output:\n%s", out.String())
	}
}

// The directories keep their owner and mode; a loose mode is put right.
func TestDirectoriesStep(t *testing.T) {
	h := newHost(t)
	ctx := context.Background()
	dir := filepath.Join(h.config, "cluster")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	s := directoriesStep(h.opts)
	p, err := s.Check(ctx)
	if err != nil || !p.Pending || !strings.Contains(p.Detail, "mode 0755") || !strings.Contains(p.Detail, "config.d is missing") {
		t.Fatalf("check = %+v, %v", p, err)
	}
	if _, err := s.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"cluster", "config.d"} {
		fi, err := os.Stat(filepath.Join(h.config, d))
		if err != nil || fi.Mode().Perm() != 0o750 {
			t.Errorf("%s: %v %v", d, fi, err)
		}
	}
	// Content of the directories is not touched.
	f := filepath.Join(h.config, "config.d", "10-cluster.toml")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if o, err := s.Apply(ctx); err != nil || len(o.Changed) != 0 {
		t.Fatalf("second apply: %+v, %v", o, err)
	}
	if fi, _ := os.Stat(f); fi.Mode().Perm() != 0o600 {
		t.Errorf("a file inside was changed: %v", fi.Mode())
	}

	// No supavise user yet (a fresh host before the installer made it): nothing to do, not an error.
	h.opts.Owner = func() (int, int, bool) { return 0, 0, false }
	p, err = directoriesStep(h.opts).Check(ctx)
	if err != nil || p.Pending || !strings.Contains(p.Detail, "does not exist") {
		t.Errorf("without the user: %+v, %v", p, err)
	}
}

func TestMountsStepWritesDropInsForEveryServiceUnit(t *testing.T) {
	h := newHost(t)
	ctx := context.Background()

	// No mount: nothing to protect.
	h.mountinfo = "20 1 8:1 / / rw - ext4 /dev/root rw\n"
	p, err := mountsStep(h.opts).Check(ctx)
	if err != nil || p.Pending || !strings.Contains(p.Detail, "is a mount") {
		t.Fatalf("no mounts: %+v, %v", p, err)
	}

	// The data volume and the bind mount of the config directory.
	h.mountinfo = "20 1 8:1 / / rw - ext4 /dev/root rw\n36 20 8:2 / " + h.state + " rw - xfs /dev/nvme1n1 rw\n37 20 8:2 /etc " + h.config + " rw - xfs /dev/nvme1n1 rw\n"
	s := mountsStep(h.opts)
	if p, _ := s.Check(ctx); !p.Pending {
		t.Fatalf("with mounts: %+v", p)
	}
	o, err := s.Apply(ctx)
	if err != nil || !o.Reload {
		t.Fatalf("apply: %+v, %v", o, err)
	}
	data, etc := mountUnits()
	if len(data) < 10 {
		t.Fatalf("units = %v", data)
	}
	for _, u := range data {
		b, err := os.ReadFile(filepath.Join(h.units, u+".d", dataMountDropIn))
		if err != nil || string(b) != "[Unit]\nRequiresMountsFor="+h.state+"\n" {
			t.Errorf("%s data drop-in = %q, %v", u, b, err)
		}
	}
	for _, u := range etc {
		b, err := os.ReadFile(filepath.Join(h.units, u+".d", etcMountDropIn))
		if err != nil || string(b) != "[Unit]\nRequiresMountsFor="+h.config+"\n" {
			t.Errorf("%s etc drop-in = %q, %v", u, b, err)
		}
	}
	// The templates the CloudFormation first-boot script covered are covered, and so are the others;
	// the upgrade service keeps its state on the root disk.
	for _, u := range []string{"supavise.service", "supavise-postgres@.service", "supavise-gotrue@.service", "supavise-basebackup@.service"} {
		if _, err := os.Stat(filepath.Join(h.units, u+".d", dataMountDropIn)); err != nil {
			t.Errorf("%s has no data drop-in: %v", u, err)
		}
	}
	if _, err := os.Stat(filepath.Join(h.units, "supavise-upgrade.service.d", dataMountDropIn)); err == nil {
		t.Error("supavise-upgrade.service got a data-mount drop-in")
	}
	if _, err := os.Stat(filepath.Join(h.units, "supavise-upgrade.service.d", etcMountDropIn)); err != nil {
		t.Error("supavise-upgrade.service has no config-mount drop-in")
	}
	// Timers and the slice get none.
	if _, err := os.Stat(filepath.Join(h.units, "supavise-upgrade.timer.d")); err == nil {
		t.Error("a timer got a drop-in")
	}
	if o, _ := s.Apply(ctx); len(o.Changed) != 0 {
		t.Errorf("second apply changed %v", o.Changed)
	}
}

// A drop-in the CloudFormation first-boot script wrote is the same file: converge changes nothing.
func TestMountsStepAcceptsTheDropInsOfTheCloudInit(t *testing.T) {
	h := newHost(t)
	h.mountinfo = "36 20 8:2 / " + h.state + " rw - xfs /dev/nvme1n1 rw\n"
	for _, u := range []string{"supavise.service", "supavise-postgres@.service"} {
		d := filepath.Join(h.units, u+".d")
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, dataMountDropIn), []byte("[Unit]\nRequiresMountsFor="+h.state+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	o, err := mountsStep(h.opts).Apply(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range o.Files {
		if strings.Contains(f, "supavise.service.d") || strings.Contains(f, "supavise-postgres@.service.d") {
			t.Errorf("rewrote %s", f)
		}
	}
}

func TestOnOwnMount(t *testing.T) {
	mounts := parseMountPoints([]byte("1 0 8:1 / / rw - ext4 /dev/a rw\n2 1 8:2 / /mnt/with\\040space rw - xfs /dev/b rw\n3 1 8:3 / /var/lib rw - xfs /dev/c rw\n"))
	for dir, want := range map[string]bool{
		"/var/lib/supavise": true, "/var/lib": true, "/var/libx": false, "/etc/supavise": false, "/mnt/with space/data": true, "/": false,
	} {
		if got := onOwnMount(dir, mounts); got != want {
			t.Errorf("onOwnMount(%q) = %v, want %v (mounts %q)", dir, got, want, mounts)
		}
	}
}

func TestUFW(t *testing.T) {
	ctx := context.Background()
	status := func(s string) *fakeRunner {
		return &fakeRunner{do: func(name string, args []string) ([]byte, error) { return []byte(s), nil }}
	}
	for _, c := range []struct {
		name, status string
		pending      bool
	}{
		{"inactive", "Status: inactive\n", false},
		{"active, no rule", "Status: active\n\n22/tcp ALLOW Anywhere\n", true},
		{"active, rule", "Status: active\n\n7443/tcp ALLOW Anywhere\n", false},
		{"active, rule without a protocol", "Status: active\n\n7443 ALLOW Anywhere\n", false},
		{"active, rule narrowed to a source", "Status: active\n\n7443/tcp ALLOW IN 10.0.0.0/8\n", false},
		{"active, denied", "Status: active\n\n7443/tcp DENY Anywhere\n", true},
		{"another port", "Status: active\n\n74430/tcp ALLOW Anywhere\n", true},
	} {
		h := newHost(t)
		r := status(c.status)
		h.opts.Runner = r
		p, err := ufwStep(h.opts).Check(ctx)
		if err != nil || p.Pending != c.pending {
			t.Errorf("%s: %+v, %v", c.name, p, err)
		}
		o, err := ufwStep(h.opts).Apply(ctx)
		if err != nil || (len(o.Changed) == 1) != c.pending || (r.ran("ufw allow 7443/tcp") == 1) != c.pending {
			t.Errorf("%s: apply %+v, %v, calls %v", c.name, o, err, r.calls)
		}
	}

	// ufw translates "Status: active"; the rules are read in English whatever the locale is.
	h0 := newHost(t)
	r0 := &fakeRunner{do: func(string, []string) ([]byte, error) { return []byte("Status: active\n"), nil }}
	h0.opts.Runner = r0
	if _, err := ufwStep(h0.opts).Check(ctx); err != nil {
		t.Fatal(err)
	}
	if len(r0.calls) != 1 || r0.calls[0] != "ufw status" || !slices.Equal(r0.envs[0], []string{"LC_ALL=C"}) {
		t.Errorf("ufw status was run as %v with the environment %v", r0.calls, r0.envs)
	}

	// No ufw, or no mesh port: nothing.
	h := newHost(t)
	h.opts.LookPath = func(string) (string, error) { return "", errors.New("not found") }
	if p, _ := ufwStep(h.opts).Check(ctx); p.Pending || !strings.Contains(p.Detail, "not installed") {
		t.Errorf("no ufw: %+v", p)
	}
	h = newHost(t)
	h.opts.PeerPort = 0
	if p, _ := ufwStep(h.opts).Check(ctx); p.Pending {
		t.Errorf("no port: %+v", p)
	}

	// Without root a check cannot read the rules and says so; it does not call the node pending.
	h = newHost(t)
	h.opts.Geteuid = func() int { return 1000 }
	h.opts.Runner = &fakeRunner{do: func(string, []string) ([]byte, error) { return nil, errors.New("you need to be root") }}
	if p, err := ufwStep(h.opts).Check(ctx); err != nil || p.Pending || !strings.Contains(p.Detail, "without root") {
		t.Errorf("as a user: %+v, %v", p, err)
	}
	// As root the same failure is no reason to fail the host: the check says so, and the apply warns
	// and opens nothing.
	h.opts.Geteuid = func() int { return 0 }
	r := h.opts.Runner.(*fakeRunner)
	if p, err := ufwStep(h.opts).Check(ctx); err != nil || p.Pending || !strings.Contains(p.Detail, "ufw status failed") {
		t.Errorf("ufw fails for root: %+v, %v", p, err)
	}
	o, err := ufwStep(h.opts).Apply(ctx)
	if err != nil || len(o.Changed) != 0 || len(o.Warnings) != 1 || !strings.Contains(o.Warnings[0], "sudo ufw allow 7443/tcp") || r.ran("ufw allow 7443/tcp") != 0 {
		t.Errorf("ufw fails for root: %+v, %v, calls %v", o, err, r.calls)
	}
	// A rule that ufw refuses is a failure.
	h.opts.Runner = &fakeRunner{do: func(name string, args []string) ([]byte, error) {
		if len(args) > 0 && args[0] == "allow" {
			return nil, errors.New("ERROR: problem running ufw-init")
		}
		return []byte("Status: active\n\nTo   Action   From\n22/tcp   ALLOW   Anywhere\n"), nil
	}}
	if _, err := ufwStep(h.opts).Apply(ctx); err == nil {
		t.Error("a ufw that refuses the rule is not an error")
	}
}

// A declared package step installs what dpkg does not report installed, once.
func TestPackageStepWithAFakeRunner(t *testing.T) {
	ctx := context.Background()
	installed := map[string]bool{"curl": true}
	r := &fakeRunner{do: func(name string, args []string) ([]byte, error) {
		switch name {
		case "dpkg-query":
			if installed[args[len(args)-1]] {
				return []byte("install ok installed"), nil
			}
			return nil, &ExitError{Command: "dpkg-query", Code: 1, Err: errors.New("exit status 1")}
		case "apt-get":
			if i := slices.Index(args, "install"); i >= 0 {
				for _, a := range args[i+1:] {
					if !strings.HasPrefix(a, "-") {
						installed[a] = true
					}
				}
			}
		}
		return nil, nil
	}}
	h := newHost(t)
	h.opts.Runner = r
	h.opts.Packages = []PackageSpec{{ID: "packages-xfs", Title: "Install the XFS tools", Names: []string{"curl", "xfsprogs"}}}
	steps := DefaultSteps(h.opts)
	var s Step
	for _, st := range steps {
		if st.ID() == "packages-xfs" {
			s = st
		}
	}
	if s == nil {
		t.Fatalf("no package step in %d steps", len(steps))
	}
	if p, _ := s.Check(ctx); !p.Pending || !strings.Contains(p.Detail, "xfsprogs") || strings.Contains(p.Detail, "curl") {
		t.Fatalf("check = %+v", p)
	}
	o, err := s.Apply(ctx)
	if err != nil || len(o.Changed) != 1 || o.Changed[0] != "installed xfsprogs" {
		t.Fatalf("apply = %+v, %v", o, err)
	}
	// apt waits for the dpkg lock that unattended-upgrades may hold, for the refresh and the install.
	if r.ran("apt-get -o DPkg::Lock::Timeout=300 install -y -qq xfsprogs") != 1 || r.ran("apt-get -o DPkg::Lock::Timeout=300 update -qq") != 1 {
		t.Errorf("calls: %v", r.calls)
	}
	n := r.ran("apt-get -o DPkg::Lock::Timeout=300 install")
	if o, err := s.Apply(ctx); err != nil || len(o.Changed) != 0 || r.ran("apt-get -o DPkg::Lock::Timeout=300 install") != n {
		t.Errorf("second apply: %+v, %v, calls %v", o, err, r.calls)
	}
	// This release declares none.
	for _, st := range DefaultSteps(Options{}) {
		if strings.HasPrefix(st.ID(), "packages") {
			t.Errorf("the release declares the package step %s", st.ID())
		}
	}
}

type failStep struct{ id string }

func (s failStep) ID() string                             { return s.id }
func (s failStep) Title() string                          { return "Fail " + s.id }
func (s failStep) NeedsRoot() bool                        { return true }
func (s failStep) Check(context.Context) (Pending, error) { return Pending{Pending: true}, nil }
func (s failStep) Apply(context.Context) (Outcome, error) {
	return Outcome{Changed: []string{"half done"}}, errors.New("boom")
}

type okStep struct {
	id      string
	applied *[]string
}

func (s okStep) ID() string      { return s.id }
func (s okStep) Title() string   { return "Ok " + s.id }
func (s okStep) NeedsRoot() bool { return false }
func (s okStep) Check(context.Context) (Pending, error) {
	return Pending{Pending: true, Detail: "d"}, nil
}
func (s okStep) Apply(context.Context) (Outcome, error) {
	*s.applied = append(*s.applied, s.id)
	return Outcome{Changed: []string{"did " + s.id}, Reload: true}, nil
}

// A failing step does not stop the others, and the marker is not written; the next run writes it.
func TestFailedStepLeavesNoMarker(t *testing.T) {
	dir := t.TempDir()
	var applied []string
	var activated []Outcome
	c := &Converger{Steps: []Step{okStep{"a", &applied}, failStep{"b"}, okStep{"c", &applied}}, StateDir: dir,
		Activate: func(_ context.Context, o Outcome) error { activated = append(activated, o); return nil }}
	res, err := c.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Fail b: boom") {
		t.Fatalf("err = %v", err)
	}
	if strings.Join(applied, "") != "ac" {
		t.Errorf("applied %v: a step after the failure must still run", applied)
	}
	if len(activated) != 1 || !activated[0].Reload || len(activated[0].Changed) != 3 {
		t.Errorf("activate got %+v", activated)
	}
	if m, _ := ReadMarker(dir); m.Revision != 0 {
		t.Errorf("marker written despite the failure: %+v", m)
	}
	last := res[len(res)-1]
	if last.ID != MarkerID || !last.Pending || last.Detail != "revision 0 of 1" {
		t.Errorf("marker result = %+v", last)
	}
	if res[1].Error != "boom" {
		t.Errorf("result of the failed step = %+v", res[1])
	}

	c.Steps = []Step{okStep{"a", &applied}}
	if _, err := c.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m, _ := ReadMarker(dir); m.Revision != Revision {
		t.Errorf("marker after a good run = %+v", m)
	}
}

// A run that is told not to record (a test directory took the unit files only) changes the steps
// and leaves the node's marker where it was.
func TestNoMarkerLeavesTheMarkerAlone(t *testing.T) {
	dir := t.TempDir()
	var applied []string
	c := &Converger{Steps: []Step{okStep{"a", &applied}}, StateDir: dir, NoMarker: true}
	res, err := c.Run(context.Background())
	if err != nil || strings.Join(applied, "") != "a" {
		t.Fatalf("%v, applied %v", err, applied)
	}
	if _, err := os.Stat(MarkerPath(dir)); err == nil {
		t.Error("the marker was written")
	}
	if last := res[len(res)-1]; last.ID != MarkerID || !last.Pending || len(last.Changed) != 0 {
		t.Errorf("marker result = %+v", last)
	}
}

func TestCheckReportsAStepThatCannotBeChecked(t *testing.T) {
	c := &Converger{StateDir: t.TempDir(), Steps: []Step{&funcStep{id: "x", title: "X", check: func(context.Context) (Pending, error) {
		return Pending{}, errors.New("no way to tell")
	}}}}
	rs := c.Check(context.Background())
	if !rs[0].Pending || rs[0].Detail != "no way to tell" || !AnyPending(rs) {
		t.Errorf("results = %+v", rs)
	}
}

func TestMarker(t *testing.T) {
	dir := t.TempDir()
	if m, err := ReadMarker(dir); err != nil || m.Revision != 0 {
		t.Fatalf("no marker: %+v, %v", m, err)
	}
	at := time.Date(2026, 10, 12, 20, 0, 0, 0, time.UTC)
	if err := WriteMarker(dir, Marker{Revision: 3, Version: "v1.2.0", At: at}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(MarkerPath(dir))
	if string(b) != "revision=3\nversion=v1.2.0\nat=2026-10-12T20:00:00Z\n" {
		t.Errorf("marker file = %q", b)
	}
	if fi, _ := os.Stat(MarkerPath(dir)); fi.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v", fi.Mode())
	}
	if m, err := ReadMarker(dir); err != nil || m.Revision != 3 || m.Version != "v1.2.0" || !m.At.Equal(at) {
		t.Errorf("read back: %+v, %v", m, err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("a temporary file was left: %v", entries)
	}
	// A bare number is a revision; nonsense is an error.
	for body, want := range map[string]int{"2\n": 2, "# c\nrevision = 4\n": 4} {
		if m, err := parseMarker([]byte(body)); err != nil || m.Revision != want {
			t.Errorf("%q: %+v, %v", body, m, err)
		}
	}
	for _, body := range []string{"", "revision=x\n", "version=v1\n", "revision=-1\n"} {
		if _, err := parseMarker([]byte(body)); err == nil {
			t.Errorf("%q parsed", body)
		}
	}
	// Behind: only when the marker is known to be.
	if s := StatusOf(dir); !s.Known || s.Have != 3 || s.Want != Revision || s.Behind() {
		t.Errorf("status = %+v", s)
	}
	if err := os.WriteFile(MarkerPath(dir), []byte("revision=0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !StatusOf(dir).Behind() {
		t.Error("revision 0 is not behind")
	}
	if err := os.WriteFile(MarkerPath(dir), []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	if s := StatusOf(dir); s.Known || s.Behind() || s.Err == nil {
		t.Errorf("unreadable marker: %+v", s)
	}
}

type noSync struct{}

func (noSync) Sync(context.Context, bool) ([]string, error) { return nil, nil }

type scriptedSync struct {
	files []string
	err   error
	asked []bool // dryRun of each call
}

func (s *scriptedSync) Sync(_ context.Context, dryRun bool) ([]string, error) {
	s.asked = append(s.asked, dryRun)
	return s.files, s.err
}

// The cluster-settings step reports what it would write and writes it; a leader that cannot be
// reached leaves the settings as they are and is a warning, never a failure of the host.
func TestConfigSyncStep(t *testing.T) {
	ctx := context.Background()
	sync := &scriptedSync{files: []string{"/etc/supavise/config.d/10-cluster.toml"}}
	step := configSyncStep(Options{ConfigSync: sync})
	if p, err := step.Check(ctx); err != nil || !p.Pending || !strings.Contains(p.Detail, "10-cluster.toml") {
		t.Errorf("check: %+v, %v", p, err)
	}
	o, err := step.Apply(ctx)
	if err != nil || len(o.Changed) != 1 || o.Changed[0] != "wrote /etc/supavise/config.d/10-cluster.toml" || len(o.Files) != 1 {
		t.Errorf("apply: %+v, %v", o, err)
	}
	if len(sync.asked) != 2 || !sync.asked[0] || sync.asked[1] {
		t.Errorf("the check must be a dry run and the apply a write: %v", sync.asked)
	}

	down := &scriptedSync{err: errors.New("cannot reach the leader n1")}
	step = configSyncStep(Options{ConfigSync: down})
	if p, err := step.Check(ctx); err != nil || p.Pending || !p.Unknown || !strings.Contains(p.Detail, "cannot reach the leader n1") {
		t.Errorf("check with the leader down: %+v, %v (want not pending, but not known to be in order either)", p, err)
	}
	o, err = step.Apply(ctx)
	if err != nil || len(o.Changed) != 0 || len(o.Warnings) != 1 || !strings.Contains(o.Warnings[0], "were not refreshed") {
		t.Errorf("apply with the leader down: %+v, %v", o, err)
	}

	// Run counts such a step as done: the converge succeeds and records its revision.
	h := newHost(t)
	h.opts.ConfigSync = down
	c := &Converger{Steps: DefaultSteps(h.opts), StateDir: h.state, Out: &bytes.Buffer{}}
	if _, err := c.Run(ctx); err != nil {
		t.Errorf("a converge failed on a leader that could not be reached: %v", err)
	}
}

// blockedSync waits for the context, as a leader that accepts the connection and never answers does.
type blockedSync struct{}

func (blockedSync) Sync(ctx context.Context, _ bool) ([]string, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// A check does not wait for a leader that is down: status and `converge --check` run it. The result
// says the step was not checked, and the converge report carries that to a person and to JSON.
func TestConfigSyncCheckGivesUpOnALeaderThatDoesNotAnswer(t *testing.T) {
	old := configCheckTimeout
	configCheckTimeout = 50 * time.Millisecond
	defer func() { configCheckTimeout = old }()

	h := newHost(t)
	h.opts.ConfigSync = blockedSync{}
	c := &Converger{Steps: []Step{configSyncStep(h.opts)}, StateDir: h.state, Out: &bytes.Buffer{}}
	start := time.Now()
	rs := c.Check(context.Background())
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("the check waited %s", took)
	}
	if len(rs) < 1 || rs[0].ID != "config-d" || rs[0].Pending || !rs[0].Unknown || !strings.Contains(rs[0].Detail, "deadline exceeded") {
		t.Fatalf("results: %+v", rs)
	}
	b, err := json.Marshal(rs[0])
	if err != nil || !strings.Contains(string(b), `"unknown":true`) {
		t.Errorf("json: %s %v", b, err)
	}
	// A step that was checked says nothing about it.
	ok := Result{ID: "x", Title: "x"}
	if b, _ := json.Marshal(ok); strings.Contains(string(b), "unknown") {
		t.Errorf("json of a checked step: %s", b)
	}
}

// The titles are what the release calls its host changes: stable, one per step, no duplicates. The
// cluster-settings step is in the step list of a node that has a syncer, and not in the release's list.
func TestTitles(t *testing.T) {
	titles := Titles()
	want := []string{titleUnits, titleDirectories, titleMounts, titleUFW}
	if strings.Join(titles, "|") != strings.Join(want, "|") {
		t.Errorf("titles = %q", titles)
	}
	seen := map[string]bool{}
	steps := DefaultSteps(Options{ConfigSync: noSync{}})
	for _, s := range steps {
		if seen[s.ID()] {
			t.Errorf("duplicate step id %s", s.ID())
		}
		seen[s.ID()] = true
	}
	if last := steps[len(steps)-1]; last.Title() != titleConfigD {
		t.Errorf("a node with a syncer ends its steps with %q, not %q", titleConfigD, last.Title())
	}
}

// Every embedded service unit is one converge knows about, so a new unit file cannot miss its
// mount protection.
func TestMountUnitsCoverTheEmbeddedServices(t *testing.T) {
	data, etc := mountUnits()
	in := func(list []string, n string) bool {
		for _, x := range list {
			if x == n {
				return true
			}
		}
		return false
	}
	for _, n := range systemd.Names() {
		if !strings.HasSuffix(n, ".service") {
			continue
		}
		if !in(etc, n) {
			t.Errorf("%s has no config-mount drop-in", n)
		}
		if n != "supavise-upgrade.service" && !in(data, n) {
			t.Errorf("%s has no data-mount drop-in", n)
		}
	}
}

func TestMonitor(t *testing.T) {
	dir := t.TempDir()
	type seen struct {
		behind bool
		have   int
	}
	var got []seen
	var mu sync.Mutex
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	quiet := false
	m := &Monitor{StateDir: dir, Delay: time.Millisecond, Every: 5 * time.Millisecond, Quiet: func() bool { mu.Lock(); defer mu.Unlock(); return quiet },
		Raise: func(_ context.Context, s Status) {
			mu.Lock()
			got = append(got, seen{s.Behind(), s.Have})
			mu.Unlock()
		}}
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	waitFor := func(what string, f func() bool) {
		t.Helper()
		for i := 0; i < 400; i++ {
			mu.Lock()
			ok := f()
			mu.Unlock()
			if ok {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s; saw %v", what, got)
	}
	waitFor("the first look at an unconverged node", func() bool { return len(got) >= 2 && got[0].behind })
	if err := WriteMarker(dir, Marker{Revision: Revision}); err != nil {
		t.Fatal(err)
	}
	waitFor("the node to catch up", func() bool { return !got[len(got)-1].behind })
	mu.Lock()
	n := len(got)
	mu.Unlock()
	time.Sleep(60 * time.Millisecond)
	mu.Lock()
	if len(got) != n {
		t.Errorf("Raise kept being called for a node that is converged: %v", got)
	}
	// While an upgrade runs the node is not judged.
	quiet = true
	mu.Unlock()
	if err := os.WriteFile(MarkerPath(dir), []byte("revision=0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond)
	mu.Lock()
	if len(got) != n {
		t.Errorf("the monitor judged the node during an upgrade: %v", got)
	}
	mu.Unlock()
	cancel()
	<-done
	_ = fmt.Sprint(got)
}
