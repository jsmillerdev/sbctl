package mesh

import (
	"net"
	"time"

	"github.com/xtaci/smux"
)

// Session is one multiplexed connection to a peer node. Either side opens streams, and
// every stream starts with a Header. The multiplexer is xtaci/smux (MIT); this file is the only
// place that names it, so that a replacement changes nothing else.
type Session interface {
	// OpenStream opens a stream to the peer.
	OpenStream() (net.Conn, error)
	// AcceptStream waits for a stream the peer opened.
	AcceptStream() (net.Conn, error)
	// Close ends the session and every stream on it.
	Close() error
	IsClosed() bool
	// CloseChan is closed when the session ends, by Close or because the connection or the
	// keepalive failed.
	CloseChan() <-chan struct{}
	NumStreams() int
}

// The multiplexer's windows and timers, from spike S6 (physical replication through a TLS and
// smux forwarder). The stream window and the session buffer are what keeps the lag of a standby
// at 0.10 to 0.41 s at 20 MB/s over 2 to 70 ms of round trip: with smaller ones the sender waits
// for window updates and the lag grows with the round trip. The receiver's setting governs, so
// both ends of a session use the same. The keepalive timeout stays below wal_receiver_timeout
// (60 s), so that a dead session is noticed, and the standby's receiver retried, before Postgres
// gives up on the connection.
const (
	// StreamWindow is the flow-control window of one stream (smux MaxStreamBuffer): at least 4 MiB.
	StreamWindow = 4 << 20
	// SessionBuffer bounds what all streams of a session hold in memory (smux MaxReceiveBuffer): at
	// least 16 MiB. It is 32 MiB, so that several busy streams keep their windows.
	SessionBuffer = 32 << 20
	// KeepAliveInterval and KeepAliveTimeout: a session with no frame for the timeout is closed.
	KeepAliveInterval = 10 * time.Second
	KeepAliveTimeout  = 30 * time.Second
	// frameSize is the largest frame smux sends.
	frameSize = 32768
)

// muxConfig is smux protocol 2, which has a flow-control window per stream: a stalled stream (a
// replica that stopped reading) cannot hold up the others.
func muxConfig() *smux.Config {
	return &smux.Config{
		Version:           2,
		KeepAliveInterval: KeepAliveInterval,
		KeepAliveTimeout:  KeepAliveTimeout,
		MaxFrameSize:      frameSize,
		MaxReceiveBuffer:  SessionBuffer,
		MaxStreamBuffer:   StreamWindow,
	}
}

type session struct{ s *smux.Session }

// NewSession multiplexes conn, usually the TLS connection of a handshake. client is the side that
// dialed the TCP connection; the roles only keep the two sides' stream numbers apart, and both
// sides may open and accept streams. Closing the Session closes conn.
func NewSession(conn net.Conn, client bool) (Session, error) {
	var s *smux.Session
	var err error
	if client {
		s, err = smux.Client(conn, muxConfig())
	} else {
		s, err = smux.Server(conn, muxConfig())
	}
	if err != nil {
		return nil, err
	}
	return session{s}, nil
}

func (m session) OpenStream() (net.Conn, error) {
	st, err := m.s.OpenStream()
	if err != nil {
		return nil, err
	}
	return st, nil
}

func (m session) AcceptStream() (net.Conn, error) {
	st, err := m.s.AcceptStream()
	if err != nil {
		return nil, err
	}
	return st, nil
}

func (m session) Close() error               { return m.s.Close() }
func (m session) IsClosed() bool             { return m.s.IsClosed() }
func (m session) CloseChan() <-chan struct{} { return m.s.CloseChan() }
func (m session) NumStreams() int            { return m.s.NumStreams() }
