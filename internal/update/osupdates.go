package update

import (
	"fmt"
	"strconv"
	"strings"
)

// Operating system patching. `supavise install` turns on unattended-upgrades for the
// distribution's security updates and nothing else, on a new install, unless the operator passes
// --no-os-updates. Supavise does not run the patching: apt's own timers do, and
// unattended-upgrades is the program that installs the packages.
//
// The reboot an OS patch asks for is not left to unattended-upgrades. Its Automatic-Reboot
// option reboots at a time of day, not on a day of the week, so it cannot honor a window such as
// "Sun 04:00-06:00": it would reboot the node the next morning whatever the day. The
// configuration below therefore turns Automatic-Reboot off, and `supavise update run` reboots the
// node when the window is open and a reboot is due (os_reboot = "window"). It finds out
// that a reboot is due from /run/reboot-required (Ubuntu writes it) and from needrestart's
// kernel check (Debian does not write it), so one rule serves both.
//
// needrestart restarts services whose libraries an update replaced, and on a Supavise node that
// would restart a project's Postgres in the middle of the day. The drop-in for needrestart leaves
// every supavise-* unit alone: a unit picks up a new library at the next reboot or at the
// next Supavise upgrade.
const (
	// AptConfigFile is the apt configuration Supavise owns. It sorts after the 50unattended-upgrades
	// file that the package installs, so its settings win.
	AptConfigFile = "/etc/apt/apt.conf.d/52supavise-unattended-upgrades"
	// NeedrestartFile is the needrestart drop-in.
	NeedrestartFile = "/etc/needrestart/conf.d/50-supavise.conf"
)

// OSPackages are the packages the installer adds for OS patching.
var OSPackages = []string{"unattended-upgrades", "needrestart"}

// Distro names the family of an /etc/os-release: "ubuntu" or "debian", or "" for anything else.
// A derivative that lists one of them in ID_LIKE counts as that family.
func Distro(osRelease map[string]string) string {
	for _, id := range append([]string{osRelease["ID"]}, strings.Fields(osRelease["ID_LIKE"])...) {
		switch id {
		case "ubuntu", "debian":
			return id
		}
	}
	return ""
}

// RenderAptConfig renders the unattended-upgrades configuration for a distribution family: the
// package lists refreshed and security updates installed daily, no other origin, and no
// automatic reboot (see the package comment). The origin patterns use unattended-upgrades'
// ${distro_codename}, so the file keeps working when the release changes.
func RenderAptConfig(distro string) (string, error) {
	var origins string
	switch distro {
	case "ubuntu":
		// Ubuntu's security pocket and, on a machine attached to Ubuntu Pro, its ESM pockets.
		origins = `        "origin=Ubuntu,archive=${distro_codename}-security";
        "origin=UbuntuESMApps,archive=${distro_codename}-apps-security";
        "origin=UbuntuESM,archive=${distro_codename}-infra-security";
`
	case "debian":
		// security.debian.org only. The label=Debian pattern in Debian's own configuration also
		// takes point-release updates, which are not security updates.
		origins = `        "origin=Debian,codename=${distro_codename}-security,label=Debian-Security";
`
	default:
		return "", fmt.Errorf("unattended security updates are set up for Ubuntu and Debian only")
	}
	return `// Managed by Supavise (supavise install; --no-os-updates removes this file).
// Security updates only. This file replaces the origin lists of the distribution's
// 50unattended-upgrades; the reboot an update needs happens in Supavise's maintenance window
// (supavise-upgrade.timer, update.os_reboot in config.toml), not here.
APT::Periodic::Update-Package-Lists "1";
APT::Periodic::Unattended-Upgrade "1";
#clear Unattended-Upgrade::Allowed-Origins;
#clear Unattended-Upgrade::Origins-Pattern;
Unattended-Upgrade::Origins-Pattern {
` + origins + `};
Unattended-Upgrade::Automatic-Reboot "false";
`, nil
}

// RenderNeedrestart renders the needrestart drop-in that keeps it away from the Supavise units.
func RenderNeedrestart() string {
	return `# Managed by Supavise. A library update must not restart a project's Postgres in the middle
# of the day: supavise-* units pick it up at the next reboot or Supavise upgrade.
$nrconf{override_rc}{qr(^supavise)} = 0;
`
}

// KernelStatus reads NEEDRESTART-KSTA from the batch output of `needrestart -b -k`: 0 unknown,
// 1 no pending kernel upgrade, 2 an ABI-compatible upgrade is pending, 3 a version upgrade is
// pending. It returns 0 when the output has no such line.
func KernelStatus(out string) int {
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), ":")
		if ok && strings.TrimSpace(k) == "NEEDRESTART-KSTA" {
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil || n < 0 || n > 3 {
				return 0
			}
			return n
		}
	}
	return 0
}

// KernelRebootDue reports whether a needrestart kernel status asks for a reboot.
func KernelRebootDue(ksta int) bool { return ksta == 2 || ksta == 3 }
