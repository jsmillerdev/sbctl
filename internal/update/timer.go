package update

import (
	"fmt"
	"strings"
	"time"

	"github.com/supavise/supavise/internal/config"
)

// The unit pair that runs `supavise update run`. deploy/systemd/ holds the files rendered with
// the defaults (a test keeps them in sync); `supavise system install-units` writes the timer with
// the node's [update] settings and enables or disables it.
const (
	ServiceUnit = "supavise-upgrade.service"
	TimerUnit   = "supavise-upgrade.timer"

	// TickStep is the spacing of the timer's runs while the window is open: a refused upgrade is
	// tried again at the next one. A long window gets a wider step, so that the timer never has
	// more than maxTicks runs per window.
	TickStep = 15 * time.Minute
	maxTicks = 16
)

// RenderTimer renders supavise-upgrade.timer for the node's [update] settings.
//
// The timer starts the service when the maintenance window opens and, when something waits for the
// window (auto mode or the reboot for OS patches), every TickStep until it closes. It has no
// release check: the daemon asks GitHub (internal/health). The service decides what each run
// does: the timer never starts an upgrade by itself, and the service does nothing outside the
// window.
func RenderTimer(u config.Update) (string, error) {
	win, err := u.ParsedWindow()
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("[Unit]\nDescription=Supavise maintenance window\n\n[Timer]\n")
	step := TickStep
	if n := int(win.Length()/step) + 1; n > maxTicks {
		step = (win.Length()/maxTicks/TickStep + 1) * TickStep
	}
	ticks := win.Ticks(step)
	if !(u.Mode == config.UpdateAuto || u.RebootsInWindow()) {
		ticks = ticks[:1] // nothing waits for the window: wake once when it opens
	}
	fmt.Fprintf(&b, "# Maintenance window %s (the node's time zone): %d run(s) while it is open.\n", win, len(ticks))
	for _, t := range ticks {
		fmt.Fprintf(&b, "OnCalendar=%s\n", calendar(t))
	}
	b.WriteString("AccuracySec=30s\n\n[Install]\nWantedBy=timers.target\n")
	return b.String(), nil
}

// calendar renders one tick as a systemd OnCalendar expression such as "Mon,Wed *-*-* 04:15:00".
func calendar(t config.Tick) string {
	var days []string
	names := [7]string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"}
	for i := 1; i <= 7; i++ {
		if d := i % 7; t.Days[d] {
			days = append(days, names[d])
		}
	}
	at := fmt.Sprintf("*-*-* %02d:%02d:00", t.Minute/60, t.Minute%60)
	if len(days) == 7 {
		return at
	}
	return strings.Join(days, ",") + " " + at
}
