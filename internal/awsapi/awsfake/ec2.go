package awsfake

import (
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// Instance is an EC2 instance in the fake. Only ID is required.
type Instance struct {
	ID    string
	State string // "running" when empty
	// PrivateIP is the primary private address, 10.77.0.<n> when empty. SecondaryIPs are the
	// extra addresses of the primary network interface.
	PrivateIP    string
	SecondaryIPs []string
	// PublicIP is an auto-assigned public address. Associating an Elastic IP with the primary
	// private address releases it.
	PublicIP string
	AZ       string // "us-east-1a" when empty
	Tags     map[string]string
	// Impaired makes DescribeInstanceStatus report the instance status as impaired.
	Impaired bool
	// StopPolls is how many describe calls still see "stopping" after StopInstances.
	StopPolls int
	// StopNeedsForce keeps the instance at "stopping" until a StopInstances call has Force set.
	StopNeedsForce bool
}

type instance struct {
	Instance
	stopIn int
	stuck  bool
}

// Address is an Elastic IP in the fake. Give InstanceID (and optionally PrivateIP, the primary
// one by default) to start it associated.
type Address struct {
	AllocationID  string
	PublicIP      string
	AssociationID string
	InstanceID    string
	PrivateIP     string
	Tags          map[string]string
}

var stateCodes = map[string]int{"pending": 0, "running": 16, "shutting-down": 32, "terminated": 48, "stopping": 64, "stopped": 80}

// AddInstance adds or replaces an instance.
func (s *Server) AddInstance(i Instance) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i.State == "" {
		i.State = "running"
	}
	if i.PrivateIP == "" {
		i.PrivateIP = fmt.Sprintf("10.77.0.%d", 10+len(s.instances))
	}
	if i.AZ == "" {
		i.AZ = "us-east-1a"
	}
	s.instances[i.ID] = &instance{Instance: i, stuck: i.State == "stopping" && i.StopNeedsForce, stopIn: i.StopPolls}
}

// UpdateInstance changes an instance in place, for example to give it a new public address.
func (s *Server) UpdateInstance(id string, f func(*Instance)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.instances[id]
	if i == nil {
		s.t.Fatalf("awsfake: no instance %s", id)
	}
	f(&i.Instance)
}

// InstanceState is the current state of an instance, without counting as a describe call.
func (s *Server) InstanceState(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i := s.instances[id]; i != nil {
		return i.State
	}
	return ""
}

// AddAddress adds an Elastic IP.
func (s *Server) AddAddress(a Address) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a.InstanceID != "" {
		if i := s.instances[a.InstanceID]; i != nil {
			if a.PrivateIP == "" {
				a.PrivateIP = i.PrivateIP
			}
			if a.PrivateIP == i.PrivateIP {
				i.PublicIP = ""
			}
		}
		if a.AssociationID == "" {
			a.AssociationID = fmt.Sprintf("eipassoc-%08x", s.next())
		}
	}
	s.addresses = append(s.addresses, &a)
}

// AddressOf returns the Elastic IP with the allocation id as it is now.
func (s *Server) AddressOf(allocationID string) Address {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.addresses {
		if a.AllocationID == allocationID {
			return *a
		}
	}
	s.t.Fatalf("awsfake: no address %s", allocationID)
	return Address{}
}

func (i *instance) eni() string { return "eni-" + strings.TrimPrefix(i.ID, "i-") }

func (i *instance) ips() []string { return append([]string{i.PrivateIP}, i.SecondaryIPs...) }

// publicIP is the address that maps to the primary private address: an Elastic IP, else the
// auto-assigned one.
func (s *Server) publicIP(i *instance, privateIP string) string {
	for _, a := range s.addresses {
		if a.InstanceID == i.ID && a.PrivateIP == privateIP {
			return a.PublicIP
		}
	}
	if privateIP == i.PrivateIP {
		return i.PublicIP
	}
	return ""
}

func (s *Server) ec2(c Call) result {
	f, hit := s.nextFault("ec2", c.Action)
	if hit && !(f.Applied && !c.DryRun) {
		return fail(f.Status, f.Code, f.Message)
	}
	r := s.ec2Action(c)
	if hit {
		return fail(f.Status, f.Code, f.Message)
	}
	return r
}

func (s *Server) ec2Action(c Call) result {
	q := c.Params
	var r result
	switch c.Action {
	case "DescribeInstances", "DescribeInstanceStatus", "DescribeAddresses":
		if c.DryRun {
			// A describe changes nothing, but a real one counts as a poll of a stopping instance.
			return fail(http.StatusPreconditionFailed, "DryRunOperation", "Request would have succeeded, but DryRun flag is set.")
		}
		r = map[string]func(url.Values) result{
			"DescribeInstances": s.describeInstances, "DescribeInstanceStatus": s.describeInstanceStatus, "DescribeAddresses": s.describeAddresses,
		}[c.Action](q)
	case "StopInstances":
		r = s.stopInstances(q, c.DryRun)
	case "AssociateAddress":
		r = s.associateAddress(q, c.DryRun)
	case "DisassociateAddress":
		r = s.disassociateAddress(q, c.DryRun)
	default:
		return fail(http.StatusBadRequest, "InvalidAction", "The action "+c.Action+" is not valid for this web service.")
	}
	if c.DryRun && r.code == "" {
		return fail(http.StatusPreconditionFailed, "DryRunOperation", "Request would have succeeded, but DryRun flag is set.")
	}
	return r
}

const ec2NS = `xmlns="http://ec2.amazonaws.com/doc/2016-11-15/"`

func envelope(action, inner string) result {
	return success(fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?><%sResponse %s><requestId>fake-request</requestId>%s</%sResponse>`, action, ec2NS, inner, action))
}

// list reads k.1, k.2, ... from the form.
func list(q url.Values, k string) []string {
	var out []string
	for n := 1; q.Has(k + "." + strconv.Itoa(n)); n++ {
		out = append(out, q.Get(k+"."+strconv.Itoa(n)))
	}
	return out
}

type filter struct {
	name   string
	values []string
}

func filters(q url.Values) []filter {
	var out []filter
	for n := 1; q.Has("Filter." + strconv.Itoa(n) + ".Name"); n++ {
		p := "Filter." + strconv.Itoa(n)
		out = append(out, filter{q.Get(p + ".Name"), list(q, p+".Value")})
	}
	return out
}

func contains(vs []string, v string) bool {
	for _, x := range vs {
		if x == v {
			return true
		}
	}
	return false
}

// page cuts items to the page that NextToken asks for and returns the token of the next one.
func (s *Server) page(n int, q url.Values) (from, to int, next string) {
	from, to = 0, n
	if t := q.Get("NextToken"); t != "" {
		from, _ = strconv.Atoi(t)
	}
	if s.pageSize > 0 && from+s.pageSize < n {
		to = from + s.pageSize
		next = strconv.Itoa(to)
	}
	return min(from, n), to, next
}

func (s *Server) sortedInstances() []*instance {
	out := make([]*instance, 0, len(s.instances))
	for _, i := range s.instances {
		out = append(out, i)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out
}

// selectInstances applies InstanceId.N and the filters; the caller holds mu.
func (s *Server) selectInstances(q url.Values) ([]*instance, *result) {
	ids := list(q, "InstanceId")
	for _, id := range ids {
		if s.instances[id] == nil {
			r := fail(http.StatusBadRequest, "InvalidInstanceID.NotFound", fmt.Sprintf("The instance ID '%s' does not exist", id))
			return nil, &r
		}
	}
	fs := filters(q)
	var out []*instance
	for _, i := range s.sortedInstances() {
		if len(ids) > 0 && !contains(ids, i.ID) {
			continue
		}
		match := true
		for _, f := range fs {
			have, present := "", true
			switch {
			case f.name == "instance-id":
				have = i.ID
			case f.name == "instance-state-name":
				have = i.State
			case f.name == "private-ip-address":
				have = i.PrivateIP
			case f.name == "ip-address":
				have = s.publicIP(i, i.PrivateIP)
			case strings.HasPrefix(f.name, "tag:"):
				have, present = i.Tags[strings.TrimPrefix(f.name, "tag:")]
			default:
				r := fail(http.StatusBadRequest, "InvalidParameterValue", "The filter '"+f.name+"' is invalid")
				return nil, &r
			}
			if !present || !contains(f.values, have) {
				match = false
			}
		}
		if match {
			out = append(out, i)
		}
	}
	return out, nil
}

// settle moves an instance that is stopping one poll closer to stopped.
func (i *instance) settle() {
	if i.State != "stopping" || i.stuck {
		return
	}
	if i.stopIn > 0 {
		i.stopIn--
		return
	}
	i.State = "stopped"
}

func stateXML(name string) string {
	return fmt.Sprintf("<code>%d</code><name>%s</name>", stateCodes[name], name)
}

func tagsXML(tags map[string]string) string {
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("<tagSet>")
	for _, k := range keys {
		fmt.Fprintf(&b, "<item><key>%s</key><value>%s</value></item>", esc(k), esc(tags[k]))
	}
	b.WriteString("</tagSet>")
	return b.String()
}

func (s *Server) describeInstances(q url.Values) result {
	s.mu.Lock()
	defer s.mu.Unlock()
	sel, errRes := s.selectInstances(q)
	if errRes != nil {
		return *errRes
	}
	from, to, next := s.page(len(sel), q)
	var b strings.Builder
	b.WriteString("<reservationSet>")
	for _, i := range sel[from:to] {
		i.settle()
		fmt.Fprintf(&b, "<item><reservationId>r-%s</reservationId><instancesSet><item><instanceId>%s</instanceId><instanceState>%s</instanceState><privateIpAddress>%s</privateIpAddress>",
			strings.TrimPrefix(i.ID, "i-"), esc(i.ID), stateXML(i.State), i.PrivateIP)
		if ip := s.publicIP(i, i.PrivateIP); ip != "" {
			fmt.Fprintf(&b, "<ipAddress>%s</ipAddress>", ip)
		}
		fmt.Fprintf(&b, "<placement><availabilityZone>%s</availabilityZone></placement><subnetId>subnet-0fake</subnetId><vpcId>vpc-0fake</vpcId>", esc(i.AZ))
		fmt.Fprintf(&b, "<networkInterfaceSet><item><networkInterfaceId>%s</networkInterfaceId><attachment><deviceIndex>0</deviceIndex></attachment><privateIpAddressesSet>", i.eni())
		for n, ip := range i.ips() {
			fmt.Fprintf(&b, "<item><privateIpAddress>%s</privateIpAddress><primary>%t</primary>", ip, n == 0)
			if pub := s.publicIP(i, ip); pub != "" {
				fmt.Fprintf(&b, "<association><publicIp>%s</publicIp></association>", pub)
			}
			b.WriteString("</item>")
		}
		b.WriteString("</privateIpAddressesSet></item></networkInterfaceSet>")
		b.WriteString(tagsXML(i.Tags))
		b.WriteString("</item></instancesSet></item>")
	}
	b.WriteString("</reservationSet>")
	if next != "" {
		fmt.Fprintf(&b, "<nextToken>%s</nextToken>", next)
	}
	return envelope("DescribeInstances", b.String())
}

func (s *Server) describeInstanceStatus(q url.Values) result {
	s.mu.Lock()
	defer s.mu.Unlock()
	sel, errRes := s.selectInstances(q)
	if errRes != nil {
		return *errRes
	}
	all := q.Get("IncludeAllInstances") == "true"
	var kept []*instance
	for _, i := range sel {
		i.settle()
		if all || i.State == "running" {
			kept = append(kept, i)
		}
	}
	from, to, next := s.page(len(kept), q)
	var b strings.Builder
	b.WriteString("<instanceStatusSet>")
	for _, i := range kept[from:to] {
		sys, inst := "not-applicable", "not-applicable"
		if i.State == "running" {
			sys, inst = "ok", "ok"
			if i.Impaired {
				inst = "impaired"
			}
		}
		fmt.Fprintf(&b, "<item><instanceId>%s</instanceId><availabilityZone>%s</availabilityZone><instanceState>%s</instanceState><systemStatus><status>%s</status></systemStatus><instanceStatus><status>%s</status></instanceStatus></item>",
			esc(i.ID), esc(i.AZ), stateXML(i.State), sys, inst)
	}
	b.WriteString("</instanceStatusSet>")
	if next != "" {
		fmt.Fprintf(&b, "<nextToken>%s</nextToken>", next)
	}
	return envelope("DescribeInstanceStatus", b.String())
}

func (s *Server) describeAddresses(q url.Values) result {
	s.mu.Lock()
	defer s.mu.Unlock()
	allocs, ips, fs := list(q, "AllocationId"), list(q, "PublicIp"), filters(q)
	for _, id := range allocs {
		if s.address(id) == nil {
			return fail(http.StatusBadRequest, "InvalidAllocationID.NotFound", fmt.Sprintf("The allocation ID '%s' does not exist", id))
		}
	}
	var b strings.Builder
	b.WriteString("<addressesSet>")
	for _, a := range s.addresses {
		if (len(allocs) > 0 && !contains(allocs, a.AllocationID)) || (len(ips) > 0 && !contains(ips, a.PublicIP)) {
			continue
		}
		match := true
		for _, f := range fs {
			var have string
			switch {
			case f.name == "allocation-id":
				have = a.AllocationID
			case f.name == "instance-id":
				have = a.InstanceID
			case f.name == "public-ip":
				have = a.PublicIP
			case strings.HasPrefix(f.name, "tag:"):
				have = a.Tags[strings.TrimPrefix(f.name, "tag:")]
			default:
				return fail(http.StatusBadRequest, "InvalidParameterValue", "The filter '"+f.name+"' is invalid")
			}
			if !contains(f.values, have) {
				match = false
			}
		}
		if !match {
			continue
		}
		fmt.Fprintf(&b, "<item><publicIp>%s</publicIp><allocationId>%s</allocationId><domain>vpc</domain>", a.PublicIP, esc(a.AllocationID))
		if a.InstanceID != "" {
			fmt.Fprintf(&b, "<associationId>%s</associationId><instanceId>%s</instanceId><networkInterfaceId>%s</networkInterfaceId><privateIpAddress>%s</privateIpAddress>",
				esc(a.AssociationID), esc(a.InstanceID), s.instances[a.InstanceID].eni(), a.PrivateIP)
		}
		b.WriteString(tagsXML(a.Tags))
		b.WriteString("</item>")
	}
	b.WriteString("</addressesSet>")
	return envelope("DescribeAddresses", b.String())
}

func (s *Server) address(allocationID string) *Address {
	for _, a := range s.addresses {
		if a.AllocationID == allocationID {
			return a
		}
	}
	return nil
}

func (s *Server) stopInstances(q url.Values, dryRun bool) result {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := list(q, "InstanceId")
	for _, id := range ids {
		if s.instances[id] == nil {
			return fail(http.StatusBadRequest, "InvalidInstanceID.NotFound", fmt.Sprintf("The instance ID '%s' does not exist", id))
		}
	}
	force := q.Get("Force") == "true"
	var b strings.Builder
	b.WriteString("<instancesSet>")
	for _, id := range ids {
		i := s.instances[id]
		prev := i.State
		if !dryRun {
			switch i.State {
			case "running":
				i.State, i.stopIn, i.stuck = "stopping", i.StopPolls, i.StopNeedsForce && !force
			case "stopping":
				if force {
					i.stuck = false
				}
			}
		}
		fmt.Fprintf(&b, "<item><instanceId>%s</instanceId><currentState>%s</currentState><previousState>%s</previousState></item>", esc(id), stateXML(i.State), stateXML(prev))
	}
	b.WriteString("</instancesSet>")
	return envelope("StopInstances", b.String())
}

func (s *Server) associateAddress(q url.Values, dryRun bool) result {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.address(q.Get("AllocationId"))
	if a == nil {
		return fail(http.StatusBadRequest, "InvalidAllocationID.NotFound", fmt.Sprintf("The allocation ID '%s' does not exist", q.Get("AllocationId")))
	}
	var target *instance
	switch {
	case q.Get("InstanceId") != "":
		target = s.instances[q.Get("InstanceId")]
	case q.Get("NetworkInterfaceId") != "":
		for _, i := range s.instances {
			if i.eni() == q.Get("NetworkInterfaceId") {
				target = i
			}
		}
	default:
		return fail(http.StatusBadRequest, "MissingParameter", "Either InstanceId or NetworkInterfaceId must be specified.")
	}
	if target == nil {
		return fail(http.StatusBadRequest, "InvalidInstanceID.NotFound", "The instance or network interface does not exist.")
	}
	priv := q.Get("PrivateIpAddress")
	if priv == "" {
		priv = target.PrivateIP
	}
	if !contains(target.ips(), priv) {
		return fail(http.StatusBadRequest, "InvalidParameterValue", fmt.Sprintf("The address %s does not belong to the network interface.", priv))
	}
	if a.InstanceID != "" && q.Get("AllowReassociation") != "true" {
		return fail(http.StatusBadRequest, "Resource.AlreadyAssociated", fmt.Sprintf("resource %s is already associated with associate-id %s.", a.AllocationID, a.AssociationID))
	}
	if dryRun {
		return envelope("AssociateAddress", "")
	}
	for _, o := range s.addresses {
		if o != a && o.InstanceID == target.ID && o.PrivateIP == priv {
			o.InstanceID, o.PrivateIP, o.AssociationID = "", "", ""
		}
	}
	a.InstanceID, a.PrivateIP, a.AssociationID = target.ID, priv, fmt.Sprintf("eipassoc-%08x", s.next())
	if priv == target.PrivateIP {
		target.PublicIP = ""
	}
	return envelope("AssociateAddress", fmt.Sprintf("<return>true</return><associationId>%s</associationId>", a.AssociationID))
}

func (s *Server) disassociateAddress(q url.Values, dryRun bool) result {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.addresses {
		if a.AssociationID != "" && a.AssociationID == q.Get("AssociationId") {
			if !dryRun {
				a.InstanceID, a.PrivateIP, a.AssociationID = "", "", ""
			}
			return envelope("DisassociateAddress", "<return>true</return>")
		}
	}
	return fail(http.StatusBadRequest, "InvalidAssociationID.NotFound", fmt.Sprintf("The association ID '%s' does not exist", q.Get("AssociationId")))
}
