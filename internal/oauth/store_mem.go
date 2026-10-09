package oauth

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"sort"
	"sync"
	"time"
)

// MemoryStore is the in-memory Store: for tests, the dev mock and a registry that is not Postgres.
// Everything is lost on restart. It follows the contract of Store (and the tests in storetest) in
// every detail the Service relies on: the same errors for the same conditions, ids assigned from a
// counter, atomic transitions under one lock, and rollback of everything fn wrote when fn fails.
//
// One mutex guards all state, so a callback of WithCode or RotateRefresh must not call back into the
// store (the Service never does).
type MemoryStore struct {
	mu sync.Mutex

	// orgSlug resolves organization ids to slugs for the OrgSlug fields; the registry owns that table,
	// which this store does not have. Nil resolves nothing (the slugs are empty).
	orgSlug func(orgID int64) string

	apps      map[string]*App
	secrets   map[string]*AppSecret
	auths     map[string]*Authorization
	grants    map[int64]*Grant
	tokens    map[int64]*Token
	byHash    map[string]int64 // token hash -> token id
	nextGrant int64
	nextToken int64
}

// NewMemoryStore returns an empty MemoryStore.
func NewMemoryStore() *MemoryStore { return &MemoryStore{} }

var _ Store = (*MemoryStore)(nil)

// SetOrgSlugs sets the function that turns an organization id into its slug, for the OrgSlug of
// authorizations, access information and grant listings. Call it before use.
func (m *MemoryStore) SetOrgSlugs(f func(orgID int64) string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.orgSlug = f
}

// lock takes the mutex, makes sure the maps exist, and returns the unlock function: defer m.lock()().
func (m *MemoryStore) lock() func() {
	m.mu.Lock()
	if m.apps == nil {
		m.apps = map[string]*App{}
		m.secrets = map[string]*AppSecret{}
		m.auths = map[string]*Authorization{}
		m.grants = map[int64]*Grant{}
		m.tokens = map[int64]*Token{}
		m.byHash = map[string]int64{}
	}
	return m.mu.Unlock
}

func (m *MemoryStore) slug(orgID int64) string {
	if m.orgSlug == nil || orgID == 0 {
		return ""
	}
	return m.orgSlug(orgID)
}

// ---- copies: the store never hands out a pointer into its own state

func memStrings(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return slices.Clone(s)
}

func memBytes(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	return bytes.Clone(b)
}

func memTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	c := *t
	return &c
}

func memApp(a *App) App {
	c := *a
	c.RedirectURIs, c.Scopes = memStrings(a.RedirectURIs), memStrings(a.Scopes)
	c.LastAuthorizedAt, c.DeletedAt = memTime(a.LastAuthorizedAt), memTime(a.DeletedAt)
	return c
}

func memSecret(s *AppSecret) AppSecret {
	c := *s
	c.Hash, c.LastUsedAt = memBytes(s.Hash), memTime(s.LastUsedAt)
	return c
}

func memAuth(a *Authorization) Authorization {
	c := *a
	c.Scopes, c.CodeHash = memStrings(a.Scopes), memBytes(a.CodeHash)
	c.DecidedAt, c.CodeExpiresAt, c.CodeUsedAt = memTime(a.DecidedAt), memTime(a.CodeExpiresAt), memTime(a.CodeUsedAt)
	return c
}

func memGrant(g *Grant) Grant {
	c := *g
	c.Scopes = memStrings(g.Scopes)
	c.LastUsedAt, c.RevokedAt = memTime(g.LastUsedAt), memTime(g.RevokedAt)
	return c
}

func memToken(t *Token) Token {
	c := *t
	c.Hash, c.LastUsedAt, c.UsedAt = memBytes(t.Hash), memTime(t.LastUsedAt), memTime(t.UsedAt)
	return c
}

// authOut is an authorization as callers see it: a copy with the organization slug filled.
func (m *MemoryStore) authOut(a *Authorization) Authorization {
	c := memAuth(a)
	if c.OrgID != 0 {
		c.OrgSlug = m.slug(c.OrgID)
	}
	return c
}

// ---- transactions: an undo log, replayed backwards when the callback fails or panics

type memUndo []func()

func (u *memUndo) add(f func()) { *u = append(*u, f) }

// run calls fn and rolls back what it logged unless it returns nil.
func (u *memUndo) run(fn func() error) error {
	ok := false
	defer func() {
		if !ok {
			for i := len(*u) - 1; i >= 0; i-- {
				(*u)[i]()
			}
		}
	}()
	err := fn()
	ok = err == nil
	return err
}

// ---- apps

func (m *MemoryStore) CreateApp(ctx context.Context, app App, secret *AppSecret) error {
	defer m.lock()()
	id, ok := canonUUID(app.ID)
	if !ok {
		return fmt.Errorf("oauth: app id %q is not a UUID", app.ID)
	}
	if _, dup := m.apps[id]; dup {
		return ErrConflict
	}
	var sec AppSecret
	if secret != nil {
		sec = memSecret(secret)
		sid, ok := canonUUID(sec.ID)
		if !ok {
			return fmt.Errorf("oauth: secret id %q is not a UUID", sec.ID)
		}
		if err := m.checkSecretFree(sid, sec.Hash); err != nil {
			return err
		}
		sec.ID, sec.AppID = sid, id
	}
	a := memApp(&app)
	a.ID = id
	m.apps[id] = &a
	if secret != nil {
		m.secrets[sec.ID] = &sec
	}
	return nil
}

// checkSecretFree returns ErrConflict if a secret has this id or hash.
func (m *MemoryStore) checkSecretFree(id string, hash []byte) error {
	if len(hash) == 0 {
		return fmt.Errorf("oauth: a client secret needs a hash")
	}
	if _, dup := m.secrets[id]; dup {
		return ErrConflict
	}
	for _, s := range m.secrets {
		if bytes.Equal(s.Hash, hash) {
			return ErrConflict
		}
	}
	return nil
}

func (m *MemoryStore) liveApp(id string) (*App, bool) {
	id, ok := canonUUID(id)
	if !ok {
		return nil, false
	}
	a, ok := m.apps[id]
	if !ok || a.DeletedAt != nil {
		return nil, false
	}
	return a, true
}

func (m *MemoryStore) GetApp(ctx context.Context, id string) (*App, error) {
	defer m.lock()()
	a, ok := m.liveApp(id)
	if !ok {
		return nil, ErrNotFound
	}
	c := memApp(a)
	return &c, nil
}

func (m *MemoryStore) UpdateApp(ctx context.Context, app App) error {
	defer m.lock()()
	a, ok := m.liveApp(app.ID)
	if !ok {
		return ErrNotFound
	}
	a.Name, a.Website, a.Icon, a.UpdatedAt = app.Name, app.Website, app.Icon, app.UpdatedAt
	a.RedirectURIs, a.Scopes = memStrings(app.RedirectURIs), memStrings(app.Scopes)
	return nil
}

func (m *MemoryStore) DeleteApp(ctx context.Context, id string, at time.Time) ([]Grant, error) {
	defer m.lock()()
	a, ok := m.liveApp(id)
	if !ok {
		return nil, ErrNotFound
	}
	t := at
	a.DeletedAt = &t
	var revoked []Grant
	for _, gid := range m.grantIDs() {
		if g := m.grants[gid]; g.AppID == a.ID && g.RevokedAt == nil {
			g.RevokedAt, g.RevokedReason = memTime(&t), ReasonAppDeleted
			revoked = append(revoked, memGrant(g))
		}
	}
	return revoked, nil
}

func (m *MemoryStore) ListManualApps(ctx context.Context, orgID int64) ([]App, error) {
	defer m.lock()()
	var out []App
	for _, a := range m.apps {
		if a.DeletedAt == nil && a.RegistrationType == RegistrationManual && a.OrgID == orgID {
			out = append(out, memApp(a))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (m *MemoryStore) CountDynamicApps(ctx context.Context) (int, error) {
	defer m.lock()()
	n := 0
	for _, a := range m.apps {
		if a.DeletedAt == nil && a.RegistrationType == RegistrationDynamic {
			n++
		}
	}
	return n, nil
}

func (m *MemoryStore) CountManualApps(ctx context.Context, orgID int64) (int, error) {
	defer m.lock()()
	n := 0
	for _, a := range m.apps {
		if a.DeletedAt == nil && a.RegistrationType == RegistrationManual && a.OrgID == orgID {
			n++
		}
	}
	return n, nil
}

// ---- client secrets

func (m *MemoryStore) ListSecrets(ctx context.Context, appID string) ([]AppSecret, error) {
	defer m.lock()()
	id, ok := canonUUID(appID)
	if !ok {
		return nil, nil
	}
	var out []AppSecret
	for _, s := range m.secrets {
		if s.AppID == id {
			out = append(out, memSecret(s))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (m *MemoryStore) CreateSecret(ctx context.Context, s AppSecret) error {
	defer m.lock()()
	aid, ok := canonUUID(s.AppID)
	if !ok {
		return ErrNotFound
	}
	if _, ok := m.apps[aid]; !ok { // the row must exist; a soft-deleted app still has its rows
		return ErrNotFound
	}
	sid, ok := canonUUID(s.ID)
	if !ok {
		return fmt.Errorf("oauth: secret id %q is not a UUID", s.ID)
	}
	if err := m.checkSecretFree(sid, s.Hash); err != nil {
		return err
	}
	c := memSecret(&s)
	c.ID, c.AppID = sid, aid
	m.secrets[sid] = &c
	return nil
}

func (m *MemoryStore) DeleteSecret(ctx context.Context, appID, secretID string) error {
	defer m.lock()()
	aid, ok1 := canonUUID(appID)
	sid, ok2 := canonUUID(secretID)
	if !ok1 || !ok2 {
		return ErrNotFound
	}
	if s, ok := m.secrets[sid]; !ok || s.AppID != aid {
		return ErrNotFound
	}
	delete(m.secrets, sid)
	return nil
}

func (m *MemoryStore) TouchSecret(ctx context.Context, secretID string, at time.Time) error {
	defer m.lock()()
	if sid, ok := canonUUID(secretID); ok {
		if s, ok := m.secrets[sid]; ok {
			s.LastUsedAt = memTime(&at)
		}
	}
	return nil
}

// ---- authorizations

func (m *MemoryStore) CreateAuthorization(ctx context.Context, a Authorization) error {
	defer m.lock()()
	id, ok := canonUUID(a.ID)
	if !ok {
		return fmt.Errorf("oauth: authorization id %q is not a UUID", a.ID)
	}
	aid, ok := canonUUID(a.AppID)
	if !ok {
		return ErrNotFound
	}
	if _, ok := m.apps[aid]; !ok {
		return ErrNotFound
	}
	if _, dup := m.auths[id]; dup {
		return ErrConflict
	}
	if len(a.CodeHash) > 0 && m.codeHashTaken(a.CodeHash) {
		return ErrConflict
	}
	c := memAuth(&a)
	c.ID, c.AppID, c.OrgSlug = id, aid, ""
	if c.Status == "" {
		c.Status = StatusPending
	}
	m.auths[id] = &c
	return nil
}

func (m *MemoryStore) codeHashTaken(h []byte) bool {
	for _, a := range m.auths {
		if len(a.CodeHash) > 0 && bytes.Equal(a.CodeHash, h) {
			return true
		}
	}
	return false
}

func (m *MemoryStore) GetAuthorization(ctx context.Context, id string) (*Authorization, error) {
	defer m.lock()()
	nid, ok := canonUUID(id)
	if !ok {
		return nil, ErrNotFound
	}
	a, ok := m.auths[nid]
	if !ok {
		return nil, ErrNotFound
	}
	out := m.authOut(a)
	return &out, nil
}

func (m *MemoryStore) DecideAuthorization(ctx context.Context, id string, d Decision) (*Authorization, error) {
	defer m.lock()()
	nid, ok := canonUUID(id)
	if !ok {
		return nil, ErrNotFound
	}
	a, ok := m.auths[nid]
	if !ok {
		return nil, ErrNotFound
	}
	if a.Status != StatusPending {
		return nil, ErrAlreadyDecided
	}
	if !a.ExpiresAt.After(d.At) {
		return nil, ErrExpired
	}
	switch d.Status {
	case StatusApproved:
		if len(d.CodeHash) == 0 {
			return nil, fmt.Errorf("oauth: an approval needs a code hash")
		}
		if m.codeHashTaken(d.CodeHash) {
			return nil, ErrConflict
		}
	case StatusDeclined:
	default:
		return nil, fmt.Errorf("oauth: a decision is approved or declined, not %q", d.Status)
	}
	a.Status, a.DecidedBy, a.DecidedAt = d.Status, d.DecidedBy, memTime(&d.At)
	if d.Status == StatusApproved {
		a.OrgID, a.CodeHash, a.CodeExpiresAt = d.OrgID, memBytes(d.CodeHash), memTime(&d.CodeExpiresAt)
	}
	out := m.authOut(a)
	return &out, nil
}

func (m *MemoryStore) CountPending(ctx context.Context, appID string, now time.Time) (int, error) {
	defer m.lock()()
	aid := ""
	if appID != "" {
		var ok bool
		if aid, ok = canonUUID(appID); !ok {
			return 0, nil
		}
	}
	n := 0
	for _, a := range m.auths {
		if a.Status == StatusPending && a.ExpiresAt.After(now) && (aid == "" || a.AppID == aid) {
			n++
		}
	}
	return n, nil
}

func (m *MemoryStore) WithCode(ctx context.Context, appID string, codeHash []byte, fn func(ctx context.Context, tx CodeTx) error) error {
	defer m.lock()()
	aid, ok := canonUUID(appID)
	if !ok || len(codeHash) == 0 {
		return ErrNotFound
	}
	var row *Authorization
	for _, a := range m.auths {
		if a.AppID == aid && bytes.Equal(a.CodeHash, codeHash) {
			row = a
			break
		}
	}
	if row == nil {
		return ErrNotFound
	}
	tx := &memCodeTx{m: m, row: row, snap: m.authOut(row)}
	return tx.undo.run(func() error { return fn(ctx, tx) })
}

type memCodeTx struct {
	m    *MemoryStore
	row  *Authorization
	snap Authorization
	undo memUndo
}

var _ CodeTx = (*memCodeTx)(nil)

func (tx *memCodeTx) Authorization() Authorization { return memAuth(&tx.snap) }

func (tx *memCodeTx) Burn(ctx context.Context, at time.Time) error {
	old := memAuth(tx.row)
	tx.undo.add(func() { *tx.row = old })
	tx.row.Status, tx.row.CodeUsedAt = StatusExchanged, memTime(&at)
	return nil
}

func (tx *memCodeTx) Complete(ctx context.Context, in NewGrant) (*CompleteResult, error) {
	m, row := tx.m, tx.row
	if row.Status != StatusApproved {
		return nil, ErrAlreadyDecided
	}
	aid, ok := canonUUID(in.Grant.AppID)
	if !ok {
		return nil, fmt.Errorf("oauth: grant app id %q is not a UUID", in.Grant.AppID)
	}
	if err := m.checkTokensFree(in.Access.Hash, in.Refresh.Hash); err != nil {
		return nil, err
	}
	at := in.At
	var superseded []Grant
	supersede := func(g *Grant) {
		old := memGrant(g)
		tx.undo.add(func() { *g = old })
		g.RevokedAt, g.RevokedReason = memTime(&at), ReasonSuperseded
		superseded = append(superseded, memGrant(g))
	}
	for _, id := range m.grantIDs() {
		if g := m.grants[id]; g.RevokedAt == nil && g.AppID == aid && g.UserID == in.Grant.UserID && g.OrgID == in.Grant.OrgID {
			supersede(g)
		}
	}
	created := in.Grant.CreatedAt
	if created.IsZero() {
		created = at
	}
	m.nextGrant++
	ng := &Grant{ID: m.nextGrant, AppID: aid, UserID: in.Grant.UserID, OrgID: in.Grant.OrgID,
		Scopes: memStrings(in.Grant.Scopes), Resource: in.Grant.Resource, CreatedAt: created}
	m.grants[ng.ID] = ng
	tx.undo.add(func() { delete(m.grants, ng.ID) })
	for _, t := range []struct {
		kind string
		tok  Token
	}{{KindAccess, in.Access}, {KindRefresh, in.Refresh}} {
		m.addToken(&tx.undo, ng.ID, t.kind, t.tok)
	}
	if in.MaxLive > 0 {
		var live []*Grant
		for _, id := range m.grantIDs() {
			if g := m.grants[id]; g.RevokedAt == nil && g.UserID == ng.UserID && g.OrgID == ng.OrgID {
				live = append(live, g)
			}
		}
		sort.Slice(live, func(i, j int) bool {
			if !live[i].CreatedAt.Equal(live[j].CreatedAt) {
				return live[i].CreatedAt.Before(live[j].CreatedAt)
			}
			return live[i].ID < live[j].ID
		})
		for len(live) > in.MaxLive {
			supersede(live[0])
			live = live[1:]
		}
	}
	old := memAuth(row)
	tx.undo.add(func() { *row = old })
	row.Status, row.GrantID, row.CodeUsedAt = StatusExchanged, ng.ID, memTime(&at)
	if app, ok := m.apps[aid]; ok {
		prev := memTime(app.LastAuthorizedAt)
		tx.undo.add(func() { app.LastAuthorizedAt = prev })
		app.LastAuthorizedAt = memTime(&at)
	}
	return &CompleteResult{Grant: memGrant(ng), Superseded: superseded}, nil
}

// checkTokensFree returns ErrConflict if one of the hashes is stored already or the two are alike.
func (m *MemoryStore) checkTokensFree(hashes ...[]byte) error {
	seen := map[string]bool{}
	for _, h := range hashes {
		if len(h) == 0 {
			return fmt.Errorf("oauth: a token needs a hash")
		}
		if _, dup := m.byHash[string(h)]; dup || seen[string(h)] {
			return ErrConflict
		}
		seen[string(h)] = true
	}
	return nil
}

// addToken stores a token of the grant. The caller has checked the hash with checkTokensFree.
func (m *MemoryStore) addToken(u *memUndo, grantID int64, kind string, t Token) *Token {
	m.nextToken++
	nt := &Token{ID: m.nextToken, GrantID: grantID, Kind: kind, Hash: memBytes(t.Hash), Prefix: t.Prefix,
		CreatedAt: t.CreatedAt, ExpiresAt: t.ExpiresAt}
	m.tokens[nt.ID] = nt
	m.byHash[string(nt.Hash)] = nt.ID
	u.add(func() {
		delete(m.tokens, nt.ID)
		delete(m.byHash, string(nt.Hash))
	})
	return nt
}

// grantIDs returns the ids of all grants in ascending order, so that loops are deterministic.
func (m *MemoryStore) grantIDs() []int64 {
	ids := make([]int64, 0, len(m.grants))
	for id := range m.grants {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// ---- grants and tokens

func (m *MemoryStore) LookupAccess(ctx context.Context, tokenHash []byte, now time.Time) (*AccessInfo, error) {
	defer m.lock()()
	tid, ok := m.byHash[string(tokenHash)]
	if !ok {
		return nil, ErrNotFound
	}
	t := m.tokens[tid]
	if t.Kind != KindAccess || !t.ExpiresAt.After(now) {
		return nil, ErrNotFound
	}
	g := m.grants[t.GrantID]
	if g == nil || g.RevokedAt != nil {
		return nil, ErrNotFound
	}
	app := m.apps[g.AppID]
	if app == nil || app.DeletedAt != nil {
		return nil, ErrNotFound
	}
	return &AccessInfo{
		TokenID: t.ID, ExpiresAt: t.ExpiresAt, GrantID: g.ID, AppID: app.ID, AppName: app.Name, UserID: g.UserID,
		OrgID: g.OrgID, OrgSlug: m.slug(g.OrgID), Resource: g.Resource,
		GrantScopes: memStrings(g.Scopes), AppScopes: memStrings(app.Scopes),
	}, nil
}

func (m *MemoryStore) TouchToken(ctx context.Context, tokenID, grantID int64, at time.Time) error {
	defer m.lock()()
	if t, ok := m.tokens[tokenID]; ok {
		t.LastUsedAt = memTime(&at)
	}
	if g, ok := m.grants[grantID]; ok {
		g.LastUsedAt = memTime(&at)
	}
	return nil
}

func (m *MemoryStore) GetToken(ctx context.Context, tokenHash []byte) (*Token, error) {
	defer m.lock()()
	tid, ok := m.byHash[string(tokenHash)]
	if !ok {
		return nil, ErrNotFound
	}
	c := memToken(m.tokens[tid])
	return &c, nil
}

func (m *MemoryStore) GetGrant(ctx context.Context, id int64) (*Grant, error) {
	defer m.lock()()
	g, ok := m.grants[id]
	if !ok {
		return nil, ErrNotFound
	}
	c := memGrant(g)
	return &c, nil
}

// memMatches reports whether the grant satisfies every field f sets.
func memMatches(f GrantFilter, g *Grant) bool {
	return (f.ID == 0 || g.ID == f.ID) && (f.AppID == "" || g.AppID == f.AppID) &&
		(f.UserID == "" || g.UserID == f.UserID) && (f.OrgID == 0 || g.OrgID == f.OrgID) &&
		(!f.Live || g.RevokedAt == nil)
}

func (m *MemoryStore) ListGrants(ctx context.Context, f GrantFilter) ([]GrantInfo, error) {
	defer m.lock()()
	if f.AppID != "" {
		id, ok := canonUUID(f.AppID)
		if !ok {
			return nil, nil
		}
		f.AppID = id
	}
	var gs []*Grant
	for _, id := range m.grantIDs() {
		if g := m.grants[id]; memMatches(f, g) {
			gs = append(gs, g)
		}
	}
	sort.Slice(gs, func(i, j int) bool { // newest first
		if !gs[i].CreatedAt.Equal(gs[j].CreatedAt) {
			return gs[i].CreatedAt.After(gs[j].CreatedAt)
		}
		return gs[i].ID > gs[j].ID
	})
	if f.Limit > 0 && len(gs) > f.Limit {
		gs = gs[:f.Limit]
	}
	out := make([]GrantInfo, 0, len(gs))
	for _, g := range gs {
		gi := GrantInfo{Grant: memGrant(g), OrgSlug: m.slug(g.OrgID)}
		if app, ok := m.apps[g.AppID]; ok {
			gi.App = memApp(app)
		}
		out = append(out, gi)
	}
	return out, nil
}

func (m *MemoryStore) RevokeGrants(ctx context.Context, f GrantFilter, reason string, at time.Time) ([]Grant, error) {
	defer m.lock()()
	if !ValidReason(reason) {
		return nil, fmt.Errorf("%w: %q is not a revocation reason", ErrInvalid, reason)
	}
	if f.AppID != "" {
		id, ok := canonUUID(f.AppID)
		if !ok {
			return nil, nil
		}
		f.AppID = id
	}
	var out []Grant
	for _, id := range m.grantIDs() {
		if g := m.grants[id]; g.RevokedAt == nil && memMatches(f, g) {
			g.RevokedAt, g.RevokedReason = memTime(&at), reason
			out = append(out, memGrant(g))
		}
	}
	return out, nil
}

func (m *MemoryStore) RotateRefresh(ctx context.Context, in RotateInput, fn func(ctx context.Context, tx RotateTx) error) error {
	defer m.lock()()
	aid, ok := canonUUID(in.AppID)
	if !ok {
		return ErrNotFound
	}
	tid, ok := m.byHash[string(in.TokenHash)]
	if !ok {
		return ErrNotFound
	}
	t := m.tokens[tid]
	if t.Kind != KindRefresh || !t.ExpiresAt.After(in.Now) {
		return ErrNotFound
	}
	g := m.grants[t.GrantID]
	if g == nil || g.RevokedAt != nil || g.AppID != aid {
		return ErrNotFound
	}
	if t.UsedAt != nil && !t.UsedAt.After(in.Now.Add(-in.Grace)) {
		return &ReuseError{GrantID: g.ID, AppID: g.AppID, UserID: g.UserID, OrgID: g.OrgID}
	}
	tx := &memRotateTx{m: m, tok: t, grant: g}
	prev := memTime(t.UsedAt)
	tx.undo.add(func() { t.UsedAt = prev })
	if t.UsedAt == nil {
		t.UsedAt = memTime(&in.Now)
	}
	return tx.undo.run(func() error { return fn(ctx, tx) })
}

type memRotateTx struct {
	m     *MemoryStore
	tok   *Token
	grant *Grant
	undo  memUndo
}

var _ RotateTx = (*memRotateTx)(nil)

func (tx *memRotateTx) Token() Token { return memToken(tx.tok) }
func (tx *memRotateTx) Grant() Grant { return memGrant(tx.grant) }

func (tx *memRotateTx) NarrowScopes(ctx context.Context, scopes []string) error {
	if !SubsetOf(scopes, tx.grant.Scopes) {
		return fmt.Errorf("%w: scopes may be narrowed, not widened", ErrInvalid)
	}
	prev := tx.grant.Scopes
	tx.undo.add(func() { tx.grant.Scopes = prev })
	tx.grant.Scopes = memStrings(scopes)
	return nil
}

func (tx *memRotateTx) Issue(ctx context.Context, access, refresh Token) error {
	if err := tx.m.checkTokensFree(access.Hash, refresh.Hash); err != nil {
		return err
	}
	tx.m.addToken(&tx.undo, tx.grant.ID, KindAccess, access)
	nr := tx.m.addToken(&tx.undo, tx.grant.ID, KindRefresh, refresh)
	if tx.tok.ReplacedBy == 0 {
		tx.undo.add(func() { tx.tok.ReplacedBy = 0 })
		tx.tok.ReplacedBy = nr.ID
	}
	return nil
}

func (tx *memRotateTx) RevokeGrant(ctx context.Context, reason string, at time.Time) error {
	if !ValidReason(reason) {
		return fmt.Errorf("%w: %q is not a revocation reason", ErrInvalid, reason)
	}
	if tx.grant.RevokedAt != nil {
		return nil
	}
	prev := memGrant(tx.grant)
	tx.undo.add(func() { *tx.grant = prev })
	tx.grant.RevokedAt, tx.grant.RevokedReason = memTime(&at), reason
	return nil
}

// ---- housekeeping

func (m *MemoryStore) Prune(ctx context.Context, p PruneParams) (PruneResult, error) {
	defer m.lock()()
	var res PruneResult
	for id, a := range m.auths {
		if a.ExpiresAt.Before(p.AuthorizationsBefore) {
			delete(m.auths, id)
			res.Authorizations++
		}
	}
	for id, t := range m.tokens {
		if t.ExpiresAt.Before(p.TokensBefore) {
			m.deleteToken(id, t)
			res.Tokens++
		}
	}
	for _, id := range m.grantIDs() {
		if g := m.grants[id]; g.RevokedAt != nil && g.RevokedAt.Before(p.RevokedGrantsBefore) {
			m.deleteGrant(id)
			res.Grants++
		}
	}
	for id, a := range m.apps {
		if a.RegistrationType != RegistrationDynamic {
			continue
		}
		unused := a.LastAuthorizedAt == nil && a.CreatedAt.Before(p.UnusedAppsBefore)
		last := a.CreatedAt
		if a.LastAuthorizedAt != nil {
			last = *a.LastAuthorizedAt
		}
		if unused || (!m.hasLiveGrant(id) && last.Before(p.IdleAppsBefore)) {
			m.deleteApp(id)
			res.Apps++
		}
	}
	return res, nil
}

func (m *MemoryStore) hasLiveGrant(appID string) bool {
	for _, g := range m.grants {
		if g.AppID == appID && g.RevokedAt == nil {
			return true
		}
	}
	return false
}

func (m *MemoryStore) deleteToken(id int64, t *Token) {
	delete(m.byHash, string(t.Hash))
	delete(m.tokens, id)
}

// deleteGrant removes a grant with its tokens; authorizations that led to it keep existing and lose
// the link (on delete set null).
func (m *MemoryStore) deleteGrant(id int64) {
	for tid, t := range m.tokens {
		if t.GrantID == id {
			m.deleteToken(tid, t)
		}
	}
	for _, a := range m.auths {
		if a.GrantID == id {
			a.GrantID = 0
		}
	}
	delete(m.grants, id)
}

// deleteApp removes an app with everything that references it (on delete cascade).
func (m *MemoryStore) deleteApp(id string) {
	for sid, s := range m.secrets {
		if s.AppID == id {
			delete(m.secrets, sid)
		}
	}
	for aid, a := range m.auths {
		if a.AppID == id {
			delete(m.auths, aid)
		}
	}
	for _, gid := range m.grantIDs() {
		if m.grants[gid].AppID == id {
			m.deleteGrant(gid)
		}
	}
	delete(m.apps, id)
}
