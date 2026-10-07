package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// logEntry is one line of the request log.
type logEntry struct {
	Time      string            `json:"time"`
	Method    string            `json:"method"`
	Path      string            `json:"path"`
	Query     string            `json:"query,omitempty"`
	Template  string            `json:"template"`
	Handler   string            `json:"handler"` // real, stub, unknown, auth, preflight, builtin-auth, health
	Status    int               `json:"status"`
	Bytes     int               `json:"bytes"`
	BodyBytes int               `json:"body_bytes,omitempty"`
	Millis    float64           `json:"ms"`
	Auth      string            `json:"auth,omitempty"` // valid, invalid, none
	User      string            `json:"user,omitempty"`
	Ref       string            `json:"ref,omitempty"`
	Origin    string            `json:"origin,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
	SQL       string            `json:"sql,omitempty"` // first 300 characters of a pg-meta query
	Err       string            `json:"err,omitempty"` // upstream failure text
}

// requestLog appends JSON lines to a file; a nil file discards.
type requestLog struct {
	mu sync.Mutex
	f  *os.File
}

func newRequestLog(path string) (*requestLog, error) {
	if path == "" {
		return &requestLog{}, nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	return &requestLog{f: f}, nil
}

func (l *requestLog) write(e logEntry) {
	if l.f == nil {
		return
	}
	b, _ := json.Marshal(e)
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = l.f.Write(append(b, '\n'))
}

func (l *requestLog) close() {
	if l.f != nil {
		_ = l.f.Close()
	}
}

// summarizeCmd prints one markdown row per (method, template) found in a request log. The
// columns match section 9 of docs/research/08-studio-platform-calls.md; the last three are left for
// a person to fill in from the screenshots.
func summarizeCmd(args []string) int {
	if len(args) != 1 {
		usage()
	}
	if err := summarize(args[0], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "mock:", err)
		return 1
	}
	return 0
}

func summarize(path string, out io.Writer) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	type agg struct {
		calls    int
		statuses map[int]int
		handlers map[string]int
	}
	rows := map[string]*agg{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var e logEntry
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.Handler == "preflight" {
			continue
		}
		key := e.Method + " " + e.Template
		a := rows[key]
		if a == nil {
			a = &agg{statuses: map[int]int{}, handlers: map[string]int{}}
			rows[key] = a
		}
		a.calls++
		a.statuses[e.Status]++
		a.handlers[e.Handler]++
	}
	keys := make([]string, 0, len(rows))
	for k := range rows {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		ti, tj := strings.SplitN(keys[i], " ", 2)[1], strings.SplitN(keys[j], " ", 2)[1]
		if ti != tj {
			return ti < tj
		}
		return keys[i] < keys[j]
	})
	fmt.Fprintln(out, "| Method | Path template | Calls | Statuses the mock returned | Answered by | In section 5? | Fail-soft? | Notes |")
	fmt.Fprintln(out, "|---|---|---|---|---|---|---|---|")
	for _, k := range keys {
		a := rows[k]
		method, tpl, _ := strings.Cut(k, " ")
		var st, hs []string
		for code, n := range a.statuses {
			st = append(st, fmt.Sprintf("%d x%d", code, n))
		}
		for h, n := range a.handlers {
			hs = append(hs, fmt.Sprintf("%s x%d", h, n))
		}
		sort.Strings(st)
		sort.Strings(hs)
		fmt.Fprintf(out, "| %s | `%s` | %d | %s | %s | | | |\n", method, tpl, a.calls, strings.Join(st, ", "), strings.Join(hs, ", "))
	}
	return sc.Err()
}

// passwordLiteral matches `PASSWORD '...'` (also `PASSWORD = '...'`, and escape strings such as E-prefixed literals) so that role
// passwords sent from the Database Roles page or the SQL editor do not end up in the request log.
var passwordLiteral = regexp.MustCompile(`(?i)(\bpassword\s*(?:=\s*)?)(?:e'(?:[^'\\]|\\.|'')*'|'(?:[^']|'')*')`)

// redactSQL replaces password literals in a statement with '***'. It runs before the statement is
// truncated for the log, so a literal cannot be cut in half and leak its first characters.
func redactSQL(sql string) string {
	return passwordLiteral.ReplaceAllString(sql, "${1}'***'")
}
