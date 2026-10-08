// Package infra tells a node what its cloud infrastructure lacks for the features of the release
// it runs. On AWS the node was created by a CloudFormation stack at some revision; each release
// needs a revision, and what lies between them is the gap: a Storage bucket and its key, a rule
// for the mesh port, the fencing permissions. The node reads its stack's revision from the
// instance tags (instance metadata, no IAM); a stack from before revisions carries none and counts as revision 1.
// This file is the contract; the revision table is in revisions.go and the detection in detect.go.
package infra

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

// Revision is the stack revision this release needs: what `supavise release-info` reports as
// infra_revision and the release manifest names as aws.stack_revision. A stack is at revision 1
// (it predates revisions) or at the revision its template carries. It is Current under the name the
// contract has used, so that the two cannot be raised apart.
const Revision = Current

// Missing is one thing the stack lacks.
type Missing struct {
	// Capability is the stable name of the stack capability ("storage-bucket", "peer-rule").
	Capability string `json:"capability"`
	// Title is the sentence shown to the operator ("Storage bucket and access key").
	Title string `json:"title"`
	// Why says what wants it, as the words that follow the title in the status block
	// ("needed for server failover", "needed to add a second server").
	Why string `json:"why"`
}

// Report is the gap between the stack and the release.
type Report struct {
	// Platform is "aws" for a node created by a stack; empty on any other host, where there is no gap to report.
	Platform string `json:"platform,omitempty"`
	// Stack is the CloudFormation stack's name, "" when the node does not know it yet.
	Stack string `json:"stack,omitempty"`
	// Have is the stack's infrastructure revision and Need the one this release requires.
	Have int `json:"have"`
	Need int `json:"need"`
	// Features are the names of what the missing capabilities hold back ("replicas", "S3 Storage").
	Features []string  `json:"features,omitempty"`
	Missing  []Missing `json:"missing,omitempty"`
	// Fix is the exact command that closes the gap ("sudo -E supavise upgrade --aws").
	Fix string `json:"fix,omitempty"`
	// Enabled names the capabilities the stack has switched on, as its instance tag
	// supavise:caps lists them ("storage-role", "fencing", "peer-rule"). A stack without the tag
	// has none.
	Enabled []string `json:"enabled,omitempty"`
}

// Has reports whether the stack has switched the capability on. A feature that needs one
// (automatic failover needs "fencing") asks here.
func (r Report) Has(capability string) bool {
	for _, c := range r.Enabled {
		if c == capability {
			return true
		}
	}
	return false
}

// Behind reports whether the stack lacks anything this release needs.
func (r Report) Behind() bool { return r.Platform != "" && r.Have < r.Need }

// Render writes the block of `supavise status`:
//
//	Infrastructure  AWS stack "supavise" is at revision 1; this release needs 2 for: replicas, S3 Storage
//	  missing  Storage bucket and access key         (needed for server failover)
//	  missing  Peer rule in the security group       (needed to add a second server)
//	  Fix: sudo -E supavise upgrade --aws
//
// A report that is not behind writes nothing.
func (r Report) Render(w io.Writer) {
	if !r.Behind() {
		return
	}
	stack := "AWS stack"
	if r.Stack != "" {
		stack += fmt.Sprintf(" %q", r.Stack)
	}
	head := fmt.Sprintf("Infrastructure  %s is at revision %d; this release needs %d", stack, r.Have, r.Need)
	if len(r.Features) > 0 {
		head += " for: " + strings.Join(r.Features, ", ")
	}
	fmt.Fprintln(w, head)
	tw := tabwriter.NewWriter(w, 0, 4, 3, ' ', 0)
	for _, m := range r.Missing {
		fmt.Fprintf(tw, "  missing  %s\t(%s)\n", m.Title, m.Why)
	}
	tw.Flush()
	if r.Fix != "" {
		fmt.Fprintf(w, "  Fix: %s\n", r.Fix)
	}
}
