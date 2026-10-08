package awsapi

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	ec2Version  = "2016-11-15"
	maxPages    = 100
	contentForm = "application/x-www-form-urlencoded; charset=utf-8"
)

// EC2 is the part of the EC2 Query API that fencing and peer discovery use. Every method that
// takes DryRun asks EC2 only whether the call would be allowed: it returns nil when the answer
// is DryRunOperation, an *Error with Code UnauthorizedOperation when the credentials lack the
// permission (IsAccessDenied), and any other error as it is. A DryRun call returns no results.
type EC2 struct{ c *core }

// Filter limits a describe call: the resource must have one of Values for Name.
type Filter struct {
	Name   string
	Values []string
}

// Instance is the part of an EC2 instance that Supavise reads. State is the lower-case name EC2
// uses: pending, running, stopping, stopped, shutting-down or terminated.
type Instance struct {
	ID                string
	State             string
	PrivateIP         string // the primary private IP
	PublicIP          string // the public IP of the primary private IP; "" when it has none
	AvailabilityZone  string
	SubnetID          string
	VPCID             string
	NetworkInterfaces []NetworkInterface
	Tags              map[string]string
}

// NetworkInterface is one network interface of an instance.
type NetworkInterface struct {
	ID          string
	DeviceIndex int
	PrivateIPs  []PrivateIP
}

// PrivateIP is one private address of a network interface and the public address, if any, that
// maps to it.
type PrivateIP struct {
	Address  string
	Primary  bool
	PublicIP string
}

// DescribeInstancesInput selects instances. With neither IDs nor Filters it lists them all.
type DescribeInstancesInput struct {
	InstanceIDs []string
	Filters     []Filter
	DryRun      bool
}

// DescribeInstances returns the matching instances, following the result pages.
func (e *EC2) DescribeInstances(ctx context.Context, in DescribeInstancesInput) ([]Instance, error) {
	var out []Instance
	token := ""
	for page := 0; page < maxPages; page++ {
		p := params{}
		p.list("InstanceId", in.InstanceIDs)
		p.filters(in.Filters)
		p.set("NextToken", token)
		var resp struct {
			Reservations []struct {
				Instances []xmlInstance `xml:"instancesSet>item"`
			} `xml:"reservationSet>item"`
			NextToken string `xml:"nextToken"`
		}
		if err := e.call(ctx, "DescribeInstances", p, in.DryRun, &resp); err != nil || in.DryRun {
			return nil, err
		}
		for _, r := range resp.Reservations {
			for _, i := range r.Instances {
				out = append(out, i.instance())
			}
		}
		if resp.NextToken == "" {
			return out, nil
		}
		token = resp.NextToken
	}
	return nil, fmt.Errorf("aws ec2 DescribeInstances: more than %d result pages", maxPages)
}

type xmlTag struct {
	Key   string `xml:"key"`
	Value string `xml:"value"`
}

func tagMap(ts []xmlTag) map[string]string {
	m := make(map[string]string, len(ts))
	for _, t := range ts {
		m[t.Key] = t.Value
	}
	return m
}

type xmlInstance struct {
	ID    string `xml:"instanceId"`
	State struct {
		Name string `xml:"name"`
	} `xml:"instanceState"`
	PrivateIP string `xml:"privateIpAddress"`
	PublicIP  string `xml:"ipAddress"`
	Placement struct {
		AZ string `xml:"availabilityZone"`
	} `xml:"placement"`
	SubnetID string `xml:"subnetId"`
	VPCID    string `xml:"vpcId"`
	ENIs     []struct {
		ID         string `xml:"networkInterfaceId"`
		Attachment struct {
			DeviceIndex int `xml:"deviceIndex"`
		} `xml:"attachment"`
		IPs []struct {
			Address     string `xml:"privateIpAddress"`
			Primary     bool   `xml:"primary"`
			Association struct {
				PublicIP string `xml:"publicIp"`
			} `xml:"association"`
		} `xml:"privateIpAddressesSet>item"`
	} `xml:"networkInterfaceSet>item"`
	Tags []xmlTag `xml:"tagSet>item"`
}

func (x xmlInstance) instance() Instance {
	i := Instance{
		ID: x.ID, State: x.State.Name, PrivateIP: x.PrivateIP, PublicIP: x.PublicIP,
		AvailabilityZone: x.Placement.AZ, SubnetID: x.SubnetID, VPCID: x.VPCID, Tags: tagMap(x.Tags),
	}
	for _, n := range x.ENIs {
		ni := NetworkInterface{ID: n.ID, DeviceIndex: n.Attachment.DeviceIndex}
		for _, ip := range n.IPs {
			ni.PrivateIPs = append(ni.PrivateIPs, PrivateIP{Address: ip.Address, Primary: ip.Primary, PublicIP: ip.Association.PublicIP})
		}
		i.NetworkInterfaces = append(i.NetworkInterfaces, ni)
	}
	return i
}

// InstanceStatus is the health EC2 reports for an instance. System and Instance are "ok",
// "impaired", "insufficient-data", "not-applicable" or "initializing".
type InstanceStatus struct {
	ID               string
	State            string
	AvailabilityZone string
	System           string
	Instance         string
	Events           []StatusEvent
}

// StatusEvent is a scheduled event, such as instance-retirement or instance-stop.
type StatusEvent struct {
	Code        string
	Description string
	NotBefore   time.Time
	NotAfter    time.Time
}

// DescribeInstanceStatusInput selects instances. EC2 reports only running instances unless
// IncludeAll is set.
type DescribeInstanceStatusInput struct {
	InstanceIDs []string
	Filters     []Filter
	IncludeAll  bool
	DryRun      bool
}

// DescribeInstanceStatus returns the status of the matching instances, following the result pages.
func (e *EC2) DescribeInstanceStatus(ctx context.Context, in DescribeInstanceStatusInput) ([]InstanceStatus, error) {
	var out []InstanceStatus
	token := ""
	for page := 0; page < maxPages; page++ {
		p := params{}
		p.list("InstanceId", in.InstanceIDs)
		p.filters(in.Filters)
		if in.IncludeAll {
			p.set("IncludeAllInstances", "true")
		}
		p.set("NextToken", token)
		var resp struct {
			Statuses []struct {
				ID    string `xml:"instanceId"`
				AZ    string `xml:"availabilityZone"`
				State struct {
					Name string `xml:"name"`
				} `xml:"instanceState"`
				System struct {
					Status string `xml:"status"`
				} `xml:"systemStatus"`
				Instance struct {
					Status string `xml:"status"`
				} `xml:"instanceStatus"`
				Events []struct {
					Code        string `xml:"code"`
					Description string `xml:"description"`
					NotBefore   string `xml:"notBefore"`
					NotAfter    string `xml:"notAfter"`
				} `xml:"eventsSet>item"`
			} `xml:"instanceStatusSet>item"`
			NextToken string `xml:"nextToken"`
		}
		if err := e.call(ctx, "DescribeInstanceStatus", p, in.DryRun, &resp); err != nil || in.DryRun {
			return nil, err
		}
		for _, s := range resp.Statuses {
			st := InstanceStatus{ID: s.ID, State: s.State.Name, AvailabilityZone: s.AZ, System: s.System.Status, Instance: s.Instance.Status}
			for _, ev := range s.Events {
				st.Events = append(st.Events, StatusEvent{Code: ev.Code, Description: ev.Description, NotBefore: parseTime(ev.NotBefore), NotAfter: parseTime(ev.NotAfter)})
			}
			out = append(out, st)
		}
		if resp.NextToken == "" {
			return out, nil
		}
		token = resp.NextToken
	}
	return nil, fmt.Errorf("aws ec2 DescribeInstanceStatus: more than %d result pages", maxPages)
}

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

// StopInstancesInput names the instances to stop. Force stops an instance that does not shut down
// cleanly, without flushing its file system caches or metadata.
type StopInstancesInput struct {
	InstanceIDs []string
	Force       bool
	DryRun      bool
}

// StateChange is the state of an instance before and after a call.
type StateChange struct {
	InstanceID string
	Previous   string
	Current    string
}

// StopInstances asks EC2 to stop the instances and returns the state each one moved to. It does
// not wait: poll DescribeInstances until State is "stopped".
func (e *EC2) StopInstances(ctx context.Context, in StopInstancesInput) ([]StateChange, error) {
	p := params{}
	p.list("InstanceId", in.InstanceIDs)
	if in.Force {
		p.set("Force", "true")
	}
	var resp struct {
		Items []struct {
			ID      string `xml:"instanceId"`
			Current struct {
				Name string `xml:"name"`
			} `xml:"currentState"`
			Previous struct {
				Name string `xml:"name"`
			} `xml:"previousState"`
		} `xml:"instancesSet>item"`
	}
	if err := e.call(ctx, "StopInstances", p, in.DryRun, &resp); err != nil || in.DryRun {
		return nil, err
	}
	out := make([]StateChange, len(resp.Items))
	for i, it := range resp.Items {
		out[i] = StateChange{InstanceID: it.ID, Previous: it.Previous.Name, Current: it.Current.Name}
	}
	return out, nil
}

// AssociateAddressInput moves an Elastic IP. Give AllocationID and either InstanceID (which uses
// the instance's primary interface) or NetworkInterfaceID. PrivateIP picks one of that interface's
// private addresses; without it the primary one is used, and an Elastic IP already on it is
// released from it (it stays allocated). With AllowReassociation false the call fails when the
// Elastic IP is already associated; with true it moves it.
type AssociateAddressInput struct {
	AllocationID       string
	InstanceID         string
	NetworkInterfaceID string
	PrivateIP          string
	AllowReassociation bool
	DryRun             bool
}

// AssociateAddress returns the id of the new association.
func (e *EC2) AssociateAddress(ctx context.Context, in AssociateAddressInput) (associationID string, err error) {
	p := params{}
	p.set("AllocationId", in.AllocationID)
	p.set("InstanceId", in.InstanceID)
	p.set("NetworkInterfaceId", in.NetworkInterfaceID)
	p.set("PrivateIpAddress", in.PrivateIP)
	p.set("AllowReassociation", strconv.FormatBool(in.AllowReassociation))
	var resp struct {
		AssociationID string `xml:"associationId"`
	}
	if err := e.call(ctx, "AssociateAddress", p, in.DryRun, &resp); err != nil || in.DryRun {
		return "", err
	}
	return resp.AssociationID, nil
}

// DisassociateAddressInput names an association, as DescribeAddresses and AssociateAddress give it.
type DisassociateAddressInput struct {
	AssociationID string
	DryRun        bool
}

// DisassociateAddress releases an Elastic IP from what it is associated with. It stays allocated.
func (e *EC2) DisassociateAddress(ctx context.Context, in DisassociateAddressInput) error {
	p := params{}
	p.set("AssociationId", in.AssociationID)
	return e.call(ctx, "DisassociateAddress", p, in.DryRun, nil)
}

// Address is an Elastic IP. The association fields are empty while it is not associated.
type Address struct {
	AllocationID       string
	PublicIP           string
	AssociationID      string
	InstanceID         string
	NetworkInterfaceID string
	PrivateIP          string // the private address of the interface it maps to
	Tags               map[string]string
}

// DescribeAddressesInput selects Elastic IPs. With neither IDs nor Filters it lists them all.
type DescribeAddressesInput struct {
	AllocationIDs []string
	PublicIPs     []string
	Filters       []Filter
	DryRun        bool
}

// DescribeAddresses returns the matching Elastic IPs.
func (e *EC2) DescribeAddresses(ctx context.Context, in DescribeAddressesInput) ([]Address, error) {
	p := params{}
	p.list("AllocationId", in.AllocationIDs)
	p.list("PublicIp", in.PublicIPs)
	p.filters(in.Filters)
	var resp struct {
		Items []struct {
			AllocationID       string   `xml:"allocationId"`
			PublicIP           string   `xml:"publicIp"`
			AssociationID      string   `xml:"associationId"`
			InstanceID         string   `xml:"instanceId"`
			NetworkInterfaceID string   `xml:"networkInterfaceId"`
			PrivateIP          string   `xml:"privateIpAddress"`
			Tags               []xmlTag `xml:"tagSet>item"`
		} `xml:"addressesSet>item"`
	}
	if err := e.call(ctx, "DescribeAddresses", p, in.DryRun, &resp); err != nil || in.DryRun {
		return nil, err
	}
	out := make([]Address, len(resp.Items))
	for i, it := range resp.Items {
		out[i] = Address{
			AllocationID: it.AllocationID, PublicIP: it.PublicIP, AssociationID: it.AssociationID,
			InstanceID: it.InstanceID, NetworkInterfaceID: it.NetworkInterfaceID, PrivateIP: it.PrivateIP, Tags: tagMap(it.Tags),
		}
	}
	return out, nil
}

// call sends one Query API action and decodes the XML answer into out (nil to ignore it). For a
// DryRun it maps EC2's answer: DryRunOperation is success, anything else is returned as it is.
func (e *EC2) call(ctx context.Context, action string, p params, dryRun bool, out any) error {
	p.set("Action", action)
	p.set("Version", ec2Version)
	if dryRun {
		p.set("DryRun", "true")
	}
	body, err := e.c.do(ctx, apiCall{
		service: "ec2", host: "ec2", action: action,
		header:   http.Header{"Content-Type": {contentForm}},
		body:     p.encode(),
		parseErr: func(status int, _ http.Header, b []byte) *Error { return parseXMLError(status, b) },
	})
	if dryRun {
		if IsCode(err, "DryRunOperation") {
			return nil
		}
		return err
	}
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := xml.Unmarshal(body, out); err != nil {
		return fmt.Errorf("aws ec2 %s: unreadable response: %w", action, err)
	}
	return nil
}

// params are the form fields of a Query API request.
type params map[string][]string

// set adds a single-valued field; an empty value leaves the field out.
func (p params) set(k, v string) {
	if v != "" {
		p[k] = []string{v}
	}
}

// list adds a list field as k.1, k.2, ...
func (p params) list(k string, vs []string) {
	for i, v := range vs {
		p.set(k+"."+strconv.Itoa(i+1), v)
	}
}

// filters adds Filter.N.Name and Filter.N.Value.M.
func (p params) filters(fs []Filter) {
	for i, f := range fs {
		n := "Filter." + strconv.Itoa(i+1)
		p.set(n+".Name", f.Name)
		p.list(n+".Value", f.Values)
	}
}

// encode is the form body: fields sorted by name, spaces as %20.
func (p params) encode() []byte {
	return []byte(strings.ReplaceAll(url.Values(p).Encode(), "+", "%20"))
}
