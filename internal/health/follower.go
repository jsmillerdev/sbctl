package health

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/units"
)

// A node that follows the leader of its cluster has the system cluster as a hot standby, on the
// replica port: the canonical port is a forwarder to the leader's cluster, and the system GoTrue is
// the leader's to run. The checks of a leader would call the standby unreachable and GoTrue dead.

// systemStandby reports whether the system cluster's data directory on this node is a standby's
// (the boot decision tells a follower by the same file).
func systemStandby(cfg *config.Config) bool {
	_, err := os.Stat(filepath.Join(cfg.Paths().PostgresData(config.SystemRef), "standby.signal"))
	return err == nil
}

// followerSystem is Deps.System of a follower: the system cluster's unit is active and its standby
// answers on its own socket and is in recovery. GoTrue is not probed (checkNode shows it parked).
func followerSystem(cfg *config.Config, sup units.Supervisor) func(ctx context.Context) []lifecycle.ServiceHealth {
	return func(ctx context.Context) []lifecycle.ServiceHealth {
		h := lifecycle.ServiceHealth{Name: config.SvcPostgres}
		st, err := sup.Status(ctx, config.UnitName(config.SvcPostgres, config.SystemRef))
		switch {
		case err != nil:
			h.Status, h.Error = "UNHEALTHY", err.Error()
		case st.State != units.StateActive:
			h.Status, h.Error = "UNHEALTHY", fmt.Sprintf("unit is %s/%s", st.State, st.SubState)
			if st.State == units.StateActivating {
				h.Status = "COMING_UP"
			}
		default:
			if err := standbyAnswers(ctx, cfg); err != nil {
				h.Status, h.Error = "UNHEALTHY", err.Error()
			} else {
				h.Healthy, h.Status = true, "ACTIVE_HEALTHY"
			}
		}
		return []lifecycle.ServiceHealth{h}
	}
}

// standbyAnswers connects to the system cluster's standby and checks that it is in recovery.
func standbyAnswers(ctx context.Context, cfg *config.Config) error {
	// FollowerRegistryDSN carries a pool setting that a single connection does not know.
	var kv []string
	for _, f := range strings.Fields(lifecycle.FollowerRegistryDSN(cfg)) {
		if !strings.HasPrefix(f, "pool_") {
			kv = append(kv, f)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, strings.Join(kv, " "))
	if err != nil {
		return err
	}
	defer conn.Close(context.WithoutCancel(ctx))
	var inRecovery bool
	if err := conn.QueryRow(ctx, "select pg_is_in_recovery()").Scan(&inRecovery); err != nil {
		return err
	}
	if !inRecovery {
		return errors.New("the system cluster of a follower is not in recovery")
	}
	return nil
}
