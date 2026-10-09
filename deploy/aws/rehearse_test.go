package awsdeploy_test

// rehearse.sh against a stub `aws` and a stub deploy.sh. It runs the real script's logic (what it
// checks, what it always deletes) without touching AWS.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const stubRehearseAWS = `#!/bin/bash
DIR="$(dirname "$0")"
echo "$*" >> "$DIR/aws.log"
arg() { local k=$1; shift; while [ $# -gt 0 ]; do if [ "$1" = "$k" ]; then echo "$2"; return; fi; shift; done; }
case "$*" in
  *"cloudformation create-stack"*) p=$(arg --parameters "$@"); cp "${p#file://}" "$DIR/console-params.json" ;;
  *"ParameterKey=='AmiId'"*) if [ -f "$DIR/updated" ]; then echo ami-0bbbbbbbbbbbbbbbb; else echo; fi ;;
  *"ec2 describe-instances"*"ImageId"*) echo ami-0bbbbbbbbbbbbbbbb ;;
  *"describe-change-set"*) echo '{"Changes": [{"Type": "Resource", "ResourceChange": {"LogicalResourceId": "Instance"}}]}' ;;
  *"ssm get-parameter"*) echo ami-0aaaaaaaaaaaaaaaa ;;
  *"ec2 describe-images"*) printf 'ami-0aaaaaaaaaaaaaaaa\tami-0bbbbbbbbbbbbbbbb\tami-0cccccccccccccccc\tami-0dddddddddddddddd\n' ;;
  *"cloudformation update-stack"*) p=$(arg --parameters "$@"); cp "${p#file://}" "$DIR/denial-params.json"; touch "$DIR/denied" ;;
  *"Stacks[0].StackStatus"*) if [ -f "$DIR/denied" ]; then echo "${DENIAL_STATUS-UPDATE_ROLLBACK_COMPLETE}"; else echo CREATE_COMPLETE; fi ;;
  *"describe-stack-events"*"UPDATE_FAILED"*) printf 'Instance\tUPDATE_FAILED\tAction denied by stack policy: Statement [#2] has a Deny effect\n' ;;
  *"set-stack-policy"*) b=$(arg --stack-policy-body "$@"); k=$(grep -c 'set-stack-policy' "$DIR/aws.log"); cp "${b#file://}" "$DIR/policy-$k.json" ;;
  *"get-stack-policy"*)
    if [ -n "$POLICY_LEFT" ]; then echo '{"Statement": [{"Effect": "Allow", "Action": "Update:*", "Principal": "*", "Resource": "*"}, {"Effect": "Deny", "Action": "Update:Replace", "Principal": "*", "Resource": "*"}]}'
    else echo '{"Statement": [{"Effect": "Allow", "Action": "Update:*", "Principal": "*", "Resource": "*"}]}'; fi ;;
  *"OutputKey=='InstanceId'"*) if [ -f "$DIR/replaced" ]; then echo i-0newnewnewnew0001; else echo i-0123456789abcdef0; fi ;;
  *"OutputKey=='BackupBucket'"*) echo supavise-rehearsal-backup ;;
  *"OutputKey=='ObjectsBucket'"*) echo supavise-rehearsal-objects ;;
  *"OutputKey=='DataVolumeId'"*) echo vol-0123456789abcdef0 ;;
  *"OutputKey=='ElasticIpAllocationId'"*) echo eipalloc-0123456789abcdef0 ;;
  *"OutputKey=='InfraRevision'"*) echo "${REVISION-2}" ;;
  *"ec2 describe-instances"*) if [ -f "$DIR/rebooted" ]; then printf '2026-10-08T13:00:00+00:00\trunning\n'; else printf '2026-10-08T12:00:00+00:00\trunning\n'; fi ;;
  *"describe-stack-events"*)
    printf '2026-10-08T12:00:01+00:00\tCREATE_COMPLETE\n'
    if [ -f "$DIR/recreated" ]; then printf '2099-01-01T00:00:00+00:00\tCREATE_IN_PROGRESS\n'; fi ;;
  *"s3api list-object-versions"*)
    if [ -f "$DIR/emptied-$(arg --bucket "$@")" ]; then echo '{}'; else echo '{"Versions":[{"Key":"k","VersionId":"v1"}],"DeleteMarkers":[]}'; fi ;;
  *"s3api delete-objects"*) touch "$DIR/emptied-$(arg --bucket "$@")" ;;
  *"ec2 describe-snapshots"*) echo "snap-1 snap-2" ;;
esac
`

// stubRehearseDeploy stands for deploy.sh: it logs its arguments and, for the update, prints a
// review and does what the environment says (a second update changes nothing).
const stubRehearseDeploy = `#!/bin/bash
DIR="$(dirname "$0")"
echo "$*" >> "$DIR/deploy.log"
echo "${SUPAVISE_IMDS_ENDPOINT-unset}" >> "$DIR/imds.log"
case "$1" in
  __params) echo '[]' ;;
  __guard) echo '{"Statement": [{"Effect": "Allow", "Action": "Update:*", "Principal": "*", "Resource": "*"}, {"Effect": "Deny", "Action": "Update:Replace", "Principal": "*", "Resource": "*", "Condition": {"StringEquals": {"ResourceType": ["AWS::EC2::Instance"]}}}]}'
    echo '{"Statement": [{"Effect": "Allow", "Action": "Update:*", "Principal": "*", "Resource": "*"}]}' >"$5" ;;
  update)
    n=$(grep -c '^update' "$DIR/deploy.log")
    if [ "$n" -ge 2 ]; then echo "Nothing to change: the stack already matches this template."; exit 0; fi
    touch "$DIR/updated"
    echo "  add      ObjectsBucket (AWS::S3::Bucket)"
    if [ -n "$UPDATE_GUARDED" ]; then echo "  guarded  Instance (AWS::EC2::Instance) may be replaced: Tags, ImageId, MetadataOptions; ImageId is resolved when the change set runs"; fi
    if [ -n "$UPDATE_REPLACES" ]; then echo "  REPLACE  Instance (AWS::EC2::Instance) would be replaced: ImageId"; touch "$DIR/replaced" "$DIR/recreated"; fi
    if [ -n "$UPDATE_REBOOTS" ]; then touch "$DIR/rebooted"; fi
    exit "${UPDATE_RC-0}" ;;
esac
exit 0
`

func rehearse(t *testing.T, env []string, args ...string) (result, string) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("no python3")
	}
	dir := t.TempDir()
	for name, body := range map[string]string{"aws": stubRehearseAWS, "deploy.sh": stubRehearseDeploy} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	script, _ := filepath.Abs("rehearse.sh")
	cmd := exec.Command(bashes(t)[0], append([]string{script}, args...)...)
	cmd.Env = append([]string{"PATH=" + dir + ":/usr/bin:/bin", "HOME=" + t.TempDir(), "TMPDIR=" + t.TempDir(), "SUPAVISE_REHEARSE_DEPLOY=" + filepath.Join(dir, "deploy.sh")}, env...)
	cmd.Dir = dir // what it saves in the current directory lands here
	var so, se strings.Builder
	cmd.Stdout, cmd.Stderr = &so, &se
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return result{so.String(), se.String(), code}, dir
}

func fileLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func TestRehearseArguments(t *testing.T) {
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"--email", "a@b.co"}, "--region is required"},
		{[]string{"--region", "mars", "--email", "a@b.co"}, "not a region name"},
		{[]string{"--region", "us-east-1"}, "--email is required"},
		{[]string{"--region", "us-east-1", "--email", "x"}, "not an email address"},
		{[]string{"--region", "us-east-1", "--email", "a@b.co", "--keep", "--purge"}, "contradict"},
		{[]string{"--region", "us-east-1", "--email", "a@b.co", "--bogus"}, "unknown option"},
	} {
		r, dir := rehearse(t, nil, c.args...)
		if r.code != 2 || !strings.Contains(r.stderr, c.want) {
			t.Errorf("%v: exit %d, stderr %q, want exit 2 and %q", c.args, r.code, r.stderr, c.want)
		}
		if len(fileLines(t, filepath.Join(dir, "aws.log"))) != 0 || len(fileLines(t, filepath.Join(dir, "deploy.log"))) != 0 {
			t.Errorf("%v: a refused command called aws or deploy.sh", c.args)
		}
	}
}

func TestRehearseDryRun(t *testing.T) {
	r, dir := rehearse(t, nil, "--region", "us-east-1", "--email", "a@b.co", "--dry-run", "--purge")
	if r.code != 0 {
		t.Fatalf("exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	for _, want := range []string{
		"The rehearsal stack would be supavise-rehearsal-",
		"--version v0.1.1 --template ", "testdata/supavise-v0.1.1.yaml --instance-type t4g.medium --volume-size 20 --daily-snapshots 0",
		"update --region us-east-1 --stack supavise-rehearsal-", "--set Failover=on --set PeerCidr1=198.51.100.0/24 --yes",
		"--delete --yes", "with --purge",
	} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("the plan lacks %q:\n%s", want, r.stdout)
		}
	}
	if len(fileLines(t, filepath.Join(dir, "aws.log"))) != 0 || len(fileLines(t, filepath.Join(dir, "deploy.log"))) != 0 {
		t.Error("a dry run called aws or deploy.sh")
	}
}

// The old stack is the template of the tag, byte for byte, so the rehearsal makes the node that
// the live one is.
func TestRehearsalUsesTheTemplateOfTheTag(t *testing.T) {
	a, err := os.ReadFile("../cloudformation/testdata/supavise-v0.1.1.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(a), "Supavise: multi-project Supabase on one Ubuntu 24.04 instance") {
		t.Error("the v0.1.1 fixture is not the template")
	}
	// The fixture predates every revision 2 name.
	for _, name := range []string{"ObjectsBucket", "StorageRole", "InfraRevision", "FencingPolicy", "IsJoiner"} {
		if strings.Contains(string(a), name) {
			t.Errorf("the v0.1.1 fixture mentions %s", name)
		}
	}
}

func TestRehearsePasses(t *testing.T) {
	r, dir := rehearse(t, nil, "--region", "us-east-1", "--email", "a@b.co", "--yes")
	if r.code != 0 {
		t.Fatalf("exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	for _, want := range []string{"PASS  the update ran", "PASS  the change set holds no replacement of the Instance", "PASS  the instance is the same",
		"PASS  the launch time is the same", "PASS  no create or delete event for the Instance", "PASS  the stack is at infrastructure revision 2",
		"PASS  a second update finds nothing to change", "== every check passed", "supavise:infra=2", "associate-address --dry-run --allocation-id eipalloc-0123456789abcdef0",
		// What the script cannot see: a reboot keeps the launch time, and an update of the instance's tags may attach the address again.
		"uptime -s", "expect a time before 20", "associate-address --allocation-id eipalloc-0123456789abcdef0 --instance-id OTHER_INSTANCE --allow-reassociation",
		"describe-addresses --allocation-ids eipalloc-0123456789abcdef0", "expect OTHER_INSTANCE",
		// The call that names a private address (a replica server's service address is one), and the update the guard holds back.
		"associate-address --dry-run --allocation-id eipalloc-0123456789abcdef0 --instance-id i-0123456789abcdef0 --private-ip-address PRIVATE_IP --allow-reassociation",
		"an update of the instance can move it back", "aws cloudformation deploy --region"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("output lacks %q:\n%s", want, r.stdout)
		}
	}
	// create, update, update again, delete; in that order, each with the profile-free region.
	d := fileLines(t, filepath.Join(dir, "deploy.log"))
	if len(d) != 4 || !strings.Contains(d[0], "--version v0.1.1") || !strings.HasPrefix(d[1], "update ") || !strings.HasPrefix(d[2], "update ") || !strings.Contains(d[3], "--delete --yes") {
		t.Errorf("deploy.sh calls: %v", d)
	}
	if !strings.Contains(d[1], "--set Failover=on") {
		t.Errorf("the update did not turn the failover permissions on: %s", d[1])
	}
	// The stack is not this host's: deploy.sh must not read the metadata service of an EC2 host running this.
	for _, l := range fileLines(t, filepath.Join(dir, "imds.log")) {
		if l != "http://127.0.0.1:1" {
			t.Errorf("deploy.sh ran with SUPAVISE_IMDS_ENDPOINT=%q", l)
		}
	}
	// The buckets and snapshots are left, with the commands to remove them, unless --purge.
	if !strings.Contains(r.stdout, "Left in your account") || strings.Contains(strings.Join(fileLines(t, filepath.Join(dir, "aws.log")), "\n"), "delete-bucket") {
		t.Errorf("without --purge nothing is deleted but the stack:\n%s", r.stdout)
	}
}

func TestRehearseFailsWhenTheInstanceIsReplacedAndStillDeletesTheStack(t *testing.T) {
	r, dir := rehearse(t, []string{"UPDATE_REPLACES=1"}, "--region", "us-east-1", "--email", "a@b.co", "--yes")
	if r.code != 1 {
		t.Fatalf("exit %d, want 1\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	for _, want := range []string{"FAIL  the change set holds no replacement of the Instance", "FAIL  the instance is the same", "FAIL  no create or delete event for the Instance", "== at least one check FAILED"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("output lacks %q:\n%s", want, r.stdout)
		}
	}
	d := fileLines(t, filepath.Join(dir, "deploy.log"))
	if len(d) == 0 || !strings.Contains(d[len(d)-1], "--delete --yes") {
		t.Errorf("the stack must be deleted whatever the checks said: %v", d)
	}
}

func TestRehearseFailsWhenTheInstanceIsInterrupted(t *testing.T) {
	r, _ := rehearse(t, []string{"UPDATE_REBOOTS=1"}, "--region", "us-east-1", "--email", "a@b.co", "--yes")
	if r.code != 1 || !strings.Contains(r.stdout, "FAIL  the launch time is the same") {
		t.Errorf("a stop and start moves the launch time: exit %d\n%s", r.code, r.stdout)
	}
}

func TestRehearseFailsOnAnUpdateThatFails(t *testing.T) {
	r, _ := rehearse(t, []string{"UPDATE_RC=2"}, "--region", "us-east-1", "--email", "a@b.co", "--yes")
	if r.code != 1 || !strings.Contains(r.stdout, "FAIL  the update ran") {
		t.Errorf("exit %d\n%s", r.code, r.stdout)
	}
}

func TestRehearseKeepAndPurge(t *testing.T) {
	r, dir := rehearse(t, nil, "--region", "us-east-1", "--email", "a@b.co", "--keep")
	if r.code != 0 || !strings.Contains(r.stdout, "is kept; delete it with") {
		t.Fatalf("exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	for _, l := range fileLines(t, filepath.Join(dir, "deploy.log")) {
		if strings.Contains(l, "--delete") {
			t.Errorf("--keep deleted the stack: %s", l)
		}
	}
	r, dir = rehearse(t, nil, "--region", "us-east-1", "--email", "a@b.co", "--yes", "--purge")
	if r.code != 0 {
		t.Fatalf("exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	// --purge is deploy.sh's own cleanup, in the same call that deletes the stack; the rehearsal runs
	// no deletion of its own.
	d := fileLines(t, filepath.Join(dir, "deploy.log"))
	if len(d) == 0 || !strings.HasSuffix(d[len(d)-1], "--delete --yes --purge") {
		t.Errorf("--purge did not become deploy.sh --delete --purge: %v", d)
	}
	log := strings.Join(fileLines(t, filepath.Join(dir, "aws.log")), "\n")
	for _, bad := range []string{"delete-objects", "delete-bucket", "delete-snapshot"} {
		if strings.Contains(log, bad) {
			t.Errorf("the rehearsal ran %s itself:\n%s", bad, log)
		}
	}
	if strings.Contains(r.stdout, "Left in your account") {
		t.Errorf("--purge leaves nothing:\n%s", r.stdout)
	}
}

// --console-stack: the stack is made as the CloudFormation console makes it, with AmiId empty, so
// that the update has to pin the image and runs under deploy.sh's guard.
func TestRehearseConsoleStack(t *testing.T) {
	r, dir := rehearse(t, nil, "--region", "us-east-1", "--email", "a@b.co", "--dry-run", "--console-stack")
	if r.code != 0 {
		t.Fatalf("exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	for _, want := range []string{"AmiId is left empty", "cloudformation create-stack --stack-name supavise-rehearsal-", "--template-body file://",
		"testdata/supavise-v0.1.1.yaml --capabilities CAPABILITY_IAM", "wait stack-create-complete", "get-stack-policy --stack-name supavise-rehearsal-"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("the plan lacks %q:\n%s", want, r.stdout)
		}
	}
	if strings.Contains(r.stdout, "--version v0.1.1 --template") {
		t.Errorf("the console's stack is not made by deploy.sh:\n%s", r.stdout)
	}
	if len(fileLines(t, filepath.Join(dir, "aws.log"))) != 0 || len(fileLines(t, filepath.Join(dir, "deploy.log"))) != 0 {
		t.Error("a dry run called aws or deploy.sh")
	}

	r, dir = rehearse(t, []string{"UPDATE_GUARDED=1"}, "--region", "us-east-1", "--email", "a@b.co", "--yes", "--console-stack")
	if r.code != 0 {
		t.Fatalf("exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	for _, want := range []string{"the console's way, AmiId empty", "PASS  the update ran", "PASS  the instance is the same",
		"PASS  the stack was made with an empty AmiId", "PASS  the stack's AmiId is the image the instance runs (ami-0bbbbbbbbbbbbbbbb, instance: ami-0bbbbbbbbbbbbbbbb)",
		"PASS  the stack policy allows every update again", "INFO  the review called the instance: guarded  Instance (AWS::EC2::Instance) may be replaced",
		"== every check passed"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("output lacks %q:\n%s", want, r.stdout)
		}
	}
	// The stack is made with create-stack and the v0.1.1 template, and without AmiId; deploy.sh updates
	// it twice and deletes it.
	log := strings.Join(fileLines(t, filepath.Join(dir, "aws.log")), "\n")
	if !strings.Contains(log, "cloudformation create-stack --stack-name supavise-rehearsal-") || !strings.Contains(log, "supavise-v0.1.1.yaml --capabilities CAPABILITY_IAM") {
		t.Errorf("aws calls:\n%s", log)
	}
	b, err := os.ReadFile(filepath.Join(dir, "console-params.json"))
	if err != nil || strings.Contains(string(b), "AmiId") || !strings.Contains(string(b), `"ParameterKey": "AdminEmail", "ParameterValue": "a@b.co"`) ||
		!strings.Contains(string(b), `"ParameterKey": "SupaviseVersion", "ParameterValue": "v0.1.1"`) {
		t.Errorf("the parameters of the console's stack: %s (%v)", b, err)
	}
	// deploy.sh builds the parameters of the change set that is saved (as AWS gave it), updates the
	// stack twice and deletes it.
	d := fileLines(t, filepath.Join(dir, "deploy.log"))
	if len(d) != 4 || !strings.HasPrefix(d[0], "__params ") || !strings.Contains(d[0], " AmiId=ami-0bbbbbbbbbbbbbbbb Failover=on PeerCidr1=198.51.100.0/24") ||
		!strings.HasPrefix(d[1], "update ") || !strings.HasPrefix(d[2], "update ") || !strings.Contains(d[3], "--delete --yes") {
		t.Errorf("deploy.sh calls: %v", d)
	}
	if !strings.Contains(log, "cloudformation create-change-set --stack-name supavise-rehearsal-") || !strings.Contains(log, "--change-set-name rehearsal-capture --include-property-values --output json") ||
		!strings.Contains(log, "delete-change-set --stack-name supavise-rehearsal-") || strings.Contains(log, "update-stack") {
		t.Errorf("aws calls:\n%s", log)
	}
	if saved, _ := filepath.Glob(filepath.Join(dir, "rehearsal-changeset-supavise-rehearsal-*.json")); len(saved) != 1 || !strings.Contains(r.stdout, "INFO  the change set is saved in ") || !strings.Contains(r.stdout, filepath.Base(saved[0])) {
		t.Errorf("saved %v; output:\n%s", saved, r.stdout)
	} else if b, _ := os.ReadFile(saved[0]); !strings.Contains(string(b), `"LogicalResourceId": "Instance"`) {
		t.Errorf("the saved change set: %s", b)
	}

	// A guard left on the stack is a failed check.
	r, _ = rehearse(t, []string{"UPDATE_GUARDED=1", "POLICY_LEFT=1"}, "--region", "us-east-1", "--email", "a@b.co", "--yes", "--console-stack")
	if r.code != 1 || !strings.Contains(r.stdout, "FAIL  the stack policy allows every update again") {
		t.Errorf("exit %d\n%s", r.code, r.stdout)
	}
}

// --force-denial: the guard is set by hand and an update with another image must fail and roll
// back, before the update of the rehearsal.
func TestRehearseForceDenial(t *testing.T) {
	r, _ := rehearse(t, nil, "--region", "us-east-1", "--email", "a@b.co", "--force-denial")
	if r.code != 2 || !strings.Contains(r.stderr, "--force-denial needs --console-stack") {
		t.Errorf("exit %d\n%s", r.code, r.stderr)
	}
	r, _ = rehearse(t, nil, "--region", "us-east-1", "--email", "a@b.co", "--dry-run", "--console-stack", "--force-denial")
	for _, want := range []string{"__guard", "set-stack-policy --stack-name supavise-rehearsal-", "update-stack --stack-name supavise-rehearsal-", "UPDATE_FAILED"} {
		if r.code != 0 || !strings.Contains(r.stdout, want) {
			t.Errorf("exit %d: the plan lacks %q:\n%s", r.code, want, r.stdout)
		}
	}

	r, dir := rehearse(t, []string{"UPDATE_GUARDED=1"}, "--region", "us-east-1", "--email", "a@b.co", "--yes", "--console-stack", "--force-denial")
	if r.code != 0 {
		t.Fatalf("exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	for _, want := range []string{"PASS  CloudFormation took the update that needs another image",
		"PASS  the update failed and rolled back completely with the guard on (UPDATE_ROLLBACK_COMPLETE",
		"PASS  an event of the update names the stack policy", "Instance\tUPDATE_FAILED\tAction denied by stack policy",
		"PASS  the instance is the same after the denial", "PASS  the stack's policy is put back after the denial", "== every check passed"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("output lacks %q:\n%s", want, r.stdout)
		}
	}
	// The image is neither the instance's nor the current one; the guard comes first and the policy
	// it saved is put back after.
	d := fileLines(t, filepath.Join(dir, "deploy.log"))
	if len(d) < 3 || !strings.HasPrefix(d[1], "__guard ") || !strings.Contains(d[2], "AmiId=ami-0cccccccccccccccc") {
		t.Errorf("deploy.sh calls: %v", d)
	}
	log := fileLines(t, filepath.Join(dir, "aws.log"))
	set, upd := -1, -1
	for i, l := range log {
		if strings.Contains(l, "set-stack-policy") && set < 0 {
			set = i
		}
		if strings.Contains(l, "update-stack") {
			upd = i
		}
	}
	if set < 0 || upd < 0 || set > upd {
		t.Errorf("the guard is not set before the update:\n%s", strings.Join(log, "\n"))
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "policy-2.json")); !strings.Contains(string(b), `"Allow"`) || strings.Contains(string(b), "Deny") {
		t.Errorf("the policy put back: %s", b)
	}

	// A rollback that stops is reported, and resumed without the guard.
	r, _ = rehearse(t, []string{"UPDATE_GUARDED=1", "DENIAL_STATUS=UPDATE_ROLLBACK_FAILED"}, "--region", "us-east-1", "--email", "a@b.co", "--yes", "--console-stack", "--force-denial")
	if r.code != 1 || !strings.Contains(r.stdout, "FAIL  the update failed and rolled back completely with the guard on (UPDATE_ROLLBACK_COMPLETE; got: UPDATE_ROLLBACK_FAILED)") ||
		!strings.Contains(r.stdout, "resuming the rollback") {
		t.Errorf("exit %d\n%s", r.code, r.stdout)
	}
}
