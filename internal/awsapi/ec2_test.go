package awsapi_test

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/awsapi"
)

var ctx = context.Background()

// Every EC2 action sends the Query API form with sorted fields, the API version, the credentials'
// signature and no field for an empty value.
func TestEC2RequestBodies(t *testing.T) {
	const v = "&Version=2016-11-15"
	cases := []struct {
		name string
		body string
		call func(c *awsapi.Client) error
	}{
		{"DescribeInstances", "Action=DescribeInstances&Filter.1.Name=tag%3Asupavise%3Acluster&Filter.1.Value.1=prod&Filter.1.Value.2=my%20cluster&InstanceId.1=i-1&InstanceId.2=i-2" + v,
			func(c *awsapi.Client) error {
				_, err := c.EC2.DescribeInstances(ctx, awsapi.DescribeInstancesInput{
					InstanceIDs: []string{"i-1", "i-2"},
					Filters:     []awsapi.Filter{{Name: "tag:supavise:cluster", Values: []string{"prod", "my cluster"}}},
				})
				return err
			}},
		{"DescribeInstances dry run", "Action=DescribeInstances&DryRun=true" + v,
			func(c *awsapi.Client) error {
				_, err := c.EC2.DescribeInstances(ctx, awsapi.DescribeInstancesInput{DryRun: true})
				return err
			}},
		{"DescribeInstanceStatus", "Action=DescribeInstanceStatus&IncludeAllInstances=true&InstanceId.1=i-1" + v,
			func(c *awsapi.Client) error {
				_, err := c.EC2.DescribeInstanceStatus(ctx, awsapi.DescribeInstanceStatusInput{InstanceIDs: []string{"i-1"}, IncludeAll: true})
				return err
			}},
		{"StopInstances", "Action=StopInstances&InstanceId.1=i-0peer" + v,
			func(c *awsapi.Client) error {
				_, err := c.EC2.StopInstances(ctx, awsapi.StopInstancesInput{InstanceIDs: []string{"i-0peer"}})
				return err
			}},
		{"StopInstances force dry run", "Action=StopInstances&DryRun=true&Force=true&InstanceId.1=i-0peer" + v,
			func(c *awsapi.Client) error {
				_, err := c.EC2.StopInstances(ctx, awsapi.StopInstancesInput{InstanceIDs: []string{"i-0peer"}, Force: true, DryRun: true})
				return err
			}},
		{"AssociateAddress", "Action=AssociateAddress&AllocationId=eipalloc-1&AllowReassociation=true&InstanceId=i-0self&PrivateIpAddress=10.77.0.11" + v,
			func(c *awsapi.Client) error {
				_, err := c.EC2.AssociateAddress(ctx, awsapi.AssociateAddressInput{AllocationID: "eipalloc-1", InstanceID: "i-0self", PrivateIP: "10.77.0.11", AllowReassociation: true})
				return err
			}},
		{"AssociateAddress by interface", "Action=AssociateAddress&AllocationId=eipalloc-1&AllowReassociation=false&NetworkInterfaceId=eni-1" + v,
			func(c *awsapi.Client) error {
				_, err := c.EC2.AssociateAddress(ctx, awsapi.AssociateAddressInput{AllocationID: "eipalloc-1", NetworkInterfaceID: "eni-1"})
				return err
			}},
		{"DisassociateAddress", "Action=DisassociateAddress&AssociationId=eipassoc-1" + v,
			func(c *awsapi.Client) error {
				return c.EC2.DisassociateAddress(ctx, awsapi.DisassociateAddressInput{AssociationID: "eipassoc-1"})
			}},
		{"DescribeAddresses", "Action=DescribeAddresses&AllocationId.1=eipalloc-1&PublicIp.1=203.0.113.25" + v,
			func(c *awsapi.Client) error {
				_, err := c.EC2.DescribeAddresses(ctx, awsapi.DescribeAddressesInput{AllocationIDs: []string{"eipalloc-1"}, PublicIPs: []string{"203.0.113.25"}})
				return err
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dry := strings.Contains(tc.body, "DryRun=true")
			ans := xmlOK("<Response/>")
			if dry {
				ans = answer{status: 412, body: `<Response><Errors><Error><Code>DryRunOperation</Code><Message>Request would have succeeded, but DryRun flag is set.</Message></Error></Errors><RequestID>r</RequestID></Response>`}
			}
			cfg, seen := stub(t, ans)
			if err := tc.call(newClient(t, cfg)); err != nil {
				t.Fatal(err)
			}
			reqs := seen()
			if len(reqs) != 1 {
				t.Fatalf("%d requests", len(reqs))
			}
			r := reqs[0]
			if r.Body != tc.body {
				t.Errorf("body\n got: %s\nwant: %s", r.Body, tc.body)
			}
			if r.Method != "POST" || r.Path != "/" || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded; charset=utf-8" {
				t.Errorf("request line or content type: %s %s %q", r.Method, r.Path, r.Header.Get("Content-Type"))
			}
		})
	}
}

// The whole signed request for one call, pinned so that a change to the headers, the signed set
// or the scope shows up as a diff. The signature is checked independently by Verify in stub.
func TestEC2SignedRequestGolden(t *testing.T) {
	cfg, seen := stub(t, xmlOK("<DescribeInstancesResponse/>"))
	if _, err := newClient(t, cfg).EC2.DescribeInstances(ctx, awsapi.DescribeInstancesInput{InstanceIDs: []string{"i-1"}}); err != nil {
		t.Fatal(err)
	}
	h := seen()[0].Header
	want := map[string]string{
		"Content-Type": "application/x-www-form-urlencoded; charset=utf-8",
		"X-Amz-Date":   "20261008T120000Z",
		"User-Agent":   "supavise-awsapi",
	}
	for k, v := range want {
		if h.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, h.Get(k), v)
		}
	}
	const authPrefix = "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20261008/us-east-1/ec2/aws4_request, SignedHeaders=content-type;host;x-amz-date, Signature="
	if a := h.Get("Authorization"); !strings.HasPrefix(a, authPrefix) || len(a) != len(authPrefix)+64 {
		t.Errorf("Authorization = %q", a)
	}
	if h.Get("X-Amz-Security-Token") != "" {
		t.Error("a session token was sent for credentials without one")
	}
}

func TestDescribeInstancesParsesTheReply(t *testing.T) {
	cfg, seen := stub(t, xmlOK(fixture(t, "describe_instances.xml")), xmlOK(fixture(t, "describe_instances_page2.xml")))
	got, err := newClient(t, cfg).EC2.DescribeInstances(ctx, awsapi.DescribeInstancesInput{})
	if err != nil {
		t.Fatal(err)
	}
	reqs := seen()
	if len(reqs) != 2 || strings.Contains(reqs[0].Body, "NextToken") || !strings.Contains(reqs[1].Body, "NextToken=page-2-token") {
		t.Fatalf("paging: %+v", reqs)
	}
	want := []awsapi.Instance{
		{
			ID: "i-1234567890abcdef0", State: "running", PrivateIP: "10.77.0.10", PublicIP: "203.0.113.25",
			AvailabilityZone: "us-east-1a", SubnetID: "subnet-0bb1c79de3EXAMPLE", VPCID: "vpc-0bb1c79de3EXAMPLE",
			NetworkInterfaces: []awsapi.NetworkInterface{{
				ID: "eni-0a1b2c3d4e5f60718", DeviceIndex: 0,
				PrivateIPs: []awsapi.PrivateIP{{Address: "10.77.0.10", Primary: true, PublicIP: "203.0.113.25"}, {Address: "10.77.0.11"}},
			}},
			Tags: map[string]string{"supavise:cluster": "prod", "Name": "supavise & co"},
		},
		{
			ID: "i-0fedcba9876543210", State: "stopped", PrivateIP: "10.77.0.20", AvailabilityZone: "us-east-1b",
			SubnetID: "subnet-0bb1c79de3EXAMPLE", VPCID: "vpc-0bb1c79de3EXAMPLE",
			NetworkInterfaces: []awsapi.NetworkInterface{{ID: "eni-0fedcba987654321", PrivateIPs: []awsapi.PrivateIP{{Address: "10.77.0.20", Primary: true}}}},
			Tags:              map[string]string{},
		},
		{ID: "i-0333333333333333", State: "running", PrivateIP: "10.77.0.30", Tags: map[string]string{}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("instances\n got: %+v\nwant: %+v", got, want)
	}
}

func TestDescribeInstanceStatusParsesTheReply(t *testing.T) {
	cfg, _ := stub(t, xmlOK(fixture(t, "describe_instance_status.xml")))
	got, err := newClient(t, cfg).EC2.DescribeInstanceStatus(ctx, awsapi.DescribeInstanceStatusInput{IncludeAll: true})
	if err != nil {
		t.Fatal(err)
	}
	want := []awsapi.InstanceStatus{
		{
			ID: "i-1234567890abcdef0", State: "running", AvailabilityZone: "us-east-1d", System: "ok", Instance: "impaired",
			Events: []awsapi.StatusEvent{{
				Code: "instance-reboot", Description: "The instance is scheduled for a reboot",
				NotBefore: time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC), NotAfter: time.Date(2026, 10, 12, 18, 0, 0, 0, time.UTC),
			}},
		},
		{ID: "i-0fedcba9876543210", State: "stopped", AvailabilityZone: "us-east-1b", System: "not-applicable", Instance: "not-applicable"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("statuses\n got: %+v\nwant: %+v", got, want)
	}
}

func TestStopAssociateAndAddressesParseTheReply(t *testing.T) {
	cfg, _ := stub(t, xmlOK(fixture(t, "stop_instances.xml")), xmlOK(fixture(t, "associate_address.xml")), xmlOK(fixture(t, "describe_addresses.xml")))
	c := newClient(t, cfg)
	changes, err := c.EC2.StopInstances(ctx, awsapi.StopInstancesInput{InstanceIDs: []string{"i-1234567890abcdef0"}})
	if err != nil || !reflect.DeepEqual(changes, []awsapi.StateChange{{InstanceID: "i-1234567890abcdef0", Previous: "running", Current: "stopping"}}) {
		t.Errorf("StopInstances = %+v, %v", changes, err)
	}
	assoc, err := c.EC2.AssociateAddress(ctx, awsapi.AssociateAddressInput{AllocationID: "eipalloc-64d5890a", InstanceID: "i-1234567890abcdef0"})
	if err != nil || assoc != "eipassoc-2bebb745" {
		t.Errorf("AssociateAddress = %q, %v", assoc, err)
	}
	addrs, err := c.EC2.DescribeAddresses(ctx, awsapi.DescribeAddressesInput{})
	want := []awsapi.Address{
		{AllocationID: "eipalloc-64d5890a", PublicIP: "203.0.113.25", AssociationID: "eipassoc-2bebb745", InstanceID: "i-1234567890abcdef0",
			NetworkInterfaceID: "eni-0a1b2c3d4e5f60718", PrivateIP: "10.77.0.11", Tags: map[string]string{"supavise:cluster": "prod"}},
		{AllocationID: "eipalloc-0123abcd", PublicIP: "198.51.100.7", Tags: map[string]string{}},
	}
	if err != nil || !reflect.DeepEqual(addrs, want) {
		t.Errorf("DescribeAddresses = %+v, %v", addrs, err)
	}
}

// DryRunOperation means the call would have been allowed; UnauthorizedOperation means it would not.
func TestDryRunMapping(t *testing.T) {
	dryRunOp := answer{status: 412, body: `<Response><Errors><Error><Code>DryRunOperation</Code><Message>Request would have succeeded, but DryRun flag is set.</Message></Error></Errors><RequestID>r1</RequestID></Response>`}
	denied := answer{status: 403, body: fixture(t, "ec2_error.xml")}
	notFound := answer{status: 400, body: `<Response><Errors><Error><Code>InvalidInstanceID.NotFound</Code><Message>The instance ID 'i-0nope' does not exist</Message></Error></Errors><RequestID>r2</RequestID></Response>`}
	stop := awsapi.StopInstancesInput{InstanceIDs: []string{"i-1"}, DryRun: true}

	cfg, seen := stub(t, dryRunOp)
	if changes, err := newClient(t, cfg).EC2.StopInstances(ctx, stop); err != nil || changes != nil {
		t.Errorf("DryRunOperation: %v, %v (want success)", changes, err)
	}
	if n := len(seen()); n != 1 {
		t.Errorf("DryRunOperation was sent %d times", n)
	}

	cfg, seen = stub(t, denied)
	_, err := newClient(t, cfg).EC2.StopInstances(ctx, stop)
	if !awsapi.IsAccessDenied(err) || !awsapi.IsCode(err, "UnauthorizedOperation") {
		t.Errorf("UnauthorizedOperation: %v (want access denied)", err)
	}
	if n := len(seen()); n != 1 {
		t.Errorf("a refusal was sent %d times; it is final", n)
	}

	cfg, _ = stub(t, notFound)
	_, err = newClient(t, cfg).EC2.StopInstances(ctx, stop)
	if err == nil || awsapi.IsAccessDenied(err) || !awsapi.IsNotFound(err) {
		t.Errorf("a dry run for a missing instance: %v (want not found)", err)
	}

	// Every call that takes DryRun maps it the same way.
	cfg, _ = stub(t, dryRunOp)
	c := newClient(t, cfg)
	checks := map[string]error{
		"AssociateAddress": func() error {
			_, err := c.EC2.AssociateAddress(ctx, awsapi.AssociateAddressInput{AllocationID: "a", InstanceID: "i", DryRun: true})
			return err
		}(),
		"DisassociateAddress": c.EC2.DisassociateAddress(ctx, awsapi.DisassociateAddressInput{AssociationID: "x", DryRun: true}),
		"DescribeAddresses": func() error {
			_, err := c.EC2.DescribeAddresses(ctx, awsapi.DescribeAddressesInput{DryRun: true})
			return err
		}(),
		"DescribeInstanceStatus": func() error {
			_, err := c.EC2.DescribeInstanceStatus(ctx, awsapi.DescribeInstanceStatusInput{DryRun: true})
			return err
		}(),
	}
	for name, err := range checks {
		if err != nil {
			t.Errorf("%s dry run: %v", name, err)
		}
	}
}

func TestErrorsCarryTheAPIFields(t *testing.T) {
	cfg, _ := stub(t, answer{status: 403, body: fixture(t, "ec2_error.xml")})
	_, err := newClient(t, cfg).EC2.StopInstances(ctx, awsapi.StopInstancesInput{InstanceIDs: []string{"i-1"}})
	var e *awsapi.Error
	if !asError(err, &e) {
		t.Fatalf("%T %v", err, err)
	}
	if e.Service != "ec2" || e.Action != "StopInstances" || e.StatusCode != 403 || e.Code != "UnauthorizedOperation" || e.RequestID != "b1c2d3e4-0000-1111-2222-example" || !strings.Contains(e.Message, "ec2:StopInstances") {
		t.Errorf("%+v", e)
	}
	if !strings.Contains(err.Error(), "aws ec2 StopInstances: UnauthorizedOperation") || !strings.Contains(err.Error(), "request id b1c2d3e4") {
		t.Errorf("message: %s", err)
	}
}

func TestRetries(t *testing.T) {
	throttle := answer{status: 503, body: `<Response><Errors><Error><Code>RequestLimitExceeded</Code><Message>slow down</Message></Error></Errors><RequestID>r</RequestID></Response>`}
	ok := xmlOK(fixture(t, "stop_instances.xml"))
	in := awsapi.StopInstancesInput{InstanceIDs: []string{"i-1234567890abcdef0"}}

	cfg, seen := stub(t, throttle, throttle, ok)
	if _, err := newClient(t, cfg).EC2.StopInstances(ctx, in); err != nil {
		t.Errorf("two throttles then success: %v", err)
	}
	if n := len(seen()); n != 3 {
		t.Errorf("%d attempts, want 3", n)
	}

	cfg, seen = stub(t, throttle)
	if _, err := newClient(t, cfg).EC2.StopInstances(ctx, in); !awsapi.IsCode(err, "RequestLimitExceeded") {
		t.Errorf("persistent throttle: %v", err)
	}
	if n := len(seen()); n != 3 {
		t.Errorf("%d attempts, want the default 3", n)
	}

	cfg, seen = stub(t, throttle)
	cfg.MaxAttempts = 1
	newClient(t, cfg).EC2.StopInstances(ctx, in)
	if n := len(seen()); n != 1 {
		t.Errorf("MaxAttempts 1 sent %d requests", n)
	}

	cfg, seen = stub(t, answer{status: 400, body: `<Response><Errors><Error><Code>InvalidParameterValue</Code><Message>bad</Message></Error></Errors></Response>`})
	newClient(t, cfg).EC2.StopInstances(ctx, in)
	if n := len(seen()); n != 1 {
		t.Errorf("a 400 was sent %d times", n)
	}
}

func TestRetryStopsWhenTheContextEnds(t *testing.T) {
	cfg, _ := stub(t, answer{status: 500, body: "<Response/>"})
	cfg.RetryBackoff = time.Hour
	c := newClient(t, cfg)
	cctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.EC2.StopInstances(cctx, awsapi.StopInstancesInput{InstanceIDs: []string{"i-1"}})
	if err == nil || time.Since(start) > 5*time.Second {
		t.Errorf("err = %v after %v", err, time.Since(start))
	}
}

func TestUnreachableEndpointIsRetriedThenReported(t *testing.T) {
	cfg, _ := stub(t, xmlOK("<x/>"))
	cfg.Endpoints.EC2 = "http://127.0.0.1:1"
	_, err := newClient(t, cfg).EC2.DescribeInstances(ctx, awsapi.DescribeInstancesInput{})
	if err == nil || !strings.Contains(err.Error(), "aws ec2 DescribeInstances") {
		t.Errorf("err = %v", err)
	}
}
