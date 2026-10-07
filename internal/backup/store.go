package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/jsmillerdev/supavise/internal/config"
)

// ErrNotFound is returned by Store.Get and Store.Stat for a missing key.
var ErrNotFound = errors.New("backup: object not found")

// ObjectInfo describes one stored object. Key is relative to the store root.
type ObjectInfo struct {
	Key     string
	Size    int64
	ModTime time.Time
}

// Store is the storage backend behind WAL archiving and base backups. Keys are
// slash-separated and relative to the backend root ("<ref>/wal/<name>.zst"). All
// methods are safe for concurrent use.
type Store interface {
	// Put writes r under key and returns only when the object is durable and visible
	// to Get. Readers never observe a partial object. An existing key is replaced.
	Put(ctx context.Context, key string, r io.Reader) error
	// Get opens key, or returns ErrNotFound.
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	// Stat returns the size of key, or ErrNotFound.
	Stat(ctx context.Context, key string) (ObjectInfo, error)
	// List returns every object whose key starts with prefix, in key order.
	List(ctx context.Context, prefix string) ([]ObjectInfo, error)
	// ListDirs returns the names of the immediate "directories" below prefix, which is
	// empty or ends in "/".
	ListDirs(ctx context.Context, prefix string) ([]string, error)
	// Delete removes keys. Missing keys are not an error.
	Delete(ctx context.Context, keys ...string) error
	// URL is the backend URL of key, as stored in registry.Backup.Location.
	URL(key string) string
}

// TempCleaner is implemented by stores that stage uploads in temporary objects of their
// own, which a crash can leave behind (FileStore). S3 keeps no such objects; its
// abandoned multipart uploads need a bucket lifecycle rule (see the README).
type TempCleaner interface {
	// DeleteStaleTemps removes leftover in-flight objects under prefix (empty or ending
	// in "/") that were last written before cutoff, and returns how many.
	DeleteStaleTemps(ctx context.Context, prefix string, cutoff time.Time) (int, error)
}

// OpenStore builds the Store named by cfg.Backend ("file:///dir" or "s3://bucket/prefix").
func OpenStore(ctx context.Context, cfg config.Backup) (Store, error) {
	u, err := url.Parse(cfg.Backend)
	if err != nil {
		return nil, fmt.Errorf("backup: backend %q: %w", cfg.Backend, err)
	}
	switch u.Scheme {
	case "file":
		if u.Host != "" || u.Path == "" || !path.IsAbs(u.Path) {
			return nil, fmt.Errorf("backup: backend %q: want file:///absolute/dir", cfg.Backend)
		}
		return NewFileStore(u.Path)
	case "s3":
		if u.Host == "" {
			return nil, fmt.Errorf("backup: backend %q: want s3://bucket/prefix", cfg.Backend)
		}
		return NewS3Store(ctx, S3Options{
			Bucket:         u.Host,
			Prefix:         strings.Trim(u.Path, "/"),
			Endpoint:       cfg.S3Endpoint,
			Region:         cfg.S3Region,
			ForcePathStyle: cfg.S3ForcePathStyle,
			AccessKeyID:    cfg.S3AccessKeyID,
			SecretKey:      cfg.S3SecretAccessKey,
		})
	case "":
		return nil, errors.New("backup: backend is not configured (backup.backend)")
	}
	return nil, fmt.Errorf("backup: backend %q: unsupported scheme %q (use file:// or s3://)", cfg.Backend, u.Scheme)
}

// validKey rejects keys that could escape the store root or confuse a backend.
func validKey(key string) error {
	if key == "" || strings.HasPrefix(key, "/") || strings.Contains(key, "\\") || strings.ContainsRune(key, 0) {
		return fmt.Errorf("backup: invalid key %q", key)
	}
	for _, seg := range strings.Split(key, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("backup: invalid key %q", key)
		}
	}
	return nil
}

// ctxReader aborts a copy when ctx ends.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
