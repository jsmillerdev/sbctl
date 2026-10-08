package nodeupgrade

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// eventLog records the events of a run, and into the host's call list too, so a test can say where
// in the sequence one came.
type eventLog struct {
	h   *fakeHost
	got []Event
}

func (l *eventLog) notify(_ context.Context, ev Event) {
	l.got = append(l.got, ev)
	l.h.rec("event %s", ev.Kind)
}

func (l *eventLog) kinds() string {
	var k []string
	for _, e := range l.got {
		k = append(k, e.Kind)
	}
	return strings.Join(k, ",")
}

func withEvents(h *fakeHost, o Options) (Options, *eventLog) {
	l := &eventLog{h: h}
	o.Notify = l.notify
	return o, l
}

func TestUpgradeEventsStartAfterThePreparationAndEndWithTheOutcome(t *testing.T) {
	h := newFakeHost()
	o, l := withEvents(h, runOpts(h))
	o.Unattended = true
	h.moves = []ProjectMove{{Ref: "aaaaaaaaaaaaaaaaaaaa"}, {Ref: "bbbbbbbbbbbbbbbbbbbb"}}
	if err := Run(context.Background(), h, o); err != nil {
		t.Fatal(err)
	}
	if l.kinds() != EventStarted+","+EventSucceeded {
		t.Fatalf("events = %s", l.kinds())
	}
	// Nothing was told while the run could still be refused with nothing changed.
	order := h.order()
	if !(strings.Index(order, "backup ") < strings.Index(order, "event started") && strings.Index(order, "event started") < strings.Index(order, "install ")) {
		t.Fatalf("started comes between the backups and the swap: %s", order)
	}
	if strings.Index(order, "status") > strings.Index(order, "event succeeded") {
		t.Fatalf("succeeded comes before the verdict: %s", order)
	}
	st, ok := l.got[0], l.got[1]
	if st.From != "v1.0.0" || st.To != "v1.1.0" || !st.Unattended || st.Rollback || st.Projects != 2 {
		t.Fatalf("started = %+v", st)
	}
	if ok.From != "v1.0.0" || ok.To != "v1.1.0" || ok.Projects != 2 || !ok.Unattended {
		t.Fatalf("succeeded = %+v", ok)
	}
}

// A refusal raises no event unless the operator was already told the node was about to change.
func TestRefusalsRaiseNoEvent(t *testing.T) {
	for name, set := range map[string]func(*fakeHost, *Options){
		"check":           func(h *fakeHost, o *Options) { o.Check = true },
		"plan":            func(h *fakeHost, o *Options) { o.Plan = true },
		"a failed fetch":  func(h *fakeHost, o *Options) { h.prefetchErr = errBoom },
		"a failed backup": func(h *fakeHost, o *Options) { h.backupErr = errBoom },
		"a gate":          func(h *fakeHost, o *Options) { h.node.Verdict = VerdictDown },
		"before the swap": func(h *fakeHost, o *Options) { h.installErr = errBoom },
		"nothing to do": func(h *fakeHost, o *Options) {
			h.tag, h.node.Version, h.node.BinaryInfo = "v1.1.0", "v1.1.0", newInfo()
			h.node.Pins = newPins()
			h.node.Projects = nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newFakeHost()
			o, l := withEvents(h, runOpts(h))
			set(h, &o)
			_ = Run(context.Background(), h, o)
			// A refusal after EventStarted (the install failed before the swap) ends it with EventRefused.
			if name == "before the swap" {
				if l.kinds() != EventStarted+","+EventRefused || l.got[1].Cause == "" {
					t.Fatalf("events = %s", l.kinds())
				}
				return
			}
			if len(l.got) != 0 {
				t.Fatalf("events = %s", l.kinds())
			}
		})
	}
}

func TestRolledBackUpgradeSaysWhereItHaltedAndWhatRuns(t *testing.T) {
	h := newFakeHost()
	h.projectsErr = errors.New("project bbbbbbbbbbbbbbbbbbbb failed")
	h.halted = "bbbbbbbbbbbbbbbbbbbb"
	h.moves = []ProjectMove{{Ref: "aaaaaaaaaaaaaaaaaaaa", From: map[string]string{"gotrue": authOld}, To: map[string]string{"gotrue": authNew}}}
	o, l := withEvents(h, runOpts(h))
	err := Run(context.Background(), h, o)
	if code(t, err) != ExitRolledBack {
		t.Fatalf("err = %v", err)
	}
	if l.kinds() != EventStarted+","+EventRolledBack {
		t.Fatalf("events = %s", l.kinds())
	}
	f := l.got[1]
	if f.From != "v1.0.0" || f.To != "v1.1.0" || f.BackTo != "v1.0.0" || f.Halted != "bbbbbbbbbbbbbbbbbbbb" || !strings.Contains(f.Cause, "rollout of the projects stopped") {
		t.Fatalf("rolled back = %+v", f)
	}
	// The event comes after the node is back.
	order := h.order()
	if strings.Index(order, "restore v1.0.0") > strings.Index(order, "event rolled_back") {
		t.Fatalf("order: %s", order)
	}
}

func TestDaemonThatDoesNotComeUpRaisesRolledBack(t *testing.T) {
	h := newFakeHost()
	h.installErr, h.installSwaps = errors.New("did not answer"), true
	o, l := withEvents(h, runOpts(h))
	if err := Run(context.Background(), h, o); code(t, err) != ExitRolledBack {
		t.Fatalf("err = %v", err)
	}
	if l.kinds() != EventStarted+","+EventRolledBack || l.got[1].Halted != "" || l.got[1].BackTo != "v1.0.0" {
		t.Fatalf("events = %s %+v", l.kinds(), l.got)
	}
}

func TestUpgradeThatNeedsTheOperatorRaisesIt(t *testing.T) {
	h := newFakeHost()
	h.projectsErr = errBoom
	h.halted = "aaaaaaaaaaaaaaaaaaaa"
	h.applied = migsV2 // the registry rule stops the rollback
	h.info.RegistryMigrations = migsV2
	o, l := withEvents(h, runOpts(h))
	if err := Run(context.Background(), h, o); code(t, err) != ExitNeedsOperator {
		t.Fatalf("err = %v", err)
	}
	if l.kinds() != EventStarted+","+EventNeedsOperator {
		t.Fatalf("events = %s", l.kinds())
	}
	f := l.got[1]
	if f.Halted != "aaaaaaaaaaaaaaaaaaaa" || !strings.Contains(f.Cause, "restore the system cluster from its pre-upgrade base backup") || !strings.Contains(f.Cause, "the upgrade failed because") {
		t.Fatalf("needs operator = %+v", f)
	}
}

func TestUpgradeThatKeepsTheBinaryRaisesTheSameEvents(t *testing.T) {
	same := func(h *fakeHost) {
		h.tag, h.node.Version, h.node.BinaryInfo = "v1.1.0", "v1.1.0", newInfo()
	}
	t.Run("rolled back", func(t *testing.T) {
		h := newFakeHost()
		same(h)
		h.projectsErr = errBoom
		o, l := withEvents(h, runOpts(h))
		if err := Run(context.Background(), h, o); code(t, err) != ExitRolledBack {
			t.Fatalf("err = %v", err)
		}
		if l.kinds() != EventStarted+","+EventRolledBack || l.got[1].BackTo != "v1.1.0" {
			t.Fatalf("events = %s %+v", l.kinds(), l.got)
		}
	})
	t.Run("a shared service", func(t *testing.T) {
		h := newFakeHost()
		same(h)
		h.sharedErr = errBoom
		o, l := withEvents(h, runOpts(h))
		if err := Run(context.Background(), h, o); code(t, err) != ExitNeedsOperator {
			t.Fatalf("err = %v", err)
		}
		if l.kinds() != EventStarted+","+EventNeedsOperator {
			t.Fatalf("events = %s", l.kinds())
		}
	})
}

func TestRollbackEvents(t *testing.T) {
	t.Run("succeeded", func(t *testing.T) {
		h := rollbackHost()
		o, l := withEvents(h, runOpts(h))
		if err := Rollback(context.Background(), h, o); err != nil {
			t.Fatal(err)
		}
		if l.kinds() != EventStarted+","+EventSucceeded {
			t.Fatalf("events = %s", l.kinds())
		}
		for _, e := range l.got {
			if !e.Rollback || e.From != "v1.1.0" || e.To != "v1.0.0" || e.Projects != 2 {
				t.Fatalf("event = %+v", e)
			}
		}
	})
	t.Run("failed", func(t *testing.T) {
		h := rollbackHost()
		h.restoreErr = errors.New("the units did not render")
		o, l := withEvents(h, runOpts(h))
		if err := Rollback(context.Background(), h, o); code(t, err) != ExitNeedsOperator {
			t.Fatalf("err = %v", err)
		}
		if l.kinds() != EventStarted+","+EventNeedsOperator || !l.got[1].Rollback || !strings.Contains(l.got[1].Cause, "the units did not render") {
			t.Fatalf("events = %s %+v", l.kinds(), l.got)
		}
	})
	t.Run("refused", func(t *testing.T) {
		h := rollbackHost()
		h.applied = migsV2
		o, l := withEvents(h, runOpts(h))
		if err := Rollback(context.Background(), h, o); code(t, err) != ExitRefused {
			t.Fatalf("err = %v", err)
		}
		if len(l.got) != 0 {
			t.Fatalf("events = %s", l.kinds())
		}
	})
}
