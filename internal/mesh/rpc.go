package mesh

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/supavise/supavise/internal/mesh/peerapi"
)

// maxRPCBody bounds a request or an answer of the peer API. The largest is the certificate
// snapshot; 64 MiB is far above any of them and still bounds what a peer can make this node read.
const maxRPCBody = 64 << 20

// Dial implements Dialer.
func (m *Manager) Dial(ctx context.Context, node string, h Header) (net.Conn, error) {
	if err := h.Validate(); err != nil {
		return nil, err
	}
	if node == m.self() {
		return nil, fmt.Errorf("%w: node %s is this node", ErrNoSession, node)
	}
	var last error
	for attempt := 0; attempt < 2; attempt++ {
		pc, err := m.ensure(ctx, node)
		if err != nil {
			return nil, err
		}
		st, err := pc.sess.OpenStream()
		if err == nil {
			if err = WriteHeader(st, h); err == nil {
				return st, nil
			}
			_ = st.Close()
		}
		last = err
		_ = pc.sess.Close() // the session is dead; the next attempt opens another
	}
	return nil, fmt.Errorf("%w: %v", ErrNoSession, last)
}

// Call implements RPC.
func (m *Manager) Call(ctx context.Context, node, method, path string, in, out any) error {
	st, err := m.Dial(ctx, node, Header{T: StreamRPC})
	if err != nil {
		return err
	}
	return call(ctx, st, node, method, path, in, out)
}

// callOn is Call over the session pc, not whichever session the node has now.
func (m *Manager) callOn(ctx context.Context, pc *peerConn, method, path string, in, out any) error {
	st, err := pc.sess.OpenStream()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrNoSession, err)
	}
	if err := WriteHeader(st, Header{T: StreamRPC}); err != nil {
		_ = st.Close()
		return fmt.Errorf("%w: %v", ErrNoSession, err)
	}
	return call(ctx, st, pc.node, method, path, in, out)
}

// call sends one request on an rpc stream and reads its answer. It closes st.
func call(ctx context.Context, st net.Conn, node, method, path string, in, out any) error {
	defer st.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = st.SetDeadline(dl)
	}
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = st.Close()
		case <-stop:
		}
	}()
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://"+node+path, body)
	if err != nil {
		return err
	}
	req.Close = true
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if err := req.Write(st); err != nil {
		return transportErr(ctx, node, err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(st), req)
	if err != nil {
		if errors.Is(err, io.EOF) && ctx.Err() == nil {
			return fmt.Errorf("%w: node %s closed the stream without answering", ErrRefused, node)
		}
		return transportErr(ctx, node, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxRPCBody+1))
	if err != nil {
		return transportErr(ctx, node, err)
	}
	if len(data) > maxRPCBody {
		return fmt.Errorf("mesh: node %s answered more than %d bytes", node, maxRPCBody)
	}
	if resp.StatusCode/100 != 2 {
		var pe peerapi.Error
		if json.Unmarshal(data, &pe) != nil || pe.Message == "" {
			pe = peerapi.Error{Message: strings.TrimSpace(string(data)), Code: pe.Code}
		}
		if pe.Message == "" {
			pe.Message = resp.Status
		}
		return &RemoteError{Node: node, Status: resp.StatusCode, Message: pe.Message, Code: pe.Code}
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("mesh: the answer of node %s is not valid: %w", node, err)
		}
	}
	return nil
}

func transportErr(ctx context.Context, node string, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return fmt.Errorf("%w: talking to node %s: %v", ErrNoSession, node, err)
}

// serveRPC is the handler of the peer API server: it applies the request policy and hands the
// request to the Mux.
func (m *Manager) serveRPC(w http.ResponseWriter, r *http.Request) {
	peer, _ := PeerFrom(r.Context())
	if ok, why := rpcAllowed(peer, r.URL.Path); !ok {
		RespondError(w, http.StatusForbidden, CodeRefused, why)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRPCBody)
	m.o.Mux.ServeHTTP(w, r)
}

// ResetDefaultMux replaces DefaultMux with an empty Mux. The daemon calls it before its wire hooks
// register their endpoints, so that a process that starts the daemon more than once (tests do)
// registers each endpoint once; http.ServeMux panics on a pattern registered twice.
func ResetDefaultMux() { DefaultMux = NewMux() }

// RespondJSON writes v as the JSON body of a peer API answer. A nil v writes 204 with no body.
func RespondJSON(w http.ResponseWriter, status int, v any) {
	if v == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// RespondError writes a non-2xx peer API answer: a peerapi.Error with the message and a code the
// caller can act on (may be empty).
func RespondError(w http.ResponseWriter, status int, code, message string) {
	RespondJSON(w, status, peerapi.Error{Message: message, Code: code})
}

// DecodeBody reads the JSON body of a request into v. A body that is not JSON of that shape is
// answered with 400 and false is returned.
func DecodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		RespondError(w, http.StatusBadRequest, "bad_request", "the request body is not valid: "+err.Error())
		return false
	}
	return true
}

// RequireLeader wraps fn so that it runs only for a request from the leader. The endpoints that the
// peer API marks "leader to node" use it, so that a follower cannot drive another follower.
func RequireLeader(t Topology, fn HandlerFunc) HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		peer, _ := PeerFrom(r.Context())
		if lead, ok := t.Leader(); !ok || lead.ID != peer.Node {
			RespondError(w, http.StatusForbidden, "not_leader", "only the leader may call this endpoint")
			return
		}
		fn(w, r)
	}
}

// PingHandler answers the ping with info(): the node, the epoch, the leader, the version, the
// schema and the node's health. The daemon registers it for "GET /peer/v1/ping".
func PingHandler(info func() peerapi.Ping) HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := info()
		p.Time = time.Now().UTC()
		RespondJSON(w, http.StatusOK, p)
	}
}
