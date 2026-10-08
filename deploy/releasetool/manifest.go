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

// convergeRevision asks a built binary for the host layer revision it expects: the field
// converge_revision of `supavise release-info --json`. A binary that does not report one (it
// predates the field) or cannot run here (a binary for the other architecture) gives 0 and a
// reason, which the caller prints as a warning; the manifest then says the host layer has no
// revision, which is what such a binary means.
func convergeRevision(binary string) (rev int, why string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, binary, "release-info", "--json").Output()
	if err != nil {
		return 0, fmt.Sprintf("%s release-info did not run: %v", binary, err)
	}
	var info map[string]json.RawMessage
	if err := json.Unmarshal(out, &info); err != nil {
		return 0, fmt.Sprintf("%s release-info printed no JSON: %v", binary, err)
	}
	raw, ok := info["converge_revision"]
	if !ok {
		return 0, fmt.Sprintf("%s release-info reports no converge_revision: the manifest will say the host layer has revision 0", binary)
	}
	if err := json.Unmarshal(raw, &rev); err != nil || rev < 0 {
		return 0, fmt.Sprintf("%s release-info: converge_revision %s is not a revision number", binary, raw)
	}
	return rev, ""
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
