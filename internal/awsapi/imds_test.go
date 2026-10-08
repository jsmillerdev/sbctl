package awsapi_test

import (
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/awsapi"
	"github.com/supavise/supavise/internal/awsapi/awsfake"
)

func TestIMDSReads(t *testing.T) {
	fake := awsfake.New(t)
	d := fake.IMDS()
	d.PublicIP = "203.0.113.25"
	d.Tags = map[string]string{"supavise:cluster": "prod", "supavise:infra": "2", "supavise:eip": "eipalloc-0123"}
	fake.SetIMDS(d)
	m := fake.Client().IMDS

	for name, tc := range map[string]struct {
		get  func() (string, error)
		want string
	}{
		"instance id":       {func() (string, error) { return m.InstanceID(ctx) }, "i-0aaaaaaaaaaaaaaaa"},
		"region":            {func() (string, error) { return m.Region(ctx) }, "us-east-1"},
		"availability zone": {func() (string, error) { return m.AvailabilityZone(ctx) }, "us-east-1a"},
		"local ip":          {func() (string, error) { return m.LocalIPv4(ctx) }, "10.77.0.10"},
		"public ip":         {func() (string, error) { return m.PublicIPv4(ctx) }, "203.0.113.25"},
	} {
		if got, err := tc.get(); err != nil || got != tc.want {
			t.Errorf("%s = %q, %v, want %q", name, got, err, tc.want)
		}
	}
	tags, err := m.Tags(ctx)
	if err != nil || !reflect.DeepEqual(tags, d.Tags) {
		t.Errorf("tags = %v, %v", tags, err)
	}
}

func TestIMDSAbsentThings(t *testing.T) {
	fake := awsfake.New(t)
	m := fake.Client().IMDS
	if ip, err := m.PublicIPv4(ctx); err != nil || ip != "" {
		t.Errorf("no public ip: %q, %v (want empty and no error)", ip, err)
	}
	if tags, err := m.Tags(ctx); err != nil || len(tags) != 0 {
		t.Errorf("no tags: %v, %v", tags, err)
	}
	d := fake.IMDS()
	d.Tags, d.TagsHidden = map[string]string{"k": "v"}, true
	fake.SetIMDS(d)
	if tags, err := m.Tags(ctx); err != nil || len(tags) != 0 {
		t.Errorf("tags not enabled in metadata: %v, %v", tags, err)
	}
	if _, err := m.Get(ctx, "/latest/meta-data/nothing-here"); !errors.Is(err, awsapi.ErrNotFound) {
		t.Errorf("unknown path: %v", err)
	}
}

func TestIMDSTokenIsFetchedOnceAndRenewedWhenRefused(t *testing.T) {
	fake := awsfake.New(t)
	m := fake.Client().IMDS
	for i := 0; i < 3; i++ {
		if _, err := m.InstanceID(ctx); err != nil {
			t.Fatal(err)
		}
	}
	puts := func() int {
		n := 0
		for _, o := range fake.Order("imds") {
			if o == "imds:PUT /latest/api/token" {
				n++
			}
		}
		return n
	}
	if puts() != 1 {
		t.Errorf("%d token requests for three reads", puts())
	}
	fake.ExpireIMDSTokens()
	if id, err := m.InstanceID(ctx); err != nil || id != "i-0aaaaaaaaaaaaaaaa" {
		t.Errorf("after the token expired: %q, %v", id, err)
	}
	if puts() != 2 {
		t.Errorf("%d token requests after an expiry, want 2", puts())
	}
}

func TestIMDSRetriesServerErrorsButNotRefusals(t *testing.T) {
	fake := awsfake.New(t)
	m := fake.Client().IMDS
	fake.Inject("imds", "/latest/meta-data/instance-id", awsfake.Fault{Status: 503, Times: 2})
	if id, err := m.InstanceID(ctx); err != nil || id == "" {
		t.Errorf("two 503s then success: %q, %v", id, err)
	}

	fake = awsfake.New(t)
	m = fake.Client().IMDS
	fake.Inject("imds", "/latest/api/token", awsfake.Fault{Status: http.StatusForbidden})
	if _, err := m.InstanceID(ctx); err == nil || !strings.Contains(err.Error(), "refused a session token") {
		t.Errorf("token refused: %v", err)
	}
	if got := fake.Order("imds"); !reflect.DeepEqual(got, []string{"imds:PUT /latest/api/token"}) {
		t.Errorf("a refused token led to %v (no retry, no IMDSv1 read)", got)
	}
}

func TestIMDSDisabledAndUnreachable(t *testing.T) {
	fake := awsfake.New(t)
	cfg := fake.Config()
	cfg.Getenv = envOf(map[string]string{"AWS_EC2_METADATA_DISABLED": "TRUE"})
	if _, err := newClient(t, cfg).IMDS.InstanceID(ctx); !errors.Is(err, awsapi.ErrIMDSDisabled) {
		t.Errorf("disabled: %v", err)
	}
	if n := len(fake.Calls()); n != 0 {
		t.Errorf("disabled service was called %d times", n)
	}

	cfg = fake.Config()
	cfg.Endpoints.IMDS = "http://127.0.0.1:1"
	cfg.MaxAttempts = 2
	if _, err := newClient(t, cfg).IMDS.InstanceID(ctx); err == nil {
		t.Error("unreachable service gave no error")
	}
}

func TestIMDSRoleCredentials(t *testing.T) {
	fake := awsfake.New(t)
	before := time.Now()
	c, err := fake.Client().IMDS.RoleCredentials(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if c.AccessKeyID != "ASIAFAKEROLE0000000" || c.SecretAccessKey != "fake-role-secret" || c.SessionToken != "fake-role-token" {
		t.Errorf("%v", c)
	}
	if c.Expires.Before(before.Add(5*time.Hour)) || c.Expires.After(before.Add(7*time.Hour)) {
		t.Errorf("expires %v", c.Expires)
	}
}
