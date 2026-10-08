package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"github.com/supavise/supavise/internal/infra"
	"github.com/supavise/supavise/internal/selfupdate"
)

// probed is what a built binary says about itself in `release-info --json` that the manifest
// needs. A field the binary does not report stays at its zero value, and Why says what was missing.
type probed struct {
	// Converge is the host layer revision (converge_revision). Why is empty when the binary reported
	// one.
	Converge int
	Why      string
	// Stack is the AWS stack revision (infra_revision); HasStack is false when the binary reports none.
	Stack    int
	HasStack bool
}

// probe runs the binary's `release-info --json` and reads the revisions. The binary is a build of
// the project's own code and needs none of the environment of the release job, which holds the
// signing key, so it runs with a minimal one. A binary that cannot run here (one for the other
// architecture) or predates a field gets a reason in Why, which the caller prints as a warning, or
// refuses with in a release that needs the field.
func probe(binary string) probed {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "release-info", "--json")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
	out, err := cmd.Output()
	if err != nil {
		return probed{Why: fmt.Sprintf("%s release-info did not run: %v", binary, err)}
	}
	var info map[string]json.RawMessage
	if err := json.Unmarshal(out, &info); err != nil {
		return probed{Why: fmt.Sprintf("%s release-info printed no JSON: %v", binary, err)}
	}
	var p probed
	if raw, ok := info["infra_revision"]; ok {
		if err := json.Unmarshal(raw, &p.Stack); err != nil || p.Stack < 0 {
			p.Stack = 0
		} else {
			p.HasStack = true
		}
	}
	raw, ok := info["converge_revision"]
	if !ok {
		p.Why = fmt.Sprintf("%s release-info reports no converge_revision: the manifest will say the host layer has revision 0", binary)
		return p
	}
	if err := json.Unmarshal(raw, &p.Converge); err != nil || p.Converge < 0 {
		p.Converge = 0
		p.Why = fmt.Sprintf("%s release-info: converge_revision %s is not a revision number", binary, raw)
	}
	return p
}

// convergeRevision asks a built binary for the host layer revision it expects: the field
// converge_revision of `supavise release-info --json`. A binary that does not report one (it
// predates the field) or cannot run here (a binary for the other architecture) gives 0 and a
// reason, which the caller prints as a warning; the manifest then says the host layer has no
// revision, which is what such a binary means.
func convergeRevision(binary string) (rev int, why string) {
	p := probe(binary)
	return p.Converge, p.Why
}

var templateRevisionRe = regexp.MustCompile(`(?m)^  InfraRevision:\n    Description: .*\n    Value: "(\d+)"$`)

// awsOf describes the CloudFormation template that is attached to the release: the stack
// revision it is at (which must be the one internal/infra says this release needs), the asset
// name and the SHA-256 of the file as it is attached, stamped.
func awsOf(templatePath string) (*selfupdate.ManifestAWS, error) {
	b, err := os.ReadFile(templatePath)
	if err != nil {
		return nil, err
	}
	m := templateRevisionRe.FindSubmatch(b)
	if m == nil {
		return nil, fmt.Errorf("%s has no InfraRevision output", templatePath)
	}
	if n, _ := strconv.Atoi(string(m[1])); n != infra.Current {
		return nil, fmt.Errorf("%s is at infrastructure revision %d and internal/infra says this release needs %d: raise them together", templatePath, n, infra.Current)
	}
	sum := sha256.Sum256(b)
	return &selfupdate.ManifestAWS{StackRevision: infra.Current, TemplateAsset: filepath.Base(templatePath), TemplateSHA256: hex.EncodeToString(sum[:])}, nil
}
