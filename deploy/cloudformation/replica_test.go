package cloudformation_test

// The parts of infrastructure revision 2 that serve replicas and failover: the Storage role, the
// mesh port rules, the fencing permissions, the tags the node reads, and the replica server stack
// (the same template with JoinLeader given).

import (
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/supavise/supavise/internal/infra"
)

// clusterTag is the value the template gives supavise:cluster: ClusterName, else the stack name.
const clusterTag = "map[Fn::If:[HasClusterName map[Ref:ClusterName] map[Ref:AWS::StackName]]]"

func TestInfraRevisionIsOneNumber(t *testing.T) {
	d := load(t)
	want := fmt.Sprint(infra.Current)
	if got := fmt.Sprint(get(t, d, "Outputs", "InfraRevision", "Value")); got != want {
		t.Errorf("output InfraRevision is %s, internal/infra says this release needs %s", got, want)
	}
	found := false
	for _, tag := range get(t, resources(t, d)["Instance"], "Properties", "Tags").([]any) {
		if m, ok := tag.(doc); ok && m["Key"] == "supavise:infra" {
			found = true
			if fmt.Sprint(m["Value"]) != want {
				t.Errorf("tag supavise:infra is %v, want %s", m["Value"], want)
			}
		}
	}
	if !found {
		t.Error("the instance has no supavise:infra tag")
	}
	// The script finds the revision with a line match; keep the template in the shape it expects.
	raw, err := os.ReadFile("supavise.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`(?m)^  InfraRevision:\n    Description: .*\n    Value: "` + want + `"$`).Match(raw) {
		t.Error(`the InfraRevision output must read:  InfraRevision / Description / Value: "N"  (deploy/aws/deploy.sh reads it)`)
	}
}

// The instance carries what internal/infra reads from the metadata service, and the metadata
// service is told to show it.
func TestInstanceTagsForTheNode(t *testing.T) {
	d := load(t)
	r := resources(t, d)
	inst := get(t, r["Instance"], "Properties")
	if get(t, inst, "MetadataOptions", "InstanceMetadataTags") != "enabled" {
		t.Error("MetadataOptions.InstanceMetadataTags must be enabled: the node reads its stack from the instance tags")
	}
	tags := map[string]string{}
	for _, tag := range get(t, inst, "Tags").([]any) {
		// An entry that is an Fn::If (the replica server's leader tag) has no key of its own.
		if m, ok := tag.(doc); ok && m["Key"] != nil {
			tags[fmt.Sprint(m["Key"])] = fmt.Sprint(m["Value"])
		}
	}
	for key, want := range map[string]string{
		"supavise:cluster":      clusterTag,
		"supavise:stack-name":   "map[Ref:AWS::StackName]",
		"supavise:eip":          "map[Fn::GetAtt:[ElasticIp AllocationId]]",
		"supavise:storage-role": "map[Fn::If:[IsJoiner map[Ref:StorageRoleArn] map[Fn::GetAtt:[StorageRole Arn]]]]",
	} {
		if tags[key] != want {
			t.Errorf("tag %s = %s, want %s", key, tags[key], want)
		}
	}
	for _, key := range []string{"Name", "supavise:infra", "supavise:caps"} {
		if _, ok := tags[key]; !ok {
			t.Errorf("tag %s is missing", key)
		}
	}
	// IMDS accepts tag keys of letters, digits and + - = . , _ : @ only, and refuses the instance
	// otherwise.
	ok := regexp.MustCompile(`^[A-Za-z0-9+=.,_:@-]+$`)
	for key := range tags {
		if !ok.MatchString(key) {
			t.Errorf("tag key %q cannot be shown in instance metadata", key)
		}
	}
	// The same cluster tag is on the Elastic IP and on the role: the failover permissions test
	// the first, the Storage role trust tests the second.
	for _, c := range []struct{ res, key string }{{"ElasticIp", "Tags"}, {"InstanceRole", "Tags"}} {
		found := false
		for _, tag := range get(t, r[c.res], "Properties", c.key).([]any) {
			if m, ok := tag.(doc); ok && m["Key"] == "supavise:cluster" {
				found = fmt.Sprint(m["Value"]) == clusterTag
			}
		}
		if !found {
			t.Errorf("%s lacks the supavise:cluster tag %s", c.res, clusterTag)
		}
	}
	// A stack with every feature off lists the capabilities every stack has; the optional ones are
	// added as the features are turned on, in the order internal/infra reads them.
	caps := func(over map[string]string) string {
		over["AdminEmail"] = "a@b.co"
		for _, tag := range get(t, render(t, d, over).res["Instance"], "Properties", "Tags").([]any) {
			if m, ok := tag.(doc); ok && m["Key"] == "supavise:caps" {
				join := get(t, m["Value"], "Fn::Join").([]any)
				var out []string
				for _, part := range join[1].([]any) {
					out = append(out, part.(string))
				}
				return strings.Join(out, join[0].(string))
			}
		}
		t.Fatal("no supavise:caps tag")
		return ""
	}
	for _, c := range []struct {
		over map[string]string
		want string
	}{
		{map[string]string{}, "tags,storage-role"},
		{map[string]string{"Failover": "on", "PeerCidr2": "203.0.113.4/32"}, "tags,storage-role,fencing,peer-rule"},
		{map[string]string{"PeerCidr3": "203.0.113.4/32"}, "tags,storage-role,peer-rule"},
	} {
		if got := caps(c.over); got != c.want {
			t.Errorf("capabilities with %v: %q, want %q", c.over, got, c.want)
		}
	}
	jo := joinerParams()
	delete(jo, "AdminEmail")
	if got := caps(jo); got != "tags,storage-role,replica-server" {
		t.Errorf("capabilities of a replica server: %q", got)
	}
	// internal/infra knows every name the template can write.
	known := map[string]bool{}
	for _, c := range infra.Capabilities() {
		known[c.Name] = true
	}
	for _, n := range []string{"tags", "storage-role", "fencing", "peer-rule", "replica-server"} {
		if !known[n] {
			t.Errorf("the template can write the capability %q and internal/infra does not know it", n)
		}
	}
}

// TestStorageRole pins decision 2: Storage gets short-lived credentials from a role, and no IAM
// user, access key or stored secret exists for the objects bucket.
func TestStorageRole(t *testing.T) {
	d := load(t)
	r := resources(t, d)
	for name, res := range r {
		switch typ := get(t, res, "Type").(string); typ {
		case "AWS::IAM::User", "AWS::IAM::AccessKey", "AWS::IAM::Group", "AWS::IAM::ManagedPolicy":
			t.Errorf("%s is %s: the stack makes no IAM user, key or managed policy", name, typ)
		}
	}
	for _, gone := range []string{"StorageUser", "StorageAccessKey", "StorageKeySecret", "StorageKeyPolicy"} {
		if _, ok := r[gone]; ok {
			t.Errorf("%s: Storage reaches its bucket through StorageRole, with no stored key", gone)
		}
	}
	if has(get(t, d, "Outputs"), "StorageKeySecretArn") {
		t.Error("output StorageKeySecretArn belongs to the design that stored a key; StorageRoleArn replaces it")
	}

	role := r["StorageRole"]
	if get(t, role, "Condition") != "NotJoiner" || has(role, "Properties", "RoleName") {
		t.Error("StorageRole exists only in the leader's stack and has a generated name")
	}
	rp := get(t, role, "Properties")
	if has(rp, "ManagedPolicyArns") {
		t.Error("StorageRole carries no managed policies")
	}
	trust := statements(t, get(t, rp, "AssumeRolePolicyDocument"))
	if len(trust) != 2 {
		t.Fatalf("StorageRole trust has %d statements, want 2 (the instance role by ARN, the cluster's roles by tag)", len(trust))
	}
	for _, st := range trust {
		if get(t, st, "Effect") != "Allow" || get(t, st, "Action") != "sts:AssumeRole" {
			t.Errorf("trust statement: %v", st)
		}
		if has(st, "Principal", "Service") || has(st, "Principal", "Federated") {
			t.Errorf("only AWS principals may assume the Storage role: %v", st)
		}
	}
	if got := fmt.Sprint(get(t, trust[0], "Principal", "AWS")); got != "map[Fn::GetAtt:[InstanceRole Arn]]" {
		t.Errorf("the first principal is %s, want the stack's InstanceRole", got)
	}
	if has(trust[0], "Condition") {
		t.Error("the stack's own role needs no condition")
	}
	// The second statement trusts the account, and only for roles (not users) that carry the cluster's tag.
	if got := fmt.Sprint(get(t, trust[1], "Principal", "AWS")); got != "map[Fn::Sub:arn:${AWS::Partition}:iam::${AWS::AccountId}:root]" {
		t.Errorf("the second principal is %s, want this account's root", got)
	}
	cond := get(t, trust[1], "Condition").(doc)
	if len(cond) != 2 || fmt.Sprint(get(t, cond, "StringEquals")) != "map[aws:PrincipalTag/supavise:cluster:"+clusterTag+"]" {
		t.Errorf("the second statement must test the supavise:cluster tag of the caller and nothing else but its kind: %v", cond)
	}
	if got := fmt.Sprint(get(t, cond, "ArnLike")); got != "map[aws:PrincipalArn:map[Fn::Sub:arn:${AWS::Partition}:iam::${AWS::AccountId}:role/*]]" {
		t.Errorf("the second statement must admit roles of this account only, not users: %s", got)
	}

	// What the role may do: the bucket's objects and nothing else, and not another bucket.
	pols := rolePolicies(t, rp)
	if len(pols) != 1 {
		t.Fatalf("StorageRole has %d inline policies, want 1", len(pols))
	}
	sts := statements(t, get(t, pols[0], "PolicyDocument"))
	if len(sts) != 2 {
		t.Fatalf("the objects policy has %d statements, want 2", len(sts))
	}
	if got := strList(t, get(t, sts[0], "Action")); !reflect.DeepEqual(got, []string{"s3:ListBucket", "s3:GetBucketLocation"}) {
		t.Errorf("bucket actions: %v", got)
	}
	if got := strList(t, get(t, sts[1], "Action")); !reflect.DeepEqual(got, []string{"s3:GetObject", "s3:PutObject", "s3:DeleteObject", "s3:AbortMultipartUpload", "s3:ListMultipartUploadParts"}) {
		t.Errorf("object actions: %v", got)
	}
	for _, st := range sts {
		if res := fmt.Sprint(get(t, st, "Resource")); !strings.Contains(res, "ObjectsBucket") || strings.Contains(res, "BackupBucket") || strings.Contains(res, "*") && !strings.Contains(res, "/*") {
			t.Errorf("the Storage role must name ObjectsBucket and only it: %s", res)
		}
	}

	// The instance role may assume it, and the Storage role cannot be reached through the other way
	// round: the policy sits in its own resource so that neither role waits for the other.
	assume := statements(t, get(t, r["StorageAssumePolicy"], "Properties", "PolicyDocument"))
	if len(assume) != 1 || get(t, assume[0], "Action") != "sts:AssumeRole" {
		t.Fatalf("StorageAssumePolicy: %v", assume)
	}
	if res := fmt.Sprint(get(t, assume[0], "Resource")); res != "map[Fn::If:[IsJoiner map[Fn::Sub:${StorageRoleArn}] map[Fn::GetAtt:[StorageRole Arn]]]]" {
		t.Errorf("StorageAssumePolicy resource: %s", res)
	}
	if has(r["InstanceRole"], "Properties", "Policies") && strings.Contains(fmt.Sprint(get(t, r["InstanceRole"], "Properties", "Policies")), "StorageRole") {
		t.Error("InstanceRole must not name StorageRole inline: the two roles would wait for each other")
	}
	// The daemon hands Storage the credentials; the bucket and role are discoverable from the outputs.
	for _, k := range []string{"ObjectsBucket", "StorageRoleArn"} {
		if !has(get(t, d, "Outputs"), k) {
			t.Errorf("output %s is missing", k)
		}
	}
}

// The mesh port rules are separate resources and off until a range is given.
func TestPeerRules(t *testing.T) {
	d := load(t)
	r := resources(t, d)
	for i := 1; i <= 3; i++ {
		id, param := fmt.Sprintf("PeerIngress%d", i), fmt.Sprintf("PeerCidr%d", i)
		res := r[id]
		if get(t, res, "Type") != "AWS::EC2::SecurityGroupIngress" || get(t, res, "Condition") != fmt.Sprintf("HasPeerCidr%d", i) {
			t.Errorf("%s: %v", id, res)
		}
		p := get(t, res, "Properties")
		if get(t, p, "IpProtocol") != "tcp" || get(t, p, "FromPort") != 7443 || get(t, p, "ToPort") != 7443 {
			t.Errorf("%s must open tcp 7443 and nothing else", id)
		}
		if got := fmt.Sprint(get(t, p, "CidrIp")); got != "map[Ref:"+param+"]" {
			t.Errorf("%s opens %s", id, got)
		}
		if got := fmt.Sprint(get(t, p, "GroupId")); got != "map[Fn::GetAtt:[SecurityGroup GroupId]]" {
			t.Errorf("%s belongs to %s", id, got)
		}
		if def := get(t, d, "Parameters", param, "Default"); def != "" {
			t.Errorf("%s defaults to %q: the mesh port is closed unless asked for", param, def)
		}
		pat := regexp.MustCompile(fmt.Sprint(get(t, d, "Parameters", param, "AllowedPattern")))
		for in, ok := range map[string]bool{"": true, "203.0.113.4/32": true, "10.0.0.0/8": true, "10.0.0.0": false, "0.0.0.0/33": false, "a/32": false} {
			if pat.MatchString(in) != ok {
				t.Errorf("%s pattern on %q: got %v, want %v", param, in, !ok, ok)
			}
		}
	}
	// The group the instance is attached to keeps exactly the rules of v0.1.1.
	if n := len(get(t, r["SecurityGroup"], "Properties", "SecurityGroupIngress").([]any)); n != 4 {
		t.Errorf("SecurityGroup lists %d rules inline, want the 4 of v0.1.1 (peer rules are separate resources)", n)
	}
}

// TestFencingPolicy: the permissions are the design's, scoped by tag, and only with Failover=on.
func TestFencingPolicy(t *testing.T) {
	d := load(t)
	r := resources(t, d)
	f := get(t, d, "Parameters", "Failover")
	if get(t, f, "Default") != "off" || !reflect.DeepEqual(strs(t, get(t, f, "AllowedValues")), []string{"off", "on"}) {
		t.Errorf("Failover: %v", f)
	}
	if got := fmt.Sprint(get(t, d, "Conditions", "FailoverOn")); got != "map[Fn::Equals:[map[Ref:Failover] on]]" {
		t.Errorf("FailoverOn = %s", got)
	}
	pol := r["FencingPolicy"]
	if get(t, pol, "Condition") != "FailoverOn" {
		t.Error("FencingPolicy must exist only when Failover is on")
	}
	sts := statements(t, get(t, pol, "Properties", "PolicyDocument"))
	if len(sts) != 3 {
		t.Fatalf("FencingPolicy has %d statements, want 3", len(sts))
	}
	want := []struct {
		sid     string
		actions []string
		tagged  bool
	}{
		{"Describe", []string{"ec2:DescribeInstances", "ec2:DescribeInstanceStatus", "ec2:DescribeAddresses"}, false},
		{"StopClusterPeer", []string{"ec2:StopInstances"}, true},
		{"MoveServiceAddress", []string{"ec2:AssociateAddress", "ec2:DisassociateAddress"}, true},
	}
	for i, w := range want {
		st := sts[i]
		if get(t, st, "Sid") != w.sid {
			t.Errorf("statement %d is %v, want %s", i, get(t, st, "Sid"), w.sid)
		}
		if got := strList(t, get(t, st, "Action")); !reflect.DeepEqual(got, w.actions) {
			t.Errorf("%s actions: %v", w.sid, got)
		}
		res := fmt.Sprint(get(t, st, "Resource"))
		if w.tagged {
			cond := fmt.Sprint(get(t, st, "Condition"))
			if cond != "map[StringEquals:map[aws:ResourceTag/supavise:cluster:"+clusterTag+"]]" {
				t.Errorf("%s must be limited to resources that carry the cluster tag: %s", w.sid, cond)
			}
			if strings.Contains(res, "*:*") || !strings.Contains(res, "${AWS::AccountId}") {
				t.Errorf("%s must name this account's resources: %s", w.sid, res)
			}
		} else if res != "*" || has(st, "Condition") {
			t.Errorf("%s takes no resource and no condition: %s", w.sid, res)
		}
	}
	// Nothing in the template grants more of EC2 than this.
	for name, res := range r {
		if get(t, res, "Type") == "AWS::IAM::Policy" && name != "FencingPolicy" && strings.Contains(fmt.Sprint(get(t, res, "Properties", "PolicyDocument")), "ec2:") {
			t.Errorf("%s grants EC2 actions; only FencingPolicy may", name)
		}
	}
}

// render of the replica server stack: the same template with JoinLeader and its inputs given.
func joinerParams() map[string]string {
	return map[string]string{
		"AdminEmail": "owner@example.com", "JoinLeader": "203.0.113.50:7443",
		"JoinTokenSecretArn": "arn:aws:secretsmanager:eu-west-1:111122223333:secret:supavise-join-AbCdEf",
		"BackupBucketName":   "supavise-backupbucket-abc", "BackupBucketRegion": "us-east-1",
		"ObjectsBucketName": "supavise-objectsbucket-abc",
		"StorageRoleArn":    "arn:aws:iam::111122223333:role/supavise-StorageRole-ABC",
		"ClusterName":       "supavise",
	}
}

func TestReplicaServerStack(t *testing.T) {
	d := load(t)
	leader := render(t, d, map[string]string{"AdminEmail": "owner@example.com"})
	joiner := render(t, d, joinerParams())

	// What the leader's stack has and a replica server stack must not: the buckets, the Storage
	// role, the claim token.
	for _, id := range []string{"BackupBucket", "BackupBucketPolicy", "ObjectsBucket", "ObjectsBucketPolicy", "StorageRole", "ClaimTokenSecret"} {
		if _, ok := leader.res[id]; !ok {
			t.Errorf("%s is missing from an ordinary stack", id)
		}
		if _, ok := joiner.res[id]; ok {
			t.Errorf("%s exists in a replica server stack, which uses the leader's", id)
		}
	}
	// What it has instead.
	if _, ok := joiner.res["JoinTokenPolicy"]; !ok {
		t.Error("a replica server stack needs JoinTokenPolicy to read its token")
	}
	if _, ok := leader.res["JoinTokenPolicy"]; ok {
		t.Error("an ordinary stack must not read a join token")
	}
	for _, id := range []string{"Instance", "InstanceRole", "InstanceProfile", "DataVolume", "DataVolumeAttachment", "ElasticIp", "ElasticIpAssociation", "SecurityGroup", "InstallWait", "StorageAssumePolicy"} {
		if _, ok := joiner.res[id]; !ok {
			t.Errorf("a replica server stack needs its own %s", id)
		}
	}
	// It is a server of its own: own address, own volume, a second private address for the service
	// address, a different zone through AvailabilityZone.
	if got := np(joiner.res["Instance"])["NetworkInterfaces"].([]any)[0].(doc)["SecondaryPrivateIpAddressCount"]; got != 1 {
		t.Errorf("SecondaryPrivateIpAddressCount = %v, want 1", got)
	}
	if _, ok := np(leader.res["Instance"])["NetworkInterfaces"].([]any)[0].(doc)["SecondaryPrivateIpAddressCount"]; ok {
		t.Error("an ordinary stack takes no second private address (the instance would be replaced)")
	}

	// The role reads the leader's backup bucket by name and assumes the leader's Storage role.
	policies := map[string]string{}
	for _, p := range np(joiner.res["InstanceRole"])["Policies"].([]any) {
		policies[p.(doc)["PolicyName"].(string)] = canon(p)
	}
	if _, ok := policies["claim-token-secret"]; ok {
		t.Error("a replica server writes no claim token")
	}
	if bp := policies["backup-bucket"]; !strings.Contains(bp, "arn:${AWS::Partition}:s3:::${BackupBucketName}") || strings.Contains(bp, "GetAtt") {
		t.Errorf("the backup policy of a replica server must name the leader's bucket: %s", bp)
	}
	if got := canon(get(t, joiner.res["StorageAssumePolicy"], "Properties", "PolicyDocument", "Statement").([]any)[0].(doc)["Resource"]); got != `{"Fn::Sub":"${StorageRoleArn}"}` {
		t.Errorf("a replica server assumes the leader's role: %s", got)
	}
	if got := canon(get(t, joiner.res["JoinTokenPolicy"], "Properties", "PolicyDocument", "Statement").([]any)[0].(doc)["Resource"]); got != `{"Fn::Sub":"${JoinTokenSecretArn}"}` {
		t.Errorf("JoinTokenPolicy resource: %s", got)
	}
	// Its tags say what it is and where its role is.
	var keys []string
	for _, tag := range np(joiner.res["Instance"])["Tags"].([]any) {
		keys = append(keys, tag.(doc)["Key"].(string))
	}
	if !strings.Contains(strings.Join(keys, " "), "supavise:leader") {
		t.Errorf("a replica server stack's instance should carry supavise:leader: %v", keys)
	}
	// Its user data is the short script.
	ud := canon(np(joiner.res["Instance"])["UserData"])
	if !strings.Contains(ud, "--join-token-file") || strings.Contains(ud, "claim-token") || strings.Contains(ud, "mkfs.xfs") {
		t.Errorf("a replica server's user data is the join stub: %.200s", ud)
	}
	if !strings.Contains(canon(np(leader.res["Instance"])["UserData"]), "mkfs.xfs") {
		t.Error("an ordinary stack's user data is the install script")
	}

	// Outputs that name the leader's things show them in the replica server's stack.
	outs := get(t, d, "Outputs").(doc)
	for _, k := range []string{"BackupBucket", "ObjectsBucket", "StorageRoleArn"} {
		if v := fmt.Sprint(get(t, outs[k], "Value")); !strings.Contains(v, "Fn::If:[IsJoiner") {
			t.Errorf("output %s should show the leader's value in a replica server stack: %s", k, v)
		}
	}
	for _, k := range []string{"ClaimUrl", "ClaimTokenCommand", "ClaimTokenSecretArn"} {
		if get(t, outs[k], "Condition") != "NotJoiner" {
			t.Errorf("output %s needs the claim token, which a replica server does not have", k)
		}
	}
}

// The rules refuse a half-filled replica server and a replica server that would collide with the leader.
func TestReplicaRules(t *testing.T) {
	d := load(t)
	rules := get(t, d, "Rules").(doc)
	for _, name := range []string{"ReplicaNeedsItsInputs", "LeaderInputsNeedAReplica", "ZoneNeedsNewSubnet"} {
		if !has(rules, name, "Assertions") {
			t.Errorf("rule %s is gone", name)
		}
	}
	// Every input of a replica server is in both rules, so that none can be forgotten on one side.
	inputs := []string{"JoinTokenSecretArn", "BackupBucketName", "BackupBucketRegion", "ObjectsBucketName", "StorageRoleArn"}
	for _, name := range []string{"ReplicaNeedsItsInputs", "LeaderInputsNeedAReplica"} {
		text := fmt.Sprint(get(t, rules, name, "Assertions"))
		for _, in := range inputs {
			if !strings.Contains(text, in) {
				t.Errorf("rule %s does not mention %s", name, in)
			}
		}
	}
	text := fmt.Sprint(get(t, rules, "ReplicaNeedsItsInputs", "Assertions"))
	for _, in := range []string{"HostedZoneId", "KeyEscrowPassphrase"} {
		if !strings.Contains(text, in) {
			t.Errorf("a replica server must not take %s: the rule does not say so", in)
		}
	}
	// The pattern of each input accepts its value and the empty string.
	for name, val := range joinerParams() {
		if name == "AdminEmail" {
			continue
		}
		p := get(t, d, "Parameters", name).(doc)
		pat, ok := p["AllowedPattern"].(string)
		if !ok {
			continue
		}
		re := regexp.MustCompile(pat)
		if !re.MatchString(val) || !re.MatchString("") {
			t.Errorf("parameter %s: pattern %s must accept %q and the empty string", name, pat, val)
		}
	}
}

// TestReplicaUserData: the short script runs only what it has verified, and keeps the token off
// every command line and out of the log.
func TestReplicaUserData(t *testing.T) {
	script, vars := userDataSub(t, true)
	if n := strings.Count(script, "__SUPAVISE_RELEASE_PUBKEY_B64__"); n != 1 {
		t.Errorf("the stub holds the release key marker %d times, want once (deploy/release-assets.sh stamps it)", n)
	}
	ud := regexp.MustCompile(`\$\{[^}]+\}`).ReplaceAllString(script, "X")
	if bash, err := exec.LookPath("bash"); err == nil {
		cmd := exec.Command(bash, "-n")
		cmd.Stdin = strings.NewReader(ud)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("the replica user data does not parse as bash: %v\n%s", err, out)
		}
	}
	// The variables the script uses are the ones the map and the template provide.
	for _, name := range []string{"ReleaseBase", "VersionArgs"} {
		if _, ok := vars[name]; !ok {
			t.Errorf("the Fn::Sub variable %s is missing", name)
		}
		if !strings.Contains(script, "${"+name+"}") {
			t.Errorf("the script does not use ${%s}", name)
		}
	}
	// Order: verify the key, the signature and the checksum, then read the token, then run.
	order := []string{"openssl pkey -pubin", "openssl pkeyutl -verify -rawin", "sha256sum -c", "get-secret-value", "bash install.sh"}
	last := -1
	for _, step := range order {
		i := strings.Index(ud, step)
		if i < 0 || i < last {
			t.Errorf("%q is missing or out of order in the replica user data (want: %s)", step, strings.Join(order, ", then "))
		}
		last = i
	}
	for _, want := range []string{
		"apt-get install -y -qq xfsprogs ", // install.sh --aws-first-boot formats the data volume
		"--aws-first-boot", "--join-token-file /root/join-token", "--firewall none",
		"> /root/join-token)",       // the secret goes to the file, never to the log
		"umask 077",                 // root-only
		"shred -u /root/join-token", // removed whichever way the script ends
		"install.sh does not match", // the installer is checked against the signed list
		"grep -E '^[0-9a-f]{64} [ *]install\\.sh$' SHA256SUMS",
	} {
		if !strings.Contains(ud, want) {
			t.Errorf("the replica user data lacks %q", want)
		}
	}
	if regexp.MustCompile(`(?m)^\s*set\s+-[a-z]*x|xtrace`).MatchString(ud) {
		t.Error("the replica user data must not trace its commands")
	}
	if strings.Contains(ud, "--join-token ") || strings.Contains(ud, "--token ") {
		t.Error("the join token must go in a file, not on a command line")
	}
	// The token is not an input of the template: only the secret that holds it is.
	if strings.Contains(strings.ToLower(ud), "claim-token") {
		t.Error("a replica server has no claim token")
	}
	// It fits in the 16 KB EC2 allows, with the long values (the wait condition URL is the longest)
	// at a generous length.
	long := regexp.MustCompile(`\$\{[^}]+\}`).ReplaceAllString(script, strings.Repeat("x", 700))
	if len(long) > 16*1024 {
		t.Errorf("the replica user data is %d bytes with long values; EC2 allows 16384", len(long))
	}
}

// The release key reaches the stub only through deploy/release-assets.sh, and the marker is the one
// install.sh uses, so one sed replaces both.
func TestReleaseKeyMarker(t *testing.T) {
	raw, err := os.ReadFile("../install.sh")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "__SUPAVISE_RELEASE_PUBKEY_B64__") {
		t.Error("install.sh no longer carries the release key marker the replica stub shares")
	}
}
