package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var (
	sshPortRe      = regexp.MustCompile(`(?im)^\s*Port\s+(\d+)`)
	socketListenRe = regexp.MustCompile(`:(\d+) \(Stream\)`)
)

// parseListeningSSHPorts reads `ss -ltnpH` output and returns the ports an sshd process
// listens on.
func parseListeningSSHPorts(ss string) []int {
	var ports []int
	for _, line := range strings.Split(ss, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 || !strings.Contains(line, `"sshd"`) {
			continue
		}
		if i := strings.LastIndex(f[3], ":"); i >= 0 {
			if n, err := strconv.Atoi(f[3][i+1:]); err == nil {
				ports = append(ports, n)
			}
		}
	}
	return ports
}

// parseSocketListen reads `systemctl show -p Listen --value ssh.socket` ("[::]:22 (Stream)"
// per line) and returns the stream ports.
func parseSocketListen(out string) []int {
	var ports []int
	for _, m := range socketListenRe.FindAllStringSubmatch(out, -1) {
		if n, err := strconv.Atoi(m[1]); err == nil {
			ports = append(ports, n)
		}
	}
	return ports
}

// parseConfiguredSSHPorts reads Port lines from sshd configuration text (sshd_config and its
// drop-ins, or `sshd -T`).
func parseConfiguredSSHPorts(text string) []int {
	var ports []int
	for _, m := range sshPortRe.FindAllStringSubmatch(text, -1) {
		if n, err := strconv.Atoi(m[1]); err == nil {
			ports = append(ports, n)
		}
	}
	return ports
}

func uniqueSorted(in []int) []int {
	seen := map[int]bool{}
	var out []int
	for _, p := range in {
		if p > 0 && p < 65536 && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	sort.Ints(out)
	return out
}

// sshPorts returns the TCP ports SSH is reachable on, so enabling a firewall cannot lock the
// administrator out. It trusts what is listening (sshd itself, or systemd for the
// socket-activated ssh.socket of Ubuntu 24.04) over what the configuration files say, because
// with socket activation sshd_config's Port is ignored. Without either it reads sshd's effective
// configuration (`sshd -T`, which follows sshd_config.d), then the files, then falls back to 22.
func sshPorts(ctx context.Context) []int {
	var listening []int
	if out, err := exec.CommandContext(ctx, "ss", "-ltnpH").Output(); err == nil {
		listening = append(listening, parseListeningSSHPorts(string(out))...)
	}
	for _, unit := range []string{"ssh.socket", "sshd.socket"} {
		if out, err := exec.CommandContext(ctx, "systemctl", "show", "-p", "Listen", "--value", unit).Output(); err == nil {
			if st, _ := exec.CommandContext(ctx, "systemctl", "is-active", unit).Output(); strings.TrimSpace(string(st)) == "active" {
				listening = append(listening, parseSocketListen(string(out))...)
			}
		}
	}
	if ports := uniqueSorted(listening); len(ports) > 0 {
		return ports
	}
	var configured []int
	if out, err := exec.CommandContext(ctx, "sshd", "-T").Output(); err == nil {
		configured = append(configured, parseConfiguredSSHPorts(string(out))...)
	}
	files := []string{"/etc/ssh/sshd_config"}
	if more, _ := filepath.Glob("/etc/ssh/sshd_config.d/*.conf"); len(more) > 0 {
		files = append(files, more...)
	}
	for _, f := range files {
		if b, err := os.ReadFile(f); err == nil {
			configured = append(configured, parseConfiguredSSHPorts(string(b))...)
		}
	}
	if ports := uniqueSorted(configured); len(ports) > 0 {
		return ports
	}
	return []int{22}
}
