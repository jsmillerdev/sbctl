package main

import (
	"bufio"
	"encoding/json"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Versions is the part of internal/versions/versions.yaml this command reads.
type Versions struct {
	Artifacts map[string]string `yaml:"artifacts"`
	Studio    struct {
		Tag string `yaml:"tag"`
	} `yaml:"studio"`
	CLI struct {
		VersionTested string `yaml:"version_tested"`
	} `yaml:"cli"`
}

// ReadVersions parses the pin file.
func ReadVersions(path string) (*Versions, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var v Versions
	if err := yaml.Unmarshal(b, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// ReadPins reads KEY=VALUE lines (comments and blank lines skipped).
func ReadPins(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			out[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return out, sc.Err()
}

// ReadSupabaseJS returns the exact @supabase/supabase-js version of a package.json.
func ReadSupabaseJS(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var p struct {
		Dependencies map[string]string `json:"dependencies"`
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return "", err
	}
	return strings.TrimLeft(p.Dependencies["@supabase/supabase-js"], "^~="), nil
}

var revision = regexp.MustCompile(`-r(\d+)$`)

func trimRevision(s string) string { return revision.ReplaceAllString(s, "") }

// splitTag splits a slim-services tag "<svc>-<version>-r<N>" into its version and revision.
func splitTag(tag, svc string) (version string, rev int, ok bool) {
	rest, found := strings.CutPrefix(tag, svc+"-")
	if !found {
		return "", 0, false
	}
	m := revision.FindStringSubmatch(rest)
	if m == nil {
		return "", 0, false
	}
	rev, _ = strconv.Atoi(m[1])
	version = strings.TrimSuffix(rest, m[0])
	// "pooler-" must not match "pooler-foo-1.0": a version starts with a digit or v<digit>.
	if !regexp.MustCompile(`^v?\d`).MatchString(version) {
		return "", 0, false
	}
	return version, rev, true
}

var digits = regexp.MustCompile(`\d+`)

// natKey turns "v2.140.10" or "17.11.0.004" into [2 140 10] / [17 11 0 4].
func natKey(s string) []int {
	var out []int
	for _, d := range digits.FindAllString(s, -1) {
		n, _ := strconv.Atoi(d)
		out = append(out, n)
	}
	return out
}

func cmpKeys(a, b []int) int {
	for i := 0; i < len(a) || i < len(b); i++ {
		var x, y int
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

// natLess reports whether version a is older than version b.
func natLess(a, b string) bool { return cmpKeys(natKey(a), natKey(b)) < 0 }

// Newest returns the newest of the slim-services tags of one service ("" when none parse).
func Newest(tags []string, svc string) string {
	type cand struct {
		tag string
		key []int
		rev int
	}
	var cs []cand
	for _, t := range tags {
		if v, rev, ok := splitTag(t, svc); ok {
			cs = append(cs, cand{t, natKey(v), rev})
		}
	}
	if len(cs) == 0 {
		return ""
	}
	sort.SliceStable(cs, func(i, j int) bool {
		if c := cmpKeys(cs[i].key, cs[j].key); c != 0 {
			return c < 0
		}
		return cs[i].rev < cs[j].rev
	})
	return cs[len(cs)-1].tag
}

// TagNewer reports whether candidate is a newer release than pinned of the same service.
func TagNewer(pinned, candidate, svc string) bool {
	pv, pr, ok1 := splitTag(pinned, svc)
	cv, cr, ok2 := splitTag(candidate, svc)
	if !ok1 || !ok2 {
		return pinned != candidate
	}
	if c := cmpKeys(natKey(pv), natKey(cv)); c != 0 {
		return c < 0
	}
	return pr < cr
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
