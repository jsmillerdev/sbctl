package selfupdate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestManifestCheck(t *testing.T) {
	for _, tc := range []struct {
		name, body, tag, current string
		want                     string // substring of the refusal; empty means allowed
	}{
		{"no limit", `{"version":"v1.4.0"}`, "v1.4.0", "v0.1.0", ""},
		{"at the minimum", `{"min_upgrade_from":"v1.2.0"}`, "v1.4.0", "v1.2.0", ""},
		{"above the minimum", `{"min_upgrade_from":"v1.2.0"}`, "v1.4.0", "v1.3.5-2-gabc", ""},
		{"below the minimum", `{"min_upgrade_from":"v1.2.0"}`, "v1.4.0", "v1.1.9", "can be installed from v1.2.0"},
		{"a development build is let through", `{"min_upgrade_from":"v1.2.0"}`, "v1.4.0", "dev", ""},
		{"a manifest of another version", `{"version":"v1.3.0"}`, "v1.4.0", "v1.3.9", "ships a manifest for v1.3.0"},
		{"unknown fields are ignored", `{"min_upgrade_from":"v1.0.0","notes":"x"}`, "v1.4.0", "v1.0.0", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, err := ParseManifest([]byte(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			err = m.Check(tc.tag, tc.current)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
	if _, err := ParseManifest([]byte(`{"min_upgrade_from":"soon"}`)); err == nil {
		t.Fatal("a minimum that is not a version was accepted")
	}
	if _, err := ParseManifest([]byte(`not json`)); err == nil {
		t.Fatal("garbage was accepted")
	}
}

func TestResolveEnforcesTheSignedManifest(t *testing.T) {
	ctx := context.Background()

	// A release below the node's minimum is refused by Update as well as by Resolve, and the
	// installed binary is left alone.
	r := newReleaseServer(t, "v1.4.0", "new binary").withManifest(`{"version":"v1.4.0","min_upgrade_from":"v1.2.0"}`, true)
	o, exe := r.opts(t, "v1.1.0")
	if _, err := Resolve(ctx, o); !errors.Is(err, ErrUnsupportedJump) {
		t.Fatalf("Resolve below the minimum: %v", err)
	}
	if _, err := Update(ctx, o); !errors.Is(err, ErrUnsupportedJump) {
		t.Fatalf("Update below the minimum: %v", err)
	}
	if read(t, exe) != "old binary" || r.calls["supavise-linux-amd64"] != 0 {
		t.Fatal("a refused jump downloaded or replaced the binary")
	}
	o.Current = "v1.2.0"
	res, err := Resolve(ctx, o)
	if err != nil || res.Manifest == nil || res.Manifest.MinUpgradeFrom != "v1.2.0" {
		t.Fatalf("Resolve at the minimum: %+v %v", res, err)
	}

	// A manifest the signature does not cover could say anything.
	r = newReleaseServer(t, "v1.4.0", "new binary").withManifest(`{"min_upgrade_from":"v0.0.1"}`, false)
	o, _ = r.opts(t, "v1.1.0")
	if _, err := Resolve(ctx, o); err == nil || !strings.Contains(err.Error(), "does not cover it") {
		t.Fatalf("unsigned manifest: %v", err)
	}

	// A manifest that was changed after signing does not match its listed checksum.
	r = newReleaseServer(t, "v1.4.0", "new binary").withManifest(`{"min_upgrade_from":"v1.2.0"}`, true)
	r.manifest = []byte(`{"min_upgrade_from":"v0.0.1"}`)
	o, _ = r.opts(t, "v1.1.0")
	if _, err := Resolve(ctx, o); err == nil || !strings.Contains(err.Error(), "does not match its checksum") {
		t.Fatalf("tampered manifest: %v", err)
	}

	// A release with no manifest has no limit.
	r = newReleaseServer(t, "v1.4.0", "new binary")
	o, _ = r.opts(t, "v0.0.1")
	if res, err := Resolve(ctx, o); err != nil || res.Manifest != nil {
		t.Fatalf("no manifest: %+v %v", res, err)
	}
}

func TestStageThenInstallOrDiscard(t *testing.T) {
	ctx := context.Background()
	r := newReleaseServer(t, "v1.2.0", "new binary")
	o, exe := r.opts(t, "v1.1.0")
	stage := t.TempDir()
	o.StageDir = stage
	res, err := Resolve(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	st, err := res.Stage(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(st.Path) != stage || read(t, st.Path) != "new binary" || read(t, exe) != "old binary" {
		t.Fatalf("staged at %s, installed binary %q", st.Path, read(t, exe))
	}
	st.Discard()
	if _, err := os.Stat(st.Path); !os.IsNotExist(err) {
		t.Fatalf("Discard left %s", st.Path)
	}
}

func TestStudioIsTakenFromTheSignedList(t *testing.T) {
	r := newReleaseServer(t, "v1.2.0", "new binary")
	r.sums = append(r.sums, []byte(strings.Repeat("c", 64)+"  supavise-studio-2026.10.05-linux-amd64.tar.zst\n"+strings.Repeat("d", 64)+"  supavise-studio-2026.10.05-linux-arm64.tar.zst\n")...)
	o, _ := r.opts(t, "v1.1.0")
	res := &Resolved{Release: &Release{Tag: "v1.2.0", Assets: map[string]string{"supavise-studio-2026.10.05-linux-amd64.tar.zst": "http://x/amd64"}}, Sums: r.sums, platform: o.Platform}
	name, url, sha, ok := res.Studio()
	if !ok || name != "supavise-studio-2026.10.05-linux-amd64.tar.zst" || url != "http://x/amd64" || sha != strings.Repeat("c", 64) {
		t.Fatalf("Studio() = %q %q %q %v", name, url, sha, ok)
	}
	res.Release.Assets = map[string]string{}
	if _, _, _, ok := res.Studio(); ok {
		t.Fatal("a Studio line without an asset to download was offered")
	}
}

func TestCompare(t *testing.T) {
	if c, ok := Compare("v1.2.0", "v1.10.0"); !ok || c >= 0 {
		t.Fatalf("v1.2.0 vs v1.10.0 = %d %v", c, ok)
	}
	if c, ok := Compare("v1.2.0-rc1", "v1.2.0"); !ok || c != 0 {
		t.Fatalf("a suffix counts as the tag: %d %v", c, ok)
	}
	if _, ok := Compare("dev", "v1.0.0"); ok {
		t.Fatal("dev was placed")
	}
}
