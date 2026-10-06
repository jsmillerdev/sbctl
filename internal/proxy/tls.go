package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"

	"github.com/caddyserver/certmagic"

	"github.com/OWNER/sbctl/internal/config"
)

// TLS modes after resolving tls.mode against the rest of the configuration.
const (
	tlsOff    = "off"
	tlsDNS01  = "dns01"  // wildcard by DNS-01 only
	tlsAuto   = "auto"   // wildcard by DNS-01, other allowed hosts by HTTP-01 on demand
	tlsHTTP01 = "http01" // per-host certificates by HTTP-01 / TLS-ALPN-01 on demand
)

// resolveTLSMode turns tls.mode, tls.dns_provider and the domain into the
// effective strategy:
//
//   - off: nothing terminates TLS (tests, or a TLS-terminating front end).
//   - dns01: *.api.<domain>, api.<domain> and studio.<domain> by DNS-01 only.
//   - auto with a DNS provider and a real domain: dns01 plus on-demand HTTP-01
//     for registry routes with custom hostnames.
//   - auto without a DNS provider, http01, or no domain (sslip.io has no DNS
//     API and no wildcard certificates): per-host HTTP-01 on demand.
func resolveTLSMode(cfg *config.Config) (string, error) {
	t := cfg.TLS
	if t.Mode == "off" {
		return tlsOff, nil
	}
	if cfg.BaseDomain() == "" {
		return "", errors.New("tls: set domain or public_ip (or tls.mode = \"off\")")
	}
	hasDomain := cfg.Domain != ""
	switch t.Mode {
	case "dns01":
		if !hasDomain {
			return "", errors.New("tls: tls.mode dns01 needs a domain; sslip.io hosts cannot use DNS-01")
		}
		return tlsDNS01, nil
	case "http01":
		return tlsHTTP01, nil
	}
	if t.DNSProvider != "" && hasDomain {
		return tlsAuto, nil
	}
	return tlsHTTP01, nil
}

// certManager owns the CertMagic configuration of one Server.
type certManager struct {
	magic  *certmagic.Config
	issuer *certmagic.ACMEIssuer
	cache  *certmagic.Cache
	names  []string // certificates managed from startup
	mode   string
}

// certOptions are the inputs of newCertManager.
type certOptions struct {
	cfg  *config.Config
	mode string
	// provider is the DNS-01 provider (modes dns01 and auto).
	provider certmagic.DNSProvider
	// allow gates on-demand issuance (HTTP-01) per host name.
	allow func(ctx context.Context, name string) error
	// httpPort and httpsPort are the ports our own listeners are bound to; they are
	// the ACME solvers' ports when we are not on 80 and 443.
	httpPort, httpsPort int
	log                 *slog.Logger
}

// managedNames lists the certificates obtained at startup.
func managedNames(cfg *config.Config, mode string) []string {
	api, studio := cfg.APIHost(), cfg.StudioHost()
	switch mode {
	case tlsDNS01, tlsAuto:
		return []string{"*.api." + cfg.BaseDomain(), api, studio}
	}
	return []string{api, studio}
}

func newCertManager(o certOptions) (*certManager, error) {
	cfg := o.cfg
	zl := zapToSlog(o.log)
	var magic *certmagic.Config
	cache := certmagic.NewCache(certmagic.CacheOptions{
		GetConfigForCert: func(certmagic.Certificate) (*certmagic.Config, error) { return magic, nil },
		Logger:           zl,
	})
	magic = certmagic.New(cache, certmagic.Config{
		Storage:  &certmagic.FileStorage{Path: cfg.Paths().Certs()},
		Logger:   zl,
		OnDemand: &certmagic.OnDemandConfig{DecisionFunc: o.allow},
	})

	tmpl := certmagic.ACMEIssuer{
		CA:     cfg.TLS.CA,
		Email:  cfg.TLS.Email,
		Agreed: true, // running sbctl with TLS enabled accepts the CA's subscriber agreement
		// Our own listeners are the challenge listeners; CertMagic must not bind :80 itself.
		AltHTTPPort:    o.httpPort,
		AltTLSALPNPort: o.httpsPort,
		Logger:         zl,
	}
	if tmpl.CA == "" {
		tmpl.CA = certmagic.LetsEncryptProductionCA
	}
	if cfg.TLS.CACert != "" {
		pem, err := os.ReadFile(cfg.TLS.CACert)
		if err != nil {
			return nil, fmt.Errorf("tls: ca_cert: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("tls: ca_cert %s holds no PEM certificate", cfg.TLS.CACert)
		}
		tmpl.TrustedRoots = pool
	}
	switch o.mode {
	case tlsDNS01, tlsAuto:
		if o.provider == nil {
			return nil, errors.New("tls: DNS-01 needs a DNS provider")
		}
		tmpl.DNS01Solver = &certmagic.DNS01Solver{DNSManager: certmagic.DNSManager{DNSProvider: o.provider}}
		if o.mode == tlsDNS01 {
			tmpl.DisableHTTPChallenge = true
			tmpl.DisableTLSALPNChallenge = true
		}
	}
	issuer := certmagic.NewACMEIssuer(magic, tmpl)
	magic.Issuers = []certmagic.Issuer{issuer}
	return &certManager{magic: magic, issuer: issuer, cache: cache, names: managedNames(cfg, o.mode), mode: o.mode}, nil
}

// tlsConfig is the config of the HTTPS listener: certificates from CertMagic and
// the ACME TLS-ALPN-01 protocol next to HTTP/2 and HTTP/1.1.
func (cm *certManager) tlsConfig() *tls.Config {
	c := cm.magic.TLSConfig()
	c.NextProtos = append([]string{"h2", "http/1.1"}, c.NextProtos...)
	c.MinVersion = tls.VersionTLS12
	return c
}

// manage starts obtaining and renewing the startup certificates in the background.
func (cm *certManager) manage(ctx context.Context) error {
	return cm.magic.ManageAsync(ctx, cm.names)
}

func (cm *certManager) close() { cm.cache.Stop() }

// allowHost is the on-demand issuance gate: a certificate is only ever requested
// for a host sbctl serves. In DNS-01 modes the derived project hosts are covered by
// the wildcard and are refused here so a broken DNS setup cannot quietly burn
// per-host HTTP-01 certificates against the CA's rate limits.
func (s *Server) allowHost(_ context.Context, name string) error {
	name = normalizeHost(name)
	switch {
	case name == "":
		return errors.New("no server name")
	case name == s.apiHost || name == s.studioHost:
		return nil
	}
	switch s.table.routeKind(name) {
	case "derived":
		if s.tlsMode == tlsDNS01 || s.tlsMode == tlsAuto {
			return fmt.Errorf("%s is covered by the wildcard certificate", name)
		}
		return nil
	case "custom":
		if s.tlsMode == tlsDNS01 {
			return fmt.Errorf("%s needs HTTP-01, which tls.mode dns01 disables", name)
		}
		return nil
	}
	return fmt.Errorf("%s is not a host this node serves", name)
}

// redirectHandler is the :80 handler in TLS modes: it answers ACME HTTP-01
// challenges, then redirects every host we serve to https and 404s the rest.
func (s *Server) redirectHandler(cm *certManager, httpsPort int) http.Handler {
	redirect := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := normalizeHost(r.Host)
		if !s.serves(host) {
			writeJSON(w, http.StatusNotFound, "Not Found")
			return
		}
		authority := host
		if httpsPort != 443 {
			authority += ":" + strconv.Itoa(httpsPort)
		}
		http.Redirect(w, r, "https://"+authority+r.URL.RequestURI(), http.StatusPermanentRedirect)
	})
	return cm.issuer.HTTPChallengeHandler(redirect)
}

// serves reports whether host is one of ours: API, Studio, or a project.
func (s *Server) serves(host string) bool {
	return host != "" && (host == s.apiHost || host == s.studioHost || s.table.routeKind(host) != "")
}
