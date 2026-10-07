// Package awsdeploy_test runs deploy.sh offline: the argument parsing, the --dry-run output and
// the real code path against a stub `aws` that records its calls. No AWS account, credentials or
// network are involved. The script is also run under /bin/bash where that is bash 3.2 (macOS),
// since people with the AWS CLI often run it there.
package awsdeploy_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func bashes(t *testing.T) []string {
	t.Helper()
	seen := map[string]bool{}
	var out []string
	for _, b := range []string{"/bin/bash"} {
		if _, err := os.Stat(b); err == nil && !seen[b] {
			seen[b] = true
			out = append(out, b)
		}
	}
	if p, err := exec.LookPath("bash"); err == nil && !seen[p] {
		out = append(out, p)
	}
	if len(out) == 0 {
		t.Skip("no bash")
	}
	return out
}

type result struct {
	stdout, stderr string
	code           int
}

// stubAWS writes an `aws` that logs every call and answers like the real CLI would for the few
// calls deploy.sh makes. STACK_EXISTS=1 makes describe-stacks find a stack; STOP_FAILS=1 makes
// stop-instances fail.
func stubAWS(t *testing.T) (dir, log string) {
	t.Helper()
	dir = t.TempDir()
	log = filepath.Join(dir, "calls.log")
	script := `#!/bin/bash
echo "$*" >> "` + log + `"
case "$*" in
  *"Stacks[0].StackStatus"*)
    if [ -n "$STACK_EXISTS" ]; then echo CREATE_COMPLETE; exit 0; fi
    echo "An error occurred (ValidationError) when calling the DescribeStacks operation: Stack with id sbctl does not exist" >&2
    exit 254 ;;
  *"Stacks[0].Parameters"*)
    printf 'AdminEmail\ta@b.co\nAmiId\t%s\nInstanceType\tt4g.large\n' "${STACK_AMI-ami-0aaaaaaaaaaaaaaaa}" ;;
  *"Stacks[0].Outputs"*)
    printf 'DashboardUrl\thttps://studio.1.2.3.4.sslip.io\nClaimUrl\thttps://api.1.2.3.4.sslip.io/claim\n'
    printf 'ClaimTokenCommand\taws secretsmanager get-secret-value --region us-east-1 --secret-id arn:aws:secretsmanager:us-east-1:111122223333:secret:x --query SecretString --output text\n'
    printf 'DnsRecordsNeeded\tNot needed (sslip.io resolves the Elastic IP)\n'
    printf 'ConnectCommand\taws ssm start-session --region us-east-1 --target i-0abc\n'
    printf 'InstanceId\ti-0123456789abcdef0\nBackupBucket\tsbctl-backupbucket-xyz\nDataVolumeId\tvol-0123456789abcdef0\n' ;;
  *"ssm get-parameter"*) echo ami-0123456789abcdef0 ;;
  *"ec2 describe-instances"*) echo ami-0bbbbbbbbbbbbbbbb ;;
  *"ec2 stop-instances"*)
    if [ -n "$STOP_FAILS" ]; then echo "An error occurred (IncorrectInstanceState) when calling the StopInstances operation" >&2; exit 254; fi ;;
  *) ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "aws"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir, log
}

func run(t *testing.T, bash string, stubDir string, env []string, args ...string) result {
	t.Helper()
	script, _ := filepath.Abs("deploy.sh")
	cmd := exec.Command(bash, append([]string{script}, args...)...)
	path := "/usr/bin:/bin"
	if stubDir != "" {
		path = stubDir + ":" + path
	}
	// A clean environment: no AWS credentials, region or profile can leak in.
	cmd.Env = append([]string{"PATH=" + path, "HOME=" + t.TempDir(), "TMPDIR=" + t.TempDir()}, env...)
	var so, se strings.Builder
	cmd.Stdout, cmd.Stderr = &so, &se
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return result{so.String(), se.String(), code}
}

func calls(t *testing.T, log string) []string {
	t.Helper()
	b, err := os.ReadFile(log)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func TestArgumentErrors(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no region", []string{"--email", "a@b.co"}, "--region is required"},
		{"bad region", []string{"--region", "mars", "--email", "a@b.co"}, "not a region name"},
		{"no email", []string{"--region", "us-east-1"}, "--email is required"},
		{"bad email", []string{"--region", "us-east-1", "--email", "nope"}, "not an email address"},
		{"zone without domain", []string{"--region", "us-east-1", "--email", "a@b.co", "--hosted-zone-id", "Z0123456789ABCDEFGHIJ"}, "needs --domain"},
		{"bad zone", []string{"--region", "us-east-1", "--email", "a@b.co", "--domain", "example.com", "--hosted-zone-id", "abc"}, "--hosted-zone-id must look like"},
		{"bad domain", []string{"--region", "us-east-1", "--email", "a@b.co", "--domain", "Example"}, "--domain must be"},
		{"bad stack", []string{"--region", "us-east-1", "--email", "a@b.co", "--stack-name", "1bad"}, "--stack-name must start"},
		{"bad type", []string{"--region", "us-east-1", "--email", "a@b.co", "--instance-type", "large"}, "--instance-type"},
		{"bad version", []string{"--region", "us-east-1", "--email", "a@b.co", "--version", "main"}, "--version must be"},
		{"bad cidr", []string{"--region", "us-east-1", "--email", "a@b.co", "--access-cidr", "10.0.0.0"}, "--access-cidr must be"},
		{"ssh without key", []string{"--region", "us-east-1", "--email", "a@b.co", "--ssh-cidr", "10.0.0.1/32"}, "--ssh-cidr needs --key-name"},
		{"bad volume", []string{"--region", "us-east-1", "--email", "a@b.co", "--volume-size", "5"}, "--volume-size must be"},
		{"bad snapshot count", []string{"--region", "us-east-1", "--email", "a@b.co", "--daily-snapshots", "many"}, "--daily-snapshots must be"},
		{"too many snapshots", []string{"--region", "us-east-1", "--email", "a@b.co", "--daily-snapshots", "1001"}, "--daily-snapshots must be"},
		{"bad ami", []string{"--region", "us-east-1", "--email", "a@b.co", "--ami-id", "ubuntu"}, "--ami-id must look like"},
		{"bad snapshot", []string{"--region", "us-east-1", "--email", "a@b.co", "--data-snapshot-id", "x"}, "--data-snapshot-id must look like"},
		{"missing value", []string{"--region"}, "--region needs a value"},
		{"unknown option", []string{"--region", "us-east-1", "--bogus"}, "unknown option: --bogus"},
		{"delete with create options", []string{"--region", "us-east-1", "--delete", "--email", "a@b.co"}, "--delete takes only"},
		{"yes without delete", []string{"--region", "us-east-1", "--email", "a@b.co", "--yes"}, "--yes belongs to --delete"},
	}
	for _, b := range bashes(t) {
		for _, c := range cases {
			t.Run(filepath.Base(b)+"/"+c.name, func(t *testing.T) {
				dir, log := stubAWS(t)
				r := run(t, b, dir, nil, append(c.args, "--dry-run")...)
				if r.code != 2 {
					t.Errorf("exit %d, want 2\nstdout: %s\nstderr: %s", r.code, r.stdout, r.stderr)
				}
				if !strings.Contains(r.stderr, c.want) {
					t.Errorf("stderr lacks %q: %s", c.want, r.stderr)
				}
				if n := calls(t, log); len(n) != 0 {
					t.Errorf("a refused command called aws: %v", n)
				}
			})
		}
	}
}

func TestHelp(t *testing.T) {
	for _, b := range bashes(t) {
		r := run(t, b, "", nil, "--help")
		if r.code != 0 || !strings.Contains(r.stdout, "--hosted-zone-id") || !strings.Contains(r.stdout, "--delete") {
			t.Errorf("%s --help: exit %d\n%s%s", b, r.code, r.stdout, r.stderr)
		}
	}
}

// dryRun runs deploy.sh --dry-run with the stub on the path and checks that it never called aws.
func dryRun(t *testing.T, bash string, env []string, args ...string) string {
	t.Helper()
	dir, log := stubAWS(t)
	r := run(t, bash, dir, env, append(args, "--dry-run")...)
	if r.code != 0 {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", r.code, r.stdout, r.stderr)
	}
	if n := calls(t, log); len(n) != 0 {
		t.Fatalf("--dry-run called aws: %v", n)
	}
	return r.stdout
}

func TestDryRunMinimal(t *testing.T) {
	for _, b := range bashes(t) {
		out := dryRun(t, b, nil, "--region", "us-east-1", "--email", "you@example.com")
		for _, want := range []string{
			"# dry run: nothing is sent to AWS",
			"aws --region us-east-1 cloudformation deploy --stack-name sbctl --template-file ",
			"/cloudformation/sbctl.yaml --capabilities CAPABILITY_IAM --no-fail-on-empty-changeset --tags Application=sbctl --parameter-overrides AdminEmail=you@example.com 'AmiId=<image-id>'",
			// The default instance type is Graviton, so the lookup asks for arm64.
			"aws --region us-east-1 ssm get-parameter --name /aws/service/canonical/ubuntu/server/24.04/stable/current/arm64/hvm/ebs-gp3/ami-id --query Parameter.Value --output text",
			"aws --region us-east-1 cloudformation describe-stacks --stack-name sbctl --query 'Stacks[0].Outputs[].[OutputKey,OutputValue]' --output text",
			// The lookups the real run makes are printed too: does the stack exist, which image
			// does it hold and, when AmiId is empty, which image its instance runs.
			"aws --region us-east-1 cloudformation describe-stacks --stack-name sbctl --query 'Stacks[0].StackStatus' --output text",
			"aws --region us-east-1 cloudformation describe-stacks --stack-name sbctl --query 'Stacks[0].Parameters[].[ParameterKey,ParameterValue]' --output text",
			"aws --region us-east-1 ec2 describe-instances --instance-ids '<InstanceId>' --query 'Reservations[0].Instances[0].ImageId' --output text",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("%s: output lacks %q:\n%s", b, want, out)
			}
		}
		for _, bad := range []string{"InstanceType=", "DomainName=", "HostedZoneId=", "SshCidr=", "AccessCidr=", "DailySnapshotsKept="} {
			if strings.Contains(out, bad) {
				t.Errorf("%s: an option that was not given shows up (%s):\n%s", b, bad, out)
			}
		}
	}
}

func TestDryRunAllOptions(t *testing.T) {
	for _, b := range bashes(t) {
		out := dryRun(t, b, nil,
			"--region=eu-west-1", "--email", "you@example.com", "--domain", "example.com", "--hosted-zone-id", "Z0123456789ABCDEFGHIJ",
			"--instance-type", "m7i.large", "--stack-name", "my-sbctl", "--volume-size", "200", "--version", "v1.2.3",
			"--access-cidr", "203.0.113.0/24", "--ssh-cidr", "203.0.113.4/32", "--key-name", "mykey", "--no-session-manager",
			"--data-snapshot-id", "snap-0123456789abcdef0", "--daily-snapshots", "014", "--profile", "work")
		for _, want := range []string{
			"aws --region eu-west-1 --profile work cloudformation deploy --stack-name my-sbctl",
			"AdminEmail=you@example.com", "InstanceType=m7i.large", "DataVolumeSize=200", "DailySnapshotsKept=14", "SbctlVersion=v1.2.3",
			"DomainName=example.com", "HostedZoneId=Z0123456789ABCDEFGHIJ", "AccessCidr=203.0.113.0/24",
			"SshCidr=203.0.113.4/32", "KeyName=mykey", "EnableSessionManager=false", "DataSnapshotId=snap-0123456789abcdef0",
			// An x86 type looks up the amd64 image.
			"/24.04/stable/current/amd64/hvm/ebs-gp3/ami-id",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("%s: output lacks %q:\n%s", b, want, out)
			}
		}
	}
}

func TestDryRunExplicitAmiSkipsLookup(t *testing.T) {
	out := dryRun(t, bashes(t)[0], nil, "--region", "us-east-1", "--email", "a@b.co", "--ami-id", "ami-0123456789abcdef0")
	if strings.Contains(out, "ssm get-parameter") || !strings.Contains(out, "AmiId=ami-0123456789abcdef0") {
		t.Errorf("an explicit image must be used as it is:\n%s", out)
	}
}

func TestDryRunRegionFromEnvironment(t *testing.T) {
	out := dryRun(t, bashes(t)[0], []string{"AWS_REGION=ap-southeast-2"}, "--email", "a@b.co")
	if !strings.Contains(out, "aws --region ap-southeast-2 cloudformation deploy") {
		t.Errorf("AWS_REGION not used:\n%s", out)
	}
}

// Every parameter deploy.sh passes must exist in the template, or the deploy would fail late.
func TestParameterNamesExistInTemplate(t *testing.T) {
	raw, err := os.ReadFile("../cloudformation/sbctl.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var tpl struct {
		Parameters map[string]yaml.Node `yaml:"Parameters"`
	}
	if err := yaml.Unmarshal(raw, &tpl); err != nil {
		t.Fatal(err)
	}
	out := dryRun(t, bashes(t)[0], nil,
		"--region", "us-east-1", "--email", "a@b.co", "--domain", "example.com", "--hosted-zone-id", "Z0123456789ABCDEFGHIJ",
		"--instance-type", "t4g.xlarge", "--volume-size", "50", "--version", "v1.0.0", "--access-cidr", "10.0.0.0/8",
		"--ssh-cidr", "10.0.0.1/32", "--key-name", "k", "--no-session-manager", "--data-snapshot-id", "snap-0123456789abcdef0",
		"--daily-snapshots", "0")
	idx := strings.Index(out, "--parameter-overrides ")
	if idx < 0 {
		t.Fatalf("no parameter overrides:\n%s", out)
	}
	line := strings.SplitN(out[idx:], "\n", 2)[0]
	n := 0
	for _, f := range strings.Fields(strings.TrimPrefix(line, "--parameter-overrides ")) {
		name, _, ok := strings.Cut(strings.Trim(f, "'"), "=")
		if !ok {
			t.Errorf("not a Name=Value pair: %s", f)
			continue
		}
		n++
		if _, ok := tpl.Parameters[name]; !ok {
			t.Errorf("deploy.sh passes %s, which the template does not declare", name)
		}
	}
	if n != 13 {
		t.Errorf("expected 13 parameters, got %d: %s", n, line)
	}
}

func TestDryRunDownloadsTheReleaseTemplateWhenNoneIsNearby(t *testing.T) {
	// A lone deploy.sh, as downloaded from a release page, has no template beside it.
	lone := t.TempDir()
	src, _ := os.ReadFile("deploy.sh")
	if err := os.WriteFile(filepath.Join(lone, "deploy.sh"), src, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		args []string
		url  string
	}{
		{nil, "https://github.com/jsmillerdev/sbctl/releases/latest/download/sbctl.yaml"},
		{[]string{"--version", "v1.2.3"}, "https://github.com/jsmillerdev/sbctl/releases/download/v1.2.3/sbctl.yaml"},
	} {
		dir, log := stubAWS(t)
		cmd := exec.Command(bashes(t)[0], append([]string{filepath.Join(lone, "deploy.sh"), "--region", "us-east-1", "--email", "a@b.co", "--dry-run"}, c.args...)...)
		cmd.Env = []string{"PATH=" + dir + ":/usr/bin:/bin", "HOME=" + t.TempDir()}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		if !strings.Contains(string(out), "curl -fsSL -o ") || !strings.Contains(string(out), c.url) {
			t.Errorf("want a download from %s:\n%s", c.url, out)
		}
		if n := calls(t, log); len(n) != 0 {
			t.Errorf("dry run called aws: %v", n)
		}
	}
	// With --template the download is skipped.
	dir, _ := stubAWS(t)
	cmd := exec.Command(bashes(t)[0], filepath.Join(lone, "deploy.sh"), "--region", "us-east-1", "--email", "a@b.co", "--dry-run", "--template", "/x/my.yaml")
	cmd.Env = []string{"PATH=" + dir + ":/usr/bin:/bin", "HOME=" + t.TempDir()}
	out, err := cmd.CombinedOutput()
	if err != nil || strings.Contains(string(out), "curl") || !strings.Contains(string(out), "--template-file /x/my.yaml") {
		t.Errorf("--template: %v\n%s", err, out)
	}
}

func TestDryRunDelete(t *testing.T) {
	for _, b := range bashes(t) {
		out := dryRun(t, b, nil, "--region", "us-east-1", "--stack-name", "sbctl", "--delete")
		for _, want := range []string{
			"About to delete the CloudFormation stack \"sbctl\" in us-east-1.",
			"the S3 backup bucket", "final EBS snapshot of the data volume",
			"aws --region us-east-1 cloudformation delete-stack --stack-name sbctl",
			"aws --region us-east-1 cloudformation wait stack-delete-complete --stack-name sbctl",
			"ec2 describe-snapshots --owner-ids self",
			// The delete path stops the instance first, and the dry run shows its lookups.
			"aws --region us-east-1 cloudformation describe-stacks --stack-name sbctl --query 'Stacks[0].StackStatus' --output text",
			"aws --region us-east-1 ec2 stop-instances --instance-ids '<InstanceId>'",
			"aws --region us-east-1 ec2 wait instance-stopped --instance-ids '<InstanceId>'",
			"daily snapshots of the data volume",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("%s: lacks %q:\n%s", b, want, out)
			}
		}
		stop, wait, del := strings.Index(out, "ec2 stop-instances"), strings.Index(out, "ec2 wait instance-stopped"), strings.Index(out, "cloudformation delete-stack")
		if !(0 < stop && stop < wait && wait < del) {
			t.Errorf("%s: the instance must be stopped, and waited for, before the stack is deleted:\n%s", b, out)
		}
	}
}

// The real path against the stub: a new stack looks up the image, deploys, then prints the
// dashboard URL and the claim-token command.
func TestRealRunNewStack(t *testing.T) {
	for _, b := range bashes(t) {
		dir, log := stubAWS(t)
		r := run(t, b, dir, nil, "--region", "us-east-1", "--email", "a@b.co")
		if r.code != 0 {
			t.Fatalf("exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
		}
		for _, want := range []string{
			"Ubuntu 24.04 (arm64) image: ami-0123456789abcdef0",
			"Dashboard:  https://studio.1.2.3.4.sslip.io",
			"Claim page: https://api.1.2.3.4.sslip.io/claim",
			"aws secretsmanager get-secret-value --region us-east-1 --secret-id arn:aws:secretsmanager:us-east-1:111122223333:secret:x --query SecretString --output text",
			"aws ssm start-session --region us-east-1 --target i-0abc",
			"--delete",
		} {
			if !strings.Contains(r.stdout, want) {
				t.Errorf("%s: output lacks %q:\n%s", b, want, r.stdout)
			}
		}
		c := calls(t, log)
		var deploy string
		for _, l := range c {
			if strings.Contains(l, "cloudformation deploy") {
				deploy = l
			}
		}
		if !strings.Contains(deploy, "--capabilities CAPABILITY_IAM") || !strings.Contains(deploy, "AdminEmail=a@b.co AmiId=ami-0123456789abcdef0") {
			t.Errorf("deploy call: %s\nall calls: %v", deploy, c)
		}
		for _, l := range c {
			if !strings.HasPrefix(l, "--region us-east-1 ") {
				t.Errorf("a call without the region: %s", l)
			}
		}
	}
}

// An existing stack keeps the image it runs, so an update never replaces the instance.
func TestRealRunExistingStackKeepsItsImage(t *testing.T) {
	dir, log := stubAWS(t)
	r := run(t, bashes(t)[0], dir, []string{"STACK_EXISTS=1"}, "--region", "us-east-1", "--email", "a@b.co", "--access-cidr", "10.0.0.0/8")
	if r.code != 0 {
		t.Fatalf("exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	joined := strings.Join(calls(t, log), "\n")
	if strings.Contains(joined, "ssm get-parameter") {
		t.Errorf("an existing stack must not look up a new image:\n%s", joined)
	}
	if !strings.Contains(joined, "AmiId=ami-0aaaaaaaaaaaaaaaa") || !strings.Contains(joined, "AccessCidr=10.0.0.0/8") {
		t.Errorf("the deploy must pass the stack's own image:\n%s", joined)
	}
	if !strings.Contains(r.stdout, "keeping its image ami-0aaaaaaaaaaaaaaaa") {
		t.Errorf("stdout: %s", r.stdout)
	}

	// A stack created in the console with an empty AmiId: the image comes from the instance.
	dir, log = stubAWS(t)
	r = run(t, bashes(t)[0], dir, []string{"STACK_EXISTS=1", "STACK_AMI="}, "--region", "us-east-1", "--email", "a@b.co")
	if r.code != 0 {
		t.Fatalf("exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	if joined = strings.Join(calls(t, log), "\n"); !strings.Contains(joined, "ec2 describe-instances --instance-ids i-0123456789abcdef0") || !strings.Contains(joined, "AmiId=ami-0bbbbbbbbbbbbbbbb") {
		t.Errorf("an empty AmiId must be filled from the running instance:\n%s", joined)
	}
}

func TestRealRunDelete(t *testing.T) {
	re := regexp.MustCompile(`(?m)^--region us-east-1 cloudformation (delete-stack|wait stack-delete-complete) --stack-name sbctl$`)
	// Not a terminal and no --yes: refuse, delete nothing.
	dir, log := stubAWS(t)
	r := run(t, bashes(t)[0], dir, []string{"STACK_EXISTS=1"}, "--region", "us-east-1", "--delete")
	if r.code == 0 || !strings.Contains(r.stderr, "pass --yes") {
		t.Errorf("want a refusal without --yes: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	if re.MatchString(strings.Join(calls(t, log), "\n")) {
		t.Error("deleted without confirmation")
	}
	// A stack that does not exist.
	dir, _ = stubAWS(t)
	r = run(t, bashes(t)[0], dir, nil, "--region", "us-east-1", "--delete", "--yes")
	if r.code == 0 || !strings.Contains(r.stderr, "no stack named sbctl") {
		t.Errorf("want 'no stack named': exit %d\n%s", r.code, r.stderr)
	}
	// Confirmed.
	dir, log = stubAWS(t)
	r = run(t, bashes(t)[0], dir, []string{"STACK_EXISTS=1"}, "--region", "us-east-1", "--delete", "--yes")
	if r.code != 0 {
		t.Fatalf("exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	if got := re.FindAllStringSubmatch(strings.Join(calls(t, log), "\n"), -1); len(got) != 2 {
		t.Errorf("want delete-stack and wait, got %v", calls(t, log))
	}
	// The instance is stopped, and waited for, before the stack goes: the final snapshot then
	// comes from a node that shut down in order.
	idx := func(sub string) int {
		for i, l := range calls(t, log) {
			if strings.Contains(l, sub) {
				return i
			}
		}
		return -1
	}
	stop, wait, del := idx("ec2 stop-instances --instance-ids i-0123456789abcdef0"), idx("ec2 wait instance-stopped --instance-ids i-0123456789abcdef0"), idx("cloudformation delete-stack")
	if !(0 <= stop && stop < wait && wait < del) {
		t.Errorf("want stop-instances, wait instance-stopped, delete-stack in that order, got %v", calls(t, log))
	}
	for _, want := range []string{"Backup bucket:  sbctl-backupbucket-xyz", "Data volume:    vol-0123456789abcdef0", "Instance:       i-0123456789abcdef0 (stopped first)", "volume-id,Values=vol-0123456789abcdef0"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, r.stdout)
		}
	}

	// An instance that cannot be stopped keeps the stack: a crash image is not what the person
	// was told to expect.
	dir, log = stubAWS(t)
	r = run(t, bashes(t)[0], dir, []string{"STACK_EXISTS=1", "STOP_FAILS=1"}, "--region", "us-east-1", "--delete", "--yes")
	if r.code == 0 || !strings.Contains(r.stderr, "could not stop i-0123456789abcdef0") {
		t.Errorf("want a refusal when the instance does not stop: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	if re.MatchString(strings.Join(calls(t, log), "\n")) {
		t.Error("deleted the stack although the instance did not stop")
	}
}
