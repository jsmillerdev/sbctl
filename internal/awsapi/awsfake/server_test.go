package awsfake_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/supavise/supavise/internal/awsapi"
	"github.com/supavise/supavise/internal/awsapi/awsfake"
)

var ctx = context.Background()

// seedPair is the failover pair: this node, with a secondary private address, and its peer, which
// holds the service address.
func seedPair(fake *awsfake.Server, peer awsfake.Instance) {
	fake.AddInstance(awsfake.Instance{ID: "i-0self", PrivateIP: "10.77.0.10", SecondaryIPs: []string{"10.77.0.11"}, Tags: map[string]string{"supavise:cluster": "prod"}})
	peer.ID, peer.PrivateIP = "i-0peer", "10.77.1.10"
	peer.SecondaryIPs = []string{"10.77.1.11"}
	peer.Tags = map[string]string{"supavise:cluster": "prod"}
	fake.AddInstance(peer)
	fake.AddAddress(awsfake.Address{AllocationID: "eipalloc-svc", PublicIP: "203.0.113.50", InstanceID: "i-0peer", PrivateIP: "10.77.1.11"})
}

// The sequence design 2.10.6 asks of the AWS fencer, run against the fake: look at the peer, stop
// it, wait for "stopped", then take the address.
func TestFencingSequenceIsRecorded(t *testing.T) {
	fake := awsfake.New(t)
	seedPair(fake, awsfake.Instance{StopPolls: 2})
	ec2 := fake.Client().EC2

	if _, err := ec2.DescribeInstances(ctx, awsapi.DescribeInstancesInput{InstanceIDs: []string{"i-0peer"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := ec2.DescribeInstanceStatus(ctx, awsapi.DescribeInstanceStatusInput{InstanceIDs: []string{"i-0peer"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := ec2.StopInstances(ctx, awsapi.StopInstancesInput{InstanceIDs: []string{"i-0peer"}, DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if got := fake.InstanceState("i-0peer"); got != "running" {
		t.Fatalf("a dry run changed the state to %s", got)
	}
	changes, err := ec2.StopInstances(ctx, awsapi.StopInstancesInput{InstanceIDs: []string{"i-0peer"}})
	if err != nil || len(changes) != 1 || changes[0].Previous != "running" || changes[0].Current != "stopping" {
		t.Fatalf("stop: %+v, %v", changes, err)
	}
	var states []string
	for i := 0; i < 10; i++ {
		got, err := ec2.DescribeInstances(ctx, awsapi.DescribeInstancesInput{InstanceIDs: []string{"i-0peer"}})
		if err != nil {
			t.Fatal(err)
		}
		states = append(states, got[0].State)
		if got[0].State == "stopped" {
			break
		}
	}
	if !reflect.DeepEqual(states, []string{"stopping", "stopping", "stopped"}) {
		t.Errorf("polls saw %v", states)
	}
	if _, err := ec2.AssociateAddress(ctx, awsapi.AssociateAddressInput{AllocationID: "eipalloc-svc", InstanceID: "i-0self", PrivateIP: "10.77.0.11", AllowReassociation: true}); err != nil {
		t.Fatal(err)
	}

	want := []string{
		"ec2:DescribeInstances", "ec2:DescribeInstanceStatus", "ec2:StopInstances(dryrun)", "ec2:StopInstances",
		"ec2:DescribeInstances", "ec2:DescribeInstances", "ec2:DescribeInstances", "ec2:AssociateAddress",
	}
	if got := fake.Order(); !reflect.DeepEqual(got, want) {
		t.Errorf("order\n got: %v\nwant: %v", got, want)
	}
	if a := fake.AddressOf("eipalloc-svc"); a.InstanceID != "i-0self" || a.PrivateIP != "10.77.0.11" {
		t.Errorf("address is now on %+v", a)
	}
	// The takeover kept the survivor's own address and moved only the service address.
	self, _ := ec2.DescribeInstances(ctx, awsapi.DescribeInstancesInput{InstanceIDs: []string{"i-0self"}})
	if ips := self[0].NetworkInterfaces[0].PrivateIPs; len(ips) != 2 || ips[1].PublicIP != "203.0.113.50" || ips[0].PublicIP != "" {
		t.Errorf("survivor addresses: %+v", ips)
	}
}

func TestStopThatNeedsForce(t *testing.T) {
	fake := awsfake.New(t)
	seedPair(fake, awsfake.Instance{StopNeedsForce: true})
	ec2 := fake.Client().EC2
	stop := func(force bool) {
		t.Helper()
		if _, err := ec2.StopInstances(ctx, awsapi.StopInstancesInput{InstanceIDs: []string{"i-0peer"}, Force: force}); err != nil {
			t.Fatal(err)
		}
	}
	state := func() string {
		t.Helper()
		got, err := ec2.DescribeInstances(ctx, awsapi.DescribeInstancesInput{InstanceIDs: []string{"i-0peer"}})
		if err != nil {
			t.Fatal(err)
		}
		return got[0].State
	}
	stop(false)
	for i := 0; i < 5; i++ {
		if s := state(); s != "stopping" {
			t.Fatalf("a plain stop reached %s", s)
		}
	}
	stop(true)
	if s := state(); s != "stopped" {
		t.Errorf("after Force: %s", s)
	}
	// Describe polls reach the fake with Force recorded for the second stop only.
	var forces []string
	for _, c := range fake.Calls() {
		if c.Action == "StopInstances" {
			forces = append(forces, c.Params.Get("Force"))
		}
	}
	if !reflect.DeepEqual(forces, []string{"", "true"}) {
		t.Errorf("Force on stops: %q", forces)
	}
}

func TestAssociateAddressMovesAndReplaces(t *testing.T) {
	fake := awsfake.New(t)
	fake.AddInstance(awsfake.Instance{ID: "i-0a", PrivateIP: "10.0.0.10", PublicIP: "198.51.100.1", SecondaryIPs: []string{"10.0.0.11"}})
	fake.AddInstance(awsfake.Instance{ID: "i-0b", PrivateIP: "10.0.1.10"})
	fake.AddAddress(awsfake.Address{AllocationID: "eipalloc-own", PublicIP: "203.0.113.1"})
	fake.AddAddress(awsfake.Address{AllocationID: "eipalloc-svc", PublicIP: "203.0.113.2", InstanceID: "i-0b"})
	ec2 := fake.Client().EC2

	publicOf := func(id string) string {
		t.Helper()
		got, err := ec2.DescribeInstances(ctx, awsapi.DescribeInstancesInput{InstanceIDs: []string{id}})
		if err != nil {
			t.Fatal(err)
		}
		return got[0].PublicIP
	}
	if publicOf("i-0a") != "198.51.100.1" {
		t.Error("auto-assigned address missing")
	}

	// An address that is associated elsewhere moves only when reassociation is allowed.
	_, err := ec2.AssociateAddress(ctx, awsapi.AssociateAddressInput{AllocationID: "eipalloc-svc", InstanceID: "i-0a", PrivateIP: "10.0.0.11"})
	if !awsapi.IsCode(err, "Resource.AlreadyAssociated") {
		t.Errorf("without AllowReassociation: %v", err)
	}
	if _, err := ec2.AssociateAddress(ctx, awsapi.AssociateAddressInput{AllocationID: "eipalloc-svc", InstanceID: "i-0a", PrivateIP: "10.0.0.11", AllowReassociation: true}); err != nil {
		t.Fatal(err)
	}
	if publicOf("i-0b") != "" || publicOf("i-0a") != "198.51.100.1" {
		t.Errorf("after moving to a secondary address: b=%q a=%q", publicOf("i-0b"), publicOf("i-0a"))
	}

	// Associating with the primary address replaces the auto-assigned one.
	if _, err := ec2.AssociateAddress(ctx, awsapi.AssociateAddressInput{AllocationID: "eipalloc-own", InstanceID: "i-0a"}); err != nil {
		t.Fatal(err)
	}
	if publicOf("i-0a") != "203.0.113.1" {
		t.Errorf("primary public ip = %q", publicOf("i-0a"))
	}
	// A second Elastic IP on the same private address releases the first, which stays allocated.
	fake.AddAddress(awsfake.Address{AllocationID: "eipalloc-new", PublicIP: "203.0.113.3"})
	if _, err := ec2.AssociateAddress(ctx, awsapi.AssociateAddressInput{AllocationID: "eipalloc-new", InstanceID: "i-0a"}); err != nil {
		t.Fatal(err)
	}
	addrs, err := ec2.DescribeAddresses(ctx, awsapi.DescribeAddressesInput{AllocationIDs: []string{"eipalloc-own"}})
	if err != nil || len(addrs) != 1 || addrs[0].InstanceID != "" || addrs[0].AssociationID != "" || addrs[0].PublicIP != "203.0.113.1" {
		t.Errorf("replaced address: %+v, %v", addrs, err)
	}

	all, _ := ec2.DescribeAddresses(ctx, awsapi.DescribeAddressesInput{Filters: []awsapi.Filter{{Name: "instance-id", Values: []string{"i-0a"}}}})
	if len(all) != 2 {
		t.Errorf("addresses on i-0a: %+v", all)
	}
	var assoc string
	for _, a := range all {
		if a.AllocationID == "eipalloc-new" {
			assoc = a.AssociationID
		}
	}
	if err := ec2.DisassociateAddress(ctx, awsapi.DisassociateAddressInput{AssociationID: assoc}); err != nil {
		t.Fatal(err)
	}
	if err := ec2.DisassociateAddress(ctx, awsapi.DisassociateAddressInput{AssociationID: assoc}); !awsapi.IsNotFound(err) {
		t.Errorf("second disassociate: %v", err)
	}
	if _, err := ec2.AssociateAddress(ctx, awsapi.AssociateAddressInput{AllocationID: "eipalloc-none", InstanceID: "i-0a"}); !awsapi.IsNotFound(err) {
		t.Errorf("unknown allocation: %v", err)
	}
	if _, err := ec2.AssociateAddress(ctx, awsapi.AssociateAddressInput{AllocationID: "eipalloc-new", InstanceID: "i-0a", PrivateIP: "10.9.9.9"}); !awsapi.IsCode(err, "InvalidParameterValue") {
		t.Errorf("foreign private address: %v", err)
	}
}

func TestDryRunAndDeny(t *testing.T) {
	fake := awsfake.New(t)
	seedPair(fake, awsfake.Instance{})
	ec2 := fake.Client().EC2
	fake.Deny("ec2", "StopInstances")

	stop := awsapi.StopInstancesInput{InstanceIDs: []string{"i-0peer"}}
	for _, dry := range []bool{true, false} {
		stop.DryRun = dry
		if _, err := ec2.StopInstances(ctx, stop); !awsapi.IsAccessDenied(err) {
			t.Errorf("denied stop (dry run %v): %v", dry, err)
		}
	}
	if got := fake.InstanceState("i-0peer"); got != "running" {
		t.Errorf("a denied stop changed the state to %s", got)
	}
	// Other actions are untouched, and a permitted dry run changes nothing.
	if _, err := ec2.AssociateAddress(ctx, awsapi.AssociateAddressInput{AllocationID: "eipalloc-svc", InstanceID: "i-0self", AllowReassociation: true, DryRun: true}); err != nil {
		t.Errorf("permitted dry run: %v", err)
	}
	if a := fake.AddressOf("eipalloc-svc"); a.InstanceID != "i-0peer" {
		t.Errorf("a dry run moved the address to %s", a.InstanceID)
	}
	if _, err := ec2.DescribeInstances(ctx, awsapi.DescribeInstancesInput{DryRun: true}); err != nil {
		t.Errorf("describe dry run: %v", err)
	}
	last := fake.Calls()[len(fake.Calls())-1]
	if !last.DryRun || last.Status != 412 || last.Code != "DryRunOperation" {
		t.Errorf("recorded %+v", last)
	}
}

func TestSignaturesAreChecked(t *testing.T) {
	fake := awsfake.New(t)
	cfg := fake.Config()
	cfg.MaxAttempts = 1

	cfg.Credentials = awsapi.StaticCredentials(awsapi.Credentials{AccessKeyID: fake.Credentials().AccessKeyID, SecretAccessKey: "the wrong secret"})
	c, _ := awsapi.New(cfg)
	if _, err := c.EC2.DescribeInstances(ctx, awsapi.DescribeInstancesInput{}); !awsapi.IsCode(err, "SignatureDoesNotMatch") {
		t.Errorf("wrong secret: %v", err)
	}
	cfg.Credentials = awsapi.StaticCredentials(awsapi.Credentials{AccessKeyID: "AKIAUNKNOWN", SecretAccessKey: "x"})
	c, _ = awsapi.New(cfg)
	if _, err := c.SecretsManager.GetSecretValue(ctx, awsapi.GetSecretValueInput{SecretID: "x"}); !awsapi.IsCode(err, "SignatureDoesNotMatch") {
		t.Errorf("unknown key: %v", err)
	}
	cfg.Credentials = awsapi.StaticCredentials(fake.Credentials())
	c, _ = awsapi.New(cfg)
	if _, err := c.EC2.DescribeInstances(ctx, awsapi.DescribeInstancesInput{}); err != nil {
		t.Errorf("right credentials: %v", err)
	}
	var statuses []int
	for _, call := range fake.Calls() {
		statuses = append(statuses, call.Status)
	}
	if !reflect.DeepEqual(statuses, []int{403, 403, 200}) {
		t.Errorf("recorded statuses %v", statuses)
	}
}

func TestInjectedFaultsRunOutAndFiltersWork(t *testing.T) {
	fake := awsfake.New(t)
	seedPair(fake, awsfake.Instance{})
	fake.Inject("ec2", "DescribeInstances", awsfake.Fault{Status: 503, Code: "RequestLimitExceeded", Message: "slow", Times: 1})
	ec2 := fake.Client().EC2

	// The client retries the one injected failure, so the caller sees success and two calls.
	got, err := ec2.DescribeInstances(ctx, awsapi.DescribeInstancesInput{Filters: []awsapi.Filter{{Name: "tag:supavise:cluster", Values: []string{"prod"}}}})
	if err != nil || len(got) != 2 {
		t.Fatalf("%+v, %v", got, err)
	}
	if o := fake.Order(); !reflect.DeepEqual(o, []string{"ec2:DescribeInstances", "ec2:DescribeInstances"}) {
		t.Errorf("order %v", o)
	}
	got, _ = ec2.DescribeInstances(ctx, awsapi.DescribeInstancesInput{Filters: []awsapi.Filter{{Name: "tag:supavise:cluster", Values: []string{"other"}}}})
	if len(got) != 0 {
		t.Errorf("filter on another cluster matched %+v", got)
	}
	got, _ = ec2.DescribeInstances(ctx, awsapi.DescribeInstancesInput{Filters: []awsapi.Filter{{Name: "instance-state-name", Values: []string{"running"}}, {Name: "private-ip-address", Values: []string{"10.77.1.10"}}}})
	if len(got) != 1 || got[0].ID != "i-0peer" {
		t.Errorf("state and address filters matched %+v", got)
	}
	if _, err := ec2.DescribeInstances(ctx, awsapi.DescribeInstancesInput{Filters: []awsapi.Filter{{Name: "no-such-filter", Values: []string{"x"}}}}); !awsapi.IsCode(err, "InvalidParameterValue") {
		t.Errorf("unknown filter: %v", err)
	}
	if _, err := ec2.StopInstances(ctx, awsapi.StopInstancesInput{InstanceIDs: []string{"i-0missing"}}); !awsapi.IsNotFound(err) {
		t.Errorf("unknown instance: %v", err)
	}
}

// An applied fault is an answer lost on the way: the call took effect and the client saw an error.
func TestAppliedFaultTakesEffect(t *testing.T) {
	fake := awsfake.New(t)
	seedPair(fake, awsfake.Instance{})
	cfg := fake.Config()
	cfg.MaxAttempts = 1
	c, err := awsapi.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	fake.Inject("ec2", "StopInstances", awsfake.Fault{Status: 503, Code: "InternalError", Message: "lost", Times: 2, Applied: true})
	stop := awsapi.StopInstancesInput{InstanceIDs: []string{"i-0peer"}}

	stop.DryRun = true
	if _, err := c.EC2.StopInstances(ctx, stop); !awsapi.IsCode(err, "InternalError") {
		t.Errorf("dry run: %v", err)
	}
	if got := fake.InstanceState("i-0peer"); got != "running" {
		t.Errorf("a dry run with an applied fault changed the state to %s", got)
	}
	stop.DryRun = false
	if _, err := c.EC2.StopInstances(ctx, stop); !awsapi.IsCode(err, "InternalError") {
		t.Errorf("real call: %v", err)
	}
	if got := fake.InstanceState("i-0peer"); got != "stopping" {
		t.Errorf("the applied fault left the state at %s", got)
	}
	if _, err := c.EC2.StopInstances(ctx, stop); err != nil {
		t.Errorf("after the faults ran out: %v", err)
	}
}

func TestPaging(t *testing.T) {
	fake := awsfake.New(t)
	for _, id := range []string{"i-1", "i-2", "i-3", "i-4", "i-5"} {
		fake.AddInstance(awsfake.Instance{ID: id})
	}
	fake.SetPageSize(2)
	ec2 := fake.Client().EC2
	got, err := ec2.DescribeInstances(ctx, awsapi.DescribeInstancesInput{})
	if err != nil || len(got) != 5 || got[0].ID != "i-1" || got[4].ID != "i-5" {
		t.Errorf("%+v, %v", got, err)
	}
	if n := len(fake.Order()); n != 3 {
		t.Errorf("%d pages", n)
	}
	st, err := ec2.DescribeInstanceStatus(ctx, awsapi.DescribeInstanceStatusInput{})
	if err != nil || len(st) != 5 || st[0].System != "ok" {
		t.Errorf("%+v, %v", st, err)
	}
}

func TestSecretsAndEnvironment(t *testing.T) {
	fake := awsfake.New(t)
	fake.AddSecret(awsfake.Secret{Name: "supavise/storage", String: `{"access_key_id":"AK"}`})
	fake.AddSecret(awsfake.Secret{Name: "blob", Binary: []byte{1, 2, 3}})

	// Code that builds its own client from the environment reaches the fake through the instance role.
	fake.SetEnv(t)
	c, err := awsapi.New(awsapi.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sec, err := c.SecretsManager.GetSecretValue(ctx, awsapi.GetSecretValueInput{SecretID: "supavise/storage"})
	if err != nil || sec.SecretString != `{"access_key_id":"AK"}` || !strings.HasPrefix(sec.ARN, "arn:aws:secretsmanager:us-east-1:123456789012:secret:supavise/storage-") {
		t.Fatalf("%+v, %v", sec, err)
	}
	byARN, err := c.SecretsManager.GetSecretValue(ctx, awsapi.GetSecretValueInput{SecretID: sec.ARN})
	if err != nil || byARN.Name != "supavise/storage" {
		t.Errorf("by ARN: %+v, %v", byARN, err)
	}
	bin, err := c.SecretsManager.GetSecretValue(ctx, awsapi.GetSecretValueInput{SecretID: "blob"})
	if err != nil || !reflect.DeepEqual(bin.SecretBinary, []byte{1, 2, 3}) {
		t.Errorf("binary: %+v, %v", bin, err)
	}
	if _, err := c.SecretsManager.GetSecretValue(ctx, awsapi.GetSecretValueInput{SecretID: "missing"}); !awsapi.IsNotFound(err) {
		t.Errorf("missing: %v", err)
	}
	fake.Deny("secretsmanager", "GetSecretValue")
	if _, err := c.SecretsManager.GetSecretValue(ctx, awsapi.GetSecretValueInput{SecretID: "blob"}); !awsapi.IsAccessDenied(err) {
		t.Errorf("denied: %v", err)
	}
	if got := fake.Order("secretsmanager"); len(got) != 5 || got[0] != "secretsmanager:GetSecretValue" {
		t.Errorf("order %v", got)
	}
	last := fake.Calls()[len(fake.Calls())-1]
	if last.Params.Get("SecretId") != "blob" || last.AccessKeyID != "ASIAFAKEROLE0000000" {
		t.Errorf("last call %+v", last)
	}
}
