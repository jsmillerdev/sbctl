package health

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jsmillerdev/supavise/internal/config"
)

// githubAPI is a fake GitHub API answering /repos/<repo>/releases/latest.
func githubAPI(t *testing.T, tag string, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/releases/latest") {
			http.NotFound(w, r)
			return
		}
		if status != 200 {
			w.WriteHeader(status)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": tag, "assets": []any{}})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func testConfig(t *testing.T) *config.Config {
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	return cfg
}

func TestCheckUpdateRecordsTheLatestRelease(t *testing.T) {
	cfg := testConfig(t)
	api := githubAPI(t, "v1.4.0", 200)
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

	rec, err := CheckUpdate(context.Background(), cfg, "v1.2.3", UpdateSource{Repo: "o/r", APIBase: api.URL}, now)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Latest != "v1.4.0" || !rec.Available || rec.Installed != "v1.2.3" || !rec.CheckedAt.Equal(now) {
		t.Errorf("record %+v", rec)
	}
	got, err := ReadUpdate(cfg.Paths())
	if err != nil || got == nil || got.Latest != "v1.4.0" || !got.Available {
		t.Fatalf("stored record %+v, %v", got, err)
	}

	// Up to date, and a build made after the tag counts as that tag.
	for _, installed := range []string{"v1.4.0", "v1.4.0-3-gabcdef", "v1.9.0"} {
		rec, err := CheckUpdate(context.Background(), cfg, installed, UpdateSource{Repo: "o/r", APIBase: api.URL}, now)
		if err != nil || rec.Available {
			t.Errorf("installed %s: %+v, %v", installed, rec, err)
		}
	}
}

func TestDevelopmentBuildIsNeverTold(t *testing.T) {
	cfg := testConfig(t)
	api := githubAPI(t, "v1.4.0", 200)
	rec, err := CheckUpdate(context.Background(), cfg, "dev", UpdateSource{APIBase: api.URL}, time.Now())
	if err != nil || rec.Available || rec.Latest != "v1.4.0" {
		t.Errorf("%+v, %v", rec, err)
	}
	if IsRelease("dev") || !IsRelease("v0.1.0") || !IsRelease("0.1.0") || !IsRelease("v1.2.3-4-gabcdef") || IsRelease("") {
		t.Error("IsRelease")
	}
}

func TestFailedCheckKeepsTheLastAnswerAndNotifiedVersion(t *testing.T) {
	cfg := testConfig(t)
	good := githubAPI(t, "v1.4.0", 200)
	bad := githubAPI(t, "", 500)
	now := time.Now()
	if _, err := CheckUpdate(context.Background(), cfg, "v1.2.3", UpdateSource{APIBase: good.URL}, now); err != nil {
		t.Fatal(err)
	}
	if err := MarkNotified(cfg, "v1.4.0"); err != nil {
		t.Fatal(err)
	}
	rec, err := CheckUpdate(context.Background(), cfg, "v1.2.3", UpdateSource{APIBase: bad.URL}, now.Add(time.Hour))
	if err == nil || rec == nil {
		t.Fatalf("a failed check: %+v, %v", rec, err)
	}
	if rec.Error == "" || rec.Latest != "v1.4.0" || !rec.Available || rec.NotifiedVersion != "v1.4.0" {
		t.Errorf("the last good answer was lost: %+v", rec)
	}
	// A later good check clears the error and keeps what was notified.
	rec, err = CheckUpdate(context.Background(), cfg, "v1.2.3", UpdateSource{APIBase: good.URL}, now.Add(2*time.Hour))
	if err != nil || rec.Error != "" || rec.NotifiedVersion != "v1.4.0" {
		t.Errorf("%+v, %v", rec, err)
	}
}

func TestUpdateRecordFileIsWorldReadableAndAtomic(t *testing.T) {
	cfg := testConfig(t)
	if err := WriteUpdate(cfg.Paths(), &UpdateRecord{Latest: "v1.0.0"}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(cfg.StateDir, "system", "update.json"))
	if err != nil || fi.Mode().Perm() != 0o644 {
		t.Errorf("%v %v", fi, err)
	}
	if rec, err := ReadUpdate(testConfig(t).Paths()); rec != nil || err != nil {
		t.Errorf("no record: %+v, %v", rec, err)
	}
}

func TestReadUpdateSettingsIsDefensive(t *testing.T) {
	dir := t.TempDir()
	files := 0
	write := func(body string) string {
		files++
		p := filepath.Join(dir, "config"+strings.Repeat("x", files)+".toml")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	t.Setenv(EnvUpdateInterval, "")
	cases := []struct {
		name string
		path string
		want time.Duration
	}{
		{"no file", "", 24 * time.Hour},
		{"missing file", filepath.Join(dir, "nope.toml"), 24 * time.Hour},
		{"no [update] section", write("domain = 'example.com'\n"), 24 * time.Hour},
		{"a duration", write("[update]\ncheck_interval = '6h'\n"), 6 * time.Hour},
		{"whole days", write("[update]\ncheck_interval = '2d'\n"), 48 * time.Hour},
		{"a bare number is seconds", write("[update]\ncheck_interval = 7200\n"), 2 * time.Hour},
		{"never under an hour", write("[update]\ncheck_interval = '1m'\n"), time.Hour},
		{"nonsense", write("[update]\ncheck_interval = 'soon'\n"), 24 * time.Hour},
		{"other keys are ignored", write("[update]\nmode = 'auto'\nwindow = 'Sun 02:00-04:00'\nchannel = 'stable'\n"), 24 * time.Hour},
		{"a broken file", write("[update\n"), 24 * time.Hour},
	}
	for _, c := range cases {
		if got := ReadUpdateSettings(c.path).CheckInterval; got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	for _, off := range []string{"[update]\ncheck_interval = 'off'\n", "[update]\ncheck_interval = 'never'\n", "[update]\ncheck_interval = 0\n"} {
		if !ReadUpdateSettings(write(off)).Off {
			t.Errorf("%q does not turn the check off", off)
		}
	}
	if ReadUpdateSettings("").Off {
		t.Error("the check is off by default")
	}
	t.Setenv(EnvUpdateInterval, "12h")
	if got := ReadUpdateSettings(write("[update]\ncheck_interval = '6h'\n")).CheckInterval; got != 12*time.Hour {
		t.Errorf("the environment does not override the file: %v", got)
	}
}
