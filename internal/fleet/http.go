package fleet

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
)

// Retry bounds the retries of a tenant call. The calls retry only what can heal: a refused
// connection (the service is still starting), a timeout, and status 408, 425, 429 and 5xx.
// A 4xx answer is the service saying no, and comes back at once.
type Retry struct {
	// Attempts is the total number of tries (default 5).
	Attempts int
	// Base is the delay before the second try; it doubles per try up to Max, with up to 25%
	// jitter (defaults 500 ms and 8 s).
	Base, Max time.Duration
}

func (r Retry) norm() Retry {
	if r.Attempts <= 0 {
		r.Attempts = 5
	}
	if r.Base <= 0 {
		r.Base = 500 * time.Millisecond
	}
	if r.Max <= 0 {
		r.Max = 8 * time.Second
	}
	return r
}

// response is a finished HTTP exchange.
type response struct {
	Status int
	Body   []byte
}

func (r *response) ok() bool { return r.Status >= 200 && r.Status < 300 }

// excerpt is the start of the body, for error messages.
func (r *response) excerpt() string {
	s := strings.TrimSpace(string(r.Body))
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	if s == "" {
		return "(empty body)"
	}
	return s
}

// apiClient sends the admin API calls of one service.
type apiClient struct {
	name  string // service name, for messages
	http  *http.Client
	retry Retry
	log   *slog.Logger
	// sleep waits between tries; tests replace it.
	sleep func(context.Context, time.Duration) error
}

func (c *apiClient) wait(ctx context.Context, d time.Duration) error {
	if c.sleep != nil {
		return c.sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func retryableStatus(s int) bool {
	return s == http.StatusRequestTimeout || s == http.StatusTooEarly || s == http.StatusTooManyRequests || s >= 500
}

// do sends one request, retrying transient failures with exponential backoff. It returns
// the first answer that is not retryable (any status below 500 except the ones above)
// and an error only when every try failed or the context ended.
func (c *apiClient) do(ctx context.Context, method, url string, header map[string]string, body any) (*response, error) {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return nil, err
		}
	}
	r := c.retry.norm()
	delay := r.Base
	var last error
	for attempt := 1; attempt <= r.Attempts; attempt++ {
		resp, err := c.once(ctx, method, url, header, payload)
		switch {
		case err == nil && !retryableStatus(resp.Status):
			return resp, nil
		case err == nil:
			last = fmt.Errorf("%s %s: status %d: %s", method, url, resp.Status, resp.excerpt())
		default:
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			last = fmt.Errorf("%s %s: %w", method, url, err)
		}
		if attempt == r.Attempts {
			break
		}
		wait := delay + time.Duration(rand.Int64N(int64(delay)/4+1))
		c.log.Debug("tenant call failed; retrying", "service", c.name, "attempt", attempt, "wait", wait, "error", last)
		if err := c.wait(ctx, wait); err != nil {
			return nil, err
		}
		if delay *= 2; delay > r.Max {
			delay = r.Max
		}
	}
	return nil, fmt.Errorf("fleet: %s unreachable or failing after %d tries: %w", c.name, r.Attempts, last)
}

func (c *apiClient) once(ctx context.Context, method, url string, header map[string]string, payload []byte) (*response, error) {
	var rd io.Reader
	if payload != nil {
		rd = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return nil, err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	return &response{Status: resp.StatusCode, Body: b}, nil
}

// apiError describes a final, non-retryable refusal.
func (c *apiClient) apiError(what string, r *response) error {
	return fmt.Errorf("fleet: %s: %s: status %d: %s", c.name, what, r.Status, r.excerpt())
}

// signToken is the bearer token of the Supavisor and Realtime APIs: HS256 over the
// service's API_JWT_SECRET. Both only verify the signature and exp; "role" is there for
// the paths that insist on it.
func signToken(secret string, now time.Time) (string, error) {
	return jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"iss": "supavise", "role": "service_role", "iat": now.Unix(), "exp": now.Add(10 * time.Minute).Unix(),
	}).SignedString([]byte(secret))
}

// tenantStore remembers, per project and service, a fingerprint of the tenant a service
// was last given. A repeated EnsureTenant with an unchanged fingerprint, for a tenant
// the service still has, sends nothing: an update would make Supavisor drop the pools
// and Realtime or Storage reload the tenant's connections. The fingerprint is a hash, and it
// is sealed like every other registry value.
type tenantStore struct {
	reg registry.Registry
	sec secrets.Secrets
}

func fingerprintName(service string) string { return "fleet_tenant_" + service }

// Fingerprint hashes the parts that make up a tenant's configuration.
func fingerprint(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		fmt.Fprintf(h, "%d:%s;", len(p), p)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (s tenantStore) get(ctx context.Context, ref, service string) string {
	if s.reg == nil {
		return ""
	}
	sealed, err := s.reg.GetSecret(ctx, ref, fingerprintName(service))
	if err != nil {
		return ""
	}
	plain, err := s.sec.Open(sealed)
	if err != nil {
		return ""
	}
	return string(plain)
}

// put records fp. Failing to is harmless (the next call sends the tenant again), so the
// error is only returned for the caller to log.
func (s tenantStore) put(ctx context.Context, ref, service, fp string) error {
	if s.reg == nil {
		return nil
	}
	sealed, err := s.sec.Seal([]byte(fp))
	if err != nil {
		return err
	}
	return s.reg.PutSecret(ctx, ref, fingerprintName(service), sealed)
}

// forget clears the fingerprint after the tenant was removed.
func (s tenantStore) forget(ctx context.Context, ref, service string) {
	if s.reg == nil {
		return
	}
	if _, err := s.reg.GetSecret(ctx, ref, fingerprintName(service)); err != nil {
		return // nothing recorded (or the project is gone)
	}
	_ = s.put(ctx, ref, service, "")
}

// validTenantRef checks that ref can be a tenant id: 20 lowercase letters. The system
// project has no tenants.
func validTenantRef(ref string) error {
	if ref == config.SystemRef || !secrets.ValidRef(ref) {
		return fmt.Errorf("fleet: %q is not a project ref", ref)
	}
	return nil
}

// withRelease folds the release tag of the service into a tenant fingerprint. An empty tag (a node
// that cannot say which release it runs) leaves the fingerprint as it was.
func withRelease(fp, tag string) string {
	if tag == "" {
		return fp
	}
	return fingerprint(fp, tag)
}

// fingerprintOf hashes the JSON form of a tenant request body (map keys are sorted, so the
// encoding is stable).
func fingerprintOf(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return fingerprint(string(b)), nil
}
