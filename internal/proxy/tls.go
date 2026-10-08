package proxy

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/caddyserver/certmagic"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/mesh/peerapi"
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

// certManager owns the CertMagic configuration of one Server. CertMagic uses
// DNS-01 exclusively whenever an issuer has a DNS solver, so a mode that needs both
// DNS-01 (the wildcard) and HTTP-01 (custom hostnames) runs two configs on one
// cache and picks one per server name:
//
//   - dns: *.api.<domain>, api.<domain> and studio.<domain> by DNS-01 (modes dns01, auto).
//   - http: every other allowed host by HTTP-01 / TLS-ALPN-01 on demand (modes auto, http01).
type certManager struct {
	dns, http             *certmagic.Config // nil when the mode does not use it
	dnsIssuer, httpIssuer *certmagic.ACMEIssuer
	cache                 *certmagic.Cache
	names                 []string // certificates managed from startup
	mode                  string
	base                  string // base domain
	log                   *slog.Logger
	wg                    sync.WaitGroup // startup issuance goroutines

	// managing is whether this node obtains and renews certificates. It is false while a follower
	// mirrors the leader's: nothing is issued then (see certsync.go).
	managing atomic.Bool
	// mirror, when set, fetches the leader's store while the node does not manage.
	mirror *certMirror
	// mirrored are the mirrored certificates the cache holds as unmanaged ones, by site directory.
	// CertMagic renews only managed certificates, so a follower's copies are never renewed.
	mirrorMu sync.Mutex
	mirrored map[string]mirroredCert
}

// mirroredCert is a mirrored certificate in the cache: tag identifies its content, hash is its
// key in the CertMagic cache.
type mirroredCert struct {
	name, tag, hash string
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
	// follower starts the manager as a mirror of another node's certificates: it issues nothing
	// until startManaging.
	follower bool
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
	cm := &certManager{mode: o.mode, base: cfg.BaseDomain(), names: managedNames(cfg, o.mode), log: o.log, mirrored: map[string]mirroredCert{}}
	cm.managing.Store(!o.follower)
	cm.cache = certmagic.NewCache(certmagic.CacheOptions{
		GetConfigForCert: func(c certmagic.Certificate) (*certmagic.Config, error) { return cm.configForNames(c.Names), nil },
		Logger:           zl,
	})
	storage := &certmagic.FileStorage{Path: cfg.Paths().Certs()}

	tmpl := certmagic.ACMEIssuer{
		CA:     cfg.TLS.CA,
		Email:  cfg.TLS.Email,
		Agreed: true, // running supavise with TLS enabled accepts the CA's subscriber agreement
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

	if o.mode == tlsDNS01 || o.mode == tlsAuto {
		if o.provider == nil {
			return nil, errors.New("tls: DNS-01 needs a DNS provider")
		}
		// Certificates of this config are obtained at startup and renewed in the
		// background; project hosts are served from the cached wildcard, so no OnDemand.
		cm.dns = certmagic.New(cm.cache, certmagic.Config{Storage: storage, Logger: zl})
		t := tmpl
		t.DNS01Solver = &certmagic.DNS01Solver{DNSManager: certmagic.DNSManager{DNSProvider: o.provider}}
		t.DisableHTTPChallenge = true
		t.DisableTLSALPNChallenge = true
		cm.dnsIssuer = certmagic.NewACMEIssuer(cm.dns, t)
		cm.dns.Issuers = []certmagic.Issuer{cm.dnsIssuer}
	}
	if o.mode == tlsAuto || o.mode == tlsHTTP01 {
		cm.http = certmagic.New(cm.cache, certmagic.Config{
			Storage:  storage,
			Logger:   zl,
			OnDemand: &certmagic.OnDemandConfig{DecisionFunc: cm.decide(o.allow)},
		})
		cm.httpIssuer = certmagic.NewACMEIssuer(cm.http, tmpl)
		cm.http.Issuers = []certmagic.Issuer{cm.httpIssuer}
	}
	return cm, nil
}

// dnsName reports whether name is covered by the DNS-01 config: api.<domain>,
// studio.<domain>, or anything under api.<domain> (including the wildcard itself).
func (cm *certManager) dnsName(name string) bool {
	name = normalizeHost(name)
	return name == "api."+cm.base || name == "studio."+cm.base || strings.HasSuffix(name, ".api."+cm.base)
}

// configFor picks the config that serves and renews the certificate for a server name.
func (cm *certManager) configFor(name string) *certmagic.Config {
	if cm.dns != nil && (name == "" || cm.dnsName(name)) {
		return cm.dns
	}
	if cm.http != nil {
		return cm.http
	}
	return cm.dns
}

// configForNames is configFor for a certificate with several names.
func (cm *certManager) configForNames(names []string) *certmagic.Config {
	for _, n := range names {
		if cm.dns != nil && cm.dnsName(n) {
			return cm.dns
		}
	}
	if cm.http != nil {
		return cm.http
	}
	return cm.dns
}

// challengeIssuer is the issuer whose HTTP-01 challenges :80 must answer.
func (cm *certManager) challengeIssuer() *certmagic.ACMEIssuer {
	if cm.httpIssuer != nil {
		return cm.httpIssuer
	}
	return cm.dnsIssuer
}

// tlsConfig is the config of the HTTPS listener: certificates from CertMagic
// (the config chosen per SNI) and the ACME TLS-ALPN-01 protocol next to HTTP/2
// and HTTP/1.1.
func (cm *certManager) tlsConfig() *tls.Config {
	primary := cm.configFor("")
	if cm.http != nil {
		primary = cm.http // its TLS config also answers TLS-ALPN-01 challenges
	}
	c := primary.TLSConfig()
	c.GetCertificate = func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		return cm.configFor(normalizeHost(hello.ServerName)).GetCertificate(hello)
	}
	c.NextProtos = append([]string{"h2", "http/1.1"}, c.NextProtos...)
	c.MinVersion = tls.VersionTLS12
	return c
}

// manage starts obtaining and renewing the startup certificates in the background.
func (cm *certManager) manage(ctx context.Context) error {
	if cm.dns != nil {
		return cm.dns.ManageAsync(ctx, cm.names)
	}
	// The HTTP config has OnDemand set, and CertMagic's ManageAsync then only adds the
	// names to the on-demand allowlist and defers issuance to the first handshake. Obtain
	// the startup names here so the first visitor to api. or studio. does not wait for
	// the CA. Failures are logged and retried by CertMagic; the on-demand path still works.
	if err := cm.http.ManageAsync(ctx, cm.names); err != nil {
		return err
	}
	for _, name := range cm.names {
		cm.wg.Add(1)
		go func() {
			defer cm.wg.Done()
			if err := cm.http.ObtainCertAsync(ctx, name); err != nil && ctx.Err() == nil {
				cm.log.Warn("could not obtain the startup certificate; it will be requested on first use", "name", name, "err", err)
			}
		}()
	}
	return nil
}

// close waits (briefly) for the startup issuance goroutines, which end when the context
// passed to manage does, then stops the certificate cache.
func (cm *certManager) close() {
	done := make(chan struct{})
	go func() { cm.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
	cm.cache.Stop()
}

// errMirroring is why a follower issues nothing.
var errMirroring = errors.New("this node mirrors the leader's certificates and obtains none")

// decide wraps the on-demand decision: a node that does not manage refuses every name that has no
// cached certificate (CertMagic asks only then) and has the mirror look for one at the leader.
func (cm *certManager) decide(allow func(ctx context.Context, name string) error) func(ctx context.Context, name string) error {
	return func(ctx context.Context, name string) error {
		if !cm.managing.Load() {
			if cm.mirror != nil {
				cm.mirror.wake()
			}
			return errMirroring
		}
		return allow(ctx, name)
	}
}

// loadMirrored puts the mirrored certificates into the cache as unmanaged ones: it serves them and
// never renews them, and a renewed certificate of the leader replaces the old one when the mirror
// brings it. A site that left the store leaves the cache.
func (cm *certManager) loadMirrored(ctx context.Context, snap peerapi.CertSnapshot) {
	sites, _ := sitesOf(snap.Files)
	cm.mirrorMu.Lock()
	defer cm.mirrorMu.Unlock()
	if cm.managing.Load() {
		return // promoted while the fetch ran: startManaging has taken over
	}
	seen := make(map[string]bool, len(sites))
	for _, st := range sites {
		seen[st.dir] = true
		tag := certTag(st.crt, st.key)
		cur, ok := cm.mirrored[st.dir]
		if ok && cur.tag == tag {
			continue
		}
		hash, err := cm.configFor(st.name).CacheUnmanagedCertificatePEMBytes(ctx, st.crt, st.key, nil)
		if err != nil {
			cm.log.Warn("proxy: could not load a mirrored certificate", "name", st.name, "err", err)
			continue
		}
		if ok && cur.hash != hash {
			cm.cache.Remove([]string{cur.hash})
		}
		cm.mirrored[st.dir] = mirroredCert{name: st.name, tag: tag, hash: hash}
	}
	for dir, cur := range cm.mirrored {
		if !seen[dir] {
			cm.cache.Remove([]string{cur.hash})
			delete(cm.mirrored, dir)
		}
	}
}

func certTag(crt, key []byte) string {
	h := sha256.New()
	h.Write(crt)
	h.Write([]byte{0})
	h.Write(key)
	return hex.EncodeToString(h.Sum(nil))
}

// startManaging makes this node the one that obtains and renews: the mirrored certificates become
// managed ones, loaded from the mirrored tree (with the ACME account the tree holds), and management
// starts as on a server of its own.
func (cm *certManager) startManaging(ctx context.Context) error {
	cm.mirrorMu.Lock()
	cm.managing.Store(true)
	for dir, cur := range cm.mirrored {
		// Out of the cache and the managed copy in at once: CertMagic keeps one entry per content.
		cm.cache.Remove([]string{cur.hash})
		if _, err := cm.configFor(cur.name).CacheManagedCertificate(ctx, cur.name); err != nil {
			cm.log.Warn("proxy: could not take over a mirrored certificate", "name", cur.name, "err", err)
		}
		delete(cm.mirrored, dir)
	}
	cm.mirrorMu.Unlock()
	return cm.manage(ctx)
}

// stopManaging makes this node mirror: the managed certificates leave the cache, which ends their
// renewal, and the mirror brings the leader's back as unmanaged ones.
func (cm *certManager) stopManaging(storeDir string) {
	cm.mirrorMu.Lock()
	defer cm.mirrorMu.Unlock()
	cm.managing.Store(false)
	// Every managed certificate has its site directory in the store.
	var subjects []certmagic.SubjectIssuer
	if dirs, err := filepath.Glob(filepath.Join(storeDir, "certificates", "*", "*")); err == nil {
		for _, d := range dirs {
			subjects = append(subjects, certmagic.SubjectIssuer{Subject: siteName(filepath.ToSlash(d))})
		}
	}
	cm.cache.RemoveManaged(subjects)
}

// allowHost is the on-demand issuance gate: a certificate is only ever requested
// for a host supavise serves. In DNS-01 modes the derived project hosts are covered by
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
	p, kind := s.table.hostProject(name)
	if kind != "" && !servable(p.status) {
		// REMOVED, INIT_FAILED, INACTIVE: nothing is served there, so a handshake for the
		// host must not spend certificate rate limit (50 per week on <ip>.sslip.io).
		return fmt.Errorf("%s belongs to a project that is %s", name, p.status)
	}
	switch kind {
	case kindDerived, kindVanity, kindReplica, kindBalancer:
		// A vanity subdomain, a replica's endpoint and a project's load balancer are all one label
		// under api.<domain>: the wildcard covers them like a ref host.
		if s.tlsMode == tlsDNS01 || s.tlsMode == tlsAuto {
			return fmt.Errorf("%s is covered by the wildcard certificate", name)
		}
		return nil
	case kindCustom:
		// A route of a custom hostname exists only once the hostname is active: the domain
		// store inserts it when the project activates the hostname and removes it with the
		// hostname. Claimed, unverified or deleted names have no route, so no certificate.
		if s.tlsMode == tlsDNS01 {
			return fmt.Errorf("%s needs HTTP-01, which tls.mode dns01 disables", name)
		}
		return nil
	}
	return fmt.Errorf("%s is not a host this node serves", name)
}

// warmCertificates obtains the certificate of a custom hostname or vanity subdomain that has
// just been routed, so that its first visitor does not wait for the CA. It asks allowHost
// first, like a handshake would, and does nothing for a name that already has a certificate.
// Failures are logged; the on-demand path at the first handshake retries.
func (s *Server) warmCertificates(ctx context.Context, cm *certManager, hosts []string) {
	if cm.http == nil || !cm.managing.Load() {
		return // a follower gets the leader's certificate through the mirror
	}
	for _, host := range hosts {
		if err := s.allowHost(ctx, host); err != nil {
			continue
		}
		go func() {
			if _, err := cm.http.CacheManagedCertificate(ctx, host); err == nil {
				return
			}
			if err := cm.http.ObtainCertAsync(ctx, host); err != nil && ctx.Err() == nil {
				s.log.Warn("proxy: could not obtain the certificate of a newly routed host; it will be requested on first use", "host", host, "err", err)
				return
			}
			s.log.Info("proxy: certificate ready for a newly routed host", "host", host)
		}()
	}
}

// forgetCertificates drops the certificates of hosts that registry routes stopped serving (a
// custom hostname deleted, a vanity subdomain released, a project removed): out of the cache,
// which ends their renewal, and out of storage, so a later claim of the name starts clean and no
// key of a name we no longer serve stays on disk. Names under the DNS-01 config are skipped: their
// certificate is the wildcard, which is not theirs to remove.
func (s *Server) forgetCertificates(ctx context.Context, cm *certManager, hosts []string) {
	if cm.http == nil || !cm.managing.Load() {
		return // a follower's store follows the leader's
	}
	for _, host := range hosts {
		if cm.dnsName(host) && cm.dns != nil {
			continue
		}
		if p, _ := s.table.hostProject(host); p.ref != "" {
			continue // routed again meanwhile (a name moved between projects)
		}
		cm.cache.RemoveManaged([]certmagic.SubjectIssuer{{Subject: host}})
		for _, is := range cm.http.Issuers {
			key := certmagic.StorageKeys.CertsSitePrefix(is.IssuerKey(), host)
			if err := cm.http.Storage.Delete(ctx, key); err != nil && !errors.Is(err, fs.ErrNotExist) {
				s.log.Warn("proxy: could not remove the certificate of a host no longer served", "host", host, "err", err)
			}
		}
		s.log.Info("proxy: removed the certificate of a host no longer served", "host", host)
	}
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
	return cm.challengeIssuer().HTTPChallengeHandler(redirect)
}

// serves reports whether host is one of ours: API, Studio, or a project.
func (s *Server) serves(host string) bool {
	return host != "" && (host == s.apiHost || host == s.studioHost || s.table.routeKind(host) != "")
}
