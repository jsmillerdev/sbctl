package api

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"path"
	"strconv"
	"time"

	"github.com/jsmillerdev/supavise/internal/backup"
	"github.com/jsmillerdev/supavise/internal/lifecycle"
	"github.com/jsmillerdev/supavise/internal/registry"
)

// Backups and point-in-time restore, in the shapes of hosted Supabase's dashboard (/platform) and
// Management API (/v1). The node's base backups are physical (a copy of the data directory) and
// its archived WAL makes every moment between the oldest backup and now restorable, so the
// project reports PITR as enabled; Studio then shows the Point in time tab and hides the
// scheduled list, as it does for a hosted project with the PITR add-on.

// BackupSource is the part of the backup service the Management API reads. *backup.Service
// implements it. The restores themselves run through the lifecycle Engine
// (lifecycle.DatabaseRestorer), which finds the backup service on its own.
type BackupSource interface {
	RestoreWindow(ctx context.Context, ref string, running bool) (backup.RestoreWindow, error)
}

// restoreWarnAfter is how long a restore may run before the log says it is taking long: the
// base backup is unpacked, WAL is replayed (up to the backup service's recovery timeout of 30
// minutes), and a new base backup is taken. The restore itself has no deadline: one that
// expired halfway would also expire the rollback, which stops and starts the project, and
// leave it down on its original data.
const restoreWarnAfter = 4 * time.Hour

// errNoBackups answers a restore on a node whose backup service is not set up.
var errNoBackups = errf(http.StatusServiceUnavailable, "Backups are not set up on this Supavise node, so there is nothing to restore from")

func (s *Server) routesBackups(add func(string, handlerFunc)) {
	add("GET /platform/database/{ref}/backups", s.platformBackups)
	add("GET /v1/projects/{ref}/database/backups", s.v1Backups)
	add("POST /platform/database/{ref}/backups/restore", s.restoreBackupRoute)
	add("POST /platform/database/{ref}/backups/restore-physical", s.restorePhysicalRoute)
	add("POST /platform/database/{ref}/backups/pitr", s.restorePITRRoute)
	add("POST /v1/projects/{ref}/database/backups/restore", s.restoreBackupRoute)
	add("POST /v1/projects/{ref}/database/backups/restore-pitr", s.restorePITRRoute)
	add("GET /platform/projects/{ref}/billing/addons", s.platformAddons)
	add("GET /v1/projects/{ref}/billing/addons", s.v1Addons)
}

// backupItem is one entry of the backup list.
type backupItem struct {
	id int64
	at time.Time
}

// backupInfo is what the list routes report.
type backupInfo struct {
	enabled          bool
	items            []backupItem
	earliest, latest time.Time
	window           backup.RestoreWindow
}

// isRunning reports whether p's cluster is up: the restore window then reaches to now.
func isRunning(p *registry.Project) bool {
	return p.Status == registry.StatusActiveHealthy || p.Status == registry.StatusActiveUnhealthy
}

// restorable refuses a restore of a project that is not running with 409, before the request's
// time or backup is judged: the cluster must be up, and the window of a project that is not running
// (one that is already RESTORING, for instance) says nothing about what a restore could reach.
// A project whose last restore failed can be restored again; its window then comes from the archive alone.
func restorable(p *registry.Project) error {
	if !isRunning(p) && p.Status != registry.StatusRestoreFailed {
		return errf(http.StatusConflict, "Cannot restore project %s while it is %s", p.Ref, p.Status)
	}
	return nil
}

// backupInfoOf reads the node's restorable backups of p. Without a backup service nothing is
// restorable and PITR is off.
func (s *Server) backupInfoOf(ctx context.Context, p *registry.Project) (backupInfo, error) {
	var bi backupInfo
	if s.backups == nil {
		return bi, nil
	}
	bi.enabled = true
	w, err := s.backups.RestoreWindow(ctx, p.Ref, isRunning(p))
	if err != nil {
		s.log.Warn("could not read the backups", "ref", p.Ref, "err", err)
		return bi, errf(http.StatusBadGateway, "Could not read the project's backups from the backup storage; the Supavise log has the details")
	}
	bi.window = w
	if len(w.Backups) == 0 {
		return bi, nil
	}
	// The picker offers whole seconds: round inward so that it never offers a moment outside the window.
	bi.earliest = w.Earliest.Truncate(time.Second)
	if bi.earliest.Before(w.Earliest) {
		bi.earliest = bi.earliest.Add(time.Second)
	}
	bi.latest = w.Latest.Truncate(time.Second)
	if bi.latest.Before(bi.earliest) {
		// Less than a second since the only backup ended: no whole second is restorable yet.
		bi.earliest, bi.latest = time.Time{}, time.Time{}
	}
	rows, err := s.reg.ListBackups(ctx, p.Ref)
	if err != nil {
		return bi, err
	}
	byID := map[string]int64{}
	for _, r := range rows {
		if r.Kind == "base" && r.Status == registry.BackupCompleted {
			byID[path.Base(r.Location)] = r.ID
		}
	}
	for _, m := range w.Backups {
		if id, ok := byID[m.ID]; ok {
			bi.items = append(bi.items, backupItem{id: id, at: m.StopTime})
		}
	}
	return bi, nil
}

func (s *Server) platformBackups(w http.ResponseWriter, r *http.Request) error {
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	bi, err := s.backupInfoOf(r.Context(), p)
	if err != nil {
		return err
	}
	resp := base("GET /platform/database/{ref}/backups")
	items := make([]any, 0, len(bi.items))
	for _, it := range bi.items {
		items = append(items, map[string]any{"id": it.id, "isPhysicalBackup": true, "project_id": projectNumID(p),
			"status": "COMPLETED", "inserted_at": ts(it.at)})
	}
	set(resp, "region", s.regionOf(p))
	set(resp, "backups", items)
	set(resp, "pitr_enabled", bi.enabled)
	set(resp, "walg_enabled", bi.enabled)
	set(resp, "physicalBackupData", map[string]any{})
	if !bi.earliest.IsZero() {
		set(resp, "physicalBackupData.earliestPhysicalBackupDateUnix", bi.earliest.Unix())
		set(resp, "physicalBackupData.latestPhysicalBackupDateUnix", bi.latest.Unix())
	}
	writeJSON(w, http.StatusOK, resp)
	return nil
}

func (s *Server) v1Backups(w http.ResponseWriter, r *http.Request) error {
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	bi, err := s.backupInfoOf(r.Context(), p)
	if err != nil {
		return err
	}
	resp := base("GET /v1/projects/{ref}/database/backups")
	items := make([]any, 0, len(bi.items))
	for _, it := range bi.items {
		items = append(items, map[string]any{"id": it.id, "is_physical_backup": true, "status": "COMPLETED", "inserted_at": ts(it.at)})
	}
	set(resp, "region", s.regionOf(p))
	set(resp, "backups", items)
	set(resp, "pitr_enabled", bi.enabled)
	set(resp, "walg_enabled", bi.enabled)
	set(resp, "physical_backup_data", map[string]any{})
	if !bi.earliest.IsZero() {
		set(resp, "physical_backup_data.earliest_physical_backup_date_unix", bi.earliest.Unix())
		set(resp, "physical_backup_data.latest_physical_backup_date_unix", bi.latest.Unix())
	}
	writeJSON(w, http.StatusOK, resp)
	return nil
}

// restoreBackupRoute restores the project to the state of one listed backup. It serves
// the dashboard's logical restore and the Management API's restore, which differ in name only
// here: every backup of this node is a physical one.
func (s *Server) restoreBackupRoute(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		ID *float64 `json:"id"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	return s.restoreFromBackup(w, r, in.ID, "")
}

// restorePhysicalRoute is the dashboard's restore of a physical backup; its optional
// recovery_time_target is a time (RFC 3339, or Unix seconds) to replay to from that backup.
func (s *Server) restorePhysicalRoute(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		ID                 *float64 `json:"id"`
		RecoveryTimeTarget *string  `json:"recovery_time_target"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	return s.restoreFromBackup(w, r, in.ID, deref(in.RecoveryTimeTarget))
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func (s *Server) restoreFromBackup(w http.ResponseWriter, r *http.Request, rawID *float64, rawTarget string) error {
	ctx := r.Context()
	p, err := s.loadProject(ctx, r.PathValue("ref"))
	if err != nil {
		return err
	}
	if err := restorable(p); err != nil {
		return err
	}
	if rawID == nil || *rawID != math.Trunc(*rawID) || *rawID < 1 {
		return errf(http.StatusBadRequest, "A backup id is required")
	}
	if s.backups == nil {
		return errNoBackups
	}
	bi, err := s.backupInfoOf(ctx, p)
	if err != nil {
		return err
	}
	id := int64(*rawID)
	var picked *backup.Manifest
	rows, err := s.reg.ListBackups(ctx, p.Ref)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if row.ID == id && row.Kind == "base" {
			for i := range bi.window.Backups {
				if bi.window.Backups[i].ID == path.Base(row.Location) {
					picked = &bi.window.Backups[i]
				}
			}
			if picked == nil && row.Status == registry.BackupCompleted {
				return errf(http.StatusBadRequest, "Backup %d cannot be restored: it is not on the history of the project's current timeline, or its WAL is no longer archived", id)
			}
		}
	}
	if picked == nil {
		return errf(http.StatusNotFound, "Backup %d not found", id)
	}
	req := lifecycle.RestoreRequest{BackupID: picked.ID}
	if rawTarget != "" {
		t, err := parseRecoveryTarget(rawTarget)
		if err != nil {
			return err
		}
		if t.Before(picked.StopTime) || t.After(bi.latest) {
			return errf(http.StatusBadRequest, "The recovery time must be between the end of backup %d (%s) and %s", id, ts(picked.StopTime), ts(bi.latest))
		}
		req.Target = t
	}
	return s.startRestore(w, r, p, req)
}

// parseRecoveryTarget reads the optional time of restore-physical: RFC 3339 or Unix seconds.
func parseRecoveryTarget(v string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.UTC(), nil
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
		return time.Unix(n, 0).UTC(), nil
	}
	return time.Time{}, errf(http.StatusBadRequest, "recovery_time_target must be an RFC 3339 time or Unix seconds")
}

// restorePITRRoute restores the project to a time. Hosted names the field
// recovery_time_target_unix on both its dashboard and its Management API.
func (s *Server) restorePITRRoute(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Target *float64 `json:"recovery_time_target_unix"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	ctx := r.Context()
	p, err := s.loadProject(ctx, r.PathValue("ref"))
	if err != nil {
		return err
	}
	if err := restorable(p); err != nil {
		return err
	}
	if in.Target == nil || *in.Target != math.Trunc(*in.Target) || *in.Target < 1 {
		return errf(http.StatusBadRequest, "recovery_time_target_unix must be a time in whole Unix seconds")
	}
	if s.backups == nil {
		return errNoBackups
	}
	bi, err := s.backupInfoOf(ctx, p)
	if err != nil {
		return err
	}
	if bi.earliest.IsZero() {
		return errf(http.StatusBadRequest, "The project has no backup to restore from yet; the first base backup is taken by the nightly timer, or by `supavise backups create`")
	}
	target := time.Unix(int64(*in.Target), 0).UTC()
	if target.Before(bi.earliest) || target.After(bi.latest) {
		return errf(http.StatusBadRequest, "The recovery time must be between %s and %s", ts(bi.earliest), ts(bi.latest))
	}
	return s.startRestore(w, r, p, lifecycle.RestoreRequest{Target: target})
}

// startRestore moves the project to RESTORING and runs the restore in the background, outside
// the request: a client that leaves must not stop it halfway. The answer is 201 once the project
// is RESTORING; the dashboard then follows the project's status until it is ACTIVE_HEALTHY again.
// A project that is not active, or is already being restored, is refused with 409, and so is
// one whose disk cannot hold the restored copy.
func (s *Server) startRestore(w http.ResponseWriter, r *http.Request, p *registry.Project, req lifecycle.RestoreRequest) error {
	dr, ok := s.mgr.(lifecycle.DatabaseRestorer)
	if !ok {
		return errNoBackups
	}
	ctx, cancel, err := s.detach(r, restoreWarnAfter)
	if err != nil {
		return err
	}
	// detach's deadline bounds BeginRestore; the restore runs on a context without one.
	rctx := context.WithoutCancel(ctx)
	rs, err := dr.BeginRestore(ctx, p.Ref)
	switch {
	case errors.Is(err, lifecycle.ErrNoRestorer):
		cancel()
		return errNoBackups
	case err != nil:
		cancel()
		return mapErr(err)
	}
	go func() {
		defer cancel()
		slow := time.AfterFunc(restoreWarnAfter, func() {
			s.log.Warn("restore is taking long", "ref", p.Ref, "after", restoreWarnAfter)
		})
		defer slow.Stop()
		if err := rs.Run(rctx, req); err != nil {
			s.log.Error("restore failed", "ref", p.Ref, "err", err)
			return
		}
		s.log.Info("restore finished", "ref", p.Ref)
	}()
	w.WriteHeader(http.StatusCreated)
	return nil
}

// pitrVariants are the retention variants the specs name; the variant that reports a node's
// retention is the nearest one, and its meta carries the real number of days.
var pitrVariants = []int{7, 14, 28}

// pitrRetentionDays is how far back the node's history reaches for p: backup.retention_days,
// or, with pruning off (0), since the project was created.
func (s *Server) pitrRetentionDays(p *registry.Project) int {
	if d := s.cfg.Backup.RetentionDays; d > 0 {
		return d
	}
	days := int(math.Ceil(s.now().Sub(p.CreatedAt).Hours() / 24))
	return max(days, 1)
}

func nearestPITRVariant(days int) int {
	best := pitrVariants[0]
	for _, v := range pitrVariants[1:] {
		if abs(v-days) < abs(best-days) {
			best = v
		}
	}
	return best
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// pitrAddon is the one add-on of a node: point-in-time recovery over the node's retention,
// at no price. Nothing is billed; the entry exists because Studio decides what to show, and
// how many days the Point in time tab states, from it.
func (s *Server) pitrAddon(p *registry.Project) (days int, id, name string) {
	days = s.pitrRetentionDays(p)
	id = fmt.Sprintf("pitr_%d", nearestPITRVariant(days))
	name = fmt.Sprintf("Point in time recovery (%d days)", days)
	return days, id, name
}

func (s *Server) platformAddons(w http.ResponseWriter, r *http.Request) error {
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	resp := base("GET /platform/projects/{ref}/billing/addons")
	set(resp, "ref", p.Ref)
	var selected []any
	if s.backups != nil {
		days, id, name := s.pitrAddon(p)
		selected = append(selected, map[string]any{"type": "pitr", "variant": map[string]any{
			"identifier": id, "name": name, "price": 0, "price_description": "Included", "price_type": "fixed",
			"price_interval": "monthly", "meta": map[string]any{"backup_duration_days": days},
		}})
	}
	// Custom domains need no add-on here; the entry is what Studio looks for before it shows
	// the Custom Domains page.
	selected = append(selected, map[string]any{"type": "custom_domain", "variant": map[string]any{
		"identifier": "cd_default", "name": "Custom domain", "price": 0, "price_description": "Included", "price_type": "fixed",
		"price_interval": "monthly",
	}})
	set(resp, "selected_addons", selected)
	writeJSON(w, http.StatusOK, resp)
	return nil
}

func (s *Server) v1Addons(w http.ResponseWriter, r *http.Request) error {
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	resp := base("GET /v1/projects/{ref}/billing/addons")
	var selected []any
	if s.backups != nil {
		days, id, name := s.pitrAddon(p)
		selected = append(selected, map[string]any{"type": "pitr", "variant": map[string]any{
			"id": id, "name": name, "meta": map[string]any{"backup_duration_days": days},
			"price": map[string]any{"description": "Included", "type": "fixed", "interval": "monthly", "amount": 0},
		}})
	}
	selected = append(selected, map[string]any{"type": "custom_domain", "variant": map[string]any{
		"id": "cd_default", "name": "Custom domain",
		"price": map[string]any{"description": "Included", "type": "fixed", "interval": "monthly", "amount": 0},
	}})
	set(resp, "selected_addons", selected)
	writeJSON(w, http.StatusOK, resp)
	return nil
}
