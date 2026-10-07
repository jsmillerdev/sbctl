package config

import (
	"fmt"
	"strings"
	"time"
)

// Window is a weekly maintenance window in the node's time zone: the days it opens on and the
// time of day it opens and closes. It is the value of update.window.
//
// The text form is "[DAYS ]HH:MM-HH:MM":
//
//	Sun 03:00-05:00        Sundays, 03:00 to 05:00
//	Sat,Sun 02:00-04:00    a comma-separated list of days
//	Mon-Fri 01:30-03:30    a range of days (Fri-Mon wraps over the weekend)
//	daily 03:00-04:00      every day; leaving the days out means the same
//	Sun 23:00-01:00        a window that ends after midnight belongs to the day it opens on
//
// Days are Mon Tue Wed Thu Fri Sat Sun (any case; full names are accepted). Times are 24-hour.
// "The node's time zone" is the zone the Go runtime and systemd read from /etc/localtime, which
// is UTC on most cloud images.
type Window struct {
	Days [7]bool // indexed by time.Weekday
	// Start and End are minutes after midnight. End <= Start means the window closes the next day.
	Start, End int
}

var dayNames = [7]string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"}

// ParseWindow reads the text form of a Window.
func ParseWindow(s string) (Window, error) {
	var w Window
	fields := strings.Fields(s)
	var days, span string
	switch len(fields) {
	case 1:
		days, span = "daily", fields[0]
	case 2:
		days, span = fields[0], fields[1]
	default:
		return w, fmt.Errorf("window %q: want [DAYS ]HH:MM-HH:MM, for example \"Sun 03:00-05:00\"", s)
	}
	from, to, ok := strings.Cut(span, "-")
	if !ok {
		return w, fmt.Errorf("window %q: want a time range HH:MM-HH:MM, for example \"Sun 03:00-05:00\"", s)
	}
	var err error
	if w.Start, err = parseClock(from); err != nil {
		return w, fmt.Errorf("window %q: %w", s, err)
	}
	if w.End, err = parseClock(to); err != nil {
		return w, fmt.Errorf("window %q: %w", s, err)
	}
	if w.Start == w.End {
		return w, fmt.Errorf("window %q: it opens and closes at the same time", s)
	}
	if err := w.parseDays(days); err != nil {
		return w, fmt.Errorf("window %q: %w", s, err)
	}
	return w, nil
}

func parseClock(s string) (int, error) {
	h, m, ok := strings.Cut(s, ":")
	if !ok || len(h) != 2 || len(m) != 2 {
		return 0, fmt.Errorf("time %q: want HH:MM", s)
	}
	hh, mm := atoi2(h), atoi2(m)
	if hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return 0, fmt.Errorf("time %q is not a time of day (00:00 to 23:59)", s)
	}
	return hh*60 + mm, nil
}

func atoi2(s string) int {
	if s[0] < '0' || s[0] > '9' || s[1] < '0' || s[1] > '9' {
		return -1
	}
	return int(s[0]-'0')*10 + int(s[1]-'0')
}

func parseDay(s string) (int, bool) {
	s = strings.ToLower(s)
	for i, n := range dayNames {
		full := strings.ToLower(time.Weekday(i).String())
		if s == strings.ToLower(n) || s == full {
			return i, true
		}
	}
	return 0, false
}

func (w *Window) parseDays(s string) error {
	if strings.EqualFold(s, "daily") {
		for i := range w.Days {
			w.Days[i] = true
		}
		return nil
	}
	for _, part := range strings.Split(s, ",") {
		a, b, isRange := strings.Cut(part, "-")
		from, ok := parseDay(a)
		if !ok {
			return fmt.Errorf("%q is not a day (Mon Tue Wed Thu Fri Sat Sun, or daily)", a)
		}
		to := from
		if isRange {
			if to, ok = parseDay(b); !ok {
				return fmt.Errorf("%q is not a day (Mon Tue Wed Thu Fri Sat Sun, or daily)", b)
			}
		}
		for d := from; ; d = (d + 1) % 7 {
			w.Days[d] = true
			if d == to {
				break
			}
		}
	}
	return nil
}

// String is the canonical text form: days in week order starting Monday ("Mon,Wed" or "daily"),
// then the times.
func (w Window) String() string {
	return fmt.Sprintf("%s %02d:%02d-%02d:%02d", w.dayList(","), w.Start/60, w.Start%60, w.End/60, w.End%60)
}

// dayList names the days the window opens on, in week order from Monday, joined by sep; "daily"
// when it opens every day.
func (w Window) dayList(sep string) string {
	var names []string
	for i := 1; i <= 7; i++ {
		if d := i % 7; w.Days[d] {
			names = append(names, dayNames[d])
		}
	}
	if len(names) == 7 {
		return "daily"
	}
	return strings.Join(names, sep)
}

// Length is how long the window stays open.
func (w Window) Length() time.Duration {
	n := w.End - w.Start
	if n <= 0 {
		n += 24 * 60
	}
	return time.Duration(n) * time.Minute
}

// opening returns the opening time of the occurrence that starts on the calendar day of t's
// location named by day offset off (0 = t's day, -1 = the day before), and whether the window
// opens on that weekday.
func (w Window) opening(t time.Time, off int) (time.Time, bool) {
	y, m, d := t.Date()
	day := time.Date(y, m, d+off, 0, 0, 0, 0, t.Location())
	if !w.Days[day.Weekday()] {
		return time.Time{}, false
	}
	return time.Date(day.Year(), day.Month(), day.Day(), w.Start/60, w.Start%60, 0, 0, t.Location()), true
}

func (w Window) closing(open time.Time) time.Time {
	endDay := 0
	if w.End <= w.Start {
		endDay = 1
	}
	return time.Date(open.Year(), open.Month(), open.Day()+endDay, w.End/60, w.End%60, 0, 0, open.Location())
}

// Occurrence returns the opening and closing time of the occurrence of the window that is open at
// t, evaluated in t's time zone. ok is false when the window is closed at t.
func (w Window) Occurrence(t time.Time) (start, end time.Time, ok bool) {
	for _, off := range []int{0, -1} {
		open, opens := w.opening(t, off)
		if !opens {
			continue
		}
		if closeAt := w.closing(open); !t.Before(open) && t.Before(closeAt) {
			return open, closeAt, true
		}
	}
	return time.Time{}, time.Time{}, false
}

// Contains reports whether the window is open at t.
func (w Window) Contains(t time.Time) bool {
	_, _, ok := w.Occurrence(t)
	return ok
}

// Next returns the opening time of the first occurrence that opens after t, in t's time zone.
func (w Window) Next(t time.Time) (time.Time, bool) {
	for off := 0; off <= 8; off++ {
		if open, opens := w.opening(t, off); opens && open.After(t) {
			return open, true
		}
	}
	return time.Time{}, false
}

// Tick is one point in time at which a timer should look at the window: the weekdays it falls on
// and the time of day.
type Tick struct {
	Days   [7]bool
	Minute int // minutes after midnight
}

// Ticks lists the points from the opening of the window to its closing, step apart, with the
// weekdays each one falls on (a tick after midnight of a window that crosses it falls on the
// next day). The opening is always the first tick; no tick falls on the closing minute.
func (w Window) Ticks(step time.Duration) []Tick {
	if step < time.Minute {
		step = time.Minute
	}
	var out []Tick
	for off := time.Duration(0); off < w.Length(); off += step {
		at := w.Start + int(off/time.Minute)
		shift := at / (24 * 60)
		var t Tick
		t.Minute = at % (24 * 60)
		for d, on := range w.Days {
			if on {
				t.Days[(d+shift)%7] = true
			}
		}
		out = append(out, t)
	}
	return out
}
