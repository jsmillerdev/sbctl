package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jsmillerdev/supavise/internal/config"
	"github.com/jsmillerdev/supavise/internal/lifecycle"
	"github.com/jsmillerdev/supavise/internal/registry"
)

// Project upgrades: the routes of Studio's Settings > General > Service versions section and of
// the Management API's upgrade endpoints. A project runs the service versions it was last
// upgraded to; the node's pins move with a Supavise release; an Owner or Administrator moves a
// project onto them (internal/lifecycle/upgrade.go).
//
//	GET  /v1/projects/{ref}/upgrade/eligibility   what the project runs, what it could run, and what stands in the way
//	POST /v1/projects/{ref}/upgrade               start (201 with a tracking id; the project is UPGRADING)
//	GET  /v1/projects/{ref}/upgrade/status        the newest upgrade's progress, as Studio's upgrade screen reads it
//	GET  /platform/projects/{ref}/service-versions  the versions the project's services run
//
// target_version is Postgres's major version, as the spec's example shows ("17"), or the app
// version the eligibility answer names. There is one target per node: the node's pins.

func (s *Server) routesUpgrade(add func(string, handlerFunc)) {
	add("GET /v1/projects/{ref}/upgrade/eligibility", s.upgradeEligibility)
	add("POST /v1/projects/{ref}/upgrade", s.upgradeProject)
	add("GET /v1/projects/{ref}/upgrade/status", s.upgradeStatus)
	add("GET /platform/projects/{ref}/service-versions", s.serviceVersions)
}

// upgradeTimeout bounds one upgrade, which includes its base backup (a large database takes
// a while) and the start of every service.
const upgradeTimeout = 2 * time.Hour

var errNoUpgrades = errf(http.StatusServiceUnavailable, "Project upgrades are not available on this Supavise node")

func (s *Server) upgrader() (lifecycle.ProjectUpgrader, error) {
	up, ok := s.mgr.(lifecycle.ProjectUpgrader)
	if !ok {
		return nil, errNoUpgrades
	}
	return up, nil
}

// mapUpgradeErr turns the errors that refuse an upgrade into API errors.
func mapUpgradeErr(err error) error {
	switch {
	case errors.Is(err, lifecycle.ErrUpgradeNotNeeded), errors.Is(err, lifecycle.ErrUpgradeUnsupported):
		return errf(http.StatusBadRequest, "%s", strings.TrimPrefix(err.Error(), "lifecycle: "))
	case errors.Is(err, lifecycle.ErrNoBackupEngine):
		return errf(http.StatusServiceUnavailable, "%s", strings.TrimPrefix(err.Error(), "lifecycle: "))
	}
	return mapErr(err)
}

func (s *Server) upgradeEligibility(w http.ResponseWriter, r *http.Request) error {
	up, err := s.upgrader()
	if err != nil {
		return err
	}
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	el, err := up.UpgradeEligibility(r.Context(), p.Ref)
	if err != nil {
		return mapUpgradeErr(err)
	}
	writeJSON(w, http.StatusOK, eligibilityBody(el))
	return nil
}

// eligibilityBody is the spec's eligibility response. Studio requires every array, so each is
// present and empty rather than missing or null. The arrays hosted fills from a scan of the
// database (objects that block pg_upgrade) stay empty: the upgrade here keeps the Postgres major
// version and the data directory, so no object of the database stands in its way. The one scan
// that does apply is the installed extensions against a new Postgres release (an extension the
// release cannot serve, in the way hosted reports one the next major version lacks).
func eligibilityBody(el *lifecycle.UpgradeEligibility) map[string]any {
	pg := config.SvcPostgres
	targets := []any{}
	if el.Eligible {
		targets = append(targets, map[string]any{
			"postgres_version": strconv.Itoa(el.TargetMajor), "release_channel": "ga", "app_version": lifecycle.AppVersion(el.Target[pg]),
		})
	}
	// Studio draws a title and a link per validation error it knows; one it does not know would
	// be an empty row. The ones that apply here are hosted's own.
	validation := []any{}
	unsupported := []any{}
	for _, b := range el.Blockers {
		switch b.Type {
		case lifecycle.BlockerHibernating:
			validation = append(validation, map[string]any{"type": "project_hibernating"})
		case lifecycle.BlockerExtension:
			validation = append(validation, map[string]any{"type": "unsupported_extension", "extension_name": b.Extension})
			unsupported = append(unsupported, b.Extension)
		}
	}
	resp := base("GET /v1/projects/{ref}/upgrade/eligibility")
	set(resp, "eligible", el.Eligible)
	set(resp, "current_app_version", lifecycle.AppVersion(el.Current[pg]))
	set(resp, "current_app_version_release_channel", "ga")
	set(resp, "latest_app_version", lifecycle.AppVersion(el.Latest[pg]))
	set(resp, "target_upgrade_versions", targets)
	set(resp, "duration_estimate_hours", el.DowntimeHours)
	set(resp, "legacy_auth_custom_roles", []any{})
	set(resp, "objects_to_be_dropped", []any{})
	set(resp, "unsupported_extensions", unsupported)
	set(resp, "user_defined_objects_in_internal_schemas", []any{})
	set(resp, "validation_errors", validation)
	set(resp, "warnings", []any{})
	// Studio's breaking-changes alert reads this field (it is not in the spec): there is none
	// within one Postgres major version.
	set(resp, "potential_breaking_changes", []any{})
	return resp
}

// upgradeTarget checks the requested target against the node's pins. The only version a project
// is upgraded to is the node's: its Postgres major version ("17"), or its app version.
func upgradeTarget(up lifecycle.ProjectUpgrader, requested string) error {
	node, err := up.NodeVersions()
	if err != nil {
		return err
	}
	pin := node[config.SvcPostgres]
	want := strings.TrimSpace(requested)
	if want == "" {
		return errf(http.StatusBadRequest, "target_version is required")
	}
	major := strconv.Itoa(lifecycle.PostgresMajor(pin))
	short := lifecycle.ShortVersion(config.SvcPostgres, pin)
	bare, _, _ := strings.Cut(short, "-r")
	switch strings.TrimPrefix(want, "supabase-postgres-") {
	case major, short, bare, pin:
		return nil
	}
	return errf(http.StatusBadRequest, "target_version %q is not available: this node offers Postgres %s (%s); upgrades across major versions are not supported", requested, major, short)
}

func (s *Server) upgradeProject(w http.ResponseWriter, r *http.Request) error {
	up, err := s.upgrader()
	if err != nil {
		return err
	}
	var in struct {
		TargetVersion  string `json:"target_version"`
		ReleaseChannel string `json:"release_channel"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	if in.ReleaseChannel != "" && in.ReleaseChannel != "ga" {
		return errf(http.StatusBadRequest, "release_channel %q is not available: this node has the ga channel only", in.ReleaseChannel)
	}
	if err := upgradeTarget(up, in.TargetVersion); err != nil {
		return err
	}
	// The request's context ends when the client leaves; the upgrade must not stop halfway
	// (detach), but it is bounded.
	ctx, cancel, err := s.detach(r, upgradeTimeout)
	if err != nil {
		return err
	}
	run, err := up.BeginUpgrade(ctx, p.Ref, lifecycle.UpgradeRequest{TargetVersion: in.TargetVersion})
	if err != nil {
		cancel()
		return mapUpgradeErr(err)
	}
	u := run.Upgrade()
	go func() {
		defer cancel()
		if err := run.Run(ctx); err != nil {
			s.log.Error("project upgrade failed", "ref", p.Ref, "tracking_id", u.TrackingID, "err", err)
			return
		}
		s.log.Info("project upgrade finished", "ref", p.Ref, "tracking_id", u.TrackingID)
	}()
	writeJSON(w, http.StatusCreated, map[string]any{"tracking_id": u.TrackingID})
	return nil
}

// upgradeStatus answers with the project's newest upgrade whatever tracking_id the query names:
// Studio keeps the id of the upgrade it started in the page's address, and a screen that went
// blank because another administrator started a later upgrade would help nobody.
func (s *Server) upgradeStatus(w http.ResponseWriter, r *http.Request) error {
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	resp := base("GET /v1/projects/{ref}/upgrade/status")
	var status any // null: this project has never been upgraded
	if store := registry.Upgrades(s.reg); store != nil {
		u, err := store.LatestUpgrade(r.Context(), p.Ref)
		switch {
		case err == nil:
			status = upgradeStatusBody(u)
		case !errors.Is(err, registry.ErrNotFound):
			return err
		}
	}
	resp["databaseUpgradeStatus"] = status
	writeJSON(w, http.StatusOK, resp)
	return nil
}

func upgradeStatusBody(u *registry.Upgrade) map[string]any {
	target := lifecycle.ShortVersion(config.SvcPostgres, u.To[config.SvcPostgres])
	if target == "" {
		target = u.TargetVersion
	}
	m := map[string]any{
		"initiated_at": ts(u.InitiatedAt), "latest_status_at": ts(u.LatestStatusAt),
		"target_version": target, "status": int(u.Status), "progress": u.Progress,
	}
	if u.Error != "" {
		m["error"] = u.Error
	}
	return m
}

func (s *Server) serviceVersions(w http.ResponseWriter, r *http.Request) error {
	up, err := s.upgrader()
	if err != nil {
		return err
	}
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	cur, err := up.EffectiveVersions(p)
	if err != nil {
		return err
	}
	resp := base("GET /platform/projects/{ref}/service-versions")
	set(resp, "gotrue", lifecycle.ShortVersion(config.SvcGoTrue, cur[config.SvcGoTrue]))
	set(resp, "postgrest", lifecycle.ShortVersion(config.SvcPostgREST, cur[config.SvcPostgREST]))
	set(resp, "supabase-postgres", lifecycle.ShortVersion(config.SvcPostgres, cur[config.SvcPostgres]))
	writeJSON(w, http.StatusOK, resp)
	return nil
}
