// Command bumpcheck checks every upstream release this repository pins:
//
//   - each artifact of internal/versions/versions.yaml still exists as a slim-services release with its linux
//     amd64 and arm64 archives; a pin that does not exit non-zero (a release pulled upstream
//     breaks every install);
//   - for each pin it reports the newest release, as a summary that never fails the run: a
//     bump is a decision, made in a pull request that the conformance suite gates.
//
// It covers the Studio tag, the Supabase CLI and supabase-js versions the conformance suite
// runs, too.
//
//	go run ./tests/conformance/bumpcheck
//
// GITHUB_TOKEN, when set, raises the GitHub API rate limit. With GITHUB_STEP_SUMMARY set the
// report is appended to it.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	versions := flag.String("versions", "internal/versions/versions.yaml", "the pin file")
	pins := flag.String("pins", "tests/conformance/pins.env", "conformance pins (SUPABASE_CLI_VERSION)")
	pkg := flag.String("package-json", "tests/conformance/js/package.json", "conformance package.json (supabase-js)")
	gh := flag.String("github-api", "https://api.github.com", "GitHub API base URL")
	npm := flag.String("npm-registry", "https://registry.npmjs.org", "npm registry base URL")
	flag.Parse()

	c := &Checker{
		Get:      httpGet(os.Getenv("GITHUB_TOKEN")),
		GitHub:   *gh,
		NPM:      *npm,
		Slim:     "supabase/slim-services",
		Versions: *versions, Pins: *pins, PackageJSON: *pkg,
	}
	rep, err := c.Run()
	if err != nil {
		fmt.Fprintln(os.Stderr, "bumpcheck:", err)
		os.Exit(2)
	}
	md := rep.Markdown()
	fmt.Print(md)
	if p := os.Getenv("GITHUB_STEP_SUMMARY"); p != "" {
		if f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644); err == nil {
			fmt.Fprintf(f, "## Pinned upstream releases\n\n%s\n", md)
			f.Close()
		}
	}
	for _, row := range rep.Rows {
		if row.Status == "unknown" {
			// A GitHub annotation, so an unverifiable pin shows on the run page.
			fmt.Printf("::warning title=bumpcheck::%s %s could not be verified: %s\n", row.Name, row.Pinned, row.Details)
		}
	}
	if rep.Missing() > 0 {
		fmt.Fprintf(os.Stderr, "\nbumpcheck: %d pinned release(s) are gone or incomplete upstream.\n", rep.Missing())
		os.Exit(1)
	}
	if rep.Unverified() > 0 {
		fmt.Fprintf(os.Stderr, "\nbumpcheck: %d slim-services pin(s) could not be checked (rate limit or upstream error); a pulled release would go unnoticed, so the run fails.\n", rep.Unverified())
		os.Exit(1)
	}
}

// Getter fetches a URL and returns the body and the status code.
type Getter func(url string) ([]byte, int, error)

func httpGet(token string) Getter {
	c := &http.Client{Timeout: 60 * time.Second}
	return func(url string) ([]byte, int, error) {
		var lastErr error
		for attempt := 1; attempt <= 3; attempt++ {
			req, err := http.NewRequest(http.MethodGet, url, nil)
			if err != nil {
				return nil, 0, err
			}
			if strings.Contains(url, "api.github.com") {
				req.Header.Set("Accept", "application/vnd.github+json")
				if token != "" {
					req.Header.Set("Authorization", "Bearer "+token)
				}
			}
			resp, err := c.Do(req)
			if err != nil {
				lastErr = err
				time.Sleep(time.Duration(attempt) * time.Second)
				continue
			}
			b, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				lastErr = err
				continue
			}
			if resp.StatusCode >= 500 {
				lastErr = fmt.Errorf("GET %s: %s", url, resp.Status)
				time.Sleep(time.Duration(attempt) * time.Second)
				continue
			}
			return b, resp.StatusCode, nil
		}
		return nil, 0, lastErr
	}
}

// Row is one pinned thing and what upstream has.
type Row struct {
	Name    string
	Pinned  string
	Status  string // ok, missing, incomplete, unpinned, unknown
	Install bool   // a slim-services release that installs fetch; an unknown state fails the run
	Newest  string
	Newer   bool
	Details string
}

// Result is the whole report.
type Result struct{ Rows []Row }

// Missing counts the pins that no longer exist upstream or lack an archive.
func (r *Result) Missing() int {
	n := 0
	for _, row := range r.Rows {
		if row.Status == "missing" || row.Status == "incomplete" {
			n++
		}
	}
	return n
}

// Unverified counts the slim-services pins whose lookup failed for a reason other than the
// release being absent. Installs download those, so the run must not read them as fine.
func (r *Result) Unverified() int {
	n := 0
	for _, row := range r.Rows {
		if row.Install && row.Status == "unknown" {
			n++
		}
	}
	return n
}

// Markdown renders the table and the list of newer releases.
func (r *Result) Markdown() string {
	var b strings.Builder
	b.WriteString("| Pin | Pinned | State | Newest upstream | Note |\n|---|---|---|---|---|\n")
	for _, row := range r.Rows {
		newest := row.Newest
		if row.Newer {
			newest = "**" + newest + "**"
		}
		fmt.Fprintf(&b, "| %s | `%s` | %s | %s | %s |\n", row.Name, row.Pinned, row.Status, newest, row.Details)
	}
	var newer []string
	for _, row := range r.Rows {
		if row.Newer {
			newer = append(newer, fmt.Sprintf("%s: %s -> %s", row.Name, row.Pinned, row.Newest))
		}
	}
	if len(newer) == 0 {
		b.WriteString("\nEvery pin is the newest release.\n")
	} else {
		fmt.Fprintf(&b, "\nNewer releases exist for %d pin(s); bump them in a pull request, the conformance suite gates it:\n\n", len(newer))
		for _, n := range newer {
			fmt.Fprintf(&b, "- %s\n", n)
		}
	}
	if m := r.Missing(); m > 0 {
		fmt.Fprintf(&b, "\n**%d pinned release(s) are gone or incomplete upstream.**\n", m)
	}
	return b.String()
}

// Checker holds the inputs of a run.
type Checker struct {
	Get                         Getter
	GitHub, NPM, Slim           string
	Versions, Pins, PackageJSON string
}

// Run reads the pin files and asks upstream about each.
func (c *Checker) Run() (*Result, error) {
	v, err := ReadVersions(c.Versions)
	if err != nil {
		return nil, err
	}
	res := &Result{}
	for _, svc := range sortedKeys(v.Artifacts) {
		res.Rows = append(res.Rows, c.slim(svc, svc, v.Artifacts[svc]))
	}
	if v.Studio.Tag != "" {
		res.Rows = append(res.Rows, c.studio(v.Studio.Tag))
	}
	cli := v.CLI.VersionTested
	if cli == "" {
		res.Rows = append(res.Rows, Row{Name: "cli (versions.yaml cli.version_tested)", Pinned: "", Status: "unpinned", Details: "empty"})
	} else {
		res.Rows = append(res.Rows, c.githubRelease("cli (versions.yaml cli.version_tested)", "supabase/cli", cli))
	}
	if p, err := ReadPins(c.Pins); err == nil && p["SUPABASE_CLI_VERSION"] != "" {
		res.Rows = append(res.Rows, c.githubRelease("cli (conformance pins.env)", "supabase/cli", p["SUPABASE_CLI_VERSION"]))
	}
	if js, err := ReadSupabaseJS(c.PackageJSON); err == nil && js != "" {
		res.Rows = append(res.Rows, c.npmPackage("supabase-js (conformance package.json)", "@supabase/supabase-js", js))
	}
	return res, nil
}

func (c *Checker) slim(name, svc, tag string) Row {
	row := Row{Name: name, Pinned: tag, Install: true}
	body, code, err := c.Get(fmt.Sprintf("%s/repos/%s/releases/tags/%s", c.GitHub, c.Slim, tag))
	if err != nil {
		row.Status, row.Details = "unknown", err.Error()
		return row
	}
	switch {
	case code == http.StatusNotFound:
		row.Status, row.Details = "missing", "no such release"
	case code != http.StatusOK:
		row.Status, row.Details = "unknown", fmt.Sprintf("HTTP %d", code)
	default:
		var rel struct {
			Assets []struct {
				Name string `json:"name"`
			} `json:"assets"`
		}
		if err := json.Unmarshal(body, &rel); err != nil {
			row.Status, row.Details = "unknown", err.Error()
			break
		}
		have := map[string]bool{}
		for _, a := range rel.Assets {
			have[a.Name] = true
		}
		var lacking []string
		for _, arch := range []string{"amd64", "arm64"} {
			if n := tag + "-linux-" + arch + ".tar.zst"; !have[n] {
				lacking = append(lacking, n)
			}
		}
		if len(lacking) > 0 {
			row.Status, row.Details = "incomplete", "lacks "+strings.Join(lacking, ", ")
		} else {
			row.Status = "ok"
		}
	}
	c.newest(&row, svc)
	return row
}

// studio checks the Studio tag: the slim-services release studio-<tag>-r<N> must exist.
func (c *Checker) studio(tag string) Row {
	row := Row{Name: "studio", Pinned: tag, Install: true}
	body, code, err := c.Get(fmt.Sprintf("%s/repos/%s/git/matching-refs/tags/studio-%s", c.GitHub, c.Slim, tag))
	if err != nil || code != http.StatusOK {
		row.Status, row.Details = "unknown", fmt.Sprintf("HTTP %d %v", code, err)
		return row
	}
	var refs []struct {
		Ref string `json:"ref"`
	}
	_ = json.Unmarshal(body, &refs)
	if len(refs) == 0 {
		row.Status, row.Details = "missing", "no studio-"+tag+"-r<N> release in slim-services"
	} else {
		row.Status = "ok"
	}
	c.newest(&row, "studio")
	if row.Newest != "" {
		// The pin is the upstream ref without the packaging suffix.
		row.Newest = strings.TrimPrefix(row.Newest, "studio-")
		row.Newest = trimRevision(row.Newest)
		row.Newer = natLess(tag, row.Newest)
	}
	return row
}

// newest fills row.Newest and row.Newer from the tags of one slim-services service.
func (c *Checker) newest(row *Row, svc string) {
	body, code, err := c.Get(fmt.Sprintf("%s/repos/%s/git/matching-refs/tags/%s-", c.GitHub, c.Slim, svc))
	if err != nil || code != http.StatusOK {
		row.Details = strings.TrimSpace(row.Details + fmt.Sprintf(" (could not list newer releases: HTTP %d %v)", code, err))
		return
	}
	var refs []struct {
		Ref string `json:"ref"`
	}
	_ = json.Unmarshal(body, &refs)
	var tags []string
	for _, r := range refs {
		tags = append(tags, strings.TrimPrefix(r.Ref, "refs/tags/"))
	}
	if best := Newest(tags, svc); best != "" {
		row.Newest = best
		row.Newer = TagNewer(row.Pinned, best, svc)
	}
}

func (c *Checker) githubRelease(name, repo, pinned string) Row {
	row := Row{Name: name, Pinned: pinned}
	body, code, err := c.Get(fmt.Sprintf("%s/repos/%s/releases/tags/v%s", c.GitHub, repo, strings.TrimPrefix(pinned, "v")))
	if err != nil {
		row.Status, row.Details = "unknown", err.Error()
		return row
	}
	switch code {
	case http.StatusOK:
		row.Status = "ok"
	case http.StatusNotFound:
		row.Status, row.Details = "missing", "no such release"
	default:
		row.Status, row.Details = "unknown", fmt.Sprintf("HTTP %d", code)
	}
	if body, code, err = c.Get(fmt.Sprintf("%s/repos/%s/releases/latest", c.GitHub, repo)); err == nil && code == http.StatusOK {
		var rel struct {
			Tag string `json:"tag_name"`
		}
		if json.Unmarshal(body, &rel) == nil && rel.Tag != "" {
			row.Newest = strings.TrimPrefix(rel.Tag, "v")
			row.Newer = natLess(pinned, row.Newest)
		}
	}
	return row
}

func (c *Checker) npmPackage(name, pkg, pinned string) Row {
	row := Row{Name: name, Pinned: pinned}
	body, code, err := c.Get(fmt.Sprintf("%s/%s/%s", c.NPM, pkg, pinned))
	switch {
	case err != nil:
		row.Status, row.Details = "unknown", err.Error()
	case code == http.StatusOK:
		row.Status = "ok"
	case code == http.StatusNotFound:
		row.Status, row.Details = "missing", "no such version on npm"
	default:
		row.Status, row.Details = "unknown", fmt.Sprintf("HTTP %d", code)
	}
	_ = body
	if body, code, err = c.Get(fmt.Sprintf("%s/%s/latest", c.NPM, pkg)); err == nil && code == http.StatusOK {
		var latest struct {
			Version string `json:"version"`
		}
		if json.Unmarshal(body, &latest) == nil && latest.Version != "" {
			row.Newest = latest.Version
			row.Newer = natLess(pinned, latest.Version)
		}
	}
	return row
}
