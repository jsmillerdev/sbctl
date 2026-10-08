package awsapi_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/awsapi"
	"github.com/supavise/supavise/internal/awsapi/awsfake"
)

func TestEnvCredentials(t *testing.T) {
	p := awsapi.EnvCredentials(envOf(map[string]string{"AWS_ACCESS_KEY_ID": "AK", "AWS_SECRET_ACCESS_KEY": "SK", "AWS_SESSION_TOKEN": "TOK"}))
	c, err := p.Retrieve(ctx)
	if err != nil || c.AccessKeyID != "AK" || c.SecretAccessKey != "SK" || c.SessionToken != "TOK" || !c.Expires.IsZero() {
		t.Errorf("%v, %v", c, err)
	}
	if _, err := awsapi.EnvCredentials(envOf(nil)).Retrieve(ctx); !errors.Is(err, awsapi.ErrNoCredentials) {
		t.Errorf("empty environment: %v", err)
	}
	for _, env := range []map[string]string{{"AWS_ACCESS_KEY_ID": "AK"}, {"AWS_SECRET_ACCESS_KEY": "SK"}} {
		if _, err := awsapi.EnvCredentials(envOf(env)).Retrieve(ctx); err == nil || errors.Is(err, awsapi.ErrNoCredentials) {
			t.Errorf("half a key pair %v: %v (want an error that stops a chain)", env, err)
		}
	}
}

func fn(c awsapi.Credentials, err error) awsapi.CredentialProvider {
	return awsapi.CredentialProviderFunc(func(context.Context) (awsapi.Credentials, error) { return c, err })
}

func TestChainCredentials(t *testing.T) {
	none := func(why string) awsapi.CredentialProvider {
		return fn(awsapi.Credentials{}, errors.Join(awsapi.ErrNoCredentials, errors.New(why)))
	}

	got, err := awsapi.ChainCredentials(none("a"), fn(awsapi.Credentials{AccessKeyID: "second"}, nil), fn(awsapi.Credentials{AccessKeyID: "third"}, nil)).Retrieve(ctx)
	if err != nil || got.AccessKeyID != "second" {
		t.Errorf("first answer wins: %v, %v", got, err)
	}
	boom := errors.New("boom")
	if _, err := awsapi.ChainCredentials(fn(awsapi.Credentials{}, boom), fn(awsapi.Credentials{AccessKeyID: "x"}, nil)).Retrieve(ctx); !errors.Is(err, boom) {
		t.Errorf("an error that is not 'no credentials' stops the chain: %v", err)
	}
	_, err = awsapi.ChainCredentials(
		awsapi.EnvCredentials(envOf(nil)),
		none("second reason"),
	).Retrieve(ctx)
	if !errors.Is(err, awsapi.ErrNoCredentials) || !strings.Contains(err.Error(), "AWS_ACCESS_KEY_ID is not set") || !strings.Contains(err.Error(), "second reason") {
		t.Errorf("all skipped: %v", err)
	}
}

func TestCachedCredentials(t *testing.T) {
	now := testNow
	calls := 0
	var next awsapi.Credentials
	var nextErr error
	p := awsapi.CachedCredentials(awsapi.CredentialProviderFunc(func(context.Context) (awsapi.Credentials, error) {
		calls++
		return next, nextErr
	}), func() time.Time { return now })

	next = awsapi.Credentials{AccessKeyID: "one", Expires: now.Add(time.Hour)}
	for i := 0; i < 3; i++ {
		if c, err := p.Retrieve(ctx); err != nil || c.AccessKeyID != "one" {
			t.Fatal(c, err)
		}
	}
	if calls != 1 {
		t.Errorf("%d reads for three uses", calls)
	}

	// Inside the five minute window before expiry the next use renews.
	now = now.Add(56 * time.Minute)
	next = awsapi.Credentials{AccessKeyID: "two", Expires: now.Add(time.Hour)}
	if c, _ := p.Retrieve(ctx); c.AccessKeyID != "two" || calls != 2 {
		t.Errorf("renewal: %v after %d reads", c, calls)
	}

	// A failed renewal keeps the old credentials while they still work, then fails.
	now = now.Add(56 * time.Minute)
	nextErr = errors.New("imds down")
	if c, err := p.Retrieve(ctx); err != nil || c.AccessKeyID != "two" {
		t.Errorf("failed renewal before expiry: %v, %v", c, err)
	}
	now = now.Add(10 * time.Minute)
	if _, err := p.Retrieve(ctx); err == nil || !strings.Contains(err.Error(), "imds down") {
		t.Errorf("failed renewal after expiry: %v", err)
	}

	// Credentials without an expiry are kept for good.
	calls, nextErr = 0, nil
	forever := awsapi.CachedCredentials(awsapi.CredentialProviderFunc(func(context.Context) (awsapi.Credentials, error) {
		calls++
		return awsapi.Credentials{AccessKeyID: "static"}, nil
	}), func() time.Time { return now })
	forever.Retrieve(ctx)
	now = now.Add(1000 * time.Hour)
	forever.Retrieve(ctx)
	if calls != 1 {
		t.Errorf("static credentials read %d times", calls)
	}
}

func TestDefaultCredentialsUseTheInstanceRole(t *testing.T) {
	fake := awsfake.New(t)
	fake.AddInstance(awsfake.Instance{ID: "i-1"})
	c := fake.Client()
	if _, err := c.EC2.DescribeInstances(ctx, awsapi.DescribeInstancesInput{}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.EC2.DescribeInstances(ctx, awsapi.DescribeInstancesInput{}); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, call := range fake.Calls() {
		if call.Service == "ec2" {
			keys = append(keys, call.AccessKeyID)
		}
	}
	if len(keys) != 2 || keys[0] != "ASIAFAKEROLE0000000" || keys[1] != keys[0] {
		t.Errorf("signed with %v", keys)
	}
	reads := 0
	for _, o := range fake.Order("imds") {
		if strings.HasSuffix(o, "/iam/security-credentials/supavise-instance-role") {
			reads++
		}
	}
	if reads != 1 {
		t.Errorf("role credentials read %d times for two calls", reads)
	}
}

func TestEnvironmentCredentialsComeBeforeTheInstanceRole(t *testing.T) {
	fake := awsfake.New(t)
	fake.AddInstance(awsfake.Instance{ID: "i-1"})
	fake.AddCredentials(awsapi.Credentials{AccessKeyID: "AKIAFROMENV", SecretAccessKey: "env-secret", SessionToken: "env-token"})
	cfg := fake.Config()
	cfg.Getenv = envOf(map[string]string{"AWS_ACCESS_KEY_ID": "AKIAFROMENV", "AWS_SECRET_ACCESS_KEY": "env-secret", "AWS_SESSION_TOKEN": "env-token"})
	if _, err := newClient(t, cfg).EC2.DescribeInstances(ctx, awsapi.DescribeInstancesInput{}); err != nil {
		t.Fatal(err)
	}
	calls := fake.Calls()
	if len(calls) != 1 || calls[0].AccessKeyID != "AKIAFROMENV" {
		t.Errorf("%+v", calls)
	}
}

func TestDisabledMetadataServiceLeavesNoCredentials(t *testing.T) {
	fake := awsfake.New(t)
	cfg := fake.Config()
	cfg.Getenv = envOf(map[string]string{"AWS_EC2_METADATA_DISABLED": "true"})
	_, err := newClient(t, cfg).EC2.DescribeInstances(ctx, awsapi.DescribeInstancesInput{})
	if !errors.Is(err, awsapi.ErrNoCredentials) || !strings.Contains(err.Error(), "AWS_EC2_METADATA_DISABLED") {
		t.Errorf("err = %v", err)
	}
	if n := len(fake.Calls()); n != 0 {
		t.Errorf("%d calls reached the fake", n)
	}
}

func TestInstanceWithoutARoleLeavesNoCredentials(t *testing.T) {
	fake := awsfake.New(t)
	d := fake.IMDS()
	d.Role = ""
	fake.SetIMDS(d)
	_, err := fake.Client().EC2.DescribeInstances(ctx, awsapi.DescribeInstancesInput{})
	if !errors.Is(err, awsapi.ErrNoCredentials) || !strings.Contains(err.Error(), "no IAM role") {
		t.Errorf("err = %v", err)
	}
}

// The role-only switch leaves the environment as the one source of credentials and the metadata
// service as the source of identity.
func TestNoInstanceRoleKeepsTheIdentityReads(t *testing.T) {
	setups := map[string]func(*awsapi.Config){
		"Config.NoInstanceRole": func(c *awsapi.Config) { c.NoInstanceRole = true },
		"=1":                    func(c *awsapi.Config) { c.Getenv = envOf(map[string]string{awsapi.EnvNoInstanceRole: "1"}) },
		"=true":                 func(c *awsapi.Config) { c.Getenv = envOf(map[string]string{awsapi.EnvNoInstanceRole: " TRUE "}) },
	}
	for name, setup := range setups {
		t.Run(name, func(t *testing.T) {
			fake := awsfake.New(t)
			d := fake.IMDS()
			d.Tags = map[string]string{"supavise:cluster": "prod"}
			fake.SetIMDS(d)
			cfg := fake.Config()
			cfg.Region = ""
			setup(&cfg)
			c := newClient(t, cfg)

			// No role credentials: a call has none to sign with and says why.
			_, err := c.EC2.DescribeInstances(ctx, awsapi.DescribeInstancesInput{})
			if !errors.Is(err, awsapi.ErrNoCredentials) || !strings.Contains(err.Error(), awsapi.EnvNoInstanceRole) {
				t.Errorf("EC2 call: %v", err)
			}
			if _, err := awsapi.DefaultCredentials(envOf(map[string]string{awsapi.EnvNoInstanceRole: "1"}), c.IMDS, time.Now).Retrieve(ctx); !errors.Is(err, awsapi.ErrNoCredentials) {
				t.Errorf("DefaultCredentials: %v", err)
			}

			// The identity reads work, the region among them.
			id, idErr := c.IMDS.InstanceID(ctx)
			region, regionErr := c.Region(ctx)
			az, azErr := c.IMDS.AvailabilityZone(ctx)
			tags, tagsErr := c.IMDS.Tags(ctx)
			if idErr != nil || regionErr != nil || azErr != nil || tagsErr != nil || id != "i-0aaaaaaaaaaaaaaaa" || region != "us-east-1" || az != "us-east-1a" || tags["supavise:cluster"] != "prod" {
				t.Errorf("identity: %q %q %q %v / %v %v %v %v", id, region, az, tags, idErr, regionErr, azErr, tagsErr)
			}
			for _, o := range fake.Order("imds") {
				if strings.Contains(o, "security-credentials") {
					t.Errorf("the instance role was read: %s", o)
				}
			}
			if n := len(fake.Order("ec2")); n != 0 {
				t.Errorf("%d EC2 calls were sent without credentials", n)
			}
		})
	}
}

// With the switch on, the credentials in the environment still sign, and the instance role is not
// consulted. Without it (or with another value) the role is the fallback as before.
func TestNoInstanceRoleUsesTheEnvironment(t *testing.T) {
	fake := awsfake.New(t)
	fake.AddInstance(awsfake.Instance{ID: "i-1"})
	fake.AddCredentials(awsapi.Credentials{AccessKeyID: "AKIAOPERATOR", SecretAccessKey: "operator-secret"})
	operator := map[string]string{"AWS_ACCESS_KEY_ID": "AKIAOPERATOR", "AWS_SECRET_ACCESS_KEY": "operator-secret"}

	cfg := fake.Config()
	cfg.NoInstanceRole = true
	cfg.Getenv = envOf(operator)
	if _, err := newClient(t, cfg).EC2.DescribeInstances(ctx, awsapi.DescribeInstancesInput{}); err != nil {
		t.Fatal(err)
	}
	if calls := fake.Calls(); len(calls) != 1 || calls[0].AccessKeyID != "AKIAOPERATOR" {
		t.Errorf("%+v", calls)
	}

	for _, v := range []string{"", "0", "false", "no"} {
		fake := awsfake.New(t)
		cfg := fake.Config()
		cfg.Getenv = envOf(map[string]string{awsapi.EnvNoInstanceRole: v})
		if _, err := newClient(t, cfg).EC2.DescribeInstances(ctx, awsapi.DescribeInstancesInput{}); err != nil {
			t.Errorf("%s=%q: %v", awsapi.EnvNoInstanceRole, v, err)
		}
	}
}

// AWS_EC2_METADATA_DISABLED stays the stronger switch: it turns off the identity reads too.
func TestMetadataDisabledStillTurnsOffTheIdentityReads(t *testing.T) {
	fake := awsfake.New(t)
	cfg := fake.Config()
	cfg.NoInstanceRole = true
	cfg.Getenv = envOf(map[string]string{"AWS_EC2_METADATA_DISABLED": "true"})
	if _, err := newClient(t, cfg).IMDS.InstanceID(ctx); !errors.Is(err, awsapi.ErrIMDSDisabled) {
		t.Errorf("err = %v", err)
	}
}
