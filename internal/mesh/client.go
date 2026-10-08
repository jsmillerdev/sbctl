package mesh

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"sync"
	"time"
)

// Client is a session to one address that is not part of a Manager: the join protocol runs over
// one before the joiner has a certificate, and a node that is starting up asks its peers for their
// epoch over one before its daemon has a mesh. Each Call uses a stream of its own. A session that
// ended (the far end restarted, a NAT forgot the connection) is replaced by a new one to the same
// address the next time a stream is wanted.
type Client struct {
	addr string
	cfg  *tls.Config
	node string

	mu     sync.Mutex
	sess   Session
	closed bool
}

// DialClient connects to addr with cfg (ClientTLS, PinnedTLS). node names the far end in errors.
func DialClient(ctx context.Context, addr, node string, cfg *tls.Config) (*Client, error) {
	sess, err := dialSession(ctx, addr, cfg)
	if err != nil {
		return nil, err
	}
	return &Client{addr: addr, cfg: cfg, node: node, sess: sess}, nil
}

func dialSession(ctx context.Context, addr string, cfg *tls.Config) (Session, error) {
	d := net.Dialer{Timeout: 5 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	tc := tls.Client(conn, cfg)
	hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := tc.HandshakeContext(hctx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("handshake with %s: %w", addr, err)
	}
	sess, err := NewSession(tc, true)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return sess, nil
}

// session returns the open session, dialing a new one when the last has ended.
func (c *Client) session(ctx context.Context) (Session, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, fmt.Errorf("%w: the client is closed", ErrNoSession)
	}
	if c.sess != nil && !c.sess.IsClosed() {
		return c.sess, nil
	}
	sess, err := dialSession(ctx, c.addr, c.cfg)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoSession, err)
	}
	c.sess = sess
	return sess, nil
}

// open opens a stream that starts with h. A session that fails to open one is closed and replaced
// once.
func (c *Client) open(ctx context.Context, h Header) (net.Conn, error) {
	var last error
	for attempt := 0; attempt < 2; attempt++ {
		sess, err := c.session(ctx)
		if err != nil {
			return nil, err
		}
		st, err := sess.OpenStream()
		if err == nil {
			if err = WriteHeader(st, h); err == nil {
				return st, nil
			}
			_ = st.Close()
		}
		last = err
		_ = sess.Close()
	}
	return nil, fmt.Errorf("%w: %v", ErrNoSession, last)
}

// Call makes one peer API call; see RPC.Call.
func (c *Client) Call(ctx context.Context, method, path string, in, out any) error {
	st, err := c.open(ctx, Header{T: StreamRPC})
	if err != nil {
		return err
	}
	return call(ctx, st, c.node, method, path, in, out)
}

// OpenForward opens a forward stream on the session.
func (c *Client) OpenForward(kind Kind, ref string) (net.Conn, error) {
	return c.open(context.Background(), Header{T: StreamForward, Kind: kind, Ref: ref})
}

// Close ends the session. The client opens no other afterwards.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	if c.sess == nil {
		return nil
	}
	return c.sess.Close()
}

// Dial implements Dialer for the one node the session reaches, whatever node is named.
func (c *Client) Dial(ctx context.Context, _ string, h Header) (net.Conn, error) {
	if err := h.Validate(); err != nil {
		return nil, err
	}
	return c.open(ctx, h)
}

var _ Dialer = (*Client)(nil)
