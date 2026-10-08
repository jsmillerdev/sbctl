package infra

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/supavise/supavise/internal/awsapi/awsfake"
)

// src builds a Source for a host that is (or is not) on EC2 with the given tags.
func src(onEC2 bool, tags map[string]string, stack string, bootstrapped bool) Source {
	return Source{
		OnEC2:        func() bool { return onEC2 },
		Tags:         func(context.Context) (map[string]string, error) { return tags, nil },
		StackName:    func() string { return stack },
		Bootstrapped: func() bool { return bootstrapped },
	}
}

func TestGapOfAStackBeforeRevisions(t *testing.T) {
	// v0.1.x: the instance shows no tags. The user data's log says a stack made it.
	r, err := Detect(context.Background(), src(true, map[string]string{}, "", true))
	if err != nil {
		t.Fatal(err)
	}
	if r.Platform != "aws" || r.Have != 1 || r.Need != Current || !r.Behind() {
		t.Fatalf("report: %+v", r)
	}
	if got := names(r.Missing); !reflect.DeepEqual(got, []string{"tags", "storage-role", "peer-rule", "fencing"}) {
		t.Errorf("missing: %v", got)
	}
	if !reflect.DeepEqual(r.Features, []string{"replicas", "S3 Storage", "failover"}) {
		t.Errorf("features: %v", r.Features)
	}
	// The stack is not named anywhere on the node: the fix asks for it.
	if r.Fix != "sudo -E supavise upgrade --aws --stack-name NAME" {
		t.Errorf("fix: %q", r.Fix)
	}
	var b strings.Builder
	r.Render(&b)
	for _, want := range []string{"AWS stack is at revision 1; this release needs 2 for: replicas, S3 Storage, failover", "missing  Storage bucket and role", "(needed to add a second server)"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("render lacks %q:\n%s", want, b.String())
		}
	}

	// The node's config remembers the stack's name (supavise upgrade --aws wrote it): the fix is the short one.
	r, _ = Detect(context.Background(), src(true, map[string]string{}, "supavise", false))
	if r.Stack != "supavise" || r.Fix != "sudo -E supavise upgrade --aws" || r.Have != 1 {
		t.Errorf("with the name in config: %+v", r)
	}
	// No tags, no name, no log: someone installed by hand on EC2. Nothing a stack can fix.
	r, err = Detect(context.Background(), src(true, map[string]string{}, "", false))
	if err != nil || r.Platform != "" || r.Behind() {
		t.Errorf("a hand-installed EC2 host: %+v %v", r, err)
	}
}

func TestGapOfARevisionTwoStack(t *testing.T) {
	tags := map[string]string{
		TagInfra: "2", TagStackName: "prod", TagCluster: "prod", TagCaps: "tags,storage-role,fencing",
		TagStorageRole: "arn:aws:iam::111122223333:role/prod-StorageRole-X",
	}
	r, err := Detect(context.Background(), src(true, tags, "", true))
	if err != nil {
		t.Fatal(err)
	}
	if r.Behind() || r.Have != 2 || r.Need != Current || r.Stack != "prod" || len(r.Missing) != 0 || r.Fix != "" {
		t.Fatalf("report: %+v", r)
	}
	if !r.Has("fencing") || !r.Has("storage-role") || r.Has("peer-rule") {
		t.Errorf("enabled: %v", r.Enabled)
	}
	var b strings.Builder
	r.Render(&b)
	if b.Len() != 0 {
		t.Errorf("a stack that is up to date wrote %q", b.String())
	}
	// A stack newer than this release is not behind either.
	tags[TagInfra] = "9"
	if r, _ = Detect(context.Background(), src(true, tags, "", true)); r.Behind() || r.Have != 9 {
		t.Errorf("a newer stack: %+v", r)
	}
}

func TestGapWhenTheNodeCannotTell(t *testing.T) {
	// Not on EC2: nothing is asked, nothing is reported.
	s := src(false, nil, "", false)
	s.Tags = func(context.Context) (map[string]string, error) {
		t.Fatal("asked the metadata service off EC2")
		return nil, nil
	}
	if r, err := Detect(context.Background(), s); err != nil || r.Platform != "" {
		t.Errorf("off EC2: %+v %v", r, err)
	}
	// A metadata service that does not answer is an error, not a gap.
	boom := errors.New("timeout")
	s = src(true, nil, "", true)
	s.Tags = func(context.Context) (map[string]string, error) { return nil, boom }
	if _, err := Detect(context.Background(), s); !errors.Is(err, boom) {
		t.Errorf("want the metadata error, got %v", err)
	}
	// A tag that is not a number is reported, not read as revision 0.
	for _, bad := range []string{"two", "0", "-1", ""} {
		if _, err := Detect(context.Background(), src(true, map[string]string{TagInfra: bad}, "", true)); err == nil {
			t.Errorf("tag %q accepted", bad)
		}
	}
}

func names(m []Missing) []string {
	var out []string
	for _, x := range m {
		out = append(out, x.Capability)
	}
	return out
}

// The real path: tags from the metadata service through internal/awsapi, against its fake.
func TestGapThroughTheMetadataService(t *testing.T) {
	f := awsfake.New(t)
	f.SetEnv(t)
	f.SetIMDS(awsfake.IMDSData{InstanceID: "i-0abc", Tags: map[string]string{TagInfra: "2", TagStackName: "supavise", TagCaps: "tags, storage-role ,peer-rule"}})
	// The switch that keeps a tool from using the instance role does not hide the tags.
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	s := DefaultSource()
	s.OnEC2 = func() bool { return true }
	s.StackName = func() string { return "" }
	r, err := Detect(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if r.Have != 2 || r.Stack != "supavise" || !reflect.DeepEqual(r.Enabled, []string{"tags", "storage-role", "peer-rule"}) {
		t.Errorf("report: %+v", r)
	}
	// An instance that does not show its tags (InstanceMetadataTags off) reads like a stack
	// from before revisions when its user data ran.
	f.SetIMDS(awsfake.IMDSData{InstanceID: "i-0abc", TagsHidden: true})
	r, err = Detect(context.Background(), Source{OnEC2: s.OnEC2, Tags: s.Tags, StackName: func() string { return "" }, Bootstrapped: func() bool { return true }})
	if err != nil || r.Have != 1 || !r.Behind() {
		t.Errorf("hidden tags: %+v %v", r, err)
	}
}

func TestOnEC2ReadsTheFirmwareStrings(t *testing.T) {
	root := t.TempDir()
	write := func(rel, v string) {
		t.Helper()
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(v+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if OnEC2(root) {
		t.Error("an empty sysfs is not EC2")
	}
	write("class/dmi/id/sys_vendor", "Dell Inc.")
	write("class/dmi/id/bios_vendor", "Dell Inc.")
	if OnEC2(root) {
		t.Error("a Dell is not EC2")
	}
	write("class/dmi/id/sys_vendor", "Amazon EC2")
	if !OnEC2(root) {
		t.Error("a Nitro instance names Amazon EC2 as its vendor")
	}
	root = t.TempDir()
	write("class/dmi/id/bios_version", "4.2.amazon")
	if !OnEC2(root) {
		t.Error("a Xen instance says amazon in its BIOS version")
	}
	root = t.TempDir()
	write("hypervisor/uuid", "ec2e1916-9099-7caf-fd21-012345abcdef")
	if !OnEC2(root) {
		t.Error("an old Xen instance has an ec2 hypervisor UUID")
	}
}

func TestCapabilityTable(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range Capabilities() {
		if c.Name == "" || seen[c.Name] {
			t.Errorf("capability %q is unnamed or repeated", c.Name)
		}
		seen[c.Name] = true
		if c.Since < 2 || c.Since > Current {
			t.Errorf("%s: since revision %d (revision 1 has none; this release is %d)", c.Name, c.Since, Current)
		}
		if !c.Role && (c.Title == "" || c.Why == "" || c.Feature == "") {
			t.Errorf("%s needs a title, a reason and a feature for the status block", c.Name)
		}
		if c.Optional && c.Enable == "" {
			t.Errorf("%s is optional: say which parameter turns it on", c.Name)
		}
	}
	// A stack at the current revision lacks nothing; one a revision behind lacks what was added since.
	if got := missingSince(Current, Current); len(got) != 0 {
		t.Errorf("missing at the current revision: %v", got)
	}
}
