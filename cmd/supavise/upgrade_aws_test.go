package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/supavise/supavise/internal/awsapi/awsfake"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/infra"
	"github.com/supavise/supavise/internal/nodeupgrade"
	"github.com/supavise/supavise/internal/selfupdate"
)

func TestAWSCredentialsPresent(t *testing.T) {
	home := t.TempDir()
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if awsCredentialsPresent(env(nil), home) {
		t.Error("an empty environment has credentials")
	}
	for name, m := range map[string]map[string]string{
		"keys":               {"AWS_ACCESS_KEY_ID": "AKIA", "AWS_SECRET_ACCESS_KEY": "s"},
		"web identity":       {"AWS_WEB_IDENTITY_TOKEN_FILE": "/x"},
		"container":          {"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI": "/x"},
		"a credentials file": {"AWS_SHARED_CREDENTIALS_FILE": filepath.Join(home, "creds")},
	} {
		if name == "a credentials file" {
			if err := os.WriteFile(m["AWS_SHARED_CREDENTIALS_FILE"], []byte("[default]\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if !awsCredentialsPresent(env(m), t.TempDir()) {
			t.Errorf("%s: not found", name)
		}
	}
	// One half of a key pair is no credential, and neither is a session token alone.
	if awsCredentialsPresent(env(map[string]string{"AWS_ACCESS_KEY_ID": "AKIA"}), home) || awsCredentialsPresent(env(map[string]string{"AWS_SESSION_TOKEN": "t"}), home) {
		t.Error("half a key pair counted")
	}
	// ~/.aws/credentials and ~/.aws/config in the home directory.
	if err := os.MkdirAll(filepath.Join(home, ".aws"), 0o700); err != nil {
		t.Fatal(err)
	}
	if awsCredentialsPresent(env(nil), home) {
		t.Error("an empty .aws directory counted")
	}
	if err := os.WriteFile(filepath.Join(home, ".aws", "config"), []byte("[default]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !awsCredentialsPresent(env(nil), home) {
		t.Error("~/.aws/config not found")
	}
}

func TestAWSCommandAndArgs(t *testing.T) {
	got := awsUpdateArgs("supavise", "/tmp/x/supavise.yaml", []string{"Failover=on", "PeerCidr1=203.0.113.7/32"})
	want := "update --stack supavise --template /tmp/x/supavise.yaml --params-from-stack --set Failover=on --set PeerCidr1=203.0.113.7/32"
	if strings.Join(got, " ") != want {
		t.Errorf("args = %q", got)
	}
	if c := awsCommand("supavise", []string{"Failover=on"}); c != "sudo -E supavise upgrade --aws --stack-name supavise --set Failover=on" {
		t.Errorf("command = %q", c)
	}
	if c := awsCommand("s", []string{"X=a b"}); c != "sudo -E supavise upgrade --aws --stack-name s --set 'X=a b'" {
		t.Errorf("command = %q", c)
	}
}

func TestCheckAWSFlags(t *testing.T) {
	for _, c := range []struct {
		aws  bool
		name string
		sets []string
		want string
	}{
		{false, "", nil, ""},
		{true, "", nil, ""},
		{true, "supavise-b", []string{"Failover=on"}, ""},
		{false, "supavise", nil, "belong to --aws"},
		{false, "", []string{"Failover=on"}, "belong to --aws"},
		{true, "1bad name", nil, "not a CloudFormation stack name"},
		{true, "", []string{"Failover"}, "Parameter=Value"},
		{true, "", []string{"=on"}, "Parameter=Value"},
	} {
		err := checkAWSFlags(c.aws, c.name, c.sets)
		if (c.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%+v: %v", c, err)
		}
	}
}

func TestEC2Likely(t *testing.T) {
	old := dmiDir
	defer func() { dmiDir = old }()
	t.Setenv(awsapiEndpointIMDS, "")
	dmiDir = t.TempDir()
	if ec2Likely() {
		t.Error("an empty firmware table looks like EC2")
	}
	if err := os.WriteFile(filepath.Join(dmiDir, "sys_vendor"), []byte("Microsoft Corporation\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ec2Likely() {
		t.Error("an Azure VM looks like EC2")
	}
	if err := os.WriteFile(filepath.Join(dmiDir, "sys_vendor"), []byte("Amazon EC2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !ec2Likely() {
		t.Error("Amazon EC2 does not")
	}
	dmiDir = t.TempDir()
	t.Setenv(awsapiEndpointIMDS, "http://127.0.0.1:1")
	if !ec2Likely() {
		t.Error("a configured metadata endpoint does not")
	}
}

const awsapiEndpointIMDS = "AWS_ENDPOINT_URL_IMDS"

// awsRelease serves a script and a template and returns the candidate `UpdateStack` takes. The
// script writes its arguments to $RECORD and exits with $EXIT_CODE.
type awsRelease struct {
	cand   *nodeupgrade.Candidate
	files  map[string][]byte
	record string
}

func newAWSRelease(t *testing.T) *awsRelease {
	t.Helper()
	r := &awsRelease{record: filepath.Join(t.TempDir(), "args")}
	script := []byte("#!/usr/bin/env bash\nprintf '%s\\n' \"$@\" >\"$RECORD\"\ncat \"${5:-/dev/null}\" >>\"$RECORD\"\nexit \"${EXIT_CODE:-0}\"\n")
	tmpl := []byte("AWSTemplateFormatVersion: 2010-09-09\n")
	r.files = map[string][]byte{selfupdate.AWSDeployAsset: script, "supavise.yaml": tmpl}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if b, ok := r.files[strings.TrimPrefix(req.URL.Path, "/")]; ok {
			_, _ = w.Write(b)
			return
		}
		http.NotFound(w, req)
	}))
	t.Cleanup(srv.Close)
	sum := func(b []byte) string { s := sha256.Sum256(b); return fmt.Sprintf("%x", s) }
	ver := &selfupdate.Verified{
		Release: &selfupdate.Release{Tag: "v1.2.0", Assets: map[string]string{selfupdate.AWSDeployAsset: srv.URL + "/" + selfupdate.AWSDeployAsset, "supavise.yaml": srv.URL + "/supavise.yaml"}},
		Sums:    []byte(fmt.Sprintf("%s  %s\n%s  supavise.yaml\n", sum(script), selfupdate.AWSDeployAsset, sum(tmpl))),
		Manifest: &selfupdate.Manifest{Schema: 1, Version: "v1.2.0", MinUpgradeFrom: "v0.0.0",
			AWS: &selfupdate.ManifestAWS{StackRevision: 2, TemplateAsset: "supavise.yaml", TemplateSHA256: sum(tmpl)}},
	}
	r.cand = &nodeupgrade.Candidate{Tag: "v1.2.0", Data: &resolved{ver: ver, opts: selfupdate.Options{HTTP: srv.Client()}}}
	t.Setenv("RECORD", r.record)
	return r
}

func awsHost(t *testing.T) (*nodeHost, string, *bytes.Buffer) {
	t.Helper()
	dir := t.TempDir()
	var out bytes.Buffer
	cfg := config.Default()
	return &nodeHost{cfg: cfg, out: &out, errw: &out, cfgPath: filepath.Join(dir, "config.toml"), root: true}, dir, &out
}

func TestUpdateStackRunsTheVerifiedScript(t *testing.T) {
	rel := newAWSRelease(t)
	h, dir, _ := awsHost(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAOPERATOR")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	t.Setenv("EXIT_CODE", "0")

	out, err := h.UpdateStack(context.Background(), rel.cand, nodeupgrade.StackOptions{Name: "supavise", Sets: []string{"Failover=on"}})
	if err != nil || out.Command != "" {
		t.Fatalf("%+v, %v", out, err)
	}
	b, _ := os.ReadFile(rel.record)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) < 9 || strings.Join(lines[:3], " ") != "update --stack supavise" || lines[3] != "--template" || !strings.HasSuffix(lines[4], "supavise.yaml") ||
		lines[5] != "--params-from-stack" || lines[6] != "--set" || lines[7] != "Failover=on" {
		t.Fatalf("script arguments:\n%s", b)
	}
	if !strings.Contains(string(b), "AWSTemplateFormatVersion") {
		t.Errorf("the template the script was given is not the release's:\n%s", b)
	}
	if _, err := os.Stat(lines[4]); err == nil {
		t.Error("the staged template was left behind")
	}
	// The stack is remembered for the next time.
	got, err := os.ReadFile(filepath.Join(dir, "config.d", "20-aws.toml"))
	if err != nil || !strings.Contains(string(got), `stack_name = "supavise"`) {
		t.Errorf("20-aws.toml = %q, %v", got, err)
	}
	if cfg, err := config.Load(filepath.Join(dir, "config.toml")); err != nil || cfg.AWS.StackName != "supavise" {
		t.Errorf("config.Load: %+v, %v", cfg.AWS, err)
	}
}

// stackHost is a node whose only work is its AWS stack, the way a stack-only `upgrade --aws` sees it.
// It implements what that run calls and nothing else (the embedded Host is nil: a call to any other
// method is a failure of the test); the stack update itself is the real nodeHost's.
type stackHost struct {
	nodeupgrade.Host
	node *nodeupgrade.Node
	cand *nodeupgrade.Candidate
	real *nodeHost
}

func (s *stackHost) Inspect(context.Context) (*nodeupgrade.Node, error) { return s.node, nil }
func (s *stackHost) Resolve(context.Context, string, *nodeupgrade.Node) (*nodeupgrade.Candidate, error) {
	return s.cand, nil
}
func (s *stackHost) UpdateStack(ctx context.Context, c *nodeupgrade.Candidate, o nodeupgrade.StackOptions) (nodeupgrade.StackOutcome, error) {
	return s.real.UpdateStack(ctx, c, o)
}

func newStackHost(t *testing.T, rel *awsRelease) *stackHost {
	t.Helper()
	info := &nodeupgrade.Info{Version: "v1.2.0", Platform: "linux-amd64", Pins: map[string]string{"gotrue": "auth-v1"},
		RegistrySchema: "1.sql", RegistryMigrations: []string{"1.sql"}, ConvergeRevision: 2, InfraRevision: 2}
	node := &nodeupgrade.Node{Version: "v1.2.0", BinaryInfo: info, Pins: info.Pins, AppliedMigrations: info.RegistryMigrations, Platform: info.Platform,
		Verdict: nodeupgrade.VerdictHealthy, ConvergeKnown: true, ConvergeRevision: 2,
		Infra: &infra.Report{Platform: "aws", Stack: "supavise-b", Have: 1, Need: 2}}
	h, _, _ := awsHost(t)
	return &stackHost{node: node, cand: rel.cand, real: h}
}

// The chain of `supavise upgrade --aws`, from the command line to the script: the flags make the
// options, the run reaches the stack step, and the host runs the release's script with the stack and
// the parameters the operator named. A flag that stops short of the options leaves `--aws` a plain
// upgrade, with the stack untouched and a `--set` dropped without a word.
func TestUpgradeAWSFlagsReachTheScript(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAOPERATOR")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	t.Setenv("EXIT_CODE", "0")
	run := func(rel *awsRelease, args ...string) (string, error) {
		t.Helper()
		up, fl := newUpgradeCmd()
		if err := up.ParseFlags(args); err != nil {
			t.Fatal(err)
		}
		if err := fl.validate(); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		err := nodeupgrade.Run(context.Background(), newStackHost(t, rel), upgradeOptions(cfg, &out, fl))
		return out.String(), err
	}

	rel := newAWSRelease(t)
	out, err := run(rel, "--aws", "--stack-name", "supavise-b", "--set", "Failover=on", "--set", "PeerCidr1=203.0.113.7/32", "--yes")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	b, rerr := os.ReadFile(rel.record)
	if rerr != nil {
		t.Fatalf("the script did not run:\n%s", out)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) < 11 || strings.Join(lines[:3], " ") != "update --stack supavise-b" || lines[5] != "--params-from-stack" ||
		strings.Join(lines[6:10], " ") != "--set Failover=on --set PeerCidr1=203.0.113.7/32" {
		t.Errorf("script arguments:\n%s", b)
	}

	// Without --aws the plan names the gap and nothing runs.
	rel = newAWSRelease(t)
	out, err = run(rel, "--yes")
	if err != nil || !strings.Contains(out, "supavise upgrade --aws") {
		t.Errorf("without --aws: %v\n%s", err, out)
	}
	if _, err := os.Stat(rel.record); err == nil {
		t.Error("the stack was updated without --aws")
	}

	// --plan --aws prints the plan and runs nothing.
	rel = newAWSRelease(t)
	if out, err = run(rel, "--aws", "--plan"); err != nil {
		t.Errorf("--plan --aws: %v\n%s", err, out)
	}
	if _, err := os.Stat(rel.record); err == nil {
		t.Error("--plan updated the stack")
	}

	// The script's refusal is the run's: exit 2, and the message says what the script said.
	rel = newAWSRelease(t)
	t.Setenv("EXIT_CODE", "2")
	if _, err = run(rel, "--aws", "--stack-name", "supavise-b", "--yes"); nodeupgrade.ExitCode(err) != nodeupgrade.ExitRefused || !strings.Contains(err.Error(), "was refused") {
		t.Errorf("a refused change set: %v", err)
	}
}

// What the node's own type has to be for the run to find it: the optional capabilities are looked
// up by interface, and a method whose signature drifts takes the capability away without an error.
var (
	_ nodeupgrade.StackUpdater  = (*nodeHost)(nil)
	_ nodeupgrade.HostConverger = (*nodeHost)(nil)
)

func TestUpdateStackMapsTheScriptsExitStatus(t *testing.T) {
	rel := newAWSRelease(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAOPERATOR")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	for _, code := range []string{"2", "3"} {
		h, dir, _ := awsHost(t)
		t.Setenv("EXIT_CODE", code)
		_, err := h.UpdateStack(context.Background(), rel.cand, nodeupgrade.StackOptions{Name: "supavise"})
		var se *nodeupgrade.StackError
		if !errors.As(err, &se) || fmt.Sprint(se.Code) != code {
			t.Errorf("exit %s: %v", code, err)
		}
		if _, err := os.Stat(filepath.Join(dir, "config.d")); err == nil {
			t.Errorf("exit %s: the stack name was recorded for a stack that was not updated", code)
		}
	}
}

// Without credentials of the operator's nothing is downloaded or run, and the command is printed.
func TestUpdateStackWithoutCredentialsPrintsTheCommand(t *testing.T) {
	rel := newAWSRelease(t)
	for _, k := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_PROFILE", "AWS_WEB_IDENTITY_TOKEN_FILE",
		"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "AWS_CONTAINER_CREDENTIALS_FULL_URI", "AWS_SHARED_CREDENTIALS_FILE", "AWS_CONFIG_FILE"} {
		t.Setenv(k, "")
	}
	t.Setenv("HOME", t.TempDir()) // no ~/.aws
	h, _, _ := awsHost(t)
	out, err := h.UpdateStack(context.Background(), rel.cand, nodeupgrade.StackOptions{Name: "supavise-b", Sets: []string{"PeerCidr1=203.0.113.7/32"}})
	if err != nil || out.Command != "sudo -E supavise upgrade --aws --stack-name supavise-b --set PeerCidr1=203.0.113.7/32" {
		t.Fatalf("%+v, %v", out, err)
	}
	if _, err := os.Stat(rel.record); err == nil {
		t.Error("the script ran without credentials")
	}
}

func TestUpdateStackRefusesWhatTheSignedListDoesNotSay(t *testing.T) {
	rel := newAWSRelease(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAOPERATOR")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	h, _, _ := awsHost(t)
	rel.files[selfupdate.AWSDeployAsset] = []byte("#!/bin/sh\nrm -rf /\n")
	_, err := h.UpdateStack(context.Background(), rel.cand, nodeupgrade.StackOptions{Name: "supavise"})
	if err == nil || !strings.Contains(err.Error(), "does not match its checksum") {
		t.Errorf("err = %v", err)
	}
	if _, err := os.Stat(rel.record); err == nil {
		t.Error("a script that does not match the signed list ran")
	}
}

// The stack is the flag, else [aws] stack_name, else the instance's tag; a stack from before the
// tags must be named.
func TestStackNameResolution(t *testing.T) {
	ctx := context.Background()
	h, _, _ := awsHost(t)
	if n, err := h.stackName(ctx, "from-flag"); err != nil || n != "from-flag" {
		t.Errorf("flag: %q, %v", n, err)
	}
	h.cfg.AWS.StackName = "from-config"
	if n, err := h.stackName(ctx, ""); err != nil || n != "from-config" {
		t.Errorf("config: %q, %v", n, err)
	}
	if n, err := h.stackName(ctx, "flag-wins"); err != nil || n != "flag-wins" {
		t.Errorf("flag over config: %q, %v", n, err)
	}
	h.cfg.AWS.StackName = ""

	fake := awsfake.New(t)
	fake.SetEnv(t)
	d := fake.IMDS()
	d.Tags = map[string]string{"supavise:stack-name": "from-tag"}
	fake.SetIMDS(d)
	if n, err := h.stackName(ctx, ""); err != nil || n != "from-tag" {
		t.Errorf("tag: %q, %v", n, err)
	}

	// A stack from before the tags: no tag, so it has to be named.
	d.Tags, d.TagsHidden = nil, true
	fake.SetIMDS(d)
	if _, err := h.stackName(ctx, ""); err == nil || !strings.Contains(err.Error(), "--stack-name") {
		t.Errorf("no tag: %v", err)
	}
	if _, err := h.stackName(ctx, "not a name"); err == nil {
		t.Error("a bad name was accepted")
	}
}
