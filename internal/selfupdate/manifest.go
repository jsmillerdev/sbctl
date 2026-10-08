package selfupdate

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
)

// ManifestAsset is the release manifest: a small JSON file attached to every release and listed
// in SHA256SUMS, so the signature over the list covers it. Download it only through Fetch or
// Verify, which check that.
const ManifestAsset = "supavise-release.json"

// ManifestSchema is the schema number this binary reads. A manifest with a higher number is
// refused: its meaning is not known here. A change that adds a field keeps the number; a change
// that alters the meaning of a field raises it.
const ManifestSchema = 1

// Manifest is the signed description of a release (deploy/releasetool writes it,
// deploy/release-assets.sh signs it).
//
//	{
//	  "schema": 1,
//	  "version": "v1.4.0",
//	  "min_upgrade_from": "v1.2.0",
//	  "artifacts": {"postgres": "postgres-17.11.0.004-r1", "auth": "auth-v2.195.0-r1", ...},
//	  "studio": "2026.10.05-sha-94b8b06"
//	}
//
// version is the release tag; Fetch refuses a release whose tag is another (an older signed
// release attached to a newer tag). min_upgrade_from is the oldest installed version that may
// upgrade to this release directly: a node on an older one must go through min_upgrade_from first
// (CheckUpgradeFrom says so). artifacts and studio are the Supabase service releases the binary
// installs (internal/versions/versions.yaml of that release), so that a plan can show what an
// upgrade changes before it downloads anything else.
type Manifest struct {
	Schema         int               `json:"schema"`
	Version        string            `json:"version"`
	MinUpgradeFrom string            `json:"min_upgrade_from"`
	Artifacts      map[string]string `json:"artifacts,omitempty"`
	Studio         string            `json:"studio,omitempty"`
	// Host and AWS name what a node needs beyond the binary: the host layer's converge revision
	// (`supavise system converge`) and the AWS stack revision with the signed template that
	// brings a stack to it. MinPeerFrom is the oldest release a joined server may run alongside
	// this one. Readers that predate the fields ignore them, so the schema stays 1.
	Host        *ManifestHost `json:"host,omitempty"`
	AWS         *ManifestAWS  `json:"aws,omitempty"`
	MinPeerFrom string        `json:"min_peer_from,omitempty"`
}

// ManifestHost is the host layer a release expects.
type ManifestHost struct {
	ConvergeRevision int `json:"converge_revision"`
}

// ManifestAWS is the CloudFormation stack a release expects, and the release asset that holds
// its template.
type ManifestAWS struct {
	StackRevision  int    `json:"stack_revision"`
	TemplateAsset  string `json:"template_asset"`
	TemplateSHA256 string `json:"template_sha256"`
}

var versionRe = regexp.MustCompile(`^v\d+\.\d+\.\d+(-[0-9A-Za-z.]+)?$`)

// Validate checks the fields every consumer relies on.
func (m *Manifest) Validate() error {
	if m.Schema != ManifestSchema {
		if m.Schema > ManifestSchema {
			return fmt.Errorf("release manifest has schema %d, this binary reads schema %d: update with the installer of that release", m.Schema, ManifestSchema)
		}
		return fmt.Errorf("release manifest has schema %d, want %d", m.Schema, ManifestSchema)
	}
	if !versionRe.MatchString(m.Version) {
		return fmt.Errorf("release manifest version %q is not vMAJOR.MINOR.PATCH[-suffix]", m.Version)
	}
	if !versionRe.MatchString(m.MinUpgradeFrom) {
		return fmt.Errorf("release manifest min_upgrade_from %q is not vMAJOR.MINOR.PATCH[-suffix]", m.MinUpgradeFrom)
	}
	v, _ := parseVersion(m.Version)
	min, _ := parseVersion(m.MinUpgradeFrom)
	for i := range v {
		if min[i] != v[i] {
			if min[i] > v[i] {
				return fmt.Errorf("release manifest min_upgrade_from %s is newer than its version %s", m.MinUpgradeFrom, m.Version)
			}
			break
		}
	}
	return nil
}

// ParseManifest reads and validates a manifest. Unknown fields are ignored, so that a later
// release can add one under the same schema.
func ParseManifest(b []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("release manifest: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// Marshal renders the manifest as the bytes that are attached to the release: indented JSON with a
// trailing newline and the keys of artifacts in sorted order, so the same input gives the same
// bytes.
func (m *Manifest) Marshal() ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// CheckUpgradeFrom reports whether a node running current may upgrade straight to this release.
// A current version that is not a release ("dev", a build made after a tag) cannot be judged and
// passes: the tag it descends from is what counts, and a suffix is dropped before the comparison,
// as Newer does.
func (m *Manifest) CheckUpgradeFrom(current string) error {
	cur, ok := parseVersion(current)
	if !ok {
		return nil
	}
	min, _ := parseVersion(m.MinUpgradeFrom)
	for i := range cur {
		if cur[i] != min[i] {
			if cur[i] > min[i] {
				return nil
			}
			return fmt.Errorf("release %s cannot be installed over %s: it upgrades from %s or later. Upgrade to %s first (supavise upgrade --version %s), then to %s",
				m.Version, current, m.MinUpgradeFrom, m.MinUpgradeFrom, m.MinUpgradeFrom, m.Version)
		}
	}
	return nil
}

// Verified is a release whose checksum list carries a valid signature and whose manifest matches
// that list and the release tag.
type Verified struct {
	Release  *Release
	Sums     []byte    // the signed checksum list
	Manifest *Manifest // the signed manifest
	// Key is which key verified the signature: CurrentKey or NextKey.
	Key int
}

// Fetch resolves the release o selects (the latest stable one, or o.Tag) and verifies it as Verify
// does. It is what `supavise upgrade` calls to read the manifest of the release it would install.
func Fetch(ctx context.Context, o Options) (*Verified, error) {
	keys, err := o.keys()
	if err != nil {
		return nil, err
	}
	rel, err := Latest(ctx, o)
	if err != nil {
		return nil, err
	}
	return o.verify(ctx, rel, keys)
}

// Verify downloads the checksum list, its signature and the manifest of rel, and checks all
// three: the signature of the list against o's keys (the embedded current and next key when none
// are given), the manifest against the checksum the signed list holds for it, and the manifest's
// version against the release tag. It returns before anything else is downloaded.
func Verify(ctx context.Context, o Options, rel *Release) (*Verified, error) {
	keys, err := o.keys()
	if err != nil {
		return nil, err
	}
	return o.verify(ctx, rel, keys)
}

func (o *Options) verify(ctx context.Context, rel *Release, keys []ed25519.PublicKey) (*Verified, error) {
	for _, need := range []string{SumsAsset, SigAsset, ManifestAsset} {
		if rel.Assets[need] == "" {
			return nil, fmt.Errorf("release %s has no asset %s", rel.Tag, need)
		}
	}
	sums, err := o.fetch(ctx, rel.Assets[SumsAsset], maxSumsBytes)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", SumsAsset, err)
	}
	sig, err := o.fetch(ctx, rel.Assets[SigAsset], 1024)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", SigAsset, err)
	}
	which, err := VerifySumsAny(keys, sums, sig)
	if err != nil {
		return nil, err
	}
	want, err := ChecksumFor(sums, ManifestAsset)
	if err != nil {
		return nil, fmt.Errorf("release %s: %w", rel.Tag, err)
	}
	raw, err := o.fetch(ctx, rel.Assets[ManifestAsset], maxManifestBytes)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", ManifestAsset, err)
	}
	if sum := sha256.Sum256(raw); hex.EncodeToString(sum[:]) != want {
		return nil, fmt.Errorf("%s does not match its checksum (got %x, signed list says %s)", ManifestAsset, sum, want)
	}
	m, err := ParseManifest(bytes.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("release %s: %w", rel.Tag, err)
	}
	if m.Version != rel.Tag {
		return nil, fmt.Errorf("release %s carries the signed manifest of %s: refusing to use it (an older signed release attached to a newer tag would be a downgrade)", rel.Tag, m.Version)
	}
	return &Verified{Release: rel, Sums: sums, Manifest: m, Key: which}, nil
}

const maxManifestBytes = 1 << 20
