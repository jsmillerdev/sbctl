package fleet

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/supavise/supavise/internal/awsapi"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/fsutil"
)

// supavise-storage cannot use the instance role: IMDS is denied to it, because Storage runs on
// user input and holds the objects of every project. On AWS it gets short-lived credentials of
// its own instead. [fleet] storage_s3_role_arn names an IAM role (the stack's StorageRole, whose
// policy covers the objects bucket and nothing else); the daemon assumes it through STS with the
// instance's credentials and serves the result on a loopback endpoint in the shape of the ECS
// container credentials. Storage's AWS SDK finds the endpoint through
// AWS_CONTAINER_CREDENTIALS_FULL_URI, authenticates with AWS_CONTAINER_AUTHORIZATION_TOKEN, caches
// the credentials and asks again shortly before they expire. No access key is stored anywhere.
const (
	storageCredentialsPath = "/credentials"
	// storageSessionName names the STS session in CloudTrail.
	storageSessionName = "supavise-storage"
	// storageCredentialsTTL is the lifetime asked of each AssumeRole, the default maximum of a role.
	storageCredentialsTTL = time.Hour
	// credentialsWarnEvery bounds how often a failing STS call is logged: Storage retries a refused
	// request ten times.
	credentialsWarnEvery = 30 * time.Second
	// credentialsCallTimeout bounds one call to STS. The call has a context of its own, not the
	// request's: a client that gives up (the container-credential fetch of an AWS SDK has a short
	// timeout) must not cancel the call in flight, or a first call slower than that timeout would be
	// cancelled by every retry and the cache would never fill.
	credentialsCallTimeout = 15 * time.Second
)

// StorageCredentialsURL is the endpoint supavise-storage is pointed at.
func StorageCredentialsURL(cfg *config.Config) string {
	return "http://" + storageCredentialsAddr(cfg) + storageCredentialsPath
}

func storageCredentialsAddr(cfg *config.Config) string {
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.Fleet.StorageCredentials()))
}

// ListenStorageCredentials binds the endpoint's loopback address. The daemon binds it before it
// renders Storage's unit, so the first request already finds it.
func ListenStorageCredentials(cfg *config.Config) (net.Listener, error) {
	return net.Listen("tcp", storageCredentialsAddr(cfg))
}

// ---- the token ----------------------------------------------------------------------------

// credentialTokenPath is the file that holds the token of this boot. It sits in Storage's own
// state directory, which no other shared service sees (deploy/systemd/README.md).
func credentialTokenPath(cfg *config.Config) string {
	return filepath.Join(cfg.Paths().System(config.SvcStorage), "credentials.json")
}

type tokenFile struct {
	// Boot is the kernel's boot id when the token was made. A token from an earlier boot is
	// replaced; one from this boot is kept, so that a daemon restart or a CLI render finds the
	// token Storage already runs with and restarts nothing.
	Boot  string `json:"boot"`
	Token string `json:"token"`
}

// bootID is replaced in tests. Where the kernel has none (a development machine) it is empty,
// and the token then lasts until the file is deleted.
var bootID = func() string {
	b, _ := os.ReadFile("/proc/sys/kernel/random/boot_id")
	return strings.TrimSpace(string(b))
}

// StorageCredentialToken returns the token supavise-storage presents to the credential endpoint:
// random, per boot, and kept in a 0600 file next to Storage's state. The daemon (which checks it)
// and any process that renders Storage's environment (which hands it over) read the same file; a
// lock makes the first creation happen once.
func StorageCredentialToken(cfg *config.Config) (string, error) {
	path := credentialTokenPath(cfg)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return "", err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if errors.Is(err, fs.ErrPermission) {
		// A file another user made: a render run as root creates it root:root 0600, and the daemon
		// cannot open it, so its endpoint would never start. The directory is the daemon's, and a
		// directory's owner may replace what is in it; the token that file held is replaced with
		// the new one, which the next render hands to Storage (one restart of Storage, once).
		if rerr := os.Remove(path); rerr != nil {
			return "", fmt.Errorf("fleet: credential token: %s belongs to another user and cannot be replaced: %w", path, rerr)
		}
		f, err = os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	}
	if err != nil {
		return "", fmt.Errorf("fleet: credential token: %w", err)
	}
	defer f.Close()
	if os.Geteuid() == 0 {
		// A token made by a render that was run with sudo can be read by the daemon, which runs as
		// the unit's user and owns the directory.
		if uid, gid, ok := fsutil.OwnerOf(filepath.Dir(path)); ok {
			_ = f.Chown(uid, gid)
		}
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return "", fmt.Errorf("fleet: credential token: %w", err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck // closing the file releases the lock anyway
	boot := bootID()
	var cur tokenFile
	if b, err := os.ReadFile(path); err == nil && json.Unmarshal(b, &cur) == nil && cur.Token != "" && cur.Boot == boot {
		return cur.Token, nil
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	b, err := json.Marshal(tokenFile{Boot: boot, Token: hex.EncodeToString(raw)})
	if err != nil {
		return "", err
	}
	if err := f.Truncate(0); err != nil {
		return "", err
	}
	if _, err := f.WriteAt(b, 0); err != nil {
		return "", err
	}
	// A file made by an earlier version of this code, or by hand, may be looser than 0600.
	if err := f.Chmod(0o600); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

// ---- the endpoint -------------------------------------------------------------------------

// StorageCredentials answers supavise-storage's credential requests.
type StorageCredentials struct {
	token string
	arn   string
	src   awsapi.CredentialProvider
	log   *slog.Logger
	now   func() time.Time

	mu       sync.Mutex
	lastWarn time.Time
}

// CredentialsOptions configures NewStorageCredentials.
type CredentialsOptions struct {
	Cfg *config.Config
	Log *slog.Logger
	// AWS is the client that calls STS with the instance's credentials. Nil builds one from the
	// environment (awsapi.New), which finds the instance role.
	AWS *awsapi.Client
	// Token is the token to require. Empty reads (or makes) the token of this boot,
	// StorageCredentialToken.
	Token string
	// Now is the clock for the Expiration the endpoint answers; nil is time.Now.
	Now func() time.Time
}

// NewStorageCredentials builds the endpoint for [fleet] storage_s3_role_arn. The credentials are
// assumed on the first request and again when five minutes of their hour are left
// (awsapi.CachedCredentials); a renewal that fails keeps the old ones for as long as they work.
func NewStorageCredentials(o CredentialsOptions) (*StorageCredentials, error) {
	arn := o.Cfg.Fleet.StorageS3RoleARN
	if arn == "" {
		return nil, errors.New("fleet: [fleet] storage_s3_role_arn is not set")
	}
	tok := o.Token
	if tok == "" {
		var err error
		if tok, err = StorageCredentialToken(o.Cfg); err != nil {
			return nil, err
		}
	}
	c := o.AWS
	if c == nil {
		var err error
		if c, err = awsapi.New(awsapi.Config{}); err != nil {
			return nil, err
		}
	}
	log := o.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	return &StorageCredentials{
		token: tok, arn: arn, log: log, now: now,
		src: c.STS.RoleCredentials(awsapi.AssumeRoleInput{RoleARN: arn, SessionName: storageSessionName, Duration: storageCredentialsTTL}),
	}, nil
}

// containerCredentials is the body ECS and EKS answer, which every AWS SDK reads.
type containerCredentials struct {
	AccessKeyID     string `json:"AccessKeyId"`
	SecretAccessKey string `json:"SecretAccessKey"`
	Token           string `json:"Token"`
	Expiration      string `json:"Expiration"`
	RoleArn         string `json:"RoleArn"`
}

// ServeHTTP answers a GET of the credentials path from a loopback address that presents the
// token, and refuses everyone else with 403 and no body: a refusal says nothing about whether the
// role exists or what it may do.
func (s *StorageCredentials) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !loopbackAddr(r.RemoteAddr) || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte(s.token)) != 1 {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	if r.URL.Path != storageCredentialsPath {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	c, err := s.retrieve(r.Context())
	if err != nil {
		if r.Context().Err() == nil {
			s.warn(err)
		}
		http.Error(w, "the role's credentials are not available", http.StatusBadGateway)
		return
	}
	body := containerCredentials{
		AccessKeyID: c.AccessKeyID, SecretAccessKey: c.SecretAccessKey, Token: c.SessionToken, RoleArn: s.arn,
		Expiration: c.Expires.UTC().Format(time.RFC3339),
	}
	if c.Expires.IsZero() {
		body.Expiration = s.now().Add(storageCredentialsTTL).UTC().Format(time.RFC3339)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(body)
}

// retrieve asks the source for the role's credentials on a context that ends after
// credentialsCallTimeout and not with the request: when the client leaves, the call goes on and
// fills the cache for the next request. The request is answered when the call is, or when the
// client's context ends.
func (s *StorageCredentials) retrieve(ctx context.Context) (awsapi.Credentials, error) {
	type result struct {
		c   awsapi.Credentials
		err error
	}
	done := make(chan result, 1)
	call, cancel := context.WithTimeout(context.WithoutCancel(ctx), credentialsCallTimeout)
	go func() {
		defer cancel()
		c, err := s.src.Retrieve(call)
		done <- result{c, err}
	}()
	select {
	case r := <-done:
		return r.c, r.err
	case <-ctx.Done():
		return awsapi.Credentials{}, ctx.Err()
	}
}

// warm assumes the role once, so that the first request of supavise-storage after the daemon starts
// finds the credentials in the cache, and a role the node cannot assume shows in the log at start.
func (s *StorageCredentials) warm() {
	if _, err := s.retrieve(context.Background()); err != nil {
		s.warn(err)
	}
}

func (s *StorageCredentials) warn(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t := s.now(); t.Sub(s.lastWarn) >= credentialsWarnEvery {
		s.lastWarn = t
		s.log.Warn("storage credentials: assuming the role failed; supavise-storage cannot reach its bucket until it works", "role", s.arn, "cause", causeOf(err), "error", err)
	}
}

// causeOf names the usual reasons the instance's own credentials, which AssumeRole is signed with,
// are missing. awsapi's errors are compared with errors.Is, never by their text.
func causeOf(err error) string {
	switch {
	case errors.Is(err, awsapi.ErrNoCredentials):
		return "this node has no AWS credentials to assume the role with: it needs an instance role"
	case errors.Is(err, awsapi.ErrIMDSDisabled):
		return "the instance metadata service is disabled for the daemon, so it has no instance role"
	case errors.Is(err, awsapi.ErrIMDSUnreachable):
		return "the instance metadata service did not answer"
	}
	return ""
}

// Serve answers on ln until ctx ends.
func (s *StorageCredentials) Serve(ctx context.Context, ln net.Listener) error {
	go s.warm()
	srv := &http.Server{Handler: s, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: time.Minute,
		ErrorLog: slog.NewLogLogger(s.log.Handler(), slog.LevelDebug)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	err := srv.Serve(ln)
	<-done
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func loopbackAddr(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
