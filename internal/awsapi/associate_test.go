package awsapi_test

import (
	"reflect"
	"testing"

	"github.com/supavise/supavise/internal/awsapi"
	"github.com/supavise/supavise/internal/awsapi/awsfake"
)

// associateFake has this node with a secondary address, another node, and an Elastic IP that is
// free, or on the other node when held is set.
func associateFake(t *testing.T, held bool) *awsfake.Server {
	t.Helper()
	fake := awsfake.New(t)
	fake.AddInstance(awsfake.Instance{ID: "i-0self", PrivateIP: "10.0.0.10", SecondaryIPs: []string{"10.0.0.11"}})
	fake.AddInstance(awsfake.Instance{ID: "i-0other", PrivateIP: "10.0.1.10"})
	a := awsfake.Address{AllocationID: "eipalloc-svc", PublicIP: "203.0.113.50"}
	if held {
		a.InstanceID = "i-0other"
	}
	fake.AddAddress(a)
	return fake
}

var (
	onceThenLost = awsfake.Fault{Status: 503, Code: "RequestLimitExceeded", Message: "slow down", Times: 1, Applied: true}
	onceThenNone = awsfake.Fault{Status: 503, Code: "RequestLimitExceeded", Message: "slow down", Times: 1}
)

// The first send took effect and its answer was lost; the second meets Resource.AlreadyAssociated
// for its own work, and the call succeeds with the association that exists.
func TestAssociateAddressRetryAfterALostAnswerSucceeds(t *testing.T) {
	for name, in := range map[string]awsapi.AssociateAddressInput{
		"instance and private address":  {AllocationID: "eipalloc-svc", InstanceID: "i-0self", PrivateIP: "10.0.0.11"},
		"instance, primary address":     {AllocationID: "eipalloc-svc", InstanceID: "i-0self"},
		"interface":                     {AllocationID: "eipalloc-svc", NetworkInterfaceID: "eni-0self"},
		"interface and private address": {AllocationID: "eipalloc-svc", NetworkInterfaceID: "eni-0self", PrivateIP: "10.0.0.11"},
	} {
		t.Run(name, func(t *testing.T) {
			fake := associateFake(t, false)
			fake.Inject("ec2", "AssociateAddress", onceThenLost)
			id, err := fake.Client().EC2.AssociateAddress(ctx, in)
			now := fake.AddressOf("eipalloc-svc")
			if err != nil || id == "" || id != now.AssociationID || now.InstanceID != "i-0self" {
				t.Errorf("AssociateAddress = %q, %v; the address is now %+v", id, err, now)
			}
			want := []string{"ec2:AssociateAddress", "ec2:AssociateAddress", "ec2:DescribeAddresses"}
			if got := fake.Order(); !reflect.DeepEqual(got, want) {
				t.Errorf("order %v, want %v", got, want)
			}
			var calls []awsfake.Call
			for _, c := range fake.Calls() {
				if c.Service == "ec2" {
					calls = append(calls, c)
				}
			}
			if calls[0].Status != 503 || calls[1].Code != "Resource.AlreadyAssociated" {
				t.Errorf("answers %d %q then %d %q", calls[0].Status, calls[0].Code, calls[1].Status, calls[1].Code)
			}
		})
	}
}

// AlreadyAssociated stays an error when the Elastic IP is not on the target the call named, or
// when the first send already met it, or when the check cannot be made.
func TestAssociateAddressAlreadyAssociatedElsewhereStaysAnError(t *testing.T) {
	in := awsapi.AssociateAddressInput{AllocationID: "eipalloc-svc", InstanceID: "i-0self", PrivateIP: "10.0.0.11"}

	t.Run("held by another instance", func(t *testing.T) {
		fake := associateFake(t, true)
		fake.Inject("ec2", "AssociateAddress", onceThenNone)
		if _, err := fake.Client().EC2.AssociateAddress(ctx, in); !awsapi.IsCode(err, "Resource.AlreadyAssociated") {
			t.Errorf("err = %v", err)
		}
		if a := fake.AddressOf("eipalloc-svc"); a.InstanceID != "i-0other" {
			t.Errorf("the address moved to %+v", a)
		}
	})

	t.Run("on the instance, at another private address", func(t *testing.T) {
		fake := associateFake(t, false)
		fake.AddAddress(awsfake.Address{AllocationID: "eipalloc-own", PublicIP: "203.0.113.51", InstanceID: "i-0self"})
		fake.Inject("ec2", "AssociateAddress", onceThenNone)
		// eipalloc-own sits on the primary address; the call asked for the secondary one.
		own := in
		own.AllocationID = "eipalloc-own"
		if _, err := fake.Client().EC2.AssociateAddress(ctx, own); !awsapi.IsCode(err, "Resource.AlreadyAssociated") {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("on another interface", func(t *testing.T) {
		fake := associateFake(t, true)
		fake.Inject("ec2", "AssociateAddress", onceThenNone)
		byENI := awsapi.AssociateAddressInput{AllocationID: "eipalloc-svc", NetworkInterfaceID: "eni-0self"}
		if _, err := fake.Client().EC2.AssociateAddress(ctx, byENI); !awsapi.IsCode(err, "Resource.AlreadyAssociated") {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("the first send already met the error", func(t *testing.T) {
		fake := associateFake(t, false)
		fake.AddAddress(awsfake.Address{AllocationID: "eipalloc-here", PublicIP: "203.0.113.52", InstanceID: "i-0self", PrivateIP: "10.0.0.11"})
		here := in
		here.AllocationID = "eipalloc-here"
		if _, err := fake.Client().EC2.AssociateAddress(ctx, here); !awsapi.IsCode(err, "Resource.AlreadyAssociated") {
			t.Errorf("err = %v", err)
		}
		if got := fake.Order(); !reflect.DeepEqual(got, []string{"ec2:AssociateAddress"}) {
			t.Errorf("order %v: a first answer is final and needs no check", got)
		}
	})

	t.Run("the check fails", func(t *testing.T) {
		fake := associateFake(t, false)
		fake.Inject("ec2", "AssociateAddress", onceThenLost)
		fake.Deny("ec2", "DescribeAddresses")
		if _, err := fake.Client().EC2.AssociateAddress(ctx, in); !awsapi.IsCode(err, "Resource.AlreadyAssociated") {
			t.Errorf("err = %v (want the association error, not the check's)", err)
		}
	})
}

// With AllowReassociation a repeated send simply succeeds again; nothing needs checking.
func TestAssociateAddressRetryWithReassociationNeedsNoCheck(t *testing.T) {
	fake := associateFake(t, true)
	fake.Inject("ec2", "AssociateAddress", onceThenLost)
	id, err := fake.Client().EC2.AssociateAddress(ctx, awsapi.AssociateAddressInput{AllocationID: "eipalloc-svc", InstanceID: "i-0self", PrivateIP: "10.0.0.11", AllowReassociation: true})
	if a := fake.AddressOf("eipalloc-svc"); err != nil || id != a.AssociationID || a.InstanceID != "i-0self" {
		t.Errorf("%q, %v, address %+v", id, err, a)
	}
	if got := fake.Order(); !reflect.DeepEqual(got, []string{"ec2:AssociateAddress", "ec2:AssociateAddress"}) {
		t.Errorf("order %v", got)
	}
}
