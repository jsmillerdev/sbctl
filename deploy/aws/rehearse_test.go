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
case "$1" in
  update)
    n=$(grep -c '^update' "$DIR/deploy.log")
    if [ "$n" -ge 2 ]; then echo "Nothing to change: the stack already matches this template."; exit 0; fi
    echo "  add      ObjectsBucket (AWS::S3::Bucket)"
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
		"PASS  a second update finds nothing to change", "== every check passed", "supavise:infra=2", "associate-address --dry-run --allocation-id eipalloc-0123456789abcdef0"} {
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
	log := strings.Join(fileLines(t, filepath.Join(dir, "aws.log")), "\n")
	for _, want := range []string{"s3api delete-objects --bucket supavise-rehearsal-backup", "s3api delete-bucket --bucket supavise-rehearsal-backup",
		"s3api delete-bucket --bucket supavise-rehearsal-objects", "ec2 delete-snapshot --snapshot-id snap-1", "ec2 delete-snapshot --snapshot-id snap-2"} {
		if !strings.Contains(log, want) {
			t.Errorf("--purge did not run %q:\n%s", want, log)
		}
	}
}
