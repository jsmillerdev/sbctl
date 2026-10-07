// Command specdiff compares the Management API OpenAPI documents this repository pins
// (internal/api/gen/specs) with the ones Supabase serves today and prints what changed:
// operations added, removed or changed, and the same for component schemas. Drift is
// noticed here and merged by a person; nothing is rewritten.
//
//	go run ./tests/conformance/specdiff            # fetch upstream, diff, exit 1 on drift
//	go run ./tests/conformance/specdiff -upstream-dir DIR   # diff against files DIR/<name>.json
//
// Exit status: 0 no drift, 1 drift, 2 an error (a spec could not be fetched or parsed).
// With GITHUB_STEP_SUMMARY set the report is also appended to it. The full report goes to
// -report (default specdiff-report.md).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	dir := flag.String("dir", "internal/api/gen/specs", "directory with the pinned specs (<name>.json)")
	base := flag.String("base", "https://api.supabase.com/api", "upstream base URL; <base>/<name>-json is fetched")
	upDir := flag.String("upstream-dir", "", "read the upstream documents from this directory instead of the network (tests)")
	names := flag.String("names", "v1,v2,platform", "comma-separated spec names")
	report := flag.String("report", "specdiff-report.md", "file for the full report")
	flag.Parse()

	var full strings.Builder
	drift := false
	for _, name := range strings.Split(*names, ",") {
		pinned, err := readJSON(filepath.Join(*dir, name+".json"))
		if err != nil {
			fatal(err)
		}
		var upstream map[string]any
		if *upDir != "" {
			upstream, err = readJSON(filepath.Join(*upDir, name+".json"))
		} else {
			upstream, err = fetchJSON(*base + "/" + name + "-json")
		}
		if err != nil {
			fatal(err)
		}
		r := Diff(pinned, upstream)
		if !r.Empty() {
			drift = true
		}
		full.WriteString(r.Markdown(name, 0))
	}
	if !drift {
		full.WriteString("\nNo drift: every pinned spec matches upstream.\n")
	}
	out := full.String()
	if err := os.WriteFile(*report, []byte(out), 0o644); err != nil {
		fatal(err)
	}
	// The log and the job summary carry a shortened report; the file has everything.
	fmt.Print(shorten(out, 150))
	if p := os.Getenv("GITHUB_STEP_SUMMARY"); p != "" {
		if f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644); err == nil {
			fmt.Fprintf(f, "## Management API spec drift\n\n%s\n", shorten(out, 300))
			f.Close()
		}
	}
	if drift {
		fmt.Fprintf(os.Stderr, "\nspecdiff: the pinned specs differ from upstream (full report: %s). Review, then re-pin with internal/api/gen/fetch-specs.sh.\n", *report)
		os.Exit(1)
	}
}

func shorten(s string, maxLines int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= maxLines {
		return s
	}
	return strings.Join(lines[:maxLines], "\n") + fmt.Sprintf("\n... %d more lines in the full report\n", len(lines)-maxLines)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "specdiff:", err)
	os.Exit(2)
}

func readJSON(path string) (map[string]any, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var v map[string]any
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return v, nil
}

func fetchJSON(url string) (map[string]any, error) {
	var lastErr error
	for attempt := 1; attempt <= 4; attempt++ {
		v, err := fetchOnce(url)
		if err == nil {
			return v, nil
		}
		lastErr = err
		time.Sleep(time.Duration(attempt) * 2 * time.Second)
	}
	return nil, lastErr
}

func fetchOnce(url string) (map[string]any, error) {
	c := &http.Client{Timeout: 90 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var v map[string]any
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("GET %s: not JSON: %w", url, err)
	}
	if _, ok := v["paths"]; !ok {
		return nil, fmt.Errorf("GET %s: no paths in the document", url)
	}
	return v, nil
}
