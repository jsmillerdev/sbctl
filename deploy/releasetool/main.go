// Command releasetool is the release engineering helper of the repository: it writes the signed
// release manifest, generates the release notes and edits internal/versions/versions.yaml for the
// nightly bump proposals. deploy/release-assets.sh, .github/workflows/release.yml and
// .github/workflows/bump-proposals.yml call it; it reads and writes files and never touches the
// network.
//
//	releasetool manifest -version v1.4.0 -min-upgrade-from v1.2.0 [-versions FILE] [-out FILE]
//	releasetool notes    -tag v1.4.0 [-prev-tag v1.3.0] -repo owner/name -new FILE [-old FILE] [-log FILE] [-min-upgrade-from v1.2.0]
//	releasetool bump     -versions FILE -service auth -to auth-v2.196.0-r0
//	releasetool table    -old FILE -new FILE        (the service version table alone, for a pull request)
package main

import (
	"flag"
	"fmt"
	"os"
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
	out := fs.String("out", "-", "output file")
	_ = fs.Parse(args)
	v, err := readVersions(*versions)
	if err != nil {
		return err
	}
	m := &selfupdate.Manifest{Schema: selfupdate.ManifestSchema, Version: *version, MinUpgradeFrom: *min,
		Artifacts: v.Artifacts, Studio: v.Studio.Tag}
	b, err := m.Marshal()
	if err != nil {
		return err
	}
	return output(*out, b)
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
