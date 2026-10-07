package selfupdate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Stage downloads and checks the binary without installing it; Install then swaps it in, Discard
// throws it away. `supavise upgrade` reads the staged binary's pins before it touches the node.
func TestStageThenInstallOrDiscard(t *testing.T) {
	ctx := context.Background()
	r := newReleaseServer(t, "v1.2.0", "new binary")
	o, exe := r.opts(t, "v1.1.0")
	stage := t.TempDir()
	o.StageDir = stage
	ver, err := Fetch(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if r.calls["supavise-linux-amd64"] != 0 {
		t.Fatal("Fetch downloaded the binary")
	}
	st, err := ver.Stage(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(st.Path) != stage || read(t, st.Path) != "new binary" || read(t, exe) != "old binary" {
		t.Fatalf("staged at %s, installed binary %q", st.Path, read(t, exe))
	}
	if fi, _ := os.Stat(st.Path); fi.Mode().Perm() != 0o755 {
		t.Fatalf("staged mode %v", fi.Mode())
	}
	st.Discard()
	if _, err := os.Stat(st.Path); !os.IsNotExist(err) {
		t.Fatalf("Discard left %s", st.Path)
	}
	if read(t, exe) != "old binary" {
		t.Fatal("Discard touched the installed binary")
	}

	// Staged next to the binary, Install is one rename and keeps the old one as .prev.
	o.StageDir = ""
	st, err = ver.Stage(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	res, err := st.Install()
	if err != nil || !res.Replaced || res.Tag != "v1.2.0" || read(t, exe) != "new binary" || read(t, res.Previous) != "old binary" {
		t.Fatalf("Install: %+v %v", res, err)
	}
}

func TestStageRefusesWhatUpdateRefuses(t *testing.T) {
	ctx := context.Background()
	r := newReleaseServer(t, "v1.2.0", "new binary")
	o, exe := r.opts(t, "v1.1.0")
	ver, err := Fetch(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	r.bin = []byte("tampered")
	if _, err := ver.Stage(ctx, o); err == nil || !strings.Contains(err.Error(), "does not match its checksum") {
		t.Fatalf("tampered binary: %v", err)
	}
	r.bin = []byte("new binary")
	o.Probe = func(context.Context, string) (string, error) { return "supavise version v1.0.0", nil }
	if _, err := ver.Stage(ctx, o); err == nil || !strings.Contains(err.Error(), "refusing to install") {
		t.Fatalf("older binary under a newer tag: %v", err)
	}
	ents, _ := os.ReadDir(filepath.Dir(exe))
	for _, e := range ents {
		if strings.Contains(e.Name(), ".new-") {
			t.Fatalf("a refused stage left %s behind", e.Name())
		}
	}
}

func TestStudioIsTakenFromTheSignedList(t *testing.T) {
	sums := []byte(strings.Repeat("c", 64) + "  supavise-studio-2026.10.05-linux-amd64.tar.zst\n" + strings.Repeat("d", 64) + "  supavise-studio-2026.10.05-linux-arm64.tar.zst\n" + strings.Repeat("e", 64) + "  supavise-linux-amd64\n")
	ver := &Verified{Release: &Release{Tag: "v1.2.0", Assets: map[string]string{"supavise-studio-2026.10.05-linux-amd64.tar.zst": "http://x/amd64"}}, Sums: sums}
	name, url, sha, ok := ver.Studio("linux-amd64")
	if !ok || name != "supavise-studio-2026.10.05-linux-amd64.tar.zst" || url != "http://x/amd64" || sha != strings.Repeat("c", 64) {
		t.Fatalf("Studio() = %q %q %q %v", name, url, sha, ok)
	}
	if _, _, _, ok := ver.Studio("linux-arm64"); ok {
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
	if c, ok := Compare("v2.0.0", "v1.9.9"); !ok || c <= 0 {
		t.Fatalf("v2.0.0 vs v1.9.9 = %d %v", c, ok)
	}
	if _, ok := Compare("dev", "v1.0.0"); ok {
		t.Fatal("dev was placed")
	}
}
