package failover

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/mesh"
	"github.com/supavise/supavise/internal/registry"
)

// scriptedRemote is a Peers that can also start a move on another node.
type scriptedRemote struct {
	Peers
	startErr  error
	started   []ServerOptions
	statuses  []ServerStatus // answered in turn; the last one repeats
	statusErr error
	asked     []int
}

func (r *scriptedRemote) StartServer(_ context.Context, _ string, o ServerOptions) error {
	r.started = append(r.started, o)
	return r.startErr
}

func (r *scriptedRemote) ServerStatus(_ context.Context, _ string, from int) (ServerStatus, error) {
	r.asked = append(r.asked, from)
	if r.statusErr != nil {
		return ServerStatus{}, r.statusErr
	}
	s := r.statuses[0]
	if len(r.statuses) > 1 {
		r.statuses = r.statuses[1:]
	}
	return s, nil
}

func delegatingLeader(t *testing.T, rem *scriptedRemote) (*world, *Orchestrator) {
	t.Helper()
	w := newWorld(t) // n1 leads and is this node
	return w, w.orch(func(d *Deps) { rem.Peers = d.Peers; d.Peers = rem })
}

func TestTheLeaderHasTheSurvivorRunTheSwitchoverAndFollowsIt(t *testing.T) {
	at := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	rem := &scriptedRemote{statuses: []ServerStatus{
		{State: "running", Steps: []stepJSON{{Name: "begin", At: at}, {Name: "quiesce", At: at, Detail: "3 cluster(s) stopped"}}, Next: 2},
		{State: "running", Next: 2},
		{State: "done", Steps: []stepJSON{{Name: "dns", At: at, Detail: "DNS needs no change"}}, Next: 3,
			Move: &moveJSON{ID: 5, Scope: "server", Kind: "switchover", From: "n1", To: "n2", Epoch: 2, State: "done"}},
	}}
	w, o := delegatingLeader(t, rem)
	var seen []string
	ctx := WithProgress(w.ctx, func(s registry.MoveStep) { seen = append(seen, s.Name) })
	mv, err := o.FailoverServer(ctx, ServerOptions{To: "n2", Force: true, RestoreMissing: true})
	if err != nil {
		t.Fatal(err)
	}
	if mv == nil || mv.ID != 5 || mv.State != registry.MoveDone || mv.ToNode != "n2" {
		t.Fatalf("move: %+v", mv)
	}
	if len(rem.started) != 1 || !rem.started[0].Force || !rem.started[0].RestoreMissing || rem.started[0].Resume {
		t.Fatalf("started %+v", rem.started)
	}
	if strings.Join(seen, ",") != "delegated,begin,quiesce,dns" {
		t.Fatalf("progress %v", seen)
	}
	// It follows from where it stopped reading.
	if len(rem.asked) != 3 || rem.asked[0] != 0 || rem.asked[1] != 2 || rem.asked[2] != 2 {
		t.Fatalf("asked from %v", rem.asked)
	}
	// This node did none of the work: its registry stops partway and the survivor's takes over.
	for _, e := range []string{"quiesce", "stop ", "local.stop", "provider.", "marker", "promote", "registry.CreateMove"} {
		w.assertNever(e)
	}
}

func TestDelegationRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		opts ServerOptions
		mut  func(w *world)
		want string
	}{
		"resume goes where the move is": {opts: ServerOptions{To: "n2", Resume: true}, mut: func(w *world) {
			// The move that runs on n2 is in the registry the leader replicates.
			must(w.t, w.reg.CreateMove(w.ctx, &registry.Move{Scope: registry.MoveServer, Kind: registry.MoveSwitchover, FromNode: "n1", ToNode: "n2", Epoch: 2}))
		}, want: "--resume on standby"},
		"a failover is not delegated": {opts: ServerOptions{To: "n2", OldPrimaryIsDown: true}, want: "failover runs on the node that takes over"},
		"a follower cannot ask": {opts: ServerOptions{To: "n2"}, mut: func(w *world) {
			w.addNode3()
			w.setSelf("n3", false)
		}, want: "runs on the node that takes over"},
	} {
		t.Run(name, func(t *testing.T) {
			rem := &scriptedRemote{statuses: []ServerStatus{{State: "done"}}}
			w, o := delegatingLeader(t, rem)
			if tc.mut != nil {
				tc.mut(w)
				o = w.orch(func(d *Deps) { rem.Peers = d.Peers; d.Peers = rem })
			}
			_, err := o.FailoverServer(w.ctx, tc.opts)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error: %v", err)
			}
			if len(rem.started) != 0 {
				t.Fatal("the survivor was asked")
			}
		})
	}
}

func TestDelegationFailureModes(t *testing.T) {
	t.Run("the survivor refuses", func(t *testing.T) {
		rem := &scriptedRemote{startErr: &mesh.RemoteError{Node: "n2", Status: 409, Message: "failover: refused:\n  - replica of aaaa: lag 45s"}}
		w, o := delegatingLeader(t, rem)
		_, err := o.FailoverServer(w.ctx, ServerOptions{To: "n2"})
		if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "lag 45s") || !strings.Contains(err.Error(), "standby") {
			t.Fatalf("error: %v", err)
		}
	})
	t.Run("the survivor cannot be reached", func(t *testing.T) {
		rem := &scriptedRemote{startErr: mesh.ErrNoSession}
		w, o := delegatingLeader(t, rem)
		if _, err := o.FailoverServer(w.ctx, ServerOptions{To: "n2"}); !errors.Is(err, mesh.ErrNoSession) {
			t.Fatalf("error: %v", err)
		}
	})
	t.Run("the move fails there", func(t *testing.T) {
		rem := &scriptedRemote{statuses: []ServerStatus{{State: "failed", Error: "1 step(s) did not finish: project aaaa", Move: &moveJSON{ID: 6, State: "failed", To: "n2"}}}}
		w, o := delegatingLeader(t, rem)
		mv, err := o.FailoverServer(w.ctx, ServerOptions{To: "n2"})
		if err == nil || !strings.Contains(err.Error(), "did not finish") || mv == nil || mv.State != registry.MoveFailed {
			t.Fatalf("move %+v, error %v", mv, err)
		}
	})
	t.Run("the survivor forgot the move", func(t *testing.T) {
		rem := &scriptedRemote{statuses: []ServerStatus{{State: "idle"}}}
		w, o := delegatingLeader(t, rem)
		if _, err := o.FailoverServer(w.ctx, ServerOptions{To: "n2"}); err == nil || !strings.Contains(err.Error(), "does not run the switchover") {
			t.Fatalf("error: %v", err)
		}
	})
	t.Run("contact is lost while it runs", func(t *testing.T) {
		rem := &scriptedRemote{statusErr: mesh.ErrNoSession}
		w, o := delegatingLeader(t, rem)
		_, err := o.FailoverServer(w.ctx, ServerOptions{To: "n2"})
		if err == nil || !strings.Contains(err.Error(), "goes on with the switchover") {
			t.Fatalf("error: %v", err)
		}
		if len(rem.asked) != delegationPatience {
			t.Fatalf("looked %d times, want %d", len(rem.asked), delegationPatience)
		}
	})
}

// ---- the survivor's side ----

// pollServer asks for the status until the move is over.
func pollServer(t *testing.T, o *Orchestrator, peer string) ServerStatus {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var st ServerStatus
		rec := serve(t, o, "GET "+PathServer, PathServer+"?from=0", peer, nil, &st)
		if rec.Code != http.StatusOK {
			t.Fatalf("status: %d %s", rec.Code, rec.Body)
		}
		if st.State != "running" {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatal("the delegated move did not end")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestTheSurvivorRunsTheSwitchoverTheLeaderAskedFor(t *testing.T) {
	w := serverWorld(t) // this node is n2; n1 leads and answers
	o := w.orch()
	rec := serve(t, o, "POST "+PathServer, PathServer, "n1", serverReq{To: "n2", Force: false}, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}
	st := pollServer(t, o, "n1")
	if st.State != "done" || st.Move == nil || st.Move.State != "done" || st.Move.To != "n2" || st.Move.Kind != "switchover" {
		t.Fatalf("status: %+v", st)
	}
	var names []string
	for _, s := range st.Steps {
		names = append(names, s.Name)
	}
	if !strings.Contains(strings.Join(names, ","), "quiesce") || !strings.Contains(strings.Join(names, ","), "dns") {
		t.Fatalf("steps: %v", names)
	}
	if cl := clusterOf(t, w); cl.Leader != "n2" {
		t.Fatalf("cluster: %+v", cl)
	}
	// Only the node that asked may follow.
	if rec := serve(t, o, "GET "+PathServer, PathServer, "n3", nil, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("another node: %d", rec.Code)
	}
	// From the middle: only the steps since.
	var tail ServerStatus
	serve(t, o, "GET "+PathServer, PathServer+"?from="+strconv.Itoa(len(st.Steps)-1), "n1", nil, &tail)
	if len(tail.Steps) != 1 || tail.Next != len(st.Steps) {
		t.Fatalf("tail: %+v", tail)
	}
}

func TestTheSurvivorRefusesWhatItCannotRun(t *testing.T) {
	t.Run("a precondition fails", func(t *testing.T) {
		w := serverWorld(t)
		w.cfg.Fleet.StorageBackend = "file"
		o := w.orch()
		rec := serve(t, o, "POST "+PathServer, PathServer, "n1", serverReq{To: "n2"}, nil)
		e := errorOf(rec)
		if rec.Code != http.StatusConflict || e.Code != "refused" || !strings.Contains(e.Message, "storage_backend") {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		w.assertNever("quiesce")
	})
	t.Run("the leader does not answer: that is a failover, run here", func(t *testing.T) {
		w := serverWorld(t)
		w.down["n1"] = true
		rec := serve(t, w.orch(), "POST "+PathServer, PathServer, "n1", serverReq{To: "n2"}, nil)
		if rec.Code != http.StatusConflict || errorOf(rec).Code != "not_planned" {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
	})
	t.Run("only the leader asks", func(t *testing.T) {
		w := serverWorld(t)
		if rec := serve(t, w.orch(), "POST "+PathServer, PathServer, "n3", serverReq{}, nil); rec.Code != http.StatusForbidden {
			t.Fatalf("%d", rec.Code)
		}
		if rec := serve(t, w.orch(), "POST "+PathServer, PathServer, "", serverReq{}, nil); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%d", rec.Code)
		}
	})
	t.Run("a move is running", func(t *testing.T) {
		w := serverWorld(t)
		o := w.orch()
		release, _ := o.acquire()
		defer release()
		if rec := serve(t, o, "POST "+PathServer, PathServer, "n1", serverReq{}, nil); rec.Code != http.StatusConflict || errorOf(rec).Code != "busy" {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
	})
	t.Run("nothing was asked yet", func(t *testing.T) {
		w := serverWorld(t)
		var st ServerStatus
		serve(t, w.orch(), "GET "+PathServer, PathServer, "n1", nil, &st)
		if st.State != "idle" {
			t.Fatalf("status: %+v", st)
		}
	})
}
