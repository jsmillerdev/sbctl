package awsapi_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
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

// closedIMDS is a metadata service that takes every connection and drops it, like a machine that is
// not on EC2 and has nothing at that address. connections says how many were made.
func closedIMDS(t *testing.T) (url string, connections func() int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var n atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			n.Add(1)
			c.Close()
		}
	}()
	return "http://" + ln.Addr().String(), func() int { return int(n.Load()) }
}

// A metadata service that does not answer is asked again only after a pause, whoever asks: a read,
// the region lookup or the credentials of an EC2 call.
func TestIMDSUnreachableIsRememberedBriefly(t *testing.T) {
	url, connections := closedIMDS(t)
	now := testNow
	cfg := awsapi.Config{
		Credentials: nil, Endpoints: awsapi.Endpoints{IMDS: url, EC2: "http://127.0.0.1:1"}, Getenv: envOf(nil),
		Now: func() time.Time { return now }, MaxAttempts: 2, RetryBackoff: time.Millisecond,
	}
	c := newClient(t, cfg)

	if _, err := c.IMDS.InstanceID(ctx); !errors.Is(err, awsapi.ErrIMDSUnreachable) {
		t.Fatalf("first read: %v", err)
	}
	asked := connections()
	if asked < 2 {
		t.Fatalf("%d connections for two tries", asked)
	}

	reads := map[string]error{}
	_, reads["InstanceID"] = c.IMDS.InstanceID(ctx)
	_, reads["Tags"] = c.IMDS.Tags(ctx)
	_, reads["Region"] = c.Region(ctx)
	_, reads["IMDSCredentials"] = awsapi.IMDSCredentials(c.IMDS).Retrieve(ctx)
	_, reads["an EC2 call without a region"] = c.EC2.DescribeInstances(ctx, awsapi.DescribeInstancesInput{})
	for what, err := range reads {
		if !errors.Is(err, awsapi.ErrIMDSUnreachable) {
			t.Errorf("%s: %v", what, err)
		}
	}
	if got := connections(); got != asked {
		t.Errorf("%d more connections within the memo", got-asked)
	}
	if err := reads["InstanceID"]; !strings.HasPrefix(err.Error(), "aws imds /latest/meta-data/instance-id: ") || !strings.Contains(err.Error(), "not asked again for") {
		t.Errorf("message: %v", err)
	}

	// An EC2 call with a region and no credentials in the environment asks the service for the
	// instance role once, and fails at once the next time.
	cfg.Region = "us-east-1"
	withRegion := newClient(t, cfg)
	for i := 0; i < 2; i++ {
		_, err := withRegion.EC2.DescribeInstances(ctx, awsapi.DescribeInstancesInput{})
		if !errors.Is(err, awsapi.ErrNoCredentials) || !errors.Is(err, awsapi.ErrIMDSUnreachable) {
			t.Errorf("EC2 call %d: %v", i+1, err)
		}
		if i == 0 {
			asked = connections()
		}
	}
	if got := connections(); got != asked {
		t.Errorf("the second EC2 call made %d connections", got-asked)
	}

	// The memo lapses.
	asked = connections()
	now = now.Add(29 * time.Second)
	c.IMDS.InstanceID(ctx)
	if got := connections(); got != asked {
		t.Errorf("a connection after 29 s")
	}
	now = now.Add(2 * time.Second)
	if _, err := c.IMDS.InstanceID(ctx); !errors.Is(err, awsapi.ErrIMDSUnreachable) {
		t.Errorf("after the memo: %v", err)
	}
	if got := connections(); got <= asked {
		t.Errorf("no new connection after 31 s")
	}
}

// What the memo records is the service's silence. A caller that gave up and a service that
// answered with an error say nothing about whether it is there.
func TestIMDSUnreachableIsNotRememberedFromOtherFailures(t *testing.T) {
	url, connections := closedIMDS(t)
	cfg := awsapi.Config{Endpoints: awsapi.Endpoints{IMDS: url}, Getenv: envOf(nil), MaxAttempts: 2, RetryBackoff: time.Millisecond}
	c := newClient(t, cfg)

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := c.IMDS.InstanceID(cancelled); errors.Is(err, awsapi.ErrIMDSUnreachable) || !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
	if _, err := c.IMDS.InstanceID(ctx); !errors.Is(err, awsapi.ErrIMDSUnreachable) || connections() == 0 {
		t.Errorf("after a cancelled call the service was not asked: %v, %d connections", err, connections())
	}

	fake := awsfake.New(t)
	m := fake.Client().IMDS
	fake.Inject("imds", "/latest/meta-data/instance-id", awsfake.Fault{Status: 503, Times: 3})
	if _, err := m.InstanceID(ctx); err == nil || errors.Is(err, awsapi.ErrIMDSUnreachable) {
		t.Errorf("a 503: %v", err)
	}
	if id, err := m.InstanceID(ctx); err != nil || id == "" {
		t.Errorf("after a 503 the service is asked again: %q, %v", id, err)
	}
}

// A caller that gave up between two tries has seen one failure, which says nothing about the
// service: the next caller asks it again.
func TestIMDSUnreachableIsNotRememberedWhenTheCallerGivesUpBetweenTries(t *testing.T) {
	url, connections := closedIMDS(t)
	c := newClient(t, awsapi.Config{Endpoints: awsapi.Endpoints{IMDS: url}, Getenv: envOf(nil), MaxAttempts: 3, RetryBackoff: time.Millisecond})

	callerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	awsapi.SetRetryHooks(c, func(ctx context.Context, _ time.Duration) error {
		cancel() // the caller gives up while it waits for its second try
		return ctx.Err()
	}, nil)
	if _, err := c.IMDS.InstanceID(callerCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("the cancelled call: %v", err)
	}
	asked := connections()

	awsapi.SetRetryHooks(c, func(context.Context, time.Duration) error { return nil }, nil)
	if _, err := c.IMDS.InstanceID(ctx); !errors.Is(err, awsapi.ErrIMDSUnreachable) {
		t.Fatalf("the next call: %v", err)
	}
	if connections() <= asked {
		t.Errorf("the next call did not ask the service: %d connections", connections())
	}
}

type callerKey struct{}

// Callers that waited for the same silent service stop once one of them has found it silent: they
// do not each spend their own tries on it.
func TestIMDSQueuedCallersStopWhenOneHasFoundTheServiceSilent(t *testing.T) {
	url, connections := closedIMDS(t)
	const slow, attempts = 4, 3
	c := newClient(t, awsapi.Config{Endpoints: awsapi.Endpoints{IMDS: url}, Getenv: envOf(nil), MaxAttempts: attempts, RetryBackoff: time.Millisecond})

	// The slow callers wait for the release after their first try; the fast one never waits.
	release := make(chan struct{})
	awsapi.SetRetryHooks(c, func(ctx context.Context, _ time.Duration) error {
		if ctx.Value(callerKey{}) == "fast" {
			return nil
		}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}, nil)

	errs := make(chan error, slow+1)
	for i := 0; i < slow; i++ {
		go func() {
			_, err := c.IMDS.InstanceID(context.WithValue(ctx, callerKey{}, "slow"))
			errs <- err
		}()
	}
	deadline := time.Now().Add(10 * time.Second)
	for connections() < slow && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if connections() < slow {
		t.Fatalf("%d connections: the slow callers did not each make a first try", connections())
	}
	if _, err := c.IMDS.InstanceID(context.WithValue(ctx, callerKey{}, "fast")); !errors.Is(err, awsapi.ErrIMDSUnreachable) {
		t.Fatalf("the fast caller: %v", err)
	}
	asked := connections()
	close(release)
	for i := 0; i < slow; i++ {
		if err := <-errs; !errors.Is(err, awsapi.ErrIMDSUnreachable) {
			t.Errorf("a queued caller: %v", err)
		}
	}
	if got := connections(); got != asked {
		t.Errorf("the queued callers made %d more connections after the service was found silent", got-asked)
	}
}
