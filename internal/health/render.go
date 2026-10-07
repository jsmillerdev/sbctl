package health

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"
)

// Render writes the human form of the report: the verdict line, then a table of the node's
// components and one of the projects. verbose lists every project; otherwise a node whose
// projects all answer shows their count and only the ones that need attention.
func Render(w io.Writer, r *Report, verbose bool) {
	fmt.Fprintln(w, r.Summary)
	fmt.Fprintln(w)
	t := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(t, "COMPONENT\tSTATE\tDETAIL")
	for _, c := range r.Components {
		fmt.Fprintf(t, "%s\t%s\t%s\n", c.Name, label(c.State), c.Detail)
	}
	t.Flush()

	var rows []ProjectResult
	for _, p := range r.Projects {
		if verbose || p.State == Warn || p.State == Fail || p.State == Info {
			rows = append(rows, p)
		}
	}
	if len(rows) == 0 {
		return
	}
	fmt.Fprintln(w)
	t = tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(t, "PROJECT\tSTATUS\tSTATE\tBACKUP\tDETAIL")
	for _, p := range rows {
		fmt.Fprintf(t, "%s\t%s\t%s\t%s\t%s\n", p.Ref, p.Status, label(p.State), backupLabel(p), p.Detail)
	}
	t.Flush()
}

func label(s State) string {
	switch s {
	case OK:
		return "ok"
	case Info:
		return "note"
	case Warn:
		return "WARN"
	}
	return "FAIL"
}

func backupLabel(p ProjectResult) string {
	b := p.Backup
	switch {
	case b == nil:
		return "-"
	case b.LastCompleted != nil:
		s := humanAge(time.Duration(b.AgeSeconds)*time.Second) + " ago"
		if b.Stale {
			s += " (stale)"
		}
		return s
	case b.Note != "":
		return strings.TrimSpace(b.Note)
	}
	return "-"
}
