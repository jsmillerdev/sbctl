package cloudformation_test

// The stack-compatibility guard. A stack made from the v0.1.1 template is updated with the
// template of this release by a change set that must not touch the instance, the data volume,
// the Elastic IP or the network. CloudFormation decides that at update time; these tests decide it
// ahead of time from the two files: they apply the conditions the way CloudFormation does with the
// parameters of a v0.1.1 stack (every parameter at its default, since an update with
// UsePreviousValue gives new parameters their defaults) and compare what is left.

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// rendered is a template with its conditions applied: the resources that exist for the given
// parameter values, their properties with every Fn::If resolved and every AWS::NoValue gone.
// Other intrinsic functions (Ref, Sub, GetAtt, Join ...) stay as they are; two templates that leave
// the same ones in a property give CloudFormation the same value to compare.
type rendered struct {
	t      *testing.T
	params map[string]string
	conds  map[string]any // the Conditions section, evaluated on demand
	res    map[string]doc
	raw    doc
}

// render applies the conditions of d for parameters at their defaults, except where over has a value.
func render(t *testing.T, d doc, over map[string]string) *rendered {
	t.Helper()
	r := &rendered{t: t, params: map[string]string{}, conds: map[string]any{}, res: map[string]doc{}, raw: d}
	for name, p := range get(t, d, "Parameters").(doc) {
		if def, ok := p.(doc)["Default"]; ok {
			r.params[name] = fmt.Sprint(def)
		}
	}
	for k, v := range over {
		r.params[k] = v
	}
	if has(d, "Conditions") {
		for name, c := range get(t, d, "Conditions").(doc) {
			r.conds[name] = c
		}
	}
	for name, res := range get(t, d, "Resources").(doc) {
		rd := res.(doc)
		if c, ok := rd["Condition"].(string); ok && !r.cond(c) {
			continue
		}
		out := doc{}
		for k, v := range rd {
			if k == "Condition" || k == "Metadata" {
				continue
			}
			pv, drop := r.prune(v)
			if !drop {
				out[k] = pv
			}
		}
		r.res[name] = out
	}
	return r
}

func (r *rendered) cond(name string) bool {
	c, ok := r.conds[name]
	if !ok {
		r.t.Fatalf("condition %s is not defined", name)
	}
	return r.evalCond(c)
}

func (r *rendered) evalCond(c any) bool {
	r.t.Helper()
	m, ok := c.(doc)
	if !ok || len(m) != 1 {
		r.t.Fatalf("not a condition: %v", c)
	}
	for fn, arg := range m {
		switch fn {
		case "Condition":
			return r.cond(arg.(string))
		case "Fn::Equals":
			l := arg.([]any)
			return r.value(l[0]) == r.value(l[1])
		case "Fn::Not":
			return !r.evalCond(arg.([]any)[0])
		case "Fn::And", "Fn::Or":
			all := fn == "Fn::And"
			for _, e := range arg.([]any) {
				if r.evalCond(e) != all {
					return !all
				}
			}
			return all
		}
		r.t.Fatalf("condition function %s is not supported by this test", fn)
	}
	return false
}

// value resolves the two operand shapes the template's conditions use.
func (r *rendered) value(v any) string {
	r.t.Helper()
	if m, ok := v.(doc); ok {
		name, ok := m["Ref"].(string)
		if !ok || len(m) != 1 {
			r.t.Fatalf("condition operand %v is not a parameter reference", v)
		}
		val, ok := r.params[name]
		if !ok {
			r.t.Fatalf("parameter %s has no value (no default and not given)", name)
		}
		return val
	}
	return fmt.Sprint(v)
}

// prune resolves Fn::If and removes AWS::NoValue, in maps and in lists.
func (r *rendered) prune(v any) (out any, drop bool) {
	switch x := v.(type) {
	case doc:
		if args, ok := x["Fn::If"]; ok && len(x) == 1 {
			l := args.([]any)
			if r.cond(l[0].(string)) {
				return r.prune(l[1])
			}
			return r.prune(l[2])
		}
		if ref, ok := x["Ref"]; ok && len(x) == 1 && ref == "AWS::NoValue" {
			return nil, true
		}
		m := doc{}
		for k, e := range x {
			if pv, d := r.prune(e); !d {
				m[k] = pv
			}
		}
		return m, false
	case []any:
		l := make([]any, 0, len(x))
		for _, e := range x {
			if pv, d := r.prune(e); !d {
				l = append(l, pv)
			}
		}
		return l, false
	}
	return v, false
}

// canon is the JSON of v with sorted keys: equal values give equal bytes.
func canon(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// mayChange lists, per logical ID, the properties that an update from v0.1.1 may change, and why.
// Everything else of a resource that exists in both templates must come out identical.
var mayChange = map[string]string{
	"Instance.Tags":            "tags change without an interruption; the node reads them from the metadata service",
	"Instance.MetadataOptions": "InstanceMetadataTags changes without an interruption",
	"ElasticIp.Tags":           "tags change without an interruption",
	"InstanceRole.Tags":        "tags change without an interruption; the failover permissions and the Storage role test them",
	"InstanceRole.Policies":    "policies may gain statements and policies (checked on its own below)",
}

// addedAtDefaults are the resources that exist after the update although the person turned nothing
// on: the Storage bucket and the role that reaches it, which an update makes for every stack. Every
// other new resource must be absent at the defaults.
var addedAtDefaults = []string{"ObjectsBucket", "ObjectsBucketPolicy", "StorageAssumePolicy", "StorageRole"}

// diffStacks returns every way the update from old to nw is not additive.
func diffStacks(old, nw *rendered) []string {
	var bad []string
	add := func(f string, a ...any) { bad = append(bad, fmt.Sprintf(f, a...)) }

	ids := make([]string, 0, len(old.res))
	for id := range old.res {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		o := old.res[id]
		n, ok := nw.res[id]
		if !ok {
			add("%s exists in v0.1.1 and not in this template (an update would delete it)", id)
			continue
		}
		keys := map[string]bool{}
		for k := range o {
			keys[k] = true
		}
		for k := range n {
			keys[k] = true
		}
		for k := range keys {
			if k != "Properties" {
				if canon(o[k]) != canon(n[k]) {
					add("%s.%s changed from %s to %s", id, k, canon(o[k]), canon(n[k]))
				}
				continue
			}
			oldProps, _ := o[k].(doc)
			newProps, _ := n[k].(doc)
			props := map[string]bool{}
			for pk := range oldProps {
				props[pk] = true
			}
			for pk := range newProps {
				props[pk] = true
			}
			for pk := range props {
				if canon(oldProps[pk]) == canon(newProps[pk]) {
					continue
				}
				if _, ok := mayChange[id+"."+pk]; !ok {
					add("%s.%s changed (an update may replace or interrupt it):\n  v0.1.1: %s\n  now:    %s", id, pk, canon(oldProps[pk]), canon(newProps[pk]))
				}
			}
		}
		// The instance role's policies may gain entries; every v0.1.1 policy stays as it was.
		if id == "InstanceRole" {
			newPolicies := map[string]string{}
			for _, p := range np(n)["Policies"].([]any) {
				newPolicies[p.(doc)["PolicyName"].(string)] = canon(p)
			}
			for _, p := range np(o)["Policies"].([]any) {
				name := p.(doc)["PolicyName"].(string)
				if newPolicies[name] != canon(p) {
					add("InstanceRole policy %s changed:\n  v0.1.1: %s\n  now:    %s", name, canon(p), newPolicies[name])
				}
			}
		}
	}

	// A resource that is new at the defaults is one of the known additions.
	var added []string
	for id := range nw.res {
		if _, ok := old.res[id]; !ok {
			added = append(added, id)
		}
	}
	sort.Strings(added)
	if !reflect.DeepEqual(added, addedAtDefaults) {
		add("resources that appear at the defaults: %v, want %v (a new resource must be conditional on a feature that is off by default)", added, addedAtDefaults)
	}

	// Parameters: nothing removed or reinterpreted, every new one optional.
	oldP, newP := get(old.t, old.raw, "Parameters").(doc), get(nw.t, nw.raw, "Parameters").(doc)
	for name, p := range oldP {
		q, ok := newP[name]
		if !ok {
			add("parameter %s was removed (an update with UsePreviousValue would fail)", name)
			continue
		}
		for _, k := range []string{"Type", "Default", "NoEcho"} {
			if canon(p.(doc)[k]) != canon(q.(doc)[k]) {
				add("parameter %s: %s changed from %s to %s", name, k, canon(p.(doc)[k]), canon(q.(doc)[k]))
			}
		}
	}
	for name, p := range newP {
		if _, ok := oldP[name]; ok {
			continue
		}
		if _, ok := p.(doc)["Default"]; !ok {
			add("new parameter %s has no default (a stack that does not know it could not be updated)", name)
		}
	}
	// Outputs people and scripts read keep existing.
	for name := range get(old.t, old.raw, "Outputs").(doc) {
		if !has(nw.raw, "Outputs", name) {
			add("output %s was removed", name)
		}
	}
	return bad
}

// np is the Properties map of a rendered resource.
func np(res doc) doc { return res["Properties"].(doc) }

func TestUpgradeFromV011(t *testing.T) {
	oldT, newT := loadFile(t, "testdata/supavise-v0.1.1.yaml"), load(t)
	// What a v0.1.1 stack holds: every parameter at its default (AdminEmail has none; any value).
	// The update passes UsePreviousValue for the parameters the stack has, and the new ones keep
	// their defaults, so this is the parameter set of the update.
	over := map[string]string{"AdminEmail": "owner@example.com"}
	old, nw := render(t, oldT, over), render(t, newT, over)

	for _, v := range diffStacks(old, nw) {
		t.Error(v)
	}

	// The user data is the same text with the same variables, byte for byte: a change would be an
	// interruption of the instance.
	if got, want := canon(np(nw.res["Instance"])["UserData"]), canon(np(old.res["Instance"])["UserData"]); got != want {
		t.Errorf("the user data of an ordinary stack differs from v0.1.1 (%d and %d bytes): an update would interrupt the instance", len(got), len(want))
	}

	// The properties that replace a resource are named in the design; say so for each, so that a
	// reader sees the guard cover them, and so that an edit of mayChange cannot quietly widen it.
	for _, c := range []struct{ id, prop string }{
		{"Instance", "ImageId"}, {"Instance", "NetworkInterfaces"}, {"Instance", "BlockDeviceMappings"}, {"Instance", "UserData"},
		{"Instance", "InstanceType"}, {"Instance", "IamInstanceProfile"}, {"Instance", "KeyName"},
		{"DataVolume", "AvailabilityZone"}, {"DataVolume", "Encrypted"}, {"DataVolume", "SnapshotId"},
		{"Subnet", "AvailabilityZone"}, {"Subnet", "CidrBlock"}, {"Vpc", "CidrBlock"},
		{"SecurityGroup", "SecurityGroupIngress"}, {"SecurityGroup", "VpcId"},
		{"ElasticIpAssociation", "InstanceId"}, {"ElasticIpAssociation", "AllocationId"},
	} {
		if _, ok := mayChange[c.id+"."+c.prop]; ok {
			t.Errorf("%s.%s must never change in an update", c.id, c.prop)
		}
		if canon(np(old.res[c.id])[c.prop]) != canon(np(nw.res[c.id])[c.prop]) {
			t.Errorf("%s.%s differs from v0.1.1", c.id, c.prop)
		}
	}
	// The BYO-VPC stack: the same holds when the person gave a VPC and subnet.
	byo := map[string]string{"AdminEmail": "owner@example.com", "VpcId": "vpc-0123456789abcdef0", "SubnetId": "subnet-0123456789abcdef0"}
	for _, v := range diffStacks(render(t, oldT, byo), render(t, newT, byo)) {
		t.Errorf("with an existing VPC: %s", v)
	}
	// The stack of the live node was made by the release, so SupaviseVersion holds the tag, not
	// "latest": the user data then takes the other branch of IsLatest, and must still not change.
	pinned := map[string]string{"AdminEmail": "owner@example.com", "SupaviseVersion": "v0.1.1"}
	pOld, pNew := render(t, oldT, pinned), render(t, newT, pinned)
	for _, v := range diffStacks(pOld, pNew) {
		t.Errorf("with SupaviseVersion=v0.1.1: %s", v)
	}
	if got, want := canon(np(pNew.res["Instance"])["UserData"]), canon(np(pOld.res["Instance"])["UserData"]); got != want {
		t.Error("with SupaviseVersion=v0.1.1 the user data differs from v0.1.1: an update would interrupt the instance")
	}
	if !strings.Contains(canon(np(pNew.res["Instance"])["UserData"]), "releases/download/") {
		t.Error("the pinned stack should download its installer from the tag's release")
	}
	// A stack with a domain and a hosted zone, and one made with the optional features it had.
	full := map[string]string{"AdminEmail": "owner@example.com", "DomainName": "example.com", "HostedZoneId": "Z0123456789ABCDEFGHIJ",
		"KeyEscrowPassphrase": "a passphrase of twelve", "SshCidr": "203.0.113.4/32", "KeyName": "k", "AmiId": "ami-0123456789abcdef0",
		"DataSnapshotId": "snap-0123456789abcdef0", "EnableSessionManager": "false", "DailySnapshotsKept": "0"}
	rOld, rNew := render(t, oldT, full), render(t, newT, full)
	if got := diffStacks(rOld, rNew); len(got) != 0 {
		// The Storage resources are the same at any parameters, so the same list is expected here.
		for _, v := range got {
			t.Errorf("with every v0.1.1 option set: %s", v)
		}
	}
}

// deepCopy copies the maps and lists of a parsed template.
func deepCopy(v any) any {
	switch x := v.(type) {
	case doc:
		m := doc{}
		for k, e := range x {
			m[k] = deepCopy(e)
		}
		return m
	case []any:
		l := make([]any, len(x))
		for i, e := range x {
			l[i] = deepCopy(e)
		}
		return l
	}
	return v
}

// TestUpgradeGuardCatchesReplacements makes sure the comparison above is not vacuous: each edit
// below is one a careless change could make, and each must be reported.
func TestUpgradeGuardCatchesReplacements(t *testing.T) {
	oldT := loadFile(t, "testdata/supavise-v0.1.1.yaml")
	over := map[string]string{"AdminEmail": "owner@example.com"}
	old := render(t, oldT, over)
	cases := []struct {
		name string
		edit func(res doc, d doc)
		want string
	}{
		{"image", func(res, d doc) { get(t, res["Instance"], "Properties").(doc)["ImageId"] = "ami-0123456789abcdef0" }, "Instance.ImageId"},
		{"network interface", func(res, d doc) {
			get(t, res["Instance"], "Properties", "NetworkInterfaces").([]any)[0].(doc)["SecondaryPrivateIpAddressCount"] = 1
		}, "Instance.NetworkInterfaces"},
		{"user data", func(res, d doc) {
			ud := get(t, res["Instance"], "Properties", "UserData", "Fn::Base64", "Fn::If").([]any)
			ud[2].(doc)["Fn::Sub"].([]any)[0] = ud[2].(doc)["Fn::Sub"].([]any)[0].(string) + "\necho changed\n"
		}, "Instance.UserData"},
		{"security group", func(res, d doc) {
			get(t, res["Instance"], "Properties").(doc)["SecurityGroupIds"] = []any{"sg-1"}
		}, "Instance.SecurityGroupIds"},
		{"volume zone", func(res, d doc) { get(t, res["DataVolume"], "Properties").(doc)["AvailabilityZone"] = "us-east-1b" }, "DataVolume.AvailabilityZone"},
		{"volume encryption", func(res, d doc) { get(t, res["DataVolume"], "Properties").(doc)["Encrypted"] = false }, "DataVolume.Encrypted"},
		{"bucket name", func(res, d doc) { get(t, res["BackupBucket"], "Properties").(doc)["BucketName"] = "fixed" }, "BackupBucket.BucketName"},
		{"bucket policy", func(res, d doc) { res["BackupBucket"].(doc)["DeletionPolicy"] = "Delete" }, "BackupBucket.DeletionPolicy"},
		{"vpc range", func(res, d doc) { get(t, res["Vpc"], "Properties").(doc)["CidrBlock"] = "10.78.0.0/24" }, "Vpc.CidrBlock"},
		{"subnet zone", func(res, d doc) { get(t, res["Subnet"], "Properties").(doc)["AvailabilityZone"] = "us-east-1c" }, "Subnet.AvailabilityZone"},
		{"address association", func(res, d doc) {
			get(t, res["ElasticIpAssociation"], "Properties").(doc)["InstanceId"] = "i-1"
		}, "ElasticIpAssociation.InstanceId"},
		{"instance type", func(res, d doc) { get(t, res["Instance"], "Properties").(doc)["InstanceType"] = "t4g.small" }, "Instance.InstanceType"},
		{"resource removed", func(res, d doc) { delete(res, "DataVolumeAttachment") }, "DataVolumeAttachment exists in v0.1.1"},
		{"policy rewritten", func(res, d doc) {
			p := get(t, res["InstanceRole"], "Properties", "Policies").([]any)[0].(doc)
			p["PolicyDocument"].(doc)["Statement"].([]any)[0].(doc)["Resource"] = "*"
		}, "InstanceRole policy backup-bucket changed"},
		{"new resource at the defaults", func(res, d doc) {
			res["Surprise"] = doc{"Type": "AWS::SQS::Queue", "Properties": doc{}}
		}, "resources that appear at the defaults"},
		{"parameter without a default", func(res, d doc) {
			get(t, d, "Parameters").(doc)["Surprise"] = doc{"Type": "String"}
		}, "new parameter Surprise has no default"},
		{"parameter removed", nil, "parameter KeyName was removed"},
		{"default changed", func(res, d doc) { get(t, d, "Parameters", "DataVolumeSize").(doc)["Default"] = 200 }, "parameter DataVolumeSize: Default changed"},
		{"output removed", func(res, d doc) { delete(get(t, d, "Outputs").(doc), "PublicIp") }, "output PublicIp was removed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := deepCopy(load(t)).(doc)
			// Edit the template, not the rendering: the rendering is made from it afterwards.
			if c.edit != nil {
				c.edit(get(t, d, "Resources").(doc), d)
			}
			nw := render(t, d, over)
			if c.edit == nil {
				// A parameter that the conditions use cannot be taken out before the rendering.
				delete(get(t, nw.raw, "Parameters").(doc), "KeyName")
			}
			got := strings.Join(diffStacks(old, nw), "\n")
			if !strings.Contains(got, c.want) {
				t.Errorf("the guard did not report %q; it reported:\n%s", c.want, got)
			}
		})
	}
	// The unedited template passes, so the failures above come from the edits.
	if got := diffStacks(old, render(t, load(t), over)); len(got) != 0 {
		t.Errorf("the template as it is fails the guard:\n%s", strings.Join(got, "\n"))
	}
}
