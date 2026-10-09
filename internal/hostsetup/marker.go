package hostsetup

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/supavise/supavise/internal/fsutil"
)

// MarkerName is the file converge writes into the state directory when it has completed.
const MarkerName = "converged"

// MarkerPath is where the marker of a node with state directory stateDir lives
// (/var/lib/supavise/converged).
func MarkerPath(stateDir string) string { return filepath.Join(stateDir, MarkerName) }

// Marker records the converge revision a node completed.
type Marker struct {
	Revision int
	// Version and At say which release's converge it was and when; they are for people.
	Version string
	At      time.Time
}

// ReadMarker reads the marker. A node that never converged has none, which is revision 0 and not
// an error; a file that cannot be read or parsed is an error.
func ReadMarker(stateDir string) (Marker, error) {
	b, err := os.ReadFile(MarkerPath(stateDir))
	if errors.Is(err, fs.ErrNotExist) {
		return Marker{}, nil
	}
	if err != nil {
		return Marker{}, err
	}
	return parseMarker(b)
}

// parseMarker reads "key=value" lines; a file that holds only a number is that revision.
func parseMarker(b []byte) (Marker, error) {
	var m Marker
	seen := false
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			k, v = "revision", line
		}
		switch strings.TrimSpace(k) {
		case "revision":
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil || n < 0 {
				return Marker{}, fmt.Errorf("%s: revision %q is not a number", MarkerName, strings.TrimSpace(v))
			}
			m.Revision, seen = n, true
		case "version":
			m.Version = strings.TrimSpace(v)
		case "at":
			m.At, _ = time.Parse(time.RFC3339, strings.TrimSpace(v))
		}
	}
	if !seen {
		return Marker{}, fmt.Errorf("%s names no revision", MarkerName)
	}
	return m, nil
}

// WriteMarker records m through a temporary file in the same directory, so a reader sees the old
// marker or the whole new one. The file is world-readable: the daemon, which is not root, reads it.
func WriteMarker(stateDir string, m Marker) error {
	var b strings.Builder
	fmt.Fprintf(&b, "revision=%d\n", m.Revision)
	if m.Version != "" {
		fmt.Fprintf(&b, "version=%s\n", m.Version)
	}
	if !m.At.IsZero() {
		fmt.Fprintf(&b, "at=%s\n", m.At.UTC().Format(time.RFC3339))
	}
	return fsutil.WriteFile(MarkerPath(stateDir), []byte(b.String()), 0o644, fsutil.Options{MkdirMode: 0o750})
}

// Status is a node's converge state against this binary's revision.
type Status struct {
	// Have is the revision the marker records, Want the revision of this binary.
	Have, Want int
	// Known is false when the marker could not be read; Err says why. A caller that cannot
	// tell does not claim the node is behind.
	Known bool
	Err   error
}

// Behind reports whether the node is known to be behind this binary's revision.
func (s Status) Behind() bool { return s.Known && s.Have < s.Want }

// StatusOf reads the marker of the node with state directory stateDir.
func StatusOf(stateDir string) Status {
	m, err := ReadMarker(stateDir)
	if err != nil {
		return Status{Want: Revision, Err: err}
	}
	return Status{Have: m.Revision, Want: Revision, Known: true}
}
