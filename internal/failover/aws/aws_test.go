package aws

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/awsapi/awsfake"
	"github.com/supavise/supavise/internal/failover"
	"github.com/supavise/supavise/internal/registry"
)

// rig is two instances in a fake EC2: n1, the old primary, with the service address, and n2, the
// survivor, which has an Elastic IP of its own on its primary address and a secondary address.
type rig struct {
	fake *awsfake.Server
	p    *Provider
	req  failover.Request
}

func newRig(t *testing.T, mut ...func(*awsfake.Server)) *rig {
	t.Helper()
	return newRigOn(t, true, mut...)
}

// newRigOn is newRig; withOwnEIP says whether n2 has an Elastic IP of its own on its primary address.
func newRigOn(t *testing.T, withOwnEIP bool, mut ...func(*awsfake.Server)) *rig {
	t.Helper()
	fake := awsfake.New(t)
	fake.AddInstance(awsfake.Instance{ID: "i-n1", PrivateIP: "10.77.0.10", StopPolls: 2})
	fake.AddInstance(awsfake.Instance{ID: "i-n2", PrivateIP: "10.77.0.20", SecondaryIPs: []string{"10.77.0.21"}})
	fake.AddAddress(awsfake.Address{AllocationID: "eipalloc-svc", PublicIP: "203.0.113.9", InstanceID: "i-n1"})
	if withOwnEIP {
		fake.AddAddress(awsfake.Address{AllocationID: "eipalloc-n2", PublicIP: "203.0.113.20", InstanceID: "i-n2"})
	}
	for _, m := range mut {
		m(fake)
	}
	client := fake.Client()
	n1 := registry.Node{ID: "n1", Name: "primary", State: registry.NodeActive, Provider: registry.NodeProvider{AWS: &registry.NodeAWS{InstanceID: "i-n1", Region: "us-east-1"}}}
	n2 := registry.Node{ID: "n2", Name: "standby", State: registry.NodeActive, Provider: registry.NodeProvider{AWS: &registry.NodeAWS{InstanceID: "i-n2", Region: "us-east-1"}}}
	svc := registry.ServiceAddress{IP: "203.0.113.9", AllocationID: "eipalloc-svc"}
	return &rig{
		fake: fake,
		p: &Provider{
			EC2: func(string) (EC2, error) { return client.EC2, nil },
			Cluster: func(context.Context) (Cluster, error) {
				return Cluster{Self: n2, Peers: []registry.Node{n1, n2}, Service: svc}, nil
			},
			StopTimeout: 2 * time.Second, Poll: time.Millisecond,
		},
		req: failover.Request{Old: n1, New: n2, Epoch: 2, ServiceAddress: svc},
	}
}

// subsequence reports whether want occurs in got in that order, with anything in between.
func subsequence(got, want []string) bool {
	i := 0
	for _, g := range got {
		if i < len(want) && g == want[i] {
			i++
		}
	}
	return i == len(want)
}

func TestFenceThenTakeOverKeepsTheOrder(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if err := r.p.Fence(ctx, r.req); err != nil {
		t.Fatal(err)
	}
	// The peer is stopped before the address moves.
	if st := r.fake.InstanceState("i-n1"); st != "stopped" {
		t.Fatalf("n1 is %s", st)
	}
	if got := r.fake.AddressOf("eipalloc-svc").InstanceID; got != "i-n1" {
		t.Fatalf("the address moved before TakeOver: %s", got)
	}
	if err := r.p.TakeOver(ctx, r.req); err != nil {
		t.Fatal(err)
	}
	got := r.fake.Order("ec2")
	want := []string{"ec2:DescribeInstances", "ec2:DescribeInstanceStatus", "ec2:StopInstances", "ec2:DescribeInstances", "ec2:DescribeAddresses", "ec2:AssociateAddress"}
	if !subsequence(got, want) {
		t.Fatalf("order\n%s\nwant (in this order)\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if i, j := slices.Index(got, "ec2:StopInstances"), slices.Index(got, "ec2:AssociateAddress"); i > j {
		t.Fatalf("the address was associated before the stop:\n%v", got)
	}
	// The peer had to be polled until it was stopped (two describe calls still saw it stopping).
	polls := 0
	for _, c := range r.fake.Calls() {
		if c.Action == "DescribeInstances" && c.Params.Get("InstanceId.1") == "i-n1" {
			polls++
		}
	}
	if polls < 4 { // before the stop, and three after it
		t.Fatalf("n1 was described %d times", polls)
	}
	// The service address maps to n2's secondary address, so its own Elastic IP stays where it was.
	a := r.fake.AddressOf("eipalloc-svc")
	if a.InstanceID != "i-n2" || a.PrivateIP != "10.77.0.21" {
		t.Fatalf("service address: %+v", a)
	}
	if own := r.fake.AddressOf("eipalloc-n2"); own.InstanceID != "i-n2" || own.PrivateIP != "10.77.0.20" {
		t.Fatalf("n2's own address was disturbed: %+v", own)
	}
	for _, c := range r.fake.Calls() {
		if c.Action == "AssociateAddress" && c.Params.Get("AllowReassociation") != "true" {
			t.Fatalf("AllocationId was associated without reassociation: %v", c.Params)
		}
	}
}

func TestAStopThatNeedsForceIsRetriedWithForce(t *testing.T) {
	r := newRig(t, func(f *awsfake.Server) {
		f.AddInstance(awsfake.Instance{ID: "i-n1", PrivateIP: "10.77.0.10", StopNeedsForce: true})
	})
	r.p.StopTimeout = 60 * time.Millisecond
	if err := r.p.Fence(context.Background(), r.req); err != nil {
		t.Fatal(err)
	}
	var stops []string
	for _, c := range r.fake.Calls() {
		if c.Action == "StopInstances" {
			stops = append(stops, c.Params.Get("Force"))
		}
	}
	if len(stops) != 2 || stops[0] == "true" || stops[1] != "true" {
		t.Fatalf("StopInstances calls (Force): %q, want a plain one and then a forced one", stops)
	}
	if st := r.fake.InstanceState("i-n1"); st != "stopped" {
		t.Fatalf("n1 is %s", st)
	}
}

func TestFenceFailsWhenThePeerNeverStops(t *testing.T) {
	r := newRig(t, func(f *awsfake.Server) {
		f.AddInstance(awsfake.Instance{ID: "i-n1", PrivateIP: "10.77.0.10", StopPolls: 1 << 20})
	})
	r.p.StopTimeout = 30 * time.Millisecond
	err := r.p.Fence(context.Background(), r.req)
	if err == nil || !strings.Contains(err.Error(), "not stopped after a forced stop") {
		t.Fatalf("error: %v", err)
	}
	// The caller promotes nothing, so nothing here may have moved the address.
	if got := r.fake.AddressOf("eipalloc-svc").InstanceID; got != "i-n1" {
		t.Fatalf("address: %s", got)
	}
}

func TestFenceNeedsTheStopToBePermitted(t *testing.T) {
	r := newRig(t, func(f *awsfake.Server) { f.Deny("ec2", "StopInstances") })
	err := r.p.Fence(context.Background(), r.req)
	if err == nil || !strings.Contains(err.Error(), "StopInstances") {
		t.Fatalf("error: %v", err)
	}
	if st := r.fake.InstanceState("i-n1"); st != "running" {
		t.Fatalf("n1 is %s", st)
	}
}

func TestFenceOfAStoppedPeerStopsNothing(t *testing.T) {
	r := newRig(t, func(f *awsfake.Server) {
		f.AddInstance(awsfake.Instance{ID: "i-n1", State: "stopped", PrivateIP: "10.77.0.10"})
	})
	if err := r.p.Fence(context.Background(), r.req); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(r.fake.Order("ec2"), "ec2:StopInstances") {
		t.Fatal("a stopped instance was stopped again")
	}
}

func TestFenceOfAPeerEC2DoesNotListFailsClosed(t *testing.T) {
	r := newRig(t)
	r.req.Old.Provider.AWS.InstanceID = "i-gone"
	if err := r.p.Fence(context.Background(), r.req); err == nil {
		t.Fatal("an instance EC2 does not know was taken for stopped")
	}
	r.req.Old.Provider.AWS = nil
	if err := r.p.Fence(context.Background(), r.req); err == nil || !strings.Contains(err.Error(), "no EC2 instance id") {
		t.Fatalf("a node with no identity: %v", err)
	}
}

func TestAPlannedSwitchoverHasNothingToFence(t *testing.T) {
	r := newRig(t)
	r.req.Planned = true
	if err := r.p.Fence(context.Background(), r.req); err != nil {
		t.Fatal(err)
	}
	if len(r.fake.Order("ec2")) != 0 {
		t.Fatalf("a planned switchover called EC2: %v", r.fake.Order("ec2"))
	}
	// The address still moves.
	if err := r.p.TakeOver(context.Background(), r.req); err != nil {
		t.Fatal(err)
	}
	if got := r.fake.AddressOf("eipalloc-svc").InstanceID; got != "i-n2" {
		t.Fatalf("address: %s", got)
	}
}

func TestTakeOverUsesThePrimaryAddressWhenThereIsNoElasticIPOfItsOwn(t *testing.T) {
	r := newRigOn(t, false, func(f *awsfake.Server) {
		f.AddInstance(awsfake.Instance{ID: "i-n2", PrivateIP: "10.77.0.20", PublicIP: "198.51.100.7"})
	})
	if err := r.p.TakeOver(context.Background(), r.req); err != nil {
		t.Fatal(err)
	}
	if a := r.fake.AddressOf("eipalloc-svc"); a.InstanceID != "i-n2" || a.PrivateIP != "10.77.0.20" {
		t.Fatalf("service address: %+v", a)
	}
	for _, c := range r.fake.Calls() {
		if c.Action == "AssociateAddress" && c.Params.Get("PrivateIpAddress") != "" {
			t.Fatalf("a private address was named although the primary one is free: %v", c.Params)
		}
	}
}

// A survivor whose only private address carries an Elastic IP of its own keeps it: the association
// would take it away, so nothing is associated and the operator is told (ErrNoTakeover).
func TestTakeOverLeavesTheSurvivorsOwnElasticIPAloneWhenItHasNoSecondaryAddress(t *testing.T) {
	r := newRig(t, func(f *awsfake.Server) { f.AddInstance(awsfake.Instance{ID: "i-n2", PrivateIP: "10.77.0.20"}) })
	err := r.p.TakeOver(context.Background(), r.req)
	if !errors.Is(err, failover.ErrNoTakeover) || !strings.Contains(err.Error(), "secondary") {
		t.Fatalf("error: %v", err)
	}
	if a := r.fake.AddressOf("eipalloc-svc"); a.InstanceID != "i-n1" {
		t.Fatalf("service address: %+v", a)
	}
	if own := r.fake.AddressOf("eipalloc-n2"); own.InstanceID != "i-n2" || own.PrivateIP != "10.77.0.20" {
		t.Fatalf("n2's own address: %+v", own)
	}
	for _, c := range r.fake.Calls() {
		if c.Action == "AssociateAddress" {
			t.Fatalf("an address was associated: %v", c.Params)
		}
	}
}

func TestTakeOverIsIdempotent(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := r.p.TakeOver(ctx, r.req); err != nil {
			t.Fatal(err)
		}
	}
	n := 0
	for _, c := range r.fake.Calls() {
		if c.Action == "AssociateAddress" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("AssociateAddress ran %d times", n)
	}
}

func TestTakeOverCasesThatLeaveTheAddressToTheOperator(t *testing.T) {
	r := newRig(t)
	r.req.ServiceAddress.AllocationID = ""
	if err := r.p.TakeOver(context.Background(), r.req); !errors.Is(err, failover.ErrNoServiceAddress) || !errors.Is(err, failover.ErrNoTakeover) {
		t.Fatalf("no allocation: %v", err)
	}
	r = newRig(t)
	r.req.Old.Provider.AWS.Region = "eu-west-1"
	if err := r.p.TakeOver(context.Background(), r.req); !errors.Is(err, failover.ErrCrossRegion) {
		t.Fatalf("another region: %v", err)
	}
	if slices.Contains(r.fake.Order("ec2"), "ec2:AssociateAddress") {
		t.Fatal("an address of another region was associated")
	}
}

func TestProbeUsesDryRunOnly(t *testing.T) {
	r := newRig(t)
	if err := r.p.Probe(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, c := range r.fake.Calls() {
		if c.Service == "ec2" && !c.DryRun {
			t.Fatalf("Probe made a real call: %s", c.Action)
		}
	}
	want := []string{"ec2:DescribeInstances(dryrun)", "ec2:DescribeInstanceStatus(dryrun)", "ec2:StopInstances(dryrun)", "ec2:DescribeAddresses(dryrun)", "ec2:AssociateAddress(dryrun)"}
	if got := r.fake.Order("ec2"); !subsequence(got, want) {
		t.Fatalf("probe calls: %v", got)
	}
	if st := r.fake.InstanceState("i-n1"); st != "running" {
		t.Fatalf("the probe stopped n1: %s", st)
	}
	if got := r.fake.AddressOf("eipalloc-svc").InstanceID; got != "i-n1" {
		t.Fatalf("the probe moved the address: %s", got)
	}
}

func TestProbeSaysWhichPermissionIsMissing(t *testing.T) {
	for _, action := range []string{"StopInstances", "AssociateAddress", "DescribeInstances"} {
		r := newRig(t, func(f *awsfake.Server) { f.Deny("ec2", action) })
		err := r.p.Probe(context.Background())
		if err == nil || !strings.Contains(err.Error(), "ec2:"+action) || !strings.Contains(err.Error(), "upgrade --aws") {
			t.Errorf("%s denied: %v", action, err)
		}
	}
}

func TestProbeNeedsAPeerAndAServiceAddress(t *testing.T) {
	r := newRig(t)
	cluster := r.p.Cluster
	r.p.Cluster = func(ctx context.Context) (Cluster, error) {
		c, _ := cluster(ctx)
		c.Peers = []registry.Node{c.Self}
		return c, nil
	}
	if err := r.p.Probe(context.Background()); err == nil || !strings.Contains(err.Error(), "no peer") {
		t.Fatalf("no peer: %v", err)
	}
	r.p.Cluster = func(ctx context.Context) (Cluster, error) {
		c, _ := cluster(ctx)
		c.Service = registry.ServiceAddress{}
		return c, nil
	}
	if err := r.p.Probe(context.Background()); err == nil || !strings.Contains(err.Error(), "allocation_id") {
		t.Fatalf("no service address: %v", err)
	}
}

func TestPeerState(t *testing.T) {
	r := newRig(t, func(f *awsfake.Server) {
		f.AddInstance(awsfake.Instance{ID: "i-n1", PrivateIP: "10.77.0.10"})
		f.AddInstance(awsfake.Instance{ID: "i-n3", PrivateIP: "10.77.0.30", Impaired: true})
		f.AddInstance(awsfake.Instance{ID: "i-n4", PrivateIP: "10.77.0.40", State: "stopped"})
	})
	ctx := context.Background()
	state := func(id string) failover.PeerState {
		n := r.req.Old
		n.Provider = registry.NodeProvider{AWS: &registry.NodeAWS{InstanceID: id, Region: "us-east-1"}}
		s, err := r.p.PeerState(ctx, n)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	if s := state("i-n1"); s.State != "running" || s.Impaired || s.Down() {
		t.Errorf("a healthy peer: %+v", s)
	}
	if s := state("i-n3"); s.State != "running" || !s.Impaired || !s.Down() {
		t.Errorf("an impaired peer: %+v", s)
	}
	if s := state("i-n4"); s.State != "stopped" || !s.Down() {
		t.Errorf("a stopped peer: %+v", s)
	}
}
