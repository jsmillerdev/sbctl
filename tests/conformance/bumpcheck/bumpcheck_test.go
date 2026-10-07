package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewestAndNewer(t *testing.T) {
	tags := []string{"postgres-17.11.0.004-r1", "postgres-17.11.0.004-r2", "postgres-17.9.1.001-r0", "postgres-17.11.0.010-r0", "postgresx-99-r0", "postgres-notes"}
	if got := Newest(tags, "postgres"); got != "postgres-17.11.0.010-r0" {
		t.Errorf("newest = %q", got)
	}
	if !TagNewer("postgres-17.11.0.004-r1", "postgres-17.11.0.004-r2", "postgres") {
		t.Error("a later packaging revision is newer")
	}
	if TagNewer("postgres-17.11.0.010-r0", "postgres-17.11.0.004-r9", "postgres") {
		t.Error("an older version with a higher revision is not newer")
	}
	// 140 > 99 numerically, though "99" sorts after "140" as text.
	if got := Newest([]string{"realtime-v2.99.1-r0", "realtime-v2.140.10-r0"}, "realtime"); got != "realtime-v2.140.10-r0" {
		t.Errorf("realtime newest = %q", got)
	}
	if got := Newest([]string{"pooler-v2.9.13-r1", "pooler-v2.10.0-r0"}, "pooler"); got != "pooler-v2.10.0-r0" {
		t.Errorf("pooler newest = %q", got)
	}
	if !natLess("2.119.0", "2.120.0") || natLess("2.120.0", "2.120.0") || natLess("2.120.1", "2.120.0") {
		t.Error("natLess")
	}
}

func fake(routes map[string]string) Getter {
	return func(url string) ([]byte, int, error) {
		for suffix, body := range routes {
			if strings.HasSuffix(url, suffix) {
				if body == "404" {
					return nil, http.StatusNotFound, nil
				}
				return []byte(body), http.StatusOK, nil
			}
		}
		return nil, 0, fmt.Errorf("unexpected request %s", url)
	}
}

func write(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRun(t *testing.T) {
	dir := t.TempDir()
	vy := write(t, dir, "versions.yaml", `artifacts:
  postgres: postgres-17.11.0.004-r1
  auth: auth-v2.195.0-r1
studio:
  tag: 2026.10.05-sha-94b8b06
cli:
  version_tested: "2.119.0"
`)
	pins := write(t, dir, "pins.env", "# pins\nSUPABASE_CLI_VERSION=2.119.0\n")
	pkg := write(t, dir, "package.json", `{"dependencies":{"@supabase/supabase-js":"2.117.3"}}`)
	assets := func(tag string, arm bool) string {
		a := fmt.Sprintf(`{"name":"%s-linux-amd64.tar.zst"}`, tag)
		if arm {
			a += fmt.Sprintf(`,{"name":"%s-linux-arm64.tar.zst"}`, tag)
		}
		return `{"assets":[` + a + `]}`
	}
	c := &Checker{
		GitHub: "https://gh", NPM: "https://npm", Slim: "o/slim", Versions: vy, Pins: pins, PackageJSON: pkg,
		Get: fake(map[string]string{
			"/releases/tags/postgres-17.11.0.004-r1": assets("postgres-17.11.0.004-r1", true),
			"/releases/tags/auth-v2.195.0-r1":        assets("auth-v2.195.0-r1", false),
			"/tags/postgres-":                        `[{"ref":"refs/tags/postgres-17.11.0.004-r1"},{"ref":"refs/tags/postgres-17.12.0.001-r0"}]`,
			"/tags/auth-":                            `[{"ref":"refs/tags/auth-v2.195.0-r1"}]`,
			"/tags/studio-2026.10.05-sha-94b8b06":    `[{"ref":"refs/tags/studio-2026.10.05-sha-94b8b06-r0"}]`,
			"/tags/studio-":                          `[{"ref":"refs/tags/studio-2026.10.05-sha-94b8b06-r0"},{"ref":"refs/tags/studio-2026.10.09-sha-aaaaaaa-r0"}]`,
			"/supabase/cli/releases/tags/v2.119.0":   `{}`,
			"/supabase/cli/releases/latest":          `{"tag_name":"v2.120.0"}`,
			"/@supabase/supabase-js/2.117.3":         `{}`,
			"/@supabase/supabase-js/latest":          `{"version":"2.117.3"}`,
		}),
	}
	res, err := c.Run()
	if err != nil {
		t.Fatal(err)
	}
	if res.Missing() != 1 {
		t.Errorf("missing = %d, want 1 (the auth release lacks its arm64 archive)", res.Missing())
	}
	md := res.Markdown()
	for _, want := range []string{"postgres-17.12.0.001-r0**", "| auth | `auth-v2.195.0-r1` | incomplete |", "studio | `2026.10.05-sha-94b8b06` | ok | **2026.10.09-sha-aaaaaaa**", "2.120.0**", "gone or incomplete"} {
		if !strings.Contains(md, want) {
			t.Errorf("report lacks %q:\n%s", want, md)
		}
	}
	if strings.Contains(md, "supabase-js (conformance package.json) | `2.117.3` | ok | **") {
		t.Errorf("supabase-js is current but marked newer:\n%s", md)
	}
}

func TestMissingRelease(t *testing.T) {
	dir := t.TempDir()
	vy := write(t, dir, "versions.yaml", "artifacts:\n  postgrest: postgrest-v16.4-r0\n")
	c := &Checker{GitHub: "https://gh", NPM: "https://npm", Slim: "o/slim", Versions: vy, Pins: filepath.Join(dir, "none"), PackageJSON: filepath.Join(dir, "none"),
		Get: fake(map[string]string{"/releases/tags/postgrest-v16.4-r0": "404", "/tags/postgrest-": "[]"})}
	res, err := c.Run()
	if err != nil {
		t.Fatal(err)
	}
	if res.Missing() != 1 || res.Rows[0].Status != "missing" {
		t.Errorf("rows: %+v", res.Rows)
	}
}
