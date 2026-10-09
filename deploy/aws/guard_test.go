//go:build unix

package awsdeploy_test

// The guard of an update: a change set that CloudFormation decides only while it runs (a stack made
// in the console resolves its image then) runs under a temporary stack policy that denies every
// replacement and removal the review does not allow, and the stack's own policy comes back after.
// Against the stub `aws` of update_test.go; nothing reaches AWS.

import (
	"encoding/json"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const allowAll = `{"Statement": [{"Effect": "Allow", "Action": "Update:*", "Principal": "*", "Resource": "*"}]}`

// consoleFake is the stub for a stack made in the CloudFormation console: its AmiId is empty, and
// the change set of the update is the one AWS gave such a stack (the image is pinned to the one the
// instance runs, which CloudFormation cannot compare with the {{resolve:ssm:...}} value before it
// runs the change set).
func consoleFake(t *testing.T, changeset string) *updFake {
	t.Helper()
	f := newUpdFake(t)
	setStackParam(t, f, "AmiId", "")
	f.copy(changeset, "changeset.json")
	return f
}

func consoleArgs(t *testing.T, more ...string) []string {
	return append([]string{"update", "--stack", "supavise", "--region", "us-east-1", "--template", realTemplate(t), "--yes"}, more...)
}

// policy is the k-th policy the stub was given with set-stack-policy, raw and parsed.
func (f *updFake) policy(k int) (string, []map[string]any) {
	f.t.Helper()
	b, err := os.ReadFile(filepath.Join(f.dir, "policy-"+string(rune('0'+k))+".json"))
	if err != nil {
		f.t.Fatalf("no policy %d was set: %v\ncalls: %v", k, err, f.calls())
	}
	var p struct{ Statement []map[string]any }
	if err := json.Unmarshal(b, &p); err != nil {
		f.t.Fatalf("policy %d is not JSON that set-stack-policy takes: %v\n%s", k, err, b)
	}
	return string(b), p.Statement
}

// denied is the ResourceType list of the Deny statement for action, or nil.
func denied(t *testing.T, statements []map[string]any, action string) []string {
	t.Helper()
	for _, s := range statements {
		if s["Effect"] != "Deny" || s["Action"] != action {
			continue
		}
		if s["Principal"] != "*" || s["Resource"] != "*" {
			t.Errorf("a Deny statement needs Principal * and Resource *: %v", s)
		}
		cond, _ := s["Condition"].(map[string]any)
		eq, _ := cond["StringEquals"].(map[string]any)
		list, _ := eq["ResourceType"].([]any)
		var out []string
		for _, v := range list {
			out = append(out, v.(string))
		}
		return out
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// indexes are the positions of the calls that contain sub.
func (f *updFake) indexes(sub string) []int {
	var out []int
	for i, c := range f.calls() {
		if strings.Contains(c, sub) {
			out = append(out, i)
		}
	}
	return out
}

// The case of the report: a v0.1.3 stack made in the console, updated with v0.2.0's script, was
// refused for "may be replaced" and an in-place AvailabilityZone. It now runs, under the guard.
func TestUpdateOfAConsoleStackRunsUnderTheGuard(t *testing.T) {
	f := consoleFake(t, "changeset-console-stack.json")
	r := f.run(nil, consoleArgs(t)...)
	if r.code != 0 {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s\ncalls: %v", r.code, r.stdout, r.stderr, f.calls())
	}
	for _, want := range []string{
		"The stack has no AmiId: keeping the image its instance runs, ami-0bbbbbbbbbbbbbbbb",
		"guarded  Instance (AWS::EC2::Instance) may be replaced: Tags, ImageId, MetadataOptions",
		"guarded  DataVolume (AWS::EC2::Volume): AvailabilityZone; AvailabilityZone follows Instance",
		"10 change(s): 6 allowed, 0 refused, 0 blocked, 4 guarded",
		"Guarded: CloudFormation decides only while the change set runs",
		"Guard set: a stack policy that denies any replacement or removal",
		"Guard taken off: the stack's policy allows every update again (it had none",
		"Updated.",
	} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, r.stdout)
		}
	}
	if p := paramsByKey(f.params(1))["AmiId"]; p["ParameterValue"] != "ami-0bbbbbbbbbbbbbbbb" {
		t.Errorf("AmiId: %v, want the image the instance runs", p)
	}
	if d := f.callsMatching("describe-change-set"); len(d) != 1 || !strings.Contains(d[0], "--include-property-values") {
		t.Errorf("the change set is read with the values of its properties: %v", d)
	}

	// The order: the stack's own policy is read, the guard set, the change set run and waited for,
	// then the policy put back.
	get, sets, ran, wait := f.indexes("cloudformation get-stack-policy --stack-name supavise"), f.indexes("cloudformation set-stack-policy --stack-name supavise"),
		f.indexes("execute-change-set"), f.indexes("wait stack-update-complete")
	if len(get) != 1 || len(sets) != 2 || len(ran) != 1 || len(wait) != 1 ||
		!(f.first("describe-change-set") < get[0] && get[0] < sets[0] && sets[0] < ran[0] && ran[0] < wait[0] && wait[0] < sets[1]) {
		t.Errorf("get %v, set %v, execute %v, wait %v in:\n%s", get, sets, ran, wait, strings.Join(f.calls(), "\n"))
	}

	// The guard: everything may be updated, nothing replaced or removed but what the review allows.
	_, guard := f.policy(1)
	if len(guard) != 3 || guard[0]["Effect"] != "Allow" || guard[0]["Action"] != "Update:*" || guard[0]["Resource"] != "*" || guard[0]["Principal"] != "*" {
		t.Errorf("the guard of a stack without a policy starts by allowing every update: %v", guard)
	}
	replace, remove := denied(t, guard, "Update:Replace"), denied(t, guard, "Update:Delete")
	for _, typ := range []string{"AWS::EC2::Instance", "AWS::EC2::Volume", "AWS::EC2::VolumeAttachment", "AWS::EC2::EIP", "AWS::EC2::EIPAssociation",
		"AWS::S3::Bucket", "AWS::IAM::Role", "AWS::EC2::SecurityGroup", "AWS::EC2::VPC", "AWS::SecretsManager::Secret"} {
		if !contains(replace, typ) || !contains(remove, typ) {
			t.Errorf("the guard does not deny the replacement and removal of %s: %v / %v", typ, replace, remove)
		}
	}
	// Exactly the review's allow-lists: a rule and two policies may be replaced, a rule and an IAM
	// policy removed.
	for _, typ := range []string{"AWS::EC2::SecurityGroupIngress", "AWS::IAM::Policy", "AWS::S3::BucketPolicy"} {
		if contains(replace, typ) {
			t.Errorf("the guard denies the replacement of %s, which the review allows", typ)
		}
	}
	if contains(remove, "AWS::EC2::SecurityGroupIngress") || contains(remove, "AWS::IAM::Policy") || !contains(remove, "AWS::S3::BucketPolicy") {
		t.Errorf("removals denied: %v", remove)
	}
	// A stack without a policy gets one that allows every update (a stack policy cannot be deleted).
	if raw, _ := f.policy(2); raw != allowAll {
		t.Errorf("the policy put back: %s", raw)
	}
}

// A stack with a policy of its own keeps it: the guard adds its denials to it (a Deny wins over an
// Allow, so nothing the stack's policy denies becomes allowed), and the policy comes back as it was.
func TestUpdateKeepsTheStacksOwnPolicy(t *testing.T) {
	f := consoleFake(t, "changeset-console-stack.json")
	own := "{\n  \"Statement\" : [\n    {\"Effect\": \"Allow\", \"Action\": \"Update:*\", \"Principal\": \"*\", \"Resource\": \"*\"},\n" +
		"    {\"Effect\": \"Deny\", \"Action\": \"Update:*\", \"Principal\": \"*\", \"Resource\": \"LogicalResourceId/BackupBucket\"}\n  ]\n}\n"
	if err := os.WriteFile(filepath.Join(f.dir, "stack-policy.json"), []byte(own), 0o644); err != nil {
		t.Fatal(err)
	}
	r := f.run(nil, consoleArgs(t)...)
	if r.code != 0 || !strings.Contains(r.stdout, "Guard set: the stack's own policy, plus") || !strings.Contains(r.stdout, "Guard taken off: the stack's own policy is back") {
		t.Fatalf("exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	_, guard := f.policy(1)
	if len(guard) != 4 || guard[1]["Resource"] != "LogicalResourceId/BackupBucket" || guard[1]["Effect"] != "Deny" {
		t.Errorf("the guard holds the stack's own statements first: %v", guard)
	}
	if raw, _ := f.policy(2); raw != own {
		t.Errorf("the stack's policy was not put back as it was:\n%q\nwant\n%q", raw, own)
	}
}

// CloudFormation decides to replace the instance while the change set runs: the guard denies it, the
// update fails and rolls back, and the policy is put back all the same.
func TestUpdateGuardedFailureRollsBackAndPutsThePolicyBack(t *testing.T) {
	events := `{"StackEvents": [
  {"LogicalResourceId": "supavise", "ResourceType": "AWS::CloudFormation::Stack", "ResourceStatus": "UPDATE_ROLLBACK_COMPLETE"},
  {"LogicalResourceId": "supavise", "ResourceType": "AWS::CloudFormation::Stack", "ResourceStatus": "UPDATE_ROLLBACK_IN_PROGRESS", "ResourceStatusReason": "The following resource(s) failed to update: [Instance]."},
  {"LogicalResourceId": "Instance", "ResourceType": "AWS::EC2::Instance", "ResourceStatus": "UPDATE_FAILED", "ResourceStatusReason": "Action denied by stack policy: Update:Replace on LogicalResourceId/Instance"},
  {"LogicalResourceId": "supavise", "ResourceType": "AWS::CloudFormation::Stack", "ResourceStatus": "UPDATE_IN_PROGRESS", "ResourceStatusReason": "User Initiated"},
  {"LogicalResourceId": "Instance", "ResourceType": "AWS::EC2::Instance", "ResourceStatus": "UPDATE_FAILED", "ResourceStatusReason": "a failure of an older update"}
]}`
	f := consoleFake(t, "changeset-console-stack.json")
	if err := os.WriteFile(filepath.Join(f.dir, "events.json"), []byte(events), 0o644); err != nil {
		t.Fatal(err)
	}
	r := f.run([]string{"STACK_WAIT_RC=255", "NEW_STACK_STATUS=UPDATE_ROLLBACK_COMPLETE"}, consoleArgs(t)...)
	if r.code != 3 {
		t.Fatalf("exit %d, want 3 (failed)\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	for _, want := range []string{
		"The update failed, and CloudFormation rolled the stack back: it is as it was before the update.",
		"Nothing was replaced: the guard let CloudFormation replace or remove nothing but a security group rule, an IAM policy or a bucket policy.",
		"CloudFormation decided to replace or remove these while the change set ran, and the guard denied it:",
		"  Instance: Action denied by stack policy: Update:Replace on LogicalResourceId/Instance",
		"Guard taken off",
	} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, r.stdout)
		}
	}
	if strings.Contains(r.stdout, "older update") {
		t.Errorf("an event of an earlier update was reported:\n%s", r.stdout)
	}
	if !strings.Contains(r.stderr, "did not complete (UPDATE_ROLLBACK_COMPLETE)") {
		t.Errorf("stderr: %s", r.stderr)
	}
	sets, wait := f.indexes("set-stack-policy"), f.indexes("wait stack-update-complete")
	if len(sets) != 2 || len(wait) != 1 || sets[1] < wait[0] {
		t.Errorf("the policy is put back after the update has ended: set %v, wait %v", sets, wait)
	}
	if raw, _ := f.policy(2); raw != allowAll {
		t.Errorf("the policy put back: %s", raw)
	}

	// A rollback that does not finish says so, and how to resume it.
	f = consoleFake(t, "changeset-console-stack.json")
	r = f.run([]string{"STACK_WAIT_RC=255", "NEW_STACK_STATUS=UPDATE_ROLLBACK_FAILED"}, consoleArgs(t)...)
	if r.code != 3 || !strings.Contains(r.stdout, "could not finish rolling the stack back") || !strings.Contains(r.stdout, "continue-update-rollback --stack-name supavise") ||
		!strings.Contains(r.stdout, "Nothing was replaced") || len(f.callsMatching("set-stack-policy")) != 2 {
		t.Errorf("exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}

	// The waiter gave up but the update completed: that is success.
	f = consoleFake(t, "changeset-console-stack.json")
	r = f.run([]string{"STACK_WAIT_RC=255", "NEW_STACK_STATUS=UPDATE_COMPLETE"}, consoleArgs(t)...)
	if r.code != 0 || !strings.Contains(r.stdout, "Updated.") || len(f.callsMatching("set-stack-policy")) != 2 {
		t.Errorf("exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
}

// The policy cannot be put back: the script says so loudly, with the command that does it.
func TestUpdateSaysLoudlyWhenThePolicyCannotBePutBack(t *testing.T) {
	cmd := `aws --region us-east-1 cloudformation set-stack-policy --stack-name supavise --stack-policy-body '` + allowAll + `'`
	f := consoleFake(t, "changeset-console-stack.json")
	r := f.run([]string{"SET_POLICY_FAILS_AT=2"}, consoleArgs(t)...)
	// The update itself went through: the stack is at the new template, and the guard only denies
	// what this script refuses anyway.
	if r.code != 0 || !strings.Contains(r.stdout, "Updated.") {
		t.Fatalf("exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	if !strings.Contains(r.stderr, "WARNING: THE GUARD IS STILL ON STACK supavise (it could not be put back)") || !strings.Contains(r.stderr, cmd) {
		t.Errorf("stderr lacks the warning or the command %q:\n%s", cmd, r.stderr)
	}
	// The same after a failed update: exit 3, and both are said.
	f = consoleFake(t, "changeset-console-stack.json")
	r = f.run([]string{"SET_POLICY_FAILS_AT=2", "STACK_WAIT_RC=255", "NEW_STACK_STATUS=UPDATE_ROLLBACK_COMPLETE"}, consoleArgs(t)...)
	if r.code != 3 || !strings.Contains(r.stdout, "rolled the stack back") || !strings.Contains(r.stderr, "THE GUARD IS STILL ON") || !strings.Contains(r.stderr, cmd) {
		t.Errorf("exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
}

// Without the guard a guarded change set does not run.
func TestUpdateRunsNothingWhenTheGuardCannotBeSet(t *testing.T) {
	for _, c := range []struct{ env, want string }{
		{"GET_POLICY_FAILS=1", "cloudformation:GetStackPolicy"},
		{"SET_POLICY_FAILS_AT=1", "cloudformation:SetStackPolicy"},
	} {
		f := consoleFake(t, "changeset-console-stack.json")
		r := f.run([]string{c.env}, consoleArgs(t)...)
		if r.code != 3 || !strings.Contains(r.stderr, c.want) || !strings.Contains(r.stderr, "Nothing was changed") {
			t.Errorf("%s: exit %d\n%s", c.env, r.code, r.stderr)
		}
		if len(f.callsMatching("execute-change-set")) != 0 || len(f.callsMatching("delete-change-set")) != 1 {
			t.Errorf("%s: the change set must be deleted and never run: %v", c.env, f.calls())
		}
		// A guard that was never set is not "put back".
		if n := len(f.callsMatching("set-stack-policy")); (c.env == "GET_POLICY_FAILS=1" && n != 0) || (c.env == "SET_POLICY_FAILS_AT=1" && n != 1) {
			t.Errorf("%s: %d set-stack-policy calls", c.env, n)
		}
	}
}

// The change set cannot be run after the guard is set: the guard comes off on the way out.
func TestUpdateTakesTheGuardOffWhenTheRunFails(t *testing.T) {
	f := consoleFake(t, "changeset-console-stack.json")
	b, _ := os.ReadFile(filepath.Join(f.dir, "aws"))
	stub := strings.Replace(string(b), `  *"cloudformation wait change-set-create-complete"*)`,
		`  *"cloudformation execute-change-set"*) echo "An error occurred (InvalidChangeSetStatus) when calling the ExecuteChangeSet operation" >&2; exit 254 ;;
  *"cloudformation wait change-set-create-complete"*)`, 1)
	if err := os.WriteFile(filepath.Join(f.dir, "aws"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	r := f.run([]string{"NEW_STACK_STATUS=UPDATE_COMPLETE"}, consoleArgs(t)...)
	if r.code != 3 || !strings.Contains(r.stderr, "cannot run the change set") || !strings.Contains(r.stdout, "Guard taken off") {
		t.Errorf("exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	if len(f.callsMatching("set-stack-policy")) != 2 {
		t.Errorf("calls: %v", f.calls())
	}
}

// --allow-risky: the person accepts replacements, so no guard is set. It still asks for the stack
// name on a terminal.
func TestUpdateAllowRiskySetsNoGuard(t *testing.T) {
	f := consoleFake(t, "changeset-console-stack.json")
	r := f.runTTY(nil, "supavise\n", consoleArgs(t, "--allow-risky")...)
	if r.code != 0 {
		t.Fatalf("exit %d\n%s\ncalls: %v", r.code, r.stdout, f.calls())
	}
	for _, want := range []string{"4 guarded", "--allow-risky: no guard is set for this run", "Type the stack name (supavise)", "Updated."} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("output lacks %q:\n%s", want, r.stdout)
		}
	}
	if len(f.callsMatching("stack-policy")) != 0 || len(f.callsMatching("execute-change-set")) != 1 {
		t.Errorf("calls: %v", f.calls())
	}
	// A wrong name runs nothing.
	f = consoleFake(t, "changeset-console-stack.json")
	r = f.runTTY(nil, "nope\n", consoleArgs(t, "--allow-risky")...)
	if r.code != 2 || !strings.Contains(r.stdout, "not confirmed") || len(f.callsMatching("execute-change-set")) != 0 {
		t.Errorf("exit %d\n%s", r.code, r.stdout)
	}
}

// What the guard is not for: a change set that is refused is never run, with or without one.
func TestUpdateRefusedChangeSetsSetNoGuard(t *testing.T) {
	for _, file := range []string{"changeset-instance-replaced.json", "changeset-instance-conditional.json", "changeset-in-place-unsafe.json", "changeset-replace-network.json"} {
		f := consoleFake(t, file)
		r := f.run(nil, consoleArgs(t)...)
		if r.code != 2 || len(f.callsMatching("stack-policy")) != 0 || len(f.callsMatching("execute-change-set")) != 0 {
			t.Errorf("%s: exit %d, calls %v", file, r.code, f.calls())
		}
	}
	// An allowed change set needs no guard either.
	f := newUpdFake(t)
	if r := f.run(nil, consoleArgs(t)...); r.code != 0 || len(f.callsMatching("stack-policy")) != 0 {
		t.Errorf("an allowed change set: exit %d, calls %v", r.code, f.calls())
	}
}

// The values CloudFormation shows for the image differ: the instance would get another image, which
// is a replacement, so the change set is refused before anything runs.
func TestUpdateRefusesAnotherImage(t *testing.T) {
	f := consoleFake(t, "changeset-console-stack-values.json")
	path := filepath.Join(f.dir, "changeset.json")
	b, _ := os.ReadFile(path)
	b = []byte(strings.Replace(string(b), `"AfterValue": "ami-0bbbbbbbbbbbbbbbb"`, `"AfterValue": "ami-0cccccccccccccccc"`, 1))
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	r := f.run(nil, consoleArgs(t)...)
	if r.code != 2 || !strings.Contains(r.stdout, "REPLACE  Instance (AWS::EC2::Instance) would be replaced: ImageId changes from ami-0bbbbbbbbbbbbbbbb to ami-0cccccccccccccccc, another image") ||
		!strings.Contains(r.stderr, "Nothing was changed") {
		t.Errorf("exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	if len(f.callsMatching("stack-policy")) != 0 || len(f.callsMatching("execute-change-set")) != 0 {
		t.Errorf("calls: %v", f.calls())
	}
}

// An AWS CLI from before --include-property-values: the review goes on without the values.
func TestUpdateWithAnOlderCLI(t *testing.T) {
	f := consoleFake(t, "changeset-console-stack.json")
	r := f.run([]string{"OLD_CLI=1"}, consoleArgs(t)...)
	if r.code != 0 || !strings.Contains(r.stdout, "This AWS CLI does not know --include-property-values") || !strings.Contains(r.stdout, "4 guarded") ||
		!strings.Contains(r.stdout, "Guard taken off") {
		t.Fatalf("exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	d := f.callsMatching("describe-change-set")
	if len(d) != 2 || !strings.Contains(d[0], "--include-property-values") || strings.Contains(d[1], "--include-property-values") {
		t.Errorf("describe-change-set: %v", d)
	}
	// An update with nothing to guard, on the same CLI.
	f = newUpdFake(t)
	if r = f.run([]string{"OLD_CLI=1"}, consoleArgs(t)...); r.code != 0 || len(f.callsMatching("execute-change-set")) != 1 {
		t.Errorf("exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
}

// The edges of the guarded verdict, on variations of the console stack's change set.
func TestClassifierGuardedRule(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("no python3")
	}
	type change = map[string]any
	load := func(file string) change {
		b, err := os.ReadFile(filepath.Join("testdata", file))
		if err != nil {
			t.Fatal(err)
		}
		var cs change
		if err := json.Unmarshal(b, &cs); err != nil {
			t.Fatal(err)
		}
		return cs
	}
	resource := func(cs change, id string) change {
		for _, c := range cs["Changes"].([]any) {
			rc := c.(change)["ResourceChange"].(change)
			if rc["LogicalResourceId"] == id {
				return rc
			}
		}
		t.Fatalf("no %s", id)
		return nil
	}
	detail := func(attr, name, need, eval, source, cause string) change {
		d := change{"Target": change{"Attribute": attr, "Name": name, "RequiresRecreation": need}, "Evaluation": eval, "ChangeSource": source}
		if cause != "" {
			d["CausingEntity"] = cause
		}
		return d
	}
	write := func(cs change) string {
		b, _ := json.Marshal(cs)
		p := filepath.Join(t.TempDir(), "cs.json")
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cases := []struct {
		name   string
		file   string
		change func(cs change)
		code   int
		wants  []string
	}{
		{"the image values differ: a new image is a replacement", "changeset-console-stack-values.json", func(cs change) {
			for _, d := range resource(cs, "Instance")["Details"].([]any) {
				if tg := d.(change)["Target"].(change); tg["Name"] == "ImageId" {
					tg["AfterValue"] = "ami-0cccccccccccccccc"
				}
			}
		}, 10, []string{"REPLACE  Instance (AWS::EC2::Instance) would be replaced: ImageId changes from ami-0bbbbbbbbbbbbbbbb to ami-0cccccccccccccccc, another image",
			"guarded  DataVolumeAttachment"}},
		{"the image values from the resource's context differ", "changeset-console-stack-values.json", func(cs change) {
			rc := resource(cs, "Instance")
			for _, d := range rc["Details"].([]any) {
				tg := d.(change)["Target"].(change)
				delete(tg, "BeforeValue")
				delete(tg, "AfterValue")
			}
			rc["AfterContext"] = `{"Properties":{"ImageId":"ami-0cccccccccccccccc"}}`
		}, 10, []string{"would be replaced: ImageId changes from ami-0bbbbbbbbbbbbbbbb to ami-0cccccccccccccccc"}},
		{"values that are not image IDs cannot be compared: still guarded", "changeset-console-stack-values.json", func(cs change) {
			for _, d := range resource(cs, "Instance")["Details"].([]any) {
				if tg := d.(change)["Target"].(change); tg["Name"] == "ImageId" {
					tg["BeforeValue"] = "{{resolve:ssm:/aws/service/canonical/ubuntu/server/24.04/stable/current/arm64/hvm/ebs-gp3/ami-id}}"
				}
			}
		}, 12, []string{"guarded  Instance (AWS::EC2::Instance) may be replaced", "which cannot be compared before it runs"}},
		{"a definite replacement of the instance is refused, and what refers to it follows for certain", "changeset-console-stack.json", func(cs change) {
			rc := resource(cs, "Instance")
			rc["Replacement"] = "True"
			rc["Details"] = append(rc["Details"].([]any), detail("Properties", "ImageId", "Always", "Static", "ParameterReference", "AmiId"))
		}, 10, []string{"REPLACE  Instance (AWS::EC2::Instance) would be replaced: Tags, ImageId, MetadataOptions",
			"REPLACE  DataVolumeAttachment (AWS::EC2::VolumeAttachment) may be replaced: InstanceId", "MODIFY   DataVolume (AWS::EC2::Volume): AvailabilityZone changes in place"}},
		{"user data that may change in place is refused: the guard does not stop a change in place", "changeset-console-stack.json", func(cs change) {
			rc := resource(cs, "Instance")
			rc["Details"] = append(rc["Details"].([]any), detail("Properties", "UserData", "Conditionally", "Dynamic", "DirectModification", ""))
		}, 10, []string{"MODIFY   Instance (AWS::EC2::Instance): UserData changes in place, which this script does not know to be safe"}},
		{"an instance type resolved at run time is refused for the same reason", "changeset-console-stack.json", func(cs change) {
			rc := resource(cs, "Instance")
			rc["Details"] = append(rc["Details"].([]any), detail("Properties", "InstanceType", "Conditionally", "Dynamic", "ParameterReference", "InstanceType"))
		}, 10, []string{"MODIFY   Instance (AWS::EC2::Instance): InstanceType changes in place"}},
		{"a reference to a resource the change set adds changes for certain", "changeset-console-stack.json", func(cs change) {
			resource(cs, "DataVolumeAttachment")["Details"] = []any{detail("Properties", "InstanceId", "Always", "Dynamic", "ResourceReference", "StorageRole")}
		}, 10, []string{"REPLACE  DataVolumeAttachment (AWS::EC2::VolumeAttachment) may be replaced: InstanceId"}},
		{"a reference to a resource that is not in the change set is not explained", "changeset-console-stack.json", func(cs change) {
			resource(cs, "DataVolume")["Details"] = []any{detail("Properties", "AvailabilityZone", "Never", "Dynamic", "ResourceAttribute", "Elsewhere.AvailabilityZone")}
		}, 10, []string{"MODIFY   DataVolume (AWS::EC2::Volume): AvailabilityZone changes in place"}},
		{"a conditional replacement that no detail explains is refused", "changeset-console-stack.json", func(cs change) {
			rc := resource(cs, "ElasticIp")
			rc["Replacement"] = "Conditional"
		}, 10, []string{"REPLACE  ElasticIp (AWS::EC2::EIP) may be replaced: Tags"}},
		{"a direct change of a safe property resolved at run time is allowed", "changeset-console-stack.json", func(cs change) {
			resource(cs, "ElasticIp")["Details"] = []any{detail("Tags", "", "Never", "Dynamic", "DirectModification", "")}
		}, 12, []string{"modify   ElasticIp (AWS::EC2::EIP): Tags (no interruption)"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cs := load(c.file)
			c.change(cs)
			out, code := classify(t, write(cs))
			if code != c.code {
				t.Errorf("exit %d, want %d\n%s", code, c.code, out)
			}
			for _, w := range c.wants {
				if !strings.Contains(out, w) {
					t.Errorf("output lacks %q:\n%s", w, out)
				}
			}
		})
	}
}

// ptyRun runs a command on a terminal (a pseudo-terminal) and types input into it: --allow-risky
// asks for the stack name, and only on a terminal. Output and errors come back together.
const ptyRun = `import os, pty, sys
pid, fd = pty.fork()
if pid == 0:
    os.execv(sys.argv[2], sys.argv[2:])
os.write(fd, sys.argv[1].encode())
out = b""
while True:
    try:
        b = os.read(fd, 65536)
    except OSError:
        break
    if not b:
        break
    out += b
_, st = os.waitpid(pid, 0)
sys.stdout.write(out.decode("utf-8", "replace"))
sys.exit(os.WEXITSTATUS(st) if os.WIFEXITED(st) else 128 + os.WTERMSIG(st))
`

func (f *updFake) runTTY(env []string, input string, args ...string) result {
	f.t.Helper()
	py, err := exec.LookPath("python3")
	if err != nil {
		f.t.Skip("no python3")
	}
	inner := f.command(env, args...)
	cmd := exec.Command(py, append([]string{"-c", ptyRun, input, inner.Path}, inner.Args[1:]...)...)
	cmd.Env = inner.Env
	var so strings.Builder
	cmd.Stdout, cmd.Stderr = &so, &so
	err = cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		f.t.Fatal(err)
	}
	return result{so.String(), "", code}
}

// interrupted runs an update that hangs in its wait for the stack, and interrupts it (Ctrl-C: SIGINT
// to the process group) each time the stub starts a wait that hangs, `times` times.
func interrupted(t *testing.T, f *updFake, env []string, times int) result {
	t.Helper()
	if signal.Ignored(syscall.SIGINT) {
		t.Skip("SIGINT is ignored in this process, and so it would be in the script")
	}
	cmd := f.command(env, consoleArgs(t)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var so, se strings.Builder
	cmd.Stdout, cmd.Stderr = &so, &se
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	for k := 1; k <= times; k++ {
		mark := filepath.Join(f.dir, "waiting-"+string(rune('0'+k)))
		deadline := time.Now().Add(20 * time.Second)
		for {
			if _, err := os.Stat(mark); err == nil {
				break
			}
			if time.Now().After(deadline) {
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				_ = cmd.Wait()
				t.Fatalf("wait %d never started\nstdout: %s\nstderr: %s\ncalls: %v", k, so.String(), se.String(), f.calls())
			}
			time.Sleep(50 * time.Millisecond)
		}
		time.Sleep(200 * time.Millisecond) // let the stub reach its sleep
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGINT); err != nil {
			t.Fatal(err)
		}
	}
	err := cmd.Wait()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return result{so.String(), se.String(), code}
}

// Ctrl-C while the guarded update runs: the update goes on in CloudFormation, so the guard stays on
// until it has ended, and then comes off.
func TestUpdateInterruptedTakesTheGuardOffOnceTheUpdateHasEnded(t *testing.T) {
	f := consoleFake(t, "changeset-console-stack.json")
	r := interrupted(t, f, []string{"WAIT_HANGS=1", "STATUS_SEQ=UPDATE_IN_PROGRESS UPDATE_COMPLETE"}, 1)
	if r.code != 130 {
		t.Fatalf("exit %d, want 130\nstdout: %s\nstderr: %s\ncalls: %v", r.code, r.stdout, r.stderr, f.calls())
	}
	if !strings.Contains(r.stderr, "the update of supavise is still running (UPDATE_IN_PROGRESS); the guard comes off when it ends") || !strings.Contains(r.stdout, "Guard taken off") {
		t.Errorf("stdout: %s\nstderr: %s", r.stdout, r.stderr)
	}
	sets, waits, status := f.indexes("set-stack-policy"), f.indexes("wait stack-update-complete"), f.indexes("Stacks[0].StackStatus")
	if len(sets) != 2 || len(waits) != 2 || len(status) != 2 || !(waits[0] < status[0] && status[0] < waits[1] && waits[1] < status[1] && status[1] < sets[1]) {
		t.Errorf("set %v, wait %v, status %v in:\n%s", sets, waits, status, strings.Join(f.calls(), "\n"))
	}
	if raw, _ := f.policy(2); raw != allowAll {
		t.Errorf("the policy put back: %s", raw)
	}
}

// A second Ctrl-C stops the wait: the guard stays on, and the script says how to take it off.
func TestUpdateInterruptedTwiceLeavesTheGuardOnAndSaysSo(t *testing.T) {
	f := consoleFake(t, "changeset-console-stack.json")
	r := interrupted(t, f, []string{"WAIT_HANGS=2", "STATUS_SEQ=UPDATE_IN_PROGRESS"}, 2)
	if r.code != 130 {
		t.Fatalf("exit %d, want 130\nstdout: %s\nstderr: %s", r.code, r.stdout, r.stderr)
	}
	if !strings.Contains(r.stderr, "WARNING: THE GUARD IS STILL ON STACK supavise (the update may still be running") ||
		!strings.Contains(r.stderr, "cloudformation set-stack-policy --stack-name supavise --stack-policy-body '"+allowAll+"'") {
		t.Errorf("stderr: %s", r.stderr)
	}
	if n := len(f.callsMatching("set-stack-policy")); n != 1 {
		t.Errorf("the guard was taken off during the update (%d set-stack-policy calls)", n)
	}
}
