// Package nodeupgrade is `supavise upgrade` and `supavise rollback`: it moves a whole node, the
// binary, the shared services and the projects, onto a newer release of Supavise, and back.
//
// A release is a tested bundle: a binary and the Supabase service versions pinned in its
// versions.yaml. The package holds the parts that decide things (what a release changes, what
// the node refuses, in which order the upgrade goes and how it is undone), and talks to the
// machine through the Host interface, so that the order and the failure paths run in unit tests
// without a server. cmd/supavise implements Host for a Linux node.
package nodeupgrade

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/jsmillerdev/supavise/internal/artifacts"
	"github.com/jsmillerdev/supavise/internal/config"
	"github.com/jsmillerdev/supavise/internal/registry"
	"github.com/jsmillerdev/supavise/internal/selfupdate"
	"github.com/jsmillerdev/supavise/internal/versions"
)

// Info describes one release as its binary reports it (`supavise release-info`): the versions
// it pins and the registry schema it expects. The upgrade runs the new binary to read this,
// so the plan comes from the versions.yaml that was built into the binary, not from a list
// next to it.
type Info struct {
	// Version is the release tag the binary was built as ("v1.4.0").
	Version  string `json:"version"`
	Platform string `json:"platform,omitempty"`
	// Pins maps a service (config.Svc*) to the slim-services release tag it runs, Studio's build
	// tag included under "studio".
	Pins map[string]string `json:"pins"`
	// RegistrySchema is the newest registry migration the binary embeds (a label for people);
	// RegistryMigrations lists all of them: the binary can run on a registry that holds these
	// and no others.
	RegistrySchema     string   `json:"registry_schema"`
	RegistryMigrations []string `json:"registry_migrations"`
}

// PinsOf maps a versions.yaml to service names.
func PinsOf(v *artifacts.Versions) map[string]string {
	pins := map[string]string{}
	for name, tag := range v.Artifacts {
		pins[serviceOfArtifact(name)] = tag
	}
	if v.Studio.Tag != "" {
		pins[config.SvcStudio] = v.Studio.Tag
	}
	return pins
}

// serviceOfArtifact maps a release name in versions.yaml (auth, pooler) to a service name.
func serviceOfArtifact(name string) string {
	switch name {
	case "auth":
		return config.SvcGoTrue
	case "pooler":
		return config.SvcSupavisor
	}
	return name
}

// OwnInfo is the Info of the running binary.
func OwnInfo(version string) (*Info, error) {
	v, err := artifacts.ParseVersions(versions.VersionsYAML)
	if err != nil {
		return nil, err
	}
	return &Info{Version: version, Platform: runtime.GOOS + "-" + runtime.GOARCH, Pins: PinsOf(v), RegistrySchema: registry.SchemaVersion(), RegistryMigrations: registry.MigrationNames()}, nil
}

// ParseInfo reads the JSON `supavise release-info --json` prints.
func ParseInfo(b []byte) (*Info, error) {
	var i Info
	if err := json.Unmarshal(b, &i); err != nil {
		return nil, fmt.Errorf("release info: %w", err)
	}
	if i.Version == "" || len(i.Pins) == 0 {
		return nil, fmt.Errorf("release info: no version or no pins in %q", strings.TrimSpace(string(b)))
	}
	return &i, nil
}

// ProbeInfo runs `<binary> release-info --json`. A binary built before the command existed fails
// here; the caller then knows less (the installed release's pins come from the rendered units).
func ProbeInfo(ctx context.Context, binary string) (*Info, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, binary, "release-info", "--json").Output()
	if err != nil {
		return nil, fmt.Errorf("%s release-info: %w", binary, err)
	}
	return ParseInfo(out)
}

// serviceOrder lists services the way the plan prints them: the project services first, then the
// shared services in the order the daemon starts them.
var serviceOrder = []string{
	config.SvcPostgres, config.SvcGoTrue, config.SvcPostgREST,
	config.SvcPGMeta, config.SvcSupavisor, config.SvcRealtime, config.SvcStorage, config.SvcStudio, config.SvcImgproxy, config.SvcEdgeRuntime,
}

// SharedServices are the shared services in the order they are rolled: postgres-meta needs
// nothing, the three multi-tenant services need the system cluster, Studio is a client of the API.
var SharedServices = []string{config.SvcPGMeta, config.SvcSupavisor, config.SvcRealtime, config.SvcStorage, config.SvcStudio, config.SvcImgproxy, config.SvcEdgeRuntime}

// ServiceMove is one service whose release changes.
type ServiceMove struct {
	Service  string
	From, To string
}

// DiffPins lists the services whose tag differs between from and to, in plan order. A service that
// to does not pin is left alone; one that from lacks is a move from "".
func DiffPins(from, to map[string]string) []ServiceMove {
	var out []ServiceMove
	seen := map[string]bool{}
	for _, svc := range serviceOrder {
		seen[svc] = true
		if t := to[svc]; t != "" && t != from[svc] {
			out = append(out, ServiceMove{Service: svc, From: from[svc], To: t})
		}
	}
	var rest []string
	for svc := range to {
		if !seen[svc] {
			rest = append(rest, svc)
		}
	}
	sort.Strings(rest)
	for _, svc := range rest {
		if t := to[svc]; t != "" && t != from[svc] {
			out = append(out, ServiceMove{Service: svc, From: from[svc], To: t})
		}
	}
	return out
}

// CheckManifestPins compares the pins a binary reports with those in the signed release manifest
// (artifacts by release name, as versions.yaml keys them, and Studio's tag). A manifest that lists
// none (an older manifest) has nothing to compare.
func CheckManifestPins(info *Info, artifacts map[string]string, studio string) error {
	for name, tag := range artifacts {
		if got := info.Pins[serviceOfArtifact(name)]; got != tag {
			return fmt.Errorf("the release manifest pins %s %s and the binary of %s pins %q: refusing a binary that is not the one the signed manifest describes", name, tag, info.Version, got)
		}
	}
	if studio != "" && info.Pins[config.SvcStudio] != studio {
		return fmt.Errorf("the release manifest pins Studio %s and the binary of %s pins %q: refusing a binary that is not the one the signed manifest describes", studio, info.Version, info.Pins[config.SvcStudio])
	}
	return nil
}

// compareVersion orders two release tags ("v1.2.0"); ok is false when either is not one.
func compareVersion(a, b string) (int, bool) { return selfupdate.Compare(a, b) }
