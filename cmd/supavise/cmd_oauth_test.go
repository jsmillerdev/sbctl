package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/api"
	"github.com/supavise/supavise/internal/oauth"
	"github.com/supavise/supavise/internal/registry"
)

// fakeGrants is the part of the OAuth service the commands use, over a list of grants. It selects the
// way the Store does: every set field of the filter must match, Live leaves out the revoked, and an
// empty filter revokes nothing unless All is set.
type fakeGrants struct {
	oauth.Authority
	grants  []oauth.GrantInfo
	revoked []revocation
	listErr error
}

type revocation struct {
	id            int64
	reason, actor string
}

func (f *fakeGrants) match(g oauth.GrantInfo, flt oauth.GrantFilter) bool {
	return (flt.ID == 0 || g.Grant.ID == flt.ID) && (flt.AppID == "" || g.Grant.AppID == flt.AppID) &&
		(flt.UserID == "" || g.Grant.UserID == flt.UserID) && (flt.OrgID == 0 || g.Grant.OrgID == flt.OrgID) &&
		(!flt.Live || g.Grant.RevokedAt == nil)
}

func (f *fakeGrants) ListGrants(_ context.Context, flt oauth.GrantFilter) ([]oauth.GrantInfo, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []oauth.GrantInfo
	for _, g := range f.grants {
		if f.match(g, flt) {
			out = append(out, g)
		}
	}
	return out, nil
}

func (f *fakeGrants) RevokeGrants(_ context.Context, flt oauth.GrantFilter, reason, actor string) (int, error) {
	if flt.IsEmpty() && !flt.All {
		return 0, oauth.ErrInvalid
	}
	n := 0
	now := time.Now()
	for i := range f.grants {
		g := &f.grants[i]
		if g.Grant.RevokedAt == nil && f.match(*g, flt) {
			g.Grant.RevokedAt, g.Grant.RevokedReason = &now, reason
			f.revoked = append(f.revoked, revocation{g.Grant.ID, reason, actor})
			n++
		}
	}
	return n, nil
}

func (f *fakeGrants) revokedIDs() []int64 {
	var ids []int64
	for _, r := range f.revoked {
		ids = append(ids, r.id)
	}
	slices.Sort(ids)
	return ids
}

const (
	oaApp1  = "11111111-aaaa-4aaa-8aaa-000000000001"
	oaApp2  = "11111111-aaaa-4aaa-8aaa-000000000002"
	oaAlice = "22222222-bbbb-4bbb-8bbb-000000000001"
	oaAlis2 = "22222222-bbbb-4bbb-8bbb-000000000002" // a single sign-on account with Alice's address
	oaBob   = "22222222-bbbb-4bbb-8bbb-000000000003"
	oaGone  = "22222222-bbbb-4bbb-8bbb-0000000000ff" // an account that GoTrue no longer has
)

var oaT0 = time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)

func oaGrant(id int64, app, appName, user string, org int64, orgSlug string, scopes []string, age time.Duration) oauth.GrantInfo {
	return oauth.GrantInfo{
		Grant:   oauth.Grant{ID: id, AppID: app, UserID: user, OrgID: org, Scopes: scopes, CreatedAt: oaT0.Add(-age)},
		App:     oauth.App{ID: app, Name: appName, RegistrationType: oauth.RegistrationDynamic},
		OrgSlug: orgSlug,
	}
}

// oauthTestEnv is a node with two organizations, two apps and these grants (newest first):
//
//	5  app2 Cursor       gone    acme   (account deleted)       2 minutes old
//	4  app1 Claude Code  bob     acme                            1 hour
//	3  app1 Claude Code  alice   labs   all 13 scopes            1 day
//	2  app2 Cursor       alice2  acme   (single sign-on)         2 days
//	1  app1 Claude Code  alice   acme   projects:read + more     3 days, used once
//
// and grant 6, revoked, which no listing shows.
func oauthTestEnv(t *testing.T) (*oauthEnv, *fakeGrants, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	thirteen := make([]string, 13)
	for i := range thirteen {
		thirteen[i] = fmt.Sprintf("scope%02d:read", i)
	}
	g1 := oaGrant(1, oaApp1, "Claude Code", oaAlice, 1, "acme", []string{"projects:read", "database:read"}, 72*time.Hour)
	used := oaT0.Add(-time.Hour)
	g1.Grant.LastUsedAt = &used
	revoked := oaGrant(6, oaApp1, "Claude Code", oaAlice, 1, "acme", []string{"projects:read"}, 96*time.Hour)
	at := oaT0
	revoked.Grant.RevokedAt, revoked.Grant.RevokedReason = &at, oauth.ReasonUser
	svc := &fakeGrants{grants: []oauth.GrantInfo{
		g1,
		oaGrant(2, oaApp2, "Cursor", oaAlis2, 1, "acme", []string{"projects:read"}, 48*time.Hour),
		oaGrant(3, oaApp1, "Claude Code", oaAlice, 2, "labs", thirteen, 24*time.Hour),
		oaGrant(4, oaApp1, "Claude Code", oaBob, 1, "acme", []string{"projects:read"}, time.Hour),
		oaGrant(5, oaApp2, "Cursor", oaGone, 1, "acme", []string{"projects:read"}, 2*time.Minute),
		revoked,
	}}
	var out, errw bytes.Buffer
	e := &oauthEnv{
		svc: svc,
		org: func(_ context.Context, slug string) (int64, error) {
			switch slug {
			case "acme":
				return 1, nil
			case "labs":
				return 2, nil
			}
			return 0, registry.ErrNotFound
		},
		users: func(context.Context) ([]api.DashboardUser, error) {
			return []api.DashboardUser{
				{ID: oaAlice, Email: "alice@acme.test"},
				{ID: oaAlis2, Email: "Alice@Acme.Test", SSOProvider: "idp-1"},
				{ID: oaBob, Email: "bob@acme.test"},
			}, nil
		},
		in: strings.NewReader(""), out: &out, errw: &errw,
	}
	return e, svc, &out, &errw
}

func TestOAuthGrantsListTable(t *testing.T) {
	e, _, out, errw := oauthTestEnv(t)
	if err := runOAuthGrantsList(context.Background(), e, grantSelector{}, false); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 6 { // the header and five live grants; the revoked one is not listed
		t.Fatalf("%d lines:\n%s", len(lines), out)
	}
	if got := strings.Fields(lines[0]); strings.Join(got, " ") != "ID APP APP ID USER ORG SCOPES CREATED LAST USED" {
		t.Errorf("header %q", lines[0])
	}
	var ids []string
	for _, l := range lines[1:] {
		ids = append(ids, strings.Fields(l)[0])
	}
	if strings.Join(ids, " ") != "5 4 3 2 1" {
		t.Errorf("order %v, want newest first", ids)
	}
	for _, want := range []struct{ line, contains string }{
		{lines[1], oaGone},                        // an account that is gone is shown by its id
		{lines[2], "bob@acme.test"},               // the address, not the id
		{lines[3], "13 scopes"},                   // many scopes are counted
		{lines[3], "labs"},                        // the organization
		{lines[4], "Alice@Acme.Test"},             // the single sign-on account, as GoTrue spells it
		{lines[5], "database:read,projects:read"}, // sorted
		{lines[5], oaApp1},
	} {
		if !strings.Contains(want.line, want.contains) {
			t.Errorf("%q lacks %q", want.line, want.contains)
		}
	}
	if !strings.HasSuffix(strings.TrimRight(lines[1], " "), "-") {
		t.Errorf("a grant that was never used ends with -: %q", lines[1])
	}
	if errw.Len() != 0 {
		t.Errorf("stderr: %s", errw)
	}
}

func TestOAuthGrantsListJSONAndEmpty(t *testing.T) {
	e, _, out, _ := oauthTestEnv(t)
	if err := runOAuthGrantsList(context.Background(), e, grantSelector{User: oaAlice}, true); err != nil {
		t.Fatal(err)
	}
	var vs []grantView
	if err := json.Unmarshal(out.Bytes(), &vs); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if len(vs) != 2 || vs[0].ID != 3 || vs[1].ID != 1 {
		t.Fatalf("grants %+v", vs)
	}
	v := vs[1]
	if v.App != "Claude Code" || v.AppID != oaApp1 || v.UserID != oaAlice || v.User != "alice@acme.test" || v.Org != "acme" ||
		v.RegistrationType != oauth.RegistrationDynamic || !slices.Equal(v.Scopes, []string{"database:read", "projects:read"}) ||
		v.LastUsedAt == nil || !v.CreatedAt.Equal(oaT0.Add(-72*time.Hour)) {
		t.Errorf("view %+v", v)
	}

	// Nothing matches: a sentence for people, [] for programs.
	out.Reset()
	if err := runOAuthGrantsList(context.Background(), e, grantSelector{Org: "labs", App: oaApp2}, false); err != nil {
		t.Fatal(err)
	}
	if out.String() != "no live OAuth grants\n" {
		t.Errorf("empty table: %q", out)
	}
	out.Reset()
	if err := runOAuthGrantsList(context.Background(), e, grantSelector{Org: "labs", App: oaApp2}, true); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != "[]" {
		t.Errorf("empty JSON: %q", out)
	}
}

// What a client or an account chose reaches the operator's terminal as plain text.
func TestOAuthGrantsListCleansNames(t *testing.T) {
	e, svc, out, _ := oauthTestEnv(t)
	svc.grants[0].App.Name = "Evil\x1b[2J\x07 app"
	svc.grants[0].OrgSlug = "ac\x00me"
	if err := runOAuthGrantsList(context.Background(), e, grantSelector{}, false); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(out.String(), "\x1b\x07\x00") || !strings.Contains(out.String(), "Evil?[2J? app") {
		t.Errorf("control characters reached the terminal:\n%q", out)
	}
}

func TestOAuthGrantsListFilters(t *testing.T) {
	for _, tc := range []struct {
		name string
		sel  grantSelector
		want []int64 // grant ids, newest first
	}{
		{"nothing", grantSelector{}, []int64{5, 4, 3, 2, 1}},
		{"org", grantSelector{Org: "acme"}, []int64{5, 4, 2, 1}},
		{"app", grantSelector{App: oaApp2}, []int64{5, 2}},
		{"an app id in capitals", grantSelector{App: strings.ToUpper(oaApp1)}, []int64{4, 3, 1}},
		{"user by address covers every account with it", grantSelector{User: "ALICE@acme.test"}, []int64{3, 2, 1}},
		{"user by id", grantSelector{User: oaBob}, []int64{4}},
		{"user of an account that is gone, by id", grantSelector{User: oaGone}, []int64{5}},
		{"user and org", grantSelector{User: "alice@acme.test", Org: "acme"}, []int64{2, 1}},
		{"user, org and app", grantSelector{User: "alice@acme.test", Org: "acme", App: oaApp1}, []int64{1}},
		{"a grant id", grantSelector{ID: 3}, []int64{3}},
		{"a revoked grant is not listed", grantSelector{ID: 6}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, _, out, _ := oauthTestEnv(t)
			if err := runOAuthGrantsList(context.Background(), e, tc.sel, true); err != nil {
				t.Fatal(err)
			}
			var vs []grantView
			if err := json.Unmarshal(out.Bytes(), &vs); err != nil {
				t.Fatalf("not JSON: %v\n%s", err, out)
			}
			var got []int64
			for _, v := range vs {
				got = append(got, v.ID)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("grants %v, want %v", got, tc.want)
			}
		})
	}
}

func TestOAuthGrantsListErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		sel  grantSelector
		want string
	}{
		{"unknown organization", grantSelector{Org: "nowhere"}, `no organization "nowhere"`},
		{"unknown address", grantSelector{User: "carol@acme.test"}, "no dashboard user carol@acme.test"},
		{"app that is not an id", grantSelector{App: "claude-code"}, "--app wants an app's id"},
		{"an id and a filter", grantSelector{ID: 3, Org: "acme"}, "on its own"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, _, out, _ := oauthTestEnv(t)
			err := runOAuthGrantsList(context.Background(), e, tc.sel, false)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v, want one with %q", err, tc.want)
			}
			if out.Len() != 0 {
				t.Errorf("printed before failing: %s", out)
			}
		})
	}
}

// GoTrue being down costs the listing its addresses and the --user lookup, not more.
func TestOAuthGrantsListWithoutTheSignInService(t *testing.T) {
	e, _, out, errw := oauthTestEnv(t)
	e.users = func(context.Context) ([]api.DashboardUser, error) { return nil, errors.New("connection refused") }

	if err := runOAuthGrantsList(context.Background(), e, grantSelector{User: "alice@acme.test"}, false); err == nil ||
		!strings.Contains(err.Error(), "connection refused") || !strings.Contains(err.Error(), "system project") {
		t.Fatalf("--user with an address: %v", err)
	}
	// By id, the lookup is not needed; the addresses are, and the id stands in.
	if err := runOAuthGrantsList(context.Background(), e, grantSelector{User: oaBob}, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), oaBob) || strings.Contains(out.String(), "bob@acme.test") {
		t.Errorf("table:\n%s", out)
	}
	if !strings.Contains(errw.String(), "shown by id") {
		t.Errorf("no note on stderr: %q", errw)
	}
}

func TestOAuthGrantsListServiceError(t *testing.T) {
	e, svc, _, _ := oauthTestEnv(t)
	svc.listErr = errors.New("the registry is down")
	if err := runOAuthGrantsList(context.Background(), e, grantSelector{}, false); err == nil || !strings.Contains(err.Error(), "registry is down") {
		t.Fatalf("error %v", err)
	}
}

func TestOAuthGrantsRevoke(t *testing.T) {
	for _, tc := range []struct {
		name string
		sel  grantSelector
		want []int64 // revoked grant ids
	}{
		{"one grant", grantSelector{ID: 4}, []int64{4}},
		{"a person, whatever account they signed in with", grantSelector{User: "alice@acme.test"}, []int64{1, 2, 3}},
		{"a person by id", grantSelector{User: oaGone}, []int64{5}},
		{"an organization", grantSelector{Org: "acme"}, []int64{1, 2, 4, 5}},
		{"an app everywhere", grantSelector{App: oaApp2}, []int64{2, 5}},
		{"a person in one organization", grantSelector{User: "alice@acme.test", Org: "labs"}, []int64{3}},
		{"everything", grantSelector{All: true}, []int64{1, 2, 3, 4, 5}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, svc, out, errw := oauthTestEnv(t)
			if err := runOAuthGrantsRevoke(context.Background(), e, tc.sel, true); err != nil {
				t.Fatal(err)
			}
			if got := svc.revokedIDs(); !slices.Equal(got, tc.want) {
				t.Errorf("revoked %v, want %v", got, tc.want)
			}
			for _, r := range svc.revoked {
				if r.reason != oauth.ReasonOperator || r.actor != oauth.ActorOperator {
					t.Errorf("grant %d revoked as %q by %q, want the operator", r.id, r.reason, r.actor)
				}
			}
			if want := fmt.Sprintf("revoked %d grant(s)\n", len(tc.want)); out.String() != want {
				t.Errorf("stdout %q, want %q", out, want)
			}
			if errw.Len() != 0 {
				t.Errorf("--yes asked or explained: %q", errw)
			}
			// The grant that was revoked before the command stays as it was.
			if svc.grants[5].Grant.RevokedReason != oauth.ReasonUser {
				t.Errorf("the revoked grant was touched: %+v", svc.grants[5].Grant)
			}
		})
	}
}

func TestOAuthGrantsRevokeAsksFirst(t *testing.T) {
	sel := grantSelector{Org: "acme"}
	for _, tc := range []struct {
		name    string
		input   string
		revoked []int64
		wantErr string
	}{
		{"yes", "y\n", []int64{1, 2, 4, 5}, ""},
		{"yes spelled out", "  Yes \n", []int64{1, 2, 4, 5}, ""},
		{"no", "n\n", nil, "not revoked"},
		{"nothing typed", "\n", nil, "not revoked"},
		{"no terminal", "", nil, "not revoked"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, svc, out, errw := oauthTestEnv(t)
			e.in = strings.NewReader(tc.input)
			err := runOAuthGrantsRevoke(context.Background(), e, sel, false)
			if tc.wantErr == "" && err != nil || tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("error %v, want %q", err, tc.wantErr)
			}
			if got := svc.revokedIDs(); !slices.Equal(got, tc.revoked) {
				t.Errorf("revoked %v, want %v", got, tc.revoked)
			}
			// The question comes with the grants it is about, on stderr; stdout carries the result only.
			for _, want := range []string{"4 live OAuth grant(s) would be revoked", "Cursor", "Claude Code", "Revoke them? [y/N]"} {
				if !strings.Contains(errw.String(), want) {
					t.Errorf("stderr lacks %q:\n%s", want, errw)
				}
			}
			if tc.wantErr == "" && out.String() != "revoked 4 grant(s)\n" || tc.wantErr != "" && out.Len() != 0 {
				t.Errorf("stdout %q", out)
			}
		})
	}
}

func TestOAuthGrantsRevokeNothingToDo(t *testing.T) {
	// A selector that matches no live grant is not an error and asks nothing.
	e, svc, out, errw := oauthTestEnv(t)
	if err := runOAuthGrantsRevoke(context.Background(), e, grantSelector{Org: "labs", App: oaApp2}, false); err != nil {
		t.Fatal(err)
	}
	if out.String() != "no live OAuth grants match; nothing revoked\n" || errw.Len() != 0 || len(svc.revoked) != 0 {
		t.Errorf("stdout %q stderr %q revoked %v", out, errw, svc.revoked)
	}
	// A grant id that names nothing live (never made, or revoked already) is: the operator typed it.
	for _, yes := range []bool{false, true} {
		for _, id := range []int64{6, 99} {
			e, _, _, _ := oauthTestEnv(t)
			err := runOAuthGrantsRevoke(context.Background(), e, grantSelector{ID: id}, yes)
			if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("no live grant %d", id)) {
				t.Errorf("grant %d (yes=%v): %v", id, yes, err)
			}
		}
	}
}

// The command refuses to guess what to revoke, and says so before it touches anything.
func TestOAuthGrantsSelectorValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		sel    grantSelector
		revoke bool
		want   string // "" is valid
	}{
		{"revoke needs a selector", grantSelector{}, true, "name what to revoke"},
		{"list needs none", grantSelector{}, false, ""},
		{"an id alone", grantSelector{ID: 3}, true, ""},
		{"an id with a user", grantSelector{ID: 3, User: "a@b.test"}, true, "on its own"},
		{"an id with --all", grantSelector{ID: 3, All: true}, true, "on its own"},
		{"--all alone", grantSelector{All: true}, true, ""},
		{"--all with --org", grantSelector{All: true, Org: "acme"}, true, "--all cannot be combined"},
		{"filters together", grantSelector{User: "a@b.test", Org: "acme", App: oaApp1}, true, ""},
		{"an app that is not a uuid", grantSelector{App: "x"}, true, "--app wants"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.sel.validate(tc.revoke)
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("validate = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestParseGrantID(t *testing.T) {
	for in, want := range map[string]int64{"1": 1, " 42 ": 42, "9223372036854775807": 9223372036854775807} {
		if got, err := parseGrantID(in); err != nil || got != want {
			t.Errorf("parseGrantID(%q) = %d, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "0", "-3", "abc", "1.5", "99999999999999999999", oaApp1} {
		if _, err := parseGrantID(in); err == nil {
			t.Errorf("parseGrantID(%q) succeeded", in)
		}
	}
}

func TestOAuthCommandsAreRegistered(t *testing.T) {
	for _, path := range [][]string{{"oauth", "grants", "list"}, {"oauth", "grants", "revoke"}} {
		c, _, err := rootCmd.Find(path)
		if err != nil || c == nil || c.Name() != path[len(path)-1] {
			t.Errorf("supavise %s is not registered: %v", strings.Join(path, " "), err)
		}
	}
	list, _, _ := rootCmd.Find([]string{"oauth", "grants", "list"})
	for _, flag := range []string{"user", "org", "app", "json"} {
		if list.Flags().Lookup(flag) == nil {
			t.Errorf("grants list has no --%s", flag)
		}
	}
	revoke, _, _ := rootCmd.Find([]string{"oauth", "grants", "revoke"})
	for _, flag := range []string{"user", "org", "app", "all", "yes"} {
		if revoke.Flags().Lookup(flag) == nil {
			t.Errorf("grants revoke has no --%s", flag)
		}
	}
}

// resetOAuthFlags puts the flags of the `oauth grants` commands back to their defaults: the command objects
// are shared by every run of the test binary, so a flag one run set would stay set for the next.
func resetOAuthFlags(t *testing.T) {
	t.Helper()
	for sub, flags := range map[string]map[string]string{
		"list":   {"user": "", "org": "", "app": "", "json": "false"},
		"revoke": {"user": "", "org": "", "app": "", "all": "false", "yes": "false"},
	} {
		c, _, err := rootCmd.Find([]string{"oauth", "grants", sub})
		if err != nil {
			t.Fatal(err)
		}
		for name, def := range flags {
			f := c.Flags().Lookup(name)
			if f == nil {
				t.Fatalf("oauth grants %s has no --%s", sub, name)
			}
			_ = f.Value.Set(def)
			f.Changed = false
		}
	}
}

// Through the command tree: a revoke that names nothing, or something that makes no sense, is refused
// before the node is opened (these runs have no config and no registry).
func TestOAuthGrantsRevokeRefusesBeforeOpeningTheNode(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{nil, "name what to revoke"},
		{[]string{"abc"}, "is not a grant id"},
		{[]string{"7", "--all"}, "on its own"},
		{[]string{"--all", "--user", "a@b.test"}, "--all cannot be combined"},
		{[]string{"--app", "claude"}, "--app wants"},
		{[]string{"7", "8"}, "accepts at most 1 arg"},
	} {
		resetOAuthFlags(t)
		out, err := runRoot(t, append([]string{"--config", "/nonexistent/config.toml", "oauth", "grants", "revoke"}, tc.args...)...)
		resetOAuthFlags(t)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("revoke %v: error %v (output %q), want one with %q", tc.args, err, out, tc.want)
		}
	}
}
