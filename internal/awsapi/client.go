// Package awsapi is the small slice of the AWS API that Supavise calls itself: EC2 (describe,
// stop and re-address instances, for fencing and for finding a peer), Secrets Manager
// (GetSecretValue), STS (AssumeRole) and the instance metadata service (IMDSv2). It signs with
// Signature Version 4 and uses nothing outside the standard library.
//
// Credentials come from the standard AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY and
// AWS_SESSION_TOKEN variables, then from the instance role. Endpoints can be overridden per
// service with AWS_ENDPOINT_URL_EC2, AWS_ENDPOINT_URL_SECRETSMANAGER, AWS_ENDPOINT_URL_STS and
// AWS_ENDPOINT_URL_IMDS, which is how tests and the two-node CI harness point it at a fake.
package awsapi

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Environment variables that override a service endpoint. Secrets Manager also answers to the
// name the AWS SDKs and CLI use, and the metadata service to AWS_EC2_METADATA_SERVICE_ENDPOINT.
const (
	EnvEndpointEC2            = "AWS_ENDPOINT_URL_EC2"
	EnvEndpointSecretsManager = "AWS_ENDPOINT_URL_SECRETSMANAGER"
	EnvEndpointSTS            = "AWS_ENDPOINT_URL_STS"
	EnvEndpointIMDS           = "AWS_ENDPOINT_URL_IMDS"

	envEndpointSecretsManagerSDK = "AWS_ENDPOINT_URL_SECRETS_MANAGER"
	envEndpointIMDSSDK           = "AWS_EC2_METADATA_SERVICE_ENDPOINT"
)

const (
	defaultIMDSEndpoint = "http://169.254.169.254"
	userAgent           = "supavise-awsapi"
	maxResponseBytes    = 8 << 20
)

// Config configures New. The zero value works on an EC2 instance and wherever the standard AWS
// variables are set.
type Config struct {
	// Region is where regional endpoints are. Empty reads AWS_REGION, then AWS_DEFAULT_REGION, then
	// the metadata service, at the first call that needs it.
	Region string
	// Credentials signs requests. Nil uses the environment, then the instance role (DefaultCredentials).
	Credentials CredentialProvider
	// Endpoints override the service URLs; an empty field falls back to the environment, then to
	// the public endpoint of Region.
	Endpoints Endpoints
	// HTTPClient sends API requests. Nil uses a client with a 30 second timeout.
	HTTPClient *http.Client
	// Getenv reads the environment. Nil is os.Getenv.
	Getenv func(string) string
	// Now is the clock for signing and for credential expiry. Nil is time.Now.
	Now func() time.Time
	// MaxAttempts is how many times a request that got no answer, a 5xx or a throttle is sent.
	// Zero is 3.
	MaxAttempts int
	// RetryBackoff is the wait after the first failed attempt, doubled for each further one.
	// Zero is 200 ms.
	RetryBackoff time.Duration
}

// Endpoints are service URLs such as http://127.0.0.1:4566.
type Endpoints struct {
	EC2            string
	SecretsManager string
	STS            string
	IMDS           string
}

// Client reaches the services this package covers.
type Client struct {
	EC2            *EC2
	SecretsManager *SecretsManager
	STS            *STS
	IMDS           *IMDS

	core *core
}

// New builds a client. It does no network I/O: the region and the credentials are looked up by
// the first call that needs them, and a lookup that succeeded is not repeated.
func New(cfg Config) (*Client, error) {
	if cfg.Getenv == nil {
		cfg.Getenv = os.Getenv
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 3
	}
	if cfg.RetryBackoff <= 0 {
		cfg.RetryBackoff = 200 * time.Millisecond
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	ep := map[string]string{
		"ec2":            firstNonEmpty(cfg.Endpoints.EC2, cfg.Getenv(EnvEndpointEC2)),
		"secretsmanager": firstNonEmpty(cfg.Endpoints.SecretsManager, cfg.Getenv(EnvEndpointSecretsManager), cfg.Getenv(envEndpointSecretsManagerSDK)),
		"sts":            firstNonEmpty(cfg.Endpoints.STS, cfg.Getenv(EnvEndpointSTS)),
		"imds":           firstNonEmpty(cfg.Endpoints.IMDS, cfg.Getenv(EnvEndpointIMDS), cfg.Getenv(envEndpointIMDSSDK)),
	}
	for name, v := range ep {
		if v == "" {
			continue
		}
		u, err := url.Parse(v)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("awsapi: the %s endpoint %q is not an http(s) URL", name, v)
		}
		ep[name] = strings.TrimRight(v, "/")
	}
	imds := newIMDS(cfg, ep["imds"])
	creds := cfg.Credentials
	if creds == nil {
		creds = DefaultCredentials(cfg.Getenv, imds, cfg.Now)
	}
	c := &core{cfg: cfg, endpoints: ep, creds: creds, imds: imds}
	return &Client{
		EC2:            &EC2{c: c},
		SecretsManager: &SecretsManager{c: c},
		STS:            &STS{c: c},
		IMDS:           imds,
		core:           c,
	}, nil
}

// DefaultCredentials is the environment, then the instance role, cached.
func DefaultCredentials(getenv func(string) string, imds *IMDS, now func() time.Time) CredentialProvider {
	return ChainCredentials(EnvCredentials(getenv), CachedCredentials(IMDSCredentials(imds), now))
}

// Region returns the region calls go to, resolving it the first time.
func (c *Client) Region(ctx context.Context) (string, error) {
	r, err := c.core.region(ctx)
	if err != nil {
		return "", fmt.Errorf("awsapi: %w", err)
	}
	return r, nil
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// core is what the service clients share.
type core struct {
	cfg       Config
	endpoints map[string]string
	creds     CredentialProvider
	imds      *IMDS

	mu         sync.Mutex
	regionName string
}

func (c *core) region(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.regionName != "" {
		return c.regionName, nil
	}
	r := firstNonEmpty(c.cfg.Region, c.cfg.Getenv("AWS_REGION"), c.cfg.Getenv("AWS_DEFAULT_REGION"))
	if r == "" {
		var err error
		if r, err = c.imds.Region(ctx); err != nil {
			return "", fmt.Errorf("no region: set AWS_REGION, or run on an EC2 instance (%w)", err)
		}
	}
	c.regionName = r
	return r, nil
}

// endpoint is the base URL of a service: the override, or https://<host>.<region>.<suffix>.
func (c *core) endpoint(service, host, region string) string {
	if e := c.endpoints[service]; e != "" {
		return e
	}
	suffix := "amazonaws.com"
	if strings.HasPrefix(region, "cn-") {
		suffix = "amazonaws.com.cn"
	}
	return "https://" + host + "." + region + "." + suffix
}

// apiCall is one request to a signed service.
type apiCall struct {
	service  string // the signing name and the endpoint key
	host     string // the first label of the public endpoint
	action   string
	header   http.Header
	body     []byte
	parseErr func(status int, h http.Header, body []byte) *Error
}

// do sends the call, signed with the current credentials, and returns the body of a 2xx answer.
// Anything else is an *Error. A request that got no answer, a 5xx or a throttle is sent again
// with a fresh signature.
func (c *core) do(ctx context.Context, call apiCall) ([]byte, error) {
	for attempt := 1; ; attempt++ {
		body, err := c.once(ctx, call)
		if err == nil || attempt >= c.cfg.MaxAttempts || !retryable(err) || ctx.Err() != nil {
			return body, err
		}
		t := time.NewTimer(c.cfg.RetryBackoff << (attempt - 1))
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, fmt.Errorf("%w: %w", err, ctx.Err())
		case <-t.C:
		}
	}
}

func (c *core) once(ctx context.Context, call apiCall) ([]byte, error) {
	region, err := c.region(ctx)
	if err != nil {
		return nil, fmt.Errorf("aws %s %s: %w", call.service, call.action, err)
	}
	creds, err := c.creds.Retrieve(ctx)
	if err != nil {
		return nil, fmt.Errorf("aws %s %s: %w", call.service, call.action, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint(call.service, call.host, region)+"/", bytes.NewReader(call.body))
	if err != nil {
		return nil, fmt.Errorf("aws %s %s: %w", call.service, call.action, err)
	}
	for k, v := range call.header {
		req.Header[k] = v
	}
	req.Header.Set("User-Agent", userAgent)
	Sign(req, call.body, creds, region, call.service, c.cfg.Now())
	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		return nil, &transportError{fmt.Errorf("aws %s %s: %w", call.service, call.action, err)}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, &transportError{fmt.Errorf("aws %s %s: reading the response: %w", call.service, call.action, err)}
	}
	if resp.StatusCode/100 == 2 {
		return body, nil
	}
	e := call.parseErr(resp.StatusCode, resp.Header, body)
	e.Service, e.Action = call.service, call.action
	return nil, e
}
