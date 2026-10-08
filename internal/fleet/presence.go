package fleet

import (
	"context"
	"fmt"
	"net/http"
	"sync"
)

// TenantChecker is an optional Tenant capability: ask the service whether it holds the
// project's tenant, with one attempt and no retries (a health check must not wait out the
// backoff that EnsureTenant uses). A service that cannot be reached is an error, not "absent".
type TenantChecker interface {
	HasTenant(ctx context.Context, ref string) (bool, error)
}

// Presence is the answer of one shared service about one project.
type Presence struct {
	Service string
	Present bool
	Err     error
}

// TenantPresence asks every tenant that implements TenantChecker, concurrently, whether it
// holds ref. The result keeps the Fleet's order.
func (f Fleet) TenantPresence(ctx context.Context, ref string) []Presence {
	var out []Presence
	var idx []int
	for i, t := range f {
		if _, ok := t.(TenantChecker); ok {
			idx = append(idx, i)
		}
	}
	out = make([]Presence, len(idx))
	var wg sync.WaitGroup
	for n, i := range idx {
		wg.Add(1)
		go func() {
			defer wg.Done()
			t := f[i]
			ok, err := t.(TenantChecker).HasTenant(ctx, ref)
			out[n] = Presence{Service: t.Service(), Present: ok, Err: err}
		}()
	}
	wg.Wait()
	return out
}

// probeTenant does one GET of a tenant URL: 2xx present, 404 absent, anything else an error.
func probeTenant(ctx context.Context, cl *apiClient, name, url string, header map[string]string) (bool, error) {
	once := *cl
	once.retry = Retry{Attempts: 1}
	res, err := once.do(ctx, http.MethodGet, url, header, nil)
	switch {
	case err != nil:
		return false, err
	case res.ok():
		return true, nil
	case res.Status == http.StatusNotFound:
		return false, nil
	}
	return false, fmt.Errorf("fleet: %s answered %d for the tenant: %s", name, res.Status, res.excerpt())
}

// HasTenant implements TenantChecker. ref may be a replica identifier.
func (t *supavisorTenant) HasTenant(ctx context.Context, ref string) (bool, error) {
	if err := validTenantID(ref); err != nil {
		return false, err
	}
	h, err := t.headers()
	if err != nil {
		return false, err
	}
	return probeTenant(ctx, t.cl, "supavisor", t.tenantURL(ref), h)
}

// HasTenant implements TenantChecker. A follower parks Realtime, so it holds no tenant there and
// is not blamed for it.
func (t *realtimeTenant) HasTenant(ctx context.Context, ref string) (bool, error) {
	if err := validTenantRef(ref); err != nil {
		return false, err
	}
	if skip, err := t.skip(ctx); skip || err != nil {
		return skip, err
	}
	h, err := t.headers()
	if err != nil {
		return false, err
	}
	return probeTenant(ctx, t.cl, "realtime", t.tenantURL(ref), h)
}

// HasTenant implements TenantChecker. A follower parks Storage: see realtimeTenant.HasTenant.
func (t *storageTenant) HasTenant(ctx context.Context, ref string) (bool, error) {
	if err := validTenantRef(ref); err != nil {
		return false, err
	}
	if skip, err := t.skip(ctx); skip || err != nil {
		return skip, err
	}
	return probeTenant(ctx, t.cl, "storage", t.tenantURL(ref), t.headers())
}

// HasTenant implements TenantChecker for the lazy entries. A service this node never
// rendered is skipped: it answers present, so a node without that service is not blamed.
func (t lazyTenant) HasTenant(ctx context.Context, ref string) (bool, error) {
	r, err := t.l.tenant(ctx, t.svc)
	if err != nil {
		return false, err
	}
	if r == nil {
		return true, nil
	}
	if c, ok := r.(TenantChecker); ok {
		return c.HasTenant(ctx, ref)
	}
	return true, nil
}
