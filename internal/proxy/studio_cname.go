package proxy

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jsmillerdev/supavise/internal/domains"
)

// Studio's Custom Domains page asks its own server route, /api/check-cname?domain=<host>, whether
// the domain has a DNS record before it will start the setup, and that route asks Cloudflare's
// public DNS-over-HTTPS service from the Studio process. A node answers it itself, from its own
// resolver (the one that verifies custom hostnames, see internal/domains), in the DoH JSON shape
// Studio decodes: no third party sees the domains, and the check works on a machine with no route
// to Cloudflare. Studio only needs an Answer (it does not look at what the records are), so an
// A or AAAA record passes as well as a CNAME, the same as the node's own verification.

// cnameCheckPerMinute bounds the lookups this route makes for anyone who can reach the dashboard
// host; the route is unauthenticated, like Studio's own.
const cnameCheckPerMinute = 30

type cnameLimiter struct {
	mu     sync.Mutex
	window time.Time
	n      int
}

func (l *cnameLimiter) allow(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.window) >= time.Minute {
		l.window, l.n = now, 0
	}
	l.n++
	return l.n <= cnameCheckPerMinute
}

// dohRecord is one record of a DNS-over-HTTPS JSON answer.
type dohRecord struct {
	Name string `json:"name"`
	Type int    `json:"type"`
	TTL  int    `json:"TTL"`
	Data string `json:"data"`
}

// answerCNAMECheck serves GET /api/check-cname on the Studio host. It reports whether it wrote a
// response.
func (s *Server) answerCNAMECheck(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path != "/api/check-cname" || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		return false
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	fail := func(status int, msg string) bool {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": msg})
		return true
	}
	if !s.cnameLimit.allow(time.Now()) {
		w.Header().Set("Retry-After", "60")
		return fail(http.StatusTooManyRequests, "Rate limit exceeded")
	}
	host, err := domains.ValidateHostname(s.cfg, r.URL.Query().Get("domain"))
	if err != nil {
		return fail(http.StatusBadRequest, err.Error())
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	resp := map[string]any{
		"Status": 0, "TC": false, "RD": true, "RA": true, "AD": false, "CD": false,
		"Question":  []map[string]any{{"name": host, "type": 5}},
		"Authority": []dohRecord{},
	}
	var answer []dohRecord
	if c, err := s.resolver.LookupCNAME(ctx, host); err == nil {
		if c = strings.TrimSuffix(strings.ToLower(c), "."); c != "" && c != host {
			answer = append(answer, dohRecord{Name: host + ".", Type: 5, TTL: 60, Data: c + "."})
		}
	}
	if len(answer) == 0 {
		if addrs, err := s.resolver.LookupHost(ctx, host); err == nil {
			for _, a := range addrs {
				typ := 1
				if ip := net.ParseIP(a); ip != nil && ip.To4() == nil {
					typ = 28
				}
				answer = append(answer, dohRecord{Name: host + ".", Type: typ, TTL: 60, Data: a})
			}
		}
	}
	if len(answer) > 0 {
		resp["Answer"] = answer
	} else {
		resp["Status"] = 3 // NXDOMAIN, which Studio reads as "no record found"
	}
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_ = json.NewEncoder(w).Encode(resp)
	}
	return true
}
