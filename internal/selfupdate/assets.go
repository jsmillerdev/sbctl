package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// Release assets of the AWS stack layer. The script and the template are listed in the signed
// SHA256SUMS like the binaries; the manifest also names the template and its hash.
const (
	// AWSDeployAsset is deploy/aws/deploy.sh as a release asset: `supavise-aws-deploy.sh update`
	// brings a CloudFormation stack to the template of the release.
	AWSDeployAsset = "supavise-aws-deploy.sh"
	// DefaultTemplateAsset is the template's asset name when the manifest does not say another.
	DefaultTemplateAsset = "supavise.yaml"

	maxScriptBytes   = 2 << 20
	maxTemplateBytes = 4 << 20
)

// FetchAsset downloads the release asset name and returns its bytes after checking them against
// the checksum the signed list holds, so the caller trusts only what the signature covers. limit
// bounds the download in bytes.
func (ver *Verified) FetchAsset(ctx context.Context, o Options, name string, limit int64) ([]byte, error) {
	want, err := ChecksumFor(ver.Sums, name)
	if err != nil {
		return nil, fmt.Errorf("release %s: %w", ver.Release.Tag, err)
	}
	u := ver.Release.Assets[name]
	if u == "" {
		return nil, fmt.Errorf("release %s has no asset %s", ver.Release.Tag, name)
	}
	b, err := o.fetch(ctx, u, limit)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", name, err)
	}
	if sum := sha256.Sum256(b); hex.EncodeToString(sum[:]) != want {
		return nil, fmt.Errorf("%s does not match its checksum (got %x, signed list says %s)", name, sum, want)
	}
	return b, nil
}

// AWSAssets are the verified script and template of a release.
type AWSAssets struct {
	Script []byte
	// Template is the CloudFormation template; TemplateName is its asset name.
	Template     []byte
	TemplateName string
	// StackRevision is the stack revision the release's manifest says the template brings a stack to.
	StackRevision int
}

// AWSAssets downloads the stack update script and the template of the release and verifies both
// against the signed checksum list. The manifest's hash of the template must agree with the list's:
// a release whose two signed records disagree is refused. A release whose manifest has no aws
// section (it predates the stack layer) has nothing to apply.
func (ver *Verified) AWSAssets(ctx context.Context, o Options) (*AWSAssets, error) {
	m := ver.Manifest
	if m == nil || m.AWS == nil {
		return nil, fmt.Errorf("release %s carries no AWS stack template (its manifest has no aws section), so there is no stack to update from it", ver.Release.Tag)
	}
	name := m.AWS.TemplateAsset
	if name == "" {
		name = DefaultTemplateAsset
	}
	if strings.ContainsAny(name, "/\\") || strings.HasPrefix(name, ".") {
		return nil, fmt.Errorf("release %s names the template %q, which is not a plain asset name", ver.Release.Tag, name)
	}
	if m.AWS.TemplateSHA256 != "" {
		listed, err := ChecksumFor(ver.Sums, name)
		if err != nil {
			return nil, fmt.Errorf("release %s: %w", ver.Release.Tag, err)
		}
		if !strings.EqualFold(listed, m.AWS.TemplateSHA256) {
			return nil, errors.New("the release manifest and the signed checksum list disagree about the template: refusing to use it")
		}
	}
	script, err := ver.FetchAsset(ctx, o, AWSDeployAsset, maxScriptBytes)
	if err != nil {
		return nil, err
	}
	tmpl, err := ver.FetchAsset(ctx, o, name, maxTemplateBytes)
	if err != nil {
		return nil, err
	}
	return &AWSAssets{Script: script, Template: tmpl, TemplateName: name, StackRevision: m.AWS.StackRevision}, nil
}
