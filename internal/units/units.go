// Package units renders and drives the systemd units (or, in dev and tests, child
// processes) that run Supabase's native artifacts.
package units

import (
	"bytes"
	"context"
	"strings"
	"time"

	"github.com/OWNER/sbctl/internal/config"
)

// Spec is everything needed to render one unit instance: the env file at
// config.Paths.EnvFile(ref, svc), resource limits as a drop-in, and the artifact
// directory the template's ExecStart points into.
type Spec struct {
	Service     string // config.Svc*
	Ref         string // project ref; "" for fleet singletons
	ArtifactDir string // unpacked artifact root (contains bin/)
	WorkDir     string // service state dir, e.g. config.Paths.ProjectService(ref, svc)
	Env         map[string]string
	Limits      config.Limits
	// Exec overrides the artifact launcher (path relative to ArtifactDir, plus args).
	// Empty means the service's standard launcher (StandardExec) without arguments.
	Exec []string
	// PreStart lists one-shot commands (path relative to ArtifactDir, plus args) run
	// in order before Exec, with the same environment; a failing command stops the
	// unit. GoTrue uses it for "bin/auth migrate".
	PreStart [][]string
	// DenyEgress confines the unit's network traffic to loopback (systemd: IPAddressDeny=any
	// with IPAddressAllow=127.0.0.1/32 ::1/128, applied as a persistent per-unit drop-in). A Spec without
	// it lifts a restriction an earlier Render of the same unit set. Only the systemd backend
	// can enforce it: the exec backend (development and tests) runs plain child processes and
	// ignores the field, which EgressEnforcer lets a caller ask about.
	DenyEgress bool
}

// EgressEnforcer is implemented by the supervisors that apply Spec.DenyEgress.
type EgressEnforcer interface {
	EnforcesEgress() bool
}

// IPRange is one entry of systemd's IPAddressAllow= and IPAddressDeny= lists as the D-Bus
// API takes it (a(iayu)): the address family (AF_INET 2, AF_INET6 10), the address bytes and
// the prefix length.
type IPRange struct {
	Family int32
	Addr   []byte
	Prefix uint32
}

// EgressDeny is IPAddressDeny=any: every IPv4 and IPv6 address.
func EgressDeny() []IPRange {
	return []IPRange{{Family: 2, Addr: make([]byte, 4)}, {Family: 10, Addr: make([]byte, 16)}}
}

// EgressAllow is IPAddressAllow=127.0.0.1/32 ::1/128: the two loopback addresses every client of
// a project's Postgres uses (listen_addresses is 127.0.0.1; the supervised services and the
// pooler connect to 127.0.0.1 or the unix socket). It is deliberately not systemd's "localhost"
// (all of 127.0.0.0/8): the rest of that block holds addresses that something on the host
// answers on, systemd-resolved's stub resolver at 127.0.0.53 first among them, which forwards
// queries upstream and so is a DNS side channel out of a confined unit.
func EgressAllow() []IPRange {
	v6 := make([]byte, 16)
	v6[15] = 1
	return []IPRange{{Family: 2, Addr: []byte{127, 0, 0, 1}, Prefix: 32}, {Family: 10, Addr: v6, Prefix: 128}}
}

// EgressHiddenPaths are the unix sockets and directories a unit with a denied egress policy
// cannot open, as InaccessiblePaths= entries (a leading "-" skips a path the host does not
// have). The IP filter does not cover AF_UNIX, and these are the ways out of it on a host: the
// resolver's varlink socket under /run/systemd/resolve (glibc reaches it through nss-resolve and
// resolves names for the unit, the default on Fedora and Arch), the D-Bus system bus (a polkit
// rule of sbctl's would let the unit lift its own IPAddressDeny) and nscd's socket. It is added
// to the unit's own InaccessiblePaths (the master key), and takes effect when the unit starts.
// A unit that also needs /etc/resolv.conf's target (a symlink into /run/systemd/resolve on
// Ubuntu) gets no name resolution, which a unit without egress does not need.
func EgressHiddenPaths() []string {
	return []string{"-/run/systemd/resolve", "-/run/dbus", "-/run/nscd"}
}

// postgresInaccessible is what deploy/systemd/sb-postgres@.service hides itself
// (TestTemplatesContainment keeps the two equal). Lifting the egress paths means resetting
// the property, which drops the unit file's own entries too, so they are set again.
func postgresInaccessible() []string { return []string{"-/etc/sbctl/master.key"} }

// pathKey is a path as the D-Bus property shows it, without the "-" (ignore) and "+" prefixes.
func pathKey(p string) string { return strings.TrimLeft(p, "-+") }

// hidesPaths reports whether the InaccessiblePaths value of a loaded unit lists every one of
// want (all=true) or at least one of them (all=false).
func hidesPaths(v interface{}, want []string, all bool) bool {
	have := map[string]bool{}
	if l, ok := v.([]string); ok {
		for _, p := range l {
			have[pathKey(p)] = true
		}
	}
	found := false
	for _, w := range want {
		if have[pathKey(w)] {
			found = true
		} else if all {
			return false
		}
	}
	return found || all
}

// egressMatches reports whether props (the Service-type properties of a loaded unit, as the D-Bus
// client returns them) already carry exactly the egress policy deny asks for: IPAddressDeny and
// IPAddressAllow equal to EgressDeny and EgressAllow and InaccessiblePaths listing EgressHiddenPaths, or none
// of that when deny is false. A unit
// that was confined by an earlier release with the wider 127.0.0.0/8 allow does not match, so
// the next Render narrows it. A value of a shape it does not know counts as not matching:
// setting the properties again is harmless.
func egressMatches(props map[string]interface{}, deny bool) bool {
	var wantDeny, wantAllow []IPRange
	if deny {
		wantDeny, wantAllow = EgressDeny(), EgressAllow()
	}
	if !sameRanges(decodeRanges(props["IPAddressDeny"]), wantDeny) || !sameRanges(decodeRanges(props["IPAddressAllow"]), wantAllow) {
		return false
	}
	if deny {
		return hidesPaths(props["InaccessiblePaths"], EgressHiddenPaths(), true)
	}
	return !hidesPaths(props["InaccessiblePaths"], EgressHiddenPaths(), false)
}

// decodeRanges reads an a(iayu) property: a list of [family, address bytes, prefix].
func decodeRanges(v interface{}) []IPRange {
	if v == nil {
		return nil
	}
	list, ok := v.([][]interface{})
	if !ok {
		return []IPRange{{Family: -1}}
	}
	out := make([]IPRange, 0, len(list))
	for _, e := range list {
		if len(e) != 3 {
			return []IPRange{{Family: -1}} // never equal to a wanted list
		}
		f, ok1 := e[0].(int32)
		a, ok2 := e[1].([]byte)
		p, ok3 := e[2].(uint32)
		if !ok1 || !ok2 || !ok3 {
			return []IPRange{{Family: -1}}
		}
		out = append(out, IPRange{Family: f, Addr: a, Prefix: p})
	}
	return out
}

func sameRanges(a, b []IPRange) bool {
	if len(a) != len(b) {
		return false
	}
	for _, x := range a {
		found := false
		for _, y := range b {
			if x.Family == y.Family && x.Prefix == y.Prefix && bytes.Equal(x.Addr, y.Addr) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// Unit returns the systemd unit name for the spec.
func (s Spec) Unit() string { return config.UnitName(s.Service, s.Ref) }

type State string

const (
	StateActive       State = "active"
	StateActivating   State = "activating"
	StateDeactivating State = "deactivating"
	StateInactive     State = "inactive"
	StateFailed       State = "failed"
	StateUnknown      State = "unknown"
)

type Status struct {
	Unit        string
	State       State
	SubState    string
	MainPID     int
	MemoryBytes uint64
	Since       time.Time
}

// Supervisor is implemented by the systemd (D-Bus) backend and by an exec backend
// that runs launchers as child processes for development and tests.
type Supervisor interface {
	// Render writes the env file and limits drop-in for spec and reloads the manager.
	// Rendering an unchanged spec is a no-op.
	Render(ctx context.Context, spec Spec) error
	Start(ctx context.Context, unit string) error
	Stop(ctx context.Context, unit string) error
	Status(ctx context.Context, unit string) (Status, error)
	// Remove stops the unit and deletes everything Render wrote for it.
	Remove(ctx context.Context, unit string) error
}

// ChangeRenderer is implemented by both backends. RenderChanged is Render that also
// reports whether the env file or run script differed from what was on disk, which tells
// a caller that a running unit still runs on the old files and needs a restart.
type ChangeRenderer interface {
	RenderChanged(ctx context.Context, spec Spec) (changed bool, err error)
}

// StandardExec returns the artifact launcher of svc, relative to the artifact root.
func StandardExec(svc string) []string {
	switch svc {
	case config.SvcPostgres:
		return []string{"bin/supabase-postgres-start"}
	case config.SvcGoTrue:
		return []string{"bin/auth"}
	case config.SvcPostgREST:
		return []string{"bin/postgrest"}
	case config.SvcSupavisor, config.SvcRealtime:
		return []string{"bin/server"}
	case config.SvcStorage:
		return []string{"bin/storage"}
	case config.SvcPGMeta:
		return []string{"bin/pgmeta"}
	case config.SvcStudio:
		return []string{"bin/studio"}
	case config.SvcImgproxy:
		return []string{"bin/imgproxy"}
	case config.SvcEdgeRuntime:
		return []string{"bin/edge-runtime"}
	}
	return nil
}

// LogTailer is implemented by backends that keep a unit's output in a file the
// process can read (the exec backend). The systemd backend logs to journald only.
type LogTailer interface {
	Tail(unit string, lines int) string
}
