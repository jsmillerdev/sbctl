package api

import (
	"context"
	"errors"
	"time"
)

// Store is the API server's own state, next to the registry: dashboard users, the
// CLI device-login sessions, Edge Function deployments and secrets, and Studio's
// saved content. The Postgres implementation lives in the registry's database (tables
// created by migrations/0100_api.sql); the memory implementation serves tests.
type Store interface {
	// UpsertUser records a dashboard user seen in a GoTrue token, keeping profile
	// edits already made, and returns the stored row.
	UpsertUser(ctx context.Context, u User) (*User, error)
	GetUser(ctx context.Context, userID string) (*User, error)
	GetUserByID(ctx context.Context, id int64) (*User, error)
	UpdateUser(ctx context.Context, u *User) error

	PutLoginSession(ctx context.Context, s LoginSession) error
	// TakeLoginSession returns the session and deletes it, so a code works once.
	// Expired sessions are reported as ErrNotFound.
	TakeLoginSession(ctx context.Context, sessionID string) (*LoginSession, error)

	UpsertFunction(ctx context.Context, f *Function, files []FunctionFile) error
	ListFunctions(ctx context.Context, ref string) ([]Function, error)
	GetFunction(ctx context.Context, ref, slug string) (*Function, error)
	FunctionFiles(ctx context.Context, ref, slug string) ([]FunctionFile, error)
	DeleteFunction(ctx context.Context, ref, slug string) error

	PutFunctionSecrets(ctx context.Context, ref string, sealed map[string][]byte) error
	ListFunctionSecrets(ctx context.Context, ref string) ([]FunctionSecret, error)
	DeleteFunctionSecrets(ctx context.Context, ref string, names []string) error

	ListContent(ctx context.Context, ref string, q ContentQuery) ([]Content, error)
	GetContent(ctx context.Context, ref, id string) (*Content, error)
	// UpsertContent inserts c (empty ID) or replaces the row with that ID.
	UpsertContent(ctx context.Context, c *Content) error
	DeleteContent(ctx context.Context, ref string, ids []string) ([]string, error)
	CountContent(ctx context.Context, ref string, ownerID int64) (ContentCount, error)
	ListFolders(ctx context.Context, ref string, parentID *string) ([]ContentFolder, error)
	CreateFolder(ctx context.Context, f *ContentFolder) error
	RenameFolder(ctx context.Context, ref, id, name string) error
	DeleteFolders(ctx context.Context, ref string, ids []string) error
}

// ErrNotFound is returned by Store lookups that find nothing.
var ErrNotFound = errors.New("api: not found")

// User is a dashboard user (a GoTrue identity from sb-gotrue@system).
type User struct {
	ID         int64  // numeric id Studio's types carry
	UserID     string // GoTrue uuid
	Email      string
	Username   string
	FirstName  string
	LastName   string
	CreatedAt  time.Time
	LastSeenAt time.Time
}

// LoginSession is one `supabase login` handshake. The token itself is never stored
// in the clear: Ciphertext is the PAT sealed to the CLI's public key.
type LoginSession struct {
	SessionID       string
	UserID          string
	TokenID         int64
	ServerPublicKey string // hex, uncompressed P-256 point
	Nonce           string // hex, 12 bytes; its first 8 characters are the verification code
	Ciphertext      string // hex, AES-256-GCM ciphertext with the tag appended
	ExpiresAt       time.Time
}

// Function is one Edge Function deployment of a project.
type Function struct {
	Ref            string
	Slug           string
	ID             string
	Name           string
	Version        int
	Status         string
	VerifyJWT      bool
	EntrypointPath string
	ImportMapPath  string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// FunctionFile is one uploaded source file of a function.
type FunctionFile struct {
	Path    string
	Content []byte
}

// FunctionSecret is a stored (sealed) Edge Function secret.
type FunctionSecret struct {
	Name      string
	Sealed    []byte
	UpdatedAt time.Time
}

// Content is a Studio saved item: a SQL snippet, report, log query or notebook.
type Content struct {
	ID          string
	Ref         string
	FolderID    *string
	OwnerID     int64
	Type        string
	Name        string
	Description string
	Visibility  string
	Favorite    bool
	Body        []byte // JSON
	InsertedAt  time.Time
	UpdatedAt   time.Time
}

// ContentQuery filters ListContent. Zero values match everything.
type ContentQuery struct {
	Type       string
	Visibility string
	OwnerID    int64
	Favorite   bool
	Name       string // substring match, case-insensitive
	FolderID   *string
	RootOnly   bool // only items without a folder
	Limit      int
}

// ContentCount is the numbers behind Studio's snippet tabs.
type ContentCount struct{ Private, Shared, Favorites int }

// ContentFolder is a folder of saved content.
type ContentFolder struct {
	ID        string
	Ref       string
	ParentID  *string
	OwnerID   int64
	Name      string
	CreatedAt time.Time
	UpdatedAt time.Time
}
