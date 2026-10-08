package mesh

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"time"
)

// Client is a session to one address that is not part of a Manager: the join protocol runs over
// one before the joiner has a certificate, and a node that is starting up asks its peers for their
// epoch over one before its daemon has a mesh. Each Call uses a stream of its own.
type Client struct {
	sess Session
	node string
}

// DialClient connects to addr with cfg (ClientTLS, PinnedTLS). node names the far end in errors.
func DialClient(ctx context.Context, addr, node string, cfg *tls.Config) (*Client, error) {
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
	return &Client{sess: sess, node: node}, nil
}

// Call makes one peer API call; see RPC.Call.
func (c *Client) Call(ctx context.Context, method, path string, in, out any) error {
	st, err := c.sess.OpenStream()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrNoSession, err)
	}
	if err := WriteHeader(st, Header{T: StreamRPC}); err != nil {
		_ = st.Close()
		return fmt.Errorf("%w: %v", ErrNoSession, err)
	}
	return call(ctx, st, c.node, method, path, in, out)
}

// OpenForward opens a forward stream on the session.
func (c *Client) OpenForward(kind Kind, ref string) (net.Conn, error) {
	st, err := c.sess.OpenStream()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoSession, err)
	}
	if err := WriteHeader(st, Header{T: StreamForward, Kind: kind, Ref: ref}); err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("%w: %v", ErrNoSession, err)
	}
	return st, nil
}

// Close ends the session.
func (c *Client) Close() error { return c.sess.Close() }

// Dial implements Dialer for the one node the session reaches, whatever node is named.
func (c *Client) Dial(_ context.Context, _ string, h Header) (net.Conn, error) {
	if err := h.Validate(); err != nil {
		return nil, err
	}
	st, err := c.sess.OpenStream()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoSession, err)
	}
	if err := WriteHeader(st, h); err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("%w: %v", ErrNoSession, err)
	}
	return st, nil
}

var _ Dialer = (*Client)(nil)
