package branching

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/OWNER/sbctl/internal/registry"
	"github.com/OWNER/sbctl/internal/secrets"
)

func twoKeySets(t *testing.T) (parent, branch *secrets.ProjectKeys) {
	t.Helper()
	parent, err := secrets.NewProjectKeys("pppppppppppppppppppp", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	branch, err = secrets.NewProjectKeys("bbbbbbbbbbbbbbbbbbbb", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return parent, branch
}

// Every kind of parent credential becomes the branch's corresponding one, whether the value is
// the credential or only contains it, and nothing of the parent is left in the result.
func TestCredentialSwapReplacesTheParentsCredentials(t *testing.T) {
	parent, branch := twoKeySets(t)
	sw := newCredentialSwap(parent, branch)

	// An older service key: signed with the parent's secret, issued before the one in the registry
	// (a re-sign changes iat and exp, not the secret), and one that has expired. Both still open the parent.
	older, err := secrets.NewLegacyKey(parent.JWTSecret, "pppppppppppppppppppp", secrets.RoleServiceRole, time.Now().Add(-24*time.Hour))
	if err != nil || older == parent.ServiceRoleKey {
		t.Fatalf("older key: %v (same as the current one: %v)", err, older == parent.ServiceRoleKey)
	}
	expired, err := secrets.NewLegacyKey(parent.JWTSecret, "pppppppppppppppppppp", secrets.RoleAnon, time.Now().Add(-20*365*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	stranger, err := secrets.NewLegacyKey("some-other-projects-secret-of-length-40!", "pppppppppppppppppppp", secrets.RoleServiceRole, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	user, err := secrets.NewLegacyKey(parent.JWTSecret, "pppppppppppppppppppp", secrets.RoleAnon, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// A parent-signed token of a role that has no branch counterpart (here: forged by role name).
	authenticated := resign(t, parent.JWTSecret, "authenticated")

	for _, tc := range []struct {
		name, in, want string
		labels         []string
	}{
		{"service key", parent.ServiceRoleKey, branch.ServiceRoleKey, []string{"service_role_key"}},
		{"anon key", parent.AnonKey, branch.AnonKey, []string{"anon_key"}},
		{"publishable key", parent.PublishableKey, branch.PublishableKey, []string{"publishable_key"}},
		{"secret key", parent.SecretKey, branch.SecretKey, []string{"secret_key"}},
		{"jwt secret", parent.JWTSecret, branch.JWTSecret, []string{"jwt_secret"}},
		{"db password", parent.DBPassword, branch.DBPassword, []string{"db_password"}},
		{"bearer header", "Bearer " + parent.ServiceRoleKey, "Bearer " + branch.ServiceRoleKey, []string{"service_role_key"}},
		{"json", `{"Authorization":"Bearer ` + parent.SecretKey + `","apikey":"` + parent.PublishableKey + `"}`,
			`{"Authorization":"Bearer ` + branch.SecretKey + `","apikey":"` + branch.PublishableKey + `"}`, []string{"secret_key", "publishable_key"}},
		{"connection string", "host=127.0.0.1 port=5432 user=postgres password=" + parent.DBPassword,
			"host=127.0.0.1 port=5432 user=postgres password=" + branch.DBPassword, []string{"db_password"}},
		{"older service key", older, branch.ServiceRoleKey, []string{"service_role_key"}},
		{"expired anon key", expired, branch.AnonKey, []string{"anon_key"}},
		{"another anon token of the parent", user, branch.AnonKey, []string{"anon_key"}},
		{"older key in a header", "Authorization: Bearer " + older + "\n", "Authorization: Bearer " + branch.ServiceRoleKey + "\n", []string{"service_role_key"}},
		{"a key of another project is not the parent's", stranger, stranger, nil},
		{"a parent-signed token of another role is left", authenticated, authenticated, nil},
		{"unrelated", "https://example.com/functions/v1/hello", "https://example.com/functions/v1/hello", nil},
		{"empty", "", "", nil},
	} {
		got, labels := sw.rewrite(tc.in)
		if got != tc.want {
			t.Errorf("%s: rewrite = %q, want %q", tc.name, got, tc.want)
		}
		if strings.Join(labels, ",") != strings.Join(tc.labels, ",") {
			t.Errorf("%s: labels = %v, want %v", tc.name, labels, tc.labels)
		}
		for _, l := range labels { // names, never values
			if strings.ContainsAny(l, " =") || strings.Contains(l, "eyJ") || strings.HasPrefix(l, "sb_") {
				t.Errorf("%s: label %q looks like a value", tc.name, l)
			}
		}
	}
}

func resign(t *testing.T, secret, role string) string {
	t.Helper()
	// NewLegacyKey signs anon and service_role only; build the claims by hand.
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"role": role, "exp": time.Now().Add(time.Hour).Unix()}).SignedString([]byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// A short credential is only replaced when it is the whole value: replacing it inside text would
// mangle data that merely contains the word.
func TestCredentialSwapMatchesShortCredentialsWhole(t *testing.T) {
	parent, branch := twoKeySets(t)
	parent.JWTSecret = "secret"
	sw := newCredentialSwap(parent, branch)
	if got, labels := sw.rewrite("a secret recipe"); got != "a secret recipe" || len(labels) != 0 {
		t.Fatalf("short secret replaced inside text: %q %v", got, labels)
	}
	if got, labels := sw.rewrite("secret"); got != branch.JWTSecret || len(labels) != 1 {
		t.Fatalf("short secret as a whole value: %q %v", got, labels)
	}
}

// Credentials that are the same on both sides (the pgsodium root key is shared) and empty ones are not swapped.
func TestCredentialSwapIgnoresEqualAndEmptyCredentials(t *testing.T) {
	parent, branch := twoKeySets(t)
	branch.DBPassword = parent.DBPassword
	parent.ReplicationPassword, branch.ReplicationPassword = "", ""
	sw := newCredentialSwap(parent, branch)
	if got, labels := sw.rewrite(parent.DBPassword); got != parent.DBPassword || len(labels) != 0 {
		t.Fatalf("an unchanged password was rewritten: %q %v", got, labels)
	}
	if got, _ := sw.rewrite("anything"); got != "anything" {
		t.Fatalf("empty credentials matched: %q", got)
	}
}

func TestNeutralizeConnStrings(t *testing.T) {
	const off = "'" + disabledConnString + "'"
	for _, tc := range []struct {
		name, in, want string
		n              int
	}{
		{"dblink_exec", `select dblink_exec('host=127.0.0.1 port=5433 dbname=postgres user=postgres password=hunter2', 'insert into t values (1)')`,
			`select dblink_exec(` + off + `, 'insert into t values (1)')`, 1},
		{"dblink with a URI", `select * from dblink('postgresql://u:p@127.0.0.1:5433/db', 'select 1') as t(a int)`,
			`select * from dblink(` + off + `, 'select 1') as t(a int)`, 1},
		{"hostaddr first", `select dblink_connect('c', 'hostaddr=10.0.0.5 dbname=x')`, `select dblink_connect('c', ` + off + `)`, 1},
		{"dollar quoted", `select dblink_exec($$host=127.0.0.1 dbname=postgres$$, $q$delete from t$q$)`,
			`select dblink_exec($$` + disabledConnString + `$$, $q$delete from t$q$)`, 1},
		{"tagged dollar quote", `select dblink($c$host=a dbname=b$c$, 'select 1')`, `select dblink($c$` + disabledConnString + `$c$, 'select 1')`, 1},
		{"two strings", `select dblink('host=a dbname=b', 'x'); select dblink('host=c dbname=d', 'y')`,
			`select dblink(` + off + `, 'x'); select dblink(` + off + `, 'y')`, 2},
		{"named server is left (the server itself is disabled)", `select dblink_connect('c', 'prod_server')`, `select dblink_connect('c', 'prod_server')`, 0},
		{"doubled quotes inside", `select 'it''s fine', 'host=x dbname=y'`, `select 'it''s fine', ` + off, 1},
		{"escape string", `select E'a\'b host=z', 'plain'`, `select E` + off + `, 'plain'`, 1},
		{"a comment is not a literal", "select 1 -- 'host=1.2.3.4 dbname=x'\n, 'ok'", "select 1 -- 'host=1.2.3.4 dbname=x'\n, 'ok'", 0},
		{"a block comment is not a literal", "select /* 'host=a dbname=b' */ 1", "select /* 'host=a dbname=b' */ 1", 0},
		{"a quoted identifier is not a literal", `select "host=a dbname=b" from t`, `select "host=a dbname=b" from t`, 0},
		{"parameters are not dollar quotes", `select $1, 'x' || $2`, `select $1, 'x' || $2`, 0},
		{"a dollar quote inside a string", `select '$$host=a dbname=b$$'`, `select ` + off, 1},
		{"unrelated command", `select cron.schedule_in_database('x', '* * * * *', 'vacuum')`, `select cron.schedule_in_database('x', '* * * * *', 'vacuum')`, 0},
		{"unterminated string", `select 'host=a dbname=b`, `select 'host=a dbname=b`, 0},
		{"unterminated dollar quote", `select $$host=a dbname=b`, `select $$host=a dbname=b`, 0},
		{"empty", ``, ``, 0},
	} {
		got, n := neutralizeConnStrings(tc.in)
		if got != tc.want || n != tc.n {
			t.Errorf("%s:\n got %q (%d)\nwant %q (%d)", tc.name, got, n, tc.want, tc.n)
		}
		if again, _ := neutralizeConnStrings(got); again != got {
			t.Errorf("%s: not stable when run twice: %q", tc.name, again)
		}
	}
}

func TestSpeaksPostgres(t *testing.T) {
	for _, tc := range []struct {
		fdw  string
		opts map[string]string
		want bool
	}{
		{"postgres_fdw", nil, true},
		{"dblink_fdw", map[string]string{}, true},
		{"stripe_wrapper", map[string]string{"api_url": "https://api.stripe.com"}, false},
		{"mystery_fdw", map[string]string{"host": "127.0.0.1"}, true},
		{"mystery_fdw", map[string]string{"dbname": "x"}, true},
		{"mystery_fdw", map[string]string{"hostaddr": "10.0.0.1"}, true},
	} {
		if got := speaksPostgres(tc.fdw, tc.opts); got != tc.want {
			t.Errorf("speaksPostgres(%s, %v) = %v, want %v", tc.fdw, tc.opts, got, tc.want)
		}
	}
	got := foreignOptions([]string{"host=127.0.0.1", "PORT=5433", "password=a=b", "novalue"})
	if got["host"] != "127.0.0.1" || got["port"] != "5433" || got["password"] != "a=b" {
		t.Fatalf("foreignOptions = %v", got)
	}
	if names := passwordMappingOptions([]string{"user=postgres", "password=x", "sslpassword=y", "Passwd=z"}); strings.Join(names, ",") != "passwd,password,sslpassword" {
		t.Fatalf("passwordMappingOptions = %v", names)
	}
	if len(passwordMappingOptions([]string{"user=postgres", "password_required=false"})) != 0 {
		t.Fatal("password_required is not a password")
	}
	// A dbname that is a connection string may carry a password: it is not recorded.
	for in, want := range map[string]string{"postgres": "postgres", "host=a password=b": "", "postgresql://u:p@h/db": ""} {
		if got := plainConnValue(in); got != want {
			t.Errorf("plainConnValue(%q) = %q, want %q", in, got, want)
		}
	}
}

// The policy has its own writer: a PATCH that read the branch before isolation denied egress
// and writes it after must not put the old policy back, and denyEgress cannot undo a policy
// that something else replaced.
func TestEgressPolicyIsNotWrittenByAStaleUpdate(t *testing.T) {
	h := cloneHarness(t)
	ctx := context.Background()
	b := h.create("data", func(in *CreateInput) { in.WithData = true })
	if b.Egress != registry.EgressPending {
		t.Fatalf("egress = %q", b.Egress)
	}
	// Between the PATCH's read of the row and its write, isolation denies egress.
	h.svc.reg = &hookRegistry{Registry: h.reg, beforeUpdate: func() {
		if err := h.svc.denyEgress(ctx, b.Ref); err != nil {
			t.Error(err)
		}
	}}
	name := "renamed"
	if _, err := h.svc.Update(ctx, b.Ref, UpdateInput{Name: &name}); err != nil {
		t.Fatal(err)
	}
	p, err := h.reg.GetProject(ctx, b.Ref)
	if err != nil || p.Branch.Egress != registry.EgressDenied || p.Branch.Name != "renamed" {
		t.Fatalf("after the racing PATCH: %+v %v", p.Branch, err)
	}
}

func TestDenyEgressIsACompareAndSet(t *testing.T) {
	h := cloneHarness(t)
	ctx := context.Background()
	b := h.create("data", func(in *CreateInput) { in.WithData = true })
	// Twice is fine.
	for i := 0; i < 2; i++ {
		if err := h.svc.denyEgress(ctx, b.Ref); err != nil {
			t.Fatalf("denyEgress #%d: %v", i+1, err)
		}
	}
	// A policy that was replaced by something else is not overwritten by a denyEgress that read the old one.
	if err := h.reg.SetBranchEgress(ctx, b.Ref, registry.EgressDenied, registry.EgressPending); err != nil {
		t.Fatal(err)
	}
	h.svc.reg = &hookRegistry{Registry: h.reg, beforeSetEgress: func() {
		if err := h.reg.SetBranchEgress(ctx, b.Ref, registry.EgressPending, registry.EgressAllowed); err != nil {
			t.Error(err)
		}
	}}
	if err := h.svc.denyEgress(ctx, b.Ref); !errors.Is(err, registry.ErrConflict) {
		t.Fatalf("denyEgress over a changed policy = %v, want a conflict", err)
	}
	if p, _ := h.reg.GetProject(ctx, b.Ref); p.Branch.Egress != registry.EgressAllowed {
		t.Fatalf("egress = %q", p.Branch.Egress)
	}
}

// A reset reads the policy when it records the new one, so a policy that changed after the reset
// began is neither lost nor overwritten blindly.
func TestResetRecordsThePolicyThroughTheSetter(t *testing.T) {
	h := cloneHarness(t)
	ctx := context.Background()
	b := h.create("data", func(in *CreateInput) { in.WithData = true })
	if err := h.reg.SetBranchEgress(ctx, b.Ref, registry.EgressPending, registry.EgressDenied); err != nil {
		t.Fatal(err)
	}
	var sets []string
	h.svc.reg = &hookRegistry{Registry: h.reg, onSetEgress: func(from, to string) { sets = append(sets, from+">"+to) }}
	if _, err := h.svc.Reset(ctx, b.Ref, ActionInput{}); err != nil {
		t.Fatal(err)
	}
	got := h.wait(b.Ref)
	h.mustState(got, registry.BranchMigrationsPassed)
	if len(sets) != 1 || sets[0] != registry.EgressDenied+">"+registry.EgressPending || got.Egress != registry.EgressPending {
		t.Fatalf("setter calls = %v, egress = %q", sets, got.Egress)
	}
}

// hookRegistry runs hooks around the registry calls the egress policy goes through.
type hookRegistry struct {
	registry.Registry
	beforeUpdate    func() // before UpdateBranch writes
	beforeSetEgress func() // before SetBranchEgress, once
	onSetEgress     func(from, to string)
}

func (r *hookRegistry) UpdateBranch(ctx context.Context, ref string, b *registry.BranchInfo) error {
	if f := r.beforeUpdate; f != nil {
		r.beforeUpdate = nil
		f()
	}
	return r.Registry.UpdateBranch(ctx, ref, b)
}

func (r *hookRegistry) SetBranchEgress(ctx context.Context, ref, from, to string) error {
	if f := r.beforeSetEgress; f != nil {
		r.beforeSetEgress = nil
		f()
	}
	if r.onSetEgress != nil {
		r.onSetEgress(from, to)
	}
	return r.Registry.SetBranchEgress(ctx, ref, from, to)
}
