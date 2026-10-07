// Package domains implements per-project custom hostnames and vanity subdomains, the way
// hosted Supabase's Custom Domains do (docs: guides/platform/custom-domains; Management API
// /v1/projects/{ref}/custom-hostname and /vanity-subdomain).
//
// A custom hostname goes through initialize (claim it, get a TXT token and the CNAME target),
// reverify (the node resolves the hostname and the TXT record itself) and activate (route it,
// let GoTrue use it). A vanity subdomain is <name>.api.<domain>, which the wildcard DNS record
// and the wildcard certificate of the node already cover. The proxy serves both from the
// registry's routes table and gates certificate issuance on it (internal/proxy).
package domains

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/jsmillerdev/supavise/internal/config"
	"github.com/jsmillerdev/supavise/internal/registry"
)

// Resolver is the DNS the node verifies with. *net.Resolver implements it; tests use a fake.
type Resolver interface {
	LookupCNAME(ctx context.Context, host string) (string, error)
	LookupTXT(ctx context.Context, name string) ([]string, error)
	LookupHost(ctx context.Context, host string) ([]string, error)
}

// Options are the inputs of New.
type Options struct {
	Store  registry.DomainStore
	Reg    registry.Registry
	Config *config.Config
	// Resolver defaults to net.DefaultResolver.
	Resolver Resolver
	// Now is the clock; empty means time.Now.
	Now func() time.Time
	// Attempts is how many verification attempts a project may make in a burst, and Refill
	// how long one attempt takes to come back; zero means 5 and 15 seconds. A node-wide bucket
	// of ten times that stops many projects from using the node as a DNS query generator.
	Attempts int
	Refill   time.Duration
}

// Service holds the domain rules. It is safe for concurrent use.
type Service struct {
	store registry.DomainStore
	reg   registry.Registry
	cfg   *config.Config
	res   Resolver
	now   func() time.Time
	lim   *limiter
}

// New returns a Service. It returns nil when the registry has no domain store.
func New(o Options) *Service {
	store := o.Store
	if store == nil {
		store = registry.Domains(o.Reg)
	}
	if store == nil {
		return nil
	}
	s := &Service{store: store, reg: o.Reg, cfg: o.Config, res: o.Resolver, now: o.Now}
	if s.res == nil {
		s.res = net.DefaultResolver
	}
	if s.now == nil {
		s.now = time.Now
	}
	n, refill := o.Attempts, o.Refill
	if n <= 0 {
		n = 5
	}
	if refill <= 0 {
		refill = 15 * time.Second
	}
	s.lim = newLimiter(n, refill, s.now)
	return s
}

// State is a custom hostname as the API reports it.
type State struct {
	registry.CustomHostname
	// Errors are what the last check found missing, in hosted's wording.
	Errors []string
	// Target is the name the hostname should be a CNAME of: the project's own host.
	Target string
	// TXTName and TXTValue are the ownership record.
	TXTName, TXTValue string
}

func (s *Service) state(h *registry.CustomHostname) *State {
	st := &State{CustomHostname: *h, Target: s.cfg.ProjectHost(h.Ref), TXTName: ChallengeName(h.Hostname), TXTValue: h.Token}
	if h.Status != registry.HostnameActive {
		if !h.CNAMEOK {
			st.Errors = append(st.Errors, CNAMEMismatch)
		}
		if !h.TXTOK {
			st.Errors = append(st.Errors, TXTMissing)
		}
	}
	return st
}

func (s *Service) project(ctx context.Context, ref string) (*registry.Project, error) {
	if ref == config.SystemRef {
		return nil, invalid("the system project cannot have a custom domain")
	}
	return s.reg.GetProject(ctx, ref)
}

// Hostname returns the project's custom hostname; a *Error of KindNotConfigured when it has none.
func (s *Service) Hostname(ctx context.Context, ref string) (*State, error) {
	h, err := s.store.GetCustomHostname(ctx, ref)
	if errors.Is(err, registry.ErrNotFound) {
		if _, perr := s.project(ctx, ref); perr != nil {
			return nil, perr
		}
		return nil, errNoHostname
	}
	if err != nil {
		return nil, err
	}
	return s.state(h), nil
}

var errNoHostname = &Error{Kind: KindNotConfigured, Msg: "Project does not have a custom hostname configuration"}

// Initialize claims hostname for the project and returns the records to create. Running it
// again with the same hostname keeps the token the user may have published already; a different
// hostname replaces the claim. An active hostname must be deleted first.
func (s *Service) Initialize(ctx context.Context, ref, hostname string) (*State, error) {
	if _, err := s.project(ctx, ref); err != nil {
		return nil, err
	}
	if s.cfg.TLS.Mode == "dns01" {
		return nil, &Error{Kind: KindUnavailable, Msg: "Custom hostnames need certificates issued on demand; this node's tls.mode is dns01, which issues only the wildcard"}
	}
	host, err := ValidateHostname(s.cfg, hostname)
	if err != nil {
		return nil, err
	}
	cur, err := s.store.GetCustomHostname(ctx, ref)
	switch {
	case err == nil && cur.Status == registry.HostnameActive:
		return nil, state("The project's custom hostname is active; delete it before setting up another")
	case err == nil && cur.Hostname == host:
		return s.state(cur), nil
	case err != nil && !errors.Is(err, registry.ErrNotFound):
		return nil, err
	}
	token, err := newToken()
	if err != nil {
		return nil, err
	}
	h := &registry.CustomHostname{Ref: ref, Hostname: host, Status: registry.HostnameInitiated, Token: token}
	if err := s.store.PutCustomHostname(ctx, h); err != nil {
		return nil, s.mapStoreErr(err)
	}
	stored, err := s.store.GetCustomHostname(ctx, ref)
	if err != nil {
		return nil, err
	}
	return s.state(stored), nil
}

func (s *Service) mapStoreErr(err error) error {
	if errors.Is(err, registry.ErrConflict) {
		return conflict("That hostname is already in use by another project")
	}
	return err
}

func newToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "supavise-verify=" + hex.EncodeToString(b), nil
}

// Reverify looks the hostname up in DNS from this node and moves the claim to what it finds:
// the TXT record alone is "3_challenge_verified", the TXT record and a hostname that resolves to
// this node make it "4_origin_setup_completed", ready to activate. An active hostname is
// reported as it is. Attempts are rate limited per project and per node.
func (s *Service) Reverify(ctx context.Context, ref string) (*State, error) {
	h, err := s.store.GetCustomHostname(ctx, ref)
	if errors.Is(err, registry.ErrNotFound) {
		return nil, errNoHostname
	}
	if err != nil {
		return nil, err
	}
	if h.Status == registry.HostnameActive {
		return s.state(h), nil
	}
	if wait := s.lim.take(ref); wait > 0 {
		return nil, &Error{Kind: KindRateLimited, Msg: "Rate limit exceeded: too many verification attempts, try again shortly", RetryAfter: wait}
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	next := *h
	next.TXTOK = s.checkTXT(cctx, h)
	next.CNAMEOK = s.checkOrigin(cctx, h)
	switch {
	case next.TXTOK && next.CNAMEOK:
		next.Status = registry.HostnameOriginReady
		if next.VerifiedAt == nil {
			now := s.now()
			next.VerifiedAt = &now
		}
	case next.TXTOK:
		next.Status, next.VerifiedAt = registry.HostnameChallengeVerified, nil
	default:
		next.Status, next.VerifiedAt = registry.HostnameInitiated, nil
	}
	if err := s.store.UpdateCustomHostname(ctx, &next); err != nil {
		return nil, s.mapStoreErr(err)
	}
	stored, err := s.store.GetCustomHostname(ctx, ref)
	if err != nil {
		return nil, err
	}
	return s.state(stored), nil
}

// checkTXT reports whether _supavise-challenge.<hostname> carries the claim's token.
func (s *Service) checkTXT(ctx context.Context, h *registry.CustomHostname) bool {
	recs, err := s.res.LookupTXT(ctx, ChallengeName(h.Hostname))
	if err != nil && len(recs) == 0 {
		return false
	}
	for _, r := range recs {
		if strings.Trim(strings.TrimSpace(r), `"`) == h.Token {
			return true
		}
	}
	return false
}

// checkOrigin reports whether the hostname reaches this node: it is a CNAME of the project's
// host, or it resolves to an address the node answers on (the addresses of api.<domain> and
// of the project's host, and public_ip).
func (s *Service) checkOrigin(ctx context.Context, h *registry.CustomHostname) bool {
	target := s.cfg.ProjectHost(h.Ref)
	if cname, err := s.res.LookupCNAME(ctx, h.Hostname); err == nil && trimDot(cname) == target {
		return true
	}
	got, err := s.res.LookupHost(ctx, h.Hostname)
	if err != nil || len(got) == 0 {
		return false
	}
	node := map[netip.Addr]bool{}
	if a, err := netip.ParseAddr(s.cfg.PublicIP); err == nil {
		node[a.Unmap()] = true
	}
	for _, name := range []string{target, s.cfg.APIHost()} {
		addrs, _ := s.res.LookupHost(ctx, name)
		for _, x := range addrs {
			if a, err := netip.ParseAddr(x); err == nil {
				node[a.Unmap()] = true
			}
		}
	}
	for _, x := range got {
		if a, err := netip.ParseAddr(x); err == nil && node[a.Unmap()] {
			return true
		}
	}
	return false
}

func trimDot(s string) string { return strings.TrimSuffix(strings.ToLower(s), ".") }

// Activate turns a verified hostname on: it gets its route (and so its certificate) and
// ActivateCustomHostname's status "5_services_reconfigured". Calling it on an active hostname
// succeeds and changes nothing, so a caller whose reconfiguration of GoTrue failed can run it
// again. changed is false in that case.
func (s *Service) Activate(ctx context.Context, ref string) (st *State, changed bool, err error) {
	h, err := s.store.GetCustomHostname(ctx, ref)
	if errors.Is(err, registry.ErrNotFound) {
		return nil, false, errNoHostname
	}
	if err != nil {
		return nil, false, err
	}
	if h.Status == registry.HostnameActive {
		return s.state(h), false, nil
	}
	if h.Status != registry.HostnameOriginReady {
		return nil, false, state("The custom hostname is not verified yet: create the DNS records and verify them first")
	}
	act, err := s.store.ActivateCustomHostname(ctx, ref)
	if err != nil {
		if errors.Is(err, registry.ErrConflict) {
			return nil, false, conflict("That hostname is already in use by another project")
		}
		return nil, false, err
	}
	return s.state(act), true, nil
}

// Delete removes the project's custom hostname and, when it was active, its route.
func (s *Service) Delete(ctx context.Context, ref string) (wasActive bool, err error) {
	h, err := s.store.GetCustomHostname(ctx, ref)
	if errors.Is(err, registry.ErrNotFound) {
		return false, errNoHostname
	}
	if err != nil {
		return false, err
	}
	if err := s.store.DeleteCustomHostname(ctx, ref); err != nil && !errors.Is(err, registry.ErrNotFound) {
		return false, err
	}
	return h.Status == registry.HostnameActive, nil
}

// VanityStatus values of hosted's GET vanity-subdomain.
const (
	VanityNotUsed          = "not-used"
	VanityCustomDomainUsed = "custom-domain-used"
	VanityActive           = "active"
)

// VanityState is the answer of GET vanity-subdomain.
type VanityState struct {
	Status string
	// CustomDomain is the host in use: the custom hostname with "custom-domain-used", the
	// vanity host with "active".
	CustomDomain string
	Name         string
}

// VanityHost is the host of a vanity subdomain: <name>.api.<domain>, under the wildcard.
func (s *Service) VanityHost(name string) string { return s.cfg.VanityHost(name) }

// Vanity reports the project's vanity subdomain. An active custom hostname wins, as it does for
// GoTrue's external URL: the vanity host keeps serving, but the project presents its own domain.
func (s *Service) Vanity(ctx context.Context, ref string) (*VanityState, error) {
	if _, err := s.project(ctx, ref); err != nil {
		return nil, err
	}
	v, err := s.store.GetVanitySubdomain(ctx, ref)
	if err != nil && !errors.Is(err, registry.ErrNotFound) {
		return nil, err
	}
	if h, herr := s.store.GetCustomHostname(ctx, ref); herr == nil && h.Status == registry.HostnameActive {
		out := &VanityState{Status: VanityCustomDomainUsed, CustomDomain: h.Hostname}
		if v != nil {
			out.Name = v.Name
		}
		return out, nil
	}
	if v == nil {
		return &VanityState{Status: VanityNotUsed}, nil
	}
	return &VanityState{Status: VanityActive, CustomDomain: s.VanityHost(v.Name), Name: v.Name}, nil
}

// CheckVanity reports whether name is free for the project: valid, not reserved, and not held by
// another project. A malformed name is an error, a reserved or taken one is "not available".
func (s *Service) CheckVanity(ctx context.Context, ref, name string) (bool, error) {
	if _, err := s.project(ctx, ref); err != nil {
		return false, err
	}
	n, err := ValidateVanityName(name)
	if err != nil {
		if e, ok := AsError(err); ok && e.Reserved {
			return false, nil
		}
		return false, err
	}
	owner, err := s.store.VanitySubdomainOwner(ctx, n)
	if errors.Is(err, registry.ErrNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return owner == ref, nil
}

// ActivateVanity gives the project the vanity subdomain name, replacing its previous one, and
// returns the full host. Reserved and malformed names are refused (400), a name another project
// holds is a conflict (409). It reports the host the project had before, if any.
func (s *Service) ActivateVanity(ctx context.Context, ref, name string) (host string, err error) {
	if _, err := s.project(ctx, ref); err != nil {
		return "", err
	}
	n, err := ValidateVanityName(name)
	if err != nil {
		return "", err
	}
	host = s.VanityHost(n)
	if err := s.store.PutVanitySubdomain(ctx, ref, n, host); err != nil {
		if errors.Is(err, registry.ErrConflict) {
			return "", conflict("That vanity subdomain is already taken")
		}
		return "", err
	}
	return host, nil
}

// DeleteVanity removes the project's vanity subdomain; a *Error of KindNotConfigured when it has none.
func (s *Service) DeleteVanity(ctx context.Context, ref string) error {
	if _, err := s.project(ctx, ref); err != nil {
		return err
	}
	if err := s.store.DeleteVanitySubdomain(ctx, ref); err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			return &Error{Kind: KindNotConfigured, Msg: "Project does not have a vanity subdomain"}
		}
		return err
	}
	return nil
}

// ExternalHost is the host GoTrue presents for the project: its active custom hostname, else its
// vanity host, else "" (the derived <ref>.api.<domain>). Nodes whose registry has no domain
// store always get "".
func ExternalHost(ctx context.Context, reg registry.Registry, cfg *config.Config, ref string) (string, error) {
	store := registry.Domains(reg)
	if store == nil {
		return "", nil
	}
	if h, err := store.GetCustomHostname(ctx, ref); err == nil && h.Status == registry.HostnameActive {
		return h.Hostname, nil
	} else if err != nil && !errors.Is(err, registry.ErrNotFound) {
		return "", err
	}
	v, err := store.GetVanitySubdomain(ctx, ref)
	if errors.Is(err, registry.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return cfg.VanityHost(v.Name), nil
}

// limiter is a token bucket per key and one for the node.
type limiter struct {
	mu     sync.Mutex
	now    func() time.Time
	cap    float64
	refill time.Duration
	per    map[string]*bucket
	node   bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newLimiter(n int, refill time.Duration, now func() time.Time) *limiter {
	l := &limiter{now: now, cap: float64(n), refill: refill, per: map[string]*bucket{}}
	l.node = bucket{tokens: l.cap * 10, last: now()}
	return l
}

func (b *bucket) fill(now time.Time, cap float64, refill time.Duration) {
	b.tokens += float64(now.Sub(b.last)) / float64(refill)
	if b.tokens > cap {
		b.tokens = cap
	}
	b.last = now
}

// take spends one attempt of key; it returns 0 when allowed, else how long until one is back.
func (l *limiter) take(key string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b := l.per[key]
	if b == nil {
		if len(l.per) > 4096 { // forget idle projects; a full bucket is the same as none
			for k, o := range l.per {
				if now.Sub(o.last) > time.Duration(l.cap)*l.refill {
					delete(l.per, k)
				}
			}
		}
		b = &bucket{tokens: l.cap, last: now}
		l.per[key] = b
	}
	b.fill(now, l.cap, l.refill)
	l.node.fill(now, l.cap*10, l.refill/10)
	for _, c := range []*bucket{b, &l.node} {
		if c.tokens < 1 {
			rate := l.refill
			if c == &l.node {
				rate = l.refill / 10
			}
			return time.Duration((1 - c.tokens) * float64(rate))
		}
	}
	b.tokens--
	l.node.tokens--
	return 0
}
