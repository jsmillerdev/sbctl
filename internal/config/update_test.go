package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func at(t *testing.T, loc *time.Location, s string) time.Time {
	t.Helper()
	v, err := time.ParseInLocation("2006-01-02 15:04", s, loc)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestParseWindow(t *testing.T) {
	cases := []struct {
		in, canon string
	}{
		{"Sun 03:00-05:00", "Sun 03:00-05:00"},
		{"sun 03:00-05:00", "Sun 03:00-05:00"},
		{"Sunday 03:00-05:00", "Sun 03:00-05:00"},
		{"Sat,Sun 02:00-04:00", "Sat,Sun 02:00-04:00"},
		{"Sun,Sat 02:00-04:00", "Sat,Sun 02:00-04:00"},
		{"Mon-Fri 01:30-03:30", "Mon,Tue,Wed,Thu,Fri 01:30-03:30"},
		{"Fri-Mon 01:00-02:00", "Mon,Fri,Sat,Sun 01:00-02:00"},
		{"daily 03:00-04:00", "daily 03:00-04:00"},
		{"03:00-04:00", "daily 03:00-04:00"},
		{"Mon-Sun 03:00-04:00", "daily 03:00-04:00"},
		{"Sun 23:00-01:00", "Sun 23:00-01:00"},
		{"  Wed   00:00-23:59 ", "Wed 00:00-23:59"},
	}
	for _, c := range cases {
		w, err := ParseWindow(c.in)
		if err != nil {
			t.Errorf("ParseWindow(%q): %v", c.in, err)
			continue
		}
		if got := w.String(); got != c.canon {
			t.Errorf("ParseWindow(%q).String() = %q, want %q", c.in, got, c.canon)
		}
		if again, err := ParseWindow(w.String()); err != nil || again != w {
			t.Errorf("the canonical form of %q does not round-trip: %v", c.in, err)
		}
	}
}

func TestParseWindowRefusals(t *testing.T) {
	for _, in := range []string{
		"", "Sun", "Sun 3:00-5:00", "Sun 03:00", "Sun 03:00-03:00", "Sun 24:00-25:00", "Sun 03:60-04:00",
		"Funday 03:00-05:00", "Sun-Foo 03:00-05:00", "Sun Mon 03:00-05:00", "Sun 03:00-05:00-06:00", "Sun 03.00-05.00",
		"Sun, 03:00-05:00", "Sun 0a:00-05:00",
	} {
		if w, err := ParseWindow(in); err == nil {
			t.Errorf("ParseWindow(%q) = %v, want an error", in, w)
		}
	}
}

func TestWindowContains(t *testing.T) {
	utc := time.UTC
	w, err := ParseWindow("Sun 03:00-05:00")
	if err != nil {
		t.Fatal(err)
	}
	// 2026-10-04 is a Sunday.
	for _, c := range []struct {
		when string
		in   bool
	}{
		{"2026-10-04 02:59", false},
		{"2026-10-04 03:00", true},
		{"2026-10-04 04:59", true},
		{"2026-10-04 05:00", false},
		{"2026-10-05 04:00", false}, // Monday
		{"2026-10-03 04:00", false}, // Saturday
		{"2026-10-11 03:30", true},
	} {
		if got := w.Contains(at(t, utc, c.when)); got != c.in {
			t.Errorf("Contains(%s) = %v, want %v", c.when, got, c.in)
		}
	}

	// A window that crosses midnight belongs to the day it opens on.
	night, _ := ParseWindow("Sun 23:00-01:00")
	for _, c := range []struct {
		when string
		in   bool
	}{
		{"2026-10-04 22:59", false},
		{"2026-10-04 23:00", true},
		{"2026-10-05 00:30", true}, // Monday morning, still Sunday's window
		{"2026-10-05 01:00", false},
		{"2026-10-04 00:30", false}, // Sunday morning: Saturday's window does not exist
		{"2026-10-05 23:30", false},
	} {
		if got := night.Contains(at(t, utc, c.when)); got != c.in {
			t.Errorf("crossing window Contains(%s) = %v, want %v", c.when, got, c.in)
		}
	}
	start, end, ok := night.Occurrence(at(t, utc, "2026-10-05 00:30"))
	if !ok || !start.Equal(at(t, utc, "2026-10-04 23:00")) || !end.Equal(at(t, utc, "2026-10-05 01:00")) {
		t.Errorf("Occurrence at Monday 00:30 = %v, %v, %v", start, end, ok)
	}
}

func TestWindowUsesTheTimeZoneOfTheInstant(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tz database")
	}
	w, _ := ParseWindow("Sun 03:00-05:00")
	// 07:30 UTC is 03:30 in New York in October (EDT, UTC-4).
	utc := time.Date(2026, 10, 4, 7, 30, 0, 0, time.UTC)
	if w.Contains(utc) {
		t.Error("07:30 UTC is outside a window of 03:00-05:00 UTC")
	}
	if !w.Contains(utc.In(ny)) {
		t.Error("07:30 UTC is 03:30 in New York, inside a window of 03:00-05:00 New York time")
	}
}

func TestWindowAcrossDaylightSavingTime(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tz database")
	}
	// Clocks go forward at 02:00 on 2026-03-08: 02:00-03:00 does not exist that day.
	w, _ := ParseWindow("Sun 01:00-04:00")
	if !w.Contains(time.Date(2026, 3, 8, 3, 30, 0, 0, ny)) {
		t.Error("03:30 EDT on the spring-forward Sunday is inside 01:00-04:00")
	}
	if w.Contains(time.Date(2026, 3, 8, 4, 0, 0, 0, ny)) {
		t.Error("04:00 is the end of the window")
	}
	// Clocks go back at 02:00 on 2026-11-01: 01:00-02:00 happens twice; both are inside.
	if !w.Contains(time.Date(2026, 11, 1, 1, 30, 0, 0, ny)) {
		t.Error("01:30 EDT on the fall-back Sunday is inside 01:00-04:00")
	}
}

func TestWindowNext(t *testing.T) {
	w, _ := ParseWindow("Sun 03:00-05:00")
	utc := time.UTC
	for _, c := range []struct{ from, next string }{
		{"2026-10-04 02:00", "2026-10-04 03:00"}, // before the window on a Sunday
		{"2026-10-04 03:00", "2026-10-11 03:00"}, // at the opening: the next one is a week away
		{"2026-10-04 04:00", "2026-10-11 03:00"}, // inside
		{"2026-10-05 12:00", "2026-10-11 03:00"},
	} {
		got, ok := w.Next(at(t, utc, c.from))
		if !ok || !got.Equal(at(t, utc, c.next)) {
			t.Errorf("Next(%s) = %v, want %s", c.from, got, c.next)
		}
	}
}

func TestWindowTicks(t *testing.T) {
	w, _ := ParseWindow("Sun 03:00-05:00")
	ticks := w.Ticks(30 * time.Minute)
	var mins []int
	for _, tk := range ticks {
		mins = append(mins, tk.Minute)
		if !tk.Days[time.Sunday] {
			t.Errorf("tick %d is not on Sunday: %v", tk.Minute, tk.Days)
		}
	}
	want := []int{180, 210, 240, 270}
	if len(mins) != len(want) {
		t.Fatalf("ticks %v, want %v (the closing minute is not a tick)", mins, want)
	}
	for i := range want {
		if mins[i] != want[i] {
			t.Fatalf("ticks %v, want %v", mins, want)
		}
	}

	night, _ := ParseWindow("Sat,Sun 23:00-01:00")
	for _, tk := range night.Ticks(30 * time.Minute) {
		switch tk.Minute {
		case 23 * 60, 23*60 + 30:
			if !tk.Days[time.Saturday] || !tk.Days[time.Sunday] || tk.Days[time.Monday] {
				t.Errorf("tick %d: want Sat and Sun, got %v", tk.Minute, tk.Days)
			}
		case 0, 30:
			// Past midnight the ticks fall on the following days: Sun and Mon.
			if !tk.Days[time.Sunday] || !tk.Days[time.Monday] || tk.Days[time.Saturday] {
				t.Errorf("tick %d: want Sun and Mon, got %v", tk.Minute, tk.Days)
			}
		default:
			t.Errorf("unexpected tick at minute %d", tk.Minute)
		}
	}
}

func TestUpdateDefaultsAndValidation(t *testing.T) {
	c := Default()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	u := c.Update
	if u.Mode != UpdateNotify || u.Channel != "stable" || u.OSReboot != "window" || u.OSSecurityUpdates {
		t.Errorf("defaults: %+v", u)
	}
	if d, ok, err := u.CheckEvery(); err != nil || !ok || d != 24*time.Hour {
		t.Errorf("CheckEvery() = %v, %v, %v", d, ok, err)
	}
	if u.RebootsInWindow() {
		t.Error("an existing node (os_security_updates unset) must not reboot itself")
	}
	if u.TimerWanted() {
		t.Error("notify mode with no managed OS updates needs no timer: the daemon does the release check")
	}
	u.Mode = UpdateAuto
	if !u.TimerWanted() {
		t.Error("auto mode needs the timer")
	}
	u.Mode, u.OSSecurityUpdates = UpdateNotify, true
	if !u.TimerWanted() {
		t.Error("the reboot in the window needs the timer")
	}

	for name, mut := range map[string]func(*Config){
		"mode":     func(c *Config) { c.Update.Mode = "yolo" },
		"channel":  func(c *Config) { c.Update.Channel = "edge" },
		"reboot":   func(c *Config) { c.Update.OSReboot = "sometimes" },
		"window":   func(c *Config) { c.Update.Window = "Sun 3am" },
		"garbage":  func(c *Config) { c.Update.CheckInterval = "often" },
		"negative": func(c *Config) { c.Update.CheckInterval = "-2h" },
		"zerodays": func(c *Config) { c.Update.CheckInterval = "0d" },
	} {
		c := Default()
		mut(c)
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), "update.") {
			t.Errorf("%s: Validate() = %v, want an update.* error", name, err)
		}
	}
}

func TestUpdateLoadsFromFileAndEnvironment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("[update]\nmode = \"auto\"\nwindow = \"Sat 02:00-04:00\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SUPAVISE_UPDATE_OS_REBOOT", "never")
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Update.Mode != "auto" || c.Update.Window != "Sat 02:00-04:00" || c.Update.OSReboot != "never" {
		t.Errorf("loaded %+v", c.Update)
	}
	if c.Update.Channel != "stable" || c.Update.CheckInterval != "24h" {
		t.Errorf("keys absent from the file keep their defaults: %+v", c.Update)
	}
}

// One grammar for update.check_interval, shared with the daemon's release check (internal/health).
func TestParseCheckInterval(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"":      24 * time.Hour,
		"12h":   12 * time.Hour,
		"90m":   90 * time.Minute,
		"2d":    48 * time.Hour,
		"2D":    48 * time.Hour,
		"7200":  2 * time.Hour,
		"5m":    time.Hour, // never under an hour
		"30":    time.Hour,
		" 6h ":  6 * time.Hour,
		"1h30m": 90 * time.Minute,
		"720h":  720 * time.Hour,
	} {
		d, on, err := ParseCheckInterval(in)
		if err != nil || !on || d != want {
			t.Errorf("ParseCheckInterval(%q) = %v, %v, %v; want %v", in, d, on, err, want)
		}
	}
	for _, in := range []string{"off", "OFF", "never", "0"} {
		if _, on, err := ParseCheckInterval(in); err != nil || on {
			t.Errorf("ParseCheckInterval(%q): on %v, err %v; want checks off", in, on, err)
		}
	}
	for _, in := range []string{"often", "d", "-1", "-2h", "0d", "1.5d", "2w"} {
		if _, _, err := ParseCheckInterval(in); err == nil {
			t.Errorf("ParseCheckInterval(%q) was accepted", in)
		}
	}
}

// The forms that internal/health documents load through the config file and validate: a bare
// number of seconds in TOML, whole days, "never".
func TestCheckIntervalLoadsInEveryDocumentedForm(t *testing.T) {
	for body, want := range map[string]time.Duration{
		"[update]\ncheck_interval = 7200\n":     2 * time.Hour,
		"[update]\ncheck_interval = \"2d\"\n":   48 * time.Hour,
		"[update]\ncheck_interval = \"12h\"\n":  12 * time.Hour,
		"[update]\ncheck_interval = \"7200\"\n": 2 * time.Hour,
		"[update]\nmode = \"auto\"\n":           24 * time.Hour,
	} {
		path := filepath.Join(t.TempDir(), "config.toml")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		c, err := Load(path)
		if err != nil {
			t.Errorf("%q: %v", body, err)
			continue
		}
		if d, on, err := c.Update.CheckEvery(); err != nil || !on || d != want {
			t.Errorf("%q: CheckEvery() = %v, %v, %v; want %v", body, d, on, err, want)
		}
	}
	for _, body := range []string{"[update]\ncheck_interval = 0\n", "[update]\ncheck_interval = \"never\"\n"} {
		path := filepath.Join(t.TempDir(), "config.toml")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		c, err := Load(path)
		if err != nil {
			t.Errorf("%q: %v", body, err)
			continue
		}
		if _, on, _ := c.Update.CheckEvery(); on {
			t.Errorf("%q: checks should be off", body)
		}
	}
	t.Setenv("SUPAVISE_UPDATE_CHECK_INTERVAL", "3d")
	if c, err := Load(filepath.Join(t.TempDir(), "none.toml")); err != nil {
		t.Fatal(err)
	} else if d, _, _ := c.Update.CheckEvery(); d != 72*time.Hour {
		t.Errorf("environment: %v", d)
	}
}
