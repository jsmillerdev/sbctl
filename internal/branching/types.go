package branching

import (
	"crypto/rand"
	"crypto/sha1"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/OWNER/sbctl/internal/registry"
)

// DefaultBranchName is the name the Management API gives the branch that is the project
// itself. The parent project is reported as a default branch so that clients that look
// for one (Studio, the CLI) find it; it cannot be created, renamed or deleted.
const DefaultBranchName = "main"

// Branch is the service-level view of a branch: registry fields plus the project's status.
type Branch struct {
	ID        string
	Name      string
	Ref       string // the branch's own project ref
	ParentRef string
	IsDefault bool
	GitBranch string

	Persistent bool
	WithData   bool
	// State is the Management API's branch status: it follows the last long operation.
	State registry.BranchState
	// ProjectStatus is the status of the branch's project (its units).
	ProjectStatus registry.Status
	// CloneMethod is how the data was obtained: schema, clonefile, reflink, zfs-snapshot or base-backup.
	CloneMethod string
	// Detail is the outcome of the last operation, an error text after a failure.
	Detail string

	CreatedAt           time.Time
	UpdatedAt           time.Time
	ExpiresAt           *time.Time
	DeletionScheduledAt *time.Time
	ReviewRequestedAt   *time.Time
	NotifyURL           string
}

var (
	// ErrNotFound is returned when a branch, or the project it belongs to, does not exist.
	ErrNotFound = errors.New("branching: branch not found")
	// ErrInvalid marks a request the caller can fix (a bad name, an unsupported combination).
	ErrInvalid = errors.New("branching: invalid request")
	// ErrConflict marks a request that clashes with the current state (name taken, another
	// operation running, divergence without force).
	ErrConflict = errors.New("branching: conflict")
	// ErrLimit is returned when creating a branch would exceed the configured caps.
	ErrLimit = errors.New("branching: branch limit reached")
	// ErrDiverged is returned by merge and push when the two histories disagree and force is not set.
	ErrDiverged = fmt.Errorf("%w: migration histories diverged", ErrConflict)
)

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

func conflict(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrConflict, fmt.Sprintf(format, a...))
}

// A branch name is what a git branch name can be (slashes included), minus anything that
// is awkward in a URL path or a log line.
var nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,99}$`)

// ValidName checks a branch name.
func ValidName(name string) error {
	switch {
	case !nameRE.MatchString(name):
		return invalid("branch name %q: use letters, digits and . _ / - (1 to 100 characters, starting with a letter or digit)", name)
	case strings.Contains(name, "..") || strings.HasSuffix(name, "/") || strings.HasSuffix(name, "."):
		return invalid("branch name %q is not allowed", name)
	case name == DefaultBranchName:
		return invalid("%q is the name of the default branch", DefaultBranchName)
	}
	return nil
}

// newUUID returns a random (version 4) UUID.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmtUUID(b)
}

// defaultBranchID derives the stable id of a project's default branch (a version 5 style
// UUID over the ref), so that repeated listings agree.
func defaultBranchID(ref string) string {
	h := sha1.Sum([]byte("sbctl-default-branch:" + ref))
	var b [16]byte
	copy(b[:], h[:16])
	b[6] = b[6]&0x0f | 0x50
	b[8] = b[8]&0x3f | 0x80
	return fmtUUID(b)
}

func fmtUUID(b [16]byte) string {
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

var uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// IsUUID reports whether s looks like a branch id.
func IsUUID(s string) bool { return uuidRE.MatchString(s) }

func branchOf(p *registry.Project) *Branch {
	b := p.Branch
	return &Branch{
		ID: b.ID, Name: b.Name, Ref: p.Ref, ParentRef: b.ParentRef, GitBranch: b.GitBranch,
		Persistent: b.Persistent, WithData: b.WithData, State: b.State, ProjectStatus: p.Status,
		CloneMethod: b.CloneMethod, Detail: b.Detail, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
		ExpiresAt: b.ExpiresAt, DeletionScheduledAt: b.DeletionScheduledAt, ReviewRequestedAt: b.ReviewRequestedAt, NotifyURL: b.NotifyURL,
	}
}

// defaultBranchOf is the synthesized default branch of a parent project.
func defaultBranchOf(p *registry.Project) *Branch {
	return &Branch{
		ID: defaultBranchID(p.Ref), Name: DefaultBranchName, Ref: p.Ref, ParentRef: p.Ref, IsDefault: true, Persistent: true,
		State: registry.BranchMigrationsPassed, ProjectStatus: p.Status, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
	}
}
