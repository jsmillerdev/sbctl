package branching

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/OWNER/sbctl/internal/config"
	"github.com/OWNER/sbctl/internal/registry"
)

// A branch cloned from the parent's data (copy-on-write or base backup) carries everything
// the parent had, including its connections to the outside world. Left alone, the branch would
// act as a second copy of the parent toward those systems:
//
//   - a logical replication SUBSCRIPTION is copied enabled, with the same slot name. The
//     branch's apply worker attaches to the parent's slot on the publisher and consumes changes
//     the parent never receives.
//   - pg_cron jobs, pg_net calls included, run on the branch as well as on the parent.
//   - HTTP requests queued in pg_net but not yet sent are sent a second time.
//
// So the first start of such a branch runs with the background workers that act on these
// disabled (quarantineSettings, written to postgresql.auto.conf before the first start). Over
// the cluster's private socket isolateCluster then disables every subscription and detaches it
// from its slot, pauses the cron jobs (recording which were active, unless [branching]
// keep_cron_jobs or the branch was created with allow_egress) and empties the pg_net queue;
// then the settings are removed, the branch's outbound network policy is recorded as denied
// (the Postgres unit is rendered behind systemd's IPAddressDeny from then on) and the cluster
// restarts normally.

const quarantineMark = "# sbctl-branch-quarantine"

// cronNodeName is the nodename of a branch's pg_cron jobs: the address its cluster listens on.
const cronNodeName = "127.0.0.1"

// quarantineSettings are the postmaster settings of a branch's first start.
var quarantineSettings = []struct{ key, value string }{
	{"max_logical_replication_workers", "0"},
	{"cron.launch_active_jobs", "off"},
	{"pg_net.database_name", "'sbctl_quarantine_no_such_database'"},
}

func quarantineLines() string {
	var b strings.Builder
	for _, q := range quarantineSettings {
		fmt.Fprintf(&b, "%s = %s %s\n", q.key, q.value, quarantineMark)
	}
	return b.String()
}

// quarantineSaved is the file next to postgresql.auto.conf that remembers the lines the
// parent had for the quarantined settings, so that clearQuarantine can put them back.
const quarantineSaved = "sbctl-branch-quarantine.json"

// confKey is the setting name of a postgresql.auto.conf line ("" for comments and blanks).
func confKey(line string) string {
	l := strings.TrimSpace(line)
	if l == "" || strings.HasPrefix(l, "#") {
		return ""
	}
	k, _, ok := strings.Cut(l, "=")
	if !ok {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(k))
}

func quarantined(key string) bool {
	for _, q := range quarantineSettings {
		if q.key == key {
			return true
		}
	}
	return false
}

// writeQuarantine appends the quarantine settings to the data directory's postgresql.auto.conf
// (created when missing). Later entries win, so an entry the parent had for the same key is
// overridden. The parent's own lines for these keys are saved first: ALTER SYSTEM (a restore
// runs ALTER SYSTEM RESET after recovery) rewrites the whole file and drops comments and
// duplicates, so the lines cannot be told apart by a marker afterwards.
func writeQuarantine(dataDir string) error {
	p := filepath.Join(dataDir, "postgresql.auto.conf")
	cur, err := os.ReadFile(p)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	saved := map[string]string{}
	for _, l := range strings.Split(string(cur), "\n") {
		if k := confKey(l); quarantined(k) && !strings.Contains(l, quarantineMark) {
			saved[k] = l // the last one wins, as in PostgreSQL
		}
	}
	sp := filepath.Join(dataDir, quarantineSaved)
	if _, err := os.Stat(sp); os.IsNotExist(err) {
		b, _ := json.Marshal(saved)
		if err := writeFileSync(sp, b, 0o600); err != nil {
			return err
		}
	}
	if len(cur) > 0 && !bytes.HasSuffix(cur, []byte("\n")) {
		cur = append(cur, '\n')
	}
	return writeFileSync(p, append(cur, quarantineLines()...), 0o600)
}

// clearQuarantine removes the quarantine settings from postgresql.auto.conf, however the file
// was rewritten since (marker or not, quoted or not), and puts back the lines the parent had.
func clearQuarantine(dataDir string) error {
	p := filepath.Join(dataDir, "postgresql.auto.conf")
	sp := filepath.Join(dataDir, quarantineSaved)
	var saved map[string]string
	if b, err := os.ReadFile(sp); err == nil {
		_ = json.Unmarshal(b, &saved)
	}
	cur, err := os.ReadFile(p)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err == nil {
		var keep []string
		for _, l := range strings.SplitAfter(string(cur), "\n") {
			if !strings.Contains(l, quarantineMark) && !quarantined(confKey(l)) {
				keep = append(keep, l)
			}
		}
		out := strings.Join(keep, "")
		if len(out) > 0 && !strings.HasSuffix(out, "\n") {
			out += "\n"
		}
		for _, q := range quarantineSettings { // a stable order
			if l, ok := saved[q.key]; ok {
				out += l + "\n"
			}
		}
		tmp := p + ".sbctl-tmp"
		if err := writeFileSync(tmp, []byte(out), 0o600); err != nil {
			return err
		}
		if err := os.Rename(tmp, p); err != nil {
			return err
		}
	}
	if err := os.Remove(sp); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// IsolateResult says what isolateCluster changed (recorded in the branch's events).
type IsolateResult struct {
	Subscriptions int  `json:"subscriptions_disabled"`
	CronJobs      int  `json:"cron_jobs_deactivated"`
	CronKept      bool `json:"cron_jobs_kept"`
	NetQueue      int  `json:"net_requests_dropped"`
	// Egress is the branch's outbound network policy once isolation is done (registry.Egress*).
	Egress string `json:"egress,omitempty"`
	// ForeignServers is the number of foreign servers (postgres_fdw, dblink, ...) made unusable,
	// MappingPasswords the number of user mappings that lost a password and CronCommands the
	// number of pg_cron commands that had a connection string replaced (see foreign.go).
	// ForeignKept is true when the branch has the egress opt-out and they were left as they were.
	ForeignServers   int  `json:"foreign_servers_disabled"`
	MappingPasswords int  `json:"user_mapping_passwords_dropped"`
	CronCommands     int  `json:"cron_commands_neutralized"`
	ForeignKept      bool `json:"foreign_servers_kept"`
	// ForeignNotNeutralized names foreign servers of another wrapper than postgres_fdw and
	// dblink_fdw whose options the wrapper's validator would not let isolation change.
	ForeignNotNeutralized []string `json:"foreign_servers_not_neutralized,omitempty"`
	// CronNodesReset is the number of cron.job rows whose nodename and nodeport (the parent's
	// own, stamped when the job was scheduled) now name the branch's cluster.
	CronNodesReset int `json:"cron_nodes_reset"`
	// AuthRowsDeleted counts the rows removed, per table, from the parent's GoTrue session
	// material (isolate_auth.go); AuthRowsCleared the rows whose one-time token columns were
	// emptied, per table. Counts only, never values. AuthTablesNotReviewed names tables of the auth
	// schema that neither list of isolate_auth.go knows (a GoTrue release newer than the one that
	// was reviewed).
	AuthRowsDeleted       map[string]int64 `json:"auth_rows_deleted,omitempty"`
	AuthRowsCleared       map[string]int64 `json:"auth_rows_cleared,omitempty"`
	AuthTablesNotReviewed []string         `json:"auth_tables_not_reviewed,omitempty"`
	// StorageRowsDeleted counts the rows removed, per table, from the parent's Storage object
	// metadata (isolate_storage.go): a branch has none of the parent's objects, only its buckets.
	StorageRowsDeleted map[string]int64 `json:"storage_rows_deleted,omitempty"`
	// DatabasesOpened names databases that refuse connections (datallowconn = false) and that
	// isolation opened for its own session and closed again.
	DatabasesOpened []string `json:"databases_opened,omitempty"`
}

// isolateOptions say what isolateCluster leaves alone: the opt-outs of a branch.
type isolateOptions struct {
	// KeepCron leaves the parent's pg_cron jobs active ([branching] keep_cron_jobs, or allow_egress).
	KeepCron bool
	// KeepForeign leaves foreign servers, user mappings and the connection strings in cron
	// commands as the parent had them (allow_egress: the branch is meant to act on the outside).
	KeepForeign bool
	// NodeName and NodePort are what every cron.job row's nodename and nodeport become: the
	// branch's own cluster. pg_cron stamps the parent's port into a job when it is scheduled, so
	// a job re-activated in the branch would otherwise connect to the parent. NodePort 0 leaves
	// the rows alone.
	NodeName string
	NodePort int
}

// isolateCluster neutralizes the parent's outbound integrations in every database of the
// cluster reachable at dsn (a superuser on the cluster's private socket). It is safe to run
// twice.
func isolateCluster(ctx context.Context, dsn string, opt isolateOptions) (res IsolateResult, err error) {
	res.CronKept, res.ForeignKept = opt.KeepCron, opt.KeepForeign
	dbs, opened, restore, err := openDatabases(ctx, dsn)
	if err != nil {
		return res, err
	}
	res.DatabasesOpened = opened
	defer func() {
		if rerr := restore(); rerr != nil && err == nil {
			err = rerr
		}
	}()
	for _, db := range dbs {
		if err := isolateDatabase(ctx, dsn, db, opt, &res); err != nil {
			return res, fmt.Errorf("database %s: %w", db, err)
		}
	}
	return res, nil
}

// openDatabases lists the databases isolation and the credential rewrite have to work through:
// every database of the cluster except template0 (which is pristine and cannot be changed),
// template databases and the ones with datallowconn = false included. The owner of a database
// that refuses connections can allow them again in the branch, and what it holds (foreign
// servers, Vault secrets, cron jobs) is as much a copy of the parent as the rest, so such a
// database is opened for the duration with ALTER DATABASE ... ALLOW_CONNECTIONS true as the
// superuser; restore puts the flag back. opened names the databases it opened. If sbctl dies
// between the two the branch stays open, and the branch it belongs to has not finished
// creating (isolation did not complete), so it ends up failed, not in use.
func openDatabases(ctx context.Context, dsn string) (dbs, opened []string, restore func() error, err error) {
	restore = func() error { return nil }
	c, err := connect(ctx, dsn, "")
	if err != nil {
		return nil, nil, restore, err
	}
	defer closeConn(c)
	rows, err := c.Query(ctx, `select datname, datallowconn from pg_database where datname <> 'template0' and datconnlimit <> -2 order by datname`)
	if err != nil {
		return nil, nil, restore, err
	}
	var closed []string
	for rows.Next() {
		var name string
		var allow bool
		if err := rows.Scan(&name, &allow); err != nil {
			rows.Close()
			return nil, nil, restore, err
		}
		dbs = append(dbs, name)
		if !allow {
			closed = append(closed, name)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, restore, err
	}
	setAllow := func(ctx context.Context, c *pgx.Conn, name string, allow bool) error {
		var stmt string
		if err := c.QueryRow(ctx, `select format('alter database %I allow_connections %s', $1::text, $2::text)`, name, fmt.Sprint(allow)).Scan(&stmt); err != nil {
			return err
		}
		_, err := c.Exec(ctx, stmt)
		return err
	}
	var done []string
	restore = func() error {
		if len(done) == 0 {
			return nil
		}
		// The caller's context may be what failed: the flags go back regardless.
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		rc, err := connect(rctx, dsn, "")
		if err != nil {
			return fmt.Errorf("close the databases that refused connections: %w", err)
		}
		defer closeConn(rc)
		var firstErr error
		for _, name := range done {
			if err := setAllow(rctx, rc, name, false); err != nil && firstErr == nil {
				firstErr = fmt.Errorf("close database %s again: %w", name, err)
			}
		}
		done = nil
		return firstErr
	}
	for _, name := range closed {
		if err := setAllow(ctx, c, name, true); err != nil {
			rerr := restore()
			return nil, nil, func() error { return nil }, errors.Join(fmt.Errorf("open database %s: %w", name, err), rerr)
		}
		done = append(done, name)
		opened = append(opened, name)
	}
	return dbs, opened, restore, nil
}

func isolateDatabase(ctx context.Context, dsn, db string, opt isolateOptions, res *IsolateResult) error {
	c, err := connect(ctx, dsn, db)
	if err != nil {
		return err
	}
	defer closeConn(c)
	rows, err := c.Query(ctx, `select subname from pg_subscription where subdbid = (select oid from pg_database where datname = current_database())`)
	if err != nil {
		return err
	}
	subs, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, sub := range subs {
		// DISABLE first: slot_name = NONE is only allowed on a disabled subscription.
		for _, verb := range []string{"disable", "set (slot_name = none)"} {
			var stmt string
			if err := c.QueryRow(ctx, `select format('alter subscription %I `+verb+`', $1::text)`, sub).Scan(&stmt); err != nil {
				return err
			}
			if _, err := c.Exec(ctx, stmt); err != nil {
				return fmt.Errorf("alter subscription %s: %w", sub, err)
			}
		}
		res.Subscriptions++
	}
	var hasCron bool
	if err := c.QueryRow(ctx, `select to_regclass('cron.job') is not null`).Scan(&hasCron); err != nil {
		return err
	}
	if hasCron && opt.NodePort != 0 {
		n, err := resetCronNodes(ctx, c, opt.NodeName, opt.NodePort)
		if err != nil {
			return fmt.Errorf("point cron jobs at the branch: %w", err)
		}
		res.CronNodesReset += n
	}
	if hasCron && !opt.KeepCron {
		n, err := pauseCronJobs(ctx, c)
		if err != nil {
			return fmt.Errorf("pause cron jobs: %w", err)
		}
		res.CronJobs += n
	}
	if !opt.KeepForeign {
		// Cron jobs that stay active (keep_cron_jobs) would otherwise reach other projects through
		// a dblink string in their command; paused ones too, for whoever activates them.
		if hasCron {
			n, err := neutralizeCronCommands(ctx, c)
			if err != nil {
				return fmt.Errorf("neutralize cron commands: %w", err)
			}
			res.CronCommands += n
		}
		if err := neutralizeForeign(ctx, c, res); err != nil {
			return fmt.Errorf("neutralize foreign servers: %w", err)
		}
	}
	// The session wipe runs after the parent's outbound integrations (cron commands, foreign
	// servers) are neutralized, and with user triggers suppressed (see wipeAuthSessions), so a
	// trigger on the auth tables cannot act on production while the branch still has network.
	if err := wipeAuthSessions(ctx, c, res); err != nil {
		return fmt.Errorf("remove the parent's GoTrue sessions: %w", err)
	}
	if err := clearStorageObjects(ctx, c, res); err != nil {
		return fmt.Errorf("remove the parent's Storage objects: %w", err)
	}
	var hasQueue bool
	if err := c.QueryRow(ctx, `select to_regclass('net.http_request_queue') is not null`).Scan(&hasQueue); err != nil {
		return err
	}
	if hasQueue {
		tag, err := c.Exec(ctx, `delete from net.http_request_queue`)
		if err != nil {
			return fmt.Errorf("empty the pg_net queue: %w", err)
		}
		res.NetQueue += int(tag.RowsAffected())
	}
	return nil
}

// PausedCronTable is the table in a branch's cron database that lists the pg_cron jobs that
// were active in the parent's data and were paused for the branch. Re-activating them is an
// explicit decision of whoever owns the branch (a job that calls out needs the branch's
// egress open too):
//
//	update cron.job set active = true where jobid in (select jobid from sbctl_branch.paused_cron_jobs);
//
// Two things about the commands of those jobs changed when the branch was made, and the jobs
// will not behave as in the parent until the owner redoes them: a connection string written
// as a literal in a command (dblink('host=... password=...', ...)) was replaced by a disabled
// one (the jobs are listed in NeutralizedCronTable; the original is not kept, it may hold a
// password), and a parent credential in a command (an API key, a JWT secret, a database
// password) was replaced by the branch's own (RewriteTable). A job that reaches a foreign
// server by name needs that server turned on again (PausedForeignTable).
const PausedCronTable = "sbctl_branch.paused_cron_jobs"

// pauseCronJobs records the active jobs of cron.job in PausedCronTable and deactivates them,
// in one transaction, and returns how many it paused. Running it again pauses nothing new and
// keeps the record.
func pauseCronJobs(ctx context.Context, c *pgx.Conn) (int, error) {
	tx, err := c.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, stmt := range []string{
		`create schema if not exists sbctl_branch`,
		`create table if not exists ` + PausedCronTable + ` (
			jobid     bigint primary key,
			jobname   text,
			schedule  text not null,
			database  text,
			username  text,
			paused_at timestamptz not null default now())`,
		`insert into ` + PausedCronTable + ` (jobid, jobname, schedule, database, username)
			select jobid, jobname, schedule, database, username from cron.job where active
			on conflict (jobid) do nothing`,
	} {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			return 0, err
		}
	}
	tag, err := tx.Exec(ctx, `update cron.job set active = false where active`)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), tx.Commit(ctx)
}

// resetCronNodes makes every job of cron.job run on the branch's own cluster. pg_cron stamps
// nodename and nodeport into a row when the job is scheduled (the parent's port, here), and the
// scheduler connects to them: left alone, a job that the branch's owner re-activates would run
// against the parent as the job's user. Returns the number of rows changed.
func resetCronNodes(ctx context.Context, c *pgx.Conn, host string, port int) (int, error) {
	var n int
	if err := c.QueryRow(ctx, `select count(*) from information_schema.columns where table_schema = 'cron' and table_name = 'job' and column_name in ('nodename', 'nodeport')`).Scan(&n); err != nil {
		return 0, err
	}
	if n != 2 {
		return 0, nil // an older pg_cron: jobs always run on this server
	}
	tag, err := c.Exec(ctx, `update cron.job set nodename = $1, nodeport = $2 where nodename is distinct from $1 or nodeport is distinct from $2`, host, port)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// connect opens a connection to dsn, to database db when it is not empty.
func connect(ctx context.Context, dsn, db string) (*pgx.Conn, error) {
	cc, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	if db != "" {
		cc.Database = db
	}
	return pgx.ConnectConfig(ctx, cc)
}

// isolateBranch is Service.isolate: it runs isolateCluster on the branch's running cluster,
// removes the first-start settings, and restarts the cluster so that it runs with the node's
// ordinary settings from then on.
func (s *Service) isolateBranch(ctx context.Context, ref string) error {
	p, err := s.reg.GetProject(ctx, ref)
	if err != nil {
		return err
	}
	egress := ""
	if p.Branch != nil {
		egress = p.Branch.Egress
	}
	res, err := isolateCluster(ctx, s.adminSocketDSN(ref, p.Seq), isolateOptions{
		KeepCron: s.keepsCron(egress), KeepForeign: egress == registry.EgressAllowed,
		NodeName: cronNodeName, NodePort: s.cfg.PortsFor(ref, p.Seq).Postgres,
	})
	if err != nil {
		return err
	}
	if err := clearQuarantine(filepath.Join(s.cfg.Paths().ProjectService(ref, config.SvcPostgres), "data")); err != nil {
		return fmt.Errorf("remove the first-start settings: %w", err)
	}
	if egress == registry.EgressPending {
		// From here on every render of the Postgres unit denies non-loopback traffic, so the
		// restart below brings the cluster up behind the filter. The cluster ran the first start
		// open, with the integrations silenced by the first-start settings instead: a base
		// backup restore may need the backup backend to finish recovery.
		if err := s.denyEgress(ctx, ref); err != nil {
			return fmt.Errorf("record the branch's egress policy: %w", err)
		}
		egress = registry.EgressDenied
	}
	res.Egress = egress
	// logical replication workers and the pg_net database are postmaster settings: only a
	// restart brings the branch back to the node's settings (new subscriptions work again).
	if err := s.eng.Pause(ctx, ref); err != nil {
		return fmt.Errorf("restart the cluster: %w", err)
	}
	if err := s.eng.Resume(ctx, ref); err != nil {
		return fmt.Errorf("restart the cluster: %w", err)
	}
	s.event(ctx, ref, "branch.isolated", res)
	return nil
}

// denyEgress records registry.EgressDenied on a branch whose policy was pending. It is a
// compare-and-set (SetBranchEgress), not a write of the whole branch row: a PATCH, a delete or a
// restore running at the same moment cannot put a stale policy back, and this cannot undo a
// reset that already replaced the policy. A branch that is denied already stays so.
func (s *Service) denyEgress(ctx context.Context, ref string) error {
	p, err := s.reg.GetProject(ctx, ref)
	if err != nil {
		return err
	}
	if p.Branch == nil {
		return fmt.Errorf("%s is not a branch", ref)
	}
	if p.Branch.Egress == registry.EgressDenied {
		return nil
	}
	return s.reg.SetBranchEgress(ctx, ref, p.Branch.Egress, registry.EgressDenied)
}
