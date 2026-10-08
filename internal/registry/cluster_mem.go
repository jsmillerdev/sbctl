package registry

import (
	"bytes"
	"context"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"time"
)

var _ ClusterStore = (*Memory)(nil)

// seedCluster gives a new in-memory registry what migration 1300 gives a database: the founder
// node and the cluster row.
func (m *Memory) seedCluster() {
	now := time.Now()
	m.nodes = map[string]Node{FounderNodeID: {ID: FounderNodeID, Name: "primary", State: NodeActive, JoinedAt: now}}
	m.cluster = Cluster{Epoch: 1, Leader: FounderNodeID, UpdatedAt: now}
	m.replicas = map[string]Replica{}
	m.joinTokens = map[string]JoinToken{}
}

// now is the time a node row is stamped with: Now when a test set it, else the wall clock.
func (m *Memory) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func nodeNumber(id string) int {
	n, _ := strconv.Atoi(id[1:])
	return n
}

// bumpCluster is what the cluster setters do to change_seq. Must be called with m.mu held.
func (m *Memory) bumpCluster() {
	m.cluster.ChangeSeq++
	m.cluster.UpdatedAt = time.Now()
}

func (m *Memory) CreateNode(_ context.Context, n *Node) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !nodeNameRe.MatchString(n.Name) {
		return fmt.Errorf("registry: node name %q must be lower case letters, digits and hyphens", n.Name)
	}
	id := n.ID
	if id == "" {
		next := 1
		for k := range m.nodes {
			next = max(next, nodeNumber(k)+1)
		}
		id = "n" + strconv.Itoa(next)
	}
	if !nodeIDRe.MatchString(id) {
		return fmt.Errorf("registry: node id %q must be n and a number", id)
	}
	if n.State == "" {
		n.State = NodeJoining
	}
	if !n.State.valid() {
		return fmt.Errorf("registry: node state %q", n.State)
	}
	if _, ok := m.nodes[id]; ok {
		return fmt.Errorf("%w: nodes_pkey", ErrConflict)
	}
	for _, o := range m.nodes {
		if o.Name == n.Name {
			return fmt.Errorf("%w: nodes_name_key", ErrConflict)
		}
	}
	n.ID, n.JoinedAt = id, m.now()
	m.nodes[id] = n.clone()
	m.notify("nodes", "insert", id)
	return nil
}

func (m *Memory) GetNode(_ context.Context, id string) (*Node, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.nodes[id]
	if !ok {
		return nil, ErrNotFound
	}
	n = n.clone()
	return &n, nil
}

func (m *Memory) GetNodeByName(_ context.Context, name string) (*Node, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, n := range m.nodes {
		if n.Name == name {
			n = n.clone()
			return &n, nil
		}
	}
	return nil, ErrNotFound
}

func (m *Memory) ListNodes(context.Context) ([]Node, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Node, 0, len(m.nodes))
	for _, n := range m.nodes {
		out = append(out, n.clone())
	}
	sort.Slice(out, func(i, j int) bool { return nodeNumber(out[i].ID) < nodeNumber(out[j].ID) })
	return out, nil
}

func (m *Memory) UpdateNode(_ context.Context, n *Node) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.nodes[n.ID]
	if !ok {
		return ErrNotFound
	}
	if !nodeNameRe.MatchString(n.Name) {
		return fmt.Errorf("registry: node name %q must be lower case letters, digits and hyphens", n.Name)
	}
	for _, o := range m.nodes {
		if o.ID != n.ID && o.Name == n.Name {
			return fmt.Errorf("%w: nodes_name_key", ErrConflict)
		}
	}
	next := cur
	next.Name, next.Region, next.PublicHost, next.PeerAddr, next.Provider, next.Version = n.Name, n.Region, n.PublicHost, n.PeerAddr, n.clone().Provider, n.Version
	if !reflect.DeepEqual(next, cur) {
		m.nodes[n.ID] = next
		m.notify("nodes", "update", n.ID)
	}
	*n = m.nodes[n.ID].clone()
	return nil
}

func (m *Memory) SetNodeState(_ context.Context, id string, s NodeState) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !s.valid() {
		return fmt.Errorf("registry: node state %q", s)
	}
	n, ok := m.nodes[id]
	if !ok {
		return ErrNotFound
	}
	if n.State != s {
		n.State = s
		if s == NodeJoining {
			n.JoinedAt = m.now()
		}
		m.nodes[id] = n
		m.notify("nodes", "update", id)
	}
	return nil
}

func (m *Memory) SetNodeCert(_ context.Context, id, serial string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.nodes[id]
	if !ok {
		return ErrNotFound
	}
	if n.CertSerial != serial {
		n.CertSerial = serial
		m.nodes[id] = n
		m.notify("nodes", "update", id)
	}
	return nil
}

func (m *Memory) DeleteNode(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.nodes[id]; !ok {
		return ErrNotFound
	}
	if m.cluster.Leader == id {
		return fmt.Errorf("%w: cluster_leader_fkey", ErrConflict)
	}
	for _, p := range m.projects {
		if p.NodeID == id {
			return fmt.Errorf("%w: projects_node_id_fkey", ErrConflict)
		}
	}
	for _, r := range m.replicas {
		if r.NodeID == id {
			return fmt.Errorf("%w: replicas_node_id_fkey", ErrConflict)
		}
	}
	delete(m.nodes, id)
	kept := m.optouts[:0]
	for _, o := range m.optouts {
		if o.NodeID != id {
			kept = append(kept, o)
		}
	}
	m.optouts = kept
	m.notify("nodes", "delete", id)
	return nil
}

func (m *Memory) GetCluster(context.Context) (*Cluster, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.cluster
	return &c, nil
}

func (m *Memory) SetClusterName(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cluster.Name = name
	m.bumpCluster()
	return nil
}

func (m *Memory) SetServiceAddress(_ context.Context, a ServiceAddress) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cluster.ServiceAddress = a
	m.bumpCluster()
	return nil
}

func (m *Memory) SetMaintenance(_ context.Context, mt Maintenance) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cluster.Maintenance = mt
	m.bumpCluster()
	return nil
}

func (m *Memory) SetLeader(_ context.Context, node string, epoch int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.nodes[node]; !ok {
		return ErrNotFound
	}
	if !(epoch > m.cluster.Epoch || (epoch == m.cluster.Epoch && node == m.cluster.Leader)) {
		return fmt.Errorf("%w: epoch %d is not above %d", ErrConflict, epoch, m.cluster.Epoch)
	}
	m.cluster.Leader, m.cluster.Epoch = node, epoch
	m.bumpCluster()
	return nil
}

func (m *Memory) SetProjectNode(_ context.Context, ref, node string, epoch int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.projects[ref]
	if !ok {
		return ErrNotFound
	}
	if epoch != m.cluster.Epoch {
		return fmt.Errorf("%w: epoch %d is not the cluster's %d", ErrConflict, epoch, m.cluster.Epoch)
	}
	n, ok := m.nodes[node]
	if !ok {
		return ErrNotFound
	}
	if n.State != NodeActive {
		return fmt.Errorf("%w: node %s is %s", ErrConflict, node, n.State)
	}
	p.NodeID, p.UpdatedAt = node, time.Now()
	m.projects[ref] = p
	for id, r := range m.replicas {
		if r.Ref == ref && r.NodeID == node {
			delete(m.replicas, id)
			m.notify("replicas", "delete", id)
		}
	}
	m.notify("projects", "update", ref)
	return nil
}

func (m *Memory) CreateReplica(_ context.Context, r *Replica) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !replicaIDRe.MatchString(r.Identifier) {
		return fmt.Errorf("registry: replica identifier %q", r.Identifier)
	}
	p, ok := m.projects[r.Ref]
	if !ok {
		return fmt.Errorf("registry: project %s: %w", r.Ref, ErrNotFound)
	}
	if _, ok := m.nodes[r.NodeID]; !ok {
		return fmt.Errorf("registry: node %s: %w", r.NodeID, ErrNotFound)
	}
	if p.NodeID == r.NodeID {
		return fmt.Errorf("%w: %s is the home of %s", ErrConflict, r.NodeID, r.Ref)
	}
	for _, o := range m.replicas {
		if o.Identifier == r.Identifier {
			return fmt.Errorf("%w: replicas_pkey", ErrConflict)
		}
		if o.Ref == r.Ref && o.NodeID == r.NodeID {
			return fmt.Errorf("%w: replicas_ref_node_id_key", ErrConflict)
		}
	}
	if r.Origin == "" {
		r.Origin = ReplicaManual
	}
	switch r.Origin {
	case ReplicaManual, ReplicaDefault, ReplicaSystem:
	default:
		return fmt.Errorf("registry: replica origin %q", r.Origin)
	}
	if r.Status == "" {
		r.Status = ReplicaInit
	}
	if r.InitStep == "" {
		r.InitStep = ReplicaStepRequested
	}
	now := time.Now()
	r.CreatedAt, r.UpdatedAt = now, now
	m.replicas[r.Identifier] = *r
	m.notify("replicas", "insert", r.Identifier)
	return nil
}

func (m *Memory) GetReplica(_ context.Context, identifier string) (*Replica, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.replicas[identifier]
	if !ok {
		return nil, ErrNotFound
	}
	return &r, nil
}

func (m *Memory) listReplicas(keep func(Replica) bool) []Replica {
	out := []Replica{}
	for _, r := range m.replicas {
		if keep(r) {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Ref != b.Ref {
			return a.Ref < b.Ref
		}
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.Before(b.CreatedAt)
		}
		return a.Identifier < b.Identifier
	})
	return out
}

func (m *Memory) ListReplicas(_ context.Context, ref string) ([]Replica, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.listReplicas(func(r Replica) bool { return ref == "" || r.Ref == ref }), nil
}

func (m *Memory) ListReplicasOn(_ context.Context, node string) ([]Replica, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.listReplicas(func(r Replica) bool { return r.NodeID == node }), nil
}

func (m *Memory) SetReplicaStatus(_ context.Context, identifier, status, initStep, initError string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.replicas[identifier]
	if !ok {
		return ErrNotFound
	}
	if r.Status != status || r.InitStep != initStep || r.InitError != initError {
		r.Status, r.InitStep, r.InitError, r.UpdatedAt = status, initStep, initError, time.Now()
		m.replicas[identifier] = r
		m.notify("replicas", "update", identifier)
	}
	return nil
}

func (m *Memory) DeleteReplica(_ context.Context, identifier string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.replicas[identifier]; !ok {
		return ErrNotFound
	}
	delete(m.replicas, identifier)
	m.notify("replicas", "delete", identifier)
	return nil
}

func (m *Memory) PutReplicaOptout(_ context.Context, ref, node string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.projects[ref]; !ok {
		return fmt.Errorf("registry: project %s: %w", ref, ErrNotFound)
	}
	if _, ok := m.nodes[node]; !ok {
		return fmt.Errorf("registry: node %s: %w", node, ErrNotFound)
	}
	o := ReplicaOptout{Ref: ref, NodeID: node}
	for _, x := range m.optouts {
		if x == o {
			return nil
		}
	}
	m.optouts = append(m.optouts, o)
	return nil
}

func (m *Memory) DeleteReplicaOptout(_ context.Context, ref, node string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	kept := m.optouts[:0]
	for _, o := range m.optouts {
		if o != (ReplicaOptout{Ref: ref, NodeID: node}) {
			kept = append(kept, o)
		}
	}
	m.optouts = kept
	return nil
}

func (m *Memory) ListReplicaOptouts(context.Context) ([]ReplicaOptout, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := append([]ReplicaOptout{}, m.optouts...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Ref != out[j].Ref {
			return out[i].Ref < out[j].Ref
		}
		return out[i].NodeID < out[j].NodeID
	})
	return out, nil
}

func cloneToken(t JoinToken) JoinToken {
	t.SecretHash = bytes.Clone(t.SecretHash)
	if t.UsedAt != nil {
		u := *t.UsedAt
		t.UsedAt = &u
	}
	return t
}

func (m *Memory) CreateJoinToken(_ context.Context, t *JoinToken) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t.ID == "" {
		return fmt.Errorf("registry: a join token needs an id")
	}
	if _, ok := m.joinTokens[t.ID]; ok {
		return fmt.Errorf("%w: join_tokens_pkey", ErrConflict)
	}
	t.CreatedAt, t.UsedAt = time.Now(), nil
	m.joinTokens[t.ID] = cloneToken(*t)
	return nil
}

func (m *Memory) GetJoinToken(_ context.Context, id string) (*JoinToken, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.joinTokens[id]
	if !ok {
		return nil, ErrNotFound
	}
	t = cloneToken(t)
	return &t, nil
}

func (m *Memory) UseJoinToken(_ context.Context, id string, at time.Time) (*JoinToken, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.joinTokens[id]
	switch {
	case !ok:
		return nil, ErrNotFound
	case t.UsedAt != nil:
		return nil, ErrTokenUsed
	case !at.Before(t.ExpiresAt):
		return nil, ErrTokenExpired
	}
	t.UsedAt = &at
	m.joinTokens[id] = cloneToken(t)
	t = cloneToken(t)
	return &t, nil
}

func (m *Memory) DeleteExpiredJoinTokens(_ context.Context, before time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for id, t := range m.joinTokens {
		if t.ExpiresAt.Before(before) {
			delete(m.joinTokens, id)
			n++
		}
	}
	return n, nil
}

func cloneMove(mv Move) Move {
	mv.Steps = append([]MoveStep{}, mv.Steps...)
	if mv.EndedAt != nil {
		e := *mv.EndedAt
		mv.EndedAt = &e
	}
	return mv
}

func (m *Memory) CreateMove(_ context.Context, mv *Move) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if mv.Scope != MoveProject && mv.Scope != MoveServer {
		return fmt.Errorf("registry: move scope %q", mv.Scope)
	}
	if mv.Kind != MoveSwitchover && mv.Kind != MoveFailover {
		return fmt.Errorf("registry: move kind %q", mv.Kind)
	}
	mv.ID, mv.State, mv.Steps, mv.Error, mv.StartedAt, mv.EndedAt = m.id(), MoveRunning, []MoveStep{}, "", time.Now(), nil
	m.moves = append(m.moves, cloneMove(*mv))
	return nil
}

func (m *Memory) move(id int64) (int, bool) {
	for i := range m.moves {
		if m.moves[i].ID == id {
			return i, true
		}
	}
	return 0, false
}

func (m *Memory) GetMove(_ context.Context, id int64) (*Move, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	i, ok := m.move(id)
	if !ok {
		return nil, ErrNotFound
	}
	mv := cloneMove(m.moves[i])
	return &mv, nil
}

func (m *Memory) ListMoves(_ context.Context, state MoveState, limit int) ([]Move, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if limit <= 0 {
		limit = 100
	}
	out := []Move{}
	for i := len(m.moves) - 1; i >= 0 && len(out) < limit; i-- {
		if state == "" || m.moves[i].State == state {
			out = append(out, cloneMove(m.moves[i]))
		}
	}
	return out, nil
}

func (m *Memory) AppendMoveStep(_ context.Context, id int64, s MoveStep) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	i, ok := m.move(id)
	if !ok {
		return ErrNotFound
	}
	m.moves[i].Steps = append(m.moves[i].Steps, s)
	return nil
}

func (m *Memory) FinishMove(_ context.Context, id int64, state MoveState, errText string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !state.valid() || state == MoveRunning {
		return fmt.Errorf("registry: a move finishes as done, failed or aborted, not %q", state)
	}
	i, ok := m.move(id)
	if !ok {
		return ErrNotFound
	}
	now := time.Now()
	m.moves[i].State, m.moves[i].Error, m.moves[i].EndedAt = state, errText, &now
	return nil
}
