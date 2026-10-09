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

// The windows of the session of a caller with no certificate (a joiner). The join exchange is a few
// KiB each way, and a stranger must not be able to make the daemon hold more than this per session.
const (
	anonSessionBuffer = 1 << 20
	anonStreamWindow  = 256 << 10
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

// anonMuxConfig is muxConfig with the small windows of a caller with no certificate.
func anonMuxConfig() *smux.Config {
	c := muxConfig()
	c.MaxReceiveBuffer, c.MaxStreamBuffer = anonSessionBuffer, anonStreamWindow
	return c
}

type session struct{ s *smux.Session }

// NewSession multiplexes conn, usually the TLS connection of a handshake. client is the side that
// dialed the TCP connection; the roles only keep the two sides' stream numbers apart, and both
// sides may open and accept streams. Closing the Session closes conn.
func NewSession(conn net.Conn, client bool) (Session, error) {
	return newSession(conn, client, muxConfig())
}

// newAnonSession is the accepting side of a session with a caller that presented no certificate.
func newAnonSession(conn net.Conn) (Session, error) {
	return newSession(conn, false, anonMuxConfig())
}

func newSession(conn net.Conn, client bool, cfg *smux.Config) (Session, error) {
	var s *smux.Session
	var err error
	if client {
		s, err = smux.Client(conn, cfg)
	} else {
		s, err = smux.Server(conn, cfg)
	}
	if err != nil {
		return nil, err
	}
	return session{s}, nil
}

// ownedWrites hands smux a buffer of its own for every write. smux queues the caller's slice for its
// send loop and returns from Write, with an error, when the session dies or the write times out before
// the loop has sent it; the loop then still reads the slice while the caller (net/http's bufio writer, an
// io.Copy buffer) reuses it. That is a data race and, on the wire, the wrong bytes in a frame of a session
// that is already lost. TestForwardingSurvivesASessionLoss found it under -race.
type ownedWrites struct{ net.Conn }

func (o ownedWrites) Write(p []byte) (int, error) {
	b := make([]byte, len(p))
	copy(b, p)
	return o.Conn.Write(b)
}

func (m session) OpenStream() (net.Conn, error) {
	st, err := m.s.OpenStream()
	if err != nil {
		return nil, err
	}
	return ownedWrites{st}, nil
}

func (m session) AcceptStream() (net.Conn, error) {
	st, err := m.s.AcceptStream()
	if err != nil {
		return nil, err
	}
	return ownedWrites{st}, nil
}

func (m session) Close() error               { return m.s.Close() }
func (m session) IsClosed() bool             { return m.s.IsClosed() }
func (m session) CloseChan() <-chan struct{} { return m.s.CloseChan() }
func (m session) NumStreams() int            { return m.s.NumStreams() }
