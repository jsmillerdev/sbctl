package awsapi_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/supavise/supavise/internal/awsapi"
	"github.com/supavise/supavise/internal/awsapi/awsfake"
)

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

// rt records the URL a request goes to and refuses to send it.
type rt struct{ urls *[]string }

func (r rt) RoundTrip(req *http.Request) (*http.Response, error) {
	*r.urls = append(*r.urls, req.URL.String())
	return nil, errors.New("not sent")
}

func TestPublicEndpoints(t *testing.T) {
	var urls []string
	for region, want := range map[string][3]string{
		"eu-west-1":  {"https://ec2.eu-west-1.amazonaws.com/", "https://secretsmanager.eu-west-1.amazonaws.com/", "https://sts.eu-west-1.amazonaws.com/"},
		"cn-north-1": {"https://ec2.cn-north-1.amazonaws.com.cn/", "https://secretsmanager.cn-north-1.amazonaws.com.cn/", "https://sts.cn-north-1.amazonaws.com.cn/"},
	} {
		urls = nil
		c := newClient(t, awsapi.Config{
			Region: region, Credentials: awsapi.StaticCredentials(testCreds), Getenv: envOf(nil), MaxAttempts: 1,
			HTTPClient: &http.Client{Transport: rt{&urls}},
		})
		c.EC2.DescribeInstances(ctx, awsapi.DescribeInstancesInput{})
		c.SecretsManager.GetSecretValue(ctx, awsapi.GetSecretValueInput{SecretID: "x"})
		c.STS.AssumeRole(ctx, awsapi.AssumeRoleInput{RoleARN: "arn", SessionName: "s"})
		if strings.Join(urls, " ") != strings.Join(want[:], " ") {
			t.Errorf("%s: %v, want %v", region, urls, want)
		}
	}
}

func TestEndpointOverrides(t *testing.T) {
	// Each variable reaches its own service; the SDK's name for Secrets Manager works too.
	for _, secretsVar := range []string{awsapi.EnvEndpointSecretsManager, "AWS_ENDPOINT_URL_SECRETS_MANAGER"} {
		var urls []string
		c := newClient(t, awsapi.Config{
			Region: "us-east-1", Credentials: awsapi.StaticCredentials(testCreds), MaxAttempts: 1,
			Getenv: envOf(map[string]string{
				awsapi.EnvEndpointEC2: "http://ec2.test:4566/", secretsVar: "https://sm.test", awsapi.EnvEndpointSTS: "http://sts.test",
			}),
			HTTPClient: &http.Client{Transport: rt{&urls}},
		})
		c.EC2.DescribeInstances(ctx, awsapi.DescribeInstancesInput{})
		c.SecretsManager.GetSecretValue(ctx, awsapi.GetSecretValueInput{SecretID: "x"})
		c.STS.AssumeRole(ctx, awsapi.AssumeRoleInput{RoleARN: "arn", SessionName: "s"})
		if got, want := strings.Join(urls, " "), "http://ec2.test:4566/ https://sm.test/ http://sts.test/"; got != want {
			t.Errorf("%s: %s, want %s", secretsVar, got, want)
		}
	}

	// An explicit endpoint beats the environment.
	cfg, seen := stub(t, xmlOK("<DescribeInstancesResponse/>"))
	cfg.Getenv = envOf(map[string]string{awsapi.EnvEndpointEC2: "http://127.0.0.1:1"})
	if _, err := newClient(t, cfg).EC2.DescribeInstances(ctx, awsapi.DescribeInstancesInput{}); err != nil || len(seen()) != 1 {
		t.Errorf("explicit endpoint: %v, %d requests", err, len(seen()))
	}

	for _, bad := range []string{"ec2.example.com", "ftp://x", "http://", "://"} {
		if _, err := awsapi.New(awsapi.Config{Getenv: envOf(map[string]string{awsapi.EnvEndpointEC2: bad})}); err == nil || !strings.Contains(err.Error(), "ec2 endpoint") {
			t.Errorf("endpoint %q: %v", bad, err)
		}
	}
}

func TestRegionSources(t *testing.T) {
	fake := awsfake.New(t)
	d := fake.IMDS()
	d.Region = "ap-south-1"
	fake.SetIMDS(d)

	cases := []struct {
		name string
		cfg  awsapi.Config
		env  map[string]string
		want string
	}{
		{"config", awsapi.Config{Region: "eu-north-1"}, map[string]string{"AWS_REGION": "x"}, "eu-north-1"},
		{"AWS_REGION", awsapi.Config{}, map[string]string{"AWS_REGION": "us-west-2", "AWS_DEFAULT_REGION": "x"}, "us-west-2"},
		{"AWS_DEFAULT_REGION", awsapi.Config{}, map[string]string{"AWS_DEFAULT_REGION": "us-west-1"}, "us-west-1"},
		{"instance metadata", awsapi.Config{}, nil, "ap-south-1"},
	}
	for _, tc := range cases {
		cfg := fake.Config()
		cfg.Region = tc.cfg.Region
		cfg.Getenv = envOf(tc.env)
		got, err := newClient(t, cfg).Region(ctx)
		if err != nil || got != tc.want {
			t.Errorf("%s: %q, %v, want %q", tc.name, got, err, tc.want)
		}
	}

	cfg := fake.Config()
	cfg.Region = ""
	cfg.Endpoints.IMDS = "http://127.0.0.1:1"
	cfg.MaxAttempts = 1
	if _, err := newClient(t, cfg).Region(ctx); err == nil || !strings.Contains(err.Error(), "no region") {
		t.Errorf("no region anywhere: %v", err)
	}
}

func TestRegionIsLookedUpOnce(t *testing.T) {
	fake := awsfake.New(t)
	cfg := fake.Config()
	cfg.Region = ""
	c := newClient(t, cfg)
	for i := 0; i < 3; i++ {
		if _, err := c.Region(ctx); err != nil {
			t.Fatal(err)
		}
	}
	n := 0
	for _, call := range fake.Calls() {
		if call.Action == "GET /latest/meta-data/placement/region" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("region read %d times", n)
	}
}
