package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/OWNER/sbctl/internal/config"
	"github.com/OWNER/sbctl/internal/members"
	"github.com/OWNER/sbctl/internal/registry"
	"github.com/OWNER/sbctl/internal/secrets"
)

// Dashboard onboarding. sb-gotrue@system runs with public sign-up disabled, so the only
// way to get a dashboard account is through sbctl: the installer prints a claim token
// that creates the first administrator, and an administrator invites later users with
// `sbctl users invite <email>`, which prints an invite token for that address. A token is
// stored as its SHA-256, works once and expires. Redeeming one creates the user in
// sb-gotrue@system through GoTrue's admin API (with the sbctl_admin claim the Management
// API requires) and, for the claim token, the first organization.

const (
	claimPrefix  = "sbc_"
	invitePrefix = "sbi_"
	// DefaultClaimTTL is how long the claim token the installer prints stays valid.
	DefaultClaimTTL = 72 * time.Hour
	// DefaultInviteTTL is how long an invite stays valid.
	DefaultInviteTTL = 7 * 24 * time.Hour

	minPasswordLen = 12
	maxPasswordLen = 72 // bcrypt, which GoTrue uses, ignores everything after 72 bytes
)

// ErrClaimed is returned when a claim token is requested on a node whose first
// administrator already exists.
var ErrClaimed = errors.New("this node already has its first administrator; invite more users with `sbctl users invite`")

// Accounts issues and redeems claim and invite tokens and manages the dashboard users
// of sb-gotrue@system. The daemon builds it for the HTTP endpoints and the CLI for
// `sbctl claim` and `sbctl users`.
type Accounts struct {
	Reg   registry.Registry
	Store ClaimStore
	// Keys returns the credentials of a project; the system project's service_role key
	// authorizes the GoTrue admin API.
	Keys   func(ctx context.Context, ref string) (*secrets.ProjectKeys, error)
	Config *config.Config
	// GoTrueURL overrides http://127.0.0.1:<ports.system_gotrue>.
	GoTrueURL string
	HTTP      *http.Client
	Now       func() time.Time
	Log       *slog.Logger
	// Members assigns roles: the claimed first user becomes Owner, an invited user joins
	// the organizations that invited the address, and a removed user loses every membership.
	Members *members.Service
	// Users records a created dashboard account as soon as it exists.
	Users Store
	// LiveRefs filters invited project refs to the projects that still exist.
	LiveRefs func(ctx context.Context, refs []string) []string
	// NoMail makes invitations skip the mail even when [mail] is configured (`users invite
	// --no-mail`): the caller passes the link on.
	NoMail bool
}

func (a *Accounts) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *Accounts) log() *slog.Logger {
	if a.Log != nil {
		return a.Log
	}
	return slog.New(slog.DiscardHandler)
}

func (a *Accounts) client() *http.Client {
	if a.HTTP != nil {
		return a.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (a *Accounts) goTrueURL() string {
	if a.GoTrueURL != "" {
		return strings.TrimRight(a.GoTrueURL, "/")
	}
	return fmt.Sprintf("http://127.0.0.1:%d", a.Config.Ports.SystemGoTrue)
}

func newToken(prefix string) string { return prefix + hex.EncodeToString(secrets.RandomBytes(24)) }

// Claimed reports whether the first administrator exists.
func (a *Accounts) Claimed(ctx context.Context) (bool, error) { return a.Store.Claimed(ctx) }

// IssueClaimToken creates the token that makes the first administrator and revokes any
// earlier unused one. It fails with ErrClaimed once a claim token has been used, unless
// force is set (an administrator locked out of the dashboard).
func (a *Accounts) IssueClaimToken(ctx context.Context, ttl time.Duration, force bool) (token string, expires time.Time, err error) {
	if ttl <= 0 {
		ttl = DefaultClaimTTL
	}
	if !force {
		claimed, err := a.Store.Claimed(ctx)
		if err != nil {
			return "", time.Time{}, err
		}
		if claimed {
			return "", time.Time{}, ErrClaimed
		}
	}
	token = newToken(claimPrefix)
	expires = a.now().Add(ttl)
	if _, err = a.Store.CreateClaimToken(ctx, KindClaim, secrets.HashToken(token), "", expires); err != nil {
		return "", time.Time{}, err
	}
	return token, expires, nil
}

// IssueInvite creates an invite token for email and revokes the unused one for the same
// address.
func (a *Accounts) IssueInvite(ctx context.Context, email string, ttl time.Duration) (token string, expires time.Time, err error) {
	email, err = normalizeEmail(email)
	if err != nil {
		return "", time.Time{}, err
	}
	if ttl <= 0 {
		ttl = DefaultInviteTTL
	}
	token = newToken(invitePrefix)
	expires = a.now().Add(ttl)
	if _, err = a.Store.CreateClaimToken(ctx, KindInvite, secrets.HashToken(token), email, expires); err != nil {
		return "", time.Time{}, err
	}
	return token, expires, nil
}

func normalizeEmail(s string) (string, error) {
	s = strings.TrimSpace(s)
	addr, err := mail.ParseAddress(s)
	if err != nil || addr.Address != s || strings.ContainsAny(s, " <>") {
		return "", errf(http.StatusBadRequest, "%q is not a valid email address", s)
	}
	return strings.ToLower(s), nil
}

// RedeemRequest is what the claim page sends.
type RedeemRequest struct {
	Token        string `json:"token"`
	Email        string `json:"email"`
	Password     string `json:"password"`
	Organization string `json:"organization_name"`
}

// RedeemResult is the answer to a successful redemption.
type RedeemResult struct {
	Email        string `json:"email"`
	UserID       string `json:"user_id"`
	Organization string `json:"organization,omitempty"` // slug, claim tokens only
	DashboardURL string `json:"dashboard_url"`
}

var errBadToken = errf(http.StatusForbidden, "Invalid, used or expired token")

// Redeem consumes a token and creates its user. A token that cannot create the user
// (the address exists, the password is refused) is released and can be used again.
func (a *Accounts) Redeem(ctx context.Context, in RedeemRequest) (*RedeemResult, error) {
	token := strings.TrimSpace(in.Token)
	if !strings.HasPrefix(token, claimPrefix) && !strings.HasPrefix(token, invitePrefix) || len(token) > 128 {
		return nil, errBadToken
	}
	if n := len(in.Password); n < minPasswordLen || n > maxPasswordLen {
		return nil, errf(http.StatusBadRequest, "password must be %d to %d characters", minPasswordLen, maxPasswordLen)
	}
	hash := secrets.HashToken(token)
	tok, err := a.Store.LookupClaimToken(ctx, hash, a.now())
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, errBadToken
		}
		return nil, err
	}
	email := tok.Email
	if tok.Kind == KindClaim {
		if email, err = normalizeEmail(in.Email); err != nil {
			return nil, err
		}
	} else if in.Email != "" {
		if given, err := normalizeEmail(in.Email); err != nil || given != email {
			return nil, errf(http.StatusBadRequest, "this invite is for %s", email)
		}
	}
	tok, err = a.Store.ConsumeClaimToken(ctx, hash, a.now(), email)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, errBadToken // lost a race to another request
		}
		return nil, err
	}
	userID, err := a.createUser(ctx, email, in.Password)
	if err != nil {
		if rerr := a.Store.ReleaseClaimToken(context.WithoutCancel(ctx), tok.ID); rerr != nil {
			a.log().Error("claim: could not release the token after a failed user creation", "error", rerr)
		}
		return nil, err
	}
	res := &RedeemResult{Email: email, UserID: userID, DashboardURL: a.Config.DashboardURL()}
	if a.Users != nil {
		// Recorded now, so that "first seen" at sign-in only ever means an account from before roles.
		if _, err := a.Users.UpsertUser(ctx, User{UserID: userID, Email: email, Username: strings.SplitN(email, "@", 2)[0]}); err != nil {
			a.log().Error("claim: dashboard user not recorded", "error", err)
		}
	}
	if tok.Kind == KindClaim {
		org, err := a.firstOrg(ctx, in.Organization)
		if err != nil {
			// The user exists and the token is spent; the organization is created on the
			// first project anyway (defaultOrg), so this is not worth undoing the claim.
			a.log().Error("claim: organization not created", "error", err)
		} else {
			res.Organization = org.Slug
			// The claimed first user owns the organization.
			if a.Members != nil {
				if err := a.Members.EnsureOwner(ctx, members.OrgRef{ID: org.ID, Slug: org.Slug}, userID); err != nil {
					a.log().Error("claim: first user not made Owner", "error", err)
				}
			}
		}
	} else if a.Members != nil {
		// The invite token proves the address, so the invitations waiting for it are accepted.
		var live func([]string) []string
		if a.LiveRefs != nil {
			live = func(refs []string) []string { return a.LiveRefs(ctx, refs) }
		}
		if _, err := a.Members.AcceptPending(ctx, userID, email, live); err != nil {
			a.log().Error("claim: pending invitations not accepted", "error", err)
		}
	}
	a.log().Info("dashboard user created", "email", email, "kind", tok.Kind)
	return res, nil
}

// firstOrg returns the first organization, creating one named name (or "Default") when
// the registry has none.
func (a *Accounts) firstOrg(ctx context.Context, name string) (*registry.Organization, error) {
	orgs, err := a.Reg.ListOrganizations(ctx)
	if err != nil {
		return nil, err
	}
	if len(orgs) > 0 {
		return &orgs[0], nil
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = "Default"
	}
	o, err := a.Reg.CreateOrganization(ctx, slugify(name), name)
	if errors.Is(err, registry.ErrConflict) {
		return a.Reg.GetOrganization(ctx, slugify(name))
	}
	return o, err
}

// goTrue calls the admin API of sb-gotrue@system.
func (a *Accounts) goTrue(ctx context.Context, method, path string, body any, out any) (int, error) {
	k, err := a.Keys(ctx, config.SystemRef)
	if err != nil {
		return 0, fmt.Errorf("system project credentials: %w", err)
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.goTrueURL()+path, rd)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+k.ServiceRoleKey)
	req.Header.Set("apikey", k.ServiceRoleKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.client().Do(req)
	if err != nil {
		return 0, fmt.Errorf("the dashboard sign-in service (sb-gotrue@system) is not reachable: %w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 300 {
		var e struct {
			Msg     string `json:"msg"`
			Message string `json:"message"`
			Code    any    `json:"error_code"`
		}
		_ = json.Unmarshal(b, &e)
		msg := e.Msg
		if msg == "" {
			msg = e.Message
		}
		return resp.StatusCode, &goTrueError{Status: resp.StatusCode, Msg: msg, Code: fmt.Sprint(e.Code)}
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			return resp.StatusCode, fmt.Errorf("gotrue %s %s: %w", method, path, err)
		}
	}
	return resp.StatusCode, nil
}

type goTrueError struct {
	Status int
	Msg    string
	Code   string
}

func (e *goTrueError) Error() string { return fmt.Sprintf("gotrue answered %d: %s", e.Status, e.Msg) }

// createUser makes a confirmed dashboard user with the admin claim.
func (a *Accounts) createUser(ctx context.Context, email, password string) (string, error) {
	var u struct {
		ID string `json:"id"`
	}
	_, err := a.goTrue(ctx, http.MethodPost, "/admin/users", map[string]any{
		"email": email, "password": password, "email_confirm": true,
		"app_metadata": map[string]any{AdminClaim: true, "provider": "email", "providers": []string{"email"}},
	}, &u)
	var ge *goTrueError
	if errors.As(err, &ge) {
		switch {
		case ge.Code == "email_exists" || ge.Code == "user_already_exists" || ge.Status == http.StatusConflict:
			return "", errf(http.StatusConflict, "an account for %s already exists", email)
		case ge.Status == http.StatusUnprocessableEntity || ge.Status == http.StatusBadRequest:
			return "", errf(http.StatusBadRequest, "%s", ge.Msg)
		}
	}
	if err != nil {
		return "", err
	}
	return u.ID, nil
}

// DashboardUser is one account of sb-gotrue@system.
type DashboardUser struct {
	ID          string     `json:"id"`
	Email       string     `json:"email"`
	CreatedAt   time.Time  `json:"created_at"`
	LastSignIn  *time.Time `json:"last_sign_in_at,omitempty"`
	Admin       bool       `json:"admin"`
	BannedUntil *time.Time `json:"banned_until,omitempty"`
}

// ListUsers returns the dashboard users.
func (a *Accounts) ListUsers(ctx context.Context) ([]DashboardUser, error) {
	var out []DashboardUser
	for page := 1; page <= 100; page++ {
		var res struct {
			Users []struct {
				ID          string         `json:"id"`
				Email       string         `json:"email"`
				CreatedAt   time.Time      `json:"created_at"`
				LastSignIn  *time.Time     `json:"last_sign_in_at"`
				BannedUntil *time.Time     `json:"banned_until"`
				AppMetadata map[string]any `json:"app_metadata"`
			} `json:"users"`
		}
		if _, err := a.goTrue(ctx, http.MethodGet, fmt.Sprintf("/admin/users?page=%d&per_page=100", page), nil, &res); err != nil {
			return nil, err
		}
		for _, u := range res.Users {
			admin, _ := u.AppMetadata[AdminClaim].(bool)
			out = append(out, DashboardUser{ID: u.ID, Email: u.Email, CreatedAt: u.CreatedAt, LastSignIn: u.LastSignIn, Admin: admin, BannedUntil: u.BannedUntil})
		}
		if len(res.Users) < 100 {
			break
		}
	}
	return out, nil
}

// RemoveUser deletes the dashboard user with this email, the memberships and roles of the
// user, and the personal access tokens the user created, which would otherwise keep working: a
// token is not re-checked against its owner's account. It refuses when the user is the only
// Owner of an organization (members.ErrLastOwner) unless force is set. It returns the number
// of tokens removed.
func (a *Accounts) RemoveUser(ctx context.Context, email string, force bool) (tokens int, err error) {
	email, err = normalizeEmail(email)
	if err != nil {
		return 0, err
	}
	users, err := a.ListUsers(ctx)
	if err != nil {
		return 0, err
	}
	for _, u := range users {
		if strings.EqualFold(u.Email, email) {
			// Memberships first, because that step can refuse (the last Owner). Tokens next: if
			// the GoTrue delete fails afterwards, the account still exists with no access and
			// `users remove` can be run again; the other order would leave live tokens of an
			// account nobody can find.
			if a.Members != nil {
				if err := a.Members.RemoveUser(ctx, u.ID, force); err != nil {
					return 0, err
				}
			}
			ts, err := a.Reg.ListAccessTokens(ctx, u.ID)
			if err != nil {
				return 0, err
			}
			for _, t := range ts {
				if err := a.Reg.DeleteAccessToken(ctx, u.ID, t.ID); err != nil {
					return tokens, err
				}
				tokens++
			}
			if _, err := a.goTrue(ctx, http.MethodDelete, "/admin/users/"+url.PathEscape(u.ID), nil, nil); err != nil {
				return tokens, err
			}
			return tokens, nil
		}
	}
	return 0, fmt.Errorf("no dashboard user %s", email)
}

// ---- HTTP -----------------------------------------------------------------

// claimLimiter bounds failed redemptions per client address: with 192-bit tokens guessing is
// out of reach, so this only keeps the endpoint from being a free oracle for load. It is keyed
// by client so that an anonymous caller who sends junk cannot lock the first administrator or
// an invitee out; a distributed flood is a load problem for the network layer, not something a
// shared counter could absorb without becoming a lockout.
type claimLimiter struct {
	mu      sync.Mutex
	clients map[string]*claimWindow
}

type claimWindow struct {
	start time.Time
	fails int
}

const (
	claimFailLimit  = 10
	claimFailWindow = time.Minute
	claimMaxClients = 4096 // bounds memory; entries older than a window are dropped first, then unknown clients fail open
)

// window returns the live window of key (starting a new one when the old expired). Caller holds mu.
func (l *claimLimiter) window(key string, now time.Time) *claimWindow {
	if l.clients == nil {
		l.clients = map[string]*claimWindow{}
	}
	w := l.clients[key]
	if w != nil && now.Sub(w.start) <= claimFailWindow {
		return w
	}
	if w == nil && len(l.clients) >= claimMaxClients {
		for k, o := range l.clients {
			if now.Sub(o.start) > claimFailWindow {
				delete(l.clients, k)
			}
		}
		if len(l.clients) >= claimMaxClients {
			// Every slot is a live window. Fail open for an unknown client: hand back a window
			// that is not stored, so its failures count for nothing. A shared overflow bucket
			// would let a flood of junk clients lock out the first administrator.
			return &claimWindow{start: now}
		}
	}
	w = &claimWindow{start: now}
	l.clients[key] = w
	return w
}

func (l *claimLimiter) blocked(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.window(key, now).fails >= claimFailLimit
}

func (l *claimLimiter) fail(key string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.window(key, now).fails++
}

// claimClient names the caller for rate limiting. Requests reach the API through sbctl's own
// reverse proxy on loopback, which replaces X-Forwarded-For with the real client address; a
// request that did not come through loopback is keyed by its own address. IPv6 clients are
// grouped by /64, the smallest block a single subscriber controls.
func claimClient(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		if xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); xff != "" {
			// Take the last entry: whatever precedes it was supplied by the caller.
			if i := strings.LastIndex(xff, ","); i >= 0 {
				xff = strings.TrimSpace(xff[i+1:])
			}
			host = xff
		}
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return host
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return ip.Mask(net.CIDRMask(64, 128)).String()
}

// claimRoutes registers GET and POST /claim, served on api.<domain> and the loopback
// admin listener without credentials (the token is the credential).
func (s *Server) claimRoutes(mux *muxSet) {
	lim := &claimLimiter{}
	mux.handle("GET /claim", s.wrap("", authNone, func(w http.ResponseWriter, r *http.Request) error {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", claimCSP)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		return claimPage.Execute(w, nil)
	}))
	mux.handle("POST /claim", s.wrap("", authNone, func(w http.ResponseWriter, r *http.Request) error {
		now := s.now()
		client := claimClient(r)
		if lim.blocked(client, now) {
			w.Header().Set("Retry-After", "60")
			return errf(http.StatusTooManyRequests, "Too many failed attempts; wait a minute")
		}
		var in RedeemRequest
		if err := decode(r, &in); err != nil {
			return err
		}
		res, err := s.accounts.Redeem(r.Context(), in)
		if err != nil {
			if e := asError(err); e.Status == http.StatusForbidden {
				lim.fail(client, now)
			}
			return err
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusCreated, res)
		return nil
	}))
}

// claimScript is the page's only script; its hash goes into the CSP. It has no comments:
// html/template strips them from the page, and the hash must match what is served. A link from
// an invitation carries the token and the address in the fragment, which a browser never sends
// to a server; the script reads them into the form and clears the address bar.
const claimScript = `
const f = document.getElementById('f'), out = document.getElementById('out'), btn = document.getElementById('go');
const hp = new URLSearchParams(location.hash.slice(1));
if (hp.get('token')) f.elements['token'].value = hp.get('token');
if (hp.get('email')) f.elements['email'].value = hp.get('email');
if (location.hash) history.replaceState(null, '', location.pathname);
f.addEventListener('submit', async (e) => {
  e.preventDefault();
  out.className = ''; out.textContent = '';
  const v = (n) => f.elements[n].value;
  if (v('password') !== v('again')) { out.className = 'err'; out.textContent = 'The passwords do not match.'; return; }
  btn.disabled = true;
  try {
    const r = await fetch('/claim', { method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ token: v('token').trim(), email: v('email').trim(), password: v('password'), organization_name: v('org').trim() }) });
    const j = await r.json().catch(() => ({}));
    if (r.ok) {
      f.hidden = true;
      out.className = 'ok';
      out.textContent = 'Account created for ' + j.email + '. ';
      const a = document.createElement('a'); a.href = j.dashboard_url; a.textContent = 'Open the dashboard'; out.appendChild(a);
    } else { out.className = 'err'; out.textContent = j.message || ('Request failed (' + r.status + ')'); }
  } catch (err) { out.className = 'err'; out.textContent = 'Could not reach the server.'; }
  btn.disabled = false;
});
`

var claimCSP = func() string {
	h := sha256.Sum256([]byte(claimScript))
	return "default-src 'none'; style-src 'unsafe-inline'; script-src 'sha256-" + base64.StdEncoding.EncodeToString(h[:]) +
		"'; connect-src 'self'; form-action 'none'; base-uri 'none'; frame-ancestors 'none'"
}()

var claimPage = template.Must(template.New("claim").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Create your account</title>
<style>
:root{color-scheme:light dark;--bg:#fff;--fg:#1a1a1a;--mut:#666;--line:#d4d4d4;--acc:#1f7a4d;--err:#b3261e}
@media (prefers-color-scheme:dark){:root{--bg:#161616;--fg:#ececec;--mut:#9a9a9a;--line:#3a3a3a;--acc:#4cc38a;--err:#ff8a80}}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--fg);font:16px/1.5 system-ui,sans-serif;display:grid;place-items:start center;padding:48px 16px}
main{width:100%;max-width:26rem}
h1{font-size:1.4rem;margin:0 0 .25rem}
p{margin:.25rem 0 1.25rem;color:var(--mut)}
label{display:block;margin:.9rem 0 .25rem;font-size:.9rem}
input{width:100%;padding:.55rem .65rem;border:1px solid var(--line);border-radius:6px;background:transparent;color:inherit;font:inherit}
input:focus-visible,button:focus-visible{outline:2px solid var(--acc);outline-offset:2px}
small{color:var(--mut);display:block;margin-top:.2rem}
button{margin-top:1.4rem;width:100%;padding:.65rem;border:0;border-radius:6px;background:var(--acc);color:#fff;font:inherit;font-weight:600;cursor:pointer}
@media (prefers-color-scheme:dark){button{color:#0b1f15}}
button:disabled{opacity:.6;cursor:wait}
#out{margin-top:1rem}.err{color:var(--err)}.ok a{color:var(--acc)}
</style></head><body><main>
<h1>Create your account</h1>
<p>Use the claim token the installer printed or saved, or the invite token an administrator gave you.</p>
<form id="f" autocomplete="off">
<label for="token">Token</label><input id="token" name="token" required spellcheck="false" autocapitalize="off">
<label for="email">Email</label><input id="email" name="email" type="email" autocomplete="username"><small>Required for the first administrator. An invite already names its address.</small>
<label for="password">Password</label><input id="password" name="password" type="password" minlength="12" maxlength="72" required autocomplete="new-password"><small>12 to 72 characters.</small>
<label for="again">Repeat password</label><input id="again" name="again" type="password" required autocomplete="new-password">
<label for="org">Organization name</label><input id="org" name="org" placeholder="Default"><small>First administrator only; optional.</small>
<button id="go" type="submit">Create account</button>
</form>
<div id="out" role="status"></div>
<noscript><p>This page needs JavaScript to send the form.</p></noscript>
<script>` + claimScript + `</script>
</main></body></html>
`))
