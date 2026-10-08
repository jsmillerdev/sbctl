package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/replicas"
)

// Read replicas. A project's replicas are standby databases on other servers of the cluster; the
// replica controller (internal/replicas) runs them and this file is the Management API in front
// of it. Studio's Infrastructure page adds and removes a replica through the two v1 routes below,
// lists them through GET databases (databases.go) and follows a new one through databases-statuses.
//
// The routes ask the controller through replicas.Service, and the placement Resolver for which
// replicas a project has. A node without a controller (Deps.Replicas nil) lists the primary alone
// and refuses to add a replica.

func (s *Server) routesReplicas(add func(string, handlerFunc)) {
	add("POST /v1/projects/{ref}/read-replicas/setup", s.setupReadReplica)
	add("POST /v1/projects/{ref}/read-replicas/remove", s.removeReadReplica)
}

// errNoReplicas answers a request to change replicas on a node that has no replica controller.
var errNoReplicas = errf(http.StatusServiceUnavailable, "Read replicas are not set up on this Supavise server")

// replicaCap is the most replicas a project of this size may have, as Studio's
// READ_REPLICA_COMPUTE_CAPS and READ_REPLICAS_MAX_COUNT say: none up to Micro, four for Small to
// Large, five above. Studio disables its Add button at the cap, and the API refuses the same.
func replicaCap(p *registry.Project) int {
	switch sizeOf(p).Name {
	case "nano", "micro":
		return 0
	case "small", "medium", "large":
		return 4
	}
	return 5
}

// replicaServiceErr turns an error of the replica controller into an API error: a refusal it
// words for the person (a *replicas.UserError) is a 400 with that text, an unknown identifier a 404.
func replicaServiceErr(err error) error {
	var ue *replicas.UserError
	switch {
	case errors.As(err, &ue):
		return errf(http.StatusBadRequest, "%s", ue.Msg)
	case errors.Is(err, replicas.ErrNotFound):
		return errf(http.StatusNotFound, "Read replica not found")
	}
	return mapErr(err)
}

// setupReadReplica adds a replica of the project in a region. The checks here are the ones that
// concern the project (state, size, how many it has, whether its port sequence leaves room for a
// replica port); the controller checks the cluster (a joined server in the region, capacity,
// storage) and writes the row. The replica comes up afterwards; databases-statuses shows the steps.
func (s *Server) setupReadReplica(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Region string `json:"read_replica_region"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	if strings.TrimSpace(in.Region) == "" {
		return errf(http.StatusBadRequest, "read_replica_region is required")
	}
	p, err := s.running(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	if s.replicas == nil {
		return errNoReplicas
	}
	if err := s.replicaRoom(r.Context(), p); err != nil {
		return err
	}
	ctx, cancel, err := s.detach(r, lifecycleTimeout)
	if err != nil {
		return err
	}
	defer cancel()
	if err := s.replicas.Setup(ctx, p.Ref, strings.TrimSpace(in.Region)); err != nil {
		return replicaServiceErr(err)
	}
	writeNoContent(w)
	return nil
}

// replicaRoom refuses a replica the project cannot have: a branch, a size below Small, the cap of
// its size, and a port sequence whose replica ports would meet the project range.
func (s *Server) replicaRoom(ctx context.Context, p *registry.Project) error {
	if p.Branch != nil {
		return errf(http.StatusBadRequest, "Read replicas are not available for branches.")
	}
	limit := replicaCap(p)
	if limit == 0 {
		return errf(http.StatusBadRequest, "Read replicas need a compute size of small or larger.")
	}
	have, err := s.placement.ReplicasOf(ctx, p.Ref)
	if err != nil {
		return mapErr(err)
	}
	if len(have) >= limit {
		return errf(http.StatusBadRequest, "The project already has the maximum of %d read replicas.", limit)
	}
	if err := s.cfg.CheckReplicaPorts(); err != nil {
		return errf(http.StatusBadRequest, "%v", err)
	}
	if p.Seq > s.cfg.MaxReplicaSeq() {
		return errf(http.StatusBadRequest, "This project cannot have a read replica: its port sequence %d is above %d, the last one with room for a replica port.", p.Seq, s.cfg.MaxReplicaSeq())
	}
	return nil
}

// removeReadReplica removes one replica of the project; the controller stops it and deletes its
// data. An identifier that is not a replica of the project, the primary's included, is a 404.
func (s *Server) removeReadReplica(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Identifier string `json:"database_identifier"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	if in.Identifier == "" {
		return errf(http.StatusBadRequest, "database_identifier is required")
	}
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	if s.replicas == nil {
		return errNoReplicas
	}
	ctx, cancel, err := s.detach(r, lifecycleTimeout)
	if err != nil {
		return err
	}
	defer cancel()
	if err := s.replicas.Remove(ctx, p.Ref, in.Identifier); err != nil {
		return replicaServiceErr(err)
	}
	writeNoContent(w)
	return nil
}

// restartReplica restarts one replica's services on its node (POST restart with a
// database_identifier). Restarting the project itself restarts the primary only.
func (s *Server) restartReplica(w http.ResponseWriter, r *http.Request, p *registry.Project, identifier string, status int) error {
	if s.replicas == nil {
		return errNoReplicas
	}
	ctx, cancel, err := s.detach(r, 2*lifecycleTimeout)
	if err != nil {
		return err
	}
	defer cancel()
	if err := s.replicas.Restart(ctx, p.Ref, identifier); err != nil {
		return replicaServiceErr(err)
	}
	w.WriteHeader(status)
	return nil
}

// replicasOf lists the project's replicas with where they run, oldest first. A node without a
// controller has none.
func (s *Server) replicasOf(ctx context.Context, ref string) ([]replicas.Replica, error) {
	if s.replicas == nil {
		return nil, nil
	}
	rs, err := s.replicas.List(ctx, ref)
	if err != nil {
		return nil, mapErr(err)
	}
	return rs, nil
}

// replicaOf finds identifier among the project's replicas by the placement Resolver, which reads
// the registry and so answers from the same rows on a follower. ok is false when it is not one.
func (s *Server) replicaOf(ctx context.Context, ref, identifier string) (rep registry.Replica, ok bool, err error) {
	rs, err := s.placement.ReplicasOf(ctx, ref)
	if err != nil {
		return rep, false, mapErr(err)
	}
	i := slices.IndexFunc(rs, func(r registry.Replica) bool { return r.Identifier == identifier })
	if i < 0 {
		return rep, false, nil
	}
	return rs[i], true, nil
}

// replicasByRef reads the replicas of a page of projects in one query, for the listings that show
// many projects at once; the Resolver answers one project at a time. Both read the registry's
// replicas, so a project has the same replicas by either. A node without a controller lists the
// primary alone, as replicasOf does, and reads nothing.
func (s *Server) replicasByRef(ctx context.Context, ps []registry.Project) (map[string][]registry.Replica, error) {
	if s.replicas == nil || len(ps) == 0 {
		return nil, nil
	}
	all, err := s.reg.ListReplicas(ctx, "")
	if err != nil {
		return nil, mapErr(err)
	}
	listed := make(map[string]bool, len(ps))
	for _, p := range ps {
		listed[p.Ref] = true
	}
	out := map[string][]registry.Replica{}
	for _, rep := range all {
		if listed[rep.Ref] {
			out[rep.Ref] = append(out[rep.Ref], rep)
		}
	}
	return out, nil
}

// nodeRegions maps node ids to the region shown for what runs on them: the node's own, else the
// node-wide one.
func (s *Server) nodeRegions(ctx context.Context) (map[string]string, error) {
	nodes, err := s.reg.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(nodes))
	for _, n := range nodes {
		out[n.ID] = s.cfg.ProjectRegion(n.Region)
	}
	return out, nil
}

// replicasOffered is whether Studio should offer read replicas: a controller is wired and a second
// server has joined. It is what takes infrastructure:read_replicas out of disabled_features, so a
// node on its own shows no replica screens that could only answer "no server is joined".
func (s *Server) replicasOffered(ctx context.Context) bool {
	if s.replicas == nil {
		return false
	}
	nodes, err := s.reg.ListNodes(ctx)
	if err != nil {
		return false
	}
	active := 0
	for _, n := range nodes {
		if n.State == registry.NodeActive {
			active++
		}
	}
	return active > 1
}
