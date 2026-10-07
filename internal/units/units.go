// Package units renders and drives the systemd units (or, in dev and tests, child
// processes) that run Supabase's native artifacts.
package units

import (
	"bytes"
	"context"
	"net"
	"strings"
	"time"

	"github.com/jsmillerdev/supavise/internal/config"
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
	// Log, when set, is a file the launcher script redirects its stdout and stderr to, for a
	// one-shot unit whose caller wants the output (the exec backend's own log is not shared
	// with systemd's journal). The path must be writable inside the unit.
	Log string
	// PreStart lists one-shot commands (path relative to ArtifactDir, plus args) run
	// in order before Exec, with the same environment; a failing command stops the
	// unit. GoTrue uses it for "bin/auth migrate".
	PreStart [][]string
	// PublicRun makes the launcher script world-readable (0755 instead of 0750), for a unit
	// that runs under another uid than the supavise user (supavise-edge-bundle@<ref>.service, a dynamic
	// user). The script holds paths and arguments, never secrets: the environment is in the
	// 0600 env file, which systemd reads as root.
	PublicRun bool
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

// IMDSDeny is IPAddressDeny=169.254.169.254 fd00:ec2::254, which every supavise-* template carries
// (the cloud metadata service holds the instance role): the baseline of a Postgres unit that is
// not egress-confined. Lifting a branch's restriction must restore this list, not empty it.
func IMDSDeny() []IPRange {
	v6 := net.ParseIP("fd00:ec2::254").To16()
	return []IPRange{{Family: 2, Addr: []byte{169, 254, 169, 254}, Prefix: 32}, {Family: 10, Addr: v6, Prefix: 128}}
}

// egressManaged reports whether the unit's IP lists are supavise's to set: a project's Postgres unit
// (the only one a branch's egress policy applies to). The other templates own their lists, which
// differ (supavise-edge-bundle@ also denies localhost and allows the resolver stub) and which a lift
// must not overwrite.
func egressManaged(unit string) bool { return strings.HasPrefix(unit, "supavise-postgres@") }

// egressMatches reports whether props (the Service-type properties of a loaded unit, as the D-Bus
// client returns them) already carry exactly the egress policy deny asks for: IPAddressDeny and
// IPAddressAllow equal to EgressDeny and EgressAllow, or, when deny is false, IPAddressDeny equal to
// the template's metadata-service list (IMDSDeny) with no IPAddressAllow. A unit that was confined by
// an earlier release with the wider 127.0.0.0/8 allow does not match, so the next Render narrows it;
// neither does one whose metadata deny was lost, so the next Render restores it. A unit that systemd
// does not have loaded (nil props) counts as having the template's defaults when no restriction is
// wanted. A value of a shape it does not know counts as not matching: setting the properties again is
// harmless.
func egressMatches(props map[string]interface{}, deny bool) bool {
	if props == nil && !deny {
		return true
	}
	wantDeny, wantAllow := IMDSDeny(), []IPRange(nil)
	if deny {
		wantDeny, wantAllow = EgressDeny(), EgressAllow()
	}
	return sameRanges(decodeRanges(props["IPAddressDeny"]), wantDeny) && sameRanges(decodeRanges(props["IPAddressAllow"]), wantAllow)
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

// Sandboxer is implemented by a Supervisor to say whether the units it runs are confined by
// their unit files (mount namespace, address filters). The exec backend runs plain child
// processes and is not.
type Sandboxer interface{ Sandboxed() bool }

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
