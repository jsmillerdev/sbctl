package awsapi_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/awsapi"
	"github.com/supavise/supavise/internal/awsapi/awsfake"
)

func ec2Error(status int, code string) answer {
	return answer{status: status, body: fmt.Sprintf(`<Response><Errors><Error><Code>%s</Code><Message>try later</Message></Error></Errors><RequestID>r</RequestID></Response>`, code)}
}

// noWait lets a client retry without sleeping and records how long it would have slept.
func noWait(c *awsapi.Client, jitter func() float64) *[]time.Duration {
	var waits []time.Duration
	awsapi.SetRetryHooks(c, func(_ context.Context, d time.Duration) error {
		waits = append(waits, d)
		return nil
	}, jitter)
	return &waits
}

// A throttle or a timeout is sent again whatever its status, because the services disagree on the
// status. Only the code decides.
func TestThrottleAndTimeoutCodesAreRetried(t *testing.T) {
	stop := awsapi.StopInstancesInput{InstanceIDs: []string{"i-1234567890abcdef0"}}
	for _, code := range []string{
		"RequestLimitExceeded", "Throttling", "ThrottlingException", "ThrottledException", "TooManyRequestsException", "RequestThrottled",
		"EC2ThrottledException", "RequestThrottledException", "PriorRequestNotComplete", "RequestTimeout",
	} {
		for _, status := range []int{400, 503} {
			t.Run(fmt.Sprintf("%s %d", code, status), func(t *testing.T) {
				cfg, seen := stub(t, ec2Error(status, code), xmlOK(fixture(t, "stop_instances.xml")))
				c := newClient(t, cfg)
				noWait(c, nil)
				if _, err := c.EC2.StopInstances(ctx, stop); err != nil {
					t.Errorf("%v", err)
				}
				if n := len(seen()); n != 2 {
					t.Errorf("%d requests, want the first and one retry", n)
				}
			})
		}
	}

	// A refusal with a 400 is not a throttle.
	cfg, seen := stub(t, ec2Error(400, "InvalidParameterValue"))
	c := newClient(t, cfg)
	noWait(c, nil)
	if _, err := c.EC2.StopInstances(ctx, stop); !awsapi.IsCode(err, "InvalidParameterValue") || len(seen()) != 1 {
		t.Errorf("a refusal: %v after %d requests", err, len(seen()))
	}
}

// The waits between tries follow the schedule 200 ms, 400 ms, 800 ms, 1.6 s, each cut to a random
// point in its upper half, so five tries last between 1.5 and 3 seconds.
func TestWaitsFollowTheJitteredSchedule(t *testing.T) {
	stop := awsapi.StopInstancesInput{InstanceIDs: []string{"i-1"}}
	schedule := []time.Duration{200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond, 1600 * time.Millisecond}

	run := func(jitter func() float64) ([]time.Duration, int) {
		cfg, seen := stub(t, ec2Error(503, "RequestLimitExceeded"))
		cfg.RetryBackoff = 0 // the default
		c := newClient(t, cfg)
		waits := noWait(c, jitter)
		if _, err := c.EC2.StopInstances(ctx, stop); !awsapi.IsCode(err, "RequestLimitExceeded") {
			t.Errorf("persistent throttle: %v", err)
		}
		return *waits, len(seen())
	}

	// With a jitter of 0.5 each wait is three quarters of its schedule.
	waits, sent := run(func() float64 { return 0.5 })
	want := []time.Duration{150 * time.Millisecond, 300 * time.Millisecond, 600 * time.Millisecond, 1200 * time.Millisecond}
	if sent != 5 || fmt.Sprint(waits) != fmt.Sprint(want) {
		t.Errorf("%d requests, waits %v, want 5 and %v", sent, waits, want)
	}

	// With the real random numbers every wait stays in its window, and the whole stays in 1.5 to 3 s.
	for i := 0; i < 20; i++ {
		waits, _ := run(nil)
		var total time.Duration
		for n, w := range waits {
			total += w
			if w < schedule[n]/2 || w >= schedule[n] {
				t.Fatalf("wait %d is %v, want [%v, %v)", n+1, w, schedule[n]/2, schedule[n])
			}
		}
		if len(waits) != 4 || total < 1500*time.Millisecond || total >= 3*time.Second {
			t.Fatalf("waits %v add up to %v", waits, total)
		}
	}
}

// The metadata service waits the same way, with its three tries.
func TestIMDSWaitsAreJittered(t *testing.T) {
	fake := awsfake.New(t)
	fake.Inject("imds", "/latest/meta-data/instance-id", awsfake.Fault{Status: 503})
	cfg := fake.Config()
	cfg.RetryBackoff = 0
	c := newClient(t, cfg)
	waits := noWait(c, func() float64 { return 0 })
	if _, err := c.IMDS.InstanceID(ctx); err == nil {
		t.Fatal("a metadata service that answers 503 gave an instance id")
	}
	if want := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond}; fmt.Sprint(*waits) != fmt.Sprint(want) {
		t.Errorf("waits %v, want %v", *waits, want)
	}
}
