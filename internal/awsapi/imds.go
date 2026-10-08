package awsapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrNotFound is the metadata service's 404: the path is not there, for example the public IP of
// an instance without one.
var ErrNotFound = errors.New("not found")

// ErrIMDSDisabled means AWS_EC2_METADATA_DISABLED=true. `supavise-aws-deploy.sh` sets it so that
// the stack step never uses the instance role. It turns off every read, the instance id included;
// SUPAVISE_AWS_NO_INSTANCE_ROLE=1 turns off the instance role alone.
var ErrIMDSDisabled = errors.New("the instance metadata service is disabled (AWS_EC2_METADATA_DISABLED=true)")

// ErrIMDSUnreachable means the metadata service gave no answer: no route, a refused connection or
// a timeout, as on a machine that is not on EC2. The client remembers it for a short while and
// answers with it at once, so that every call does not spend the full retries finding out again.
var ErrIMDSUnreachable = errors.New("the instance metadata service did not answer")

const (
	imdsTokenTTL    = 6 * time.Hour
	imdsTokenMargin = time.Minute
	imdsTimeout     = 2 * time.Second
	imdsMaxBytes    = 1 << 20
	// imdsDownMemo is how long an unreachable service is not asked again.
	imdsDownMemo = 30 * time.Second
)

// IMDS reads the instance metadata service with IMDSv2: every read carries a session token that
// is fetched first and reused until shortly before it expires. There is no IMDSv1 fallback.
type IMDS struct {
	base   string
	hc     *http.Client
	getenv func(string) string
	now    func() time.Time
	retry  *retrier

	mu      sync.Mutex
	token   string
	expires time.Time
	// down is the last failure to get any answer and downAt when it happened.
	down   *unreachableError
	downAt time.Time
}

// unreachableError is a request that got no answer from the metadata service. It matches
// ErrIMDSUnreachable and keeps the transport error underneath.
type unreachableError struct{ err error }

func (e *unreachableError) Error() string   { return ErrIMDSUnreachable.Error() + ": " + e.err.Error() }
func (e *unreachableError) Unwrap() []error { return []error{ErrIMDSUnreachable, e.err} }

func newIMDS(cfg Config, base string, attempts int) *IMDS {
	if base == "" {
		base = defaultIMDSEndpoint
	}
	// The metadata address is link-local: a proxy configured for other traffic must not see it.
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	tr.DialContext = (&net.Dialer{Timeout: imdsTimeout}).DialContext
	return &IMDS{
		base:   base,
		hc:     &http.Client{Transport: tr, Timeout: imdsTimeout},
		getenv: cfg.Getenv,
		now:    cfg.Now,
		retry:  newRetrier(attempts, cfg.RetryBackoff),
	}
}

// Get returns the body of path, such as "/latest/meta-data/instance-id". A missing path is
// ErrNotFound. A service that gave no answer is ErrIMDSUnreachable, and is not asked again for 30
// seconds: the next reads fail at once with the same error.
func (m *IMDS) Get(ctx context.Context, path string) (string, error) {
	if strings.EqualFold(m.getenv("AWS_EC2_METADATA_DISABLED"), "true") {
		return "", fmt.Errorf("aws imds %s: %w", path, ErrIMDSDisabled)
	}
	if err := m.recall(path); err != nil {
		return "", err
	}
	body, err := m.getWithRetries(ctx, path)
	var down *unreachableError
	if errors.As(err, &down) {
		m.mu.Lock()
		m.down, m.downAt = down, m.now()
		m.mu.Unlock()
	}
	return body, err
}

// recall returns the error for a service that failed to answer a short while ago, or nil.
func (m *IMDS) recall(path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down == nil {
		return nil
	}
	ago := m.now().Sub(m.downAt)
	if ago < 0 || ago >= imdsDownMemo {
		return nil
	}
	return fmt.Errorf("aws imds %s: %w (%s ago; not asked again for %s)", path, m.down, ago.Round(time.Second), (imdsDownMemo - ago).Round(time.Second))
}

func (m *IMDS) getWithRetries(ctx context.Context, path string) (string, error) {
	var lastErr error
	for attempt := 1; attempt <= m.retry.attempts; attempt++ {
		if attempt > 1 {
			if werr := m.retry.wait(ctx, attempt-1); werr != nil {
				return "", fmt.Errorf("%w: %w", lastErr, werr)
			}
		}
		body, retry, err := m.get(ctx, path)
		if err == nil || !retry || ctx.Err() != nil {
			return body, err
		}
		lastErr = err
	}
	return "", lastErr
}

// get makes one request and says whether another try could help.
func (m *IMDS) get(ctx context.Context, path string) (body string, retry bool, err error) {
	for refreshed := false; ; refreshed = true {
		tok, err := m.sessionToken(ctx)
		if err != nil {
			return "", !errors.Is(err, errIMDSRefused), err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.base+path, nil)
		if err != nil {
			return "", false, err
		}
		req.Header.Set("X-aws-ec2-metadata-token", tok)
		b, status, err := m.send(req)
		if err != nil {
			return "", true, fmt.Errorf("aws imds %s: %w", path, err)
		}
		switch {
		case status == http.StatusUnauthorized && !refreshed:
			m.forgetToken(tok)
			continue
		case status == http.StatusNotFound:
			return "", false, fmt.Errorf("aws imds %s: %w", path, ErrNotFound)
		case status/100 == 2:
			return b, false, nil
		}
		return "", status >= 500, fmt.Errorf("aws imds %s: status %d", path, status)
	}
}

var (
	errIMDSRefused = errors.New("the instance metadata service refused a session token")
	errNoRole      = errors.New("aws imds role credentials: the instance has no IAM role")
)

func (m *IMDS) sessionToken(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.token != "" && m.now().Before(m.expires) {
		return m.token, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, m.base+"/latest/api/token", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-aws-ec2-metadata-token-ttl-seconds", strconv.Itoa(int(imdsTokenTTL/time.Second)))
	b, status, err := m.send(req)
	if err != nil {
		return "", fmt.Errorf("aws imds token: %w", err)
	}
	switch {
	case status/100 == 2 && b != "":
		m.token, m.expires = b, m.now().Add(imdsTokenTTL-imdsTokenMargin)
		return b, nil
	case status == http.StatusForbidden, status == http.StatusNotFound, status == http.StatusMethodNotAllowed:
		return "", fmt.Errorf("aws imds token: status %d: %w", status, errIMDSRefused)
	}
	return "", fmt.Errorf("aws imds token: status %d", status)
}

func (m *IMDS) forgetToken(tok string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.token == tok {
		m.token = ""
	}
}

// send makes one request. A failure to get an answer is an *unreachableError, unless the caller's
// context ended, which says nothing about the service.
func (m *IMDS) send(req *http.Request) (string, int, error) {
	resp, err := m.hc.Do(req)
	if err != nil {
		return "", 0, m.noAnswer(req, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, imdsMaxBytes))
	if err != nil {
		return "", 0, m.noAnswer(req, err)
	}
	return strings.TrimSpace(string(b)), resp.StatusCode, nil
}

func (m *IMDS) noAnswer(req *http.Request, err error) error {
	if req.Context().Err() != nil {
		return err
	}
	return &unreachableError{err}
}

// InstanceID is this instance's id.
func (m *IMDS) InstanceID(ctx context.Context) (string, error) {
	return m.Get(ctx, "/latest/meta-data/instance-id")
}

// Region is the region this instance runs in.
func (m *IMDS) Region(ctx context.Context) (string, error) {
	return m.Get(ctx, "/latest/meta-data/placement/region")
}

// AvailabilityZone is the zone this instance runs in.
func (m *IMDS) AvailabilityZone(ctx context.Context) (string, error) {
	return m.Get(ctx, "/latest/meta-data/placement/availability-zone")
}

// PublicIPv4 is the public address of the instance's primary interface, or "" when it has none.
func (m *IMDS) PublicIPv4(ctx context.Context) (string, error) {
	ip, err := m.Get(ctx, "/latest/meta-data/public-ipv4")
	if errors.Is(err, ErrNotFound) {
		return "", nil
	}
	return ip, err
}

// LocalIPv4 is the primary private address of the instance.
func (m *IMDS) LocalIPv4(ctx context.Context) (string, error) {
	return m.Get(ctx, "/latest/meta-data/local-ipv4")
}

// Tags returns the instance's tags. The service lists them only when the instance allows tags in
// metadata (InstanceMetadataTags), so an instance without them and an instance that hides them
// both give an empty map.
func (m *IMDS) Tags(ctx context.Context) (map[string]string, error) {
	list, err := m.Get(ctx, "/latest/meta-data/tags/instance")
	tags := map[string]string{}
	if errors.Is(err, ErrNotFound) {
		return tags, nil
	}
	if err != nil {
		return nil, err
	}
	for _, key := range strings.Fields(list) {
		v, err := m.Get(ctx, "/latest/meta-data/tags/instance/"+url.PathEscape(key))
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		tags[key] = v
	}
	return tags, nil
}

// RoleCredentials returns the temporary credentials of the instance's role.
func (m *IMDS) RoleCredentials(ctx context.Context) (Credentials, error) {
	const dir = "/latest/meta-data/iam/security-credentials/"
	roles, err := m.Get(ctx, dir)
	if errors.Is(err, ErrNotFound) {
		return Credentials{}, errNoRole
	}
	if err != nil {
		return Credentials{}, err
	}
	role := strings.TrimSpace(strings.SplitN(roles, "\n", 2)[0])
	if role == "" {
		return Credentials{}, errNoRole
	}
	doc, err := m.Get(ctx, dir+url.PathEscape(role))
	if err != nil {
		return Credentials{}, err
	}
	var v struct {
		Code            string
		AccessKeyID     string `json:"AccessKeyId"`
		SecretAccessKey string
		Token           string
		Expiration      time.Time
	}
	if err := json.Unmarshal([]byte(doc), &v); err != nil {
		return Credentials{}, fmt.Errorf("aws imds role credentials: %w", err)
	}
	if v.Code != "Success" || v.AccessKeyID == "" || v.SecretAccessKey == "" {
		return Credentials{}, fmt.Errorf("aws imds role credentials: code %q", v.Code)
	}
	return Credentials{AccessKeyID: v.AccessKeyID, SecretAccessKey: v.SecretAccessKey, SessionToken: v.Token, Expires: v.Expiration}, nil
}
