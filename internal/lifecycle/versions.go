package lifecycle

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/registry"
)

// A project runs the service versions recorded in registry.Project.Versions (service name to
// slim-services release tag). A new project records the node's pins; the node's pins move with
// a Supavise release and a project follows only when it is upgraded (upgrade.go), as on hosted
// Supabase, where a project's Postgres version changes when its owner upgrades it.

// TagArtifacts is the part of an artifact store that finds an artifact by release tag. A store
// that has it lets projects run versions other than the node's pins; without it a project
// whose recorded version differs from the pin cannot be rendered. artifacts.Store implements it.
type TagArtifacts interface {
	// DirFor returns the unpacked artifact root of svc at tag, or an error wrapping
	// artifacts.ErrNotFetched.
	DirFor(svc, tag string) (string, error)
}

// TagFetcher is a TagArtifacts that can also download a release tag that is not on disk.
type TagFetcher interface {
	TagArtifacts
	FetchTag(ctx context.Context, svc, tag string) (string, error)
}

// artifactDir is the artifact root p runs svc from: the recorded version, which is the node's
// pin unless the project was created before a node update and not upgraded since. The system
// project belongs to the node and always runs its pins.
func (pl *PostgresPlane) artifactDir(p *registry.Project, svc string) (string, error) {
	tag := p.Versions[svc]
	if tag == "" || p.Ref == config.SystemRef {
		return pl.arts.Dir(svc)
	}
	if pin, err := pl.arts.Tag(svc); err == nil && pin == tag {
		return pl.arts.Dir(svc)
	}
	ta, ok := pl.arts.(TagArtifacts)
	if !ok {
		return "", fmt.Errorf("lifecycle: project %s runs %s %s, which is not the node's pin, and the artifact store cannot find a release by tag", p.Ref, svc, tag)
	}
	dir, err := ta.DirFor(svc, tag)
	if err != nil {
		return "", fmt.Errorf("lifecycle: project %s runs %s %s, which is not on disk: %w (`supavise projects upgrade %s` moves it to the node's versions)", p.Ref, svc, tag, err, p.Ref)
	}
	return dir, nil
}

// NodeVersions returns the tags the node pins for the project services: what a new project
// records and what an upgrade moves a project to.
func (e *Engine) NodeVersions() (map[string]string, error) { return e.versions() }

// EffectiveVersions returns the tag each project service of p runs: its recorded version, or
// the node's pin for a service the project recorded none of.
func (e *Engine) EffectiveVersions(p *registry.Project) (map[string]string, error) {
	node, err := e.versions()
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(node))
	for svc, pin := range node {
		out[svc] = pin
		if p.Ref != config.SystemRef && p.Versions[svc] != "" {
			out[svc] = p.Versions[svc]
		}
	}
	return out, nil
}

// ServiceChange is one service whose version an upgrade changes.
type ServiceChange struct {
	Service string
	From    string
	To      string
}

// DiffVersions lists the project services whose tag differs between from and to, in the order
// of config.ProjectServices. A service that to does not name is left as it is.
func DiffVersions(from, to map[string]string) []ServiceChange {
	var out []ServiceChange
	for _, svc := range config.ProjectServices {
		if t, ok := to[svc]; ok && t != "" && t != from[svc] {
			out = append(out, ServiceChange{Service: svc, From: from[svc], To: t})
		}
	}
	return out
}

var majorRe = regexp.MustCompile(`\d+`)

// tagRe is a release tag without its service prefix: the upstream version (dotted numbers, with
// an optional leading v) and the packaging revision.
var tagRe = regexp.MustCompile(`^v?(\d+(?:\.\d+)*)-r(\d+)$`)

// CompareTags orders two release tags of svc: negative when a is older than b, zero when they are
// the same release, positive when a is newer. The upstream version is compared number by number,
// then the packaging revision. It fails when either tag does not have that shape; callers that
// guard against a downgrade treat that as "cannot tell" and refuse.
func CompareTags(svc, a, b string) (int, error) {
	if a == b {
		return 0, nil
	}
	va, ra, err := parseTag(svc, a)
	if err != nil {
		return 0, err
	}
	vb, rb, err := parseTag(svc, b)
	if err != nil {
		return 0, err
	}
	for i := 0; i < max(len(va), len(vb)); i++ {
		var x, y int
		if i < len(va) {
			x = va[i]
		}
		if i < len(vb) {
			y = vb[i]
		}
		if x != y {
			return cmpInt(x, y), nil
		}
	}
	if ra != rb {
		return cmpInt(ra, rb), nil
	}
	return 0, nil
}

func cmpInt(x, y int) int {
	if x < y {
		return -1
	}
	return 1
}

// parseTag returns the numbers of a tag's upstream version and its packaging revision.
func parseTag(svc, tag string) (version []int, revision int, err error) {
	m := tagRe.FindStringSubmatch(ShortVersion(svc, tag))
	if m == nil {
		return nil, 0, fmt.Errorf("lifecycle: %q is not a release tag of the form <name>-<version>-r<revision>", tag)
	}
	for _, f := range strings.Split(m[1], ".") {
		n, err := strconv.Atoi(f)
		if err != nil {
			return nil, 0, fmt.Errorf("lifecycle: %q: %w", tag, err)
		}
		version = append(version, n)
	}
	revision, err = strconv.Atoi(m[2])
	if err != nil {
		return nil, 0, fmt.Errorf("lifecycle: %q: %w", tag, err)
	}
	return version, revision, nil
}

// PostgresMajor is the major version in a postgres release tag ("postgres-17.11.0.004-r1" is
// 17); 0 when the tag has none.
func PostgresMajor(tag string) int {
	n, _ := strconv.Atoi(majorRe.FindString(strings.TrimPrefix(tag, "postgres-")))
	return n
}

// AppVersion is the Management API's name for a postgres release: Studio and the Supabase CLI
// show what follows "supabase-postgres-". A tag "postgres-17.11.0.004-r1" is
// "supabase-postgres-17.11.0.004-r1" (the packaging revision stays, so a rebuilt artifact of the
// same upstream version is a different app version).
func AppVersion(tag string) string {
	if tag == "" {
		return ""
	}
	return "supabase-" + tag
}

// ShortVersion is a release tag without its service prefix: "auth-v2.195.0-r1" is
// "v2.195.0-r1", "postgres-17.11.0.004-r1" is "17.11.0.004-r1".
func ShortVersion(svc, tag string) string {
	for _, prefix := range []string{config.ArtifactName(svc) + "-", svc + "-"} {
		if strings.HasPrefix(tag, prefix) {
			return strings.TrimPrefix(tag, prefix)
		}
	}
	return tag
}

// mergeVersions returns base with the services of over replaced.
func mergeVersions(base, over map[string]string) map[string]string {
	out := make(map[string]string, len(base))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		if v != "" {
			out[k] = v
		}
	}
	return out
}
