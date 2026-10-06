package proxy

import (
	"fmt"
	"strings"

	"github.com/caddyserver/certmagic"
	"github.com/libdns/cloudflare"
	"github.com/libdns/digitalocean"
	hetzner "github.com/libdns/hetzner/v2"
	"github.com/libdns/route53"
)

// DNSProviders are the values of tls.dns_provider.
var DNSProviders = []string{"route53", "cloudflare", "hetzner", "digitalocean"}

// newDNSProvider builds the libdns provider for DNS-01 from [tls] credentials
// (config keys or SBCTL_TLS_CREDENTIALS_<KEY> variables):
//
//	route53       none needed on AWS (instance role / default credential chain);
//	              optional access_key_id, secret_access_key, session_token, region,
//	              profile, hosted_zone_id
//	cloudflare    api_token (Zone.DNS:Edit); optional zone_token (Zone:Read) when
//	              api_token is scoped to one zone
//	hetzner       api_token (Hetzner Cloud API token, DNS zones in the Hetzner Console)
//	digitalocean  api_token
func newDNSProvider(name string, cred map[string]string) (certmagic.DNSProvider, error) {
	need := func(key string) (string, error) {
		v := strings.TrimSpace(cred[key])
		if v == "" {
			return "", fmt.Errorf("tls: dns_provider %q needs credential %q (set tls.credentials.%s or SBCTL_TLS_CREDENTIALS_%s)",
				name, key, key, strings.ToUpper(key))
		}
		return v, nil
	}
	switch name {
	case "route53":
		return &route53.Provider{
			Region:          cred["region"],
			Profile:         cred["profile"],
			AccessKeyId:     cred["access_key_id"],
			SecretAccessKey: cred["secret_access_key"],
			SessionToken:    cred["session_token"],
			HostedZoneID:    cred["hosted_zone_id"],
		}, nil
	case "cloudflare":
		tok, err := need("api_token")
		if err != nil {
			return nil, err
		}
		return &cloudflare.Provider{APIToken: tok, ZoneToken: cred["zone_token"]}, nil
	case "hetzner":
		tok, err := need("api_token")
		if err != nil {
			return nil, err
		}
		return &hetzner.Provider{APIToken: tok}, nil
	case "digitalocean":
		tok, err := need("api_token")
		if err != nil {
			return nil, err
		}
		return &digitalocean.Provider{APIToken: tok}, nil
	case "":
		return nil, fmt.Errorf("tls: no dns_provider configured")
	}
	return nil, fmt.Errorf("tls: unknown dns_provider %q (want one of %s)", name, strings.Join(DNSProviders, ", "))
}
