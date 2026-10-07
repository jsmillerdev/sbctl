package proxy

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/OWNER/sbctl/internal/config"
	"github.com/OWNER/sbctl/internal/registry"
	"github.com/OWNER/sbctl/internal/secrets"
)

const (
	// keysTTL bounds how long project keys stay cached when no change notification
	// arrives (LISTEN/NOTIFY delivery is best effort).
	keysTTL = 5 * time.Minute
	// resyncInterval is the safety-net full reload of the host table.
	resyncInterval = 5 * time.Minute
	reloadTimeout  = 15 * time.Second
)

// project is what the proxy needs to know about a registered project.
type project struct {
	ref    string
	seq    int
	status registry.Status
}

type keyEntry struct {
	keys    *secrets.ProjectKeys
	expires time.Time
}

// table is the host -> project cache and the per-project key cache. It is filled
// from the registry and kept current by registry change notifications.
type table struct {
	cfg  *config.Config
	reg  registry.Registry
	keys KeySource
	log  *slog.Logger
	now  func() time.Time
	// retry is the first resubscribe delay; it doubles up to 30 s while subscribing fails.
	retry time.Duration

	mu       sync.RWMutex
	projects map[string]project // by ref
	custom   map[string]string  // registry routes (any kind): host -> ref
	keyCache map[string]keyEntry
	// keyGen counts invalidations per ref and keyEpoch counts full reloads. A key
	// fetch caches its result only if neither moved while it ran, so a fetch that
	// read secrets before a rotation committed cannot undo the invalidation.
	keyGen   map[string]uint64
	keyEpoch uint64
	sf       singleflight.Group
}

func newTable(cfg *config.Config, reg registry.Registry, keys KeySource, log *slog.Logger) *table {
	return &table{
		cfg: cfg, reg: reg, keys: keys, log: log, now: time.Now, retry: time.Second,
		projects: map[string]project{}, custom: map[string]string{}, keyCache: map[string]keyEntry{},
		keyGen: map[string]uint64{},
	}
}

// normalizeHost lower-cases host and drops the port and a trailing dot.
func normalizeHost(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	if strings.HasPrefix(h, "[") { // [::1]:443
		if i := strings.Index(h, "]"); i > 0 {
			return h[1:i]
		}
	}
	if i := strings.LastIndexByte(h, ':'); i >= 0 && !strings.Contains(h[:i], ":") {
		h = h[:i]
	}
	return strings.TrimSuffix(h, ".")
}

// lookup resolves a request host to a project: either the derived
// <ref>.api.<domain> host of a known project, or a registry route.
func (t *table) lookup(host string) (project, bool) {
	host = normalizeHost(host)
	t.mu.RLock()
	defer t.mu.RUnlock()
	// A derived project host always belongs to its own project; routes cannot take it over.
	if ref := t.cfg.RefFromProjectHost(host); ref != "" {
		p, ok := t.projects[ref]
		return p, ok
	}
	if ref, ok := t.custom[host]; ok {
		p, ok := t.projects[ref]
		return p, ok
	}
	return project{}, false
}

// routeKind reports how host reaches a project: "derived", "custom" or "".
func (t *table) routeKind(host string) string {
	host = normalizeHost(host)
	t.mu.RLock()
	defer t.mu.RUnlock()
	if ref := t.cfg.RefFromProjectHost(host); ref != "" {
		if _, ok := t.projects[ref]; ok {
			return "derived"
		}
	}
	if ref, ok := t.custom[host]; ok {
		if _, ok := t.projects[ref]; ok {
			return "custom"
		}
	}
	return ""
}

func routable(ref string) bool { return ref != config.SystemRef }

// reload replaces the whole table from the registry and drops cached keys.
func (t *table) reload(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, reloadTimeout)
	defer cancel()
	ps, err := t.reg.ListProjects(ctx)
	if err != nil {
		return err
	}
	rs, err := t.reg.ListRoutes(ctx)
	if err != nil {
		return err
	}
	projects := make(map[string]project, len(ps))
	for _, p := range ps {
		if routable(p.Ref) {
			projects[p.Ref] = project{ref: p.Ref, seq: p.Seq, status: p.Status}
		}
	}
	t.mu.Lock()
	t.projects = projects
	t.custom = t.customRoutes(rs)
	t.dropAllKeysLocked()
	t.mu.Unlock()
	t.log.Debug("proxy table reloaded", "projects", len(projects), "routes", len(rs))
	return nil
}

// customRoutes indexes registry routes by host. Rows for a host the proxy owns
// itself (a derived project host, api.<domain>, studio.<domain>) are ignored: no
// route may take over another project's host or the control-plane hosts.
func (t *table) customRoutes(rs []registry.Route) map[string]string {
	m := make(map[string]string, len(rs))
	for _, r := range rs {
		h := normalizeHost(r.Host)
		if t.cfg.RefFromProjectHost(h) != "" || h == t.cfg.APIHost() || h == t.cfg.StudioHost() {
			t.log.Warn("proxy table: ignoring route for a host the proxy owns", "host", h, "ref", r.Ref)
			continue
		}
		m[h] = r.Ref
	}
	return m
}

// apply updates the table for one registry change.
func (t *table) apply(ctx context.Context, c registry.Change) {
	switch c.Table {
	case "projects":
		if c.Op == "delete" {
			t.mu.Lock()
			delete(t.projects, c.Key)
			t.dropKeysLocked(c.Key)
			t.mu.Unlock()
			return
		}
		p, err := t.reg.GetProject(ctx, c.Key)
		switch {
		case errors.Is(err, registry.ErrNotFound):
			t.mu.Lock()
			delete(t.projects, c.Key)
			t.dropKeysLocked(c.Key)
			t.mu.Unlock()
		case err != nil:
			t.log.Warn("proxy table: project refresh failed, reloading", "ref", c.Key, "err", err)
			_ = t.reload(ctx)
		case routable(p.Ref):
			t.mu.Lock()
			t.projects[p.Ref] = project{ref: p.Ref, seq: p.Seq, status: p.Status}
			t.mu.Unlock()
		}
	case "routes":
		rs, err := t.reg.ListRoutes(ctx)
		if err != nil {
			t.log.Warn("proxy table: route refresh failed", "err", err)
			return
		}
		t.mu.Lock()
		t.custom = t.customRoutes(rs)
		t.mu.Unlock()
	case "project_secrets":
		t.invalidateKeys(c.Key)
	}
}

// dropKeysLocked forgets ref's cached keys and marks fetches in flight stale.
// t.mu must be held.
func (t *table) dropKeysLocked(ref string) {
	delete(t.keyCache, ref)
	t.keyGen[ref]++
}

// dropAllKeysLocked is dropKeysLocked for every project. t.mu must be held.
func (t *table) dropAllKeysLocked() {
	t.keyCache = map[string]keyEntry{}
	t.keyEpoch++
}

func (t *table) invalidateKeys(ref string) {
	t.mu.Lock()
	t.dropKeysLocked(ref)
	t.mu.Unlock()
	// A fetch already in flight may carry the old keys; forget it so the next request refetches.
	t.sf.Forget(ref)
}

// projectKeys returns the project's credentials, cached until a change
// notification or the TTL.
func (t *table) projectKeys(ctx context.Context, ref string) (*secrets.ProjectKeys, error) {
	t.mu.RLock()
	e, ok := t.keyCache[ref]
	t.mu.RUnlock()
	if ok && t.now().Before(e.expires) {
		return e.keys, nil
	}
	v, err, _ := t.sf.Do(ref, func() (any, error) {
		// Detached from the caller: one client hanging up must not fail the shared fetch.
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		t.mu.RLock()
		gen, epoch := t.keyGen[ref], t.keyEpoch
		t.mu.RUnlock()
		k, err := t.keys.Keys(fctx, ref)
		if err != nil {
			return nil, err
		}
		t.mu.Lock()
		// Cache only if nothing invalidated ref while the fetch ran; otherwise this
		// request still gets k, but the next one refetches.
		if t.keyGen[ref] == gen && t.keyEpoch == epoch {
			t.keyCache[ref] = keyEntry{keys: k, expires: t.now().Add(keysTTL)}
		}
		t.mu.Unlock()
		return k, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*secrets.ProjectKeys), nil
}

// sync keeps the table current until ctx ends: subscribe, reload, apply changes;
// when the subscription channel closes, resubscribe (after a backoff) and reload
// in full, so no change is lost across a dropped connection.
func (t *table) sync(ctx context.Context) {
	backoff := t.retry
	resync := time.NewTicker(resyncInterval)
	defer resync.Stop()
	for ctx.Err() == nil {
		ch, err := t.reg.Subscribe(ctx)
		if err != nil {
			t.log.Warn("proxy table: subscribe failed", "err", err, "retry_in", backoff)
		}
		// Reload after subscribing so a change between the two cannot be missed.
		if rerr := t.reload(ctx); rerr != nil && ctx.Err() == nil {
			t.log.Warn("proxy table: reload failed", "err", rerr)
		}
		if err == nil {
			backoff = t.retry
		loop:
			for {
				select {
				case c, ok := <-ch:
					if !ok {
						if ctx.Err() == nil {
							t.log.Warn("proxy table: change stream closed, resubscribing")
						}
						break loop
					}
					t.apply(ctx, c)
				case <-resync.C:
					if rerr := t.reload(ctx); rerr != nil && ctx.Err() == nil {
						t.log.Warn("proxy table: periodic reload failed", "err", rerr)
					}
				case <-ctx.Done():
					return
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if err != nil && backoff < 30*time.Second {
			backoff *= 2
		}
	}
}
