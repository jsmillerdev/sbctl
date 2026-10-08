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

// muxConfig is smux protocol 2, which has a flow-control window per stream: a stalled stream (a
// replica that stopped reading) cannot hold up the others. The stream window is sized for
// WAL at 20 MB/s over a 100 ms round trip (2 MB in flight) with room to spare; the session
// buffer bounds what all streams together hold in memory.
func muxConfig() *smux.Config {
	return &smux.Config{
		Version:           2,
		KeepAliveInterval: 10 * time.Second,
		KeepAliveTimeout:  30 * time.Second,
		MaxFrameSize:      32768,
		MaxReceiveBuffer:  32 << 20,
		MaxStreamBuffer:   4 << 20,
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
