package backup

import (
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/jsmillerdev/supavise/internal/config"
)

// The nightly base backup is a systemd template pair. deploy/systemd/ holds the files
// rendered with the defaults (a test keeps them in sync); lifecycle renders them with
// the node's config when a project is created: RenderBackupService once per node,
// RenderBackupTimer once per node (OnCalendar comes from config), then
// `systemctl enable --now supavise-basebackup@<ref>.timer` per project.

// BackupServiceUnit and BackupTimerUnit are the template unit names; instantiate with
// the project ref, e.g. supavise-basebackup@<ref>.timer.
const (
	BackupServiceUnit = "supavise-basebackup@.service"
	BackupTimerUnit   = "supavise-basebackup@.timer"
)

// EnvFile is the optional environment file the backup service reads. `supavise backups`
// runs outside the daemon and finds the registry through SUPAVISE_REGISTRY_DSN, which
// the installer or lifecycle writes here. The DSN contains the registry password, so
// the file must be mode 0600 and owned by the supavise user (HANDOFF section 1); `supavise
// backups` warns when it is readable by group or others.
const EnvFile = "/etc/supavise/supavise.env"

// The node-wide prune pair covers what the per-project timers cannot: a deleted project's
// timer is gone, but its final backup and WAL stay in the backend and must still age out.
// It runs `supavise backups prune` without a ref, which prunes every ref in the registry and
// in the backend. Enable it once per node: systemctl enable --now supavise-basebackup-prune.timer.
const (
	PruneServiceUnit = "supavise-basebackup-prune.service"
	PruneTimerUnit   = "supavise-basebackup-prune.timer"
	// PruneOnCalendar is when the node-wide prune runs: after the nightly base backups.
	PruneOnCalendar = "*-*-* 05:00:00"
)

// ValidateOnCalendar rejects a calendar expression that cannot be put in a unit file as
// is: empty, or containing a control character (a newline would inject directives).
// Whether systemd understands the expression is for `systemd-analyze calendar` to say.
func ValidateOnCalendar(s string) error {
	if strings.TrimSpace(s) == "" {
		return errors.New("OnCalendar expression is empty")
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return fmt.Errorf("OnCalendar expression %q contains a control character", s)
		}
	}
	return nil
}

// BackupTimerInstance is the timer instance to enable for ref.
func BackupTimerInstance(ref string) string { return "supavise-basebackup@" + ref + ".timer" }

// RenderBackupService renders supavise-basebackup@.service for the supavise binary at binPath.
// The two ExecStart lines run in order and the second is skipped if the first fails,
// so retention never runs after a failed backup.
func RenderBackupService(binPath string) string {
	bin := unitExec(binPath)
	return fmt.Sprintf(`[Unit]
Description=Supavise base backup and retention for project %%i
After=supavise-postgres@%%i.service

[Service]
Type=oneshot
User=supavise
Group=supavise
Slice=%s
# SUPAVISE_REGISTRY_DSN (and any other SUPAVISE_* override) for processes outside the daemon.
EnvironmentFile=-%s
Nice=10
# Not "idle": an idle-class backup can be starved indefinitely on a busy disk while
# pg_backup_start() is held, which keeps the cluster in backup mode.
IOSchedulingClass=best-effort
IOSchedulingPriority=7
ExecStart=%s backups create %%i --reason scheduled
ExecStart=%s backups prune %%i
NoNewPrivileges=yes
PrivateTmp=yes
ProtectHome=yes
# The same uid runs every unit, and a process may read /proc/<pid>/environ and /proc/<pid>/root of
# another process of its uid unless that process has capabilities the reader lacks. These units read
# the master key and the backend credentials, so they hold one harmless capability in their permitted
# set (supavise.service does the same); the supavise-* units that run tenant code have none (deploy/systemd/README.md).
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
AmbientCapabilities=CAP_NET_BIND_SERVICE
# Mount allowlist: the unit sees the registry's socket, this project's data directory (read-only)
# and WAL socket directory, and the file backend's directory (no effect on S3), not other projects'
# data directories, environment files or functions.
TemporaryFileSystem=/var/lib/supavise:ro
BindReadOnlyPaths=/var/lib/supavise/projects/system/postgres/sock /var/lib/supavise/projects/%%i/postgres
BindPaths=-/var/lib/supavise/projects/%%i/wal -/var/lib/supavise/backups
`, config.Slice, EnvFile, bin, bin)
}

// RenderBackupTimer renders supavise-basebackup@.timer. onCalendar is a systemd OnCalendar
// expression (config.Backup.BaseBackupOnCalendar). The randomized delay spreads many
// projects over a quarter of an hour instead of starting them in the same second.
//
// An invalid expression (see ValidateOnCalendar) never reaches the unit file: the
// default schedule is rendered instead. Callers that want the error use
// RenderBackupTimerChecked.
func RenderBackupTimer(onCalendar string) string {
	t, err := RenderBackupTimerChecked(onCalendar)
	if err != nil {
		t, _ = RenderBackupTimerChecked(config.Default().Backup.BaseBackupOnCalendar)
	}
	return t
}

// RenderBackupTimerChecked is RenderBackupTimer that reports an invalid expression.
func RenderBackupTimerChecked(onCalendar string) (string, error) {
	if err := ValidateOnCalendar(onCalendar); err != nil {
		return "", err
	}
	return fmt.Sprintf(`[Unit]
Description=Nightly Supavise base backup of project %%i

[Timer]
OnCalendar=%s
RandomizedDelaySec=15min
Persistent=true

[Install]
WantedBy=timers.target
`, onCalendar), nil
}

// RenderPruneService renders supavise-basebackup-prune.service for the supavise binary at binPath.
func RenderPruneService(binPath string) string {
	return fmt.Sprintf(`[Unit]
Description=Supavise backup retention for every project and every deleted project's archive

[Service]
Type=oneshot
User=supavise
Group=supavise
Slice=%s
EnvironmentFile=-%s
Nice=10
IOSchedulingClass=best-effort
IOSchedulingPriority=7
ExecStart=%s backups prune
NoNewPrivileges=yes
PrivateTmp=yes
ProtectHome=yes
# The same uid runs every unit, and a process may read /proc/<pid>/environ and /proc/<pid>/root of
# another process of its uid unless that process has capabilities the reader lacks. These units read
# the master key and the backend credentials, so they hold one harmless capability in their permitted
# set (supavise.service does the same); the supavise-* units that run tenant code have none (deploy/systemd/README.md).
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
AmbientCapabilities=CAP_NET_BIND_SERVICE
# Mount allowlist: the registry's socket and the file backend's directory, no project directory.
TemporaryFileSystem=/var/lib/supavise:ro
BindReadOnlyPaths=/var/lib/supavise/projects/system/postgres/sock
BindPaths=-/var/lib/supavise/backups
`, config.Slice, EnvFile, unitExec(binPath))
}

// RenderPruneTimer renders supavise-basebackup-prune.timer.
func RenderPruneTimer() string {
	return fmt.Sprintf(`[Unit]
Description=Daily Supavise backup retention

[Timer]
OnCalendar=%s
RandomizedDelaySec=15min
Persistent=true

[Install]
WantedBy=timers.target
`, PruneOnCalendar)
}

// unitExec quotes a path for an ExecStart= line.
func unitExec(p string) string {
	if strings.ContainsAny(p, " \t\"'\\%$") {
		p = strings.NewReplacer(`\`, `\\`, `"`, `\"`, `%`, `%%`, `$`, `$$`).Replace(p)
		return `"` + p + `"`
	}
	return p
}
