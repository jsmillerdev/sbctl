package hostsetup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/supavise/supavise/deploy/systemd"
)

// Titles of the steps. They are what a release's host_changes lists and what the upgrade plan
// prints, so they say what changes on the host in a sentence.
const (
	titleUnits       = "Install the systemd units and the polkit rule"
	titleDirectories = "Create the cluster and config.d directories in /etc/supavise"
	titleMounts      = "Make the units wait for the data and config directories when they are mounts"
	titleUFW         = "Open the mesh port in ufw when ufw is active"
	titleConfigD     = "Refresh the cluster settings in config.d from the leader"
)

// Options say where the host's files are and how to reach the rest of it. NewOptions' callers set
// the paths; everything else has a default that works on a Linux server.
type Options struct {
	// StateDir is the state directory (/var/lib/supavise) and ConfigDir the directory of
	// config.toml (/etc/supavise).
	StateDir, ConfigDir string
	// UnitDir is where systemd reads unit files (/etc/systemd/system); PolkitDir is where polkit
	// reads rules (/etc/polkit-1/rules.d), "" to leave the rule out.
	UnitDir, PolkitDir string
	// UnitOverrides replace the embedded content of a unit file, by file name: the timers that the
	// node's [backup] and [update] settings render.
	UnitOverrides map[string][]byte

	// Owner gives the ids of the supavise user; ok is false when the user does not exist yet.
	Owner func() (uid, gid int, ok bool)
	// PeerPort is the TCP port of the mesh (7443), which ufw is told to admit; zero leaves ufw alone.
	PeerPort int
	// Packages are the packages the release declares; nil means DeclaredPackages.
	Packages []PackageSpec
	// ConfigSync refreshes config.d/10-cluster.toml from the leader; nil on a node that is not in
	// a cluster, which leaves the step out.
	ConfigSync ConfigSyncer

	Runner    Runner
	LookPath  func(string) (string, error)
	Geteuid   func() int
	MountInfo func() ([]byte, error)
}

func (o *Options) runner() Runner {
	if o.Runner != nil {
		return o.Runner
	}
	return ExecRunner{}
}

func (o *Options) lookPath(name string) (string, error) {
	if o.LookPath != nil {
		return o.LookPath(name)
	}
	return exec.LookPath(name)
}

func (o *Options) geteuid() int {
	if o.Geteuid != nil {
		return o.Geteuid()
	}
	return os.Geteuid()
}

func (o *Options) mountInfo() ([]byte, error) {
	if o.MountInfo != nil {
		return o.MountInfo()
	}
	return os.ReadFile("/proc/self/mountinfo")
}

// DefaultSteps is the step list of this release, in the order they run: the unit files first
// (the other steps may name units), then directories, mount protection, the firewall, packages
// and the cluster settings.
func DefaultSteps(o Options) []Step {
	steps := []Step{unitsStep(o), directoriesStep(o), mountsStep(o), ufwStep(o)}
	pkgs := o.Packages
	if pkgs == nil {
		pkgs = DeclaredPackages
	}
	for _, p := range pkgs {
		steps = append(steps, packageStep(o, p))
	}
	if o.ConfigSync != nil {
		steps = append(steps, configSyncStep(o))
	}
	return steps
}

// Titles are the titles of the steps of this release, in order: the release's host_changes. The
// cluster-settings step is not among them: a node has it only when it is in a cluster, which a
// release cannot know, and a single server's plan must not list a step it never runs.
func Titles() []string {
	var out []string
	for _, s := range DefaultSteps(Options{}) {
		out = append(out, s.Title())
	}
	return out
}

// ---- unit files ---------------------------------------------------------------------------

func unitsStep(o Options) Step {
	return &funcStep{
		id: "units", title: titleUnits, root: true, same: "units are up to date",
		check: func(context.Context) (Pending, error) {
			files, err := systemd.Pending(o.UnitDir, o.PolkitDir, o.UnitOverrides)
			if err != nil {
				return Pending{}, err
			}
			return Pending{Pending: len(files) > 0, Detail: countOf(len(files), "file differs", "files differ")}, nil
		},
		apply: func(context.Context) (Outcome, error) {
			changed, err := systemd.InstallWith(o.UnitDir, o.PolkitDir, o.UnitOverrides)
			out := Outcome{Files: changed, Reload: len(changed) > 0}
			for _, f := range changed {
				out.Changed = append(out.Changed, "installed "+f)
			}
			return out, err
		},
	}
}

func countOf(n int, one, many string) string {
	switch n {
	case 0:
		return ""
	case 1:
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

// ---- directories --------------------------------------------------------------------------

// clusterDirs are the directories under the config directory that converge keeps: the node's
// mesh key and certificate, and the files that are merged over config.toml.
var clusterDirs = []string{"cluster", "config.d"}

func directoriesStep(o Options) Step {
	// want lists what is wrong, as "<path> <what>".
	want := func() (todo []string, why string, err error) {
		uid, gid, ok := o.owner()
		if !ok {
			return nil, "the supavise user does not exist yet", nil
		}
		for _, name := range clusterDirs {
			dir := filepath.Join(o.ConfigDir, name)
			fi, err := os.Lstat(dir)
			switch {
			case errors.Is(err, fs.ErrNotExist):
				todo = append(todo, dir+" is missing")
			case errors.Is(err, fs.ErrPermission):
				return nil, "cannot look inside " + o.ConfigDir + " without root", nil
			case err != nil:
				return nil, "", err
			case !fi.IsDir():
				return nil, "", fmt.Errorf("%s is not a directory", dir)
			default:
				if fi.Mode().Perm() != 0o750 {
					todo = append(todo, fmt.Sprintf("%s is mode %04o", dir, fi.Mode().Perm()))
				}
				if u, g, ok := ownerOf(fi); ok && (u != uid || g != gid) {
					todo = append(todo, dir+" has another owner")
				}
			}
		}
		return todo, "", nil
	}
	return &funcStep{
		id: "directories", title: titleDirectories, root: true,
		check: func(context.Context) (Pending, error) {
			todo, why, err := want()
			if err != nil {
				return Pending{}, err
			}
			if why != "" {
				return Pending{Detail: why}, nil
			}
			return Pending{Pending: len(todo) > 0, Detail: strings.Join(todo, "; ")}, nil
		},
		apply: func(context.Context) (Outcome, error) {
			uid, gid, ok := o.owner()
			if !ok {
				return Outcome{}, nil
			}
			var out Outcome
			for _, name := range clusterDirs {
				dir := filepath.Join(o.ConfigDir, name)
				fi, err := os.Lstat(dir)
				created := errors.Is(err, fs.ErrNotExist)
				if err != nil && !created {
					return out, err
				}
				if !created && !fi.IsDir() {
					return out, fmt.Errorf("%s is not a directory", dir)
				}
				fixed := created
				if created {
					if err := os.MkdirAll(dir, 0o750); err != nil {
						return out, err
					}
				} else {
					if u, g, ok := ownerOf(fi); ok && (u != uid || g != gid) {
						fixed = true
					}
					fixed = fixed || fi.Mode().Perm() != 0o750
				}
				if !fixed {
					continue
				}
				if err := os.Chown(dir, uid, gid); err != nil {
					return out, err
				}
				if err := os.Chmod(dir, 0o750); err != nil {
					return out, err
				}
				verb := "set the owner and mode of"
				if created {
					verb = "created"
				}
				out.Changed = append(out.Changed, verb+" "+dir)
			}
			return out, nil
		},
	}
}

func (o *Options) owner() (uid, gid int, ok bool) {
	if o.Owner == nil {
		return 0, 0, false
	}
	return o.Owner()
}

// ---- mount protection ---------------------------------------------------------------------

// A unit whose state lives on a mounted volume must not start while the mount is missing, or it
// would start against an empty directory on the root disk. systemd's RequiresMountsFor= says
// that; the drop-in files below have the names and contents the CloudFormation template's
// first-boot script gave two of the units, so a node it made finds them already in place.
const (
	dataMountDropIn = "10-supavise-data-mount.conf"
	etcMountDropIn  = "20-supavise-etc-mount.conf"
)

// mountUnits are the service units that get a drop-in for the state directory (data) and for the
// config directory (etc). Every embedded service unit reads the state directory. The upgrade
// service keeps its own state on the root disk, so the data mount is none of its business; it
// does read config.toml.
func mountUnits() (data, etc []string) {
	for _, n := range systemd.Names() {
		if !strings.HasSuffix(n, ".service") || !strings.HasPrefix(n, "supavise") {
			continue
		}
		etc = append(etc, n)
		if n != "supavise-upgrade.service" {
			data = append(data, n)
		}
	}
	return data, etc
}

// mountWork is one drop-in file converge wants.
type mountWork struct{ path, content string }

func mountsWanted(o Options) (work []mountWork, why string, err error) {
	b, err := o.mountInfo()
	if err != nil {
		return nil, "the mount table cannot be read here", nil
	}
	mounts := parseMountPoints(b)
	data, etc := mountUnits()
	add := func(units []string, dir, file string) {
		if !onOwnMount(dir, mounts) {
			return
		}
		for _, u := range units {
			work = append(work, mountWork{
				path:    filepath.Join(o.UnitDir, u+".d", file),
				content: "[Unit]\nRequiresMountsFor=" + dir + "\n",
			})
		}
	}
	add(data, o.StateDir, dataMountDropIn)
	add(etc, o.ConfigDir, etcMountDropIn)
	if len(work) == 0 {
		return nil, "neither " + o.StateDir + " nor " + o.ConfigDir + " is a mount", nil
	}
	return work, "", nil
}

func mountsStep(o Options) Step {
	differing := func() ([]mountWork, string, error) {
		work, why, err := mountsWanted(o)
		if err != nil || why != "" {
			return nil, why, err
		}
		var todo []mountWork
		for _, w := range work {
			if cur, err := os.ReadFile(w.path); err == nil && string(cur) == w.content {
				continue
			}
			todo = append(todo, w)
		}
		return todo, "", nil
	}
	return &funcStep{
		id: "mounts", title: titleMounts, root: true,
		check: func(context.Context) (Pending, error) {
			todo, why, err := differing()
			if err != nil {
				return Pending{}, err
			}
			if why != "" {
				return Pending{Detail: why}, nil
			}
			return Pending{Pending: len(todo) > 0, Detail: countOf(len(todo), "drop-in is missing", "drop-ins are missing")}, nil
		},
		apply: func(context.Context) (Outcome, error) {
			todo, _, err := differing()
			if err != nil {
				return Outcome{}, err
			}
			var out Outcome
			for _, w := range todo {
				if err := os.MkdirAll(filepath.Dir(w.path), 0o755); err != nil {
					return out, err
				}
				if err := os.WriteFile(w.path, []byte(w.content), 0o644); err != nil {
					return out, err
				}
				out.Files = append(out.Files, w.path)
				out.Changed = append(out.Changed, "wrote "+w.path)
			}
			out.Reload = len(todo) > 0
			return out, nil
		},
	}
}

// parseMountPoints returns the mount points in the text of /proc/self/mountinfo (the fifth field of
// each line, with the octal escapes the kernel uses for spaces undone).
func parseMountPoints(b []byte) []string {
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 5 {
			continue
		}
		out = append(out, unescapeMount(f[4]))
	}
	return out
}

var octalEscape = regexp.MustCompile(`\\[0-7]{3}`)

func unescapeMount(s string) string {
	return octalEscape.ReplaceAllStringFunc(s, func(m string) string {
		n, _ := strconv.ParseUint(m[1:], 8, 8)
		return string([]byte{byte(n)})
	})
}

// onOwnMount reports whether dir is a mount point or lies below one other than the root file
// system's: the directory then lives on a volume that can be absent.
func onOwnMount(dir string, mounts []string) bool {
	dir = filepath.Clean(dir)
	for _, m := range mounts {
		if m == "/" {
			continue
		}
		if dir == m || strings.HasPrefix(dir, strings.TrimSuffix(m, "/")+"/") {
			return true
		}
	}
	return false
}

// ---- ufw ----------------------------------------------------------------------------------

func ufwStep(o Options) Step {
	// state says whether ufw is active and already admits the port; why explains a step that does
	// not apply. A ufw that cannot be read as root also leaves nothing to do, and unreadable says so:
	// a firewall query that fails is no reason to fail an upgrade, but the operator is told.
	state := func(ctx context.Context) (open bool, why string, unreadable bool) {
		if o.PeerPort <= 0 {
			return true, "no mesh port is configured", false
		}
		if _, err := o.lookPath("ufw"); err != nil {
			return true, "ufw is not installed", false
		}
		// ufw translates its output, and sudo keeps LANG and LC_*: "Status: active" is read in English.
		out, err := o.runner().Run(ctx, []string{"LC_ALL=C"}, "ufw", "status")
		if err != nil {
			if o.geteuid() != 0 {
				return true, "ufw's rules cannot be read without root", false
			}
			return true, fmt.Sprintf("ufw status failed (%v), so %d/tcp was not checked", err, o.PeerPort), true
		}
		if !strings.Contains(string(out), "Status: active") {
			return true, "ufw is not active", false
		}
		return ufwAdmits(string(out), o.PeerPort), "", false
	}
	return &funcStep{
		id: "ufw", title: titleUFW, root: true,
		check: func(ctx context.Context) (Pending, error) {
			open, why, _ := state(ctx)
			if open {
				return Pending{Detail: why}, nil
			}
			return Pending{Pending: true, Detail: fmt.Sprintf("%d/tcp is not allowed", o.PeerPort)}, nil
		},
		apply: func(ctx context.Context) (Outcome, error) {
			open, why, unreadable := state(ctx)
			if unreadable {
				return Outcome{Warnings: []string{why + fmt.Sprintf("; if ufw is active, run `sudo ufw allow %d/tcp`", o.PeerPort)}}, nil
			}
			if open {
				return Outcome{}, nil
			}
			rule := strconv.Itoa(o.PeerPort) + "/tcp"
			if _, err := o.runner().Run(ctx, nil, "ufw", "allow", rule); err != nil {
				return Outcome{}, err
			}
			return Outcome{Changed: []string{"allowed " + rule + " in ufw"}}, nil
		},
	}
}

// ufwAdmits reports whether `ufw status` lists an allow rule for the TCP port. A rule that the
// operator narrowed to a source address still counts: converge does not widen it.
func ufwAdmits(status string, port int) bool {
	p := strconv.Itoa(port)
	for _, line := range strings.Split(status, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || (f[0] != p && f[0] != p+"/tcp") {
			continue
		}
		for _, w := range f[1:] {
			if w == "ALLOW" {
				return true
			}
			if w == "DENY" || w == "REJECT" || w == "LIMIT" {
				break
			}
		}
	}
	return false
}

// ---- packages -----------------------------------------------------------------------------

// PackageSpec is a step that installs Debian packages a release needs.
type PackageSpec struct {
	// ID and Title name the step ("packages-xfsprogs", "Install xfsprogs").
	ID, Title string
	// Names are the apt package names.
	Names []string
}

// DeclaredPackages are the packages this release's converge installs. This release needs none; a
// release that does lists them here, and its host_changes then show the step.
var DeclaredPackages []PackageSpec

func packageStep(o Options, p PackageSpec) Step {
	missing := func(ctx context.Context) ([]string, error) {
		var out []string
		for _, n := range p.Names {
			b, err := o.runner().Run(ctx, nil, "dpkg-query", "-W", "-f=${Status}", n)
			if err != nil || !strings.Contains(string(b), "install ok installed") {
				out = append(out, n)
			}
		}
		return out, nil
	}
	return &funcStep{
		id: p.ID, title: p.Title, root: true,
		check: func(ctx context.Context) (Pending, error) {
			m, err := missing(ctx)
			if err != nil {
				return Pending{}, err
			}
			return Pending{Pending: len(m) > 0, Detail: strings.Join(m, ", ") + " not installed"}, nil
		},
		apply: func(ctx context.Context) (Outcome, error) {
			m, err := missing(ctx)
			if err != nil || len(m) == 0 {
				return Outcome{}, err
			}
			env := []string{"DEBIAN_FRONTEND=noninteractive"}
			// unattended-upgrades holds the dpkg lock for minutes at times; wait for it instead of
			// failing the converge (and, under the upgrade, rolling it back).
			lock := []string{"-o", "DPkg::Lock::Timeout=300"}
			// A stale package index is common on a server that has been up for months; a failed
			// refresh is not fatal if the install can still find the packages.
			_, _ = o.runner().Run(ctx, env, "apt-get", append(append([]string{}, lock...), "update", "-qq")...)
			if _, err := o.runner().Run(ctx, env, "apt-get", append(append([]string{}, lock...), append([]string{"install", "-y", "-qq"}, m...)...)...); err != nil {
				return Outcome{}, err
			}
			return Outcome{Changed: []string{"installed " + strings.Join(m, ", ")}}, nil
		},
	}
}

// ---- cluster settings ---------------------------------------------------------------------

// ConfigSyncer refreshes config.d/10-cluster.toml from the leader. The mesh package implements it
// on a node that is in a cluster.
type ConfigSyncer interface {
	// Sync fetches the leader's cluster-scoped settings and rewrites the file when they differ;
	// dryRun only reports. It returns the files it changed (or would change).
	Sync(ctx context.Context, dryRun bool) (changed []string, err error)
}

func configSyncStep(o Options) Step {
	return &funcStep{
		id: "config-d", title: titleConfigD, root: true,
		// A leader that cannot be reached is no reason to fail a host: the settings stay as they are
		// and the step says so (an upgrade that failed on it would roll the node back for the sake of
		// a copy of the leader's settings).
		check: func(ctx context.Context) (Pending, error) {
			changed, err := o.ConfigSync.Sync(ctx, true)
			if err != nil {
				return Pending{Detail: "the cluster settings were not refreshed: " + err.Error()}, nil
			}
			sort.Strings(changed)
			return Pending{Pending: len(changed) > 0, Detail: strings.Join(changed, ", ")}, nil
		},
		apply: func(ctx context.Context) (Outcome, error) {
			changed, err := o.ConfigSync.Sync(ctx, false)
			var out Outcome
			if err != nil {
				out.Warnings = append(out.Warnings, "the cluster settings were not refreshed: "+err.Error())
				return out, nil
			}
			for _, f := range changed {
				out.Changed = append(out.Changed, "wrote "+f)
			}
			out.Files = changed
			return out, nil
		},
	}
}
