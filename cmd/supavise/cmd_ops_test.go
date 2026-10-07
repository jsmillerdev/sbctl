package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// opsConfig writes a config.toml for a node that exists only as a directory, and returns its
// path and state directory. extra is appended to the file.
func opsConfig(t *testing.T, extra string) (cfgPath, state string) {
	t.Helper()
	state = t.TempDir()
	// Addresses nothing listens on: a listener opened and closed again.
	closed := func() string {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		ln.Close()
		return addr
	}
	body := fmt.Sprintf("domain = 'node.example.com'\nstate_dir = %q\nsupervisor = 'exec'\n[tls]\nmode = 'off'\n[listen]\nhttp = %q\nhttps = %q\nadmin = %q\n%s",
		state, closed(), closed(), closed(), extra)
	cfgPath = filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { configPath = "" })
	return cfgPath, state
}

// resetFlags puts the flags of a command back to their defaults: the commands are package
// variables, so a flag one run set would otherwise still be set for the next.
func resetFlags(t *testing.T, path []string, names ...string) {
	t.Helper()
	c, _, err := rootCmd.Find(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		f := c.Flags().Lookup(n)
		if f == nil {
			t.Fatalf("no flag %s", n)
		}
		_ = f.Value.Set(f.DefValue)
		f.Changed = false
	}
}

var announceFlags = []string{"at", "until", "duration", "notice", "message"}

func TestOpsCommandsAreRegistered(t *testing.T) {
	for _, path := range [][]string{
		{"status"}, {"alerts", "test"}, {"alerts", "list"},
		{"maintenance", "announce"}, {"maintenance", "clear"}, {"maintenance", "show"},
	} {
		if c, _, err := rootCmd.Find(path); err != nil || c == nil || c.Name() != path[len(path)-1] {
			t.Errorf("supavise %s is not registered: %v", strings.Join(path, " "), err)
		}
	}
}

func TestMaintenanceAnnounceShowClear(t *testing.T) {
	cfg, state := opsConfig(t, "")
	start := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	resetFlags(t, []string{"maintenance", "announce"}, announceFlags...)

	if out, err := runRoot(t, "--config", cfg, "maintenance", "show"); err != nil || !strings.Contains(out, "no maintenance window is announced") {
		t.Fatalf("show: %q %v", out, err)
	}
	out, err := runRoot(t, "--config", cfg, "maintenance", "announce", "--at", start, "--duration", "90m", "--notice", "24h", "--message", "Database upgrade")
	if err != nil || !strings.Contains(out, "announced:") || !strings.Contains(out, "notice 24h0m0s ahead") {
		t.Fatalf("announce: %q %v", out, err)
	}
	b, err := os.ReadFile(filepath.Join(state, "system", "maintenance.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Message     string    `json:"message"`
		StartsAt    time.Time `json:"starts_at"`
		EndsAt      time.Time `json:"ends_at"`
		LeadSeconds int       `json:"lead_seconds"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m.Message != "Database upgrade" || m.EndsAt.Sub(m.StartsAt) != 90*time.Minute || m.LeadSeconds != 86400 {
		t.Errorf("%+v", m)
	}
	if out, err := runRoot(t, "--config", cfg, "maintenance", "show"); err != nil || !strings.Contains(out, "notice period") {
		t.Errorf("show: %q %v", out, err)
	}
	if out, err := runRoot(t, "--config", cfg, "maintenance", "clear"); err != nil || strings.TrimSpace(out) != "cleared" {
		t.Errorf("clear: %q %v", out, err)
	}
	if _, err := os.Stat(filepath.Join(state, "system", "maintenance.json")); !os.IsNotExist(err) {
		t.Error("the file is still there")
	}
	if out, _ := runRoot(t, "--config", cfg, "maintenance", "clear"); !strings.Contains(out, "no maintenance window was announced") {
		t.Errorf("a second clear: %q", out)
	}
}

func TestMaintenanceAnnounceRefusesBadInput(t *testing.T) {
	cfg, state := opsConfig(t, "")
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	past := time.Now().Add(-5 * time.Hour).UTC().Format(time.RFC3339)
	for name, args := range map[string][]string{
		"no message":       {"--at", future},
		"no time":          {"--message", "m"},
		"not a time":       {"--at", "next tuesday", "--message", "m"},
		"already over":     {"--at", past, "--duration", "1h", "--message", "m"},
		"until before at":  {"--at", future, "--until", past, "--message", "m"},
		"bad duration":     {"--at", future, "--duration", "soon", "--message", "m"},
		"bad notice":       {"--at", future, "--notice", "-3h", "--message", "m"},
		"empty message":    {"--at", future, "--message", "   "},
		"a very long text": {"--at", future, "--message", strings.Repeat("x", 600)},
	} {
		resetFlags(t, []string{"maintenance", "announce"}, announceFlags...)
		if _, err := runRoot(t, append([]string{"--config", cfg, "maintenance", "announce"}, args...)...); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if _, err := os.Stat(filepath.Join(state, "system", "maintenance.json")); !os.IsNotExist(err) {
		t.Error("a refused announcement left a file")
	}
}

func TestParseWhen(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	for in, want := range map[string]time.Time{
		"now":                       now,
		"2026-10-12T22:00:00Z":      time.Date(2026, 10, 12, 22, 0, 0, 0, time.UTC),
		"2026-10-12T22:00:00+02:00": time.Date(2026, 10, 12, 20, 0, 0, 0, time.UTC),
		"2026-10-12 22:00":          time.Date(2026, 10, 12, 22, 0, 0, 0, time.UTC),
		"2026-10-12T22:00":          time.Date(2026, 10, 12, 22, 0, 0, 0, time.UTC),
	} {
		got, err := parseWhen(in, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("%q: %v %v, want %v", in, got, err, want)
		}
	}
	if _, err := parseWhen("tomorrow", now); err == nil || !strings.Contains(err.Error(), "RFC 3339") {
		t.Errorf("%v", err)
	}
}

func TestAlertsTestCommand(t *testing.T) {
	var got []string
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		got = append(got, fmt.Sprint(b["kind"], " ", r.Header.Get("X-Supavise-Signature") != ""))
	}))
	defer good.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) }))
	defer bad.Close()

	// Nothing configured: say so, fail.
	cfg, _ := opsConfig(t, "")
	if _, err := runRoot(t, "--config", cfg, "alerts", "test"); err == nil || !strings.Contains(err.Error(), "no alert destination") {
		t.Errorf("no destination: %v", err)
	}

	cfg, _ = opsConfig(t, fmt.Sprintf("[[alerts.webhooks]]\nurl = %q\nsecret = 'k'\n", good.URL))
	out, err := runRoot(t, "--config", cfg, "alerts", "test")
	if err != nil || !strings.HasPrefix(out, "ok ") {
		t.Fatalf("%q %v", out, err)
	}
	if len(got) != 1 || got[0] != "test true" {
		t.Errorf("the endpoint got %v", got)
	}

	cfg, _ = opsConfig(t, fmt.Sprintf("[[alerts.webhooks]]\nurl = %q\n[[alerts.webhooks]]\nurl = %q\n", good.URL, bad.URL))
	out, err = runRoot(t, "--config", cfg, "alerts", "test")
	if err == nil || !strings.Contains(err.Error(), "1 of 2 destination(s) failed") || !strings.Contains(out, "FAILED") || !strings.Contains(out, "403") {
		t.Errorf("%q %v", out, err)
	}

	// A config the node would refuse to start with is refused here too.
	cfg, _ = opsConfig(t, "[[alerts.webhooks]]\nurl = 'not a url'\n")
	if _, err := runRoot(t, "--config", cfg, "alerts", "test"); err == nil {
		t.Error("a bad webhook URL was accepted")
	}
}

func TestAlertsListIsEmptyOnAFreshNode(t *testing.T) {
	cfg, _ := opsConfig(t, "")
	if out, err := runRoot(t, "--config", cfg, "alerts", "list"); err != nil || !strings.Contains(out, "no active alerts") {
		t.Errorf("%q %v", out, err)
	}
}

// A node whose system cluster is not running is down: exit status 2, and the report says why
// without needing the registry.
func TestStatusOfANodeThatIsNotRunning(t *testing.T) {
	cfg, _ := opsConfig(t, "")
	configPath = cfg
	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	code, err := runStatus(ctx, &out, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if code != 2 {
		t.Errorf("exit status %d, want 2 (down)\n%s", code, out.String())
	}
	if !strings.HasPrefix(out.String(), "down: ") {
		t.Errorf("the first line is not the verdict:\n%s", out.String())
	}
	for _, want := range []string{"COMPONENT", "daemon", "edge", "registry", "disk"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("no %q in\n%s", want, out.String())
		}
	}

	out.Reset()
	if code, err = runStatus(ctx, &out, true, false); err != nil || code != 2 {
		t.Fatalf("--json: %d %v", code, err)
	}
	var rep struct {
		Status     string `json:"status"`
		Summary    string `json:"summary"`
		Components []struct {
			Name  string `json:"name"`
			State string `json:"state"`
		} `json:"components"`
	}
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("--json is not JSON: %v\n%s", err, out.String())
	}
	if rep.Status != "down" || rep.Summary == "" || len(rep.Components) < 4 {
		t.Errorf("%+v", rep)
	}
}

// A user who cannot read the node's files gets told how to run it, not a verdict of "down".
func TestStatusAsAUserWhoCannotReadTheKey(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every file")
	}
	cfg, state := opsConfig(t, "")
	key := filepath.Join(state, "master.key")
	if err := os.WriteFile(key, []byte("00"), 0o000); err != nil {
		t.Fatal(err)
	}
	configPath = cfg
	t.Setenv("SUPAVISE_KEY_PATH", key)
	var out bytes.Buffer
	_, err := runStatus(context.Background(), &out, false, false)
	if err == nil || !strings.Contains(err.Error(), "sudo -u supavise supavise status") {
		t.Errorf("err = %v, out = %q", err, out.String())
	}
}
