//go:build !linux

package backup

import "net"

// checkRelayPeer accepts every peer: without systemd there are no units to tell apart.
func checkRelayPeer(net.Conn, string) error { return nil }
