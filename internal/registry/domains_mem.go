package registry

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// heldStatus reports whether a custom hostname in status s holds its name (verified or active).
func heldStatus(s string) bool { return s == HostnameOriginReady || s == HostnameActive }

func (m *Memory) GetCustomHostname(_ context.Context, ref string) (*CustomHostname, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.hostnames[ref]
	if !ok {
		return nil, ErrNotFound
	}
	return &h, nil
}

// heldByOther reports whether a project other than ref holds hostname. m.mu must be held.
func (m *Memory) heldByOther(ref, hostname string) bool {
	for r, h := range m.hostnames {
		if r != ref && h.Hostname == hostname && heldStatus(h.Status) {
			return true
		}
	}
	return false
}

func (m *Memory) PutCustomHostname(_ context.Context, h *CustomHostname) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.projects[h.Ref]; !ok {
		return ErrNotFound
	}
	if m.heldByOther(h.Ref, h.Hostname) {
		return fmt.Errorf("%w: custom hostname held by another project", ErrConflict)
	}
	if old, ok := m.hostnames[h.Ref]; ok && old.Status == HostnameActive {
		return fmt.Errorf("%w: the project's custom hostname is active", ErrConflict)
	}
	if m.hostnames == nil {
		m.hostnames = map[string]CustomHostname{}
	}
	now := time.Now()
	m.hostnames[h.Ref] = CustomHostname{Ref: h.Ref, Hostname: h.Hostname, Status: h.Status, Token: h.Token, CreatedAt: now, UpdatedAt: now}
	return nil
}

func (m *Memory) UpdateCustomHostname(_ context.Context, h *CustomHostname) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.hostnames[h.Ref]
	if !ok {
		return ErrNotFound
	}
	if cur.Hostname != h.Hostname || cur.Status == HostnameActive {
		return fmt.Errorf("%w: the custom hostname changed meanwhile", ErrConflict)
	}
	if heldStatus(h.Status) && m.heldByOther(h.Ref, h.Hostname) {
		return fmt.Errorf("%w: custom hostname held by another project", ErrConflict)
	}
	cur.Status, cur.CNAMEOK, cur.TXTOK, cur.VerifiedAt, cur.UpdatedAt = h.Status, h.CNAMEOK, h.TXTOK, h.VerifiedAt, time.Now()
	m.hostnames[h.Ref] = cur
	return nil
}

func (m *Memory) ActivateCustomHostname(_ context.Context, ref string) (*CustomHostname, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.hostnames[ref]
	if !ok {
		return nil, ErrNotFound
	}
	if h.Status != HostnameOriginReady {
		return nil, fmt.Errorf("%w: the custom hostname is %s", ErrConflict, h.Status)
	}
	if _, taken := m.routes[h.Hostname]; taken {
		return nil, fmt.Errorf("%w: routes_pkey", ErrConflict)
	}
	now := time.Now()
	m.routes[h.Hostname] = Route{Host: h.Hostname, Ref: ref, Kind: RouteCustom, CreatedAt: now}
	h.Status, h.ActivatedAt, h.UpdatedAt = HostnameActive, &now, now
	m.hostnames[ref] = h
	m.notify("routes", "insert", h.Hostname)
	return &h, nil
}

func (m *Memory) DeleteCustomHostname(_ context.Context, ref string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.hostnames[ref]
	if !ok {
		return ErrNotFound
	}
	delete(m.hostnames, ref)
	if h.Status == HostnameActive {
		if r, ok := m.routes[h.Hostname]; ok && r.Ref == ref && r.Kind == RouteCustom {
			delete(m.routes, h.Hostname)
			m.notify("routes", "delete", h.Hostname)
		}
	}
	return nil
}

func (m *Memory) ListCustomHostnames(context.Context) ([]CustomHostname, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]CustomHostname, 0, len(m.hostnames))
	for _, h := range m.hostnames {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Hostname != out[j].Hostname {
			return out[i].Hostname < out[j].Hostname
		}
		return out[i].Ref < out[j].Ref
	})
	return out, nil
}

func (m *Memory) GetVanitySubdomain(_ context.Context, ref string) (*VanitySubdomain, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.vanity[ref]
	if !ok {
		return nil, ErrNotFound
	}
	return &v, nil
}

func (m *Memory) VanitySubdomainOwner(_ context.Context, name string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for ref, v := range m.vanity {
		if v.Name == name {
			return ref, nil
		}
	}
	return "", ErrNotFound
}

// dropVanityRoute removes ref's vanity route. m.mu must be held.
func (m *Memory) dropVanityRoute(ref string) {
	for h, r := range m.routes {
		if r.Ref == ref && r.Kind == RouteVanity {
			delete(m.routes, h)
			m.notify("routes", "delete", h)
		}
	}
}

func (m *Memory) PutVanitySubdomain(_ context.Context, ref, name, host string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.projects[ref]; !ok {
		return ErrNotFound
	}
	for r, v := range m.vanity {
		if v.Name == name && r != ref {
			return fmt.Errorf("%w: vanity_subdomains_name_key", ErrConflict)
		}
	}
	if old, taken := m.routes[host]; taken && !(old.Ref == ref && old.Kind == RouteVanity) {
		return fmt.Errorf("%w: routes_pkey", ErrConflict)
	}
	m.dropVanityRoute(ref)
	if m.vanity == nil {
		m.vanity = map[string]VanitySubdomain{}
	}
	now := time.Now()
	m.vanity[ref] = VanitySubdomain{Ref: ref, Name: name, CreatedAt: now}
	m.routes[host] = Route{Host: host, Ref: ref, Kind: RouteVanity, CreatedAt: now}
	m.notify("routes", "insert", host)
	return nil
}

func (m *Memory) DeleteVanitySubdomain(_ context.Context, ref string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.vanity[ref]; !ok {
		return ErrNotFound
	}
	delete(m.vanity, ref)
	m.dropVanityRoute(ref)
	return nil
}
