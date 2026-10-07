package fleet

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jsmillerdev/supavise/internal/config"
)

// HasTenant asks once and does not retry: a health check must not wait out EnsureTenant's backoff.
func TestHasTenantAnswersFromTheServicesOwnRecords(t *testing.T) {
	n := newTestNode(t)
	ctx := context.Background()
	const rtSecret = "realtime-api-secret-0123456789abcdef"
	const stKey = "storage-admin-key-0123456789"
	const svSecret = "supavisor-api-secret-0123456789abcdef"
	rt := newFakeAPI(t, bearerOK(rtSecret), pathTenant("/api/tenants/"), 201)
	st := newFakeAPI(t, func(h http.Header) bool { return h.Get("apikey") == stKey }, pathTenant("/tenants/"), 204)
	sv := newFakeAPI(t, bearerOK(svSecret), pathTenant("/api/tenants/"), 201)
	clRT, _ := testClient(config.SvcRealtime)
	clST, _ := testClient(config.SvcStorage)
	clSV, _ := testClient(config.SvcSupavisor)
	f := Fleet{
		&supavisorTenant{cl: clSV, store: tenantStore{reg: n.reg, sec: n.sec}, base: sv.srv.URL, secret: svSecret, now: time.Now},
		&realtimeTenant{cl: clRT, store: tenantStore{reg: n.reg, sec: n.sec}, base: rt.srv.URL, secret: rtSecret, now: time.Now},
		&storageTenant{cl: clST, store: tenantStore{reg: n.reg, sec: n.sec}, base: st.srv.URL, adminKey: stKey},
	}

	byService := func(ps []Presence) map[string]Presence {
		m := map[string]Presence{}
		for _, p := range ps {
			m[p.Service] = p
		}
		return m
	}
	got := f.TenantPresence(ctx, testRef)
	if len(got) != 3 || got[0].Service != config.SvcSupavisor || got[1].Service != config.SvcRealtime || got[2].Service != config.SvcStorage {
		t.Fatalf("the order of the fleet is not kept: %+v", got)
	}
	for _, p := range got {
		if p.Present || p.Err != nil {
			t.Errorf("%s holds a tenant nobody created: %+v", p.Service, p)
		}
	}

	rt.mu.Lock()
	rt.tenants[testRef] = true
	rt.mu.Unlock()
	st.mu.Lock()
	st.tenants[testRef] = true
	st.mu.Unlock()
	m := byService(f.TenantPresence(ctx, testRef))
	if !m[config.SvcRealtime].Present || !m[config.SvcStorage].Present || m[config.SvcSupavisor].Present {
		t.Errorf("%+v", m)
	}
	if rt.methods() != "GET GET" || st.methods() != "GET GET" {
		t.Errorf("a presence check does more than one GET: realtime %q storage %q", rt.methods(), st.methods())
	}

	// A service that answers 500 is an error, not "absent", and is asked once.
	rt.fail = func(call, int) int { return 500 }
	before := len(rt.calls)
	start := time.Now()
	m = byService(f.TenantPresence(ctx, testRef))
	if m[config.SvcRealtime].Err == nil || m[config.SvcRealtime].Present || !strings.Contains(m[config.SvcRealtime].Err.Error(), "500") && !strings.Contains(m[config.SvcRealtime].Err.Error(), "failing") {
		t.Errorf("a failing service: %+v", m[config.SvcRealtime])
	}
	if len(rt.calls)-before != 1 {
		t.Errorf("%d attempts at a failing service, want 1", len(rt.calls)-before)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("a failing service held the check for %v", time.Since(start))
	}
	// 4xx other than 404 (a wrong key) is an error too.
	rt.fail = func(call, int) int { return 403 }
	if p := byService(f.TenantPresence(ctx, testRef))[config.SvcRealtime]; p.Err == nil || p.Present {
		t.Errorf("a rejected key: %+v", p)
	}

	if _, err := (f[1].(TenantChecker)).HasTenant(ctx, "../x"); err == nil {
		t.Error("a bad ref was asked about")
	}
}

func TestPresenceSkipsTenantsThatCannotBeAsked(t *testing.T) {
	type plain struct{ Tenant }
	var f Fleet = Fleet{plain{}}
	if got := f.TenantPresence(context.Background(), testRef); len(got) != 0 {
		t.Errorf("%+v", got)
	}
	var none Fleet
	if got := none.TenantPresence(context.Background(), testRef); len(got) != 0 {
		t.Errorf("%+v", got)
	}
}

// A node that never rendered a shared service is not blamed for lacking its tenants.
func TestLazyHasTenantSkipsServicesThisNodeNeverRendered(t *testing.T) {
	n := newTestNode(t)
	lz := NewLazy(Deps{Cfg: n.cfg, Log: discardLog()})
	lz.Bind(n.reg, n.sec)
	for _, p := range lz.Fleet().TenantPresence(context.Background(), testRef) {
		if !p.Present || p.Err != nil {
			t.Errorf("%s: %+v", p.Service, p)
		}
	}
}
