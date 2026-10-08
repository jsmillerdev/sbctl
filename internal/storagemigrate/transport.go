package storagemigrate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"
)

const (
	// paceBlock is how many bytes of a request's body are paid for at once: small enough for the
	// limit to hold within a fraction of a second, big enough not to sleep for every write.
	paceBlock = 64 << 10
	// stallAfter is how long a request may go without moving a byte before it is ended. The SDK
	// tries a request that ended this way again, up to its attempts.
	stallAfter = 2 * time.Minute
)

// errStalled ends a request whose connection stopped moving data.
var errStalled = errors.New("the connection to the bucket stopped moving data")

// pacedTransport carries the S3 client's requests. It holds back the bytes a request sends (the rate
// limit of the copy, charged where the bytes leave: the SDK reads a body once to sign it over plain
// http and again to send it) and ends a request that has stopped moving data, which the SDK's own
// client would wait for forever.
type pacedTransport struct {
	next  http.RoundTripper
	lim   atomic.Pointer[limiter]
	stall time.Duration
}

// RoundTrip implements http.RoundTripper.
func (t *pacedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithCancelCause(req.Context())
	g := &watchdog{ctx: ctx, d: t.stall}
	g.timer = time.AfterFunc(t.stall, func() { cancel(errStalled) })
	lim := t.lim.Load()
	out := req.Clone(ctx)
	if req.Body != nil && req.Body != http.NoBody {
		out.Body = g.wrap(newPacedBody(ctx, req.Body, lim, req.ContentLength))
		if gb := req.GetBody; gb != nil {
			out.GetBody = func() (io.ReadCloser, error) {
				b, err := gb()
				if err != nil {
					return nil, err
				}
				return g.wrap(newPacedBody(ctx, b, lim, req.ContentLength)), nil
			}
		}
	}
	resp, err := t.next.RoundTrip(out)
	if err != nil {
		g.timer.Stop()
		err = g.explain(err)
		cancel(nil)
		return nil, err
	}
	g.timer.Reset(t.stall)
	resp.Body = &guardedResponse{ReadCloser: g.wrap(resp.Body), cancel: func() { g.timer.Stop(); cancel(nil) }}
	return resp, nil
}

// watchdog ends a request that moves no data for d: every read of a body is progress.
type watchdog struct {
	ctx   context.Context
	d     time.Duration
	timer *time.Timer
}

func (g *watchdog) wrap(rc io.ReadCloser) io.ReadCloser { return &watchedBody{ReadCloser: rc, g: g} }

// explain names the stall when it is what ended the request.
func (g *watchdog) explain(err error) error {
	if err != nil && errors.Is(context.Cause(g.ctx), errStalled) {
		return fmt.Errorf("%w for %s", errStalled, g.d)
	}
	return err
}

type watchedBody struct {
	io.ReadCloser
	g *watchdog
}

func (b *watchedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.g.timer.Reset(b.g.d)
	}
	return n, b.g.explain(err)
}

// guardedResponse ends the request's context and timer when the caller is done with the response.
type guardedResponse struct {
	io.ReadCloser
	cancel func()
}

func (r *guardedResponse) Close() error {
	err := r.ReadCloser.Close()
	r.cancel()
	return err
}

// pacedBody asks the limiter for a block of its bytes before it hands any out.
type pacedBody struct {
	io.ReadCloser
	ctx    context.Context
	lim    *limiter
	known  bool  // the request's length is known
	left   int64 // bytes of it still to be paid for
	credit int64 // bytes paid for and not yet read
}

func newPacedBody(ctx context.Context, rc io.ReadCloser, lim *limiter, length int64) *pacedBody {
	return &pacedBody{ReadCloser: rc, ctx: ctx, lim: lim, known: length > 0, left: length}
}

func (b *pacedBody) Read(p []byte) (int, error) {
	if b.credit <= 0 {
		n := int64(paceBlock)
		if b.known {
			if b.left <= 0 {
				return b.ReadCloser.Read(p) // everything is paid for: what is left to read is the end
			}
			n = min(n, b.left)
		}
		if err := b.lim.wait(b.ctx, n); err != nil {
			return 0, err
		}
		b.credit = n
	}
	if int64(len(p)) > b.credit {
		p = p[:b.credit]
	}
	n, err := b.ReadCloser.Read(p)
	b.credit -= int64(n)
	if b.known {
		b.left -= int64(n)
	}
	return n, err
}
