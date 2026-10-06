package backup

import (
	"fmt"
	"strings"

	"github.com/OWNER/sbctl/internal/config"
)

// The nightly base backup is a systemd template pair. deploy/systemd/ holds the files
// rendered with the defaults (a test keeps them in sync); lifecycle renders them with
// the node's config when a project is created: RenderBackupService once per node,
// RenderBackupTimer once per node (OnCalendar comes from config), then
// `systemctl enable --now sb-basebackup@<ref>.timer` per project.

// BackupServiceUnit and BackupTimerUnit are the template unit names; instantiate with
// the project ref, e.g. sb-basebackup@<ref>.timer.
const (
	BackupServiceUnit = "sb-basebackup@.service"
	BackupTimerUnit   = "sb-basebackup@.timer"
)

// EnvFile is the optional environment file the backup service reads. `sbctl backups`
// runs outside the daemon and finds the registry through SBCTL_REGISTRY_DSN, which
// the installer or lifecycle writes here.
const EnvFile = "/etc/sbctl/sbctl.env"

// BackupTimerInstance is the timer instance to enable for ref.
func BackupTimerInstance(ref string) string { return "sb-basebackup@" + ref + ".timer" }

// RenderBackupService renders sb-basebackup@.service for the sbctl binary at binPath.
// The two ExecStart lines run in order and the second is skipped if the first fails,
// so retention never runs after a failed backup.
func RenderBackupService(binPath string) string {
	bin := unitExec(binPath)
	return fmt.Sprintf(`[Unit]
Description=sbctl base backup and retention for project %%i
After=sb-postgres@%%i.service

[Service]
Type=oneshot
User=sbctl
Group=sbctl
Slice=%s
# SBCTL_REGISTRY_DSN (and any other SBCTL_* override) for processes outside the daemon.
EnvironmentFile=-%s
Nice=10
IOSchedulingClass=idle
ExecStart=%s backups create %%i --reason scheduled
ExecStart=%s backups prune %%i
NoNewPrivileges=yes
PrivateTmp=yes
ProtectHome=yes
`, config.Slice, EnvFile, bin, bin)
}

// RenderBackupTimer renders sb-basebackup@.timer. onCalendar is a systemd OnCalendar
// expression (config.Backup.BaseBackupOnCalendar). The randomized delay spreads many
// projects over a quarter of an hour instead of starting them in the same second.
func RenderBackupTimer(onCalendar string) string {
	return fmt.Sprintf(`[Unit]
Description=Nightly sbctl base backup of project %%i

[Timer]
OnCalendar=%s
RandomizedDelaySec=15min
Persistent=true

[Install]
WantedBy=timers.target
`, onCalendar)
}

// unitExec quotes a path for an ExecStart= line.
func unitExec(p string) string {
	if strings.ContainsAny(p, " \t\"'\\%$") {
		p = strings.NewReplacer(`\`, `\\`, `"`, `\"`, `%`, `%%`, `$`, `$$`).Replace(p)
		return `"` + p + `"`
	}
	return p
}
