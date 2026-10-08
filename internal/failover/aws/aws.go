// Package aws is the failover provider for nodes on EC2: it stops the old primary's instance so that
// it cannot write, moves the cluster's Elastic IP to the survivor, and says what the cloud knows
// about a peer (design 2.10.6). It speaks the EC2 Query API through internal/awsapi with the
// instance role; the role's permissions are tag-scoped to the cluster (supavise:cluster), and
// Probe asks EC2 with DryRun whether they are in place.
//
// The order that keeps one writer, on the survivor and before anything is promoted:
//
//  1. DescribeInstances and DescribeInstanceStatus for the peer.
//  2. StopInstances; poll DescribeInstances until the peer is "stopped" (as long as the stop
//     timeout), then StopInstances again with Force and poll once more. A peer that is not stopped
//     after that is an error, and the caller promotes nothing.
//  3. AssociateAddress with reassociation allowed, to the right private IP of the survivor.
//
// A peer that merely missed pings is not stopped by this package. The caller decides whether to
// fence (the automatic mode checks EC2's view of the peer first, through PeerState).
package aws

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/supavise/supavise/internal/awsapi"
	"github.com/supavise/supavise/internal/failover"
	"github.com/supavise/supavise/internal/registry"
)

// EC2 is the part of awsapi.EC2 the provider uses; *awsapi.EC2 implements it.
type EC2 interface {
	DescribeInstances(ctx context.Context, in awsapi.DescribeInstancesInput) ([]awsapi.Instance, error)
	DescribeInstanceStatus(ctx context.Context, in awsapi.DescribeInstanceStatusInput) ([]awsapi.InstanceStatus, error)
	StopInstances(ctx context.Context, in awsapi.StopInstancesInput) ([]awsapi.StateChange, error)
	AssociateAddress(ctx context.Context, in awsapi.AssociateAddressInput) (string, error)
	DescribeAddresses(ctx context.Context, in awsapi.DescribeAddressesInput) ([]awsapi.Address, error)
}

var _ EC2 = (*awsapi.EC2)(nil)

// Cluster is what the provider reads from the registry when it is asked without a move: the node
// it runs on, the other nodes, and the service address.
type Cluster struct {
	Self    registry.Node
	Peers   []registry.Node
	Service registry.ServiceAddress
}

// Provider fences and takes over on EC2. It implements failover.Provider and failover.Cloud.
type Provider struct {
	// EC2 returns the client for a region; an empty region is the node's own. An EC2 address and an
	// instance belong to one region, so a peer in another region needs its own client.
	EC2 func(region string) (EC2, error)
	// Cluster is read by Probe and PeerState.
	Cluster func(ctx context.Context) (Cluster, error)
	// StopTimeout is how long to wait for the peer to reach "stopped", once without Force and once
	// with it. Zero is 2 minutes.
	StopTimeout time.Duration
	// Poll is the pause between two looks at the peer. Zero is 2 seconds.
	Poll time.Duration
}

var (
	_ failover.Provider      = (*Provider)(nil)
	_ failover.Cloud         = (*Provider)(nil)
	_ failover.AddressProber = (*Provider)(nil)
)

func (p *Provider) Name() string { return "aws" }

func (p *Provider) stopTimeout() time.Duration {
	if p.StopTimeout <= 0 {
		return 2 * time.Minute
	}
	return p.StopTimeout
}

func (p *Provider) poll() time.Duration {
	if p.Poll <= 0 {
		return 2 * time.Second
	}
	return p.Poll
}

func (p *Provider) client(region string) (EC2, error) {
	c, err := p.EC2(region)
	if err != nil {
		return nil, fmt.Errorf("aws: client for %q: %w", region, err)
	}
	return c, nil
}

// identity returns the EC2 identity the registry holds for a node.
func identity(n registry.Node) (*registry.NodeAWS, error) {
	a := n.Provider.AWS
	if a == nil || a.InstanceID == "" {
		return nil, fmt.Errorf("aws: node %s has no EC2 instance id in the registry", n.Name)
	}
	return a, nil
}

// Fence stops the old primary's instance and returns when EC2 says it is stopped. A planned
// switchover has stopped it cleanly already; Fence then has nothing to do.
func (p *Provider) Fence(ctx context.Context, req failover.Request) error {
	if req.Planned {
		return nil
	}
	old, err := identity(req.Old)
	if err != nil {
		return err
	}
	c, err := p.client(old.Region)
	if err != nil {
		return err
	}
	inst, err := describe(ctx, c, old.InstanceID)
	if err != nil {
		return err
	}
	// The status checks do not decide anything here; they are what the error says when the stop fails.
	checks := "status checks unknown"
	if st, err := status(ctx, c, old.InstanceID); err == nil && st != nil {
		checks = fmt.Sprintf("status checks: system %s, instance %s", st.System, st.Instance)
	}
	if gone(inst.State) {
		return nil // already stopped: it cannot write
	}
	if _, err := c.StopInstances(ctx, awsapi.StopInstancesInput{InstanceIDs: []string{old.InstanceID}}); err != nil && !awsapi.IsCode(err, "IncorrectInstanceState") {
		return fmt.Errorf("aws: StopInstances %s: %w", old.InstanceID, err)
	}
	if p.waitStopped(ctx, c, old.InstanceID) == nil {
		return nil
	}
	// A guest that does not shut down: the second request is the forced stop.
	if _, err := c.StopInstances(ctx, awsapi.StopInstancesInput{InstanceIDs: []string{old.InstanceID}, Force: true}); err != nil && !awsapi.IsCode(err, "IncorrectInstanceState") {
		return fmt.Errorf("aws: StopInstances --force %s: %w", old.InstanceID, err)
	}
	if err := p.waitStopped(ctx, c, old.InstanceID); err != nil {
		return fmt.Errorf("aws: %s is not stopped after a forced stop: %w (%s)", old.InstanceID, err, checks)
	}
	return nil
}

// gone reports whether an instance in this state cannot write.
func gone(state string) bool { return state == "stopped" || state == "terminated" }

// waitStopped polls until the instance is stopped or terminated, for the stop timeout.
func (p *Provider) waitStopped(ctx context.Context, c EC2, id string) error {
	deadline := time.Now().Add(p.stopTimeout())
	for {
		inst, err := describe(ctx, c, id)
		if err != nil {
			return err
		}
		if gone(inst.State) {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("still %s after %s", inst.State, p.stopTimeout())
		}
		t := time.NewTimer(p.poll())
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

func describe(ctx context.Context, c EC2, id string) (awsapi.Instance, error) {
	is, err := c.DescribeInstances(ctx, awsapi.DescribeInstancesInput{InstanceIDs: []string{id}})
	if err != nil {
		return awsapi.Instance{}, fmt.Errorf("aws: DescribeInstances %s: %w", id, err)
	}
	if len(is) != 1 {
		// Fail closed: an instance that EC2 does not list may be another one than the node runs on.
		return awsapi.Instance{}, fmt.Errorf("aws: DescribeInstances %s returned %d instances", id, len(is))
	}
	return is[0], nil
}

func status(ctx context.Context, c EC2, id string) (*awsapi.InstanceStatus, error) {
	ss, err := c.DescribeInstanceStatus(ctx, awsapi.DescribeInstanceStatusInput{InstanceIDs: []string{id}, IncludeAll: true})
	if err != nil {
		return nil, fmt.Errorf("aws: DescribeInstanceStatus %s: %w", id, err)
	}
	if len(ss) == 0 {
		return nil, nil
	}
	return &ss[0], nil
}

// TakeOver associates the cluster's Elastic IP with the survivor. The private address it maps to
// is the survivor's own secondary address when its primary address carries an Elastic IP of its own
// (a stack from the template that creates one), so that the association replaces nothing; else
// it is the primary address, and a public address the survivor had there is replaced. A survivor
// whose only private address carries an Elastic IP of its own is not touched: ErrNoTakeover, and
// the operator moves the address.
func (p *Provider) TakeOver(ctx context.Context, req failover.Request) error {
	alloc := req.ServiceAddress.AllocationID
	if alloc == "" {
		return failover.ErrNoServiceAddress
	}
	newAWS, err := identity(req.New)
	if err != nil {
		return err
	}
	if o := req.Old.Provider.AWS; o != nil && o.Region != "" && newAWS.Region != "" && o.Region != newAWS.Region {
		return failover.ErrCrossRegion
	}
	c, err := p.client(newAWS.Region)
	if err != nil {
		return err
	}
	inst, err := describe(ctx, c, newAWS.InstanceID)
	if err != nil {
		return err
	}
	own, err := c.DescribeAddresses(ctx, awsapi.DescribeAddressesInput{Filters: []awsapi.Filter{{Name: "instance-id", Values: []string{newAWS.InstanceID}}}})
	if err != nil {
		return fmt.Errorf("aws: DescribeAddresses for %s: %w", newAWS.InstanceID, err)
	}
	target, err := takeoverIP(inst, own, alloc)
	if err != nil {
		return err
	}

	svc, err := c.DescribeAddresses(ctx, awsapi.DescribeAddressesInput{AllocationIDs: []string{alloc}})
	if err != nil {
		return fmt.Errorf("aws: DescribeAddresses %s: %w", alloc, err)
	}
	if len(svc) != 1 {
		return fmt.Errorf("aws: the service address %s does not exist in this region", alloc)
	}
	if svc[0].InstanceID == newAWS.InstanceID && (target == "" || svc[0].PrivateIP == target) {
		return nil // an earlier try of this move got here
	}
	if _, err := c.AssociateAddress(ctx, awsapi.AssociateAddressInput{
		AllocationID: alloc, InstanceID: newAWS.InstanceID, PrivateIP: target, AllowReassociation: true,
	}); err != nil {
		return fmt.Errorf("aws: AssociateAddress %s to %s: %w", alloc, newAWS.InstanceID, err)
	}
	after, err := c.DescribeAddresses(ctx, awsapi.DescribeAddressesInput{AllocationIDs: []string{alloc}})
	if err != nil {
		return fmt.Errorf("aws: DescribeAddresses %s after the association: %w", alloc, err)
	}
	if len(after) != 1 || after[0].InstanceID != newAWS.InstanceID {
		return fmt.Errorf("aws: %s is not associated with %s after AssociateAddress", alloc, newAWS.InstanceID)
	}
	return nil
}

// takeoverIP picks the private address of the survivor that the service address maps to: its
// secondary address when its primary one has an Elastic IP of its own, else "" (the primary one).
// When every private address has an Elastic IP of its own, associating the service address would
// take one of them from the survivor (its peers and its operator may reach it there): that is
// ErrNoTakeover. own are the Elastic IPs associated with the survivor now; the service address
// itself does not count.
func takeoverIP(inst awsapi.Instance, own []awsapi.Address, serviceAlloc string) (string, error) {
	taken := map[string]bool{} // private addresses with an Elastic IP other than the service address
	for _, a := range own {
		if a.AllocationID != serviceAlloc && a.PrivateIP != "" {
			taken[a.PrivateIP] = true
		}
	}
	var primary string
	var secondary []string
	for _, ni := range inst.NetworkInterfaces {
		if ni.DeviceIndex != 0 {
			continue
		}
		for _, ip := range ni.PrivateIPs {
			if ip.Primary {
				primary = ip.Address
			} else {
				secondary = append(secondary, ip.Address)
			}
		}
	}
	if primary == "" {
		primary = inst.PrivateIP
	}
	if !taken[primary] {
		return "", nil
	}
	for _, s := range secondary {
		if !taken[s] {
			return s, nil
		}
	}
	return "", fmt.Errorf("%w: %s's private address %s has an Elastic IP of its own and it has no free secondary address, so the service address would replace it; add a secondary private address to its network interface or move the service address by hand", failover.ErrNoTakeover, inst.ID, primary)
}

// Probe asks EC2, with DryRun and no side effect, whether the instance role may do what a
// failover does: describe, stop the peers, associate the service address. It answers nil when every call
// is allowed (DryRunOperation).
func (p *Provider) Probe(ctx context.Context) error {
	cl, err := p.Cluster(ctx)
	if err != nil {
		return fmt.Errorf("aws: reading the cluster: %w", err)
	}
	self, err := identity(cl.Self)
	if err != nil {
		return err
	}
	if cl.Service.AllocationID == "" {
		return errors.New("aws: the cluster has no service address to take over (cluster.service_address has no allocation_id)")
	}
	peers := 0
	byRegion := map[string][]string{}
	for _, n := range cl.Peers {
		if n.State == registry.NodeLeft || n.ID == cl.Self.ID {
			continue
		}
		a, err := identity(n)
		if err != nil {
			return err
		}
		byRegion[a.Region] = append(byRegion[a.Region], a.InstanceID)
		peers++
	}
	if peers == 0 {
		return errors.New("aws: there is no peer node whose instance could be stopped")
	}
	for region, ids := range byRegion {
		c, err := p.client(region)
		if err != nil {
			return err
		}
		if err := dry("DescribeInstances", func() error {
			_, err := c.DescribeInstances(ctx, awsapi.DescribeInstancesInput{InstanceIDs: ids, DryRun: true})
			return err
		}); err != nil {
			return err
		}
		if err := dry("DescribeInstanceStatus", func() error {
			_, err := c.DescribeInstanceStatus(ctx, awsapi.DescribeInstanceStatusInput{InstanceIDs: ids, DryRun: true})
			return err
		}); err != nil {
			return err
		}
		if err := dry("StopInstances", func() error {
			_, err := c.StopInstances(ctx, awsapi.StopInstancesInput{InstanceIDs: ids, DryRun: true})
			return err
		}); err != nil {
			return err
		}
	}
	c, err := p.client(self.Region)
	if err != nil {
		return err
	}
	if err := dry("DescribeAddresses", func() error {
		_, err := c.DescribeAddresses(ctx, awsapi.DescribeAddressesInput{AllocationIDs: []string{cl.Service.AllocationID}, DryRun: true})
		return err
	}); err != nil {
		return err
	}
	// The permission is asked for the call a failover makes: with the survivor's secondary private
	// address when that is where the service address goes, which the role's policy covers through the
	// network interface and not only the instance and the address. A survivor with no free address is
	// ProbeTakeover's to report; its dry run asks about the primary address.
	target, err := p.takeoverTarget(ctx, c, self, cl.Service.AllocationID)
	if err != nil && !errors.Is(err, failover.ErrNoTakeover) {
		return err
	}
	return dry("AssociateAddress", func() error {
		_, err := c.AssociateAddress(ctx, awsapi.AssociateAddressInput{
			AllocationID: cl.Service.AllocationID, InstanceID: self.InstanceID, PrivateIP: target, AllowReassociation: true, DryRun: true,
		})
		return err
	})
}

// takeoverTarget is the private address of the survivor that TakeOver maps the service address to,
// "" for its primary address; failover.ErrNoTakeover when it has no address to spare.
func (p *Provider) takeoverTarget(ctx context.Context, c EC2, self *registry.NodeAWS, alloc string) (string, error) {
	inst, err := describe(ctx, c, self.InstanceID)
	if err != nil {
		return "", err
	}
	own, err := c.DescribeAddresses(ctx, awsapi.DescribeAddressesInput{Filters: []awsapi.Filter{{Name: "instance-id", Values: []string{self.InstanceID}}}})
	if err != nil {
		return "", fmt.Errorf("aws: DescribeAddresses for %s: %w", self.InstanceID, err)
	}
	return takeoverIP(inst, own, alloc)
}

// ProbeTakeover asks whether this node can take the service address over by itself. A survivor whose
// only private address carries an Elastic IP of its own cannot, and a failover on it ends with the
// address left to the operator; the automatic mode, which has no operator to hand it to, stays off
// with this as its reason (failover.AddressProber).
func (p *Provider) ProbeTakeover(ctx context.Context) error {
	cl, err := p.Cluster(ctx)
	if err != nil {
		return fmt.Errorf("aws: reading the cluster: %w", err)
	}
	self, err := identity(cl.Self)
	if err != nil {
		return err
	}
	if cl.Service.AllocationID == "" {
		return failover.ErrNoServiceAddress
	}
	c, err := p.client(self.Region)
	if err != nil {
		return err
	}
	_, err = p.takeoverTarget(ctx, c, self, cl.Service.AllocationID)
	return err
}

// dry runs one DryRun call and words a refusal for the operator.
func dry(action string, call func() error) error {
	err := call()
	switch {
	case err == nil:
		return nil
	case awsapi.IsAccessDenied(err):
		return fmt.Errorf("the instance role may not call ec2:%s (UnauthorizedOperation): run `sudo -E supavise upgrade --aws` to bring the stack's permissions forward", action)
	}
	return fmt.Errorf("ec2:%s: %s", action, strings.TrimPrefix(err.Error(), "awsapi: "))
}

// PeerState says what EC2 reports about the node's instance: its state and whether its status
// checks fail. The automatic server mode acts only on a peer that EC2 says is down.
func (p *Provider) PeerState(ctx context.Context, n registry.Node) (failover.PeerState, error) {
	a, err := identity(n)
	if err != nil {
		return failover.PeerState{}, err
	}
	c, err := p.client(a.Region)
	if err != nil {
		return failover.PeerState{}, err
	}
	inst, err := describe(ctx, c, a.InstanceID)
	if err != nil {
		return failover.PeerState{}, err
	}
	ps := failover.PeerState{State: inst.State}
	st, err := status(ctx, c, a.InstanceID)
	if err != nil {
		return failover.PeerState{}, err
	}
	if st != nil {
		ps.Impaired = st.System == "impaired" || st.Instance == "impaired"
		ps.Detail = fmt.Sprintf("system %s, instance %s", st.System, st.Instance)
	}
	return ps, nil
}
