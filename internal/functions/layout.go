package functions

import (
	"net"
	"path/filepath"
	"strings"

	"github.com/OWNER/sbctl/internal/config"
)

// File and directory names under projects/<ref>/. The Deno main service reads exactly
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

// FunctionsDir is projects/<ref>/functions.
func FunctionsDir(cfg *config.Config, ref string) string {
	return filepath.Join(cfg.Paths().Project(ref), FunctionsDirName)
}

// EnvPath is projects/<ref>/functions-env.json.
func EnvPath(cfg *config.Config, ref string) string {
	return filepath.Join(cfg.Paths().Project(ref), EnvFileName)
}

// FunctionPath is projects/<ref>/functions/<slug>, the symlink to the live generation.
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
