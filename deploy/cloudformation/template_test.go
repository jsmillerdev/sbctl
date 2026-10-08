// Package cloudformation_test guards the drop-in AWS path: the template parses, its console form
// is clear, its outputs name what a person needs, nothing that holds data is deleted silently and
// the instance role stays narrow. cfn-lint and checkov (ci.yml) check the syntax and the generic
// rules; these tests check the promises of this template, so that an edit cannot break them
// unnoticed. They run offline and touch no AWS account.
package cloudformation_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type doc = map[string]any

// load parses supavise.yaml. CloudFormation's short forms (!Ref, !Sub, !If ...) become the long
// forms ({"Ref": ...}, {"Fn::Sub": ...}), so the assertions read like the JSON of the template.
func load(t *testing.T) doc {
	t.Helper()
	raw, err := os.ReadFile("supavise.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var root yaml.Node
	if err := yaml.Unmarshal(raw, &root); err != nil {
		t.Fatalf("supavise.yaml does not parse: %v", err)
	}
	return convert(t, &root).(doc)
}

func convert(t *testing.T, n *yaml.Node) any {
	t.Helper()
	var v any
	switch n.Kind {
	case yaml.DocumentNode:
		return convert(t, n.Content[0])
	case yaml.MappingNode:
		m := doc{}
		for i := 0; i < len(n.Content); i += 2 {
			m[n.Content[i].Value] = convert(t, n.Content[i+1])
		}
		v = m
	case yaml.SequenceNode:
		l := make([]any, 0, len(n.Content))
		for _, c := range n.Content {
			l = append(l, convert(t, c))
		}
		v = l
	case yaml.AliasNode:
		t.Fatalf("line %d: aliases are not CloudFormation", n.Line)
	default:
		if strings.HasPrefix(n.Tag, "!") && !strings.HasPrefix(n.Tag, "!!") {
			v = n.Value
		} else if err := n.Decode(&v); err != nil {
			t.Fatalf("line %d: %v", n.Line, err)
		}
	}
	if strings.HasPrefix(n.Tag, "!") && !strings.HasPrefix(n.Tag, "!!") {
		name := n.Tag[1:]
		switch name {
		case "Ref", "Condition":
		case "GetAtt":
			if s, ok := v.(string); ok {
				a, b, _ := strings.Cut(s, ".")
				v = []any{a, b}
			}
			name = "Fn::GetAtt"
		default:
			name = "Fn::" + name
		}
		return doc{name: v}
	}
	return v
}

func get(t *testing.T, v any, path ...string) any {
	t.Helper()
	for _, p := range path {
		m, ok := v.(doc)
		if !ok {
			t.Fatalf("%s: not a map at %q", strings.Join(path, "."), p)
		}
		v, ok = m[p]
		if !ok {
			t.Fatalf("%s: missing %q", strings.Join(path, "."), p)
		}
	}
	return v
}

func has(v any, path ...string) bool {
	for _, p := range path {
		m, ok := v.(doc)
		if !ok {
			return false
		}
		if v, ok = m[p]; !ok {
			return false
		}
	}
	return true
}

func keys(m doc) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func strs(t *testing.T, v any) []string {
	t.Helper()
	l, ok := v.([]any)
	if !ok {
		t.Fatalf("not a list: %v", v)
	}
	out := make([]string, 0, len(l))
	for _, x := range l {
		s, ok := x.(string)
		if !ok {
			t.Fatalf("not a string: %v", x)
		}
		out = append(out, s)
	}
	return out
}

// strList reads a value that is one string or a list of strings.
func strList(t *testing.T, v any) []string {
	t.Helper()
	if s, ok := v.(string); ok {
		return []string{s}
	}
	return strs(t, v)
}

func TestParametersAreMinimal(t *testing.T) {
	params := get(t, load(t), "Parameters").(doc)
	want := []string{"AccessCidr", "AdminEmail", "AmiId", "DailySnapshotsKept", "DataSnapshotId", "DataVolumeSize", "DomainName", "EnableSessionManager",
		"HostedZoneId", "InstanceType", "KeyEscrowPassphrase", "KeyName", "SshCidr", "SubnetId", "SupaviseVersion", "VpcId"}
	if got := keys(params); !reflect.DeepEqual(got, want) {
		t.Fatalf("parameters changed (update the README table and this list together):\n got %v\nwant %v", got, want)
	}
	// Only the admin email is required: everything else has a default, so the console form needs
	// one field.
	for name, p := range params {
		_, hasDefault := p.(doc)["Default"]
		if name == "AdminEmail" && hasDefault {
			t.Errorf("AdminEmail must have no default")
		}
		if name != "AdminEmail" && !hasDefault {
			t.Errorf("parameter %s has no default: only AdminEmail may be required", name)
		}
		if d, _ := p.(doc)["Description"].(string); strings.TrimSpace(d) == "" {
			t.Errorf("parameter %s has no description", name)
		}
	}
}

func TestDefaultsAreGravitonAndSized(t *testing.T) {
	d := load(t)
	def := get(t, d, "Parameters", "InstanceType", "Default").(string)
	if def != "t4g.large" {
		t.Errorf("default instance type is %s; docs/reference/footprint.md sizes t4g.large (8 GiB) for about 20 projects", def)
	}
	arch := get(t, d, "Mappings", "InstanceTypes", def, "Ubuntu")
	if arch != "arm64" {
		t.Errorf("default instance type must be Graviton (arm64), got %v", arch)
	}
	allowed := strs(t, get(t, d, "Parameters", "InstanceType", "AllowedValues"))
	mapped := keys(get(t, d, "Mappings", "InstanceTypes").(doc))
	sort.Strings(allowed)
	if !reflect.DeepEqual(allowed, mapped) {
		t.Errorf("InstanceType AllowedValues and Mappings.InstanceTypes differ:\n%v\n%v", allowed, mapped)
	}
	graviton := regexp.MustCompile(`^[a-z]+[0-9]+g[a-z]*\.`)
	for _, ty := range allowed {
		want := "amd64"
		if graviton.MatchString(ty) {
			want = "arm64"
		}
		if got := get(t, d, "Mappings", "InstanceTypes", ty, "Ubuntu"); got != want {
			t.Errorf("%s maps to %v, want %s", ty, got, want)
		}
	}
	// A node needs about 1.5 GB before the first project: nothing under 4 GiB.
	for _, ty := range allowed {
		if strings.HasSuffix(ty, ".small") || strings.HasSuffix(ty, ".micro") || strings.HasSuffix(ty, ".nano") {
			t.Errorf("%s is too small for the fixed footprint", ty)
		}
	}
	if v := get(t, d, "Parameters", "SupaviseVersion", "Default"); v != "latest" {
		t.Errorf("the repository template defaults SupaviseVersion to latest (release-assets.sh stamps the tag), got %v", v)
	}
}

func TestConsoleFormCoversEveryParameter(t *testing.T) {
	d := load(t)
	params := get(t, d, "Parameters").(doc)
	iface := get(t, d, "Metadata", "AWS::CloudFormation::Interface")
	groups := get(t, iface, "ParameterGroups").([]any)
	seen := map[string]int{}
	for i, g := range groups {
		label := get(t, g, "Label", "default").(string)
		if label == "" {
			t.Errorf("group %d has no label", i)
		}
		for _, p := range strs(t, get(t, g, "Parameters")) {
			seen[p]++
			if _, ok := params[p]; !ok {
				t.Errorf("group %q lists %s, which is not a parameter", label, p)
			}
		}
	}
	labels := get(t, iface, "ParameterLabels").(doc)
	for name := range params {
		if seen[name] != 1 {
			t.Errorf("parameter %s is in %d groups, want 1", name, seen[name])
		}
		if !has(labels, name, "default") {
			t.Errorf("parameter %s has no label", name)
		}
	}
	for name := range labels {
		if _, ok := params[name]; !ok {
			t.Errorf("label for unknown parameter %s", name)
		}
	}
	first := strs(t, get(t, groups[0], "Parameters"))
	if !reflect.DeepEqual(first, []string{"AdminEmail"}) {
		t.Errorf("the first group is the required one: want [AdminEmail], got %v", first)
	}
}

func TestOutputsNameWhatAPersonNeeds(t *testing.T) {
	d := load(t)
	outs := get(t, d, "Outputs").(doc)
	for _, k := range []string{"DashboardUrl", "ApiUrl", "ClaimUrl", "ClaimTokenCommand", "BackupBucket", "InstanceId", "DataVolumeId", "ConnectCommand", "PublicIp", "DnsRecordsNeeded"} {
		o, ok := outs[k]
		if !ok {
			t.Errorf("output %s is missing", k)
			continue
		}
		if s, _ := get(t, o, "Description").(string); strings.TrimSpace(s) == "" {
			t.Errorf("output %s has no description", k)
		}
	}
	cmd := fmt.Sprint(get(t, outs["ClaimTokenCommand"], "Value"))
	for _, want := range []string{"aws secretsmanager get-secret-value", "--region", "ClaimTokenSecret", "--query SecretString", "--output text"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("ClaimTokenCommand lacks %q: %s", want, cmd)
		}
	}
	if c := fmt.Sprint(get(t, outs["ConnectCommand"], "Value")); !strings.Contains(c, "aws ssm start-session") {
		t.Errorf("ConnectCommand: %s", c)
	}
	// The dashboard and the API must resolve on the sslip.io default as well as on the domain.
	for _, k := range []string{"DashboardUrl", "ApiUrl", "ClaimUrl"} {
		v := fmt.Sprint(get(t, outs[k], "Value"))
		if !strings.Contains(v, "sslip.io") || !strings.Contains(v, "DomainName") {
			t.Errorf("%s must cover the domain and the sslip.io default: %s", k, v)
		}
	}
}

func resources(t *testing.T, d doc) doc { return get(t, d, "Resources").(doc) }

func TestDataIsNeverDeletedSilently(t *testing.T) {
	d := load(t)
	r := resources(t, d)
	for _, c := range []struct{ name, policy string }{
		{"BackupBucket", "Retain"},
		{"DataVolume", "Snapshot"},
	} {
		for _, attr := range []string{"DeletionPolicy", "UpdateReplacePolicy"} {
			got := get(t, r[c.name], attr)
			if got != c.policy && got != "Retain" {
				t.Errorf("%s %s = %v, want %s (or Retain)", c.name, attr, got, c.policy)
			}
		}
	}
	// Nothing else may be retained or snapshotted by accident, and nothing that holds data may
	// be left without a policy: list every resource type that stores data.
	for name, res := range r {
		typ := get(t, res, "Type").(string)
		switch typ {
		case "AWS::S3::Bucket", "AWS::EC2::Volume", "AWS::RDS::DBInstance", "AWS::EFS::FileSystem":
			if !has(res, "DeletionPolicy") || !has(res, "UpdateReplacePolicy") {
				t.Errorf("%s (%s) holds data and needs DeletionPolicy and UpdateReplacePolicy", name, typ)
			}
		}
	}
	vol := get(t, r["DataVolume"], "Properties")
	if get(t, vol, "Encrypted") != true {
		t.Error("the data volume must be encrypted")
	}
	if !has(vol, "SnapshotId") {
		t.Error("the data volume must accept DataSnapshotId (restore path)")
	}
	bucket := get(t, r["BackupBucket"], "Properties")
	if get(t, bucket, "VersioningConfiguration", "Status") != "Enabled" {
		t.Error("the backup bucket must be versioned")
	}
	for _, k := range []string{"BlockPublicAcls", "BlockPublicPolicy", "IgnorePublicAcls", "RestrictPublicBuckets"} {
		if get(t, bucket, "PublicAccessBlockConfiguration", k) != true {
			t.Errorf("backup bucket: %s must be true", k)
		}
	}
	if !has(bucket, "BucketEncryption") {
		t.Error("the backup bucket must be encrypted")
	}
	// The bucket must not name itself: a fixed name would collide when the stack is made again
	// while the retained bucket still exists.
	if has(bucket, "BucketName") {
		t.Error("the backup bucket must have a generated name (a retained bucket would block a new stack)")
	}
	// A fixed secret name stays reserved for the recovery window after the stack is deleted.
	if has(r["ClaimTokenSecret"], "Properties", "Name") {
		t.Error("ClaimTokenSecret must have a generated name (a deleted secret's name is reserved for 30 days)")
	}
	pol := fmt.Sprint(get(t, r["BackupBucketPolicy"], "Properties", "PolicyDocument"))
	if !strings.Contains(pol, "aws:SecureTransport") {
		t.Error("the bucket policy must deny non-TLS access")
	}
}

func statements(t *testing.T, policyDoc any) []any {
	t.Helper()
	s := get(t, policyDoc, "Statement")
	if l, ok := s.([]any); ok {
		return l
	}
	return []any{s}
}

// TestInstanceRoleIsLeastPrivilege lists every action the instance role may take. A new line here
// is a decision, not an accident.
func TestInstanceRoleIsLeastPrivilege(t *testing.T) {
	d := load(t)
	r := resources(t, d)

	role := get(t, r["InstanceRole"], "Properties")
	if has(role, "ManagedPolicyArns") {
		t.Error("the role must carry no managed policies (AmazonSSMManagedInstanceCore reads every Parameter Store value)")
	}
	trust := get(t, role, "AssumeRolePolicyDocument")
	for _, st := range statements(t, trust) {
		if svc := get(t, st, "Principal", "Service"); svc != "ec2.amazonaws.com" {
			t.Errorf("the role may be assumed by %v, want ec2.amazonaws.com only", svc)
		}
	}

	// Every IAM statement of the stack: role inline policies and AWS::IAM::Policy resources.
	type stmt struct {
		where   string
		actions []string
		res     string
		cond    bool
	}
	var all []stmt
	collect := func(where string, pd any) {
		for _, st := range statements(t, pd) {
			if get(t, st, "Effect") != "Allow" {
				t.Errorf("%s: only Allow statements expected", where)
			}
			if has(st, "NotAction") || has(st, "NotResource") || has(st, "Principal") {
				t.Errorf("%s: NotAction, NotResource and Principal are not allowed", where)
			}
			all = append(all, stmt{where, strList(t, get(t, st, "Action")), fmt.Sprint(get(t, st, "Resource")), has(st, "Condition")})
		}
	}
	for _, p := range get(t, role, "Policies").([]any) {
		collect("role/"+get(t, p, "PolicyName").(string), get(t, p, "PolicyDocument"))
	}
	var policyNames []string
	for name, res := range r {
		if get(t, res, "Type") != "AWS::IAM::Policy" {
			continue
		}
		policyNames = append(policyNames, name)
		if ref := fmt.Sprint(get(t, res, "Properties", "Roles")); !strings.Contains(ref, "InstanceRole") {
			t.Errorf("%s is attached to something other than InstanceRole: %s", name, ref)
		}
		collect(name, get(t, res, "Properties", "PolicyDocument"))
	}
	sort.Strings(policyNames)
	if want := []string{"DnsPolicy", "KeyEscrowPolicy", "SessionManagerPolicy"}; !reflect.DeepEqual(policyNames, want) {
		t.Errorf("IAM policy resources are %v, want %v", policyNames, want)
	}
	for _, name := range policyNames {
		if !has(r[name], "Condition") {
			t.Errorf("%s must be conditional (it is optional)", name)
		}
	}

	allowed := map[string]bool{
		// the backup bucket
		"s3:ListBucket": true, "s3:GetBucketLocation": true, "s3:ListBucketMultipartUploads": true,
		"s3:GetObject": true, "s3:PutObject": true, "s3:DeleteObject": true,
		"s3:AbortMultipartUpload": true, "s3:ListMultipartUploadParts": true,
		// the claim token (write) and the key escrow passphrase (read once, replace once)
		"secretsmanager:PutSecretValue": true, "secretsmanager:GetSecretValue": true,
		// DNS-01 certificates
		"route53:ChangeResourceRecordSets": true, "route53:ListResourceRecordSets": true, "route53:GetChange": true,
		// Session Manager channels
		"ssm:UpdateInstanceInformation":    true,
		"ssmmessages:CreateControlChannel": true, "ssmmessages:CreateDataChannel": true,
		"ssmmessages:OpenControlChannel": true, "ssmmessages:OpenDataChannel": true,
	}
	starOK := map[string]bool{ // actions that accept no narrower resource
		"ssm:UpdateInstanceInformation":    true,
		"ssmmessages:CreateControlChannel": true, "ssmmessages:CreateDataChannel": true,
		"ssmmessages:OpenControlChannel": true, "ssmmessages:OpenDataChannel": true,
	}
	for _, s := range all {
		for _, a := range s.actions {
			if strings.Contains(a, "*") {
				t.Errorf("%s: wildcard action %s", s.where, a)
			}
			if !allowed[a] {
				t.Errorf("%s: unexpected action %s (add it here only when the node needs it)", s.where, a)
			}
			if strings.HasPrefix(a, "iam:") || strings.HasPrefix(a, "sts:") || strings.HasPrefix(a, "ec2:") || strings.HasPrefix(a, "kms:") {
				t.Errorf("%s: %s must not be granted to the instance", s.where, a)
			}
			if s.res == "*" && !starOK[a] {
				t.Errorf("%s: %s on Resource * (scope it)", s.where, a)
			}
			switch {
			case strings.HasPrefix(a, "s3:"):
				if !strings.Contains(s.res, "BackupBucket") {
					t.Errorf("%s: %s must be scoped to BackupBucket, got %s", s.where, a, s.res)
				}
			case strings.HasPrefix(a, "secretsmanager:"):
				switch {
				case strings.Contains(s.res, "ClaimTokenSecret"):
					if a != "secretsmanager:PutSecretValue" {
						t.Errorf("%s: %s on ClaimTokenSecret: the instance may only write it", s.where, a)
					}
				case strings.Contains(s.res, "KeyEscrowSecret"):
					if a != "secretsmanager:GetSecretValue" && a != "secretsmanager:PutSecretValue" {
						t.Errorf("%s: %s on KeyEscrowSecret: only Get and Put", s.where, a)
					}
				default:
					t.Errorf("%s: %s must name ClaimTokenSecret (put) or KeyEscrowSecret (get, put), got %s", s.where, a, s.res)
				}
			case a == "route53:ChangeResourceRecordSets":
				if !strings.Contains(s.res, "hostedzone/") || !strings.Contains(s.res, "HostedZoneId") {
					t.Errorf("%s: %s must name the one hosted zone, got %s", s.where, a, s.res)
				}
				if !s.cond {
					t.Errorf("%s: %s needs a condition on the record type and name", s.where, a)
				}
			case strings.HasPrefix(a, "route53:"):
				if s.res == "*" {
					t.Errorf("%s: %s must be scoped", s.where, a)
				}
			}
		}
	}

	// The DNS-01 statement may change only _acme-challenge TXT records of the domain.
	dns := fmt.Sprint(get(t, r["DnsPolicy"], "Properties", "PolicyDocument"))
	for _, want := range []string{"ChangeResourceRecordSetsRecordTypes", "TXT", "ChangeResourceRecordSetsNormalizedRecordNames", "_acme-challenge.${DomainName}", "_acme-challenge.*.${DomainName}"} {
		if !strings.Contains(dns, want) {
			t.Errorf("DnsPolicy lost %q", want)
		}
	}
}

// TestDailySnapshotsOfTheDataVolume pins the promises of the Data Lifecycle Manager policy: it
// selects the stack's own volume and nothing else, keeps DailySnapshotsKept snapshots, copies the
// volume's Name tag so that a snapshot can be passed back as DataSnapshotId, is switched off by 0,
// and runs under a role that only the dlm service can assume.
func TestDailySnapshotsOfTheDataVolume(t *testing.T) {
	d := load(t)
	r := resources(t, d)

	// The knob: 7 by default, 0 turns the policy and its role off.
	count := get(t, d, "Parameters", "DailySnapshotsKept")
	// A String with a pattern, so that the console refuses 7.5 and 00 (a Number would take them
	// and fail at the policy, and 00 would slip past the comparison with "0").
	if get(t, count, "Type") != "String" || get(t, count, "Default") != "7" {
		t.Errorf("DailySnapshotsKept must be a String with default \"7\": %v", count)
	}
	pat := regexp.MustCompile(fmt.Sprint(get(t, count, "AllowedPattern")))
	for _, ok := range []string{"0", "1", "7", "99", "100", "999", "1000"} {
		if !pat.MatchString(ok) {
			t.Errorf("DailySnapshotsKept must accept %q", ok)
		}
	}
	for _, bad := range []string{"", "00", "01", "7.5", "-1", "1001", "10000", "1e2", " 7", "7 ", "seven"} {
		if pat.MatchString(bad) {
			t.Errorf("DailySnapshotsKept must refuse %q (a schedule keeps a whole number from 1 to 1000)", bad)
		}
	}
	if cd, _ := get(t, count, "ConstraintDescription").(string); cd == "" {
		t.Error("DailySnapshotsKept needs a ConstraintDescription")
	}
	if eq := get(t, d, "Conditions", "DailySnapshotsOn", "Fn::Not").([]any)[0].(map[string]any)["Fn::Equals"].([]any); eq[1] != "0" {
		t.Errorf("DailySnapshotsOn must compare with the string \"0\", got %#v", eq[1])
	}
	if got, want := fmt.Sprint(get(t, d, "Conditions", "DailySnapshotsOn")), "map[Fn::Not:[map[Fn::Equals:[map[Ref:DailySnapshotsKept] 0]]]]"; got != want {
		t.Errorf("DailySnapshotsOn = %s, want %s", got, want)
	}
	iface := get(t, d, "Metadata", "AWS::CloudFormation::Interface", "ParameterGroups").([]any)
	var inStorage bool
	for _, g := range iface {
		if p := strs(t, get(t, g, "Parameters")); len(p) > 1 && p[0] == "InstanceType" {
			for _, n := range p {
				inStorage = inStorage || n == "DailySnapshotsKept"
			}
		}
	}
	if !inStorage {
		t.Error("DailySnapshotsKept belongs in the size and storage group, next to DataVolumeSize")
	}

	// The policy, and only one.
	var dlm []string
	for name, res := range r {
		if get(t, res, "Type") == "AWS::DLM::LifecyclePolicy" {
			dlm = append(dlm, name)
		}
	}
	if !reflect.DeepEqual(dlm, []string{"DataSnapshotPolicy"}) {
		t.Fatalf("lifecycle policies: %v, want [DataSnapshotPolicy]", dlm)
	}
	pol := r["DataSnapshotPolicy"]
	if get(t, pol, "Condition") != "DailySnapshotsOn" {
		t.Error("the policy must be conditional on DailySnapshotsOn (0 disables it)")
	}
	props := get(t, pol, "Properties")
	if get(t, props, "State") != "ENABLED" {
		t.Error("the policy must be ENABLED")
	}
	if desc := fmt.Sprint(get(t, props, "Description")); !regexp.MustCompile(`^map\[Fn::Sub:[0-9A-Za-z _$\{\}:-]+\]$`).MatchString(desc) {
		// The service accepts only letters, digits, spaces, underscores and hyphens.
		t.Errorf("the policy description may hold only letters, digits, spaces, _ and - after substitution: %s", desc)
	}
	if arn := fmt.Sprint(get(t, props, "ExecutionRoleArn")); arn != "map[Fn::GetAtt:[DataSnapshotRole Arn]]" {
		t.Errorf("ExecutionRoleArn = %s, want the stack's own DataSnapshotRole", arn)
	}
	details := get(t, props, "PolicyDetails")
	if got := strs(t, get(t, details, "ResourceTypes")); !reflect.DeepEqual(got, []string{"VOLUME"}) {
		t.Errorf("ResourceTypes = %v, want [VOLUME]", got)
	}
	if get(t, details, "PolicyType") != "EBS_SNAPSHOT_MANAGEMENT" {
		t.Errorf("PolicyType = %v", get(t, details, "PolicyType"))
	}

	// The target tag is on the data volume, and its value is unique to this stack.
	targets := get(t, details, "TargetTags").([]any)
	if len(targets) != 1 {
		t.Fatalf("want exactly one target tag, got %v", targets)
	}
	key, val := get(t, targets[0], "Key"), fmt.Sprint(get(t, targets[0], "Value"))
	if val != "map[Ref:AWS::StackId]" {
		t.Errorf("the target tag value is %s: it must be the stack ID, which no other stack shares", val)
	}
	found := false
	for _, tag := range get(t, r["DataVolume"], "Properties", "Tags").([]any) {
		if get(t, tag, "Key") == key && fmt.Sprint(get(t, tag, "Value")) == val {
			found = true
		}
	}
	if !found {
		t.Errorf("the data volume does not carry the policy's target tag %v=%s", key, val)
	}
	for name, res := range r {
		if name == "DataVolume" || get(t, res, "Type") != "AWS::EC2::Volume" {
			continue
		}
		t.Errorf("%s is another volume: the target tag must stay on the data volume alone", name)
	}

	// Daily, DailySnapshotsKept deep, with the volume's tags (its Name tag) on every snapshot.
	sched := get(t, details, "Schedules").([]any)
	if len(sched) != 1 {
		t.Fatalf("want one schedule, got %d", len(sched))
	}
	if get(t, sched[0], "CreateRule", "Interval") != 24 || get(t, sched[0], "CreateRule", "IntervalUnit") != "HOURS" {
		t.Errorf("the schedule must run every 24 hours: %v", get(t, sched[0], "CreateRule"))
	}
	if got := fmt.Sprint(get(t, sched[0], "RetainRule", "Count")); got != "map[Ref:DailySnapshotsKept]" {
		t.Errorf("retention = %s, want the DailySnapshotsKept parameter", got)
	}
	if get(t, sched[0], "CopyTags") != true {
		t.Error("CopyTags must be true: the snapshots carry the volume's Name tag")
	}
	named := false
	for _, tag := range get(t, r["DataVolume"], "Properties", "Tags").([]any) {
		named = named || get(t, tag, "Key") == "Name"
	}
	if !named {
		t.Error("the data volume needs its Name tag: the snapshots copy it")
	}

	// The role: assumed by the dlm service for this account's policies only, with the AWS managed
	// policy of that service and nothing inline.
	role := r["DataSnapshotRole"]
	if get(t, role, "Type") != "AWS::IAM::Role" || get(t, role, "Condition") != "DailySnapshotsOn" {
		t.Errorf("DataSnapshotRole must be a role that exists only while snapshots are on")
	}
	rp := get(t, role, "Properties")
	if has(rp, "Policies") || has(rp, "RoleName") {
		t.Error("DataSnapshotRole carries no inline policy and no fixed name")
	}
	managed := get(t, rp, "ManagedPolicyArns").([]any)
	if len(managed) != 1 || fmt.Sprint(managed[0]) != "map[Fn::Sub:arn:${AWS::Partition}:iam::aws:policy/service-role/AWSDataLifecycleManagerServiceRole]" {
		t.Errorf("the role must carry exactly the AWS managed policy AWSDataLifecycleManagerServiceRole: %v", managed)
	}
	sts := statements(t, get(t, rp, "AssumeRolePolicyDocument"))
	if len(sts) != 1 {
		t.Fatalf("the trust policy has %d statements, want 1", len(sts))
	}
	if get(t, sts[0], "Principal", "Service") != "dlm.amazonaws.com" || get(t, sts[0], "Action") != "sts:AssumeRole" {
		t.Errorf("only dlm.amazonaws.com may assume the role: %v", sts[0])
	}
	cond := fmt.Sprint(get(t, sts[0], "Condition"))
	for _, want := range []string{"aws:SourceAccount", "AWS::AccountId", "aws:SourceArn", ":dlm:", "AWS::Region", "policy/*"} {
		if !strings.Contains(cond, want) {
			t.Errorf("the trust policy lost %q (confused deputy protection): %s", want, cond)
		}
	}
	// The node itself gets nothing from this: no EC2 rights, and the instance profile holds only
	// the instance role.
	if got := fmt.Sprint(get(t, r["InstanceProfile"], "Properties", "Roles")); got != "[map[Ref:InstanceRole]]" {
		t.Errorf("InstanceProfile roles = %s", got)
	}

	// The output that lists the snapshots follows the same condition.
	out := get(t, d, "Outputs", "DataSnapshotsCommand")
	if get(t, out, "Condition") != "DailySnapshotsOn" || !strings.Contains(fmt.Sprint(get(t, out, "Value")), "ec2 describe-snapshots") {
		t.Errorf("DataSnapshotsCommand: %v", out)
	}
}

// userData returns the first-boot script with every ${...} of the template replaced by a word.
func userData(t *testing.T) string {
	t.Helper()
	sub := get(t, resources(t, load(t))["Instance"], "Properties", "UserData", "Fn::Base64", "Fn::Sub").([]any)
	script := strings.ReplaceAll(sub[0].(string), "${!", "${")
	return regexp.MustCompile(`\$\{[^}]+\}`).ReplaceAllString(script, "X")
}

func TestUserDataIsValidBash(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	cmd := exec.Command(bash, "-n")
	cmd.Stdin = strings.NewReader(userData(t))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the user data does not parse as bash: %v\n%s", err, out)
	}
}

// The data volume is mounted with XFS project quotas: they are what gives a project a disk size
// of its own (internal/diskquota). The fstab line is the one that survives a reboot, and the
// first mount reads it, so the option has to be in that line and not only on a mount command.
func TestDataVolumeIsMountedWithProjectQuotas(t *testing.T) {
	ud := userData(t)
	line := regexp.MustCompile(`(?m)^\s*echo "UUID=.* /var/lib/supavise xfs ([^ ]+) 0 2" >> /etc/fstab$`).FindStringSubmatch(ud)
	if line == nil {
		t.Fatal("user data has no fstab line for /var/lib/supavise")
	}
	opts := strings.Split(line[1], ",")
	for _, want := range []string{"prjquota", "nofail"} {
		found := false
		for _, o := range opts {
			found = found || o == want
		}
		if !found {
			t.Errorf("the fstab options %q lack %s", line[1], want)
		}
	}
	// The mount comes from the fstab line, so nothing may mount it first without the option.
	if strings.Contains(ud, "mount -o") && regexp.MustCompile(`mount -o [^\n]*/var/lib/supavise`).MatchString(ud) {
		t.Error("the data volume is mounted by a command that may leave out prjquota")
	}
}

// The snap fallback installs the AWS CLI to /snap/bin, which cloud-init's PATH does not hold. The
// claim-token write is the first use of the CLI after the install.
func TestUserDataCanRunAwsFromSnap(t *testing.T) {
	ud := userData(t)
	pathLine := regexp.MustCompile(`(?m)^\s*export PATH="\$PATH:/snap/bin"$`).FindStringIndex(ud)
	if pathLine == nil {
		t.Fatal(`user data must add /snap/bin to PATH (export PATH="$PATH:/snap/bin")`)
	}
	for _, later := range []string{"snap install aws-cli", "aws secretsmanager put-secret-value"} {
		if i := strings.Index(ud, later); i < 0 || i < pathLine[0] {
			t.Errorf("the PATH export must come before %q (at %d, export at %d)", later, i, pathLine[0])
		}
	}
}

// A repair of a restored volume issues no claim token. The secret must then say so instead of
// keeping the placeholder that promises one.
func TestUserDataReplacesTheTokenPlaceholderWhenNoneIsIssued(t *testing.T) {
	ud := userData(t)
	if n := strings.Count(ud, "aws secretsmanager put-secret-value"); n != 2 {
		t.Fatalf("want two writes of the claim secret (the token, or the note that none was issued), got %d", n)
	}
	tail := ud[strings.Index(ud, "step \"storing the claim token\""):]
	if !strings.Contains(tail, "No claim token was issued") || !strings.Contains(tail, "sudo -u supavise supavise claim token") || !strings.Contains(tail, "--force") {
		t.Errorf("the note must say no token was issued and how to get one:\n%s", tail)
	}
	// The note says an administrator works, so it is written only when the node is claimed. An
	// unclaimed node keeps its secret: that may be the only copy of a live token (the installer
	// keeps a hash), for example when the instance of the same stack is replaced.
	elif := strings.Index(tail, "elif ")
	note := strings.Index(tail, "No claim token was issued")
	if elif < 0 || elif > note || !strings.Contains(tail[elif:note], "supavise claim status") || !strings.Contains(tail[elif:note], "= claimed") {
		t.Errorf("the note must be gated on `supavise claim status` reporting claimed:\n%s", tail)
	}
	if regexp.MustCompile(`(?m)^\s*else\b`).MatchString(tail[:strings.Index(tail, "rm -f /root/supavise-install.sh")]) {
		t.Errorf("an unclaimed node with no new token must leave the secret alone, not take an else branch:\n%s", tail)
	}
}

// userDataWithKeyEscrow returns the user data as it reads when KeyEscrowPassphrase is given: the
// template's own pieces for the passphrase (the map values of the Fn::Sub) are put in place of
// ${KeyEscrowFetch} and ${KeyEscrowDone}, and the resource references inside them become words.
func userDataWithKeyEscrow(t *testing.T) string {
	t.Helper()
	sub := get(t, resources(t, load(t))["Instance"], "Properties", "UserData", "Fn::Base64", "Fn::Sub").([]any)
	script := strings.ReplaceAll(sub[0].(string), "${!", "${")
	vars := sub[1].(doc)
	for _, name := range []string{"KeyEscrowFetch", "KeyEscrowDone"} {
		branches := get(t, vars[name], "Fn::If").([]any)
		if branches[0] != "HasKeyEscrow" {
			t.Fatalf("%s must depend on HasKeyEscrow, got %v", name, branches[0])
		}
		script = strings.ReplaceAll(script, "${"+name+"}", get(t, branches[1], "Fn::Sub").(string))
	}
	return regexp.MustCompile(`\$\{[^}]+\}`).ReplaceAllString(script, "X")
}

// The key escrow passphrase reaches the installer unattended and nowhere else: not user data (which
// anyone who may describe the instance can read), not a command line (every process on the host
// reads those), not the bootstrap log.
func TestKeyEscrowPassphrase(t *testing.T) {
	d := load(t)
	r := resources(t, d)

	p := get(t, d, "Parameters", "KeyEscrowPassphrase")
	if get(t, p, "NoEcho") != true {
		t.Error("KeyEscrowPassphrase must be NoEcho")
	}
	if get(t, p, "Default") != "" {
		t.Error("KeyEscrowPassphrase is optional: its default is empty")
	}
	// The installer refuses fewer than 12 characters; the form should refuse them first.
	pat := regexp.MustCompile(get(t, p, "AllowedPattern").(string))
	for _, c := range []struct {
		in string
		ok bool
	}{{"", true}, {"short", false}, {"elevenchars", false}, {"twelve chars", true}, {strings.Repeat("x", 128), true}, {strings.Repeat("x", 129), false}} {
		if pat.MatchString(c.in) != c.ok {
			t.Errorf("AllowedPattern on %q (%d characters): got %v, want %v", c.in, len(c.in), !c.ok, c.ok)
		}
	}

	// Everything that exists for it is conditional, and the secret has a generated name.
	for _, name := range []string{"KeyEscrowSecret", "KeyEscrowPolicy"} {
		if get(t, r[name], "Condition") != "HasKeyEscrow" {
			t.Errorf("%s must exist only when a passphrase is given", name)
		}
	}
	if has(r["KeyEscrowSecret"], "Properties", "Name") {
		t.Error("KeyEscrowSecret must have a generated name (a deleted secret's name is reserved for 30 days)")
	}
	if ref := get(t, r["KeyEscrowSecret"], "Properties", "SecretString"); !reflect.DeepEqual(ref, doc{"Ref": "KeyEscrowPassphrase"}) {
		t.Errorf("KeyEscrowSecret holds %v, want the parameter", ref)
	}
	// The role may read and replace this secret and nothing else of it.
	pol := get(t, r["KeyEscrowPolicy"], "Properties", "PolicyDocument")
	sts := statements(t, pol)
	if len(sts) != 1 {
		t.Fatalf("KeyEscrowPolicy has %d statements, want 1", len(sts))
	}
	if got := strList(t, get(t, sts[0], "Action")); !reflect.DeepEqual(got, []string{"secretsmanager:GetSecretValue", "secretsmanager:PutSecretValue"}) {
		t.Errorf("KeyEscrowPolicy actions: %v", got)
	}
	if res := get(t, sts[0], "Resource"); !reflect.DeepEqual(res, doc{"Ref": "KeyEscrowSecret"}) {
		t.Errorf("KeyEscrowPolicy resource: %v, want only KeyEscrowSecret", res)
	}

	// The user data never names the parameter: not in the script, not in the pieces for it.
	raw, err := os.ReadFile("supavise.yaml")
	if err != nil {
		t.Fatal(err)
	}
	ud := get(t, r["Instance"], "Properties", "UserData")
	if strings.Contains(fmt.Sprint(ud), "map[Ref:KeyEscrowPassphrase]") || strings.Contains(fmt.Sprint(ud), "${KeyEscrowPassphrase}") {
		t.Errorf("the user data refers to the KeyEscrowPassphrase parameter, which would put the passphrase in it: %v", ud)
	}
	// The one place the parameter is used is the secret.
	uses := regexp.MustCompile(`!Ref KeyEscrowPassphrase`).FindAllIndex(raw, -1)
	if len(uses) != 2 { // the Equals of HasKeyEscrow and the SecretString
		t.Errorf("KeyEscrowPassphrase is referenced %d times, want 2 (HasKeyEscrow, KeyEscrowSecret)", len(uses))
	}

	// What the script does with it.
	for _, name := range []string{"without a passphrase", "with a passphrase"} {
		script := userData(t)
		if name == "with a passphrase" {
			script = userDataWithKeyEscrow(t)
		}
		if regexp.MustCompile(`(?m)^\s*set\s+-[a-z]*x|xtrace`).MatchString(script) {
			t.Errorf("%s: the script must not trace its commands (the passphrase would reach the log)", name)
		}
		if strings.Contains(script, "--key-passphrase ") || strings.Contains(script, "--passphrase ") {
			t.Errorf("%s: the passphrase must go in a file, not on a command line", name)
		}
	}
	if strings.Contains(userData(t), "key-passphrase-file") {
		t.Error("without a passphrase the installer must not be given --key-passphrase-file")
	}
	ud2 := userDataWithKeyEscrow(t)
	for _, want := range []string{
		"umask 077",              // the file is created root-only
		"> /root/key-passphrase", // stdout of the read goes to the file, not to the log
		"--key-passphrase-file /root/key-passphrase", // the installer reads the file
		"ESCROWED:", // a replaced instance does not escrow under the note
		"shred -u /root/key-passphrase",
	} {
		if !strings.Contains(ud2, want) {
			t.Errorf("the user data with a passphrase lacks %q", want)
		}
	}
	// The read comes before the installer, the replacement after it, and the file is also removed
	// when the script ends any other way.
	read := strings.Index(ud2, "get-secret-value")
	install := strings.Index(ud2, `bash /root/supavise-install.sh "$@"`)
	replace := strings.Index(ud2, "--secret-string 'ESCROWED:")
	if read < 0 || install < read || replace < install {
		t.Errorf("order wrong: read %d, install %d, replace %d", read, install, replace)
	}
	if !regexp.MustCompile(`(?m)^trap '[^']*shred -u /root/key-passphrase[^']*' EXIT$`).MatchString(ud2) {
		t.Error("the passphrase file must be removed by an EXIT trap too")
	}
	// The read prints the secret to the file only: never to the log, which tee copies to the console.
	for _, line := range strings.Split(ud2, "\n") {
		if strings.Contains(line, "get-secret-value") && strings.Contains(line, "key-passphrase") && !strings.Contains(line, "> /root/key-passphrase") {
			t.Errorf("a read of the passphrase that is not sent to the file: %s", line)
		}
	}
	// The note that replaces the passphrase carries no part of it, and says what happened.
	if !strings.Contains(ud2, "nothing on the node keeps it") && !strings.Contains(ud2, "which nothing on the node keeps") {
		t.Error("the replacement note should say that nothing on the node keeps the passphrase")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	cmd := exec.Command(bash, "-n")
	cmd.Stdin = strings.NewReader(ud2)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the user data with a passphrase does not parse as bash: %v\n%s", err, out)
	}
}

// The README states what was checked and what was not. These phrases guard the two places where
// a claim was once stated as fact that nobody had run in AWS.
func TestReadmeStatesWhatIsNotChecked(t *testing.T) {
	raw, err := os.ReadFile("../README.md")
	if err != nil {
		t.Fatal(err)
	}
	readme := string(raw)
	for _, line := range strings.Split(readme, "\n") {
		if strings.Contains(line, "AmiId") && strings.Contains(line, "on an update") && strings.Contains(line, "CloudFormation") {
			if !strings.Contains(line, "may") || !strings.Contains(line, "not been checked") {
				t.Errorf("the README states the empty AmiId update behavior as fact: %s", line)
			}
		}
	}
	for _, want := range []string{
		"crash-consistent", "AWSDataLifecycleManagerServiceRole", "DailySnapshotsKept",
		"https://aws.amazon.com/ebs/pricing/", "supavise claim token --force", "Stop the instance",
	} {
		if !strings.Contains(readme, want) {
			t.Errorf("the README lacks %q", want)
		}
	}
}

func TestNetworkAndInstanceHardening(t *testing.T) {
	d := load(t)
	r := resources(t, d)

	var ports []string
	for _, rule := range get(t, r["SecurityGroup"], "Properties", "SecurityGroupIngress").([]any) {
		ports = append(ports, fmt.Sprint(get(t, rule, "FromPort")))
		if from, to := get(t, rule, "FromPort"), get(t, rule, "ToPort"); from != to {
			t.Errorf("ingress %v-%v is a range", from, to)
		}
		if fmt.Sprint(get(t, rule, "CidrIp")) != "map[Ref:AccessCidr]" {
			t.Errorf("ingress on %v must use AccessCidr", get(t, rule, "FromPort"))
		}
	}
	sort.Strings(ports)
	if want := []string{"443", "5432", "6543", "80"}; !reflect.DeepEqual(ports, want) {
		t.Errorf("security group ports %v, want %v: SSH is a separate, conditional rule", ports, want)
	}
	ssh := r["SshIngress"]
	if get(t, ssh, "Condition") != "HasSsh" || fmt.Sprint(get(t, ssh, "Properties", "CidrIp")) != "map[Ref:SshCidr]" {
		t.Error("SSH must open only when SshCidr is given, from SshCidr")
	}
	if p := get(t, d, "Parameters", "SshCidr", "Default"); p != "" {
		t.Errorf("SshCidr defaults to %q: SSH must be off by default", p)
	}
	if p := get(t, d, "Parameters", "EnableSessionManager", "Default"); p != "true" {
		t.Errorf("Session Manager is the default way in, got %v", p)
	}

	inst := get(t, r["Instance"], "Properties")
	if get(t, inst, "MetadataOptions", "HttpTokens") != "required" {
		t.Error("the instance must require IMDSv2")
	}
	if get(t, inst, "MetadataOptions", "HttpPutResponseHopLimit") != 1 {
		t.Error("IMDS hop limit must be 1 (no containers or forwarded requests)")
	}
	for _, m := range get(t, inst, "BlockDeviceMappings").([]any) {
		if get(t, m, "Ebs", "Encrypted") != true {
			t.Error("the root volume must be encrypted")
		}
	}
	if !has(inst, "KeyName") || !strings.Contains(fmt.Sprint(get(t, inst, "KeyName")), "HasKeyName") {
		t.Error("KeyName must be optional (HasKeyName)")
	}
	if ud := fmt.Sprint(get(t, inst, "UserData")); !strings.Contains(ud, "--claim-token-file") || !strings.Contains(ud, "--firewall none") {
		t.Error("user data must write the claim token to a file and leave the firewall to the security group")
	}
	// The image is a plain ID when given, otherwise the Canonical Ubuntu 24.04 parameter of the
	// instance's architecture.
	img := fmt.Sprint(get(t, inst, "ImageId"))
	for _, want := range []string{"HasAmiId", "AmiId", "/aws/service/canonical/ubuntu/server/24.04/stable/current/", "ami-id", "InstanceTypes"} {
		if !strings.Contains(img, want) {
			t.Errorf("ImageId lost %q: %s", want, img)
		}
	}
}

func TestTemplateIsSelfContained(t *testing.T) {
	raw, err := os.ReadFile("supavise.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// aws cloudformation deploy sends the file in the request when it is below 51,200 bytes and
	// needs a bucket above it. deploy.sh does not ask for one.
	if len(raw) > 48000 {
		t.Errorf("supavise.yaml is %d bytes; deploy.sh needs it under 51,200 (keep a margin)", len(raw))
	}
	d := load(t)
	for name, res := range resources(t, d) {
		typ := get(t, res, "Type").(string)
		if typ == "AWS::CloudFormation::Stack" || strings.HasPrefix(typ, "AWS::Lambda::") || strings.HasPrefix(typ, "AWS::Serverless") || typ == "AWS::CloudFormation::CustomResource" || strings.HasPrefix(typ, "Custom::") {
			t.Errorf("%s is %s: the template must stay one file with no nested stacks, Lambda code or custom resources", name, typ)
		}
	}
	if has(d, "Transform") {
		t.Error("no Transform: the console must be able to upload the file as it is")
	}
	if strings.Contains(string(raw), "TemplateURL") {
		t.Error("no TemplateURL: no nested stacks")
	}
	// Outside the user data, a statically known place for supavise/supavise is the release download.
	if !strings.Contains(string(raw), "https://github.com/supavise/supavise/releases/") {
		t.Error("user data downloads install.sh from the GitHub release")
	}
}

func TestCheckovSkipsAreExplained(t *testing.T) {
	d := load(t)
	for name, res := range resources(t, d) {
		if !has(res, "Metadata", "checkov", "skip") {
			continue
		}
		skips := get(t, res, "Metadata", "checkov", "skip").([]any)
		for _, s := range skips {
			if c, _ := get(t, s, "comment").(string); len(c) < 20 {
				t.Errorf("%s: checkov skip %v needs a real comment", name, get(t, s, "id"))
			}
		}
	}
}

func TestRulesAndConditionsStayInStep(t *testing.T) {
	d := load(t)
	conds := get(t, d, "Conditions").(doc)
	raw, _ := os.ReadFile("supavise.yaml")
	for name := range conds {
		// Each condition is used at least once outside its own definition.
		if n := len(regexp.MustCompile(`\b`+name+`\b`).FindAllString(string(raw), -1)); n < 2 {
			t.Errorf("condition %s is never used", name)
		}
	}
	for _, rule := range []string{"ZoneNeedsDomain", "VpcNeedsSubnet"} {
		if !has(d, "Rules", rule, "Assertions") {
			t.Errorf("rule %s is gone", rule)
		}
	}
}

// TestReleaseAssetsStampTheTag runs the real deploy/release-assets.sh with a throwaway key and
// checks what a release attaches: the template carries the tag as its default release and the
// AWS deploy script travels with it.
func TestReleaseAssetsStampTheTag(t *testing.T) {
	// release-assets.sh signs with ed25519, which macOS's own LibreSSL lacks; use an OpenSSL 3 when
	// there is one (Homebrew), else skip. CI runs on Ubuntu, where it always runs.
	pathEnv := os.Getenv("PATH")
	ssl := ""
	for _, c := range []string{"openssl", "/opt/homebrew/opt/openssl@3/bin/openssl", "/usr/local/opt/openssl@3/bin/openssl"} {
		p, err := exec.LookPath(c)
		if err != nil {
			continue
		}
		if exec.Command(p, "genpkey", "-algorithm", "ed25519", "-out", filepath.Join(t.TempDir(), "k.pem")).Run() == nil {
			ssl = p
			break
		}
	}
	if ssl == "" {
		t.Skip("no openssl with ed25519 support")
	}
	pathEnv = filepath.Dir(ssl) + ":" + pathEnv
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not installed")
	}
	tmp := t.TempDir()
	run := func(name string, args ...string) {
		t.Helper()
		cmd := exec.Command(name, args...)
		cmd.Dir = tmp
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s %v: %v\n%s", name, args, err, out)
		}
	}
	run(ssl, "genpkey", "-algorithm", "ed25519", "-out", "priv.pem")
	run(ssl, "pkey", "-in", "priv.pem", "-pubout", "-out", "pub.pem")
	dist := filepath.Join(tmp, "dist")
	if err := os.MkdirAll(dist, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"supavise-linux-amd64", "supavise-linux-arm64"} {
		if err := os.WriteFile(filepath.Join(dist, f), []byte("not a binary"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	script, _ := filepath.Abs("../release-assets.sh")
	cmd := exec.Command("bash", script, dist, filepath.Join(tmp, "priv.pem"), filepath.Join(tmp, "pub.pem"))
	cmd.Env = append(os.Environ(), "PATH="+pathEnv, "SUPAVISE_RELEASE_TAG=v9.8.7-rc.1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("release-assets.sh: %v\n%s", err, out)
	}
	tpl, err := os.ReadFile(filepath.Join(dist, "supavise.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var root yaml.Node
	if err := yaml.Unmarshal(tpl, &root); err != nil {
		t.Fatalf("the stamped template does not parse: %v", err)
	}
	stamped := get(t, convert(t, &root), "Parameters", "SupaviseVersion", "Default")
	if stamped != "v9.8.7-rc.1" {
		t.Errorf("stamped default is %v, want the tag", stamped)
	}
	orig, _ := os.ReadFile("supavise.yaml")
	if d := len(tpl) - len(orig); d != len("v9.8.7-rc.1")-len("latest") {
		t.Errorf("stamping changed more than the default: size differs by %d", d)
	}
	for _, f := range []string{"supavise-aws-deploy.sh", "install.sh", "SHA256SUMS", "SHA256SUMS.sig", "supavise-release.json"} {
		if _, err := os.Stat(filepath.Join(dist, f)); err != nil {
			t.Errorf("release asset %s missing: %v", f, err)
		}
	}
	// The manifest names the tag and the oldest version that upgrades to it, and the signed list
	// covers it: the signature check on SHA256SUMS is then a check on the manifest.
	manifest, err := os.ReadFile(filepath.Join(dist, "supavise-release.json"))
	if err != nil {
		t.Fatal(err)
	}
	var mf struct {
		Schema         int               `json:"schema"`
		Version        string            `json:"version"`
		MinUpgradeFrom string            `json:"min_upgrade_from"`
		Artifacts      map[string]string `json:"artifacts"`
	}
	if err := json.Unmarshal(manifest, &mf); err != nil {
		t.Fatalf("manifest: %v\n%s", err, manifest)
	}
	if mf.Schema != 1 || mf.Version != "v9.8.7-rc.1" || mf.MinUpgradeFrom == "" || mf.Artifacts["postgres"] == "" {
		t.Errorf("manifest: %+v", mf)
	}
	sums, _ := os.ReadFile(filepath.Join(dist, "SHA256SUMS"))
	sum := sha256.Sum256(manifest)
	if !strings.Contains(string(sums), hex.EncodeToString(sum[:])+"  supavise-release.json") {
		t.Errorf("SHA256SUMS does not list the manifest:\n%s", sums)
	}
	run(ssl, "pkeyutl", "-verify", "-rawin", "-pubin", "-inkey", "pub.pem", "-sigfile", filepath.Join(dist, "SHA256SUMS.sig"), "-in", filepath.Join(dist, "SHA256SUMS"))
	if fi, err := os.Stat(filepath.Join(dist, "supavise-aws-deploy.sh")); err == nil && fi.Mode()&0o111 == 0 {
		t.Error("supavise-aws-deploy.sh must be executable")
	}

	// Without a tag the template is attached as it is, and there is no manifest to name a version.
	cmd = exec.Command("bash", script, dist, filepath.Join(tmp, "priv.pem"), filepath.Join(tmp, "pub.pem"))
	cmd.Env = append(os.Environ(), "PATH="+pathEnv, "SUPAVISE_RELEASE_TAG=")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("release-assets.sh without a tag: %v\n%s", err, out)
	}
	tpl, _ = os.ReadFile(filepath.Join(dist, "supavise.yaml"))
	if string(tpl) != string(orig) {
		t.Error("without SUPAVISE_RELEASE_TAG the template must be copied unchanged")
	}
	if _, err := os.Stat(filepath.Join(dist, "supavise-release.json")); err == nil {
		t.Error("a run without a tag left a manifest of an earlier run in the release directory")
	}

	// A tag that is not a version is refused: it would end up in a parameter default.
	cmd = exec.Command("bash", script, dist, filepath.Join(tmp, "priv.pem"), filepath.Join(tmp, "pub.pem"))
	cmd.Env = append(os.Environ(), "PATH="+pathEnv, "SUPAVISE_RELEASE_TAG=latest; echo hi")
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Errorf("a bad tag must fail, got success:\n%s", out)
	}
}
