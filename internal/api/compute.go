package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/supavise/supavise/internal/diskquota"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/registry"
)

// Compute sizes and disk: the routes behind Studio's Compute and Disk page and the Supabase
// CLI and MCP server's add-on and disk calls.
//
// Studio reads the project's size from infra_compute_size, lists the choices from
// GET billing/addons (available_addons, type compute_instance), changes it with POST (PATCH on
// /v1) billing/addons {addon_type, addon_variant} and polls the project's status, which is
// RESIZING while the change runs. Prices are 0 and the dashboard hides them
// (project_addons:show_compute_price is in disabledFeatures). Nano is never listed or selected
// here: the specs' variant enums have no ci_nano, and Studio adds a Nano card itself and reads the
// current size from infra_compute_size. Removing the compute add-on (DELETE) returns the
// project to Nano, as on hosted.
//
// The sizes that do not fit the node (lifecycle.Offers) are left out of available_addons, which is
// how hosted leaves out the sizes a project cannot have: Studio's cards have no disabled state of
// their own. `supavise projects sizes` shows every size with the reason. A request for a size that
// does not fit is refused with 400 and the reason.

// DiskLimits is what the disk routes need of the data volume (internal/diskquota.Manager).
type DiskLimits interface {
	Volume() (diskquota.Volume, error)
	// Used is the bytes under the project's directory.
	Used(ref string) int64
	// Limit is the project's configured limit in GB (ok false: none).
	Limit(ref string) (gb int, ok bool)
	// Set limits the project's disk and returns once the limit is in force.
	Set(ctx context.Context, ref string, seq, gb int) error
}

// resizeTimeout bounds one resize, as createTimeout bounds a create: the project restarts and
// each service must answer.
const resizeTimeout = 15 * time.Minute

func (s *Server) routesCompute(add func(string, handlerFunc)) {
	add("POST /platform/projects/{ref}/billing/addons", s.platformSetAddon)
	add("DELETE /platform/projects/{ref}/billing/addons/{addon_variant}", s.platformRemoveAddon)
	add("PATCH /v1/projects/{ref}/billing/addons", s.v1SetAddon)
	add("DELETE /v1/projects/{ref}/billing/addons/{addon_variant}", s.v1RemoveAddon)
	add("GET /platform/projects/{ref}/disk", s.platformDisk)
	add("POST /platform/projects/{ref}/disk", s.platformSetDisk)
	add("GET /platform/projects/{ref}/disk/util", s.diskUtil("GET /platform/projects/{ref}/disk/util"))
	add("GET /platform/projects/{ref}/disk/custom-config", s.diskAutoscale("GET /platform/projects/{ref}/disk/custom-config"))
	add("POST /platform/projects/{ref}/disk/custom-config", s.setDiskAutoscale)
	add("POST /platform/projects/{ref}/resize", s.platformResizeVolume)
	add("GET /v1/projects/{ref}/config/disk", s.v1Disk)
	add("POST /v1/projects/{ref}/config/disk", s.v1SetDisk)
	add("GET /v1/projects/{ref}/config/disk/util", s.diskUtil("GET /v1/projects/{ref}/config/disk/util"))
	add("GET /v1/projects/{ref}/config/disk/autoscale", s.diskAutoscale("GET /v1/projects/{ref}/config/disk/autoscale"))
}

// sizeOf is the project's compute size; a project with no or an unknown class counts as the
// default size.
func sizeOf(p *registry.Project) lifecycle.Class {
	c, err := lifecycle.ClassFor(p.Class)
	if err != nil {
		c, _ = lifecycle.ClassFor("")
	}
	return c
}

// inputError is a 400 with a message the dashboard shows as is.
func inputError(format string, args ...any) *Error {
	return errf(http.StatusBadRequest, format, args...)
}

// computeMeta is the variant's meta as Studio reads it (memory_gb, cpu_cores) plus the connection
// limits hosted lists.
func computeMeta(c lifecycle.Class) map[string]any {
	return map[string]any{
		"memory_gb": c.MemoryGB(), "cpu_cores": c.VCPUs(), "cpu_dedicated": !c.Shared,
		"connections_direct": c.MaxConnections, "connections_pooler": c.PoolerMaxClients,
	}
}

// computeVariantPlatform and computeVariantV1 are one size as the two APIs shape an add-on variant.
func computeVariantPlatform(c lifecycle.Class) map[string]any {
	return map[string]any{
		"identifier": c.Variant, "name": c.Title, "price_description": "Included", "price_type": "fixed",
		"price_interval": "monthly", "price": 0, "meta": computeMeta(c),
	}
}

func computeVariantV1(c lifecycle.Class) map[string]any {
	return map[string]any{
		"id": c.Variant, "name": c.Title, "meta": computeMeta(c),
		"price": map[string]any{"description": "Included", "type": "fixed", "interval": "monthly", "amount": 0},
	}
}

// offeredSizes returns the sizes to list for p: those the node can give it, and its own.
func (s *Server) offeredSizes(ctx context.Context, p *registry.Project) []lifecycle.Class {
	cur := sizeOf(p)
	rz, ok := s.mgr.(lifecycle.Resizer)
	if !ok {
		return []lifecycle.Class{cur}
	}
	offers, err := rz.Offers(ctx, p.Ref)
	if err != nil {
		s.log.Warn("compute: could not work out the sizes the node offers", "ref", p.Ref, "err", err)
		return []lifecycle.Class{cur}
	}
	var out []lifecycle.Class
	for _, o := range offers {
		if o.Fits || o.Class.Name == cur.Name {
			out = append(out, o.Class)
		}
	}
	return out
}

// computeAddons returns the compute_instance entries of the add-ons answer: the selected one
// (none for Nano, which is the absence of an add-on) and the available one.
func (s *Server) computeAddons(ctx context.Context, p *registry.Project, variant func(lifecycle.Class) map[string]any) (selected, available []any) {
	cur := sizeOf(p)
	if cur.Name != "nano" {
		selected = append(selected, map[string]any{"type": "compute_instance", "variant": variant(cur)})
	}
	var vs []any
	for _, c := range s.offeredSizes(ctx, p) {
		if c.Name != "nano" {
			vs = append(vs, variant(c))
		}
	}
	available = append(available, map[string]any{"type": "compute_instance", "name": "Compute", "variants": vs})
	return selected, available
}

// ---- changing the size ------------------------------------------------------------------------

type addonBody struct {
	AddonType    string `json:"addon_type"`
	AddonVariant string `json:"addon_variant"`
}

func (s *Server) platformSetAddon(w http.ResponseWriter, r *http.Request) error {
	return s.setAddon(w, r, http.StatusCreated, "POST /platform/projects/{ref}/billing/addons")
}

func (s *Server) v1SetAddon(w http.ResponseWriter, r *http.Request) error {
	return s.setAddon(w, r, http.StatusOK, "PATCH /v1/projects/{ref}/billing/addons")
}

func (s *Server) setAddon(w http.ResponseWriter, r *http.Request, status int, key string) error {
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	var in addonBody
	if err := decode(r, &in); err != nil {
		return err
	}
	if in.AddonType != "compute_instance" {
		// Only compute is an add-on here; the others (PITR, IPv4, MFA, log drains) are not sold.
		stubHandler(operationByKey(key))(w, r)
		return nil
	}
	if in.AddonVariant == "" {
		return inputError("addon_variant is required")
	}
	return s.resize(w, r, p, in.AddonVariant, status)
}

func (s *Server) platformRemoveAddon(w http.ResponseWriter, r *http.Request) error {
	return s.removeAddon(w, r, "DELETE /platform/projects/{ref}/billing/addons/{addon_variant}")
}

func (s *Server) v1RemoveAddon(w http.ResponseWriter, r *http.Request) error {
	return s.removeAddon(w, r, "DELETE /v1/projects/{ref}/billing/addons/{addon_variant}")
}

// removeAddon: taking the compute add-on away leaves the project on the free size, Nano.
func (s *Server) removeAddon(w http.ResponseWriter, r *http.Request, key string) error {
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	if !strings.HasPrefix(r.PathValue("addon_variant"), "ci_") {
		stubHandler(operationByKey(key))(w, r)
		return nil
	}
	variant := r.PathValue("addon_variant")
	if _, ok := lifecycle.ClassByVariant(variant); !ok {
		return inputError("%q is not a compute size", variant)
	}
	if cur := sizeOf(p); cur.Variant != variant {
		// Only the add-on the project has can be taken away; a project on Small does not drop to
		// Nano because someone named another variant.
		return inputError("this project does not have the %s compute add-on (it is on %s)", variant, cur.Title)
	}
	return s.resize(w, r, p, "nano", http.StatusOK)
}

// resize validates the change, moves the project to RESIZING and lets the restart run on after
// the answer: the dashboard polls the project's status until it is ACTIVE_HEALTHY again.
func (s *Server) resize(w http.ResponseWriter, r *http.Request, p *registry.Project, variant string, status int) error {
	rz, ok := s.mgr.(lifecycle.Resizer)
	if !ok {
		return errf(http.StatusNotImplemented, "This Supavise node cannot change compute sizes")
	}
	size, ok := lifecycle.ParseSize(variant)
	if !ok {
		return inputError("%q is not a compute size this node offers (have %s)", variant, strings.Join(lifecycle.ClassNames(), ", "))
	}
	endOp, err := s.beginOp()
	if err != nil {
		return err
	}
	run, err := rz.BeginResize(r.Context(), p.Ref, size)
	if err != nil {
		endOp()
		return mapResizeErr(err)
	}
	if !run.Changed() {
		run.Close()
		endOp()
		writeJSON(w, status, map[string]any{})
		return nil
	}
	go func() {
		defer endOp()
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), resizeTimeout)
		defer cancel()
		if err := run.Run(ctx); err != nil {
			s.log.Error("compute: resize failed", "ref", p.Ref, "from", run.From(), "to", run.To(), "err", err)
		}
	}()
	writeJSON(w, status, map[string]any{})
	return nil
}

// mapResizeErr turns a refusal of the lifecycle into the answer the dashboard shows: the node
// cannot honor the size (400), or the project is busy or in the wrong state (409).
func mapResizeErr(err error) error {
	if ce, ok := lifecycle.IsCapacity(err); ok {
		return inputError("%s", capitalize(ce.Message))
	}
	if se, ok := lifecycle.IsSettings(err); ok {
		return inputError("%s", capitalize(se.Message))
	}
	return mapErr(err)
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// ---- disk -------------------------------------------------------------------------------------

// The disk numbers hosted reports that this node cannot change: a gp3 volume with the baseline.
const (
	diskType       = "gp3"
	diskIOPS       = 3000
	diskThroughput = 125
	gib            = int64(1) << 30
	// diskGrowthPercent and the others are the autoscale settings shown, which nothing acts on.
	diskGrowthPercent = 50
	diskMinIncrement  = 4
)

// diskState is what the disk routes report about p.
type diskState struct {
	enforced bool  // the size is a quota in force
	sizeGB   int   // the size shown: the quota, else the volume's
	sizeB    int64 // the same in bytes
	usedB    int64
	freeB    int64
	vol      diskquota.Volume
}

type diskUse struct {
	at    time.Time
	bytes int64
}

// usedBytes walks the project's directory at most every 20 seconds: Studio polls the route.
func (s *Server) usedBytes(ref string) int64 {
	if v, ok := s.diskUse.Load(ref); ok && s.now().Sub(v.(diskUse).at) < 20*time.Second {
		return v.(diskUse).bytes
	}
	n := s.disk.Used(ref)
	s.diskUse.Store(ref, diskUse{at: s.now(), bytes: n})
	return n
}

func (s *Server) diskOf(p *registry.Project) (diskState, error) {
	vol, err := s.disk.Volume()
	if err != nil {
		return diskState{}, errf(http.StatusInternalServerError, "Could not read the data volume: %v", err)
	}
	d := diskState{vol: vol, usedB: s.usedBytes(p.Ref)}
	if gb, ok := s.disk.Limit(p.Ref); ok && vol.Enforceable() {
		d.enforced, d.sizeGB, d.sizeB = true, gb, int64(gb)*gib
		d.freeB = max(d.sizeB-d.usedB, 0)
		return d, nil
	}
	d.sizeGB = int((vol.TotalBytes + gib - 1) / gib)
	d.sizeB = vol.TotalBytes
	d.freeB = vol.FreeBytes
	return d, nil
}

// diskAttributes is the attributes object of GET disk.
func (d diskState) diskAttributes(platform bool) map[string]any {
	a := map[string]any{"type": diskType, "size_gb": d.sizeGB, "iops": diskIOPS}
	if platform {
		a["throughput_mbps"], a["throughput_mibps"] = diskThroughput, diskThroughput
	} else {
		a["throughput_mibps"] = diskThroughput
	}
	return a
}

func (s *Server) platformDisk(w http.ResponseWriter, r *http.Request) error {
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	d, err := s.diskOf(p)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"attributes": d.diskAttributes(true)})
	return nil
}

func (s *Server) v1Disk(w http.ResponseWriter, r *http.Request) error {
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	d, err := s.diskOf(p)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"attributes": d.diskAttributes(false)})
	return nil
}

// diskUtil reports the real use of the project's data directory against the volume's size, or
// against the quota where one is in force.
func (s *Server) diskUtil(key string) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		p, err := s.loadProject(r.Context(), r.PathValue("ref"))
		if err != nil {
			return err
		}
		d, err := s.diskOf(p)
		if err != nil {
			return err
		}
		resp := base(key)
		set(resp, "timestamp", s.now().UTC().Format(time.RFC3339))
		set(resp, "metrics", map[string]any{"fs_size_bytes": d.sizeB, "fs_avail_bytes": d.freeB, "fs_used_bytes": d.usedB})
		writeJSON(w, http.StatusOK, resp)
		return nil
	}
}

// diskAutoscale shows settings that nothing acts on: the volume does not grow by itself here.
func (s *Server) diskAutoscale(key string) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		p, err := s.loadProject(r.Context(), r.PathValue("ref"))
		if err != nil {
			return err
		}
		d, err := s.diskOf(p)
		if err != nil {
			return err
		}
		resp := base(key)
		setAll(resp, map[string]any{"growth_percent": diskGrowthPercent, "min_increment_gb": diskMinIncrement, "max_size_gb": d.sizeGB})
		writeJSON(w, http.StatusOK, resp)
		return nil
	}
}

func (s *Server) setDiskAutoscale(w http.ResponseWriter, r *http.Request) error {
	if _, err := s.loadProject(r.Context(), r.PathValue("ref")); err != nil {
		return err
	}
	return inputError("Disk autoscaling is not available on this Supavise node: the data volume does not grow by itself. Grow the volume yourself (deploy/README.md, Sizing).")
}

type diskBody struct {
	Attributes struct {
		Type           string   `json:"type"`
		SizeGB         *float64 `json:"size_gb"`
		IOPS           *float64 `json:"iops"`
		ThroughputMbps *float64 `json:"throughput_mbps"`
		ThroughputMibs *float64 `json:"throughput_mibps"`
	} `json:"attributes"`
}

func (s *Server) platformSetDisk(w http.ResponseWriter, r *http.Request) error {
	return s.setDisk(w, r)
}

func (s *Server) v1SetDisk(w http.ResponseWriter, r *http.Request) error { return s.setDisk(w, r) }

func (s *Server) setDisk(w http.ResponseWriter, r *http.Request) error {
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	var in diskBody
	if err := decode(r, &in); err != nil {
		return err
	}
	a := in.Attributes
	if a.SizeGB == nil {
		return inputError("attributes.size_gb is required")
	}
	if (a.Type != "" && a.Type != diskType) || (a.IOPS != nil && *a.IOPS != diskIOPS) ||
		(a.ThroughputMbps != nil && *a.ThroughputMbps != diskThroughput) || (a.ThroughputMibs != nil && *a.ThroughputMibs != diskThroughput) {
		return inputError("The disk type, IOPS and throughput are those of the volume this node runs on and cannot be changed here. Only the size of the project's disk can, and only on an XFS data volume mounted with prjquota (deploy/README.md, Sizing).")
	}
	if err := s.setDiskSize(r.Context(), p, *a.SizeGB); err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, map[string]any{})
	return nil
}

func (s *Server) platformResizeVolume(w http.ResponseWriter, r *http.Request) error {
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	var in struct {
		VolumeSizeGB *float64 `json:"volume_size_gb"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	if in.VolumeSizeGB == nil {
		return inputError("volume_size_gb is required")
	}
	if err := s.setDiskSize(r.Context(), p, *in.VolumeSizeGB); err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, map[string]any{})
	return nil
}

// setDiskSize changes the size of p's disk to gb, where it can be enforced; elsewhere it refuses
// with the way to make it so.
func (s *Server) setDiskSize(ctx context.Context, p *registry.Project, gb float64) error {
	d, err := s.diskOf(p)
	if err != nil {
		return err
	}
	if gb != float64(int(gb)) || gb < 1 {
		return inputError("The disk size must be a whole number of GB, at least 1")
	}
	size := int(gb)
	if size == d.sizeGB {
		return nil
	}
	if !d.vol.Enforceable() {
		fs := d.vol.FSType
		if fs == "" {
			fs = "an unknown filesystem"
		}
		return inputError("A project's disk size cannot be changed on this node: its data volume (%s) is not XFS mounted with prjquota, so the size shown (%d GB) is the whole volume and is informational. Mount the data volume as XFS with the prjquota option to give projects their own sizes (deploy/README.md, Sizing).", fs, d.sizeGB)
	}
	if need := (d.usedB*12/10 + gib - 1) / gib; int64(size) < need {
		return inputError("The disk size cannot be smaller than what the project holds plus 20%% (%.1f GB used, so at least %d GB)", float64(d.usedB)/float64(gib), need)
	}
	if volGB := int(d.vol.TotalBytes / gib); size > volGB {
		return inputError("The data volume holds %d GB in all, so a project's disk cannot be %d GB", volGB, size)
	}
	if err := s.disk.Set(ctx, p.Ref, p.Seq, size); err != nil {
		if errors.Is(err, diskquota.ErrNotEnforceable) {
			return inputError("%s", capitalize(err.Error()))
		}
		return errf(http.StatusInternalServerError, "Could not set the disk size: %v", err)
	}
	s.diskUse.Delete(p.Ref)
	return nil
}

// ---- project views ----------------------------------------------------------------------------

// infraComputeSize is the project's size as Studio and the platform spec name it.
func infraComputeSize(p *registry.Project) string { return sizeOf(p).Name }
