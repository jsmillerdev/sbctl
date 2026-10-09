package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/supavise/supavise/internal/config"
)

// `supavise oauth grants` end to end over a real registry: the Postgres store, the service and the
// audit events behind the commands. It needs SUPAVISE_TEST_DATABASE_URL (CI provides one) and the
// registry's OAuth tables (migration 1350); the rows are made with SQL, so what the test depends on
// is the schema of the design (2.9) and the Authority contract, not on how a grant is made.
func TestOAuthGrantsAgainstPostgres(t *testing.T) {
	cfg, reg := orgsNode(t)
	ctx := context.Background()
	acme, err := reg.CreateOrganization(ctx, "acme", "Acme")
	if err != nil {
		t.Fatal(err)
	}
	labs, err := reg.CreateOrganization(ctx, "labs", "Labs")
	if err != nil {
		t.Fatal(err)
	}
	pool := reg.Pool()
	app := func(name string) string {
		t.Helper()
		var id string
		err := pool.QueryRow(ctx, `
			insert into supavise.oauth_apps (registration_type, name, redirect_uris, scopes)
			values ('dynamic', $1, array['http://127.0.0.1/callback'], array['projects:read','database:read'])
			returning id::text`, name).Scan(&id)
		if err != nil {
			t.Fatalf("inserting app %s: %v", name, err)
		}
		return id
	}
	grant := func(appID, user string, orgID int64) int64 {
		t.Helper()
		var id int64
		err := pool.QueryRow(ctx, `
			insert into supavise.oauth_grants (app_id, user_id, org_id, scopes)
			values ($1::uuid, $2::uuid, $3, array['projects:read'])
			returning id`, appID, user, orgID).Scan(&id)
		if err != nil {
			t.Fatalf("inserting grant: %v", err)
		}
		return id
	}
	const (
		alice = "22222222-bbbb-4bbb-8bbb-000000000001"
		bob   = "22222222-bbbb-4bbb-8bbb-000000000002"
	)
	claude, cursor := app("Claude Code"), app("Cursor")
	g1 := grant(claude, alice, acme.ID)
	g2 := grant(cursor, alice, labs.ID)
	g3 := grant(claude, bob, acme.ID)

	run := func(args ...string) string {
		t.Helper()
		resetOAuthFlags(t)
		defer resetOAuthFlags(t)
		out, err := runRoot(t, append([]string{"--config", cfg, "oauth", "grants"}, args...)...)
		if err != nil {
			t.Fatalf("oauth grants %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return out
	}
	// The sign-in service is not running in this test, so the commands note that users are shown by id;
	// the JSON starts at the first line that begins with a bracket.
	listIDs := func(args ...string) []int64 {
		t.Helper()
		out := run(append([]string{"list", "--json"}, args...)...)
		i := 0
		if !strings.HasPrefix(out, "[") {
			if i = strings.Index(out, "\n["); i < 0 {
				t.Fatalf("no JSON in %q", out)
			}
			i++
		}
		var vs []grantView
		if err := json.NewDecoder(strings.NewReader(out[i:])).Decode(&vs); err != nil {
			t.Fatalf("not JSON: %v\n%s", err, out)
		}
		var ids []int64
		for _, v := range vs {
			ids = append(ids, v.ID)
		}
		return ids
	}
	same := func(got []int64, want ...int64) bool {
		if len(got) != len(want) {
			return false
		}
		have := map[int64]bool{}
		for _, id := range got {
			have[id] = true
		}
		for _, id := range want {
			if !have[id] {
				return false
			}
		}
		return true
	}

	if got := listIDs(); !same(got, g1, g2, g3) {
		t.Fatalf("list: %v, want %v", got, []int64{g1, g2, g3})
	}
	if got := listIDs("--org", "acme"); !same(got, g1, g3) {
		t.Fatalf("list --org acme: %v", got)
	}
	if got := listIDs("--user", alice, "--app", claude); !same(got, g1) {
		t.Fatalf("list --user alice --app claude: %v", got)
	}

	// Revoking one organization's grants leaves the other's, and says how many it ended.
	if out := run("revoke", "--org", "acme", "--yes"); !strings.Contains(out, "revoked 2 grant(s)") {
		t.Fatalf("revoke --org acme: %s", out)
	}
	if got := listIDs(); !same(got, g2) {
		t.Fatalf("list after the revocation: %v, want only %d", got, g2)
	}
	var revoked, byOperator int
	if err := pool.QueryRow(ctx, `select count(*) filter (where revoked_at is not null), count(*) filter (where revoked_reason = 'operator')
		from supavise.oauth_grants`).Scan(&revoked, &byOperator); err != nil {
		t.Fatal(err)
	}
	if revoked != 2 || byOperator != 2 {
		t.Errorf("%d grants revoked, %d with the reason operator; want 2 and 2", revoked, byOperator)
	}
	// The service records each revocation on the system project, in the operator's name.
	evs, err := reg.ListEvents(ctx, config.SystemRef, 50)
	if err != nil {
		t.Fatal(err)
	}
	var audited int
	for _, e := range evs {
		if e.Kind == "oauth.grant_revoked" {
			audited++
			if !strings.Contains(string(e.Payload), "operator") {
				t.Errorf("revocation event without the operator: %s", e.Payload)
			}
		}
	}
	if audited == 0 {
		t.Errorf("no oauth.grant_revoked event among %d events", len(evs))
	}

	// Everything that is left, and then nothing to end.
	if out := run("revoke", "--all", "--yes"); !strings.Contains(out, "revoked 1 grant(s)") {
		t.Fatalf("revoke --all: %s", out)
	}
	if out := run("revoke", "--all", "--yes"); !strings.Contains(out, "revoked 0 grant(s)") {
		t.Fatalf("revoke --all again: %s", out)
	}
	if got := listIDs(); len(got) != 0 {
		t.Fatalf("list after everything was revoked: %v", got)
	}
}
