package branching

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/secrets"
)

// rotateCredentials gives a branch whose cluster came from the parent (a clone or a
// restore) credentials of its own. The data carries the parent's role passwords, so
// anyone holding the parent's database password could otherwise open the branch, and the
// reverse. Steps, in this order so that a failure leaves a state that a retry (or a reset)
// repairs rather than a branch nobody can open:
//
//  1. seal the new passwords into the registry (the running services keep their old ones)
//  2. set them on the roles, over the cluster's private unix socket as supabase_admin
//  3. Manager.RotateKeys: new JWT secret and API keys, GoTrue and PostgREST restart on the
//     new environment, and the fleet tenants (Supavisor, Realtime, Storage) are updated
//
// The temporary login roles the parent issued for `supabase db push|pull|dump` (cli_login_*,
// supavise_cli_ro_*) came along with their password verifiers; step 2 also disables them
// (NOLOGIN, no password, already expired), so that a parent's CLI password does not open the
// branch. The API drops them with the next expired-role sweep.
//
// The pgsodium root key cannot change: the Vault secrets inside the data are encrypted
// with it. The branch shares it with the parent, so whoever holds the parent's root key can
// decrypt the branch's copy of the parent's Vault secrets. Re-encrypting vault.secrets under
// a key of its own is not done (required for full separation, see the README).
func (s *Service) rotateCredentials(ctx context.Context, ref string) error {
	p, err := s.reg.GetProject(ctx, ref)
	if err != nil {
		return err
	}
	old, err := s.eng.Keys(ctx, ref)
	if err != nil {
		return err
	}
	nk := *old
	nk.DBPassword, nk.AdminPassword, nk.AuthenticatorPassword = secrets.NewPassword(), secrets.NewPassword(), secrets.NewPassword()
	nk.AuthAdminPassword, nk.StorageAdminPassword, nk.ReplicationPassword = secrets.NewPassword(), secrets.NewPassword(), secrets.NewPassword()

	pw := map[string]string{ // secret name -> new value
		secrets.NameDBPassword: nk.DBPassword, secrets.NameAdminPassword: nk.AdminPassword,
		secrets.NameAuthenticatorPassword: nk.AuthenticatorPassword, secrets.NameAuthAdminPassword: nk.AuthAdminPassword,
		secrets.NameStorageAdminPassword: nk.StorageAdminPassword, secrets.NameReplicationPassword: nk.ReplicationPassword,
	}
	for name, v := range pw {
		sealed, err := s.sec.Seal([]byte(v))
		if err != nil {
			return err
		}
		if err := s.reg.PutSecret(ctx, ref, name, sealed); err != nil {
			return err
		}
	}
	if err := setRolePasswords(ctx, s.adminSocketDSN(ref, p.Seq), map[string]string{
		lifecycle.RolePostgres: nk.DBPassword, lifecycle.RoleAdmin: nk.AdminPassword, lifecycle.RoleAuthn: nk.AuthenticatorPassword,
		lifecycle.RoleAuthAdmin: nk.AuthAdminPassword, lifecycle.RoleStorage: nk.StorageAdminPassword, lifecycle.RoleReplication: nk.ReplicationPassword,
	}); err != nil {
		return err
	}
	if err := disableLoginRoles(ctx, s.adminSocketDSN(ref, p.Seq)); err != nil {
		return err
	}
	if _, err := s.eng.RotateKeys(ctx, ref); err != nil {
		return err
	}
	return nil
}

// disableLoginRoles makes the parent's temporary CLI login roles unusable on the branch: no
// login, no password, validity in the past (the API's expired-role sweep then drops them).
func disableLoginRoles(ctx context.Context, dsn string) error {
	c, err := connect(ctx, dsn, "")
	if err != nil {
		return err
	}
	defer closeConn(c)
	_, err = c.Exec(ctx, `do $$ declare r record; begin
  for r in select rolname from pg_roles where rolname like 'cli\_login\_%' or rolname like 'supavise\_cli\_ro\_%' loop
    execute format('alter role %I nologin password null valid until %L', r.rolname, '1970-01-01 00:00:00+00');
  end loop;
end $$`)
	if err != nil {
		return fmt.Errorf("disable the parent's temporary login roles: %w", err)
	}
	return nil
}

// adminSocketDSN connects as supabase_admin over the cluster's private unix socket, which
// needs no password and cannot be reached from outside the node.
func (s *Service) adminSocketDSN(ref string, seq int) string {
	sock := filepath.Join(s.cfg.Paths().ProjectService(ref, config.SvcPostgres), "sock")
	return fmt.Sprintf("host=%s port=%d user=%s dbname=postgres sslmode=disable connect_timeout=5 application_name=supavise-branching",
		kvQuote(sock), s.cfg.PortsFor(ref, seq).Postgres, lifecycle.RoleAdmin)
}

func kvQuote(v string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(v) + "'"
}

// setRolePasswords sets login passwords as SCRAM-SHA-256 verifiers, never plaintext, with
// statement logging off for the session: the artifact logs DDL, and a plaintext ALTER ROLE
// would end up in the journal.
func setRolePasswords(ctx context.Context, dsn string, passwords map[string]string) error {
	cc, err := pgx.ParseConfig(dsn)
	if err != nil {
		return err
	}
	c, err := pgx.ConnectConfig(ctx, cc)
	if err != nil {
		return fmt.Errorf("connect over the unix socket: %w", err)
	}
	defer closeConn(c)
	if _, err := c.Exec(ctx, `set log_statement = 'none'`); err != nil {
		return err
	}
	if _, err := c.Exec(ctx, `set pgaudit.log = 'none'`); err != nil {
		return err
	}
	for role, pw := range passwords {
		verifier, err := lifecycle.ScramVerifier(pw)
		if err != nil {
			return err
		}
		var stmt string
		if err := c.QueryRow(ctx, `select format('alter role %I with password %L', $1::text, $2::text)`, role, verifier).Scan(&stmt); err != nil {
			return err
		}
		if _, err := c.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("set the password of %s: %w", role, err)
		}
	}
	return nil
}
