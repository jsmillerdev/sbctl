package awsdeploy_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The purge path of deploy.sh against testdata/fake-aws-purge.py: a stack, its buckets with
// versioned objects and its snapshots live in a JSON state file that the fake changes as the script
// deletes things, so the tests see what is left.

const (
	pAccount = "111122223333"
	pStackID = "arn:aws:cloudformation:us-east-1:111122223333:stack/supavise/11111111-2222-3333-4444-555555555555"
	pOldID   = "arn:aws:cloudformation:us-east-1:111122223333:stack/supavise/99999999-2222-3333-4444-555555555555"
	pVolume  = "vol-0123456789abcdef0"
	pBackup  = "supavise-backupbucket-abc123"
	pObjects = "supavise-objectsbucket-def456"
)

type pbucket struct {
	Tags     map[string]string `json:"tags,omitempty"`
	Versions [][2]string       `json:"versions"`
	Markers  [][2]string       `json:"markers"`
	Sizes    int64             `json:"sizes"`
	Uploads  [][2]string       `json:"uploads,omitempty"`
	Keep     []string          `json:"keep,omitempty"`
}

type psnap struct {
	ID    string            `json:"id"`
	Vol   string            `json:"vol"`
	Size  int               `json:"size"`
	Start string            `json:"start"`
	State string            `json:"state"`
	Tags  map[string]string `json:"tags,omitempty"`
	Fail  bool              `json:"fail,omitempty"`
}

type pstack struct {
	Name          string            `json:"name"`
	ID            string            `json:"id"`
	Status        string            `json:"status"`
	Exists        bool              `json:"exists"`
	Outputs       map[string]string `json:"outputs,omitempty"`
	Params        map[string]string `json:"params,omitempty"`
	Volume        string            `json:"volume"`
	FinalSnapshot string            `json:"final_snapshot,omitempty"`
	FinalTags     map[string]string `json:"final_tags,omitempty"`
	Records       bool              `json:"records"`
}

type pdeleted struct {
	Name   string `json:"name"`
	ID     string `json:"id"`
	Volume string `json:"volume"`
}

type pother struct {
	Name   string            `json:"name"`
	Params map[string]string `json:"params"`
}

type pstate struct {
	Account    string              `json:"account"`
	Stack      pstack              `json:"stack"`
	Deleted    []pdeleted          `json:"deleted_stacks,omitempty"`
	Others     []pother            `json:"other_stacks,omitempty"`
	Unlistable bool                `json:"stacks_unlistable,omitempty"`
	Buckets    map[string]*pbucket `json:"buckets"`
	Snaps      []psnap             `json:"snapshots"`
	StopFail   bool                `json:"stop_fails,omitempty"`
}

func versions(n int, prefix string) [][2]string {
	var out [][2]string
	for i := 0; i < n; i++ {
		out = append(out, [2]string{fmt.Sprintf("%s/%05d.bin", prefix, i), fmt.Sprintf("v%d", i)})
	}
	return out
}

func cfnTags(stack, id, logical string) map[string]string {
	return map[string]string{"aws:cloudformation:stack-name": stack, "aws:cloudformation:stack-id": id, "aws:cloudformation:logical-id": logical, "Application": "supavise"}
}

// deletedStack is a stack that is gone and left a backup bucket, an objects bucket, two daily
// snapshots (tagged) and a final snapshot (not tagged), next to things that are not its own.
func deletedStack() *pstate {
	return &pstate{
		Account: pAccount,
		Stack:   pstack{Name: "supavise", ID: pStackID, Exists: false, Volume: pVolume},
		Deleted: []pdeleted{{Name: "supavise", ID: pStackID, Volume: pVolume}},
		Buckets: map[string]*pbucket{
			pBackup:  {Tags: cfnTags("supavise", pStackID, "BackupBucket"), Versions: versions(2500, "wal"), Markers: versions(40, "gone"), Sizes: 3 << 30, Uploads: [][2]string{{"base/part one.tar", "up1"}}},
			pObjects: {Tags: cfnTags("supavise", pStackID, "ObjectsBucket"), Versions: versions(3, "obj"), Sizes: 4096},
			// Names that start the same way and are not the stack's: no tags, another stack's tags,
			// another logical resource, another region.
			"supavise-templates-111122223333-us-east-1": {Versions: versions(1, "t"), Sizes: 10},
			"supavise-backupbucket-other":               {Tags: cfnTags("supavise-2", "arn:aws:cloudformation:us-east-1:111122223333:stack/supavise-2/aaaa", "BackupBucket"), Versions: versions(1, "o")},
			"supavise-somethingelse-zzz":                {Tags: cfnTags("supavise", pStackID, "SomethingElse"), Versions: versions(1, "s")},
			"supavise-backupbucket-eu":                  {Tags: cfnTags("supavise", "arn:aws:cloudformation:eu-west-1:111122223333:stack/supavise/bbbb", "BackupBucket"), Versions: versions(1, "e")},
		},
		Snaps: []psnap{
			{ID: "snap-0000000000000001", Vol: pVolume, Size: 100, Start: "2026-10-06T03:00:00+00:00", State: "completed", Tags: map[string]string{"supavise:stack": pStackID, "Name": "supavise-data", "supavise:snapshot": "daily"}},
			{ID: "snap-0000000000000002", Vol: pVolume, Size: 100, Start: "2026-10-07T03:00:00+00:00", State: "completed", Tags: map[string]string{"supavise:stack": pStackID, "Name": "supavise-data", "supavise:snapshot": "daily"}},
			{ID: "snap-0000000000000003", Vol: pVolume, Size: 100, Start: "2026-10-08T12:00:00+00:00", State: "completed"}, // the final one: no tags
			// Not the stack's: another stack's volume and snapshot, a snapshot with nobody's marks.
			{ID: "snap-0000000000000009", Vol: "vol-0aaaaaaaaaaaaaaaa", Size: 50, Start: "2026-10-01T03:00:00+00:00", State: "completed", Tags: map[string]string{"supavise:stack": "arn:aws:cloudformation:us-east-1:111122223333:stack/supavise-2/aaaa"}},
			{ID: "snap-0000000000000008", Vol: "vol-0bbbbbbbbbbbbbbbb", Size: 20, Start: "2026-10-01T04:00:00+00:00", State: "completed"},
		},
	}
}

// liveStack is a stack that exists, with the outputs delete reads, and the same leftovers waiting.
func liveStack() *pstate {
	st := deletedStack()
	st.Deleted = nil
	st.Stack.Exists = true
	st.Stack.Status = "CREATE_COMPLETE"
	st.Stack.Outputs = map[string]string{"InstanceId": "i-0123456789abcdef0", "BackupBucket": pBackup, "ObjectsBucket": pObjects, "DataVolumeId": pVolume}
	st.Stack.Params = map[string]string{"AdminEmail": "a@b.co", "JoinLeader": ""}
	st.Stack.FinalSnapshot = "snap-0000000000000003"
	st.Stack.Records = true
	// The final snapshot does not exist until the stack is deleted.
	st.Snaps = append(st.Snaps[:2], st.Snaps[3:]...)
	return st
}

func purgeStub(t *testing.T, st *pstate) (dir, log, statePath string) {
	t.Helper()
	dir = t.TempDir()
	log = filepath.Join(dir, "calls.log")
	statePath = filepath.Join(dir, "state.json")
	fake, err := os.ReadFile("testdata/fake-aws-purge.py")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "aws"), fake, 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, log, statePath
}

func readState(t *testing.T, path string) *pstate {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var st pstate
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	return &st
}

func purgeEnv(dir, state string) []string {
	return []string{"FAKE_AWS_STATE=" + state, "FAKE_AWS_LOG=" + filepath.Join(dir, "calls.log")}
}

func count(lines []string, sub string) int {
	n := 0
	for _, l := range lines {
		if strings.Contains(l, sub) {
			n++
		}
	}
	return n
}

func snapIDs(st *pstate) []string {
	var ids []string
	for _, s := range st.Snaps {
		ids = append(ids, s.ID)
	}
	return ids
}

func bucketNames(st *pstate) []string {
	var names []string
	for n := range st.Buckets {
		names = append(names, n)
	}
	return names
}

func TestPurgeArgumentErrors(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no stack name", []string{"purge", "--region", "us-east-1"}, "purge needs --stack-name"},
		{"no region", []string{"purge", "--stack-name", "supavise"}, "--region is required"},
		{"with delete", []string{"purge", "--region", "us-east-1", "--stack-name", "supavise", "--delete"}, "purge does not take --delete"},
		{"with create options", []string{"purge", "--region", "us-east-1", "--stack-name", "supavise", "--email", "a@b.co"}, "purge takes only"},
		{"with a template", []string{"purge", "--region", "us-east-1", "--stack-name", "supavise", "--template", "x.yaml"}, "purge takes only"},
		{"purge without delete", []string{"--region", "us-east-1", "--stack-name", "supavise", "--email", "a@b.co", "--purge"}, "--purge belongs to --delete"},
		{"update with purge", []string{"update", "--stack", "supavise", "--purge"}, "update does not take --purge"},
		{"status with purge", []string{"status", "--stack", "supavise", "--purge"}, "status takes only"},
		{"replica with purge", []string{"replica", "--leader-stack", "supavise", "--region", "eu-west-1", "--az", "eu-west-1b", "--token-file", "t", "--purge"}, "replica does not take --purge"},
		{"bad stack", []string{"purge", "--region", "us-east-1", "--stack-name", "1bad"}, "--stack-name must start"},
	}
	for _, b := range bashes(t) {
		for _, c := range cases {
			t.Run(filepath.Base(b)+"/"+c.name, func(t *testing.T) {
				dir, log, state := purgeStub(t, deletedStack())
				r := run(t, b, dir, purgeEnv(dir, state), append(c.args, "--dry-run")...)
				if r.code != 2 || !strings.Contains(r.stderr, c.want) {
					t.Errorf("exit %d, stderr %q; want exit 2 and %q", r.code, r.stderr, c.want)
				}
				if n := calls(t, log); len(n) != 0 {
					t.Errorf("a refused command called aws: %v", n)
				}
			})
		}
	}
}

func TestPurgeDryRun(t *testing.T) {
	for _, b := range bashes(t) {
		dir, log, state := purgeStub(t, deletedStack())
		r := run(t, b, dir, purgeEnv(dir, state), "purge", "--region", "us-east-1", "--stack-name", "supavise", "--dry-run")
		if r.code != 0 {
			t.Fatalf("exit %d\n%s%s", r.code, r.stdout, r.stderr)
		}
		if n := calls(t, log); len(n) != 0 {
			t.Fatalf("--dry-run called aws: %v", n)
		}
		for _, want := range []string{
			"aws --region us-east-1 cloudformation describe-stacks --stack-name supavise --query 'Stacks[0].StackStatus' --output text",
			"cloudformation list-stacks --stack-status-filter DELETE_COMPLETE",
			"s3api list-buckets",
			"s3api get-bucket-tagging --bucket '<bucket>' --expected-bucket-owner '<account>'",
			"s3api list-object-versions --bucket '<bucket>'",
			"s3api delete-objects --bucket '<bucket>' --expected-bucket-owner '<account>' --delete 'file://<batch of up to 1000>'",
			"s3api delete-bucket --bucket '<bucket>'",
			"ec2 describe-snapshots --owner-ids self --filters 'Name=tag:supavise:stack,Values=arn:*:cloudformation:us-east-1:<account>:stack/supavise/*'",
			"ec2 delete-snapshot --snapshot-id '<snapshot>'",
		} {
			if !strings.Contains(r.stdout, want) {
				t.Errorf("%s: dry run lacks %q:\n%s", b, want, r.stdout)
			}
		}
		if _, err := os.Stat(state); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDeletePurgeDryRun(t *testing.T) {
	for _, b := range bashes(t) {
		dir, log, state := purgeStub(t, liveStack())
		r := run(t, b, dir, purgeEnv(dir, state), "--region", "us-east-1", "--stack-name", "supavise", "--delete", "--purge", "--dry-run")
		if r.code != 0 {
			t.Fatalf("exit %d\n%s%s", r.code, r.stdout, r.stderr)
		}
		if n := calls(t, log); len(n) != 0 {
			t.Fatalf("--dry-run called aws: %v", n)
		}
		for _, want := range []string{
			"--purge: once the stack is deleted, what stays is destroyed too",
			"cloudformation describe-stacks --stack-name supavise --query 'Stacks[0].StackId'",
			"ec2 describe-snapshots --owner-ids self --filters 'Name=tag:supavise:stack,Values=<StackId>'",
			"s3api delete-objects --bucket '<BackupBucket>'",
			"ec2 delete-snapshot --snapshot-id '<snapshot>'",
		} {
			if !strings.Contains(r.stdout, want) {
				t.Errorf("%s: lacks %q:\n%s", b, want, r.stdout)
			}
		}
		stop, del, purge := strings.Index(r.stdout, "ec2 stop-instances"), strings.Index(r.stdout, "cloudformation delete-stack"), strings.Index(r.stdout, "s3api delete-bucket")
		if !(0 < stop && stop < del && del < purge) {
			t.Errorf("%s: the purge must come after the instance is stopped and the stack deleted:\n%s", b, r.stdout)
		}
	}
}

func TestPurgeRefusesWhileTheStackExists(t *testing.T) {
	st := liveStack()
	dir, log, state := purgeStub(t, st)
	r := run(t, bashes(t)[0], dir, purgeEnv(dir, state), "purge", "--region", "us-east-1", "--stack-name", "supavise", "--yes")
	if r.code != 2 || !strings.Contains(r.stderr, "stack supavise still exists in us-east-1 (CREATE_COMPLETE)") {
		t.Errorf("exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	c := strings.Join(calls(t, log), "\n")
	for _, bad := range []string{"delete-objects", "delete-bucket", "delete-snapshot", "list-buckets"} {
		if strings.Contains(c, bad) {
			t.Errorf("a refused purge called %s:\n%s", bad, c)
		}
	}
}

// Without --yes and without a terminal nothing is destroyed; the list is shown.
func TestPurgeAsksBeforeItDestroysAnything(t *testing.T) {
	dir, log, state := purgeStub(t, deletedStack())
	r := run(t, bashes(t)[0], dir, purgeEnv(dir, state), "purge", "--region", "us-east-1", "--stack-name", "supavise")
	if r.code != 2 || !strings.Contains(r.stderr, "pass --yes") {
		t.Errorf("want a refusal (exit 2) without --yes: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	for _, want := range []string{pBackup, "2500 object version(s), 40 delete marker(s), 3.0 GiB", pObjects, "3 object version(s)", "snap-0000000000000001", "snap-0000000000000003"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("the list lacks %q:\n%s", want, r.stdout)
		}
	}
	c := strings.Join(calls(t, log), "\n")
	for _, bad := range []string{"delete-objects", "delete-bucket", "delete-snapshot", "abort-multipart"} {
		if strings.Contains(c, bad) {
			t.Errorf("destroyed something (%s) without confirmation:\n%s", bad, c)
		}
	}
	if got := readState(t, state); len(got.Buckets) != 6 || len(got.Snaps) != 5 {
		t.Errorf("state changed: %v %v", bucketNames(got), snapIDs(got))
	}
}

// Finding only what is provably the stack's: the buckets by CloudFormation's tags, the snapshots by
// the stack's id and by the volumes those came from.
func TestPurgeOfADeletedStack(t *testing.T) {
	for _, b := range bashes(t) {
		dir, log, state := purgeStub(t, deletedStack())
		r := run(t, b, dir, purgeEnv(dir, state), "purge", "--region", "us-east-1", "--stack-name", "supavise", "--yes")
		if r.code != 0 {
			t.Fatalf("%s: exit %d\n%s\n%s", b, r.code, r.stdout, r.stderr)
		}
		got := readState(t, state)
		// What is left is exactly what is not the stack's.
		wantBuckets := map[string]bool{"supavise-templates-111122223333-us-east-1": true, "supavise-backupbucket-other": true, "supavise-somethingelse-zzz": true, "supavise-backupbucket-eu": true}
		for n := range got.Buckets {
			if !wantBuckets[n] {
				t.Errorf("%s: bucket %s should be gone", b, n)
			}
			delete(wantBuckets, n)
		}
		for n := range wantBuckets {
			t.Errorf("%s: bucket %s was not the stack's and is gone", b, n)
		}
		if ids := snapIDs(got); strings.Join(ids, ",") != "snap-0000000000000009,snap-0000000000000008" {
			t.Errorf("%s: snapshots left: %v (the final one is found by the volume of the tagged ones)", b, ids)
		}
		c := calls(t, log)
		// 2500 versions and 40 markers are 2540 entries of one listing: three batches (1000, 1000,
		// 540), and the page after them comes back empty.
		if n := count(c, "s3api delete-objects --bucket "+pBackup); n != 3 {
			t.Errorf("%s: %d delete-objects calls for the backup bucket, want 3:\n%s", b, n, strings.Join(c, "\n"))
		}
		if n := count(c, "s3api delete-objects --bucket "+pObjects); n != 1 {
			t.Errorf("%s: %d delete-objects calls for the objects bucket, want 1", b, n)
		}
		if n := count(c, "abort-multipart-upload --bucket "+pBackup+" --key=base/part one.tar --upload-id up1"); n != 1 {
			t.Errorf("%s: the incomplete upload was not aborted exactly once (%d)", b, n)
		}
		// The uploads go before the objects, the objects before the bucket.
		first := func(sub string) int {
			for i, l := range c {
				if strings.Contains(l, sub) {
					return i
				}
			}
			return -1
		}
		abort, del, rm := first("abort-multipart-upload --bucket "+pBackup), first("delete-objects --bucket "+pBackup), first("delete-bucket --bucket "+pBackup)
		if !(0 <= abort && abort < del && del < rm) {
			t.Errorf("%s: want abort, delete-objects, delete-bucket in that order:\n%s", b, strings.Join(c, "\n"))
		}
		for _, l := range c {
			if !strings.HasPrefix(l, "--region us-east-1 ") {
				t.Errorf("%s: a call without the region: %s", b, l)
			}
			if strings.Contains(l, "s3api") && !strings.Contains(l, "--expected-bucket-owner "+pAccount) && !strings.Contains(l, "list-buckets") {
				t.Errorf("%s: a bucket call that does not pass the expected owner: %s", b, l)
			}
		}
		for _, bad := range []string{"supavise-templates", "supavise-backupbucket-other", "supavise-somethingelse", "supavise-backupbucket-eu", "snap-0000000000000009", "snap-0000000000000008"} {
			for _, l := range c {
				if (strings.Contains(l, "delete-") || strings.Contains(l, "abort-")) && strings.Contains(l, bad) {
					t.Errorf("%s: touched something that is not the stack's: %s", b, l)
				}
			}
		}
		if !strings.Contains(r.stdout, "Purged. Nothing of stack supavise is left in us-east-1.") {
			t.Errorf("%s: stdout: %s", b, r.stdout)
		}
	}
}

// Where CloudFormation no longer has the stack's records (it keeps a deleted stack for 90 days),
// the tags of the daily snapshots still lead to the volume and so to the final snapshot.
func TestPurgeFindsTheFinalSnapshotThroughTheDailyOnes(t *testing.T) {
	st := deletedStack()
	st.Deleted = nil // no record of the deleted stack
	dir, _, state := purgeStub(t, st)
	r := run(t, bashes(t)[0], dir, purgeEnv(dir, state), "purge", "--region", "us-east-1", "--stack-name", "supavise", "--yes")
	if r.code != 0 {
		t.Fatalf("exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	if ids := snapIDs(readState(t, state)); strings.Join(ids, ",") != "snap-0000000000000009,snap-0000000000000008" {
		t.Errorf("snapshots left: %v", ids)
	}

	// A final snapshot with no daily snapshot beside it and no record of the stack cannot be matched:
	// the script says so and leaves it.
	st = deletedStack()
	st.Deleted = nil
	st.Snaps = []psnap{st.Snaps[2], st.Snaps[3]}
	for n := range st.Buckets {
		delete(st.Buckets, n)
	}
	dir, log, state := purgeStub(t, st)
	r = run(t, bashes(t)[0], dir, purgeEnv(dir, state), "purge", "--region", "us-east-1", "--stack-name", "supavise", "--yes")
	if r.code != 0 || !strings.Contains(r.stdout, "Nothing of stack supavise is left in us-east-1") || !strings.Contains(r.stdout, "deleted more than 90 days ago") {
		t.Errorf("exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	if strings.Contains(strings.Join(calls(t, log), "\n"), "delete-snapshot") {
		t.Error("deleted a snapshot that carries no mark of the stack")
	}
}

func TestPurgeWithNothingLeft(t *testing.T) {
	st := deletedStack()
	st.Buckets = map[string]*pbucket{}
	st.Snaps = nil
	st.Deleted = nil
	dir, log, state := purgeStub(t, st)
	r := run(t, bashes(t)[0], dir, purgeEnv(dir, state), "purge", "--region", "us-east-1", "--stack-name", "supavise", "--yes")
	if r.code != 0 || !strings.Contains(r.stdout, "Nothing of stack supavise is left in us-east-1") {
		t.Errorf("exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	if strings.Contains(strings.Join(calls(t, log), "\n"), "delete-") {
		t.Error("deleted something although nothing was found")
	}
}

// One thing S3 or EC2 refuses does not stop the rest, and the script ends with a failure that says
// what is left.
func TestPurgeReportsWhatItCouldNotDestroy(t *testing.T) {
	st := deletedStack()
	st.Buckets[pBackup].Keep = []string{"wal/00007.bin"} // Object Lock
	st.Snaps[0].Fail = true                              // in use by an image
	dir, _, state := purgeStub(t, st)
	r := run(t, bashes(t)[0], dir, purgeEnv(dir, state), "purge", "--region", "us-east-1", "--stack-name", "supavise", "--yes")
	if r.code != 3 {
		t.Errorf("exit %d, want 3\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	for _, want := range []string{"Object lock", "could not empty and delete " + pBackup, "could not delete snap-0000000000000001", "currently in use by ami-", "run  deploy.sh purge --region us-east-1 --stack-name supavise"} {
		if !strings.Contains(r.stderr, want) && !strings.Contains(strings.ToLower(r.stderr), strings.ToLower(want)) {
			t.Errorf("stderr lacks %q:\n%s", want, r.stderr)
		}
	}
	got := readState(t, state)
	if _, ok := got.Buckets[pBackup]; !ok {
		t.Error("the bucket that still holds a protected object was deleted")
	}
	if _, ok := got.Buckets[pObjects]; ok {
		t.Error("the other bucket was not purged after the first failed")
	}
	if ids := snapIDs(got); strings.Join(ids, ",") != "snap-0000000000000001,snap-0000000000000009,snap-0000000000000008" {
		t.Errorf("snapshots left: %v", ids)
	}
}

// --delete --purge: one listing and one confirmation before anything is stopped, then the stack goes,
// then what it left, the final snapshot included.
func TestDeleteWithPurge(t *testing.T) {
	for _, b := range bashes(t) {
		dir, log, state := purgeStub(t, liveStack())
		r := run(t, b, dir, purgeEnv(dir, state), "--region", "us-east-1", "--stack-name", "supavise", "--delete", "--purge", "--yes")
		if r.code != 0 {
			t.Fatalf("%s: exit %d\n%s\n%s", b, r.code, r.stdout, r.stderr)
		}
		got := readState(t, state)
		if got.Stack.Exists {
			t.Error("the stack still exists")
		}
		if _, ok := got.Buckets[pBackup]; ok {
			t.Error("the backup bucket is still there")
		}
		if _, ok := got.Buckets[pObjects]; ok {
			t.Error("the objects bucket is still there")
		}
		if ids := snapIDs(got); strings.Join(ids, ",") != "snap-0000000000000009,snap-0000000000000008" {
			t.Errorf("snapshots left: %v (the final one is made by the delete and found afterwards)", ids)
		}
		c := calls(t, log)
		idx := func(sub string) int {
			for i, l := range c {
				if strings.Contains(l, sub) {
					return i
				}
			}
			return -1
		}
		last := func(sub string) int {
			n := -1
			for i, l := range c {
				if strings.Contains(l, sub) {
					n = i
				}
			}
			return n
		}
		stop, wait, del, waitDel, empty, snaps := idx("ec2 stop-instances"), idx("ec2 wait instance-stopped"), idx("cloudformation delete-stack"), idx("cloudformation wait stack-delete-complete"), idx("delete-objects"), idx("delete-snapshot")
		if !(0 <= stop && stop < wait && wait < del && del < waitDel && waitDel < empty) {
			t.Errorf("%s: want stop, wait, delete-stack, wait, then the purge:\n%s", b, strings.Join(c, "\n"))
		}
		// The snapshots are listed again after the stack is deleted, and only then deleted.
		if listed := last("describe-snapshots"); !(waitDel < listed && listed < snaps) {
			t.Errorf("%s: the snapshots must be listed again after the delete, before they are deleted:\n%s", b, strings.Join(c, "\n"))
		}
		for _, want := range []string{"Destroyed once the stack is deleted", pBackup, pObjects, "and the final snapshot of " + pVolume, "Deleted and purged. Nothing of stack supavise is left in us-east-1."} {
			if !strings.Contains(r.stdout, want) {
				t.Errorf("%s: stdout lacks %q:\n%s", b, want, r.stdout)
			}
		}
	}
}

// The stack of a replica server shows its leader's buckets in its outputs: they are never touched.
func TestDeleteWithPurgeLeavesALeadersBucketsAlone(t *testing.T) {
	st := liveStack()
	st.Stack.Params["JoinLeader"] = "203.0.113.50:7443"
	dir, log, state := purgeStub(t, st)
	r := run(t, bashes(t)[0], dir, purgeEnv(dir, state), "--region", "us-east-1", "--stack-name", "supavise", "--delete", "--purge", "--yes")
	if r.code != 0 {
		t.Fatalf("exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	got := readState(t, state)
	if _, ok := got.Buckets[pBackup]; !ok {
		t.Error("the leader's backup bucket was purged")
	}
	if _, ok := got.Buckets[pObjects]; !ok {
		t.Error("the leader's objects bucket was purged")
	}
	if strings.Contains(strings.Join(calls(t, log), "\n"), "s3api") {
		t.Errorf("a replica server's purge called s3api:\n%s", strings.Join(calls(t, log), "\n"))
	}
	if !strings.Contains(r.stdout, "Replica server of 203.0.113.50:7443: the leader's buckets are not touched.") {
		t.Errorf("stdout: %s", r.stdout)
	}
	if ids := snapIDs(got); strings.Join(ids, ",") != "snap-0000000000000009,snap-0000000000000008" {
		t.Errorf("snapshots left: %v", ids)
	}
}

// A bucket whose CloudFormation tags name another stack is not purged, and nothing is stopped.
func TestDeleteWithPurgeRefusesABucketOfAnotherStack(t *testing.T) {
	st := liveStack()
	st.Buckets[pBackup].Tags = cfnTags("supavise-2", "arn:aws:cloudformation:us-east-1:111122223333:stack/supavise-2/aaaa", "BackupBucket")
	dir, log, state := purgeStub(t, st)
	r := run(t, bashes(t)[0], dir, purgeEnv(dir, state), "--region", "us-east-1", "--stack-name", "supavise", "--delete", "--purge", "--yes")
	if r.code != 2 || !strings.Contains(r.stderr, "carries the CloudFormation tags of another stack") {
		t.Errorf("exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	c := strings.Join(calls(t, log), "\n")
	for _, bad := range []string{"stop-instances", "delete-stack", "delete-objects", "delete-snapshot"} {
		if strings.Contains(c, bad) {
			t.Errorf("went on (%s) after the refusal:\n%s", bad, c)
		}
	}
	if !readState(t, state).Stack.Exists {
		t.Error("the stack was deleted")
	}
}

// A failed stop keeps both the stack and everything the purge would destroy.
func TestDeleteWithPurgeKeepsEverythingWhenTheStopFails(t *testing.T) {
	st := liveStack()
	st.StopFail = true
	dir, log, state := purgeStub(t, st)
	r := run(t, bashes(t)[0], dir, purgeEnv(dir, state), "--region", "us-east-1", "--stack-name", "supavise", "--delete", "--purge", "--yes")
	if r.code == 0 || !strings.Contains(r.stderr, "could not stop i-0123456789abcdef0") {
		t.Errorf("exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	if c := strings.Join(calls(t, log), "\n"); strings.Contains(c, "delete-stack") || strings.Contains(c, "delete-objects") || strings.Contains(c, "delete-snapshot") {
		t.Errorf("destroyed something although the instance did not stop:\n%s", c)
	}
}

// S3 lists object versions and delete markers in one sequence, in key order, a page of 1000. A
// bucket whose first entries are all delete markers (the ones left when old versions expire), or
// that alternates long runs of the two, is still emptied in one run: every page is deleted whatever
// it holds, until a page comes back empty.
func TestPurgeEmptiesABucketWhoseFirstPageHoldsOneKind(t *testing.T) {
	cases := []struct {
		name     string
		versions [][2]string
		markers  [][2]string
	}{
		{"markers first", versions(1500, "z-live"), versions(1500, "a-gone")},
		{"versions first", versions(1500, "a-live"), versions(1500, "z-gone")},
		{"alternating runs", append(versions(1100, "b-live"), versions(1100, "d-live")...), append(versions(1100, "a-gone"), versions(1100, "c-gone")...)},
		{"only markers", nil, versions(2100, "gone")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := deletedStack()
			st.Buckets[pBackup].Versions, st.Buckets[pBackup].Markers = c.versions, c.markers
			st.Buckets[pBackup].Uploads = nil
			dir, log, state := purgeStub(t, st)
			r := run(t, bashes(t)[0], dir, purgeEnv(dir, state), "purge", "--region", "us-east-1", "--stack-name", "supavise", "--yes")
			if r.code != 0 {
				t.Fatalf("exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
			}
			if _, ok := readState(t, state).Buckets[pBackup]; ok {
				t.Error("the bucket is still there")
			}
			want := (len(c.versions) + len(c.markers) + 999) / 1000
			if n := count(calls(t, log), "s3api delete-objects --bucket "+pBackup); n != want {
				t.Errorf("%d delete-objects calls, want %d (one per page of the listing)", n, want)
			}
		})
	}
}

// EC2 reports vol-ffffffff as the volume of every snapshot that is a copy (of a snapshot or of an
// image). A tagged copy is the stack's by its tag, but the id must not lead to the other snapshots of
// the account that share it.
func TestPurgeDoesNotSearchByTheVolumeOfCopiedSnapshots(t *testing.T) {
	st := deletedStack()
	const copied = "vol-ffffffff"
	st.Snaps = append(st.Snaps,
		psnap{ID: "snap-00000000000000c1", Vol: copied, Size: 100, Start: "2026-10-05T03:00:00+00:00", State: "completed", Tags: map[string]string{"supavise:stack": pStackID}},
		psnap{ID: "snap-00000000000000c2", Vol: copied, Size: 8, Start: "2026-09-01T03:00:00+00:00", State: "completed"},
		psnap{ID: "snap-00000000000000c3", Vol: copied, Size: 8, Start: "2026-09-02T03:00:00+00:00", State: "completed", Tags: map[string]string{"Name": "somebody else's copy"}},
	)
	st.Deleted[0].Volume = copied // a record that names the copy marker is no more a volume than the snapshots' own
	for _, b := range bashes(t) {
		dir, log, state := purgeStub(t, st)
		r := run(t, b, dir, purgeEnv(dir, state), "purge", "--region", "us-east-1", "--stack-name", "supavise", "--yes")
		if r.code != 0 {
			t.Fatalf("%s: exit %d\n%s\n%s", b, r.code, r.stdout, r.stderr)
		}
		ids := snapIDs(readState(t, state))
		if want := "snap-0000000000000009,snap-0000000000000008,snap-00000000000000c2,snap-00000000000000c3"; strings.Join(ids, ",") != want {
			t.Errorf("%s: snapshots left: %v, want the ones that are not the stack's: %s", b, ids, want)
		}
		for _, l := range calls(t, log) {
			if strings.Contains(l, "volume-id") && strings.Contains(l, copied) {
				t.Errorf("%s: searched by the volume of copies: %s", b, l)
			}
		}
	}

	// The same through --delete --purge, where the volume is the stack's output.
	live := liveStack()
	live.Snaps = append(live.Snaps, st.Snaps[len(st.Snaps)-3:]...)
	live.Stack.Outputs["DataVolumeId"] = copied
	dir, _, state := purgeStub(t, live)
	r := run(t, bashes(t)[0], dir, purgeEnv(dir, state), "--region", "us-east-1", "--stack-name", "supavise", "--delete", "--purge", "--yes")
	if r.code != 0 {
		t.Fatalf("exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	for _, id := range snapIDs(readState(t, state)) {
		if id == "snap-00000000000000c1" {
			t.Error("the tagged copy was left")
		}
	}
	if got := strings.Join(snapIDs(readState(t, state)), ","); !strings.Contains(got, "snap-00000000000000c2") || !strings.Contains(got, "snap-00000000000000c3") {
		t.Errorf("an untagged copy of somebody else's was destroyed: %s", got)
	}
}

// A bucket the stack's outputs name, whose tags cannot be read or are absent, is not provably the
// stack's: the purge stops before it stops anything, as it does for another stack's bucket.
func TestDeleteWithPurgeRefusesABucketWithoutTags(t *testing.T) {
	st := liveStack()
	st.Buckets[pBackup].Tags = nil
	dir, log, state := purgeStub(t, st)
	r := run(t, bashes(t)[0], dir, purgeEnv(dir, state), "--region", "us-east-1", "--stack-name", "supavise", "--delete", "--purge", "--yes")
	if r.code != 2 || !strings.Contains(r.stderr, "shows no CloudFormation tags") || !strings.Contains(r.stderr, "s3:GetBucketTagging") {
		t.Errorf("exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	c := strings.Join(calls(t, log), "\n")
	for _, bad := range []string{"stop-instances", "delete-stack", "delete-objects", "delete-bucket", "delete-snapshot"} {
		if strings.Contains(c, bad) {
			t.Errorf("went on (%s) after the refusal:\n%s", bad, c)
		}
	}
	if got := readState(t, state); !got.Stack.Exists || got.Buckets[pBackup] == nil {
		t.Error("the stack or the bucket is gone")
	}
}

// bucketBlock is the line of a bucket in the purge list and the lines under it that belong to it.
func bucketBlock(t *testing.T, stdout, bucket string) string {
	t.Helper()
	lines := strings.Split(stdout, "\n")
	for i, l := range lines {
		if !strings.HasPrefix(l, "  "+bucket+"   ") {
			continue
		}
		block := l
		for _, next := range lines[i+1:] {
			if !strings.HasPrefix(next, "    ") {
				break
			}
			block += "\n" + next
		}
		return block
	}
	t.Fatalf("no line for %s in:\n%s", bucket, stdout)
	return ""
}

// Replica servers keep their backups and Storage files in their leader's buckets. The list warns,
// before the question, about every stack of the region that names a bucket it is about to destroy.
func TestPurgeWarnsAboutStacksThatUseTheBuckets(t *testing.T) {
	others := []pother{
		{Name: "supavise-replica", Params: map[string]string{"BackupBucketName": pBackup, "ObjectsBucketName": pObjects, "JoinLeader": "203.0.113.50:7443"}},
		{Name: "supavise-eu", Params: map[string]string{"ObjectsBucketName": pObjects}},
		{Name: "unrelated", Params: map[string]string{"BackupBucketName": "somebody-elses-bucket"}},
	}
	for _, c := range []struct {
		name string
		args []string
		st   *pstate
	}{
		{"purge", []string{"purge", "--region", "us-east-1", "--stack-name", "supavise"}, deletedStack()},
		{"delete --purge", []string{"--region", "us-east-1", "--stack-name", "supavise", "--delete", "--purge"}, liveStack()},
	} {
		t.Run(c.name, func(t *testing.T) {
			c.st.Others = others
			dir, log, state := purgeStub(t, c.st)
			// no --yes and no terminal: the list is printed and nothing is destroyed
			r := run(t, bashes(t)[0], dir, purgeEnv(dir, state), c.args...)
			if r.code == 0 || !strings.Contains(r.stderr, "pass --yes") {
				t.Errorf("exit %d\n%s%s", r.code, r.stdout, r.stderr)
			}
			if w := bucketBlock(t, r.stdout, pBackup); !strings.Contains(w, "WARNING: stack(s) supavise-replica in us-east-1 use it") || strings.Contains(w, "supavise-eu") || strings.Contains(w, "unrelated") {
				t.Errorf("backup bucket warning: %s", w)
			}
			if w := bucketBlock(t, r.stdout, pObjects); !strings.Contains(w, "WARNING: stack(s) supavise-replica supavise-eu in us-east-1 use it") {
				t.Errorf("objects bucket warning: %s", w)
			}
			if !strings.Contains(r.stdout, "servers in other regions are not looked at") {
				t.Errorf("no note about other regions:\n%s", r.stdout)
			}
			if got := strings.Join(calls(t, log), "\n"); strings.Contains(got, "delete-objects") || strings.Contains(got, "stop-instances") {
				t.Errorf("destroyed or stopped something without an answer:\n%s", got)
			}
		})
	}

	// Stacks that cannot be listed are said so, not read as "nobody uses it".
	st := deletedStack()
	st.Unlistable = true
	dir, _, state := purgeStub(t, st)
	r := run(t, bashes(t)[0], dir, purgeEnv(dir, state), "purge", "--region", "us-east-1", "--stack-name", "supavise")
	if !strings.Contains(r.stdout, "could not be listed, so it is not known whether a replica server still uses it") {
		t.Errorf("stdout:\n%s", r.stdout)
	}

	// A stack with a bucket of its own and nobody using it has no warning.
	dir, _, state = purgeStub(t, deletedStack())
	r = run(t, bashes(t)[0], dir, purgeEnv(dir, state), "purge", "--region", "us-east-1", "--stack-name", "supavise")
	if strings.Contains(r.stdout, "WARNING") {
		t.Errorf("a warning with no other stack:\n%s", r.stdout)
	}
}
