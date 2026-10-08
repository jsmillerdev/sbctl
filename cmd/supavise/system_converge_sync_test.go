package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/hostsetup"
	"github.com/supavise/supavise/internal/mesh/peerapi"
)

func syncOf(t *testing.T, text string) (*clusterConfigSync, string) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	sum := sha256.Sum256([]byte(text))
	cc := &peerapi.ClusterConfig{Revision: hex.EncodeToString(sum[:]), TOML: text}
	s := &clusterConfigSync{ConfigPath: cfgPath, UID: os.Getuid(), GID: os.Getgid(),
		Fetch: func(context.Context) (*peerapi.ClusterConfig, error) { return cc, nil }}
	return s, filepath.Join(dir, config.ConfigDName, config.ClusterConfigFile)
}

const leaderText = "# Cluster settings\ndomain = \"example.com\"\n\n[backup]\nbackend = \"s3://bucket/prefix\"\n\n[fleet]\nstorage_backend = \"s3\"\nstorage_s3_secret_access_key = \"s3cret\"\n"

// The file is written 0600 (it can hold secrets), only when it differs, and the dry run writes nothing.
func TestClusterConfigSyncWritesTheLeadersSettings(t *testing.T) {
	ctx := context.Background()
	s, path := syncOf(t, leaderText)
	got, err := s.Sync(ctx, true)
	if err != nil || len(got) != 1 || got[0] != path {
		t.Fatalf("dry run: %v, %v", got, err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("the dry run wrote the file")
	}
	if got, err = s.Sync(ctx, false); err != nil || len(got) != 1 {
		t.Fatalf("sync: %v, %v", got, err)
	}
	b, err := os.ReadFile(path)
	fi, _ := os.Stat(path)
	if err != nil || string(b) != leaderText || fi.Mode().Perm() != 0o600 {
		t.Errorf("file: %q mode %v, %v", b, fi.Mode(), err)
	}
	// Nothing differs the second time.
	if got, err = s.Sync(ctx, false); err != nil || len(got) != 0 {
		t.Errorf("second sync: %v, %v", got, err)
	}
	// A file that was loosened is written again with the mode it should have only when its text differs;
	// the text is the same here, so it stays: the mode is the writer's business at the write.
	if err := os.WriteFile(path, []byte("domain = \"old.example\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err = s.Sync(ctx, false); err != nil || len(got) != 1 {
		t.Fatalf("sync over an older file: %v, %v", got, err)
	}
	if fi, _ = os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode after the rewrite: %v", fi.Mode())
	}
}

// What the leader sends is checked before it is written: a revision that does not match, a text that
// does not parse and a text that sets a node-local key are refused and leave the file alone.
func TestClusterConfigSyncRefusesWhatItCannotTrust(t *testing.T) {
	ctx := context.Background()
	for name, c := range map[string]struct{ text, want string }{
		"a node-local key": {"[ports]\nproject_base = 1\n", "ports.project_base"},
		"the node's name":  {"domain = \"a\"\n[node]\nname = \"n2\"\n", "node.name"},
		"not toml":         {"domain = = \n", "do not parse"},
	} {
		s, path := syncOf(t, c.text)
		_, err := s.Sync(ctx, false)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
		if _, err := os.Stat(path); err == nil {
			t.Errorf("%s: the file was written", name)
		}
	}
	s, path := syncOf(t, leaderText)
	cc, _ := s.Fetch(ctx)
	cc.Revision = strings.Repeat("0", 64)
	if _, err := s.Sync(ctx, false); err == nil || !strings.Contains(err.Error(), "revision") {
		t.Errorf("a wrong revision: %v", err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("a text with the wrong revision was written")
	}
}

// A server that leads, or is not in a cluster, has nothing to fetch; an error from the leader comes
// back as an error (the step turns it into a warning).
func TestClusterConfigSyncWhenThereIsNothingToFetch(t *testing.T) {
	ctx := context.Background()
	s, path := syncOf(t, leaderText)
	s.Fetch = func(context.Context) (*peerapi.ClusterConfig, error) { return nil, nil }
	if got, err := s.Sync(ctx, false); err != nil || len(got) != 0 {
		t.Errorf("leader: %v, %v", got, err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("a file was written on the leader")
	}
	s.Fetch = func(context.Context) (*peerapi.ClusterConfig, error) { return nil, errors.New("the leader is down") }
	if _, err := s.Sync(ctx, false); err == nil || !strings.Contains(err.Error(), "the leader is down") {
		t.Errorf("leader down: %v", err)
	}
}

// A server without a mesh identity has no such step.
func TestNewClusterConfigSyncNeedsAMeshIdentity(t *testing.T) {
	dir := t.TempDir()
	if cs := newClusterConfigSync(config.Default(), filepath.Join(dir, "config.toml")); cs != nil {
		t.Error("a server without node.crt got the cluster settings step")
	}
	if err := os.MkdirAll(filepath.Join(dir, config.ClusterDirName), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, config.ClusterDirName, config.NodeCertFile), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if cs := newClusterConfigSync(config.Default(), filepath.Join(dir, "config.toml")); cs == nil {
		t.Error("a server with a mesh identity has no cluster settings step")
	}
}

// converge --check tells a step that was not checked from one that is in order: a leader that does
// not answer must not read as "host is converged".
func TestRenderCheckSaysWhatWasNotChecked(t *testing.T) {
	ok := hostsetup.Result{ID: "units", Title: "Install the systemd units"}
	unknown := hostsetup.Result{ID: "config-d", Title: "Refresh the cluster settings", Unknown: true, Detail: "could not compare with the leader's cluster settings: refused"}
	pending := hostsetup.Result{ID: "ufw", Title: "Open the mesh port", Pending: true}
	for _, c := range []struct {
		name string
		rs   []hostsetup.Result
		want []string
		not  string
	}{
		{"all in order", []hostsetup.Result{ok}, []string{"host is converged"}, "could not be checked"},
		{"one not checked", []hostsetup.Result{ok, unknown}, []string{"unknown  Refresh the cluster settings", "nothing is pending, but 1 step(s) could not be checked"}, "host is converged"},
		{"pending and not checked", []hostsetup.Result{pending, unknown}, []string{"pending  Open the mesh port", "1 step(s) pending", "1 step(s) could not be checked"}, "host is converged"},
	} {
		var b bytes.Buffer
		renderCheck(&b, c.rs)
		for _, w := range c.want {
			if !strings.Contains(b.String(), w) {
				t.Errorf("%s: output lacks %q:\n%s", c.name, w, b.String())
			}
		}
		if strings.Contains(b.String(), c.not) {
			t.Errorf("%s: output has %q:\n%s", c.name, c.not, b.String())
		}
	}
}
