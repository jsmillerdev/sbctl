package functions

import (
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/OWNER/sbctl/internal/config"
)

// File and directory names under <state>/system/edge-runtime/tenants/<ref>/. The Deno main service reads exactly
// these (functions-main/src/projects.ts); change both together.
const (
	// EnvFileName holds the project's JWT secret, SUPABASE_* values and secrets (0600).
	EnvFileName = "functions-env.json"
	// FunctionsDirName holds one symlink per deployed function (functions/<slug>) to
	// the current generation under functions/.gen.
	FunctionsDirName = "functions"
	genDirName       = ".gen"
	// MetaFileName is the metadata file inside a generation.
	MetaFileName = ".sbctl-function.json"
)

// ProjectDir is <tenants>/<ref>: everything this package keeps for one project.
func ProjectDir(cfg *config.Config, ref string) string {
	return filepath.Join(cfg.Paths().FunctionsRoot(), ref)
}

// FunctionsDir is <tenants>/<ref>/functions.
func FunctionsDir(cfg *config.Config, ref string) string {
	return filepath.Join(ProjectDir(cfg, ref), FunctionsDirName)
}

// EnvPath is <tenants>/<ref>/functions-env.json.
func EnvPath(cfg *config.Config, ref string) string {
	return filepath.Join(ProjectDir(cfg, ref), EnvFileName)
}

// FunctionPath is <tenants>/<ref>/functions/<slug>, the symlink to the live generation.
func FunctionPath(cfg *config.Config, ref, slug string) string {
	return filepath.Join(FunctionsDir(cfg, ref), slug)
}

// ProjectURL is SUPABASE_URL as a function of ref sees it: the public origin of the
// project's API. With TLS off and a public listener on another port than 80 the port is
// part of it, since a function reaches the node through the same address a client does.
func ProjectURL(cfg *config.Config, ref string) string {
	if t := cfg.Functions.ProjectURLTemplate; t != "" {
		return strings.ReplaceAll(t, "{ref}", ref)
	}
	if cfg.BaseDomain() == "" {
		return ""
	}
	scheme, listen, def := "https", cfg.Listen.HTTPS, "443"
	if cfg.TLS.Mode == "off" {
		scheme, listen, def = "http", cfg.Listen.HTTP, "80"
	}
	host := cfg.ProjectHost(ref)
	if _, port, err := net.SplitHostPort(listen); err == nil && port != "" && port != def {
		host = net.JoinHostPort(host, port)
	}
	return scheme + "://" + host
}

// Live reports the version of the function that is on disk and served for slug in ref.
func Live(cfg *config.Config, ref, slug string) (version int, ok bool) {
	m, ok := liveMeta(FunctionPath(cfg, ref, slug))
	return m.Version, ok && m.Kind == kindEszip
}

// RemoveFiles deletes everything this package keeps for ref. Deleting a project from the
// command line calls it; a node that runs the API server also gets it from Reconcile.
func RemoveFiles(cfg *config.Config, ref string) error {
	if err := validRef(ref); err != nil {
		return err
	}
	return os.RemoveAll(ProjectDir(cfg, ref))
}
