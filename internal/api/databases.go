package api

import (
	"fmt"
	"net/http"
	"time"

	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/replicas"
)

// The databases of a project: its primary and its read replicas. Studio lists them in the
// Infrastructure page, the SQL editor's source selector and the reports; it reads the primary as
// the row whose identifier is the project's ref.

func (s *Server) routesDatabases(add func(string, handlerFunc)) {
	add("GET /platform/projects/{ref}/databases", s.listDatabases)
	add("GET /platform/projects/{ref}/databases-statuses", s.databasesStatuses)
}

// readOnlyUser is the user in a database's read-only connection string. Hosted's name for it:
// Studio's reports pass that string back to pg-meta, which reads it as a selector only (pgmeta.go).
const readOnlyUser = "supabase_read_only_user"

// dbString is a placeholder connection string to the database called identifier (a ref or a
// replica's identifier) as user. The password is never in it: Studio sends the string back to
// pg-meta, which names the database by its host and connects with credentials of its own.
func (s *Server) dbString(identifier, user string) string {
	return fmt.Sprintf("postgresql://%s:[YOUR-PASSWORD]@%s:%d/postgres", user, s.dbHost(identifier), s.cfg.Ports.SupavisorSession)
}

// databaseStatus is a project status as the database schemas spell it. They have no INACTIVE,
// PAUSING, UPGRADING or failed-restore values, so a paused project reads as unknown, a pausing
// one as going down and an upgrade as a restart.
func databaseStatus(st registry.Status) string {
	switch st {
	case registry.StatusPausing:
		return "GOING_DOWN"
	case registry.StatusUpgrading:
		return "RESTARTING"
	case registry.StatusInactive, registry.StatusRestoreFailed:
		return "UNKNOWN"
	}
	return string(st)
}

// databaseRow is one row of GET databases. A replica's row has the primary's shape: its own
// identifier and endpoints, the region of the node it runs on, and strings Studio's reports can
// use (they have no fallback to the primary's for a replica).
func (s *Server) databaseRow(p *registry.Project, identifier, status, region string, created time.Time) map[string]any {
	return setAll(elem("GET /platform/projects/{ref}/databases", ""), map[string]any{
		"identifier": identifier, "inserted_at": ts(created), "status": status, "size": infraComputeSize(p),
		"region": region, "cloud_provider": "AWS", "db_port": s.cfg.Ports.SupavisorSession, "db_name": "postgres",
		"db_user": "postgres", "restUrl": s.projectURL(identifier) + "/rest/v1/", "db_host": s.dbHost(identifier),
		"connectionString": s.dbString(identifier, "postgres"), "connection_string_read_only": s.dbString(identifier, readOnlyUser),
	})
}

func (s *Server) listDatabases(w http.ResponseWriter, r *http.Request) error {
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	rs, err := s.replicasOf(r.Context(), p.Ref)
	if err != nil {
		return err
	}
	rows := []any{s.databaseRow(p, p.Ref, databaseStatus(p.Status), s.regionOf(p), p.CreatedAt)}
	for _, rep := range rs {
		rows = append(rows, s.databaseRow(p, rep.Identifier, rep.Status, s.cfg.ProjectRegion(rep.Region), rep.CreatedAt))
	}
	writeJSON(w, http.StatusOK, rows)
	return nil
}

// databasesStatuses lists every database with its status, the primary first. Studio compares the
// length of this list with the databases', and asks again until they match, so the primary is in
// it. A replica that is still setting up carries replicaInitializationStatus: the step it has
// reached with the time left, "completed", or the step that failed.
func (s *Server) databasesStatuses(w http.ResponseWriter, r *http.Request) error {
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	rs, err := s.replicasOf(r.Context(), p.Ref)
	if err != nil {
		return err
	}
	rows := []any{map[string]any{"identifier": p.Ref, "status": databaseStatus(p.Status)}}
	if len(rs) > 0 {
		sts, err := s.replicas.Statuses(r.Context(), p.Ref)
		if err != nil {
			return mapErr(err)
		}
		byID := make(map[string]replicas.Status, len(sts))
		for _, st := range sts {
			byID[st.Identifier] = st
		}
		for _, rep := range rs {
			st, ok := byID[rep.Identifier]
			if !ok {
				// The controller has not caught up with a row it has just been given.
				st = replicas.Status{Identifier: rep.Identifier, Status: rep.Status, Init: initFromRow(rep.Replica)}
			}
			row := map[string]any{"identifier": rep.Identifier, "status": st.Status}
			if init := initStatusJSON(st); init != nil {
				row["replicaInitializationStatus"] = init
			}
			rows = append(rows, row)
		}
	}
	writeJSON(w, http.StatusOK, rows)
	return nil
}

// initStatusJSON is replicaInitializationStatus of a replica's status. A replica that is up and
// has no setup record reads as completed; one that is still being set up and has none gets no
// field, which Studio takes as "no step known yet".
func initStatusJSON(st replicas.Status) map[string]any {
	in := st.Init
	if in == nil {
		switch st.Status {
		case registry.ReplicaInit, registry.ReplicaInitError:
			return nil
		}
		return map[string]any{"status": "completed"}
	}
	switch in.Status {
	case "in_progress":
		m := map[string]any{"status": "in_progress", "estimations": map[string]any{
			"baseBackupDownloadEstimateSeconds": in.BaseBackupDownloadEstimateSeconds,
			"walArchiveReplayEstimateSeconds":   in.WALArchiveReplayEstimateSeconds,
		}}
		if in.Progress != "" {
			m["progress"] = in.Progress
		}
		return m
	case "failed":
		m := map[string]any{"status": "failed"}
		if in.Error != "" {
			m["error"] = in.Error
		}
		return m
	}
	return map[string]any{"status": "completed"}
}

// initFromRow is the setup state a replicas row records, for a replica the controller has no
// status for yet.
func initFromRow(r registry.Replica) *replicas.InitStatus {
	switch {
	case r.InitError != "":
		return &replicas.InitStatus{Status: "failed", Error: r.InitError}
	case r.InitStep == registry.ReplicaStepDone:
		return &replicas.InitStatus{Status: "completed"}
	}
	return &replicas.InitStatus{Status: "in_progress", Progress: r.InitStep}
}
