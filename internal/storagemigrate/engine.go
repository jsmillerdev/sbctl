package storagemigrate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/ctxutil"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/secrets"
)

// Deps are the parts of the node a run drives. Everything but Cfg, Tenants, Storage and Settings
// has a default.
type Deps struct {
	Cfg *config.Config
	Log *slog.Logger
	// Out receives the progress lines the operator reads.
	Out io.Writer
	// Open connects to the bucket; nil is OpenS3.
	Open OpenFunc
	// Files is the objects directory; nil is OSFiles.
	Files Files
	// Tenants reads the databases. Storage and Settings drive the service and its configuration.
	Tenants  Tenants
	Storage  Service
	Settings Settings
	// Reader, when set, reads a few objects back through Storage once it runs on the new backend.
	Reader Reader
	// Now and Sleep are the clock; nil are time.Now and a timer.
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error

	// Workers is how many files are sent at once (default 8). HoldFor is how long a write hold lasts
	// without being renewed (default 2 minutes). VerifyWait is the time between rounds of
	// verification (default 3 s).
	Workers    int
	HoldFor    time.Duration
	VerifyWait time.Duration
}

const (
	// catchUpWithin is how short a pass over the changes must be for the switch to begin, and
	// maxCatchUp how many passes are tried before it begins anyway.
	catchUpWithin = time.Minute
	maxCatchUp    = 5
	// verifyAttempts rounds of verification are made before it fails.
	verifyAttempts = 3
)

// DefaultRateMiB is the copy's speed limit in MiB per second when the command gives none.
const DefaultRateMiB = 32

// Engine runs migrations. It is used by one command at a time.
type Engine struct {
	d     Deps // with the defaults filled in
	paths config.Paths
	lim   *limiter

	// The run in progress. inv is the inventory of each project's last pass; it is empty in a
	// process that continues a run, and the first pass then asks the bucket.
	st    *State
	inv   map[string]inventory
	creds Credentials
	fence bool // the files no longer change: a file that changes is an error
}

// New builds an Engine.
func New(d Deps) *Engine {
	if d.Log == nil {
		d.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if d.Out == nil {
		d.Out = io.Discard
	}
	if d.Files == nil {
		d.Files = OSFiles{}
	}
	if d.Open == nil {
		d.Open = OpenS3
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Sleep == nil {
		d.Sleep = ctxutil.Sleep
	}
	if d.Workers <= 0 {
		d.Workers = 8
	}
	if d.HoldFor <= 0 {
		d.HoldFor = 2 * time.Minute
	}
	if d.VerifyWait <= 0 {
		d.VerifyWait = 3 * time.Second
	}
	return &Engine{d: d, paths: d.Cfg.Paths(), inv: map[string]inventory{}}
}

func (e *Engine) say(format string, args ...any) {
	fmt.Fprintf(e.d.Out, format+"\n", args...)
	e.d.Log.Debug(fmt.Sprintf(format, args...))
}

// Status returns the record of the last run, or nil.
func (e *Engine) Status() (*State, error) { return ReadState(e.paths) }

func (e *Engine) save() error { return saveState(e.paths, e.st, e.d.Now()) }

// destination is where the bucket is, from the request and the [fleet] settings.
func (e *Engine) destination(req Request) Destination {
	f := e.d.Cfg.Fleet
	d := Destination{Bucket: f.StorageS3Bucket, Endpoint: f.StorageS3Endpoint, Region: f.StorageS3Region, PathStyle: f.StorageS3ForcePathStyle}
	if req.Bucket != "" {
		d.Bucket = req.Bucket
	}
	if d.Region == "" {
		d.Region = e.d.Cfg.Backup.S3Region
	}
	if d.Region == "" {
		d.Region = "us-east-1"
	}
	return d
}

func (e *Engine) begin(req Request) {
	e.creds = req.Credentials
	rate := req.RateMiB
	if rate == 0 {
		rate = DefaultRateMiB
	}
	e.lim = &limiter{now: e.d.Now, sleep: e.d.Sleep}
	if rate > 0 {
		e.lim.rate = float64(rate) * (1 << 20)
	}
	e.fence = false
}

// connect opens the bucket and hands it the limiter of the copy.
func (e *Engine) connect(ctx context.Context, d Destination, c Credentials) (Bucket, error) {
	b, err := e.d.Open(ctx, d, c)
	if err != nil {
		return nil, err
	}
	if p, ok := b.(pacer); ok {
		p.pace(e.lim)
	}
	return b, nil
}

// unthrottle lifts the rate limit until the returned function is called: the passes of the switch
// run while writes are held or Storage is stopped, and the limit is there to spare the link of a
// Storage that is serving.
func (e *Engine) unthrottle() (restore func()) {
	old := e.lim.setRate(0)
	return func() { e.lim.setRate(old) }
}

// checkState refuses a record that this command could not have written for this node. It names the
// backend, the directories and the service that --resume and --rollback act on.
func (e *Engine) checkState(st *State) error {
	record := filepath.Join(runDir(e.paths), stateFile)
	if st.PrevBackend != "" && st.PrevBackend != "file" {
		return fmt.Errorf("%s names %q as the backend before the migration, which this command never records", record, st.PrevBackend)
	}
	for _, name := range []string{st.Retained, st.KeepAs} {
		if name != "" && (!strings.HasPrefix(name, retainedPrefix) || strings.ContainsRune(name, filepath.Separator)) {
			return fmt.Errorf("%s names %q as a directory that keeps the files, which is not one this command keeps", record, name)
		}
	}
	if st.Dest.Bucket == "" {
		return fmt.Errorf("%s names no bucket", record)
	}
	if want := e.destination(Request{Bucket: st.Dest.Bucket}); st.Dest != want {
		return fmt.Errorf("the run used %s but [fleet] now says %s: put the settings back, because Storage would use another service than the one the objects went to", st.Dest, want)
	}
	return nil
}

// String describes where the bucket is.
func (d Destination) String() string {
	at := "AWS"
	if d.Endpoint != "" {
		at = d.Endpoint
	}
	style := ""
	if d.PathStyle {
		style = ", path-style"
	}
	return fmt.Sprintf("bucket %q at %s (region %s%s)", d.Bucket, at, d.Region, style)
}

// Migrate starts a run: it checks the bucket and Storage, copies, verifies and switches.
func (e *Engine) Migrate(ctx context.Context, req Request) error {
	release, err := lock(e.paths)
	if err != nil {
		return err
	}
	defer release()
	if prev, err := ReadState(e.paths); err != nil {
		return err
	} else if prev != nil && !prev.Phase.Terminal() {
		return ErrInProgress{prev.Phase}
	}
	backend := e.d.Cfg.Fleet.StorageBackend
	if backend == "s3" {
		return fmt.Errorf("Storage already keeps its objects in the bucket %q; there is nothing to migrate (--rollback undoes a migration)", e.d.Cfg.Fleet.StorageS3Bucket)
	}
	dest := e.destination(req)
	if dest.Bucket == "" {
		return errors.New("no bucket: give --bucket or set [fleet] storage_s3_bucket")
	}
	e.begin(req)
	b, err := e.connect(ctx, dest, req.Credentials)
	if err != nil {
		return err
	}
	if err := e.preflight(ctx, b, dest, req.Credentials); err != nil {
		return err
	}
	e.st = &State{ID: fmt.Sprintf("%x", e.d.Now().UnixNano()), Phase: PhaseCopying, Dest: dest, Credentials: req.Credentials.Source,
		CredentialsFile: req.CredentialsFile, RoleARN: req.Credentials.RoleARN, PrevBackend: backend, StartedAt: e.d.Now().UTC()}
	e.inv = map[string]inventory{}
	if err := e.save(); err != nil {
		return err
	}
	return e.run(ctx, b)
}

// preflight checks what would otherwise fail halfway: that Storage runs, that the credentials can
// write, read and delete in the bucket, and that the switch would take effect and Storage could start.
func (e *Engine) preflight(ctx context.Context, b Bucket, dest Destination, c Credentials) error {
	if err := e.d.Storage.Healthy(ctx); err != nil {
		return fmt.Errorf("Storage is not healthy, so the switch could not be checked afterwards: %w", err)
	}
	if plainRemote(dest.Endpoint) {
		e.say("warning: %s is not TLS: the objects and the signed requests cross the network unencrypted", dest.Endpoint)
	}
	key := fmt.Sprintf(".supavise-migrate-probe/%x", e.d.Now().UnixNano())
	body := []byte("supavise storage migrate")
	if err := b.Put(ctx, key, strings.NewReader(string(body)), int64(len(body)), FileMeta{ContentType: "text/plain"}); err != nil {
		return fmt.Errorf("the bucket does not take a write with these credentials: %w", err)
	}
	rc, _, err := b.Get(ctx, key)
	if err != nil {
		return fmt.Errorf("the bucket does not give back what was written: %w", err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || string(got) != string(body) {
		return fmt.Errorf("the bucket does not give back what was written (%v)", err)
	}
	if err := b.Delete(ctx, key); err != nil {
		return fmt.Errorf("the bucket does not take a delete with these credentials: %w", err)
	}
	return e.checkSwitch(ctx, dest, c)
}

// checkSwitch tries the new configuration without writing it: what the switch would make the node
// load, and the unit of supavise-storage that follows from it. A configuration that fails here would
// otherwise fail after Storage was stopped.
func (e *Engine) checkSwitch(ctx context.Context, dest Destination, c Credentials) error {
	cfg, err := e.d.Settings.Preview(ctx, dest, c)
	if err != nil {
		return fmt.Errorf("the switch would not take effect: %w", err)
	}
	if err := e.d.Storage.Render(ctx, cfg); err != nil {
		return fmt.Errorf("supavise-storage could not start on the bucket with this configuration: %w", err)
	}
	return nil
}

// Resume continues the run that stopped, where it stopped.
func (e *Engine) Resume(ctx context.Context, req Request) error {
	release, err := lock(e.paths)
	if err != nil {
		return err
	}
	defer release()
	st, err := ReadState(e.paths)
	if err != nil {
		return err
	}
	switch {
	case st == nil:
		return errors.New("there is no Storage migration to resume")
	case st.Phase.Terminal():
		return fmt.Errorf("the last Storage migration is finished (%s); there is nothing to resume", st.Phase)
	case req.Bucket != "" && req.Bucket != st.Dest.Bucket:
		return fmt.Errorf("the run in progress uses the bucket %q, not %q", st.Dest.Bucket, req.Bucket)
	}
	if err := e.checkState(st); err != nil {
		return err
	}
	e.st, e.inv = st, map[string]inventory{}
	e.begin(req)
	var b Bucket
	if st.NeedsBucket() {
		if b, err = e.connect(ctx, st.Dest, req.Credentials); err != nil {
			return err
		}
	}
	st.Error = ""
	if st.Phase == PhaseRollingBack {
		return e.rollback(ctx, b)
	}
	return e.run(ctx, b)
}

// run goes through the phases from the one the state names.
func (e *Engine) run(ctx context.Context, b Bucket) error {
	for !e.st.Phase.Terminal() {
		var err error
		switch e.st.Phase {
		case PhaseCopying:
			err = e.copyAll(ctx, b)
		case PhaseCatchingUp:
			err = e.catchUp(ctx, b)
		case PhaseVerifying:
			err = e.verifyAll(ctx, b)
		case PhaseFlipping:
			err = e.flip(ctx, b)
		default:
			err = fmt.Errorf("the run is in the phase %q, which this release does not know", e.st.Phase)
		}
		if err != nil {
			return e.fail(err)
		}
	}
	return nil
}

// fail records why a run stopped. The state keeps its phase, so --resume tries again from there.
func (e *Engine) fail(err error) error {
	e.st.Error = err.Error()
	if errors.Is(err, context.Canceled) {
		e.st.Error = "interrupted"
	}
	if serr := e.save(); serr != nil {
		e.d.Log.Warn("could not save the migration state", "err", serr)
	}
	return err
}

// next moves the run to its next phase and saves.
func (e *Engine) next(p Phase) error {
	e.st.Phase, e.st.Step = p, ""
	return e.save()
}

// diskRefs lists the projects that have a directory of objects.
func (e *Engine) diskRefs() ([]string, error) {
	ents, err := os.ReadDir(filepath.Dir(e.paths.StorageObjects("x")))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var refs []string
	for _, en := range ents {
		switch {
		case !en.IsDir():
		case secrets.ValidRef(en.Name()):
			refs = append(refs, en.Name())
		default:
			e.say("ignoring %s: it is not a project directory", quote(en.Name()))
		}
	}
	return refs, nil
}

func (e *Engine) account(res passResult) {
	e.st.Uploaded.Files += res.Uploaded.Files
	e.st.Uploaded.Bytes += res.Uploaded.Bytes
	e.st.Deleted += res.Deleted
}

// syncOne runs a pass over one project and keeps its inventory.
func (e *Engine) syncOne(ctx context.Context, b Bucket, ref string) error {
	inv, res, err := e.syncTenant(ctx, b, ref, e.inv[ref])
	if err != nil {
		return err
	}
	e.inv[ref] = inv
	e.account(res)
	return nil
}

// pass runs one pass over every project and says how long it took.
func (e *Engine) pass(ctx context.Context, b Bucket, what string) (time.Duration, error) {
	start := e.d.Now()
	refs, err := e.diskRefs()
	if err != nil {
		return 0, err
	}
	// A project whose directory is gone has no files left: its inventory empties and its keys go.
	for ref := range e.inv {
		if !slices.Contains(refs, ref) {
			refs = append(refs, ref)
		}
	}
	sort.Strings(refs)
	var total passResult
	for _, ref := range refs {
		inv, res, err := e.syncTenant(ctx, b, ref, e.inv[ref])
		if err != nil {
			return 0, err
		}
		e.inv[ref] = inv
		total.add(res)
		e.account(res)
	}
	// What a pass leaves out is the same every time; the state keeps the last pass's view.
	e.st.SkippedTotal, e.st.Skipped = len(total.Skipped), first(total.Skipped, maxSkippedKept)
	e.st.Passes++
	dur := e.d.Now().Sub(start)
	files := 0
	for _, inv := range e.inv {
		files += len(inv)
	}
	e.say("%s: %d files in %d projects, %d sent (%s), %d removed from the bucket, %s", what, files, len(refs), total.Uploaded.Files, lifecycle.HumanBytes(total.Uploaded.Bytes), total.Deleted, dur.Round(time.Millisecond))
	return dur, e.save()
}

func (e *Engine) copyAll(ctx context.Context, b Bucket) error {
	if _, err := e.pass(ctx, b, "copy"); err != nil {
		return err
	}
	if e.st.SkippedTotal > 0 {
		e.say("%d files cannot be S3 keys and were left out (--status lists them)", e.st.SkippedTotal)
	}
	return e.next(PhaseCatchingUp)
}

// catchUp repeats passes until one is short: what Storage wrote during a pass is what the next one
// has to send, so a short pass means a short last one inside the switch.
func (e *Engine) catchUp(ctx context.Context, b Bucket) error {
	for i := 1; ; i++ {
		dur, err := e.pass(ctx, b, fmt.Sprintf("catch-up %d", i))
		if err != nil {
			return err
		}
		if dur <= catchUpWithin {
			break
		}
		if i >= maxCatchUp {
			e.say("catch-up passes still take %s after %d passes; the switch will hold writes for about that long", dur.Round(time.Second), i)
			break
		}
	}
	return e.next(PhaseVerifying)
}

// verifyAll checks every project, with a few rounds when Storage takes writes meanwhile.
func (e *Engine) verifyAll(ctx context.Context, b Bucket) error {
	projects, err := e.d.Tenants.Projects(ctx)
	if err != nil {
		return err
	}
	byRef := map[string]Project{}
	for _, p := range projects {
		byRef[p.Ref] = p
	}
	refs, err := e.refs(byRef)
	if err != nil {
		return err
	}
	var tenants []Tenant
	for _, ref := range refs {
		var t Tenant
		for attempt := 1; ; attempt++ {
			t, err = e.verifyTenant(ctx, b, ref, byRef[ref])
			var verr *VerifyError
			if err == nil || !errors.As(err, &verr) || attempt >= verifyAttempts {
				break
			}
			e.say("project %s: %v; checking again", ref, err)
			if serr := e.d.Sleep(ctx, e.d.VerifyWait); serr != nil {
				return serr
			}
		}
		if err != nil {
			return err
		}
		tenants = append(tenants, t)
		e.say("project %s: %d objects (%s) are in the bucket%s", ref, t.Rows, lifecycle.HumanBytes(t.Bytes), t.note())
	}
	e.st.Tenants = tenants
	return e.next(PhaseFlipping)
}

func (t Tenant) note() string {
	var n []string
	if t.Offline {
		n = append(n, "its database is not running, so only the copy was checked")
	}
	if t.Orphans > 0 {
		n = append(n, fmt.Sprintf("%d files have no row and are copied anyway", t.Orphans))
	}
	if t.Unversioned > 0 {
		n = append(n, fmt.Sprintf("%d rows have no version, so their files could not be matched and are not verified", t.Unversioned))
	}
	if t.Extra > 0 {
		n = append(n, fmt.Sprintf("%d objects in the bucket have no file and were left alone", t.Extra))
	}
	if len(n) == 0 {
		return ""
	}
	return " (" + strings.Join(n, "; ") + ")"
}

// refs lists the projects to look at: those with a directory and those the registry knows.
func (e *Engine) refs(byRef map[string]Project) ([]string, error) {
	refs, err := e.diskRefs()
	if err != nil {
		return nil, err
	}
	for ref := range byRef {
		if !slices.Contains(refs, ref) && secrets.ValidRef(ref) {
			refs = append(refs, ref)
		}
	}
	sort.Strings(refs)
	return refs, nil
}
