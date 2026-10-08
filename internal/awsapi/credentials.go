package awsapi

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// ErrNoCredentials means a provider has no credentials to give, as opposed to failing to read
// them. A chain moves on to the next provider on it.
var ErrNoCredentials = errors.New("no AWS credentials")

// refreshWindow is how long before expiry a cached credential is replaced. Instance-role and
// assumed-role credentials are valid for at least 15 minutes, so a refresh always has time to retry.
const refreshWindow = 5 * time.Minute

// Credentials are one set of AWS credentials. Printing one shows only the access key id.
type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	// Expires is when temporary credentials stop working. Zero means they do not expire.
	Expires time.Time
}

func (c Credentials) String() string   { return "Credentials{" + c.AccessKeyID + "}" }
func (c Credentials) GoString() string { return c.String() }

// CredentialProvider returns the credentials to sign the next request with.
type CredentialProvider interface {
	Retrieve(ctx context.Context) (Credentials, error)
}

// CredentialProviderFunc adapts a function to a CredentialProvider.
type CredentialProviderFunc func(ctx context.Context) (Credentials, error)

func (f CredentialProviderFunc) Retrieve(ctx context.Context) (Credentials, error) { return f(ctx) }

// StaticCredentials always returns c.
func StaticCredentials(c Credentials) CredentialProvider {
	return CredentialProviderFunc(func(context.Context) (Credentials, error) { return c, nil })
}

// EnvCredentials reads AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY and AWS_SESSION_TOKEN through
// getenv. With neither key set it returns ErrNoCredentials; with only one it fails, because
// that is a mistake and not an absence.
func EnvCredentials(getenv func(string) string) CredentialProvider {
	return CredentialProviderFunc(func(context.Context) (Credentials, error) {
		id, secret := getenv("AWS_ACCESS_KEY_ID"), getenv("AWS_SECRET_ACCESS_KEY")
		switch {
		case id == "" && secret == "":
			return Credentials{}, fmt.Errorf("%w: AWS_ACCESS_KEY_ID is not set", ErrNoCredentials)
		case id == "" || secret == "":
			return Credentials{}, errors.New("AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY must be set together")
		}
		return Credentials{AccessKeyID: id, SecretAccessKey: secret, SessionToken: getenv("AWS_SESSION_TOKEN")}, nil
	})
}

// IMDSCredentials returns the credentials of the instance's role from the metadata service. Every
// failure to get them, including a service that is off or unreachable and an instance with no
// role, wraps ErrNoCredentials.
func IMDSCredentials(m *IMDS) CredentialProvider {
	return CredentialProviderFunc(func(ctx context.Context) (Credentials, error) {
		c, err := m.RoleCredentials(ctx)
		if err != nil {
			return Credentials{}, fmt.Errorf("%w: instance metadata: %v", ErrNoCredentials, err)
		}
		return c, nil
	})
}

// ChainCredentials asks each provider in turn and returns the first answer. A provider that
// returns ErrNoCredentials is skipped; any other error stops the chain. When every provider is
// skipped the error wraps ErrNoCredentials and carries each reason.
func ChainCredentials(providers ...CredentialProvider) CredentialProvider {
	return CredentialProviderFunc(func(ctx context.Context) (Credentials, error) {
		var reasons []string
		for _, p := range providers {
			c, err := p.Retrieve(ctx)
			if err == nil {
				return c, nil
			}
			if !errors.Is(err, ErrNoCredentials) {
				return Credentials{}, err
			}
			reasons = append(reasons, strings.TrimPrefix(err.Error(), ErrNoCredentials.Error()+": "))
		}
		return Credentials{}, fmt.Errorf("%w: %s", ErrNoCredentials, strings.Join(reasons, "; "))
	})
}

// CachedCredentials remembers what p returns until refreshWindow before it expires, and
// serializes refreshes. Credentials without an expiry are kept for good. When a refresh fails
// the previous credentials are returned for as long as they are still valid.
func CachedCredentials(p CredentialProvider, now func() time.Time) CredentialProvider {
	var (
		mu  sync.Mutex
		cur Credentials
		ok  bool
	)
	return CredentialProviderFunc(func(ctx context.Context) (Credentials, error) {
		mu.Lock()
		defer mu.Unlock()
		t := now()
		if ok && (cur.Expires.IsZero() || t.Add(refreshWindow).Before(cur.Expires)) {
			return cur, nil
		}
		c, err := p.Retrieve(ctx)
		if err != nil {
			if ok && (cur.Expires.IsZero() || t.Before(cur.Expires)) {
				return cur, nil
			}
			return Credentials{}, err
		}
		cur, ok = c, true
		return c, nil
	})
}
