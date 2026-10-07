package branching

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
}

// isolateCluster neutralizes the parent's outbound integrations in every database of the
// cluster reachable at dsn (a superuser on the cluster's private socket). It is safe to run
// twice.
func isolateCluster(ctx context.Context, dsn string, keepCron bool) (IsolateResult, error) {
	var res IsolateResult
	res.CronKept = keepCron
	c, err := connect(ctx, dsn, "")
	if err != nil {
		return res, err
	}
	rows, err := c.Query(ctx, `select datname from pg_database where datallowconn and not datistemplate order by datname`)
	var dbs []string
	if err == nil {
		dbs, err = pgx.CollectRows(rows, pgx.RowTo[string])
	}
	closeConn(c)
	if err != nil {
		return res, err
	}
	for _, db := range dbs {
		if err := isolateDatabase(ctx, dsn, db, keepCron, &res); err != nil {
			return res, fmt.Errorf("database %s: %w", db, err)
		}
	}
	return res, nil
}

func isolateDatabase(ctx context.Context, dsn, db string, keepCron bool, res *IsolateResult) error {
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
	if !keepCron {
		var has bool
		if err := c.QueryRow(ctx, `select to_regclass('cron.job') is not null`).Scan(&has); err != nil {
			return err
		}
		if has {
			n, err := pauseCronJobs(ctx, c)
			if err != nil {
				return fmt.Errorf("pause cron jobs: %w", err)
			}
			res.CronJobs += n
		}
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
	res, err := isolateCluster(ctx, s.adminSocketDSN(ref, p.Seq), s.keepsCron(egress))
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

// denyEgress records registry.EgressDenied on a branch whose policy was pending.
func (s *Service) denyEgress(ctx context.Context, ref string) error {
	p, err := s.reg.GetProject(ctx, ref)
	if err != nil {
		return err
	}
	if p.Branch == nil {
		return fmt.Errorf("%s is not a branch", ref)
	}
	b := *p.Branch
	b.Egress = registry.EgressDenied
	return s.reg.UpdateBranch(ctx, ref, &b)
}
