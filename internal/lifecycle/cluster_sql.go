package lifecycle

import (
	"context"
	"fmt"
	"time"
)

// ClusterAddr says where a cluster's unix socket is: the directory and the port in the socket's name.
type ClusterAddr struct {
	Sock string
	Port int
}

func addrOf(pp pgPaths) ClusterAddr { return ClusterAddr{Sock: pp.Sock, Port: pp.Port} }

func (a ClusterAddr) dsn() string {
	return socketDSN(pgPaths{Sock: a.Sock, Port: a.Port}, "postgres")
}

// ClusterStatus is what a cluster says about its recovery.
type ClusterStatus struct {
	InRecovery bool
	// ReceiverStatus is pg_stat_wal_receiver.status, empty when there is no receiver.
	ReceiverStatus string
	// ReceiveLSN and ReplayLSN are empty on a cluster that is not in recovery.
	ReceiveLSN string
	ReplayLSN  string
	// ReplayAgeSeconds is the time since the last replayed commit; nil when none was replayed.
	ReplayAgeSeconds *float64
}

// ClusterSQL is the SQL the plane asks of a cluster to start it and to run the replica operations, as
// supabase_admin over its unix socket. The default talks to the cluster; tests replace it
// (PlaneOptions.ClusterSQL).
type ClusterSQL interface {
	// Ping runs "select 1": the cluster accepts connections.
	Ping(ctx context.Context, a ClusterAddr) error
	// Status reads the recovery state. It fails when the cluster does not answer on a.
	Status(ctx context.Context, a ClusterAddr) (ClusterStatus, error)
	// ReplayedTo reports whether replay has reached lsn and everything the receiver got.
	ReplayedTo(ctx context.Context, a ClusterAddr, lsn string) (bool, error)
	// ReplayLSN is pg_last_wal_replay_lsn().
	ReplayLSN(ctx context.Context, a ClusterAddr) (string, error)
	// RetryInterval is wal_retrieve_retry_interval: how long a standby waits before it asks the
	// archive for the next segment again.
	RetryInterval(ctx context.Context, a ClusterAddr) (time.Duration, error)
	// Promote runs pg_promote and waits up to wait for the promotion to finish.
	Promote(ctx context.Context, a ClusterAddr, wait time.Duration) error
	// Checkpoint runs CHECKPOINT.
	Checkpoint(ctx context.Context, a ClusterAddr) error
	// AlterSystem sets a server setting with ALTER SYSTEM (reset when value is empty) and reloads.
	AlterSystem(ctx context.Context, a ClusterAddr, name, value string) error
}

func (pl *PostgresPlane) sql() ClusterSQL {
	if pl.opts.ClusterSQL != nil {
		return pl.opts.ClusterSQL
	}
	return pgSQL{}
}

type pgSQL struct{}

func (pgSQL) Ping(ctx context.Context, a ClusterAddr) error {
	return ping(ctx, pgPaths{Sock: a.Sock, Port: a.Port})
}

func (pgSQL) Status(ctx context.Context, a ClusterAddr) (ClusterStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	c, err := connect(ctx, a.dsn())
	if err != nil {
		return ClusterStatus{}, err
	}
	defer c.Close(context.Background())
	var st ClusterStatus
	err = c.QueryRow(ctx, `select pg_is_in_recovery(),
		coalesce((select status from pg_stat_wal_receiver limit 1), ''),
		coalesce(pg_last_wal_receive_lsn()::text, ''),
		coalesce(pg_last_wal_replay_lsn()::text, ''),
		extract(epoch from now() - pg_last_xact_replay_timestamp())::float8`).
		Scan(&st.InRecovery, &st.ReceiverStatus, &st.ReceiveLSN, &st.ReplayLSN, &st.ReplayAgeSeconds)
	return st, err
}

func (pgSQL) ReplayedTo(ctx context.Context, a ClusterAddr, lsn string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	c, err := connect(ctx, a.dsn())
	if err != nil {
		return false, err
	}
	defer c.Close(context.Background())
	var ok *bool
	err = c.QueryRow(ctx, `select pg_last_wal_replay_lsn() >= $1::pg_lsn
		and pg_last_wal_replay_lsn() >= coalesce(pg_last_wal_receive_lsn(), '0/0'::pg_lsn)`, lsn).Scan(&ok)
	return ok != nil && *ok, err
}

func (pgSQL) ReplayLSN(ctx context.Context, a ClusterAddr) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	c, err := connect(ctx, a.dsn())
	if err != nil {
		return "", err
	}
	defer c.Close(context.Background())
	var lsn *string
	if err := c.QueryRow(ctx, `select pg_last_wal_replay_lsn()::text`).Scan(&lsn); err != nil {
		return "", err
	}
	if lsn == nil {
		return "", nil
	}
	return *lsn, nil
}

func (pgSQL) RetryInterval(ctx context.Context, a ClusterAddr) (time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	c, err := connect(ctx, a.dsn())
	if err != nil {
		return 0, err
	}
	defer c.Close(context.Background())
	var ms int64
	if err := c.QueryRow(ctx, `select setting::bigint from pg_settings where name = 'wal_retrieve_retry_interval'`).Scan(&ms); err != nil {
		return 0, err
	}
	return time.Duration(ms) * time.Millisecond, nil
}

func (pgSQL) Promote(ctx context.Context, a ClusterAddr, wait time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, wait+30*time.Second)
	defer cancel()
	c, err := connect(ctx, a.dsn())
	if err != nil {
		return err
	}
	defer c.Close(context.Background())
	var promoted bool
	if err := c.QueryRow(ctx, `select pg_promote(true, $1)`, int(wait.Seconds())).Scan(&promoted); err != nil {
		return fmt.Errorf("pg_promote: %w", err)
	}
	if !promoted {
		return fmt.Errorf("pg_promote: the promotion did not finish within %s", wait)
	}
	return nil
}

func (pgSQL) Checkpoint(ctx context.Context, a ClusterAddr) error {
	c, err := connect(ctx, a.dsn())
	if err != nil {
		return err
	}
	defer c.Close(context.Background())
	_, err = c.Exec(ctx, `checkpoint`)
	return err
}

func (pgSQL) AlterSystem(ctx context.Context, a ClusterAddr, name, value string) error {
	c, err := connect(ctx, a.dsn())
	if err != nil {
		return err
	}
	defer c.Close(context.Background())
	var stmt string
	if value == "" {
		err = c.QueryRow(ctx, `select format('alter system reset %I', $1::text)`, name).Scan(&stmt)
	} else {
		err = c.QueryRow(ctx, `select format('alter system set %I = %L', $1::text, $2::text)`, name, value).Scan(&stmt)
	}
	if err != nil {
		return err
	}
	if _, err := c.Exec(ctx, stmt); err != nil {
		return fmt.Errorf("lifecycle: %s: %w", stmt, err)
	}
	_, err = c.Exec(ctx, `select pg_reload_conf()`)
	return err
}
