package infra

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/supavise/supavise/internal/notimpl"
)

func TestRender(t *testing.T) {
	r := Report{Platform: "aws", Stack: "supavise", Have: 1, Need: 2, Features: []string{"replicas", "S3 Storage"}, Fix: "sudo -E supavise upgrade --aws",
		Missing: []Missing{
			{Capability: "storage-bucket", Title: "Storage bucket and access key", Why: "needed for server failover"},
			{Capability: "peer-rule", Title: "Peer rule in the security group", Why: "needed to add a second server"},
		}}
	var b strings.Builder
	r.Render(&b)
	want := `Infrastructure  AWS stack "supavise" is at revision 1; this release needs 2 for: replicas, S3 Storage
  missing  Storage bucket and access key     (needed for server failover)
  missing  Peer rule in the security group   (needed to add a second server)
  Fix: sudo -E supavise upgrade --aws
`
	if got := b.String(); got != want {
		t.Errorf("render:\n%s\nwant:\n%s", got, want)
	}
	b.Reset()
	Report{Platform: "aws", Have: 2, Need: 2}.Render(&b)
	Report{Have: 1, Need: 2}.Render(&b) // not AWS
	if b.Len() != 0 {
		t.Errorf("a report that is not behind wrote %q", b.String())
	}
}

func TestGapIsNotImplementedYet(t *testing.T) {
	if _, err := Gap(context.Background()); !errors.Is(err, notimpl.Err) {
		t.Fatalf("Gap: %v", err)
	}
}
