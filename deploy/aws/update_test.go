package awsdeploy_test

// update, status and replica of deploy.sh against a stub `aws` that records its calls and answers
// from files, a fake metadata service and a release directory signed with a throwaway key. Nothing
// reaches AWS.

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const stubUpdate = `#!/bin/bash
DIR="$(dirname "$0")"
echo "$*" >> "$DIR/calls.log"
echo "${AWS_EC2_METADATA_DISABLED-unset}" >> "$DIR/imds.log"
arg() { local k=$1; shift; while [ $# -gt 0 ]; do if [ "$1" = "$k" ]; then echo "$2"; return; fi; shift; done; }
n=$(grep -c 'cloudformation create-change-set' "$DIR/calls.log")
case "$*" in
  *"sts get-caller-identity --query Arn"*) echo "${CALLER_ARN-arn:aws:iam::111122223333:user/admin}" ;;
  *"sts get-caller-identity --query Account"*) echo 111122223333 ;;
  *"cloudformation describe-stacks"*"--output json"*)
    f="$DIR/stack-$(arg --stack-name "$@").json"; [ -f "$f" ] || f="$DIR/stack.json"
    if [ ! -f "$f" ]; then echo "An error occurred (ValidationError) when calling the DescribeStacks operation: Stack with id x does not exist" >&2; exit 254; fi
    cat "$f" ;;
  *"cloudformation describe-stacks"*"Stacks[0].StackStatus"*) echo "${NEW_STACK_STATUS-CREATE_IN_PROGRESS}" ;;
  *"cloudformation describe-stack-resource"*) printf 'CREATE_COMPLETE\t203.0.113.77\n' ;;
  *"cloudformation create-change-set"*)
    p=$(arg --parameters "$@"); cp "${p#file://}" "$DIR/params-$n.json"
    t=$(arg --template-body "$@"); if [ -n "$t" ]; then cp "${t#file://}" "$DIR/template-$n.yaml"; fi
    u=$(arg --template-url "$@"); if [ -n "$u" ]; then cp "$DIR/s3-$(basename "$u")" "$DIR/template-$n.yaml"; fi
    if [ -n "$CREATE_FAILS" ]; then echo "An error occurred (ValidationError) when calling the CreateChangeSet operation: bad" >&2; exit 254; fi ;;
  *"cloudformation wait change-set-create-complete"*) exit "${CS_WAIT_RC-0}" ;;
  *"cloudformation describe-change-set"*"StatusReason"*) echo "${CS_REASON-}" ;;
  *"cloudformation describe-change-set"*)
    f="$DIR/changeset-$n.json"; [ -f "$f" ] || f="$DIR/changeset.json"; cat "$f" ;;
  *"cloudformation get-template"*)
    if [ -n "$GET_TEMPLATE_FAILS" ]; then echo "An error occurred (AccessDenied) when calling the GetTemplate operation" >&2; exit 254; fi
    python3 -c 'import json, sys; print(json.dumps(open(sys.argv[1]).read()))' "$DIR/template-$n.yaml" ;;
  *"cloudformation wait stack-update-complete"*) exit "${STACK_WAIT_RC-0}" ;;
  *"cloudformation wait stack-create-complete"*) exit "${CREATE_WAIT_RC-${STACK_WAIT_RC-0}}" ;;
  *"ec2 describe-addresses"*)
    if [ -n "$ADDRESS_FAILS" ]; then echo "An error occurred (UnauthorizedOperation)" >&2; exit 254; fi
    echo "${ADDRESS_HOLDER-i-0123456789abcdef0}" ;;
  *"ec2 describe-instances"*) echo ami-0bbbbbbbbbbbbbbbb ;;
  *"s3api head-bucket"*) exit "${HEAD_BUCKET_RC-254}" ;;
  *"s3api put-object"*)
    b=$(arg --body "$@"); k=$(arg --key "$@"); cp "$b" "$DIR/s3-$(basename "$k")"
    # What a swap of the object between the upload and the read would do; trailing white space is harmless.
    if [ -n "$S3_TAMPER" ]; then printf 'Resources:\n  Evil: { Type: "AWS::IAM::Role" }\n' >> "$DIR/s3-$(basename "$k")"; fi
    if [ -n "$S3_PAD" ]; then printf '\n\n  \n' >> "$DIR/s3-$(basename "$k")"; fi ;;
  *"ssm get-parameter"*) echo ami-0123456789abcdef0 ;;
  *"secretsmanager create-secret"*)
    v=$(arg --secret-string "$@"); cat "${v#file://}" > "$DIR/secret-value"
    echo arn:aws:secretsmanager:eu-west-1:111122223333:secret:supavise-join-AbCdEf ;;
  *) ;;
esac
`

// updFake is a directory with the stub on its path and the files it answers from.
type updFake struct {
	t   *testing.T
	dir string
	env []string
}

func newUpdFake(t *testing.T) *updFake {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("no python3")
	}
	f := &updFake{t: t, dir: t.TempDir()}
	if err := os.WriteFile(filepath.Join(f.dir, "aws"), []byte(stubUpdate), 0o755); err != nil {
		t.Fatal(err)
	}
	f.copy("stack-v011.json", "stack.json")
	f.copy("changeset-v011-to-rev2.json", "changeset.json")
	return f
}

// copy puts a testdata file in the stub's directory under the name the stub reads.
func (f *updFake) copy(from, to string) {
	f.t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", from))
	if err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.dir, to), b, 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *updFake) calls() []string { return calls(f.t, filepath.Join(f.dir, "calls.log")) }

func (f *updFake) callsMatching(sub string) []string {
	var out []string
	for _, c := range f.calls() {
		if strings.Contains(c, sub) {
			out = append(out, c)
		}
	}
	return out
}

func (f *updFake) first(sub string) int {
	for i, c := range f.calls() {
		if strings.Contains(c, sub) {
			return i
		}
	}
	return -1
}

// run runs deploy.sh with the stub on the path. The metadata service is unreachable unless the
// test sets SUPAVISE_IMDS_ENDPOINT; the release key and base URL are the test's when it sets them.
func (f *updFake) run(env []string, args ...string) result {
	f.t.Helper()
	script, _ := filepath.Abs("deploy.sh")
	cmd := exec.Command(bashes(f.t)[0], append([]string{script}, args...)...)
	base := []string{"PATH=" + f.dir + ":/usr/bin:/bin", "HOME=" + f.t.TempDir(), "TMPDIR=" + f.t.TempDir(), "SUPAVISE_IMDS_ENDPOINT=http://127.0.0.1:1"}
	cmd.Env = append(append(base, f.env...), env...)
	var so, se strings.Builder
	cmd.Stdout, cmd.Stderr = &so, &se
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		f.t.Fatal(err)
	}
	return result{so.String(), se.String(), code}
}

func (f *updFake) params(n int) []map[string]any {
	f.t.Helper()
	b, err := os.ReadFile(filepath.Join(f.dir, fmt.Sprintf("params-%d.json", n)))
	if err != nil {
		f.t.Fatalf("no parameters were given to change set %d: %v\ncalls: %v", n, err, f.calls())
	}
	var out []map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		f.t.Fatalf("parameters of change set %d: %v\n%s", n, err, b)
	}
	return out
}

func paramsByKey(ps []map[string]any) map[string]map[string]any {
	m := map[string]map[string]any{}
	for _, p := range ps {
		m[p["ParameterKey"].(string)] = p
	}
	return m
}

// smallTemplate is a template of a few bytes that has what the script reads of one: the
// parameters and the revision. It fits in an API call, so it goes in the request body.
func smallTemplate(t *testing.T) string { return smallTemplateAt(t, "2") }

func smallTemplateAt(t *testing.T, revision string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "small.yaml")
	body := `AWSTemplateFormatVersion: "2010-09-09"
Parameters:
  AdminEmail:
    Type: String
  InstanceType:
    Type: String
    Default: t4g.large
  SupaviseVersion:
    Type: String
    Default: latest
  KeyEscrowPassphrase:
    Type: String
    NoEcho: true
    Default: ""
  Failover:
    Type: String
    Default: "off"
  PeerCidr1:
    Type: String
    Default: ""
Resources: {}
Outputs:
  InfraRevision:
    Description: Revision.
    Value: "` + revision + `"
`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func realTemplate(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs("../cloudformation/supavise.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// ---- update -------------------------------------------------------------------------------

func TestUpdateKeepsPreviousValuesAndStagesALargeTemplate(t *testing.T) {
	f := newUpdFake(t)
	r := f.run(nil, "update", "--stack", "supavise", "--region", "us-east-1", "--template", realTemplate(t), "--yes",
		"--set", "Failover=on", "--set", "PeerCidr1=203.0.113.4/32")
	if r.code != 0 {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s\ncalls: %v", r.code, r.stdout, r.stderr, f.calls())
	}
	// Every parameter the stack has keeps its value, NoEcho ones included; the ones the stack
	// does not know take their defaults (they are not sent); --set gives the others.
	ps := paramsByKey(f.params(1))
	for _, k := range []string{"AdminEmail", "InstanceType", "DataVolumeSize", "DailySnapshotsKept", "SupaviseVersion", "KeyEscrowPassphrase",
		"DomainName", "HostedZoneId", "AccessCidr", "EnableSessionManager", "SshCidr", "KeyName", "VpcId", "SubnetId", "AmiId", "DataSnapshotId"} {
		p, ok := ps[k]
		if !ok || p["UsePreviousValue"] != true || p["ParameterValue"] != nil {
			t.Errorf("%s: %v, want UsePreviousValue", k, p)
		}
	}
	for k, want := range map[string]string{"Failover": "on", "PeerCidr1": "203.0.113.4/32"} {
		if p := ps[k]; p["ParameterValue"] != want || p["UsePreviousValue"] != nil {
			t.Errorf("%s: %v, want the value %s", k, p, want)
		}
	}
	for _, k := range []string{"ClusterName", "PeerCidr2", "JoinLeader", "StorageRoleArn", "AvailabilityZone"} {
		if _, ok := ps[k]; ok {
			t.Errorf("%s is new to the stack and must be left to its default, not sent", k)
		}
	}
	if len(ps) != 18 {
		t.Errorf("%d parameters sent: %v", len(ps), ps)
	}

	// The template is over the inline limit: it goes to the stack's backup bucket first, and
	// the change set reads it from there.
	cs := f.callsMatching("cloudformation create-change-set")
	if len(cs) != 1 {
		t.Fatalf("change sets: %v", cs)
	}
	for _, want := range []string{"--change-set-type UPDATE", "--capabilities CAPABILITY_IAM", "--stack-name supavise",
		"--template-url https://supavise-backupbucket-abc123.s3.us-east-1.amazonaws.com/_stack/"} {
		if !strings.Contains(cs[0], want) {
			t.Errorf("create-change-set lacks %q: %s", want, cs[0])
		}
	}
	if strings.Contains(cs[0], "--template-body") {
		t.Error("a template over 51,200 bytes cannot go in the body")
	}
	raw, _ := os.ReadFile(realTemplate(t))
	sum := sha256.Sum256(raw)
	// The object goes to the stack's bucket, which must be the account's: the owner is checked.
	if up := f.callsMatching("s3api put-object"); len(up) != 1 || !strings.Contains(up[0], "--bucket supavise-backupbucket-abc123 --key _stack/"+hex.EncodeToString(sum[:])+".yaml") ||
		!strings.Contains(up[0], "--expected-bucket-owner 111122223333") {
		t.Errorf("upload: %v", up)
	}
	// The order: the credentials are checked, the stack read, the change set made, reviewed and run.
	idx := func(s string) int { return f.first(s) }
	order := []string{"sts get-caller-identity", "describe-stacks", "ec2 describe-addresses", "s3api put-object", "create-change-set", "wait change-set-create-complete",
		"get-template --stack-name supavise --change-set-name", "describe-change-set", "execute-change-set", "wait stack-update-complete"}
	last := -1
	for _, o := range order {
		if i := idx(o); i < 0 || i < last {
			t.Errorf("%q is missing or out of order in %v", o, f.calls())
		} else {
			last = i
		}
	}
	// The instance role is never used: the metadata service is off for every call.
	b, _ := os.ReadFile(filepath.Join(f.dir, "imds.log"))
	for _, l := range strings.Fields(string(b)) {
		if l != "true" {
			t.Errorf("a call ran with AWS_EC2_METADATA_DISABLED=%q", l)
		}
	}
	for _, c := range f.calls() {
		if !strings.HasPrefix(c, "--region us-east-1 ") {
			t.Errorf("a call without the region: %s", c)
		}
	}
	if !strings.Contains(r.stdout, "add      ObjectsBucket (AWS::S3::Bucket)") || !strings.Contains(r.stdout, "modify   Instance (AWS::EC2::Instance): Tags, MetadataOptions (no interruption)") {
		t.Errorf("the change summary is missing:\n%s", r.stdout)
	}
}

func TestUpdateSendsASmallTemplateInTheBody(t *testing.T) {
	f := newUpdFake(t)
	r := f.run(nil, "update", "--stack", "supavise", "--region", "us-east-1", "--template", smallTemplate(t), "--yes")
	if r.code != 0 {
		t.Fatalf("exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	cs := f.callsMatching("create-change-set")
	if len(cs) != 1 || !strings.Contains(cs[0], "--template-body file://") || strings.Contains(cs[0], "--template-url") {
		t.Errorf("create-change-set: %v", cs)
	}
	if len(f.callsMatching("s3api put-object")) != 0 {
		t.Error("a small template needs no upload")
	}
	// The stack keeps what the template still declares; one the template dropped is only noted.
	ps := paramsByKey(f.params(1))
	if len(ps) != 4 || ps["SupaviseVersion"]["UsePreviousValue"] != true {
		t.Errorf("parameters: %v", ps)
	}
	if !strings.Contains(r.stderr, "the template no longer declares") {
		t.Errorf("stderr: %s", r.stderr)
	}
}

// A template staged in S3 is out of the script's hands until CloudFormation has read it, and the
// instance role may write to the backup bucket. The template the change set holds is read back and
// must be the file that was verified.
func TestUpdateRefusesATemplateThatChangedOnItsWayToCloudFormation(t *testing.T) {
	f := newUpdFake(t)
	r := f.run([]string{"S3_TAMPER=1"}, "update", "--stack", "supavise", "--region", "us-east-1", "--template", realTemplate(t), "--yes")
	if r.code != 2 || !strings.Contains(r.stderr, "is not the file that was verified") || !strings.Contains(r.stderr, "Nothing was changed") {
		t.Fatalf("exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	if len(f.callsMatching("delete-change-set")) != 1 || len(f.callsMatching("execute-change-set")) != 0 || len(f.callsMatching("describe-change-set")) != 0 {
		t.Errorf("the change set must be deleted, unread and never run: %v", f.calls())
	}
	if len(f.callsMatching("get-template --stack-name supavise --change-set-name supavise-update-")) != 1 ||
		!strings.Contains(f.callsMatching("get-template")[0], "--template-stage Original") {
		t.Errorf("the template of the change set is read in the stage the person submitted: %v", f.callsMatching("get-template"))
	}

	// The same in front of a new server: nothing of it exists when the leader's check stops.
	f, tok := replicaFake(t)
	r = f.run([]string{"S3_TAMPER=1"}, "replica", "--leader-stack", "supavise", "--region", "us-east-1", "--az", "us-east-1b", "--token-file", tok, "--template", realTemplate(t), "--yes")
	if r.code != 2 || !strings.Contains(r.stderr, "is not the file that was verified") {
		t.Fatalf("replica: exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	for _, none := range []string{"create-secret", "execute-change-set", "--change-set-type CREATE"} {
		if len(f.callsMatching(none)) != 0 {
			t.Errorf("%s ran although the template was changed", none)
		}
	}

	// Trailing white space is not a change.
	f = newUpdFake(t)
	if r = f.run([]string{"S3_PAD=1"}, "update", "--stack", "supavise", "--region", "us-east-1", "--template", realTemplate(t), "--yes"); r.code != 0 {
		t.Errorf("padded template: exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}

	// A template that cannot be read back is not trusted either.
	f = newUpdFake(t)
	r = f.run([]string{"GET_TEMPLATE_FAILS=1"}, "update", "--stack", "supavise", "--region", "us-east-1", "--template", smallTemplate(t), "--yes")
	if r.code != 3 || !strings.Contains(r.stderr, "cannot read back the template") || len(f.callsMatching("execute-change-set")) != 0 || len(f.callsMatching("delete-change-set")) != 1 {
		t.Errorf("exit %d\n%s\ncalls: %v", r.code, r.stderr, f.calls())
	}
}

// A value may hold spaces, commas and quotes; it reaches the parameters document as one string.
func TestUpdateSetValueWithSpaces(t *testing.T) {
	f := newUpdFake(t)
	r := f.run(nil, "update", "--stack", "supavise", "--region", "us-east-1", "--template", smallTemplate(t), "--yes",
		"--set", "KeyEscrowPassphrase=twelve chars, \"quoted\" and more", "--set", "Failover=on")
	if r.code != 0 {
		t.Fatalf("exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	ps := paramsByKey(f.params(1))
	if got := ps["KeyEscrowPassphrase"]["ParameterValue"]; got != `twelve chars, "quoted" and more` {
		t.Errorf("KeyEscrowPassphrase = %q", got)
	}
	if ps["Failover"]["ParameterValue"] != "on" {
		t.Errorf("Failover = %v", ps["Failover"])
	}
	// The value is not on any command line: it travels in the parameters file.
	for _, c := range f.calls() {
		if strings.Contains(c, "quoted") {
			t.Errorf("a value is on a command line: %s", c)
		}
	}
}

func TestUpdateNeverChangesTheVersion(t *testing.T) {
	f := newUpdFake(t)
	r := f.run(nil, "update", "--stack", "supavise", "--region", "us-east-1", "--template", smallTemplate(t), "--set", "SupaviseVersion=v9.9.9", "--yes")
	if r.code != 2 || !strings.Contains(r.stderr, "SupaviseVersion cannot be set") {
		t.Errorf("exit %d\n%s", r.code, r.stderr)
	}
	if len(f.callsMatching("create-change-set")) != 0 {
		t.Error("a change set was made although SupaviseVersion was asked for")
	}
	for _, bad := range [][]string{{"--set", "Nope=1"}, {"--set", "Failover"}} {
		r = f.run(nil, append([]string{"update", "--stack", "supavise", "--region", "us-east-1", "--template", smallTemplate(t), "--yes"}, bad...)...)
		if r.code != 2 {
			t.Errorf("%v: exit %d\n%s", bad, r.code, r.stderr)
		}
	}
}

func TestUpdateRefusesReplacementsAndRemovals(t *testing.T) {
	for i, c := range []struct{ file, want string }{
		{"changeset-instance-replaced.json", "REPLACE  Instance (AWS::EC2::Instance) would be replaced: ImageId"},
		{"changeset-instance-replaced.json", "REPLACE  DataVolume (AWS::EC2::Volume) would be replaced"},
		{"changeset-instance-conditional.json", "REPLACE  Instance (AWS::EC2::Instance) may be replaced: InstanceType"},
		{"changeset-in-place-unsafe.json", "MODIFY   Instance (AWS::EC2::Instance): UserData changes in place"},
		{"changeset-in-place-unsafe.json", "REMOVE   DnsRecords (AWS::Route53::RecordSetGroup) would be deleted"},
		{"changeset-unknown.json", "DYNAMIC"},
	} {
		t.Run(fmt.Sprintf("%s/%d", c.file, i), func(t *testing.T) {
			f := newUpdFake(t)
			f.copy(c.file, "changeset.json")
			r := f.run(nil, "update", "--stack", "supavise", "--region", "us-east-1", "--template", smallTemplate(t), "--yes")
			if r.code != 2 {
				t.Fatalf("exit %d, want 2 (refused)\n%s\n%s", r.code, r.stdout, r.stderr)
			}
			if !strings.Contains(r.stdout, c.want) {
				t.Errorf("the review lacks %q:\n%s", c.want, r.stdout)
			}
			if !strings.Contains(r.stderr, "Nothing was changed") {
				t.Errorf("stderr: %s", r.stderr)
			}
			// The change set is deleted and never run.
			if len(f.callsMatching("delete-change-set")) != 1 || len(f.callsMatching("execute-change-set")) != 0 {
				t.Errorf("calls: %v", f.calls())
			}
		})
	}
}

// --allow-risky needs a person: it asks for the stack name, and there is none to ask here.
func TestUpdateAllowRiskyNeedsATerminal(t *testing.T) {
	f := newUpdFake(t)
	f.copy("changeset-instance-replaced.json", "changeset.json")
	r := f.run(nil, "update", "--stack", "supavise", "--region", "us-east-1", "--template", smallTemplate(t), "--yes", "--allow-risky")
	if r.code != 2 || !strings.Contains(r.stderr, "needs a terminal") {
		t.Errorf("exit %d\n%s", r.code, r.stderr)
	}
	if len(f.callsMatching("execute-change-set")) != 0 {
		t.Error("ran a risky change set without a typed confirmation")
	}
}

func TestUpdateAsksBeforeRunning(t *testing.T) {
	f := newUpdFake(t)
	r := f.run(nil, "update", "--stack", "supavise", "--region", "us-east-1", "--template", smallTemplate(t))
	if r.code != 2 || !strings.Contains(r.stderr, "pass --yes") {
		t.Errorf("exit %d\n%s", r.code, r.stderr)
	}
	if len(f.callsMatching("execute-change-set")) != 0 || len(f.callsMatching("delete-change-set")) != 1 {
		t.Errorf("calls: %v", f.calls())
	}
}

func TestUpdateWithNothingToChange(t *testing.T) {
	f := newUpdFake(t)
	r := f.run([]string{"CS_WAIT_RC=255", "CS_REASON=The submitted information didn't contain changes. Submit different information to create a change set."},
		"update", "--stack", "supavise", "--region", "us-east-1", "--template", smallTemplate(t), "--yes")
	if r.code != 0 || !strings.Contains(r.stdout, "Nothing to change") {
		t.Errorf("exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	if len(f.callsMatching("execute-change-set")) != 0 || len(f.callsMatching("delete-change-set")) != 1 {
		t.Errorf("calls: %v", f.calls())
	}
	// Any other failure of the change set is a failure.
	f = newUpdFake(t)
	r = f.run([]string{"CS_WAIT_RC=255", "CS_REASON=Parameters: [Nope] do not exist in the template"},
		"update", "--stack", "supavise", "--region", "us-east-1", "--template", smallTemplate(t), "--yes")
	if r.code != 3 || !strings.Contains(r.stderr, "do not exist in the template") {
		t.Errorf("exit %d\n%s", r.code, r.stderr)
	}
}

func TestUpdateFailureIsExit3(t *testing.T) {
	f := newUpdFake(t)
	r := f.run([]string{"STACK_WAIT_RC=255"}, "update", "--stack", "supavise", "--region", "us-east-1", "--template", smallTemplate(t), "--yes")
	if r.code != 3 || !strings.Contains(r.stderr, "did not finish") {
		t.Errorf("exit %d\n%s", r.code, r.stderr)
	}
	f = newUpdFake(t)
	r = f.run([]string{"CREATE_FAILS=1"}, "update", "--stack", "supavise", "--region", "us-east-1", "--template", smallTemplate(t), "--yes")
	if r.code != 3 {
		t.Errorf("a change set that cannot be made: exit %d\n%s", r.code, r.stderr)
	}
	// A stack that does not exist.
	f = newUpdFake(t)
	os.Remove(filepath.Join(f.dir, "stack.json"))
	r = f.run(nil, "update", "--stack", "nope", "--region", "us-east-1", "--template", smallTemplate(t), "--yes")
	if r.code != 3 || !strings.Contains(r.stderr, "cannot read the stack nope") {
		t.Errorf("exit %d\n%s", r.code, r.stderr)
	}
}

// An older template would take the new resources away; update does not go back.
func TestUpdateRefusesAnOlderTemplate(t *testing.T) {
	f := newUpdFake(t)
	f.copy("stack-rev2.json", "stack.json")
	r := f.run(nil, "update", "--stack", "supavise", "--region", "us-east-1", "--template", smallTemplateAt(t, "1"), "--yes")
	if r.code != 2 || !strings.Contains(r.stderr, "does not move a stack back to an older template") {
		t.Errorf("exit %d\n%s", r.code, r.stderr)
	}
	if len(f.callsMatching("create-change-set")) != 0 {
		t.Error("a change set was made from an older template")
	}
	// The same revision, and a stack from before revisions with a template at 1 or more, are fine.
	f = newUpdFake(t)
	f.copy("stack-rev2.json", "stack.json")
	if r = f.run(nil, "update", "--stack", "supavise", "--region", "us-east-1", "--template", smallTemplateAt(t, "2"), "--yes"); r.code != 0 {
		t.Errorf("same revision: exit %d\n%s", r.code, r.stderr)
	}
	if !strings.Contains(r.stdout, "Credentials: arn:aws:iam::111122223333:user/admin") {
		t.Errorf("the credentials in use are not shown:\n%s", r.stdout)
	}
}

func TestUpdateRefusesAStackThatIsBusy(t *testing.T) {
	f := newUpdFake(t)
	b, _ := os.ReadFile(filepath.Join(f.dir, "stack.json"))
	os.WriteFile(filepath.Join(f.dir, "stack.json"), []byte(strings.Replace(string(b), "CREATE_COMPLETE", "UPDATE_IN_PROGRESS", 1)), 0o644)
	r := f.run(nil, "update", "--stack", "supavise", "--region", "us-east-1", "--template", smallTemplate(t), "--yes")
	if r.code != 2 || !strings.Contains(r.stderr, "UPDATE_IN_PROGRESS") {
		t.Errorf("exit %d\n%s", r.code, r.stderr)
	}
}

// The instance role is not a way to change the stack.
func TestUpdateRefusesTheInstanceRole(t *testing.T) {
	f := newUpdFake(t)
	r := f.run([]string{"CALLER_ARN=arn:aws:sts::111122223333:assumed-role/supavise-InstanceRole-ABC/i-0123456789abcdef0"},
		"update", "--stack", "supavise", "--region", "us-east-1", "--template", smallTemplate(t), "--yes")
	if r.code != 2 || !strings.Contains(r.stderr, "instance role") {
		t.Errorf("exit %d\n%s", r.code, r.stderr)
	}
	if len(f.callsMatching("describe-stacks")) != 0 {
		t.Error("read a stack with the instance role")
	}
	// An operator's assumed role is fine.
	f = newUpdFake(t)
	r = f.run([]string{"CALLER_ARN=arn:aws:sts::111122223333:assumed-role/Admin/jane"}, "update", "--stack", "supavise", "--region", "us-east-1", "--template", smallTemplate(t), "--yes")
	if r.code != 0 {
		t.Errorf("an assumed administrator role: exit %d\n%s", r.code, r.stderr)
	}
	// No credentials at all.
	f = newUpdFake(t)
	b, _ := os.ReadFile(filepath.Join(f.dir, "aws"))
	os.WriteFile(filepath.Join(f.dir, "aws"), []byte(strings.Replace(string(b), `*"sts get-caller-identity --query Arn"*) echo`, `*"sts get-caller-identity --query Arn"*) echo "Unable to locate credentials" >&2; exit 253; echo`, 1)), 0o755)
	r = f.run(nil, "update", "--stack", "supavise", "--region", "us-east-1", "--template", smallTemplate(t), "--yes")
	if r.code != 3 || !strings.Contains(r.stderr, "no usable AWS credentials") {
		t.Errorf("exit %d\n%s", r.code, r.stderr)
	}
}

// A failover moved the service address: the update goes on, and refuses to touch the association.
func TestUpdateAfterAFailover(t *testing.T) {
	f := newUpdFake(t)
	r := f.run([]string{"ADDRESS_HOLDER=i-0peerpeerpeer00001"}, "update", "--stack", "supavise", "--region", "us-east-1", "--template", smallTemplate(t), "--yes")
	if r.code != 0 || !strings.Contains(r.stdout, "WARNING: the service address of supavise is not on its instance") {
		t.Errorf("a change set that leaves the association alone goes on: exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	for _, risky := range []string{"", "--allow-risky"} {
		f = newUpdFake(t)
		f.copy("changeset-association.json", "changeset.json")
		args := []string{"update", "--stack", "supavise", "--region", "us-east-1", "--template", smallTemplate(t), "--yes"}
		if risky != "" {
			args = append(args, risky)
		}
		r = f.run([]string{"ADDRESS_HOLDER=i-0peerpeerpeer00001"}, args...)
		if r.code != 2 || !strings.Contains(r.stderr, "take the service address back") || !strings.Contains(r.stdout, "the service address is on another server after a failover") {
			t.Errorf("%q: a change set that touches the association is refused: exit %d\n%s\n%s", risky, r.code, r.stdout, r.stderr)
		}
		if len(f.callsMatching("execute-change-set")) != 0 {
			t.Errorf("%q: ran a change set that moves the service address back", risky)
		}
	}
	// The same change set, with the address where it belongs, is judged as any other: a change to
	// the association is not on the list of safe ones, so it is refused as well, for that reason.
	f = newUpdFake(t)
	f.copy("changeset-association.json", "changeset.json")
	r = f.run(nil, "update", "--stack", "supavise", "--region", "us-east-1", "--template", smallTemplate(t), "--yes")
	if r.code != 2 || strings.Contains(r.stdout, "after a failover") || !strings.Contains(r.stdout, "MODIFY   ElasticIpAssociation (AWS::EC2::EIPAssociation): InstanceId changes in place") {
		t.Errorf("the association is in place: exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	// An address that cannot be read counts as moved.
	f = newUpdFake(t)
	f.copy("changeset-association.json", "changeset.json")
	r = f.run([]string{"ADDRESS_FAILS=1"}, "update", "--stack", "supavise", "--region", "us-east-1", "--template", smallTemplate(t), "--yes")
	if r.code != 2 {
		t.Errorf("an unreadable address: exit %d\n%s", r.code, r.stdout)
	}
}

// A stack made in the console has an empty AmiId: the image of its instance is passed, so that an
// update cannot replace the instance for a newer image.
func TestUpdateKeepsTheRunningImage(t *testing.T) {
	f := newUpdFake(t)
	b, _ := os.ReadFile(filepath.Join(f.dir, "stack.json"))
	os.WriteFile(filepath.Join(f.dir, "stack.json"), []byte(strings.Replace(string(b), `"ParameterValue": "ami-0aaaaaaaaaaaaaaaa"`, `"ParameterValue": ""`, 1)), 0o644)
	r := f.run(nil, "update", "--stack", "supavise", "--region", "us-east-1", "--template", realTemplate(t), "--yes")
	if r.code != 0 {
		t.Fatalf("exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	if p := paramsByKey(f.params(1))["AmiId"]; p["ParameterValue"] != "ami-0bbbbbbbbbbbbbbbb" {
		t.Errorf("AmiId: %v, want the image the instance runs", p)
	}
	if len(f.callsMatching("ec2 describe-instances --instance-ids i-0123456789abcdef0")) != 1 {
		t.Errorf("calls: %v", f.calls())
	}
	// With an AmiId in the stack, nothing is looked up.
	f = newUpdFake(t)
	f.run(nil, "update", "--stack", "supavise", "--region", "us-east-1", "--template", realTemplate(t), "--yes")
	if len(f.callsMatching("ec2 describe-instances")) != 0 {
		t.Error("looked up an image although the stack has one")
	}
}

// ---- the metadata service -------------------------------------------------------------------

// imdsServer answers IMDSv2 the way EC2 does: a token on PUT, then values for it.
func imdsServer(t *testing.T, id, region, stack string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && r.URL.Path == "/latest/api/token" {
			fmt.Fprint(w, "tok123")
			return
		}
		if r.Header.Get("X-aws-ec2-metadata-token") != "tok123" {
			http.Error(w, "no token", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/latest/meta-data/instance-id":
			fmt.Fprint(w, id)
		case "/latest/meta-data/placement/region":
			fmt.Fprint(w, region)
		case "/latest/meta-data/tags/instance/supavise:stack-name":
			if stack == "" {
				http.NotFound(w, r)
				return
			}
			fmt.Fprint(w, stack)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestUpdateOnTheNode(t *testing.T) {
	// A revision 2 node: the stack and the region come from its tags and metadata, and its instance
	// id is checked against the stack's.
	f := newUpdFake(t)
	f.copy("stack-rev2.json", "stack.json")
	srv := imdsServer(t, "i-0123456789abcdef0", "us-east-1", "supavise")
	r := f.run([]string{"SUPAVISE_IMDS_ENDPOINT=" + srv.URL}, "update", "--template", smallTemplate(t), "--yes")
	if r.code != 0 {
		t.Fatalf("exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	if len(f.callsMatching("--stack-name supavise")) == 0 || !strings.HasPrefix(f.calls()[0], "--region us-east-1 ") {
		t.Errorf("calls: %v", f.calls())
	}
	b, _ := os.ReadFile(filepath.Join(f.dir, "imds.log"))
	for _, l := range strings.Fields(string(b)) {
		if l != "true" {
			t.Errorf("AWS_EC2_METADATA_DISABLED=%q for an aws call", l)
		}
	}

	// A stack from before revisions: no tag, so --stack is needed; the instance id still must match.
	f = newUpdFake(t)
	srv = imdsServer(t, "i-0123456789abcdef0", "us-east-1", "")
	r = f.run([]string{"SUPAVISE_IMDS_ENDPOINT=" + srv.URL}, "update", "--template", smallTemplate(t), "--yes")
	if r.code != 2 || !strings.Contains(r.stderr, "--stack is required") {
		t.Errorf("no tag and no --stack: exit %d\n%s", r.code, r.stderr)
	}
	r = f.run([]string{"SUPAVISE_IMDS_ENDPOINT=" + srv.URL}, "update", "--stack", "supavise", "--template", smallTemplate(t), "--yes")
	if r.code != 0 {
		t.Errorf("--stack on a v0.1.x node, region from the metadata service: exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}

	// A host that is not a node of a revision 2 stack (a jump host, another server of the cluster)
	// may update it: the node's tags, not its id, tell nodes apart.
	f = newUpdFake(t)
	f.copy("stack-rev2.json", "stack.json")
	srv = imdsServer(t, "i-0aaaaaaaaaaaaaaaa", "us-east-1", "")
	r = f.run([]string{"SUPAVISE_IMDS_ENDPOINT=" + srv.URL}, "update", "--stack", "supavise", "--template", smallTemplate(t), "--yes")
	if r.code != 0 {
		t.Errorf("another EC2 host, revision 2 stack: exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}

	// Another node's stack from before revision 2 is refused: nothing else says whose it is.
	f = newUpdFake(t)
	srv = imdsServer(t, "i-0aaaaaaaaaaaaaaaa", "us-east-1", "")
	r = f.run([]string{"SUPAVISE_IMDS_ENDPOINT=" + srv.URL}, "update", "--stack", "supavise", "--template", smallTemplate(t), "--yes")
	if r.code != 2 || !strings.Contains(r.stderr, "belongs to instance i-0123456789abcdef0, and this is i-0aaaaaaaaaaaaaaaa") {
		t.Errorf("exit %d\n%s", r.code, r.stderr)
	}
	// A tag that names another stack than --stack is refused.
	srv = imdsServer(t, "i-0123456789abcdef0", "us-east-1", "other")
	r = f.run([]string{"SUPAVISE_IMDS_ENDPOINT=" + srv.URL}, "update", "--stack", "supavise", "--template", smallTemplate(t), "--yes")
	if r.code != 2 || !strings.Contains(r.stderr, "belongs to stack other") {
		t.Errorf("exit %d\n%s", r.code, r.stderr)
	}
}

// ---- the verified download ------------------------------------------------------------------

// needOpenSSL skips a test when no OpenSSL here can verify ed25519, like the script itself.
func needOpenSSL(t *testing.T) {
	t.Helper()
	for _, c := range []string{"openssl", "/opt/homebrew/opt/openssl@3/bin/openssl", "/usr/local/opt/openssl@3/bin/openssl"} {
		p, err := exec.LookPath(c)
		if err == nil && exec.Command(p, "genpkey", "-algorithm", "ed25519").Run() == nil {
			return
		}
	}
	t.Skip("no openssl with ed25519 support")
}

type relDir struct {
	base   string // the SUPAVISE_DEPLOY_BASE_URL
	keyB64 string
	dir    string
	priv   ed25519.PrivateKey
}

// makeRelease writes a release directory the way GitHub serves one: download/<tag>/<asset>, with
// a checksum list signed by a throwaway key. The script's own checksum is listed when scriptSum is not "-".
func makeRelease(t *testing.T, tag string, template []byte) *relDir {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	r := &relDir{dir: t.TempDir(), keyB64: base64.StdEncoding.EncodeToString(pemBytes), priv: priv}
	r.base = "file://" + r.dir
	asset := filepath.Join(r.dir, "download", tag)
	if err := os.MkdirAll(asset, 0o755); err != nil {
		t.Fatal(err)
	}
	script, _ := os.ReadFile("deploy.sh")
	sum := func(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
	sums := fmt.Sprintf("%s  supavise.yaml\n%s  supavise-aws-deploy.sh\n", sum(template), sum(script))
	r.write(t, tag, "supavise.yaml", template)
	r.write(t, tag, "SHA256SUMS", []byte(sums))
	r.write(t, tag, "SHA256SUMS.sig", ed25519.Sign(priv, []byte(sums)))
	return r
}

func (r *relDir) write(t *testing.T, tag, name string, b []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(r.dir, "download", tag, name), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func (r *relDir) env() []string {
	return []string{"SUPAVISE_DEPLOY_BASE_URL=" + r.base, "SUPAVISE_DEPLOY_PUBKEY_B64=" + r.keyB64}
}

func TestUpdateVerifiesTheTemplateItDownloads(t *testing.T) {
	needOpenSSL(t)
	tpl, _ := os.ReadFile(smallTemplate(t))
	args := []string{"update", "--stack", "supavise", "--region", "us-east-1", "--version", "v1.2.3", "--yes"}

	// A good release: signed list, matching template, this script.
	rel := makeRelease(t, "v1.2.3", tpl)
	f := newUpdFake(t)
	r := f.run(rel.env(), args...)
	if r.code != 0 || !strings.Contains(r.stdout, "Template verified against the signed checksums of v1.2.3") {
		t.Fatalf("exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	if got, _ := os.ReadFile(filepath.Join(f.dir, "template-1.yaml")); string(got) != string(tpl) {
		t.Error("the change set was not made from the downloaded template")
	}

	// The template is changed after it was signed.
	rel = makeRelease(t, "v1.2.3", tpl)
	rel.write(t, "v1.2.3", "supavise.yaml", append([]byte("# edited\n"), tpl...))
	f = newUpdFake(t)
	r = f.run(rel.env(), args...)
	if r.code != 2 || !strings.Contains(r.stderr, "does not match the signed checksum list") || len(f.callsMatching("create-change-set")) != 0 {
		t.Errorf("an edited template: exit %d\n%s", r.code, r.stderr)
	}

	// The list is changed (a hash swapped), so the signature fails.
	rel = makeRelease(t, "v1.2.3", tpl)
	other := append([]byte("# other\n"), tpl...)
	h := sha256.Sum256(other)
	rel.write(t, "v1.2.3", "SHA256SUMS", []byte(hex.EncodeToString(h[:])+"  supavise.yaml\n"))
	rel.write(t, "v1.2.3", "supavise.yaml", other)
	f = newUpdFake(t)
	r = f.run(rel.env(), args...)
	if r.code != 2 || !strings.Contains(r.stderr, "signature of SHA256SUMS does not verify") || len(f.callsMatching("create-change-set")) != 0 {
		t.Errorf("a forged list: exit %d\n%s", r.code, r.stderr)
	}

	// Signed by another key.
	rel = makeRelease(t, "v1.2.3", tpl)
	rogue := makeRelease(t, "v1.2.3", tpl)
	env := []string{"SUPAVISE_DEPLOY_BASE_URL=" + rogue.base, "SUPAVISE_DEPLOY_PUBKEY_B64=" + rel.keyB64}
	f = newUpdFake(t)
	r = f.run(env, args...)
	if r.code != 2 || !strings.Contains(r.stderr, "does not verify") {
		t.Errorf("another key: exit %d\n%s", r.code, r.stderr)
	}

	// The script that is running is not the one the release signed.
	rel = makeRelease(t, "v1.2.3", tpl)
	sums, _ := os.ReadFile(filepath.Join(rel.dir, "download", "v1.2.3", "SHA256SUMS"))
	forged := regexp.MustCompile(`(?m)^[0-9a-f]{64}  supavise-aws-deploy.sh$`).ReplaceAllString(string(sums), strings.Repeat("0", 64)+"  supavise-aws-deploy.sh")
	rel.write(t, "v1.2.3", "SHA256SUMS", []byte(forged))
	rel.write(t, "v1.2.3", "SHA256SUMS.sig", ed25519.Sign(rel.priv, []byte(forged)))
	f = newUpdFake(t)
	r = f.run(rel.env(), args...)
	if r.code != 2 || !strings.Contains(r.stderr, "is not the one release v1.2.3 signed") {
		t.Errorf("an edited script: exit %d\n%s", r.code, r.stderr)
	}

	// A signed list that does not name the script cannot vouch for it.
	rel = makeRelease(t, "v1.2.3", tpl)
	sums, _ = os.ReadFile(filepath.Join(rel.dir, "download", "v1.2.3", "SHA256SUMS"))
	noScript := regexp.MustCompile(`(?m)^[0-9a-f]{64}  supavise-aws-deploy.sh\n`).ReplaceAllString(string(sums), "")
	rel.write(t, "v1.2.3", "SHA256SUMS", []byte(noScript))
	rel.write(t, "v1.2.3", "SHA256SUMS.sig", ed25519.Sign(rel.priv, []byte(noScript)))
	f = newUpdFake(t)
	r = f.run(rel.env(), args...)
	if r.code != 2 || !strings.Contains(r.stderr, "does not name supavise-aws-deploy.sh") || len(f.callsMatching("create-change-set")) != 0 {
		t.Errorf("a list without the script: exit %d\n%s", r.code, r.stderr)
	}

	// A copy of the script without a key (a checkout) cannot verify, and says what to do.
	rel = makeRelease(t, "v1.2.3", tpl)
	f = newUpdFake(t)
	r = f.run([]string{"SUPAVISE_DEPLOY_BASE_URL=" + rel.base}, args...)
	if r.code != 2 || !strings.Contains(r.stderr, "holds no release key") || !strings.Contains(r.stderr, "--template FILE") {
		t.Errorf("no key: exit %d\n%s", r.code, r.stderr)
	}
	// A release that cannot be downloaded.
	f = newUpdFake(t)
	r = f.run(append(rel.env(), "SUPAVISE_DEPLOY_BASE_URL=file:///nonexistent"), args...)
	if r.code != 3 || !strings.Contains(r.stderr, "cannot download") {
		t.Errorf("no release: exit %d\n%s", r.code, r.stderr)
	}
}

// ---- status -------------------------------------------------------------------------------------

func TestStatus(t *testing.T) {
	f := newUpdFake(t)
	r := f.run(nil, "status", "--stack", "supavise", "--region", "us-east-1", "--template", realTemplate(t))
	if r.code != 0 {
		t.Fatalf("exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	for _, want := range []string{
		"Stack:            supavise (us-east-1) CREATE_COMPLETE",
		"Instance:         i-0123456789abcdef0",
		"SupaviseVersion=v0.1.1",
		"Infrastructure:   revision 1 (a stack from before revisions)",
		"Template:         revision 2: run  deploy.sh update --stack supavise --region us-east-1",
		"203.0.113.50 is on this stack's instance",
		"Before you replace an instance of this stack",
		"set SupaviseVersion to",
	} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("status lacks %q:\n%s", want, r.stdout)
		}
	}
	if len(f.callsMatching("create-change-set")) != 0 {
		t.Error("status changed something")
	}

	// A stack at revision 2 whose address a failover moved.
	f = newUpdFake(t)
	f.copy("stack-rev2.json", "stack.json")
	r = f.run([]string{"ADDRESS_HOLDER=i-0peerpeerpeer00001"}, "status", "--stack", "supavise", "--region", "us-east-1", "--template", realTemplate(t))
	for _, want := range []string{"Infrastructure:   revision 2\n", "revision 2: the stack is up to date", "is NOT on this stack's instance but on i-0peerpeerpeer00001", "update leaves the association alone"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("status lacks %q:\n%s", want, r.stdout)
		}
	}
}

// ---- replica ------------------------------------------------------------------------------------

func replicaFake(t *testing.T) (*updFake, string) {
	t.Helper()
	f := newUpdFake(t)
	f.copy("stack-rev2.json", "stack-supavise.json")
	f.copy("stack-rev2.json", "stack-supavise-replica.json")
	tok := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tok, []byte("svj1.TOKENTOKENTOKEN\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return f, tok
}

func TestReplica(t *testing.T) {
	f, tok := replicaFake(t)
	r := f.run(nil, "replica", "--leader-stack", "supavise", "--leader-region", "us-east-1", "--region", "eu-west-1", "--az", "eu-west-1b",
		"--token-file", tok, "--template", realTemplate(t), "--template-bucket", "my-eu-templates", "--yes")
	if r.code != 0 {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s\ncalls: %v", r.code, r.stdout, r.stderr, f.calls())
	}
	// The token is read from the file by the CLI and never appears on a command line or in the output.
	for _, c := range f.calls() {
		if strings.Contains(c, "TOKENTOKEN") {
			t.Errorf("the token is on a command line: %s", c)
		}
	}
	if strings.Contains(r.stdout+r.stderr, "TOKENTOKEN") {
		t.Error("the token was printed")
	}
	if b, _ := os.ReadFile(filepath.Join(f.dir, "secret-value")); !strings.Contains(string(b), "svj1.TOKENTOKENTOKEN") {
		t.Errorf("the secret does not hold the token: %q", b)
	}
	if sec := f.callsMatching("secretsmanager create-secret"); len(sec) != 1 || !strings.HasPrefix(sec[0], "--region eu-west-1 ") || !strings.Contains(sec[0], "--secret-string file://") {
		t.Errorf("create-secret: %v", sec)
	}

	// Three change sets, in this order: the leader's (checked and dropped), the new stack's, the
	// leader's again (run). The new stack has a stack name of its own and the leader's inputs.
	cs := f.callsMatching("create-change-set")
	if len(cs) != 3 {
		t.Fatalf("change sets: %v", cs)
	}
	if !strings.Contains(cs[0], "--stack-name supavise ") || !strings.Contains(cs[0], "--change-set-type UPDATE") {
		t.Errorf("first change set: %s", cs[0])
	}
	if !strings.Contains(cs[1], "--stack-name supavise-replica ") || !strings.Contains(cs[1], "--change-set-type CREATE") || !strings.HasPrefix(cs[1], "--region eu-west-1 ") ||
		!strings.Contains(cs[1], "--template-url https://my-eu-templates.s3.eu-west-1.amazonaws.com/_stack/") {
		t.Errorf("second change set: %s", cs[1])
	}
	if !strings.Contains(cs[2], "--stack-name supavise ") || !strings.Contains(cs[2], "--change-set-type UPDATE") || !strings.HasPrefix(cs[2], "--region us-east-1 ") {
		t.Errorf("third change set: %s", cs[2])
	}
	want := map[string]string{
		"AdminEmail": "owner@example.com", "JoinLeader": "203.0.113.50:7443", "BackupBucketName": "supavise-backupbucket-abc123", "BackupBucketRegion": "us-east-1",
		"ObjectsBucketName": "supavise-objectsbucket-abc123", "StorageRoleArn": "arn:aws:iam::111122223333:role/supavise-StorageRole-ABC",
		"ClusterName": "supavise", "PeerCidr1": "203.0.113.50/32", "AvailabilityZone": "eu-west-1b", "AmiId": "ami-0123456789abcdef0",
		"JoinTokenSecretArn": "arn:aws:secretsmanager:eu-west-1:111122223333:secret:supavise-join-AbCdEf",
	}
	np := paramsByKey(f.params(2))
	for k, v := range want {
		if np[k]["ParameterValue"] != v {
			t.Errorf("new stack parameter %s = %v, want %s", k, np[k], v)
		}
	}
	for _, k := range []string{"SupaviseVersion", "KeyEscrowPassphrase", "DomainName", "HostedZoneId"} {
		if _, ok := np[k]; ok {
			t.Errorf("the new stack was given %s; it takes the template's default", k)
		}
	}
	// The leader opens PeerCidr1 to the new address, and keeps everything else as it was.
	lp := paramsByKey(f.params(3))
	if lp["PeerCidr1"]["ParameterValue"] != "203.0.113.77/32" || lp["SupaviseVersion"]["UsePreviousValue"] != true {
		t.Errorf("leader parameters: %v", lp)
	}
	if lp0 := paramsByKey(f.params(1)); lp0["PeerCidr1"]["ParameterValue"] != "192.0.2.1/32" {
		t.Errorf("the check uses a stand-in address: %v", lp0["PeerCidr1"])
	}
	// The order of the calls.
	order := []string{"create-change-set --stack-name supavise ", "delete-change-set --stack-name supavise ", "create-secret", "create-change-set --stack-name supavise-replica",
		"execute-change-set --stack-name supavise-replica", "describe-stack-resource", "execute-change-set --stack-name supavise ", "wait stack-create-complete --stack-name supavise-replica", "delete-secret"}
	last := -1
	for _, o := range order {
		i := -1
		for j, c := range f.calls() {
			if j > last && strings.Contains(c, o) {
				i = j
				break
			}
		}
		if i < 0 {
			t.Errorf("%q is missing or out of order in:\n%s", o, strings.Join(f.calls(), "\n"))
			break
		}
		last = i
	}
	if !strings.Contains(r.stdout, "The new server's address is 203.0.113.77") || !strings.Contains(r.stdout, "supavise node ls") {
		t.Errorf("stdout:\n%s", r.stdout)
	}
}

func TestReplicaStopsBeforeCreatingAnythingWhenTheLeaderCannotBeOpened(t *testing.T) {
	// The leader's change set holds a replacement: nothing of the new server exists yet.
	f, tok := replicaFake(t)
	f.copy("changeset-instance-replaced.json", "changeset-1.json")
	r := f.run(nil, "replica", "--leader-stack", "supavise", "--region", "us-east-1", "--az", "us-east-1b", "--token-file", tok, "--template", smallTemplate(t), "--yes")
	if r.code != 2 || !strings.Contains(r.stderr, "Nothing was changed") {
		t.Fatalf("exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	for _, none := range []string{"create-secret", "execute-change-set", "--change-set-type CREATE"} {
		if len(f.callsMatching(none)) != 0 {
			t.Errorf("%s ran although the leader cannot be opened", none)
		}
	}
}

func TestReplicaNeedsARevision2Leader(t *testing.T) {
	f, tok := replicaFake(t)
	f.copy("stack-v011.json", "stack-supavise.json")
	r := f.run(nil, "replica", "--leader-stack", "supavise", "--region", "us-east-1", "--az", "us-east-1b", "--token-file", tok, "--template", smallTemplate(t), "--yes")
	if r.code != 2 || !strings.Contains(r.stderr, "infrastructure revision 1") || !strings.Contains(r.stderr, "deploy.sh update --stack supavise") {
		t.Errorf("exit %d\n%s", r.code, r.stderr)
	}
	if len(f.callsMatching("create-secret")) != 0 {
		t.Error("stored a token for a stack that cannot take the server")
	}
}

func TestReplicaRemovesTheTokenWhenItStopsEarly(t *testing.T) {
	f, tok := replicaFake(t)
	// The person declines at the prompt (there is no terminal and no --yes): the secret that was
	// stored for the stack is removed again.
	r := f.run(nil, "replica", "--leader-stack", "supavise", "--region", "us-east-1", "--az", "us-east-1b", "--token-file", tok, "--template", smallTemplate(t))
	if r.code != 2 {
		t.Fatalf("exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	if len(f.callsMatching("create-secret")) != 1 || len(f.callsMatching("delete-secret")) != 1 {
		t.Errorf("the secret is made and removed again: %v", f.calls())
	}
	if len(f.callsMatching("execute-change-set --stack-name supavise-replica")) != 0 {
		t.Error("ran the change set")
	}
	// The change set made the new stack, empty: the output says how to remove it.
	if !strings.Contains(r.stderr, "exists but is empty") || !strings.Contains(r.stderr, "aws cloudformation delete-stack --region us-east-1 --stack-name supavise-replica") {
		t.Errorf("stderr does not say how to remove the empty stack:\n%s", r.stderr)
	}
}

// The leader cannot be opened after the new stack exists (a change set that is refused here): the
// server is booting and cannot join, and the token secret stays. The output says what to do.
func TestReplicaSaysWhatStaysWhenTheLeaderCannotBeOpenedAfterTheStackExists(t *testing.T) {
	f, tok := replicaFake(t)
	f.copy("changeset-instance-replaced.json", "changeset-3.json")
	r := f.run(nil, "replica", "--leader-stack", "supavise", "--region", "us-east-1", "--az", "us-east-1b", "--token-file", tok, "--template", smallTemplate(t), "--yes")
	if r.code != 2 || !strings.Contains(r.stderr, "Nothing was changed") {
		t.Fatalf("exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	if len(f.callsMatching("execute-change-set --stack-name supavise-replica")) != 1 || len(f.callsMatching("delete-secret")) != 0 {
		t.Errorf("the stack was made and the secret stays: %v", f.calls())
	}
	for _, want := range []string{"Stack supavise-replica in us-east-1 is being created", "update --stack supavise --region us-east-1 --set PeerCidr1=ADDRESS/32",
		"aws cloudformation delete-stack --region us-east-1 --stack-name supavise-replica",
		"aws secretsmanager delete-secret --region us-east-1 --force-delete-without-recovery --secret-id arn:aws:secretsmanager:eu-west-1:111122223333:secret:supavise-join-AbCdEf"} {
		if !strings.Contains(r.stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, r.stderr)
		}
	}

	// The server never finished: the secret is named too, and the exit is a failure.
	f, tok = replicaFake(t)
	r = f.run([]string{"CREATE_WAIT_RC=255"}, "replica", "--leader-stack", "supavise", "--region", "us-east-1", "--az", "us-east-1b", "--token-file", tok, "--template", smallTemplate(t), "--yes")
	if r.code != 3 || !strings.Contains(r.stderr, "did not finish creating") || !strings.Contains(r.stderr, "delete-secret --region us-east-1 --force-delete-without-recovery") {
		t.Errorf("exit %d\n%s", r.code, r.stderr)
	}
	// A run that works says nothing of leftovers.
	f, tok = replicaFake(t)
	if r = f.run(nil, "replica", "--leader-stack", "supavise", "--region", "us-east-1", "--az", "us-east-1b", "--token-file", tok, "--template", smallTemplate(t), "--yes"); r.code != 0 ||
		strings.Contains(r.stderr, "delete-stack") || strings.Contains(r.stderr, "delete-secret") {
		t.Errorf("exit %d\n%s", r.code, r.stderr)
	}
}

func TestReplicaTokenFile(t *testing.T) {
	f, tok := replicaFake(t)
	if err := os.Chmod(tok, 0o644); err != nil {
		t.Fatal(err)
	}
	r := f.run(nil, "replica", "--leader-stack", "supavise", "--region", "us-east-1", "--az", "us-east-1b", "--token-file", tok, "--template", smallTemplate(t), "--yes")
	if r.code != 2 || !strings.Contains(r.stderr, "can be read by other users") {
		t.Errorf("exit %d\n%s", r.code, r.stderr)
	}
	r = f.run(nil, "replica", "--leader-stack", "supavise", "--region", "us-east-1", "--az", "us-east-1b", "--token-file", filepath.Join(t.TempDir(), "absent"), "--template", smallTemplate(t), "--yes")
	if r.code != 2 || !strings.Contains(r.stderr, "empty or unreadable") {
		t.Errorf("exit %d\n%s", r.code, r.stderr)
	}
	if len(f.calls()) != 0 {
		t.Errorf("aws was called: %v", f.calls())
	}
}

func TestReplicaFullPeerRules(t *testing.T) {
	f, tok := replicaFake(t)
	b, _ := os.ReadFile(filepath.Join(f.dir, "stack-supavise.json"))
	s := string(b)
	for i := 1; i <= 3; i++ {
		re := regexp.MustCompile(fmt.Sprintf(`("ParameterKey": "PeerCidr%d",\s*"ParameterValue": )""`, i))
		s = re.ReplaceAllString(s, fmt.Sprintf(`${1}"198.51.100.%d/32"`, i))
	}
	os.WriteFile(filepath.Join(f.dir, "stack-supavise.json"), []byte(s), 0o644)
	r := f.run(nil, "replica", "--leader-stack", "supavise", "--region", "us-east-1", "--az", "us-east-1b", "--token-file", tok, "--template", smallTemplate(t), "--yes")
	if r.code != 2 || !strings.Contains(r.stderr, "uses all three PeerCidr parameters") {
		t.Errorf("exit %d\n%s", r.code, r.stderr)
	}
}

// ---- arguments of the new commands ---------------------------------------------------------------

func TestNewCommandArgumentErrors(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"update delete", []string{"update", "--stack", "s", "--region", "us-east-1", "--delete"}, "update does not take --delete"},
		{"update create option", []string{"update", "--stack", "s", "--region", "us-east-1", "--email", "a@b.co"}, "update changes nothing but what --set names"},
		{"update bad stack", []string{"update", "--stack", "1bad", "--region", "us-east-1"}, "--stack-name must start"},
		{"update bad region", []string{"update", "--stack", "s", "--region", "mars"}, "not a region name"},
		{"update bad bucket", []string{"update", "--stack", "s", "--region", "us-east-1", "--template-bucket", "X"}, "not an S3 bucket name"},
		{"status risky", []string{"status", "--stack", "s", "--region", "us-east-1", "--allow-risky"}, "status takes only"},
		{"status set", []string{"status", "--stack", "s", "--region", "us-east-1", "--set", "A=b"}, "status takes only"},
		{"replica no leader", []string{"replica", "--region", "us-east-1", "--az", "us-east-1b", "--token-file", "t"}, "--leader-stack is required"},
		{"replica no region", []string{"replica", "--leader-stack", "s", "--az", "us-east-1b", "--token-file", "t"}, "--region is required"},
		{"replica no az", []string{"replica", "--leader-stack", "s", "--region", "us-east-1", "--token-file", "t"}, "--az is required"},
		{"replica az elsewhere", []string{"replica", "--leader-stack", "s", "--region", "us-east-1", "--az", "eu-west-1b", "--token-file", "t"}, "is not in --region"},
		{"replica bad az", []string{"replica", "--leader-stack", "s", "--region", "us-east-1", "--az", "us-east-1", "--token-file", "t"}, "is not a zone name"},
		{"replica no token", []string{"replica", "--leader-stack", "s", "--region", "us-east-1", "--az", "us-east-1b"}, "--token-file is required"},
		{"replica set", []string{"replica", "--leader-stack", "s", "--region", "us-east-1", "--az", "us-east-1b", "--token-file", "t", "--set", "A=b"}, "replica takes no --set"},
		{"replica domain", []string{"replica", "--leader-stack", "s", "--region", "us-east-1", "--az", "us-east-1b", "--token-file", "t", "--domain", "example.com"}, "takes its domain from the leader"},
		{"replica vpc alone", []string{"replica", "--leader-stack", "s", "--region", "us-east-1", "--az", "us-east-1b", "--token-file", "t", "--vpc-id", "vpc-0123456789abcdef0"}, "give both --vpc-id and --subnet-id"},
		{"replica vpc and az", []string{"replica", "--leader-stack", "s", "--region", "us-east-1", "--az", "us-east-1b", "--token-file", "t", "--vpc-id", "vpc-0123456789abcdef0", "--subnet-id", "subnet-0123456789abcdef0"}, "with --subnet-id the subnet decides"},
		{"replica bad email", []string{"replica", "--leader-stack", "s", "--region", "us-east-1", "--az", "us-east-1b", "--token-file", "t", "--email", "x"}, "not an email address"},
		{"replica bad version", []string{"replica", "--leader-stack", "s", "--region", "us-east-1", "--az", "us-east-1b", "--token-file", "t", "--version", "main"}, "--version must be"},
	}
	for _, b := range bashes(t) {
		for _, c := range cases {
			t.Run(filepath.Base(b)+"/"+c.name, func(t *testing.T) {
				dir, log := stubAWS(t)
				r := run(t, b, dir, nil, append(c.args, "--dry-run")...)
				if r.code != 2 || !strings.Contains(r.stderr, c.want) {
					t.Errorf("exit %d, stderr %q, want exit 2 and %q", r.code, r.stderr, c.want)
				}
				if n := calls(t, log); len(n) != 0 {
					t.Errorf("a refused command called aws: %v", n)
				}
			})
		}
	}
}

func TestNewCommandDryRuns(t *testing.T) {
	for _, b := range bashes(t) {
		dir, log := stubAWS(t)
		for name, c := range map[string]struct {
			args []string
			want []string
		}{
			"update": {[]string{"update", "--stack", "supavise", "--region", "us-east-1", "--template", "/x/t.yaml", "--set", "Failover=on"}, []string{
				"# dry run: nothing is sent to AWS",
				"aws --region us-east-1 sts get-caller-identity --query Arn --output text",
				"aws --region us-east-1 cloudformation describe-stacks --stack-name supavise --output json",
				"--change-set-type UPDATE --capabilities CAPABILITY_IAM --template-url",
				"s3api put-object --bucket '<BackupBucket>'", "--expected-bucket-owner '<account>'",
				"cloudformation get-template --stack-name supavise --change-set-name", "--template-stage Original",
				"aws --region us-east-1 cloudformation execute-change-set --stack-name supavise",
				"SupaviseVersion is never set",
			}},
			"status": {[]string{"status", "--stack", "supavise", "--region", "us-east-1"}, []string{"cloudformation describe-stacks --stack-name supavise --output json", "ec2 describe-addresses"}},
			"replica": {[]string{"replica", "--leader-stack", "supavise", "--leader-region", "us-east-1", "--region", "eu-west-1", "--az", "eu-west-1b", "--token-file", "/x/token", "--template", "/x/t.yaml"}, []string{
				"aws --region us-east-1 cloudformation describe-stacks --stack-name supavise --output json",
				"secretsmanager create-secret --name",
				"--secret-string file:///x/token",
				"--change-set-type CREATE",
				"deploy.sh update --stack supavise --region us-east-1 --set 'PeerCidr<n>=<new Elastic IP>/32'",
			}},
		} {
			r := run(t, b, dir, nil, append(c.args, "--dry-run")...)
			if r.code != 0 {
				t.Errorf("%s: exit %d\n%s\n%s", name, r.code, r.stdout, r.stderr)
				continue
			}
			for _, want := range c.want {
				if !strings.Contains(r.stdout, want) {
					t.Errorf("%s: output lacks %q:\n%s", name, want, r.stdout)
				}
			}
		}
		if n := calls(t, log); len(n) != 0 {
			t.Errorf("a dry run called aws: %v", n)
		}
	}
}

func TestHelpListsTheNewCommands(t *testing.T) {
	r := run(t, bashes(t)[0], "", nil, "--help")
	for _, want := range []string{"update  --stack NAME", "status  --stack NAME", "replica --leader-stack NAME", "--allow-risky", "--set NAME=VALUE", "Exit status of update, status and replica"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("--help lacks %q", want)
		}
	}
}

// ---- the classifier --------------------------------------------------------------------------------

// classify runs the review of a change set file the way update does.
func classify(t *testing.T, file string, flags ...string) (stdout string, code int) {
	t.Helper()
	script, _ := filepath.Abs("deploy.sh")
	args := append([]string{script, "__classify", filepath.Join("testdata", file)}, flags...)
	cmd := exec.Command(bashes(t)[0], args...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir()}
	var so, se strings.Builder
	cmd.Stdout, cmd.Stderr = &so, &se
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	if se.Len() != 0 {
		t.Errorf("%s: stderr %q", file, se.String())
	}
	return so.String(), code
}

func TestClassifier(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("no python3")
	}
	cases := []struct {
		name   string
		file   string
		flags  []string
		code   int
		wants  []string
		wantNo []string
	}{
		{"the update of a v0.1.1 stack to revision 2", "changeset-v011-to-rev2.json", nil, 0, []string{
			"add      ObjectsBucket (AWS::S3::Bucket)", "add      StorageRole (AWS::IAM::Role)", "add      StorageAssumePolicy (AWS::IAM::Policy)",
			"modify   Instance (AWS::EC2::Instance): Tags, MetadataOptions (no interruption)", "modify   ElasticIp (AWS::EC2::EIP): Tags (no interruption)",
			"modify   InstanceRole (AWS::IAM::Role): Tags (no interruption)", "7 change(s): 7 allowed, 0 refused, 0 blocked"}, []string{"REPLACE", "REMOVE"}},
		{"the same with failover and a peer rule", "changeset-v011-to-rev2-features.json", nil, 0, []string{"add      FencingPolicy (AWS::IAM::Policy)", "add      PeerIngress1 (AWS::EC2::SecurityGroupIngress)", "9 change(s): 9 allowed"}, nil},
		{"the same after a failover", "changeset-v011-to-rev2-features.json", []string{"--failed-over"}, 0, []string{"9 change(s): 9 allowed"}, nil},
		{"instance, volume and address replaced", "changeset-instance-replaced.json", nil, 10, []string{
			"REPLACE  Instance (AWS::EC2::Instance) would be replaced: ImageId", "REPLACE  DataVolume (AWS::EC2::Volume) would be replaced: AvailabilityZone",
			"REPLACE  DataVolumeAttachment (AWS::EC2::VolumeAttachment) would be replaced", "REPLACE  ElasticIpAssociation (AWS::EC2::EIPAssociation) would be replaced",
			"4 change(s): 0 allowed, 4 refused, 0 blocked"}, nil},
		{"a conditional replacement counts as one", "changeset-instance-conditional.json", nil, 10, []string{"REPLACE  Instance (AWS::EC2::Instance) may be replaced: InstanceType"}, nil},
		{"in place but not known to be safe, and a removal", "changeset-in-place-unsafe.json", nil, 10, []string{
			"MODIFY   Instance (AWS::EC2::Instance): UserData changes in place, which this script does not know to be safe",
			"REPLACE  SecurityGroup (AWS::EC2::SecurityGroup) would be replaced: GroupDescription",
			"REMOVE   DnsRecords (AWS::Route53::RecordSetGroup) would be deleted"}, nil},
		{"the address association after a failover is blocked", "changeset-association.json", []string{"--failed-over"}, 11, []string{
			"ElasticIpAssociation (AWS::EC2::EIPAssociation): the service address is on another server after a failover, and this would move it back", "1 allowed, 0 refused, 1 blocked"}, nil},
		{"the address association in place is not on the safe list", "changeset-association.json", nil, 10, []string{"MODIFY   ElasticIpAssociation (AWS::EC2::EIPAssociation): InstanceId changes in place"}, []string{"blocked:"}},
		{"a peer rule that changes is replaced, which is harmless", "changeset-peer-rule.json", nil, 0, []string{
			"replace  PeerIngress1 (AWS::EC2::SecurityGroupIngress): CidrIp", "add      PeerIngress2 (AWS::EC2::SecurityGroupIngress)"}, nil},
		{"actions the script does not know", "changeset-unknown.json", nil, 10, []string{"DYNAMIC  Surprise (AWS::SQS::Queue) is a change this script does not know", "IMPORT   Other", "changes in a way the change set does not describe"}, nil},
		{"the network and the records are not replaced, rules and policies may be", "changeset-replace-network.json", nil, 10, []string{
			"REPLACE  DnsRecords (AWS::Route53::RecordSetGroup) would be replaced: HostedZoneId", "REPLACE  InternetGateway (AWS::EC2::InternetGateway) may be replaced: Tags",
			"REPLACE  DefaultRoute (AWS::EC2::Route) would be replaced: DestinationCidrBlock", "replace  PeerIngress1 (AWS::EC2::SecurityGroupIngress): CidrIp",
			"replace  FencingPolicy (AWS::IAM::Policy): PolicyName", "5 change(s): 2 allowed, 3 refused, 0 blocked"}, nil},
		{"nothing", "changeset-empty.json", nil, 0, []string{"(no resource changes)", "0 change(s)"}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, code := classify(t, c.file, c.flags...)
			if code != c.code {
				t.Errorf("exit %d, want %d\n%s", code, c.code, out)
			}
			for _, w := range c.wants {
				if !strings.Contains(out, w) {
					t.Errorf("output lacks %q:\n%s", w, out)
				}
			}
			for _, w := range c.wantNo {
				if strings.Contains(out, w) {
					t.Errorf("output has %q:\n%s", w, out)
				}
			}
		})
	}
	// The worst findings come first.
	out, _ := classify(t, "changeset-association.json", "--failed-over")
	if strings.Index(out, "ElasticIpAssociation") > strings.Index(out, "modify   Instance") {
		t.Errorf("a blocked change is listed after an allowed one:\n%s", out)
	}
}

// The parameters of an update: every parameter that the stack has and the template declares keeps
// its value; --set gives others; nothing is sent for what is new.
func TestParamsOfAnUpdate(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("no python3")
	}
	script, _ := filepath.Abs("deploy.sh")
	params := func(args ...string) ([]map[string]any, result) {
		t.Helper()
		cmd := exec.Command(bashes(t)[0], append([]string{script, "__params"}, args...)...)
		cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir()}
		var so, se strings.Builder
		cmd.Stdout, cmd.Stderr = &so, &se
		code := 0
		if err := cmd.Run(); err != nil {
			code = err.(*exec.ExitError).ExitCode()
		}
		var out []map[string]any
		_ = json.Unmarshal([]byte(so.String()), &out)
		return out, result{so.String(), se.String(), code}
	}
	tpl := realTemplate(t)
	got, r := params(filepath.Join("testdata", "stack-v011.json"), tpl, "Failover=on", "PeerCidr2=10.0.0.0/8")
	if r.code != 0 {
		t.Fatalf("exit %d\n%s", r.code, r.stderr)
	}
	ps := paramsByKey(got)
	if len(ps) != 18 || ps["KeyEscrowPassphrase"]["UsePreviousValue"] != true || ps["PeerCidr2"]["ParameterValue"] != "10.0.0.0/8" {
		t.Errorf("parameters: %v", got)
	}
	// The order is the template's, so that two runs give the same document.
	if got[0]["ParameterKey"] != "AdminEmail" {
		t.Errorf("first parameter %v", got[0])
	}
	// A value with commas and quotes goes through as one string.
	got, _ = params(filepath.Join("testdata", "stack-v011.json"), tpl, `DomainName=a,b "c"`)
	if paramsByKey(got)["DomainName"]["ParameterValue"] != `a,b "c"` {
		t.Errorf("value: %v", paramsByKey(got)["DomainName"])
	}
	// A stack at revision 2 keeps the parameters it has of the new ones too.
	got, _ = params(filepath.Join("testdata", "stack-rev2.json"), tpl)
	if ps := paramsByKey(got); len(ps) != 28 || ps["Failover"]["UsePreviousValue"] != true {
		t.Errorf("revision 2 stack: %d parameters", len(ps))
	}
	// The template's parameter names are exactly the ones the package of tests expects.
	var names []string
	for _, p := range got {
		names = append(names, p["ParameterKey"].(string))
	}
	if names[len(names)-1] != "VpcId" && names[len(names)-1] != "DataSnapshotId" && names[len(names)-1] != "StorageRoleArn" && names[len(names)-1] != "AvailabilityZone" {
		t.Errorf("last parameter %s: the template's parameter list was not read to its end", names[len(names)-1])
	}
	for _, bad := range []struct{ arg, want string }{{"SupaviseVersion=v2.0.0", "SupaviseVersion cannot be set"}, {"Nope=1", "has no parameter Nope"}, {"Failover", "write NAME=VALUE"}} {
		if _, r := params(filepath.Join("testdata", "stack-v011.json"), tpl, bad.arg); r.code != 2 || !strings.Contains(r.stderr, bad.want) {
			t.Errorf("%s: exit %d %s", bad.arg, r.code, r.stderr)
		}
	}
}
