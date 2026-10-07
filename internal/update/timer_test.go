package update

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsmillerdev/supavise/internal/config"
)

func lines(s, prefix string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.HasPrefix(l, prefix) {
			out = append(out, strings.TrimPrefix(l, prefix))
		}
	}
	return out
}

func TestRenderTimerDefaults(t *testing.T) {
	got, err := RenderTimer(config.DefaultUpdate())
	if err != nil {
		t.Fatal(err)
	}
	// The release check belongs to the daemon: the timer holds the window and nothing else.
	if strings.Contains(got, "OnBootSec") || strings.Contains(got, "OnUnitActiveSec") {
		t.Errorf("the timer has a release check:\n%s", got)
	}
	// Notify mode on a node with no managed OS updates: the window only needs one wake-up.
	if v := lines(got, "OnCalendar="); len(v) != 1 || v[0] != "Sun *-*-* 04:00:00" {
		t.Errorf("OnCalendar: %v", v)
	}
	if !strings.Contains(got, "[Install]\nWantedBy=timers.target") {
		t.Error("the timer must install into timers.target")
	}
}

// The default timer that ships in deploy/systemd is what a node without its own settings gets, so
// install-units must find it unchanged.
func TestShippedTimerMatchesTheDefaultRendering(t *testing.T) {
	want, err := RenderTimer(config.DefaultUpdate())
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join("..", "..", "deploy", "systemd", TimerUnit))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("deploy/systemd/%s differs from RenderTimer(DefaultUpdate()):\n--- shipped\n%s--- rendered\n%s", TimerUnit, got, want)
	}
}

func TestRenderTimerAutoModeTicksThroughTheWindow(t *testing.T) {
	u := config.DefaultUpdate()
	u.Mode, u.Window = config.UpdateAuto, "Sun 03:00-05:00"
	got, err := RenderTimer(u)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Sun *-*-* 03:00:00", "Sun *-*-* 03:15:00", "Sun *-*-* 03:30:00", "Sun *-*-* 03:45:00",
		"Sun *-*-* 04:00:00", "Sun *-*-* 04:15:00", "Sun *-*-* 04:30:00", "Sun *-*-* 04:45:00"}
	if v := lines(got, "OnCalendar="); strings.Join(v, "|") != strings.Join(want, "|") {
		t.Errorf("OnCalendar: %v, want %v", v, want)
	}
}

func TestRenderTimerManyDaysAndMidnight(t *testing.T) {
	u := config.DefaultUpdate()
	u.Mode, u.Window = config.UpdateAuto, "Fri,Sat 23:30-00:30"
	got, err := RenderTimer(u)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Fri,Sat *-*-* 23:30:00", "Fri,Sat *-*-* 23:45:00", "Sat,Sun *-*-* 00:00:00", "Sat,Sun *-*-* 00:15:00"}
	if v := lines(got, "OnCalendar="); strings.Join(v, "|") != strings.Join(want, "|") {
		t.Errorf("OnCalendar: %v, want %v", v, want)
	}
	u.Window = "daily 03:00-04:00"
	got, _ = RenderTimer(u)
	if v := lines(got, "OnCalendar="); v[0] != "*-*-* 03:00:00" {
		t.Errorf("a daily window needs no weekday: %v", v)
	}
}

func TestRenderTimerCapsTheRunsOfALongWindow(t *testing.T) {
	u := config.DefaultUpdate()
	u.Mode, u.Window = config.UpdateAuto, "daily 00:00-23:00"
	got, err := RenderTimer(u)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(lines(got, "OnCalendar=")); n > maxTicks || n < 2 {
		t.Errorf("a 23h window got %d runs, want 2 to %d", n, maxTicks)
	}
}

func TestRenderTimerRebootOnlyNode(t *testing.T) {
	u := config.DefaultUpdate()
	u.OSSecurityUpdates = true // notify mode, but the node reboots itself in the window
	got, _ := RenderTimer(u)
	if n := len(lines(got, "OnCalendar=")); n != 8 {
		t.Errorf("a node that reboots in the window needs a run per tick: got %d", n)
	}
}

func TestRenderTimerRefusesBadSettings(t *testing.T) {
	u := config.DefaultUpdate()
	u.Window = "never"
	if _, err := RenderTimer(u); err == nil {
		t.Error("a bad window must not become a unit file")
	}
}
