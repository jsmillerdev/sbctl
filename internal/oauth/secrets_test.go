package oauth

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/secrets"
)

// The tokens of this package repeat the prefixes and lengths of internal/secrets as constants; keep
// the two in step.
func TestTokenConstantsMatchSecrets(t *testing.T) {
	for _, c := range []struct {
		name      string
		got, want any
	}{
		{"access prefix", AccessTokenPrefix, secrets.PrefixOAuthAccess},
		{"access length", AccessTokenHexLen, secrets.OAuthAccessHexLen},
		{"refresh prefix", RefreshTokenPrefix, secrets.PrefixOAuthRefresh},
		{"refresh length", RefreshTokenHexLen, secrets.OAuthRefreshHexLen},
		{"code prefix", AuthCodePrefix, secrets.PrefixAuthCode},
		{"code length", AuthCodeHexLen, secrets.AuthCodeHexLen},
		{"client secret prefix", ClientSecretPrefix, secrets.PrefixClientSecret},
		{"client secret length", ClientSecretHexLen, secrets.ClientSecretHexLen},
		{"stored prefix length", StoredPrefixLen, secrets.OAuthStoredPrefixLen},
	} {
		if c.got != c.want {
			t.Errorf("%s: oauth has %v, secrets has %v", c.name, c.got, c.want)
		}
	}
}

// walk runs every flow that handles a secret and returns the secrets it created, by kind: client secrets,
// authorization codes, access and refresh tokens, PKCE verifiers.
type walked struct {
	clientSecrets, codes, accessTokens, refreshTokens []string
	verifier, challenge, state                        string
}

func (w walked) all() []string {
	var out []string
	for _, l := range [][]string{w.clientSecrets, w.codes, w.accessTokens, w.refreshTokens, {w.verifier}} {
		out = append(out, l...)
	}
	return out
}

func walkAll(t *testing.T, fx *svcFixture) walked {
	t.Helper()
	w := walked{verifier: testVerifier, challenge: pkceChallengeS256(testVerifier), state: "state-" + strings.Repeat("x", 20) + "-marker"}

	// A dynamic app: register, authorize, approve, exchange, refresh, reuse (kills the grant), replay of a code.
	ra := fx.register()
	w.clientSecrets = append(w.clientSecrets, ra.ClientSecret)
	req := authorizeReq(&ra.App)
	req.State = w.state
	authID := fx.start(req)
	_, code := fx.approve(authID, testUserA, testOrgAcme)
	w.codes = append(w.codes, code)
	tr, err := fx.svc.Exchange(fx.ctx(), codeRequest(ra, req, code))
	mustNoErr(t, err)
	w.accessTokens, w.refreshTokens = append(w.accessTokens, tr.AccessToken), append(w.refreshTokens, tr.RefreshToken)
	f := &grantFlow{App: ra, Tokens: tr}
	t1, err := fx.svc.Exchange(fx.ctx(), f.refreshRequest(tr.RefreshToken))
	mustNoErr(t, err)
	w.accessTokens, w.refreshTokens = append(w.accessTokens, t1.AccessToken), append(w.refreshTokens, t1.RefreshToken)
	fx.clock.Advance(time.Minute)
	_, _ = fx.svc.Exchange(fx.ctx(), f.refreshRequest(tr.RefreshToken)) // reuse: alert, revoke
	_, _ = fx.svc.Exchange(fx.ctx(), codeRequest(ra, req, code))        // replay: still dead

	// A second flow with a code replay that revokes a live grant.
	req2 := authorizeReq(&ra.App)
	req2.State = w.state
	_, code2 := fx.approve(fx.start(req2), testUserB, testOrgOther)
	w.codes = append(w.codes, code2)
	tr2, err := fx.svc.Exchange(fx.ctx(), codeRequest(ra, req2, code2))
	mustNoErr(t, err)
	w.accessTokens, w.refreshTokens = append(w.accessTokens, tr2.AccessToken), append(w.refreshTokens, tr2.RefreshToken)
	_, _ = fx.svc.Exchange(fx.ctx(), codeRequest(ra, req2, code2)) // replay: alert, revoke

	// A wrong verifier burns a code; a declined request; a client revocation.
	req3 := authorizeReq(&ra.App)
	req3.State = w.state
	_, code3 := fx.approve(fx.start(req3), testUserA, testOrgAcme)
	w.codes = append(w.codes, code3)
	bad := codeRequest(ra, req3, code3)
	bad.CodeVerifier = strings.Repeat("Z", 43)
	_, _ = fx.svc.Exchange(fx.ctx(), bad)
	mustNoErr(t, fx.svc.Decline(fx.ctx(), DeclineRequest{AuthID: fx.start(req3), UserID: testUserA}))
	g := fx.grantFor(ra, testUserA, testOrgAcme)
	w.accessTokens, w.refreshTokens = append(w.accessTokens, g.Tokens.AccessToken), append(w.refreshTokens, g.Tokens.RefreshToken)
	w.codes = append(w.codes, g.Code)
	mustNoErr(t, fx.svc.Revoke(fx.ctx(), RevokeRequest{ClientID: ra.App.ID, ClientSecret: ra.ClientSecret, Token: g.Tokens.RefreshToken}))

	// A manual app with its secrets, a code and tokens, then an update, a revocation and a deletion.
	ca, err := fx.svc.CreateApp(fx.ctx(), createReq(testOrgAcme))
	mustNoErr(t, err)
	w.clientSecrets = append(w.clientSecrets, ca.ClientSecret)
	cs, err := fx.svc.CreateClientSecret(fx.ctx(), CreateSecretRequest{OrgID: testOrgAcme, AppID: ca.App.ID, CreatedBy: testUserA})
	mustNoErr(t, err)
	w.clientSecrets = append(w.clientSecrets, cs.ClientSecret)
	mreq := AuthorizeRequest{ClientID: ca.App.ID, ResponseType: ResponseTypeCode, RedirectURI: ca.App.RedirectURIs[0], State: w.state}
	_, mcode := fx.approve(fx.start(mreq), testUserA, testOrgAcme)
	w.codes = append(w.codes, mcode)
	mtr, err := fx.svc.Exchange(fx.ctx(), TokenRequest{GrantType: GrantTypeAuthorizationCode, ClientID: ca.App.ID, ClientSecret: cs.ClientSecret, Code: mcode, RedirectURI: mreq.RedirectURI})
	mustNoErr(t, err)
	w.accessTokens, w.refreshTokens = append(w.accessTokens, mtr.AccessToken), append(w.refreshTokens, mtr.RefreshToken)
	_, err = fx.svc.UpdateApp(fx.ctx(), UpdateAppRequest{OrgID: testOrgAcme, AppID: ca.App.ID, Actor: testUserA, Name: "Renamed", Scopes: []string{ScopeProjectsRead}, RedirectURIs: ca.App.RedirectURIs})
	mustNoErr(t, err)
	mustNoErr(t, fx.svc.DeleteClientSecret(fx.ctx(), DeleteSecretRequest{OrgID: testOrgAcme, AppID: ca.App.ID, SecretID: ca.Secret.ID, Actor: testUserA}))
	_, err = fx.svc.RevokeApp(fx.ctx(), RevokeAppRequest{AppID: ca.App.ID, OrgID: testOrgAcme, Reason: ReasonAdmin, Actor: testUserA})
	mustNoErr(t, err)
	_, err = fx.svc.DeleteApp(fx.ctx(), DeleteAppRequest{OrgID: testOrgAcme, AppID: ca.App.ID, Actor: testUserA})
	mustNoErr(t, err)
	_, err = fx.svc.RevokeUser(fx.ctx(), testUserB, ReasonUserRemoved, ActorSystem)
	mustNoErr(t, err)
	return w
}

// TestNoPlaintextAtRest is R3: after full flows, no table holds a client secret, a code, a token or a
// verifier; each is found only through its SHA-256.
func TestNoPlaintextAtRest(t *testing.T) {
	fx := newSvc(t)
	w := walkAll(t, fx)

	dump := fx.store.dumpForTest(t)
	for _, secret := range w.all() {
		if strings.Contains(dump, secret) {
			t.Errorf("the store holds %.14s... in plaintext", secret)
		}
		// Not even as a part: the part after the prefix.
		if _, rest, ok := strings.Cut(secret, "_"); ok && len(rest) >= 32 && strings.Contains(dump, rest[len(rest)-32:]) {
			t.Errorf("the store holds the tail of %.14s...", secret)
		}
	}
	// The hashes are there: a token is found by its hash, and the stored prefix is the constant one.
	for _, tok := range append(append([]string(nil), w.accessTokens...), w.refreshTokens...) {
		row, err := fx.store.GetToken(fx.ctx(), secrets.HashToken(tok))
		if err != nil {
			t.Errorf("token %.12s not found by its hash: %v", tok, err)
			continue
		}
		if row.Prefix != tok[:StoredPrefixLen] || len(row.Hash) != 32 {
			t.Errorf("token row: prefix %q hash length %d", row.Prefix, len(row.Hash))
		}
	}
	for _, code := range w.codes {
		found := false
		for _, a := range fx.store.authsForTest() {
			if string(a.CodeHash) == string(secrets.HashToken(code)) {
				found = true
			}
		}
		if !found {
			t.Errorf("code %.12s... has no row by its hash", code)
		}
	}
	// Client secret aliases hide all but the prefix.
	for _, s := range fx.store.secretsForTest() {
		if len(s.Alias) != 16 || !strings.HasSuffix(s.Alias, "********") || len(s.Hash) != 32 {
			t.Errorf("secret row: alias %q hash length %d", s.Alias, len(s.Hash))
		}
	}
	// The challenge is stored (a hash by construction); the verifier never.
	if strings.Contains(dump, w.verifier) {
		t.Error("the verifier is stored")
	}
}

// TestAuditPayloadsHaveNoSecrets is T4 at the service: audit events, alerts and the log never carry
// a token, a code, a client secret, a verifier, a challenge or the state of an authorization request.
func TestAuditPayloadsHaveNoSecrets(t *testing.T) {
	fx := newSvc(t)
	w := walkAll(t, fx)

	var text strings.Builder
	fx.mu.Lock()
	for _, e := range fx.events {
		b, err := json.Marshal(e.Payload)
		if err != nil {
			t.Fatalf("event %s is not JSON: %v", e.Kind, err)
		}
		fmt.Fprintf(&text, "%s %s\n", e.Kind, b)
	}
	for _, a := range fx.alerts {
		fmt.Fprintf(&text, "%+v\n", a)
	}
	fx.mu.Unlock()
	text.WriteString(fx.logbuf.String())
	out := text.String()
	if len(fx.eventsOf(EventGrantCreated)) == 0 || len(fx.eventsOf(EventAppCreated)) == 0 || len(fx.alertList()) == 0 {
		t.Fatalf("the walk did not produce the events it should: %d events, %d alerts", len(fx.events), len(fx.alertList()))
	}
	for _, secret := range append(w.all(), w.challenge, w.state) {
		if secret != "" && strings.Contains(out, secret) {
			t.Errorf("audit trail, alerts or log contain %.14s...", secret)
		}
	}
	// Nor the hashes, which would identify a secret to anyone who can read the trail.
	for _, secret := range w.all() {
		if strings.Contains(out, fmt.Sprintf("%x", secrets.HashToken(secret))) {
			t.Errorf("a hash of %.12s... is in the trail", secret)
		}
	}
	// Every kind of event W0 names was exercised, so the check above means something.
	for _, kind := range []string{EventAuthorizationApproved, EventAuthorizationDeclined, EventGrantCreated, EventGrantRevoked, EventRefreshReuse,
		EventCodeReuse, EventAppCreated, EventAppUpdated, EventAppDeleted, EventClientSecretCreated, EventClientSecretDeleted} {
		if len(fx.eventsOf(kind)) == 0 {
			t.Errorf("no %s event in the walk", kind)
		}
	}
}

// TestAuditEvents is O2 at the service: each state change writes its event.
func TestAuditEvents(t *testing.T) {
	fx := newSvc(t)
	walkAll(t, fx)
	counts := map[string]int{}
	fx.mu.Lock()
	for _, e := range fx.events {
		counts[e.Kind]++
		// Every payload names what it is about by id.
		if _, ok := e.Payload["app_id"]; !ok && !strings.HasPrefix(e.Kind, "oauth.client_secret") {
			t.Errorf("%s has no app_id: %v", e.Kind, e.Payload)
		}
		for k, v := range e.Payload {
			if s, ok := v.(string); ok && strings.ContainsAny(s, "\x00\n") {
				t.Errorf("%s.%s has control characters", e.Kind, k)
			}
		}
	}
	fx.mu.Unlock()
	for kind, min := range map[string]int{
		EventAuthorizationApproved: 4, EventAuthorizationDeclined: 1, EventGrantCreated: 4, EventGrantRevoked: 4,
		EventRefreshReuse: 1, EventCodeReuse: 1, EventAppCreated: 1, EventAppUpdated: 1, EventAppDeleted: 1,
		EventClientSecretCreated: 2, EventClientSecretDeleted: 1,
	} {
		if counts[kind] < min {
			t.Errorf("%s: %d events, want at least %d", kind, counts[kind], min)
		}
	}
	// A Service without Audit and Alert callbacks still works.
	quiet := newSvc(t)
	quiet.svc.Audit, quiet.svc.Alert, quiet.svc.Log = nil, nil, nil
	walkAll(t, quiet)
}
