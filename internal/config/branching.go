package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Branching is the [branching] config section (docs/design.md section 9a).
type Branching struct {
	// DefaultTTL is how long a non-persistent branch lives before the sweeper deletes it:
	// a Go duration ("36h") or whole days ("7d"). Empty means 7d; "0" or "off" never expires.
	DefaultTTL string `toml:"default_ttl"`
	// DefaultClass is the project class of a branch that does not ask for an instance size.
	// Empty means "micro": branches are disposable and a node runs many of them.
	DefaultClass string `toml:"default_class"`
	// MaxPerProject and MaxTotal bound the branches of one project and of the node (0 means
	// 10 and 50). Each branch is a full cluster, so a runaway agent must hit a wall.
	MaxPerProject int `toml:"max_per_project"`
	MaxTotal      int `toml:"max_total"`
	// SweepIntervalSeconds is how often the daemon looks for lapsed branches (0 means 60).
	SweepIntervalSeconds int `toml:"sweep_interval_seconds"`
	// Clone is "auto" (default: copy-on-write when the data disk supports it, else restore
	// the parent's base backup plus WAL) or "backup" (always restore from the base backup).
	Clone string `toml:"clone"`
	// SoftDeleteGraceMinutes is the grace period of DELETE /v1/branches/{id}?force=false
	// (0 means 60, the Management API's documented hour).
	SoftDeleteGraceMinutes int `toml:"soft_delete_grace_minutes"`
	// AllowPrivateNotifyURLs lets a branch's notify_url resolve to loopback, private and
	// link-local addresses. Off by default: the daemon would otherwise be a way to reach
	// the instance metadata service or other internal endpoints.
	AllowPrivateNotifyURLs bool `toml:"allow_private_notify_urls"`
	// KeepCronJobs leaves the parent's pg_cron jobs active in a branch that was cloned from the
	// parent's data. Off by default: a job that calls an external service (pg_net, an HTTP
	// extension, a function URL) would otherwise run twice, once from the parent and once
	// from every branch.
	KeepCronJobs bool `toml:"keep_cron_jobs"`
	// DiskReserveMB is the free space, in MB, that must remain on the state directory's disk
	// after a branch with data is created or reset, on top of 1.2 times the parent's data
	// (0 means 2048). A create or reset that would not leave it is refused before anything is
	// done: a clone that fills the disk takes every project on the node down with it.
	DiskReserveMB int `toml:"disk_reserve_mb"`
}

const (
	defaultBranchTTL      = 7 * 24 * time.Hour
	defaultMaxPerProject  = 10
	defaultMaxBranches    = 50
	defaultSweepInterval  = time.Minute
	defaultSoftDeleteHold = time.Hour
	defaultDiskReserveMB  = 2048
)

// TTL returns the default lifetime of a non-persistent branch; ok is false when branches
// do not expire by default.
func (b Branching) TTL() (d time.Duration, ok bool, err error) {
	s := strings.ToLower(strings.TrimSpace(b.DefaultTTL))
	switch s {
	case "":
		return defaultBranchTTL, true, nil
	case "0", "off", "never":
		return 0, false, nil
	}
	if days, found := strings.CutSuffix(s, "d"); found {
		n, perr := strconv.Atoi(days)
		if perr != nil || n <= 0 {
			return 0, false, fmt.Errorf("config: branching.default_ttl %q: want a duration like 36h, whole days like 7d, or off", b.DefaultTTL)
		}
		return time.Duration(n) * 24 * time.Hour, true, nil
	}
	d, perr := time.ParseDuration(s)
	if perr != nil || d <= 0 {
		return 0, false, fmt.Errorf("config: branching.default_ttl %q: want a duration like 36h, whole days like 7d, or off", b.DefaultTTL)
	}
	return d, true, nil
}

// Class is DefaultClass or "micro".
func (b Branching) Class() string {
	if b.DefaultClass == "" {
		return "micro"
	}
	return b.DefaultClass
}

// Limits returns the per-project and node-wide branch caps.
func (b Branching) Limits() (perProject, total int) {
	perProject, total = b.MaxPerProject, b.MaxTotal
	if perProject <= 0 {
		perProject = defaultMaxPerProject
	}
	if total <= 0 {
		total = defaultMaxBranches
	}
	return perProject, total
}

// DiskReserve is the free space in bytes that a branch with data must leave on the disk.
func (b Branching) DiskReserve() int64 {
	mb := b.DiskReserveMB
	if mb <= 0 {
		mb = defaultDiskReserveMB
	}
	return int64(mb) << 20
}

// SweepInterval is the pause between sweeps.
func (b Branching) SweepInterval() time.Duration {
	if b.SweepIntervalSeconds <= 0 {
		return defaultSweepInterval
	}
	return time.Duration(b.SweepIntervalSeconds) * time.Second
}

// SoftDeleteGrace is the hold of a scheduled deletion.
func (b Branching) SoftDeleteGrace() time.Duration {
	if b.SoftDeleteGraceMinutes <= 0 {
		return defaultSoftDeleteHold
	}
	return time.Duration(b.SoftDeleteGraceMinutes) * time.Minute
}

func (b Branching) validate() error {
	if _, _, err := b.TTL(); err != nil {
		return err
	}
	switch b.Clone {
	case "", "auto", "backup":
	default:
		return fmt.Errorf("config: branching.clone must be auto or backup, not %q", b.Clone)
	}
	if b.DiskReserveMB < 0 {
		return fmt.Errorf("config: branching.disk_reserve_mb must not be negative, not %d", b.DiskReserveMB)
	}
	return nil
}
