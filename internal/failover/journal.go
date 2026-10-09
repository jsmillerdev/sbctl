package failover

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/supavise/supavise/internal/fsutil"
	"github.com/supavise/supavise/internal/registry"
)

// journal is the log of one move: the steps that finished, in order. A step is recorded after it
// has been done, and a step that is recorded is not done again, so a move that stopped (a crash,
// a failed step) continues with --resume from the first step that is not in the log. Every
// step is written so that doing it twice is harmless as well, because a crash can fall between
// the work and its record.
//
// A project move logs to the registry's moves table. A server move cannot: the survivor's
// registry is a read-only standby until it has promoted its system cluster. It logs to
// failover.json until then and adopts the log into a moves row at the promotion (adopt).
type journal struct {
	o *Orchestrator

	mu   sync.Mutex
	move registry.Move
	// file is failover.json while the move logs to it; empty once it logs to the registry.
	file string
	// opts are the flags of a server move, kept in the file so that a resume runs the same move.
	opts serverFlags
	// err and state are the outcome while the move logs to the file.
	state registry.MoveState
	err   string
}

// serverFlags are the options of a server move that a resume must find again.
type serverFlags struct {
	Force            bool `json:"force,omitempty"`
	RestoreMissing   bool `json:"restore_missing,omitempty"`
	OldPrimaryIsDown bool `json:"old_primary_is_down,omitempty"`
}

// stateFile is the failover.json of a server move that has not promoted its system cluster yet.
type stateFile struct {
	Version   int                 `json:"version"`
	Kind      registry.MoveKind   `json:"kind"`
	From      string              `json:"from"`
	To        string              `json:"to"`
	Epoch     int64               `json:"epoch"`
	Flags     serverFlags         `json:"flags"`
	StartedAt time.Time           `json:"started_at"`
	State     registry.MoveState  `json:"state"`
	Error     string              `json:"error,omitempty"`
	Steps     []registry.MoveStep `json:"steps"`
}

const stateVersion = 1

// has reports whether the step was recorded.
func (j *journal) has(step string) bool {
	_, ok := j.lookup(step)
	return ok
}

// detail returns the detail the step was recorded with.
func (j *journal) detail(step string) string {
	d, _ := j.lookup(step)
	return d
}

func (j *journal) lookup(step string) (string, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, s := range j.move.Steps {
		if s.Name == step {
			return s.Detail, true
		}
	}
	return "", false
}

// withPrefix lists the details of the recorded steps whose name starts with prefix, by the rest of the name.
func (j *journal) withPrefix(prefix string) map[string]string {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := map[string]string{}
	for _, s := range j.move.Steps {
		if rest, ok := strings.CutPrefix(s.Name, prefix); ok {
			out[rest] = s.Detail
		}
	}
	return out
}

// record writes a finished step down and reports it. A step recorded again keeps its first record.
func (j *journal) record(ctx context.Context, name, detail string) error {
	j.mu.Lock()
	for _, s := range j.move.Steps {
		if s.Name == name {
			j.mu.Unlock()
			return nil
		}
	}
	s := registry.MoveStep{Name: name, At: j.o.d.Now().UTC(), Detail: detail}
	j.move.Steps = append(j.move.Steps, s)
	var err error
	if j.file != "" {
		err = j.writeFileLocked()
	} else {
		err = j.o.store().AppendMoveStep(ctx, j.move.ID, s)
	}
	j.mu.Unlock()
	if err != nil {
		return fmt.Errorf("failover: recording step %s: %w", name, err)
	}
	j.o.d.Log.Info("failover step", "move", j.label(), "step", name, "detail", detail)
	reportStep(ctx, s)
	return nil
}

func (j *journal) label() string {
	if j.move.ID != 0 {
		return fmt.Sprintf("%d", j.move.ID)
	}
	return string(j.move.Scope) + ":" + string(j.move.Kind)
}

// finish ends the move with a final state. A move that failed can be finished again after a resume.
func (j *journal) finish(ctx context.Context, state registry.MoveState, errText string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.file != "" {
		if state == registry.MoveAborted { // undone: nothing is left to resume
			if err := os.Remove(j.file); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			return nil
		}
		j.state, j.err = state, errText
		return j.writeFileLocked()
	}
	return j.o.store().FinishMove(context.WithoutCancel(ctx), j.move.ID, state, errText)
}

func (j *journal) snapshot() registry.Move {
	j.mu.Lock()
	defer j.mu.Unlock()
	m := j.move
	m.Steps = append([]registry.MoveStep(nil), j.move.Steps...)
	return m
}

// adopt moves the log from failover.json into a row of the moves table, step by step with the
// times they were recorded at, and removes the file. The registry must accept writes (the system
// cluster is promoted). Called again after a crash between the two, it finds the row it made.
func (j *journal) adopt(ctx context.Context) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.file == "" {
		return nil
	}
	st := j.o.store()
	mv := registry.Move{Scope: j.move.Scope, Kind: j.move.Kind, FromNode: j.move.FromNode, ToNode: j.move.ToNode, Epoch: j.move.Epoch}
	if prior, err := findServerMove(ctx, st, mv.ToNode, mv.Epoch); err != nil {
		return err
	} else if prior != nil {
		mv = *prior
	} else if err := st.CreateMove(ctx, &mv); err != nil {
		return fmt.Errorf("failover: creating the move: %w", err)
	}
	have := map[string]bool{}
	for _, s := range mv.Steps {
		have[s.Name] = true
	}
	for _, s := range j.move.Steps {
		if have[s.Name] {
			continue
		}
		if err := st.AppendMoveStep(ctx, mv.ID, s); err != nil {
			return fmt.Errorf("failover: copying step %s to the registry: %w", s.Name, err)
		}
	}
	j.move.ID = mv.ID
	file := j.file
	j.file = ""
	if err := os.Remove(file); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("failover: removing %s: %w", file, err)
	}
	return nil
}

func (j *journal) writeFileLocked() error {
	st := stateFile{
		Version: stateVersion, Kind: j.move.Kind, From: j.move.FromNode, To: j.move.ToNode, Epoch: j.move.Epoch,
		Flags: j.opts, StartedAt: j.move.StartedAt, State: j.state, Error: j.err, Steps: j.move.Steps,
	}
	if st.State == "" {
		st.State = registry.MoveRunning
	}
	return writeJSONFile(j.file, st)
}

// writeJSONFile writes v to path, 0600, through a temporary file and a rename, so that a reader
// sees the old file or the new one.
func writeJSONFile(path string, v any) error {
	return fsutil.WriteJSON(path, v, 0o600, fsutil.Options{Sync: true, MkdirMode: 0o750})
}

// readStateFile returns nil, nil when there is no file.
func readStateFile(path string) (*stateFile, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var st stateFile
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("failover: %s is not a state file: %w", path, err)
	}
	if st.Version != stateVersion {
		return nil, fmt.Errorf("failover: %s has version %d, this release reads %d", path, st.Version, stateVersion)
	}
	return &st, nil
}

// newestMove returns the newest move that pred accepts, or nil. The registry lists moves newest first.
func newestMove(ctx context.Context, st Store, pred func(*registry.Move) bool) (*registry.Move, error) {
	ms, err := st.ListMoves(ctx, "", 100)
	if err != nil {
		return nil, fmt.Errorf("failover: listing moves: %w", err)
	}
	for i := range ms {
		m := ms[i]
		if pred(&m) {
			return &m, nil
		}
	}
	return nil, nil
}

// unfinished passes on m from newestMove when it is running or failed, and answers nil for one that
// is done or aborted.
func unfinished(m *registry.Move, err error) (*registry.Move, error) {
	if err != nil || m == nil || m.State == registry.MoveRunning || m.State == registry.MoveFailed {
		return m, err
	}
	return nil, nil
}

// findServerMove returns the newest server move to the node at the epoch, or nil. A server move
// that was adopted into the registry carries its epoch, which no other move of that node has.
func findServerMove(ctx context.Context, st Store, to string, epoch int64) (*registry.Move, error) {
	return newestMove(ctx, st, func(m *registry.Move) bool {
		return m.Scope == registry.MoveServer && m.ToNode == to && m.Epoch == epoch
	})
}

// unfinishedProjectMove returns the newest move of the project when it is not done or aborted, or nil.
func unfinishedProjectMove(ctx context.Context, st Store, ref string) (*registry.Move, error) {
	return unfinished(newestMove(ctx, st, func(m *registry.Move) bool {
		return m.Scope == registry.MoveProject && m.Ref == ref
	}))
}

// unfinishedServerMove returns the newest server move that is running or failed.
func unfinishedServerMove(ctx context.Context, st Store) (*registry.Move, error) {
	return unfinished(newestMove(ctx, st, func(m *registry.Move) bool { return m.Scope == registry.MoveServer }))
}

// journalFor wraps a registry move that already exists.
func (o *Orchestrator) journalFor(m registry.Move) *journal {
	return &journal{o: o, move: m}
}

// newProjectJournal creates the moves row of a project move.
func (o *Orchestrator) newProjectJournal(ctx context.Context, m registry.Move) (*journal, error) {
	if err := o.store().CreateMove(ctx, &m); err != nil {
		return nil, fmt.Errorf("failover: creating the move: %w", err)
	}
	return o.journalFor(m), nil
}

// fileJournal starts or loads the log of a server move in failover.json.
func (o *Orchestrator) fileJournal(m registry.Move, flags serverFlags, steps []registry.MoveStep) *journal {
	m.Steps = steps
	if m.StartedAt.IsZero() {
		m.StartedAt = o.d.Now().UTC()
	}
	return &journal{o: o, move: m, file: o.d.Cfg.Paths().FailoverState(), opts: flags}
}
