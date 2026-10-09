// Command releasetool is the release engineering helper of the repository: it writes the signed
// release manifest, generates the release notes and edits internal/versions/versions.yaml for the
// nightly bump proposals. deploy/release-assets.sh, .github/workflows/release.yml and
// .github/workflows/bump-proposals.yml call it; it reads and writes files, runs a built binary's
// release-info when the manifest is given one, and never touches the network.
//
//	releasetool manifest -version v1.4.0 -min-upgrade-from v1.2.0 [-min-peer-from v1.3.0] [-wal-compat=false] [-versions FILE] [-patchset FILE] [-dist DIR] [-binary FILE [-require-converge]] [-template FILE] [-out FILE]
//	releasetool notes    -tag v1.4.0 [-prev-tag v1.3.0] -repo owner/name -new FILE [-old FILE] [-log FILE] [-min-upgrade-from v1.2.0]
//	releasetool bump     -versions FILE -service auth -to auth-v2.196.0-r0
//	releasetool table    -old FILE -new FILE        (the service version table alone, for a pull request)
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/supavise/supavise/internal/artifacts"
	"github.com/supavise/supavise/internal/selfupdate"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "manifest":
		err = cmdManifest(os.Args[2:])
	case "notes":
		err = cmdNotes(os.Args[2:])
	case "bump":
		err = cmdBump(os.Args[2:])
	case "table":
		err = cmdTable(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "releasetool:", err)
		if err == errPrerelease {
			os.Exit(3)
		}
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: releasetool manifest|notes|bump|table [flags]  (see the package comment)")
	os.Exit(2)
}

func readVersions(path string) (*artifacts.Versions, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return artifacts.ParseVersions(b)
}

func output(path string, b []byte) error {
	if path == "" || path == "-" {
		_, err := os.Stdout.Write(b)
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

func cmdManifest(args []string) error {
	fs := flag.NewFlagSet("manifest", flag.ExitOnError)
	version := fs.String("version", "", "the release tag, vMAJOR.MINOR.PATCH[-suffix]")
	min := fs.String("min-upgrade-from", "", "the oldest installed version that upgrades straight to this release")
	versions := fs.String("versions", "internal/versions/versions.yaml", "the pin file of this release")
	binary := fs.String("binary", "", "a built supavise binary that can run here: its release-info names the host converge revision (without it the manifest says 0)")
	requireConverge := fs.Bool("require-converge", false, "refuse to write the manifest when the binary does not report its converge revision (a release must: the host-not-converged gating reads it)")
	template := fs.String("template", "", "the CloudFormation template as attached to the release: the manifest names its stack revision, asset and SHA-256; with -binary the two must agree on the stack revision")
	minPeer := fs.String("min-peer-from", "", "the oldest release a joined server may run alongside this one (min_peer_from; empty leaves the field out)")
	walCompat := fs.Bool("wal-compat", true, "false when this release's PostgreSQL cannot read the WAL of the releases before it, so that the servers holding standbys must upgrade first (wal_compat)")
	patchset := fs.String("patchset", "", "studio/PATCHSET of the tree the Studio archives were built from: it must equal studio.patchset in -versions (empty skips the check)")
	dist := fs.String("dist", "", "the directory of the release's assets: every Studio archive in it must be the build -versions pins (empty skips the check)")
	out := fs.String("out", "-", "output file")
	_ = fs.Parse(args)
	v, err := readVersions(*versions)
	if err != nil {
		return err
	}
	if *patchset != "" {
		if err := checkPatchset(*patchset, v); err != nil {
			return err
		}
	}
	if *dist != "" {
		if err := checkStudioAssets(*dist, v); err != nil {
			return err
		}
	}
	// studio is the Studio build, the upstream tag and the patch set (artifacts.Versions.StudioBuild),
	// which is the Studio pin the release's binary reports: every reader of the manifest compares the
	// two as strings (nodeupgrade.CheckManifestPins), so the meaning of the field and the schema stay.
	m := &selfupdate.Manifest{Schema: selfupdate.ManifestSchema, Version: *version, MinUpgradeFrom: *min,
		MinPeerFrom: *minPeer, Artifacts: v.Artifacts, Studio: v.StudioBuild()}
	if !*walCompat {
		m.WALCompat = walCompat
	}
	var built probed
	if *binary != "" {
		built = probe(*binary)
		if built.Why != "" {
			if *requireConverge {
				return fmt.Errorf("%s (a release must say which host layer its binary expects; build the binary from a tree that has `supavise release-info` report converge_revision)", built.Why)
			}
			fmt.Fprintln(os.Stderr, "releasetool: warning:", built.Why)
		}
		// Every node refuses a binary whose Studio pin is not the manifest's (CheckManifestPins), so a
		// release that would be refused everywhere is stopped here.
		if built.HasStudio && built.Studio != m.Studio {
			return fmt.Errorf("the binary %s pins the Studio build %q and %s pins %q: they are not of one release", *binary, built.Studio, *versions, m.Studio)
		}
		m.Host = &selfupdate.ManifestHost{ConvergeRevision: built.Converge}
	}
	if *template != "" {
		if m.AWS, err = awsOf(*template); err != nil {
			return err
		}
		// The binary and the template are one release: a binary built before the template's revision
		// was raised (or the other way round) would be refused by every node that stages it.
		if built.HasStack && built.Stack != m.AWS.StackRevision {
			return fmt.Errorf("the binary %s reports stack revision %d (infra_revision) and %s is at revision %d: they are not of one release", *binary, built.Stack, *template, m.AWS.StackRevision)
		}
	}
	b, err := m.Marshal()
	if err != nil {
		return err
	}
	return output(*out, b)
}

// checkPatchset compares studio/PATCHSET (the patch set in the name of the Studio archives that
// studio/build.sh wrote) with studio.patchset of the versions the binary was built with.
func checkPatchset(path string, v *artifacts.Versions) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if n != v.Studio.Patchset {
		return fmt.Errorf("%s says patch set %d and versions.yaml studio.patchset says %d: the release would ship Studio archives of one build and a binary that runs another; change them together", path, n, v.Studio.Patchset)
	}
	return nil
}

// checkStudioAssets refuses a release directory that holds a Studio archive of a build other than
// the one v pins. The signed list covers every archive in it, and a reader takes the first Studio
// line for its platform, so a stale or extra archive would be handed to nodes as the release's.
func checkStudioAssets(dir string, v *artifacts.Versions) error {
	names, err := filepath.Glob(filepath.Join(dir, "supavise-studio-*"))
	if err != nil {
		return err
	}
	build := v.StudioBuild()
	for _, path := range names {
		name := filepath.Base(path)
		arch, ok := strings.CutPrefix(name, "supavise-studio-"+build+"-linux-")
		if ok {
			arch, ok = strings.CutSuffix(arch, ".tar.zst")
		}
		if !ok || name != artifacts.StudioAsset(build, "linux-"+arch) || strings.ContainsAny(arch, "-.") || arch == "" {
			return fmt.Errorf("%s holds %s, and this release pins the Studio build %s (supavise-studio-%s-linux-<arch>.tar.zst): remove the archive of the other build", dir, name, build, build)
		}
	}
	return nil
}

func cmdBump(args []string) error {
	fs := flag.NewFlagSet("bump", flag.ExitOnError)
	file := fs.String("versions", "internal/versions/versions.yaml", "the pin file to edit in place")
	svc := fs.String("service", "", "artifact name (postgres, auth, ...) or studio")
	to := fs.String("to", "", "the new pin: a slim-services tag, or the Studio tag for studio")
	_ = fs.Parse(args)
	b, err := os.ReadFile(*file)
	if err != nil {
		return err
	}
	nb, err := Bump(b, *svc, *to)
	if err != nil {
		return err
	}
	return os.WriteFile(*file, nb, 0o644)
}

func cmdTable(args []string) error {
	fs := flag.NewFlagSet("table", flag.ExitOnError)
	oldF := fs.String("old", "", "the pin file before")
	newF := fs.String("new", "", "the pin file after")
	_ = fs.Parse(args)
	oldV, err := readVersions(*oldF)
	if err != nil {
		return err
	}
	newV, err := readVersions(*newF)
	if err != nil {
		return err
	}
	_, err = fmt.Fprint(os.Stdout, ServiceTable(Diff(oldV, newV)))
	return err
}

func cmdNotes(args []string) error {
	fs := flag.NewFlagSet("notes", flag.ExitOnError)
	tag := fs.String("tag", "", "this release's tag")
	prev := fs.String("prev-tag", "", "the previous release's tag (empty for the first release)")
	repo := fs.String("repo", "supavise/supavise", "owner/name, for links")
	oldF := fs.String("old", "", "internal/versions/versions.yaml at the previous tag (empty for the first release)")
	newF := fs.String("new", "internal/versions/versions.yaml", "internal/versions/versions.yaml at this tag")
	logF := fs.String("log", "", "file of `git log --no-merges --pretty=format:'%H%x09%s' PREV..TAG` lines")
	min := fs.String("min-upgrade-from", "", "min_upgrade_from of this release")
	_ = fs.Parse(args)
	in := NotesInput{Tag: *tag, PrevTag: *prev, Repo: *repo, MinUpgradeFrom: *min}
	var err error
	if in.New, err = readVersions(*newF); err != nil {
		return err
	}
	if *oldF != "" {
		if in.Old, err = readVersions(*oldF); err != nil {
			return err
		}
	}
	if *logF != "" {
		b, err := os.ReadFile(*logF)
		if err != nil {
			return err
		}
		in.Commits = ParseLog(string(b))
	}
	_, err = fmt.Fprint(os.Stdout, strings.TrimLeft(RenderNotes(in), "\n"))
	return err
}
