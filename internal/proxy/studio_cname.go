package proxy

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/supavise/supavise/internal/domains"
)

// Studio's Custom Domains page asks its own server route, /api/check-cname?domain=<host>, whether
// the domain has a DNS record before it will start the setup, and that route asks Cloudflare's
// public DNS-over-HTTPS service from the Studio process. A node answers it itself, from its own
// resolver (the one that verifies custom hostnames, see internal/domains), in the DoH JSON shape
// Studio decodes: no third party sees the domains, and the check works on a machine with no route
// to Cloudflare. Studio only needs an Answer (it does not look at what the records are), so an
// A or AAAA record passes as well as a CNAME, the same as the node's own verification.

// The route is unauthenticated, like Studio's own: the browser calls it without the dashboard's
// token, so the node cannot tell a signed-in user from anyone else who reaches the Studio host. It is
// limited per client address, so that one client cannot use up the budget of the others, and by a
// higher ceiling for the whole node, which bounds the lookups it makes for a crowd of clients.
const (
	cnameCheckPerIPMinute = 20
	cnameCheckPerMinute   = 300
)

type cnameLimiter struct {
	mu     sync.Mutex
	window time.Time
	n      int
	perIP  map[string]int
}

// allow counts one request of ip in the current minute. A refusal because of ip's own count does
// not use the node's budget, and the table only grows with allowed requests, so it holds at most
// cnameCheckPerMinute addresses.
func (l *cnameLimiter) allow(now time.Time, ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.perIP == nil || now.Sub(l.window) >= time.Minute {
		l.window, l.n, l.perIP = now, 0, map[string]int{}
	}
	if l.perIP[ip] >= cnameCheckPerIPMinute || l.n >= cnameCheckPerMinute {
		return false
	}
	l.perIP[ip]++
	l.n++
	return true
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
	if !s.cnameLimit.allow(time.Now(), clientIP(r)) {
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
