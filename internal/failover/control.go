package failover

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/registry"
)

// The control socket lets the CLI drive the orchestrator that lives in the daemon. A move needs the
// daemon: its mesh sessions, its plane, its registry (read-only on a follower until the move
// promotes it), its shared services. The CLI is therefore a client: it asks for a plan, prints it,
// asks the operator, and follows the run, which goes on in the daemon if the CLI goes away
// (--resume continues it). The socket is HTTP over a unix socket, mode 0600, in a directory of mode
// 0700: only the daemon's user and root reach it, and it authorizes a failover with no other check.
// It carries no secrets.
//
//	GET  /v1/readiness      the failover-readiness block (404 on a node that is not in a cluster)
//	POST /v1/plan/project   ProjectOptions in, a Plan out
//	POST /v1/plan/server    ServerOptions in, a Plan out
//	POST /v1/run/project    ProjectOptions in, a stream of events (one JSON object per line)
//	POST /v1/run/server     ServerOptions in, a stream of events
//	GET  /v1/follow         ?from=N&epoch=E: the move the daemon continued after it restarted, or runs
//	                        for a leader, else the server move of epoch E in the registry (a
//	                        ServerStatus, the steps from index N on)

// maxSocketPath is the longest unix socket path Linux and macOS accept, less a margin.
const maxSocketPath = 100

// ControlSocket is where the daemon of cfg listens for the CLI.
func ControlSocket(cfg *config.Config) string {
	return filepath.Join(cfg.Paths().System("failover"), "control.sock")
}

// projectReq and serverReq are the wire forms of the options.
type projectReq struct {
	Ref        string `json:"ref"`
	To         string `json:"to,omitempty"`
	Force      bool   `json:"force,omitempty"`
	DryRun     bool   `json:"dry_run,omitempty"`
	Resume     bool   `json:"resume,omitempty"`
	ExpectKind string `json:"expect_kind,omitempty"`
}

type serverReq struct {
	To               string `json:"to,omitempty"`
	Force            bool   `json:"force,omitempty"`
	DryRun           bool   `json:"dry_run,omitempty"`
	Resume           bool   `json:"resume,omitempty"`
	RestoreMissing   bool   `json:"restore_missing,omitempty"`
	OldPrimaryIsDown bool   `json:"old_primary_is_down,omitempty"`
	Abort            bool   `json:"abort,omitempty"`
	ExpectKind       string `json:"expect_kind,omitempty"`
	ExpectEpoch      int64  `json:"expect_epoch,omitempty"`
}

func (r projectReq) options() ProjectOptions {
	return ProjectOptions{Ref: r.Ref, To: r.To, Force: r.Force, DryRun: r.DryRun, Resume: r.Resume, ExpectKind: r.ExpectKind}
}

func (r serverReq) options() ServerOptions {
	return ServerOptions{To: r.To, Force: r.Force, DryRun: r.DryRun, Resume: r.Resume, RestoreMissing: r.RestoreMissing, OldPrimaryIsDown: r.OldPrimaryIsDown, Yes: true,
		Abort: r.Abort, ExpectKind: r.ExpectKind, ExpectEpoch: r.ExpectEpoch}
}

// stepJSON, moveJSON and event are what the stream carries.
type stepJSON struct {
	Name   string    `json:"name"`
	At     time.Time `json:"at"`
	Detail string    `json:"detail,omitempty"`
}

type moveJSON struct {
	ID    int64      `json:"id"`
	Scope string     `json:"scope"`
	Kind  string     `json:"kind"`
	Ref   string     `json:"ref,omitempty"`
	From  string     `json:"from"`
	To    string     `json:"to"`
	Epoch int64      `json:"epoch"`
	State string     `json:"state"`
	Error string     `json:"error,omitempty"`
	Steps []stepJSON `json:"steps"`
}

type event struct {
	Step  *stepJSON `json:"step,omitempty"`
	Move  *moveJSON `json:"move,omitempty"`
	Error string    `json:"error,omitempty"`
	// Code names the kind of error: "refused", "busy", "nothing_to_resume", "plan_changed", "no_cluster",
	// "restarting" (the daemon restarts in its new role and continues the move) or "failed".
	Code   string  `json:"code,omitempty"`
	Checks []Check `json:"checks,omitempty"`
	// Move is set with an error too when the move got as far as being recorded.
}

func toMoveJSON(m *registry.Move) *moveJSON {
	if m == nil {
		return nil
	}
	j := &moveJSON{ID: m.ID, Scope: string(m.Scope), Kind: string(m.Kind), Ref: m.Ref, From: m.FromNode, To: m.ToNode, Epoch: m.Epoch, State: string(m.State), Error: m.Error}
	for _, s := range m.Steps {
		j.Steps = append(j.Steps, stepJSON{Name: s.Name, At: s.At, Detail: s.Detail})
	}
	return j
}

func (j *moveJSON) move() *registry.Move {
	if j == nil {
		return nil
	}
	m := &registry.Move{ID: j.ID, Scope: registry.MoveScope(j.Scope), Kind: registry.MoveKind(j.Kind), Ref: j.Ref, FromNode: j.From, ToNode: j.To, Epoch: j.Epoch, State: registry.MoveState(j.State), Error: j.Error}
	for _, s := range j.Steps {
		m.Steps = append(m.Steps, registry.MoveStep{Name: s.Name, At: s.At, Detail: s.Detail})
	}
	return m
}

// Follower is what a Service implements when it keeps the run it continued after its daemon restarted
// (*Orchestrator does): the control socket serves it at /v1/follow.
type Follower interface {
	Follow(ctx context.Context, from int, epoch int64) ServerStatus
}

// ControlServer serves the control socket for a Service.
type ControlServer struct {
	Svc Service
	Log *slog.Logger
}

// Handler is the HTTP handler of the control socket. A run takes ctx, the daemon's, and not the
// request's: a CLI that goes away does not stop a move, and a daemon that stops interrupts it
// between steps, which leaves it failed and resumable.
func (s *ControlServer) Handler(ctx context.Context) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/readiness", func(w http.ResponseWriter, r *http.Request) {
		rd, err := s.Svc.Readiness(r.Context())
		switch {
		case errors.Is(err, ErrNoCluster):
			writeControl(w, http.StatusNotFound, event{Error: err.Error(), Code: "no_cluster"})
		case err != nil:
			writeControl(w, http.StatusInternalServerError, event{Error: err.Error(), Code: "failed"})
		default:
			writeControl(w, http.StatusOK, rd)
		}
	})
	mux.HandleFunc("GET /v1/follow", func(w http.ResponseWriter, r *http.Request) {
		f, ok := s.Svc.(Follower)
		if !ok {
			writeControl(w, http.StatusOK, ServerStatus{State: "idle"})
			return
		}
		from, _ := strconv.Atoi(r.URL.Query().Get("from"))
		epoch, _ := strconv.ParseInt(r.URL.Query().Get("epoch"), 10, 64)
		writeControl(w, http.StatusOK, f.Follow(r.Context(), from, epoch))
	})
	mux.HandleFunc("POST /v1/plan/project", func(w http.ResponseWriter, r *http.Request) {
		var req projectReq
		if !decodeControl(w, r, &req) {
			return
		}
		pl, err := s.Svc.PlanProject(r.Context(), req.options())
		s.answerPlan(w, pl, err)
	})
	mux.HandleFunc("POST /v1/plan/server", func(w http.ResponseWriter, r *http.Request) {
		var req serverReq
		if !decodeControl(w, r, &req) {
			return
		}
		pl, err := s.Svc.PlanServer(r.Context(), req.options())
		s.answerPlan(w, pl, err)
	})
	mux.HandleFunc("POST /v1/run/project", func(w http.ResponseWriter, r *http.Request) {
		var req projectReq
		if !decodeControl(w, r, &req) {
			return
		}
		s.run(ctx, w, r, func(ctx context.Context) (*registry.Move, error) { return s.Svc.FailoverProject(ctx, req.options()) })
	})
	mux.HandleFunc("POST /v1/run/server", func(w http.ResponseWriter, r *http.Request) {
		var req serverReq
		if !decodeControl(w, r, &req) {
			return
		}
		s.run(ctx, w, r, func(ctx context.Context) (*registry.Move, error) { return s.Svc.FailoverServer(ctx, req.options()) })
	})
	return mux
}

func (s *ControlServer) answerPlan(w http.ResponseWriter, pl *Plan, err error) {
	if err != nil {
		writeControl(w, http.StatusInternalServerError, errorEvent(err))
		return
	}
	writeControl(w, http.StatusOK, pl)
}

func decodeControl(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeControl(w, http.StatusBadRequest, event{Error: "the request is not valid: " + err.Error(), Code: "bad_request"})
		return false
	}
	return true
}

func writeControl(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// errorEvent words an error of the orchestrator for the wire.
func errorEvent(err error) event {
	ev := event{Error: err.Error(), Code: "failed"}
	var re *RefusedError
	switch {
	case errors.As(err, &re):
		ev.Code, ev.Checks = "refused", re.Checks
	case errors.Is(err, ErrBusy):
		ev.Code = "busy"
	case errors.Is(err, ErrNothingToResume):
		ev.Code = "nothing_to_resume"
	case errors.Is(err, ErrPlanChanged):
		ev.Code = "plan_changed"
	case errors.Is(err, ErrNoCluster):
		ev.Code = "no_cluster"
	}
	return ev
}

// run starts a move on a goroutine of the daemon and streams its steps until it ends.
func (s *ControlServer) run(base context.Context, w http.ResponseWriter, r *http.Request, do func(context.Context) (*registry.Move, error)) {
	// The buffer is for a client that reads slower than the move records. A step that finds it full
	// is not reported: it is in the log, and a move must not wait on a terminal.
	steps := make(chan registry.MoveStep, 256)
	type result struct {
		mv  *registry.Move
		err error
	}
	done := make(chan result, 1)
	gone := r.Context().Done()
	ctx := WithProgress(base, func(st registry.MoveStep) {
		select {
		case steps <- st:
		default:
		}
	})
	go func() {
		mv, err := do(ctx)
		done <- result{mv, err}
	}()

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	enc := json.NewEncoder(w)
	emit := func(ev event) {
		_ = enc.Encode(ev)
		if fl != nil {
			fl.Flush()
		}
	}
	for {
		select {
		case st := <-steps:
			emit(event{Step: &stepJSON{Name: st.Name, At: st.At, Detail: st.Detail}})
		case res := <-done:
			for drained := false; !drained; { // the steps recorded just before the end
				select {
				case st := <-steps:
					emit(event{Step: &stepJSON{Name: st.Name, At: st.At, Detail: st.Detail}})
				default:
					drained = true
				}
			}
			ev := event{Move: toMoveJSON(res.mv)}
			if res.err != nil {
				e := errorEvent(res.err)
				ev.Error, ev.Code, ev.Checks = e.Error, e.Code, e.Checks
			}
			emit(ev)
			return
		case <-gone:
			return
		}
	}
}

// Serve listens on the socket at path until ctx ends.
func (s *ControlServer) Serve(ctx context.Context, path string) error {
	if len(path) > maxSocketPath {
		return fmt.Errorf("failover: the control socket path %q is longer than %d bytes", path, maxSocketPath)
	}
	// The directory is closed to everyone else before the socket exists in it, so the socket is not
	// reachable in the moment between Listen and Chmod.
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("failover: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("failover: %w", err)
	}
	_ = os.Remove(path) // a socket the last daemon left
	ln, err := net.Listen("unix", path)
	if err != nil {
		return fmt.Errorf("failover: listening on %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return fmt.Errorf("failover: %w", err)
	}
	srv := &http.Server{Handler: s.Handler(ctx), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
		_ = os.Remove(path)
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Client is the CLI's side of the control socket.
type Client struct {
	// Path is the socket (ControlSocket).
	Path string
}

// RemoteError is an error the daemon reported.
type RemoteError struct {
	Code    string
	Message string
	Checks  []Check
}

func (e *RemoteError) Error() string { return e.Message }

// Is maps the daemon's codes back to the package's errors.
func (e *RemoteError) Is(target error) bool {
	switch e.Code {
	case "refused":
		return target == ErrRefused
	case "busy":
		return target == ErrBusy
	case "nothing_to_resume":
		return target == ErrNothingToResume
	case "plan_changed":
		return target == ErrPlanChanged
	case "no_cluster":
		return target == ErrNoCluster
	case "restarting":
		return target == ErrRestarting
	}
	return false
}

func (c Client) http() *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", c.Path)
		},
	}}
}

func (c Client) do(ctx context.Context, method, path string, in any) (*http.Response, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://failover"+path, body)
	if err != nil {
		return nil, err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http().Do(req)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("failover: there is no control socket at %s: this server is not part of a cluster, or supavise is not running on it: %w", c.Path, err)
	case errors.Is(err, fs.ErrPermission):
		return nil, fmt.Errorf("failover: the control socket %s is for root and the user supavise runs as: run this command as one of them: %w", c.Path, err)
	case err != nil:
		return nil, fmt.Errorf("failover: the daemon's control socket %s does not answer (is supavise running on this node?): %w", c.Path, err)
	}
	return resp, nil
}

func (c Client) call(ctx context.Context, method, path string, in, out any) error {
	resp, err := c.do(ctx, method, path, in)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var ev event
		_ = json.NewDecoder(resp.Body).Decode(&ev)
		return &RemoteError{Code: ev.Code, Message: ev.Error, Checks: ev.Checks}
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Readiness asks for the failover-readiness block. ErrNoCluster when the node is not in a cluster.
func (c Client) Readiness(ctx context.Context) (Readiness, error) {
	var r Readiness
	err := c.call(ctx, http.MethodGet, "/v1/readiness", nil, &r)
	return r, err
}

func (c Client) PlanProject(ctx context.Context, o ProjectOptions) (*Plan, error) {
	var pl Plan
	if err := c.call(ctx, http.MethodPost, "/v1/plan/project", projectReq{Ref: o.Ref, To: o.To, Force: o.Force, Resume: o.Resume}, &pl); err != nil {
		return nil, err
	}
	return &pl, nil
}

func (c Client) PlanServer(ctx context.Context, o ServerOptions) (*Plan, error) {
	var pl Plan
	if err := c.call(ctx, http.MethodPost, "/v1/plan/server", serverFromOptions(o), &pl); err != nil {
		return nil, err
	}
	return &pl, nil
}

func serverFromOptions(o ServerOptions) serverReq {
	return serverReq{To: o.To, Force: o.Force, Resume: o.Resume, RestoreMissing: o.RestoreMissing, OldPrimaryIsDown: o.OldPrimaryIsDown,
		Abort: o.Abort, ExpectKind: o.ExpectKind, ExpectEpoch: o.ExpectEpoch}
}

// RunProject starts the move and calls onStep for each step as the daemon records it. The move is
// returned with the error when it got as far as being recorded.
func (c Client) RunProject(ctx context.Context, o ProjectOptions, onStep func(registry.MoveStep)) (*registry.Move, error) {
	return c.stream(ctx, "/v1/run/project", projectReq{Ref: o.Ref, To: o.To, Force: o.Force, Resume: o.Resume, ExpectKind: o.ExpectKind}, onStep)
}

func (c Client) RunServer(ctx context.Context, o ServerOptions, onStep func(registry.MoveStep)) (*registry.Move, error) {
	return c.stream(ctx, "/v1/run/server", serverFromOptions(o), onStep)
}

func (c Client) stream(ctx context.Context, path string, in any, onStep func(registry.MoveStep)) (*registry.Move, error) {
	resp, err := c.do(ctx, http.MethodPost, path, in)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var ev event
		_ = json.NewDecoder(resp.Body).Decode(&ev)
		return nil, &RemoteError{Code: ev.Code, Message: ev.Error, Checks: ev.Checks}
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		var ev event
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			return nil, fmt.Errorf("failover: the daemon sent a line that is not an event: %w", err)
		}
		switch {
		case ev.Step != nil:
			if onStep != nil {
				onStep(registry.MoveStep{Name: ev.Step.Name, At: ev.Step.At, Detail: ev.Step.Detail})
			}
		case ev.Error != "":
			return ev.Move.move(), &RemoteError{Code: ev.Code, Message: ev.Error, Checks: ev.Checks}
		case ev.Move != nil:
			return ev.Move.move(), nil
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("%w; a move that was cut off by the daemon's restart is continued by the daemon that starts, and Follow shows it; any other goes on in the daemon, follow it with supavise failover --resume or the moves log", ErrStreamClosed)
}

// followPatience is how long Follow waits for a daemon that is restarting to answer again, and
// followPoll how often it looks.
var (
	followPatience = 5 * time.Minute
	followPoll     = 2 * time.Second
)

// Follow shows a server move of epoch after the connection that ran it was cut by the daemon's
// restart in its new role: it calls onStep for each step the daemon that starts records (the node that
// leads now continues the move; a node that follows it reads the log from the registry) and returns
// the move when it ends, or an error when it failed. It waits for the daemon to answer first.
// ErrNothingRunning when the daemon has no move to show.
func (c Client) Follow(ctx context.Context, epoch int64, onStep func(registry.MoveStep)) (*registry.Move, error) {
	from, deadline := 0, time.Now().Add(followPatience)
	var lastErr error
	var idleSince time.Time
	for {
		var st ServerStatus
		err := c.call(ctx, http.MethodGet, "/v1/follow?from="+strconv.Itoa(from)+"&epoch="+strconv.FormatInt(epoch, 10), nil, &st)
		switch {
		case ctx.Err() != nil:
			return nil, ctx.Err()
		case err != nil:
			lastErr = err
			if time.Now().After(deadline) {
				return nil, fmt.Errorf("failover: the daemon did not answer again within %s: %w", followPatience, lastErr)
			}
		default:
			for _, s := range st.Steps {
				if onStep != nil {
					onStep(registry.MoveStep{Name: s.Name, At: s.At, Detail: s.Detail})
				}
			}
			from = st.Next
			if st.State != "idle" {
				idleSince = time.Time{}
			}
			switch st.State {
			case "done":
				return st.Move.move(), nil
			case "failed", "aborted":
				return st.Move.move(), &RemoteError{Code: "failed", Message: st.Error}
			case "idle":
				// A daemon that is up and runs nothing yet may be about to pick the move up (it waits a
				// moment after it starts); one that stays idle has none.
				if idleSince.IsZero() {
					idleSince = time.Now()
				}
				if time.Since(idleSince) > resumeSettle+3*followPoll {
					return nil, ErrNothingRunning
				}
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(followPoll):
		}
	}
}
