package update

import (
	"strings"
	"testing"
)

func TestDistro(t *testing.T) {
	for _, c := range []struct {
		rel  map[string]string
		want string
	}{
		{map[string]string{"ID": "ubuntu"}, "ubuntu"},
		{map[string]string{"ID": "debian"}, "debian"},
		{map[string]string{"ID": "linuxmint", "ID_LIKE": "ubuntu debian"}, "ubuntu"},
		{map[string]string{"ID": "raspbian", "ID_LIKE": "debian"}, "debian"},
		{map[string]string{"ID": "fedora"}, ""},
	} {
		if got := Distro(c.rel); got != c.want {
			t.Errorf("Distro(%v) = %q, want %q", c.rel, got, c.want)
		}
	}
}

func TestRenderAptConfig(t *testing.T) {
	for _, distro := range []string{"ubuntu", "debian"} {
		got, err := RenderAptConfig(distro)
		if err != nil {
			t.Fatal(err)
		}
		for _, must := range []string{
			`APT::Periodic::Unattended-Upgrade "1";`,
			`#clear Unattended-Upgrade::Allowed-Origins;`,
			`#clear Unattended-Upgrade::Origins-Pattern;`,
			`Unattended-Upgrade::Automatic-Reboot "false";`,
			"${distro_codename}-security",
		} {
			if !strings.Contains(got, must) {
				t.Errorf("%s: missing %q in\n%s", distro, must, got)
			}
		}
		// Only security origins: never the -updates, -backports or -proposed pockets, and never
		// Debian's point-release pattern.
		_, rest, _ := strings.Cut(got, "Unattended-Upgrade::Origins-Pattern {")
		list, _, _ := strings.Cut(rest, "};")
		for _, never := range []string{"-updates", "backports", "proposed", "label=Debian\"", "label=Debian;"} {
			if strings.Contains(list, never) {
				t.Errorf("%s: %q must not be in the origin list:\n%s", distro, never, list)
			}
		}
		// The clears must come before the list they empty, or they would remove the new entries.
		if strings.Index(got, "#clear Unattended-Upgrade::Origins-Pattern") > strings.Index(got, "Unattended-Upgrade::Origins-Pattern {") {
			t.Errorf("%s: #clear must precede the list", distro)
		}
	}
	u, _ := RenderAptConfig("ubuntu")
	if !strings.Contains(u, "origin=Ubuntu,archive=${distro_codename}-security") {
		t.Errorf("ubuntu: security pocket missing:\n%s", u)
	}
	d, _ := RenderAptConfig("debian")
	if !strings.Contains(d, "origin=Debian,codename=${distro_codename}-security,label=Debian-Security") {
		t.Errorf("debian: security archive missing:\n%s", d)
	}
	if _, err := RenderAptConfig("fedora"); err == nil {
		t.Error("an unsupported distribution must be an error, not a guess")
	}
}

func TestNeedrestartKeepsSupaviseUnitsAlone(t *testing.T) {
	if got := RenderNeedrestart(); !strings.Contains(got, "$nrconf{override_rc}{qr(^supavise)} = 0;") {
		t.Errorf("needrestart drop-in:\n%s", got)
	}
}

func TestKernelStatus(t *testing.T) {
	out := "NEEDRESTART-VER: 3.6\nNEEDRESTART-KCUR: 6.1.0-18-amd64\nNEEDRESTART-KEXP: 6.1.0-21-amd64\nNEEDRESTART-KSTA: 3\n"
	if got := KernelStatus(out); got != 3 || !KernelRebootDue(got) {
		t.Errorf("KSTA 3: %d", got)
	}
	for ksta, due := range map[string]bool{"0": false, "1": false, "2": true, "3": true, "9": false, "x": false} {
		got := KernelStatus("NEEDRESTART-KSTA: " + ksta + "\n")
		if KernelRebootDue(got) != due {
			t.Errorf("KSTA %s: due = %v, want %v", ksta, KernelRebootDue(got), due)
		}
	}
	if KernelStatus("nothing useful") != 0 || KernelRebootDue(0) {
		t.Error("output without a status line is 'unknown', which never asks for a reboot")
	}
}
