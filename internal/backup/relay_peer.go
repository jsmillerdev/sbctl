package backup

import (
	"fmt"
	"net"
	"strings"
)

// Who may use a relay socket
//
// The mount namespace decides which units can reach a project's socket file, but the units
// share one uid, so a process with code execution in any unit can also open the socket
// through /proc/<pid-of-that-project's-postmaster>/root/... The relay therefore checks the
// peer of every connection: the pid from SO_PEERCRED and the systemd unit in
// /proc/<pid>/cgroup. A tenant cannot move itself into another unit's cgroup (the cgroup
// tree is not writable from a unit and the polkit rule gives no access to the system bus).
//
// The check is a deny rule for the units that run tenant code: every sb-* unit is refused
// except sb-postgres@<ref>.service for the socket of <ref> (whose archive_command and
// restore_command are the clients) and the sb-basebackup units (the sbctl binary, which runs
// without tenant code). A process outside every sb-* unit (the daemon, sbctl.service, a
// root or operator shell, a CLI command) is accepted: it can already read the backup
// credentials or the archive directly. Where the cgroup cannot be read the peer is refused.
// On other platforms there are no such units and the check accepts every peer.

// peerUnit returns the systemd unit of a /proc/<pid>/cgroup file: the first path element
// that ends in .service or .scope, "" when the process is in none. It fails when the file
// names no cgroup.
func peerUnit(cgroup string) (string, error) {
	found := false
	for _, line := range strings.Split(cgroup, "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		// cgroup v2 is the line "0::/path"; on v1 the systemd hierarchy is "name=systemd".
		if !(parts[0] == "0" && parts[1] == "") && parts[1] != "name=systemd" {
			continue
		}
		found = true
		for _, el := range strings.Split(parts[2], "/") {
			if strings.HasSuffix(el, ".service") || strings.HasSuffix(el, ".scope") {
				return el, nil
			}
		}
	}
	if !found {
		return "", fmt.Errorf("no cgroup found")
	}
	return "", nil
}

// relayPeerUnitAllowed reports whether a process of the given unit may use the relay
// socket of ref.
func relayPeerUnitAllowed(unit, ref string) bool {
	switch {
	case unit == "sb-postgres@"+ref+".service":
		return true
	case strings.HasPrefix(unit, "sb-basebackup@"), unit == "sb-basebackup-prune.service":
		return true
	case strings.HasPrefix(unit, "sb-"):
		return false
	}
	return true
}

// peerListener refuses the connections of peers that check rejects.
type peerListener struct {
	net.Listener
	check func(net.Conn) error
	deny  func(error)
}

func (l peerListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if err := l.check(c); err != nil {
			l.deny(err)
			c.Close()
			continue
		}
		return c, nil
	}
}
