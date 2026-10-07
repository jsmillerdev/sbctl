package domains

import (
	"fmt"
	"net/netip"
	"regexp"
	"strings"

	"github.com/jsmillerdev/supavise/internal/config"
)

// ChallengeLabel is the label under which a hostname's ownership TXT record lives:
// _supavise-challenge.<hostname>.
const ChallengeLabel = "_supavise-challenge"

// ChallengeName is the DNS name of the ownership TXT record of hostname.
func ChallengeName(hostname string) string { return ChallengeLabel + "." + hostname }

// CNAMEMismatch is the verification error Studio's Custom Domains page recognizes: while it
// is listed, the page shows the CNAME record to create.
const CNAMEMismatch = "custom hostname does not CNAME to this zone."

// TXTMissing is listed while the ownership TXT record is not found.
const TXTMissing = "ownership TXT record not found."

var (
	labelRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	refRE   = regexp.MustCompile(`^[a-z]{20}$`)
)

// reservedSuffixes are DNS zones a custom hostname cannot be in or under: names that are
// never a project's (loopback, link-local and private-use zones, and our internal hosts).
var reservedSuffixes = []string{"localhost", "internal", "local", "arpa", "invalid", "realtime.internal"}

// ValidateHostname normalizes a custom hostname (lower case, one trailing dot dropped) and
// refuses what cannot be one: an IP address, a name with fewer than two labels, a wildcard, a
// malformed label, a name in a reserved zone, and every name the node serves itself (api., studio.
// and pooler. of the node's domain, and everything under api.<domain>, where project and
// vanity hosts live). On a node without a domain of its own (sslip.io) the whole base domain is
// refused, since the operator does not control it.
func ValidateHostname(cfg *config.Config, in string) (string, error) {
	h := strings.ToLower(strings.TrimSpace(in))
	h = strings.TrimSuffix(h, ".")
	if h == "" {
		return "", invalid("custom_hostname is required")
	}
	if len(h) > 253 {
		return "", invalid("custom_hostname is longer than 253 characters")
	}
	if _, err := netip.ParseAddr(h); err == nil {
		return "", invalid("custom_hostname must be a domain name, not an IP address")
	}
	if strings.Contains(h, "*") {
		return "", invalid("wildcard hostnames are not supported")
	}
	labels := strings.Split(h, ".")
	if len(labels) < 2 {
		return "", invalid("custom_hostname must have at least two labels, like app.example.com")
	}
	for _, l := range labels {
		if !labelRE.MatchString(l) {
			return "", invalid(fmt.Sprintf("custom_hostname has an invalid label %q (letters, digits and hyphens; punycode for international names)", l))
		}
	}
	if tld := labels[len(labels)-1]; strings.Trim(tld, "0123456789") == "" {
		return "", invalid("custom_hostname must end in a name, not a number")
	}
	for _, z := range reservedSuffixes {
		if h == z || strings.HasSuffix(h, "."+z) {
			return "", invalid("custom_hostname is in a reserved zone (." + z + ")")
		}
	}
	if base := cfg.BaseDomain(); base != "" {
		own := []string{"api." + base, "studio." + base, "pooler." + base}
		if cfg.Domain == "" {
			own = []string{base}
		}
		for _, o := range own {
			if h == o || strings.HasSuffix(h, "."+o) {
				return "", invalid("custom_hostname is a name this node serves itself (" + o + ")")
			}
		}
	}
	return h, nil
}

// reservedNames cannot be a vanity subdomain: names the node or its operator is likely to use
// for something else, and names that would pass for the platform.
var reservedNames = map[string]bool{
	"api": true, "studio": true, "pooler": true, "db": true, "www": true, "mail": true, "smtp": true,
	"imap": true, "admin": true, "administrator": true, "dashboard": true, "app": true, "auth": true,
	"rest": true, "graphql": true, "realtime": true, "storage": true, "functions": true, "edge": true,
	"supabase": true, "supavise": true, "system": true, "internal": true, "localhost": true,
	"status": true, "dns": true, "ns": true, "ns1": true, "ns2": true, "cdn": true, "static": true,
	"assets": true, "docs": true, "help": true, "support": true, "billing": true, "login": true,
	"signup": true, "console": true, "management": true, "metrics": true, "postgres": true,
	"pgmeta": true, "supavisor": true, "kong": true, "root": true, "ftp": true, "ssh": true,
	"vpn": true, "git": true, "test": true, "staging": true, "prod": true, "production": true,
}

// ValidateVanityName normalizes a vanity subdomain and refuses a malformed name (one DNS label
// of at most 63 letters, digits and hyphens, no hyphen at either end), a reserved one, and one
// that looks like a project ref (twenty lower-case letters: those hosts belong to projects).
func ValidateVanityName(in string) (string, error) {
	n := strings.ToLower(strings.TrimSpace(in))
	if n == "" {
		return "", invalid("vanity_subdomain is required")
	}
	if len(n) > 63 || !labelRE.MatchString(n) {
		return "", invalid("vanity_subdomain must be one DNS label of at most 63 letters, digits and hyphens, not starting or ending with a hyphen")
	}
	if strings.HasPrefix(n, "xn--") {
		return "", invalid("vanity_subdomain cannot start with xn--")
	}
	if refRE.MatchString(n) {
		return "", &Error{Kind: KindInvalid, Reserved: true, Msg: "vanity_subdomain cannot be twenty lower-case letters: that is the shape of a project ref"}
	}
	if reservedNames[n] {
		return "", &Error{Kind: KindInvalid, Reserved: true, Msg: "vanity_subdomain " + n + " is reserved"}
	}
	return n, nil
}
