package api

import (
	"strings"
	"testing"

	"golang.org/x/net/publicsuffix"

	"github.com/jsmillerdev/supavise/internal/config"
)

// The Supabase CLI connects through the pooler URL that `supabase link` recorded only when
// the effective TLD plus one of the URL's host equals the profile's pooler_host
// (apps/cli-go/internal/utils/connect.go, assertDomainInProfile). A profile that names the
// pooler host itself fails that check, and `supabase db push` then has no route to the
// database on a node whose direct database host does not answer.
func TestCLIProfilePoolerHostPassesTheCLIsDomainCheck(t *testing.T) {
	for _, tc := range []struct {
		name     string
		domain   string
		publicIP string
		want     string
	}{
		{"own domain", "example.com", "", "example.com"},
		{"subdomain of a company domain", "supabase.corp.example.org", "", "example.org"},
		{"a registry under a public suffix", "db.example.co.uk", "", "example.co.uk"},
		{"test domain", "conformance.test", "", "conformance.test"},
		{"the default sslip.io name", "", "203.0.113.7", "sslip.io"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Domain, cfg.PublicIP = tc.domain, tc.publicIP
			p := NewCLIProfile(cfg, "")
			if p.PoolerHost != tc.want {
				t.Errorf("pooler_host = %q, want %q", p.PoolerHost, tc.want)
			}
			// What the CLI computes from the host of the pooler connection string.
			got, err := publicsuffix.EffectiveTLDPlusOne(cfg.PoolerHost())
			if err != nil || !strings.EqualFold(got, p.PoolerHost) {
				t.Errorf("the CLI derives %q (%v) from %s, the profile says %q", got, err, cfg.PoolerHost(), p.PoolerHost)
			}
			out, err := p.Render("yaml")
			if err != nil || !strings.Contains(out, "\npooler_host: "+tc.want+"\n") {
				t.Errorf("rendered profile: %q (%v)", out, err)
			}
		})
	}
}

func TestPoolerDomainKeepsAHostWithoutARegistrableDomain(t *testing.T) {
	if got := poolerDomain("com"); got != "com" {
		t.Errorf("poolerDomain(com) = %q", got)
	}
}
