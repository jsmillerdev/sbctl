// Package storagemigrate moves Storage's objects between its file backend and an S3 bucket
// (`supavise storage migrate`, design 2.11). The copy runs while Storage keeps serving from the
// files; the switch holds Storage's writes, copies what changed, stops supavise-storage, copies
// once more from files that no longer change, points the configuration at the bucket and starts
// Storage again. The run's state is a JSON file, so a run that stopped continues with --resume.
package storagemigrate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

// Phase is where a run stands.
type Phase string

const (
	PhaseCopying     Phase = "copying"      // the first copy; Storage serves from files
	PhaseCatchingUp  Phase = "catching_up"  // passes over what changed meanwhile
	PhaseVerifying   Phase = "verifying"    // every object the databases list is in the bucket
	PhaseFlipping    Phase = "flipping"     // writes held, last passes, Storage restarted on the bucket
	PhaseDone        Phase = "done"         // Storage uses the bucket
	PhaseRollingBack Phase = "rolling_back" // the same switch the other way
	PhaseRolledBack  Phase = "rolled_back"  // Storage uses files again
)

// Terminal reports whether a run in this phase has nothing left to do.
func (p Phase) Terminal() bool { return p == PhaseDone || p == PhaseRolledBack }

// Steps of the switch, in order. A run that stops in one repeats it: every step can run twice.
const (
	stepHold     = "hold"     // Storage's writes get 503 Retry-After
	stepRestore  = "restore"  // rollback only: the kept files are moved back
	stepFinal    = "final"    // a pass while Storage still runs
	stepStopped  = "stopped"  // supavise-storage is stopped
	stepFence    = "fence"    // a pass over files nothing writes to, and the last check of the databases
	stepSwitched = "switched" // the configuration names the new backend
	stepStarted  = "started"  // Storage runs on the new backend and serves a sample
)

// Destination is the bucket and how to reach it. It holds no secret.
type Destination struct {
	Bucket    string `json:"bucket"`
	Endpoint  string `json:"endpoint,omitempty"` // empty for AWS
	Region    string `json:"region"`
	PathStyle bool   `json:"path_style,omitempty"`
}

// Credential sources.
const (
	CredFile   = "file"   // an access key from a file only its reader may open
	CredRole   = "role"   // an IAM role the node assumes
	CredConfig = "config" // the static key already in the [fleet] settings
)

// Credentials say how to sign requests to the bucket. The secret never reaches a log or a state
// file; String shows the source only.
type Credentials struct {
	Source          string
	AccessKeyID     string
	SecretAccessKey string
	RoleARN         string
}

func (c Credentials) String() string   { return "credentials(" + c.Source + ")" }
func (c Credentials) GoString() string { return c.String() }

// Counts is a number of objects and their bytes.
type Counts struct {
	Files int   `json:"files"`
	Bytes int64 `json:"bytes"`
}

// Skipped is a file that cannot be an S3 key and was left out.
type Skipped struct {
	Path   string `json:"path"` // quoted, relative to the objects directory
	Reason string `json:"reason"`
}

// maxSkippedKept bounds the list the state file keeps; the count is kept in full.
const maxSkippedKept = 50

// Tenant is what a verification found for one project.
type Tenant struct {
	Ref     string `json:"ref"`
	Rows    int    `json:"rows"`  // objects the project's database lists
	Bytes   int64  `json:"bytes"` // their size
	Files   int    `json:"files"` // files in the project's directory
	Orphans int    `json:"orphans,omitempty"`
	// Extra counts keys in the bucket that no file matches; they are left alone.
	Extra   int  `json:"extra,omitempty"`
	Offline bool `json:"offline,omitempty"` // the database was not running: only the copy was checked
}

// State is the persistent record of a run.
type State struct {
	ID          string      `json:"id"`
	Phase       Phase       `json:"phase"`
	Step        string      `json:"step,omitempty"`
	Dest        Destination `json:"destination"`
	Credentials string      `json:"credentials"` // a Cred* source
	// CredentialsFile and RoleARN say where the credentials came from, for --resume. The file's
	// content is never copied.
	CredentialsFile string `json:"credentials_file,omitempty"`
	RoleARN         string `json:"role_arn,omitempty"`
	// WroteCredentials is set when the switch put the credentials into a file of its own under
	// config.d; going back removes that file.
	WroteCredentials bool `json:"wrote_credentials,omitempty"`
	// PrevBackend is [fleet] storage_backend before the switch.
	PrevBackend string `json:"previous_backend,omitempty"`

	StartedAt time.Time `json:"started_at"`
	UpdatedAt time.Time `json:"updated_at"`
	FlippedAt time.Time `json:"flipped_at,omitzero"`
	PID       int       `json:"pid,omitempty"`

	Passes   int       `json:"passes"`
	Uploaded Counts    `json:"uploaded"`
	Deleted  int       `json:"deleted"`
	Tenants  []Tenant  `json:"tenants,omitempty"`
	Skipped  []Skipped `json:"skipped,omitempty"`
	// SkippedTotal counts every file left out, Skipped lists the first few.
	SkippedTotal int `json:"skipped_total,omitempty"`

	// Retained is the directory (under system/storage) that holds the files after the switch;
	// RetainUntil is when the advice to keep it ends. Cleaned is set once it was deleted.
	Retained    string    `json:"retained,omitempty"`
	RetainUntil time.Time `json:"retain_until,omitzero"`
	Cleaned     bool      `json:"cleaned,omitempty"`

	// Downloaded and Removed count what a rollback changed in the files.
	Downloaded Counts `json:"downloaded,omitzero"`
	Removed    int    `json:"removed,omitempty"`

	// Error is why the last attempt stopped.
	Error string `json:"error,omitempty"`
}

// RetainFor is how long the files stay after a switch (design 2.11).
const RetainFor = 14 * 24 * time.Hour

// Entry is an object of the bucket as a listing shows it.
type Entry struct {
	Key     string
	Size    int64
	ModTime time.Time
}

// FileMeta is what Storage's file backend keeps in a file's extended attributes and its S3
// backend keeps in the object's headers.
type FileMeta struct {
	ContentType  string
	CacheControl string
}

// ErrNotFound is what a Bucket returns for a key that does not exist.
var ErrNotFound = errors.New("storagemigrate: no such object")

// Bucket is the part of an S3 bucket the migration uses.
type Bucket interface {
	// Put stores the first size bytes of src under key with meta. src can be read more than once.
	Put(ctx context.Context, key string, src io.ReaderAt, size int64, meta FileMeta) error
	// List calls fn for every object whose key starts with prefix, in key order.
	List(ctx context.Context, prefix string, fn func(Entry) error) error
	// Get opens the object; ErrNotFound when there is none.
	Get(ctx context.Context, key string) (io.ReadCloser, FileMeta, error)
	// Delete removes the keys; a key that does not exist is not an error.
	Delete(ctx context.Context, keys ...string) error
}

// OpenFunc connects to the bucket.
type OpenFunc func(ctx context.Context, d Destination, c Credentials) (Bucket, error)

// Project is a project that may have a Storage tenant.
type Project struct {
	Ref string
	// Online is true when its database is running, so that its rows can be read.
	Online bool
}

// Row is one row of storage.objects.
type Row struct {
	Bucket, Name, Version string
	Size                  int64
	HasSize               bool
}

// ErrOffline is what Tenants.Rows returns for a project whose database is not running.
var ErrOffline = errors.New("storagemigrate: the project's database is not running")

// Tenants reads what the projects' databases say about their objects.
type Tenants interface {
	// Projects lists the projects, the system project excluded.
	Projects(ctx context.Context) ([]Project, error)
	// Rows calls fn for every row of the project's storage.objects.
	Rows(ctx context.Context, ref string, fn func(Row) error) error
}

// Service is the shared Storage service the switch restarts.
type Service interface {
	// Healthy returns nil when supavise-storage runs and answers.
	Healthy(ctx context.Context) error
	// Stop stops supavise-storage; a service that does not run is not an error.
	Stop(ctx context.Context) error
	// Start renders the unit from the configuration as it is on disk now, starts it and waits
	// until it answers.
	Start(ctx context.Context) error
}

// Settings changes the node's configuration files.
type Settings interface {
	// UseBucket makes the configuration say that Storage keeps its objects in d with c. It reports
	// whether it wrote the credentials into a file of its own.
	UseBucket(ctx context.Context, d Destination, c Credentials) (wroteCredentials bool, err error)
	// UseFiles makes it say that Storage keeps them as files (previous is the backend to restore).
	// removeCredentials removes the file UseBucket wrote.
	UseFiles(ctx context.Context, previous string, removeCredentials bool) error
}

// Reader reads an object through the running Storage service, as a client would.
type Reader interface {
	// Read fetches the object and returns its size.
	Read(ctx context.Context, ref string, row Row) (int64, error)
}

// Request is what a command asks of a run.
type Request struct {
	// Bucket overrides the bucket of the [fleet] settings.
	Bucket string
	// Credentials are how to reach it.
	Credentials Credentials
	// CredentialsFile and RoleARN are remembered for --resume.
	CredentialsFile string
	// RateMiB limits the copy to this many MiB per second; zero uses the default and a negative
	// number lifts the limit.
	RateMiB int
}

// ErrInProgress is returned when a run exists that has not finished.
type ErrInProgress struct{ Phase Phase }

func (e ErrInProgress) Error() string {
	return fmt.Sprintf("a Storage migration is in progress (%s): continue it with --resume, look at it with --status", e.Phase)
}
