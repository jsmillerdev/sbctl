package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var _ ClusterStore = (*Postgres)(nil)

// reloadTables are the tables a read-only registry reports as changed when change_seq moves.
var reloadTables = []string{"projects", "routes", "project_secrets", "nodes", "replicas"}

// mapWriteErr is mapErr for an insert or update that names rows of other tables: a foreign key
// violation there means the referenced row does not exist.
func mapWriteErr(err error) error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23503" {
		return fmt.Errorf("%w: %s", ErrNotFound, pe.ConstraintName)
	}
	return mapErr(err)
}

// subscribePoll is Subscribe for a read-only registry (a standby cannot LISTEN): it reads
// cluster.change_seq every readOnlyPoll and, when it moved, tells the consumer to reload each
// table. The channel closes when ctx ends or a poll fails.
func (r *Postgres) subscribePoll(ctx context.Context) (<-chan Change, error) {
	// A registry that had not run migration 1300 may have since: look again before giving up.
	if r.legacy.Load() {
		if err := r.probe(ctx); err != nil {
			return nil, err
		}
		if r.legacy.Load() {
			return nil, errors.New("registry: the registry has no change counter yet (migration 1300 has not run)")
		}
	}
	seq, err := r.changeSeq(ctx)
	if err != nil {
		return nil, err
	}
	ch := make(chan Change, len(reloadTables)*4)
	go func() {
		defer close(ch)
		tick := time.NewTicker(readOnlyPoll)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			cur, err := r.changeSeq(ctx)
			if err != nil {
				return
			}
			if cur == seq {
				continue
			}
			seq = cur
			for _, t := range reloadTables {
				select {
				case ch <- Change{Table: t, Op: "reload"}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return ch, nil
}

func (r *Postgres) changeSeq(ctx context.Context) (int64, error) {
	var seq int64
	err := r.pool.QueryRow(ctx, `select change_seq from supavise.cluster`).Scan(&seq)
	return seq, mapErr(err)
}

// Nodes

const nodeCols = `id, name, region, public_host, peer_addr, provider, version, state, cert_serial, joined_at`

func scanNode(row pgx.Row) (*Node, error) {
	var n Node
	var provider []byte
	if err := row.Scan(&n.ID, &n.Name, &n.Region, &n.PublicHost, &n.PeerAddr, &provider, &n.Version, &n.State, &n.CertSerial, &n.JoinedAt); err != nil {
		return nil, mapErr(err)
	}
	if err := json.Unmarshal(provider, &n.Provider); err != nil {
		return nil, err
	}
	return &n, nil
}

func (r *Postgres) CreateNode(ctx context.Context, n *Node) error {
	if n.State == "" {
		n.State = NodeJoining
	}
	if !n.State.valid() {
		return fmt.Errorf("registry: node state %q", n.State)
	}
	return pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		// Serialize id allocation.
		if _, err := tx.Exec(ctx, `lock table supavise.nodes in share row exclusive mode`); err != nil {
			return mapErr(err)
		}
		got, err := scanNode(tx.QueryRow(ctx, `
			insert into supavise.nodes (id, name, region, public_host, peer_addr, provider, version, state, cert_serial)
			values (coalesce(nullif($1, ''), (select 'n' || (coalesce(max(substr(id, 2)::int), 0) + 1) from supavise.nodes)),
				$2, $3, $4, $5, $6, $7, $8, $9) returning `+nodeCols,
			n.ID, n.Name, n.Region, n.PublicHost, n.PeerAddr, marshalJSON(n.Provider), n.Version, n.State, n.CertSerial))
		if err != nil {
			return err
		}
		*n = *got
		return nil
	})
}

func (r *Postgres) GetNode(ctx context.Context, id string) (*Node, error) {
	return scanNode(r.pool.QueryRow(ctx, `select `+nodeCols+` from supavise.nodes where id = $1`, id))
}

func (r *Postgres) GetNodeByName(ctx context.Context, name string) (*Node, error) {
	return scanNode(r.pool.QueryRow(ctx, `select `+nodeCols+` from supavise.nodes where name = $1`, name))
}

func (r *Postgres) ListNodes(ctx context.Context) ([]Node, error) {
	rows, err := r.pool.Query(ctx, `select `+nodeCols+` from supavise.nodes order by substr(id, 2)::int`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Node, error) {
		n, err := scanNode(row)
		if err != nil {
			return Node{}, err
		}
		return *n, nil
	})
}

// The setters below read the row first and write only a change. The change_seq trigger is a
// statement trigger, which fires even when an update matches no row, so a conditional update
// would still move the counter on every status report.

func (r *Postgres) UpdateNode(ctx context.Context, n *Node) error {
	cur, err := r.GetNode(ctx, n.ID)
	if err != nil {
		return err
	}
	if cur.Name != n.Name || cur.Region != n.Region || cur.PublicHost != n.PublicHost || cur.PeerAddr != n.PeerAddr ||
		cur.Version != n.Version || !reflect.DeepEqual(cur.Provider, n.Provider) {
		cur, err = scanNode(r.pool.QueryRow(ctx, `
			update supavise.nodes set name = $2, region = $3, public_host = $4, peer_addr = $5, provider = $6, version = $7
			where id = $1 returning `+nodeCols,
			n.ID, n.Name, n.Region, n.PublicHost, n.PeerAddr, marshalJSON(n.Provider), n.Version))
		if err != nil {
			return err
		}
	}
	*n = *cur
	return nil
}

func (r *Postgres) SetNodeState(ctx context.Context, id string, s NodeState) error {
	if !s.valid() {
		return fmt.Errorf("registry: node state %q", s)
	}
	cur, err := r.GetNode(ctx, id)
	if err != nil || cur.State == s {
		return err
	}
	return affected(r.pool.Exec(ctx, `update supavise.nodes set state = $2 where id = $1`, id, s))
}

func (r *Postgres) SetNodeCert(ctx context.Context, id, serial string) error {
	cur, err := r.GetNode(ctx, id)
	if err != nil || cur.CertSerial == serial {
		return err
	}
	return affected(r.pool.Exec(ctx, `update supavise.nodes set cert_serial = $2 where id = $1`, id, serial))
}

func (r *Postgres) DeleteNode(ctx context.Context, id string) error {
	return affected(r.pool.Exec(ctx, `delete from supavise.nodes where id = $1`, id))
}

// Cluster

func (r *Postgres) GetCluster(ctx context.Context) (*Cluster, error) {
	var c Cluster
	var addr, maint []byte
	err := r.pool.QueryRow(ctx, `select name, epoch, leader, service_address, maintenance, change_seq, updated_at from supavise.cluster`).
		Scan(&c.Name, &c.Epoch, &c.Leader, &addr, &maint, &c.ChangeSeq, &c.UpdatedAt)
	if err != nil {
		return nil, mapErr(err)
	}
	if err := json.Unmarshal(addr, &c.ServiceAddress); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(maint, &c.Maintenance); err != nil {
		return nil, err
	}
	return &c, nil
}

// updateCluster runs an update of the cluster row that also moves change_seq.
func (r *Postgres) updateCluster(ctx context.Context, set string, args ...any) error {
	_, err := r.pool.Exec(ctx, `update supavise.cluster set `+set+`, change_seq = change_seq + 1, updated_at = now()`, args...)
	return mapErr(err)
}

func (r *Postgres) SetClusterName(ctx context.Context, name string) error {
	return r.updateCluster(ctx, `name = $1`, name)
}

func (r *Postgres) SetServiceAddress(ctx context.Context, a ServiceAddress) error {
	return r.updateCluster(ctx, `service_address = $1`, marshalJSON(a))
}

func (r *Postgres) SetMaintenance(ctx context.Context, m Maintenance) error {
	return r.updateCluster(ctx, `maintenance = $1`, marshalJSON(m))
}

func (r *Postgres) SetLeader(ctx context.Context, node string, epoch int64) error {
	tag, err := r.pool.Exec(ctx, `
		update supavise.cluster set leader = $1, epoch = $2, change_seq = change_seq + 1, updated_at = now()
		where epoch < $2 or (epoch = $2 and leader = $1)`, node, epoch)
	if err != nil {
		return mapWriteErr(err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: epoch %d is not above the cluster's", ErrConflict, epoch)
	}
	return nil
}

// SetProjectNode implements ClusterStore. The project row is written before the replica row so
// that the order in which it takes locks matches the writers of projects (project, then the
// cluster row the change_seq trigger updates).
func (r *Postgres) SetProjectNode(ctx context.Context, ref, node string, epoch int64) error {
	return pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			update supavise.projects set node_id = $2, updated_at = now()
			where ref = $1
			  and (select epoch from supavise.cluster) = $3
			  and exists (select 1 from supavise.nodes where id = $2 and state = 'active')`, ref, node, epoch)
		if err != nil {
			return mapWriteErr(err)
		}
		if tag.RowsAffected() == 0 {
			return r.whyNotMoved(ctx, tx, ref, node, epoch)
		}
		_, err = tx.Exec(ctx, `delete from supavise.replicas where ref = $1 and node_id = $2`, ref, node)
		return mapErr(err)
	})
}

func (r *Postgres) whyNotMoved(ctx context.Context, tx pgx.Tx, ref, node string, epoch int64) error {
	var have int64
	var projectOK bool
	if err := tx.QueryRow(ctx, `select epoch, exists (select 1 from supavise.projects where ref = $1) from supavise.cluster`, ref).Scan(&have, &projectOK); err != nil {
		return mapErr(err)
	}
	if !projectOK {
		return ErrNotFound
	}
	if have != epoch {
		return fmt.Errorf("%w: epoch %d is not the cluster's %d", ErrConflict, epoch, have)
	}
	var state *string
	if err := tx.QueryRow(ctx, `select (select state from supavise.nodes where id = $1)`, node).Scan(&state); err != nil {
		return mapErr(err)
	}
	if state == nil {
		return ErrNotFound
	}
	return fmt.Errorf("%w: node %s is %s", ErrConflict, node, *state)
}

// Replicas

const replicaCols = `identifier, ref, node_id, origin, status, init_step, init_error, created_at, updated_at`

func scanReplica(row pgx.Row) (*Replica, error) {
	x, err := scanReplicaRaw(row)
	return x, mapErr(err)
}

// scanReplicaRaw is scanReplica that leaves the error as the driver returned it.
func scanReplicaRaw(row pgx.Row) (*Replica, error) {
	var x Replica
	if err := row.Scan(&x.Identifier, &x.Ref, &x.NodeID, &x.Origin, &x.Status, &x.InitStep, &x.InitError, &x.CreatedAt, &x.UpdatedAt); err != nil {
		return nil, err
	}
	return &x, nil
}

func collectReplicas(rows pgx.Rows, err error) ([]Replica, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Replica, error) {
		x, err := scanReplica(row)
		if err != nil {
			return Replica{}, err
		}
		return *x, nil
	})
}

func (r *Postgres) CreateReplica(ctx context.Context, x *Replica) error {
	if !replicaIDRe.MatchString(x.Identifier) {
		return fmt.Errorf("registry: replica identifier %q", x.Identifier)
	}
	if x.Origin == "" {
		x.Origin = ReplicaManual
	}
	switch x.Origin {
	case ReplicaManual, ReplicaDefault, ReplicaSystem:
	default:
		return fmt.Errorf("registry: replica origin %q", x.Origin)
	}
	if x.Status == "" {
		x.Status = ReplicaInit
	}
	if x.InitStep == "" {
		x.InitStep = ReplicaStepRequested
	}
	return pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		var home string
		if err := tx.QueryRow(ctx, `select node_id from supavise.projects where ref = $1 for share`, x.Ref).Scan(&home); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("registry: project %s: %w", x.Ref, ErrNotFound)
			}
			return err
		}
		if home == x.NodeID {
			return fmt.Errorf("%w: %s is the home of %s", ErrConflict, x.NodeID, x.Ref)
		}
		got, err := scanReplicaRaw(tx.QueryRow(ctx, `
			insert into supavise.replicas (identifier, ref, node_id, origin, status, init_step, init_error)
			values ($1, $2, $3, $4, $5, $6, $7) returning `+replicaCols,
			x.Identifier, x.Ref, x.NodeID, x.Origin, x.Status, x.InitStep, x.InitError))
		if err != nil {
			var pe *pgconn.PgError
			if errors.As(err, &pe) && pe.Code == "23503" {
				return fmt.Errorf("registry: node %s: %w", x.NodeID, ErrNotFound)
			}
			return mapErr(err)
		}
		*x = *got
		return nil
	})
}

func (r *Postgres) GetReplica(ctx context.Context, identifier string) (*Replica, error) {
	return scanReplica(r.pool.QueryRow(ctx, `select `+replicaCols+` from supavise.replicas where identifier = $1`, identifier))
}

func (r *Postgres) ListReplicas(ctx context.Context, ref string) ([]Replica, error) {
	return collectReplicas(r.pool.Query(ctx, `
		select `+replicaCols+` from supavise.replicas where $1 = '' or ref = $1 order by ref, created_at, identifier`, ref))
}

func (r *Postgres) ListReplicasOn(ctx context.Context, node string) ([]Replica, error) {
	return collectReplicas(r.pool.Query(ctx, `
		select `+replicaCols+` from supavise.replicas where node_id = $1 order by ref, created_at, identifier`, node))
}

func (r *Postgres) SetReplicaStatus(ctx context.Context, identifier, status, initStep, initError string) error {
	cur, err := r.GetReplica(ctx, identifier)
	if err != nil || (cur.Status == status && cur.InitStep == initStep && cur.InitError == initError) {
		return err
	}
	return affected(r.pool.Exec(ctx, `
		update supavise.replicas set status = $2, init_step = $3, init_error = $4, updated_at = now() where identifier = $1`,
		identifier, status, initStep, initError))
}

func (r *Postgres) DeleteReplica(ctx context.Context, identifier string) error {
	return affected(r.pool.Exec(ctx, `delete from supavise.replicas where identifier = $1`, identifier))
}

func (r *Postgres) PutReplicaOptout(ctx context.Context, ref, node string) error {
	_, err := r.pool.Exec(ctx, `insert into supavise.replica_optouts (ref, node_id) values ($1, $2) on conflict do nothing`, ref, node)
	return mapWriteErr(err)
}

func (r *Postgres) DeleteReplicaOptout(ctx context.Context, ref, node string) error {
	_, err := r.pool.Exec(ctx, `delete from supavise.replica_optouts where ref = $1 and node_id = $2`, ref, node)
	return mapErr(err)
}

func (r *Postgres) ListReplicaOptouts(ctx context.Context) ([]ReplicaOptout, error) {
	rows, err := r.pool.Query(ctx, `select ref, node_id from supavise.replica_optouts order by ref, node_id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (ReplicaOptout, error) {
		var o ReplicaOptout
		err := row.Scan(&o.Ref, &o.NodeID)
		return o, err
	})
}

// Join tokens

const tokenColsJoin = `id, secret_hash, node_name, created_at, expires_at, used_at`

func scanJoinToken(row pgx.Row) (*JoinToken, error) {
	var t JoinToken
	if err := row.Scan(&t.ID, &t.SecretHash, &t.NodeName, &t.CreatedAt, &t.ExpiresAt, &t.UsedAt); err != nil {
		return nil, mapErr(err)
	}
	return &t, nil
}

func (r *Postgres) CreateJoinToken(ctx context.Context, t *JoinToken) error {
	if t.ID == "" {
		return fmt.Errorf("registry: a join token needs an id")
	}
	got, err := scanJoinToken(r.pool.QueryRow(ctx, `
		insert into supavise.join_tokens (id, secret_hash, node_name, expires_at) values ($1, $2, $3, $4)
		returning `+tokenColsJoin, t.ID, t.SecretHash, t.NodeName, t.ExpiresAt))
	if err != nil {
		return err
	}
	*t = *got
	return nil
}

func (r *Postgres) GetJoinToken(ctx context.Context, id string) (*JoinToken, error) {
	return scanJoinToken(r.pool.QueryRow(ctx, `select `+tokenColsJoin+` from supavise.join_tokens where id = $1`, id))
}

func (r *Postgres) UseJoinToken(ctx context.Context, id string, at time.Time) (*JoinToken, error) {
	t, err := scanJoinToken(r.pool.QueryRow(ctx, `
		update supavise.join_tokens set used_at = $2
		where id = $1 and used_at is null and expires_at > $2 returning `+tokenColsJoin, id, at))
	if err == nil {
		return t, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	cur, err := r.GetJoinToken(ctx, id)
	switch {
	case err != nil:
		return nil, err
	case cur.UsedAt != nil:
		return nil, ErrTokenUsed
	}
	return nil, ErrTokenExpired
}

func (r *Postgres) DeleteExpiredJoinTokens(ctx context.Context, before time.Time) (int, error) {
	tag, err := r.pool.Exec(ctx, `delete from supavise.join_tokens where expires_at < $1`, before)
	return int(tag.RowsAffected()), mapErr(err)
}

// Moves

const moveCols = `id, scope, kind, coalesce(ref, ''), from_node, to_node, epoch, state, steps, error, started_at, ended_at`

func scanMove(row pgx.Row) (*Move, error) {
	var m Move
	var steps []byte
	if err := row.Scan(&m.ID, &m.Scope, &m.Kind, &m.Ref, &m.FromNode, &m.ToNode, &m.Epoch, &m.State, &steps, &m.Error, &m.StartedAt, &m.EndedAt); err != nil {
		return nil, mapErr(err)
	}
	if err := json.Unmarshal(steps, &m.Steps); err != nil {
		return nil, err
	}
	if m.Steps == nil {
		m.Steps = []MoveStep{}
	}
	return &m, nil
}

func (r *Postgres) CreateMove(ctx context.Context, m *Move) error {
	got, err := scanMove(r.pool.QueryRow(ctx, `
		insert into supavise.moves (scope, kind, ref, from_node, to_node, epoch)
		values ($1, $2, $3, $4, $5, $6) returning `+moveCols,
		m.Scope, m.Kind, nullStr(m.Ref), m.FromNode, m.ToNode, m.Epoch))
	if err != nil {
		return err
	}
	*m = *got
	return nil
}

func (r *Postgres) GetMove(ctx context.Context, id int64) (*Move, error) {
	return scanMove(r.pool.QueryRow(ctx, `select `+moveCols+` from supavise.moves where id = $1`, id))
}

func (r *Postgres) ListMoves(ctx context.Context, state MoveState, limit int) ([]Move, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := r.pool.Query(ctx, `
		select `+moveCols+` from supavise.moves where $1 = '' or state = $1 order by id desc limit $2`, string(state), limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Move, error) {
		m, err := scanMove(row)
		if err != nil {
			return Move{}, err
		}
		return *m, nil
	})
}

func (r *Postgres) AppendMoveStep(ctx context.Context, id int64, s MoveStep) error {
	return affected(r.pool.Exec(ctx, `update supavise.moves set steps = steps || $2::jsonb where id = $1`, id, marshalJSON([]MoveStep{s})))
}

func (r *Postgres) FinishMove(ctx context.Context, id int64, state MoveState, errText string) error {
	if !state.valid() || state == MoveRunning {
		return fmt.Errorf("registry: a move finishes as done, failed or aborted, not %q", state)
	}
	return affected(r.pool.Exec(ctx, `update supavise.moves set state = $2, error = $3, ended_at = now() where id = $1`, id, string(state), errText))
}
