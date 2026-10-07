package api

import (
	"encoding/json"
	"fmt"
	"strings"

	"golang.org/x/net/publicsuffix"

	"github.com/OWNER/sbctl/internal/config"
)

// CLIProfile is the file the Supabase CLI reads with --profile (or
// SUPABASE_PROFILE) to talk to a self-hosted Management API: the keys of the CLI's
// Go Profile struct (apps/cli-go/internal/utils/profile.go). The CLI rejects
// unknown keys.
type CLIProfile struct {
	Name         string `json:"name"`
	APIURL       string `json:"api_url"`
	DashboardURL string `json:"dashboard_url"`
	// ProjectHost is the base of project hosts: https://<ref>.<project_host> is the
	// project's API gateway and db.<ref>.<project_host> its direct database host.
	ProjectHost string `json:"project_host"`
	// PoolerHost is the registrable domain of the shared pooler's host (example.com for
	// pooler.example.com), not the host itself: before it connects through the pooler URL a
	// project records, the CLI checks that the URL's host ends in this domain by comparing it
	// with the host's effective TLD plus one.
	PoolerHost string `json:"pooler_host"`
}

// NewCLIProfile derives the profile of this installation from its config.
func NewCLIProfile(c *config.Config, name string) CLIProfile {
	if name == "" {
		name = "sbctl"
	}
	return CLIProfile{
		Name: name, APIURL: c.APIURL(), DashboardURL: c.DashboardURL(),
		ProjectHost: c.APIHost(), PoolerHost: poolerDomain(c.PoolerHost()),
	}
}

// poolerDomain is the effective TLD plus one of host, which is what the CLI compares the
// pooler host with (apps/cli-go/internal/utils/connect.go, assertDomainInProfile). A host
// that has no such domain (it is itself a public suffix) is returned unchanged.
func poolerDomain(host string) string {
	d, err := publicsuffix.EffectiveTLDPlusOne(host)
	if err != nil {
		return host
	}
	return d
}

// Render writes the profile as "yaml", "toml" or "json". The CLI picks its parser
// from the file extension, so save it as profile.<format>.
func (p CLIProfile) Render(format string) (string, error) {
	switch strings.ToLower(format) {
	case "yaml", "yml", "":
		return fmt.Sprintf("name: %s\napi_url: %s\ndashboard_url: %s\nproject_host: %s\npooler_host: %s\n",
			p.Name, p.APIURL, p.DashboardURL, p.ProjectHost, p.PoolerHost), nil
	case "toml":
		return fmt.Sprintf("name = %q\napi_url = %q\ndashboard_url = %q\nproject_host = %q\npooler_host = %q\n",
			p.Name, p.APIURL, p.DashboardURL, p.ProjectHost, p.PoolerHost), nil
	case "json":
		b, err := json.MarshalIndent(p, "", "  ")
		return string(b) + "\n", err
	}
	return "", fmt.Errorf("unknown format %q (yaml, toml or json)", format)
}
