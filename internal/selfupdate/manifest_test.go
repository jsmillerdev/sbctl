package selfupdate

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
)

func TestManifestMarshalIsDeterministicAndRoundTrips(t *testing.T) {
	m := &Manifest{Schema: 1, Version: "v1.4.0", MinUpgradeFrom: "v1.2.0",
		Artifacts: map[string]string{"postgres": "postgres-17.11.0.004-r1", "auth": "auth-v2.195.0-r1"}, Studio: "2026.10.05-sha-94b8b06"}
	a, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := m.Marshal()
	if string(a) != string(b) {
		t.Fatal("the same manifest marshaled to different bytes")
	}
	if !strings.HasSuffix(string(a), "}\n") || strings.Index(string(a), `"auth"`) > strings.Index(string(a), `"postgres"`) {
		t.Errorf("want sorted keys and a trailing newline:\n%s", a)
	}
	got, err := ParseManifest(a)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != "v1.4.0" || got.MinUpgradeFrom != "v1.2.0" || got.Artifacts["auth"] != "auth-v2.195.0-r1" || got.Studio != m.Studio {
		t.Errorf("round trip: %+v", got)
	}
}

func TestParseManifestRefusals(t *testing.T) {
	good := `{"schema":1,"version":"v1.4.0","min_upgrade_from":"v1.2.0"}`
	if _, err := ParseManifest([]byte(good)); err != nil {
		t.Fatal(err)
	}
	// An unknown field is ignored: a later release may add one under the same schema.
	if _, err := ParseManifest([]byte(`{"schema":1,"version":"v1.4.0","min_upgrade_from":"v1.2.0","new_field":true}`)); err != nil {
		t.Errorf("unknown field: %v", err)
	}
	for name, body := range map[string]string{
		"not json":           `{`,
		"no schema":          `{"version":"v1.4.0","min_upgrade_from":"v1.2.0"}`,
		"future schema":      `{"schema":2,"version":"v1.4.0","min_upgrade_from":"v1.2.0"}`,
		"no version":         `{"schema":1,"min_upgrade_from":"v1.2.0"}`,
		"bad version":        `{"schema":1,"version":"latest","min_upgrade_from":"v1.2.0"}`,
		"no min":             `{"schema":1,"version":"v1.4.0"}`,
		"bad min":            `{"schema":1,"version":"v1.4.0","min_upgrade_from":"1.2"}`,
		"min newer":          `{"schema":1,"version":"v1.4.0","min_upgrade_from":"v1.5.0"}`,
		"min newer in patch": `{"schema":1,"version":"v1.4.0","min_upgrade_from":"v1.4.1"}`,
	} {
		if _, err := ParseManifest([]byte(body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// min_upgrade_from equal to the version, and a suffix, are fine.
	if _, err := ParseManifest([]byte(`{"schema":1,"version":"v1.4.0-rc.1","min_upgrade_from":"v1.4.0"}`)); err != nil {
		t.Errorf("equal core versions: %v", err)
	}
	if _, err := ParseManifest([]byte(`{"schema":2,"version":"v1.4.0","min_upgrade_from":"v1.2.0"}`)); err == nil ||
		!strings.Contains(err.Error(), "update with the installer") {
		t.Errorf("a future schema must say what to do: %v", err)
	}
}

func TestCheckUpgradeFrom(t *testing.T) {
	m := &Manifest{Schema: 1, Version: "v2.0.0", MinUpgradeFrom: "v1.2.0"}
	for _, c := range []struct {
		current string
		ok      bool
	}{
		{"v1.2.0", true},
		{"v1.9.9", true},
		{"v1.2.0-3-gabcdef", true}, // a build after the tag counts as the tag
		{"v1.1.9", false},
		{"v1.0.0", false},
		{"v0.9.0", false},
		{"dev", true}, // cannot be judged
		{"", true},
	} {
		err := m.CheckUpgradeFrom(c.current)
		if (err == nil) != c.ok {
			t.Errorf("CheckUpgradeFrom(%q) = %v, want ok=%v", c.current, err, c.ok)
		}
		if err != nil && !strings.Contains(err.Error(), "v1.2.0 first") {
			t.Errorf("the refusal must name the version to go through: %v", err)
		}
	}
}

func TestFetchReturnsTheSignedManifest(t *testing.T) {
	r := newReleaseServer(t, "v1.2.0", "new binary")
	o, _ := r.opts(t, "v1.0.0")
	v, err := Fetch(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if v.Release.Tag != "v1.2.0" || v.Manifest.Version != "v1.2.0" || v.Manifest.Artifacts["auth"] != "auth-v2.195.0-r1" || v.Key != CurrentKey {
		t.Errorf("%+v %+v", v.Release, v.Manifest)
	}
	// Nothing but the three small files was downloaded.
	if r.calls["supavise-linux-amd64"] != 0 {
		t.Error("Fetch downloaded the binary")
	}
	// Without any key it does not go to the network.
	o.Key = nil
	withoutEmbeddedKeys(t)
	if _, err := Fetch(context.Background(), o); err != ErrNoKey {
		t.Errorf("Fetch with no key: %v", err)
	}
}

func TestUpdateHonorsMinUpgradeFrom(t *testing.T) {
	setMin := func(r *releaseServer, min string) {
		m := &Manifest{Schema: 1, Version: r.tag, MinUpgradeFrom: min}
		b, _ := m.Marshal()
		r.manifest = b
		r.rebuild(t)
	}
	r := newReleaseServer(t, "v2.0.0", "new binary")
	setMin(r, "v1.5.0")
	o, exe := r.opts(t, "v1.2.0")
	_, err := Update(context.Background(), o)
	if err == nil || !strings.Contains(err.Error(), "upgrades from v1.5.0 or later") {
		t.Fatalf("%v", err)
	}
	if read(t, exe) != "old binary" || r.calls["supavise-linux-amd64"] != 0 {
		t.Fatal("went on to download and install a release the node cannot upgrade to")
	}
	// From the minimum itself it works.
	o, _ = r.opts(t, "v1.5.0")
	if res, err := Update(context.Background(), o); err != nil || !res.Replaced {
		t.Fatalf("%+v %v", res, err)
	}
	// --force skips the check, like it skips the "newer" check.
	o, _ = r.opts(t, "v1.2.0")
	o.Force = true
	if res, err := Update(context.Background(), o); err != nil || !res.Replaced {
		t.Fatalf("forced: %+v %v", res, err)
	}
}

func TestManifestIsSignedByTheSameKeyAsTheChecksums(t *testing.T) {
	// A manifest swapped for another one signed by another key: the list it is checked against
	// is not the attacker's to sign.
	r := newReleaseServer(t, "v1.2.0", "new binary")
	_, evil, _ := ed25519.GenerateKey(rand.Reader)
	m := &Manifest{Schema: 1, Version: "v1.2.0", MinUpgradeFrom: "v0.0.0", Studio: "evil"}
	r.manifest, _ = m.Marshal()
	r.sums = r.sumsFor(r.bin, r.manifest)
	r.sig = ed25519.Sign(evil, r.sums)
	o, _ := r.opts(t, "v1.0.0")
	if _, err := Fetch(context.Background(), o); err == nil || !strings.Contains(err.Error(), "does not verify") {
		t.Fatalf("%v", err)
	}
}
