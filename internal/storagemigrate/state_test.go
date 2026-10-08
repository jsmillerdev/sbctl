package storagemigrate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadStateRefusesAFileThatIsNotJSON(t *testing.T) {
	e := newEnv(t)
	if err := os.WriteFile(filepath.Join(serviceDir(e.paths), stateFile), []byte("{nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadState(e.paths); err == nil || !strings.Contains(err.Error(), stateFile) {
		t.Errorf("ReadState = %v", err)
	}
}

func TestStateRoundTripKeepsNoSecret(t *testing.T) {
	e := newEnv(t)
	st := &State{ID: "1", Phase: PhaseFlipping, Step: stepFence, Dest: Destination{Bucket: "b", Region: "r", PathStyle: true},
		Credentials: CredFile, CredentialsFile: "/etc/supavise/creds", Uploaded: Counts{Files: 3, Bytes: 9},
		Skipped: []Skipped{{Path: `"a"`, Reason: "the name is not valid UTF-8"}}, SkippedTotal: 1}
	now := time.Now()
	if err := saveState(e.paths, st, now); err != nil {
		t.Fatal(err)
	}
	got, err := ReadState(e.paths)
	if err != nil || got.Phase != PhaseFlipping || got.Step != stepFence || got.Dest != st.Dest || got.Uploaded != st.Uploaded || got.PID != os.Getpid() {
		t.Errorf("round trip: %+v, %v", got, err)
	}
	if d := got.UpdatedAt.Sub(now); d < -time.Second || d > time.Second {
		t.Errorf("updated_at %s, saved at %s", got.UpdatedAt, now)
	}
	if fi, err := os.Stat(filepath.Join(serviceDir(e.paths), stateFile)); err != nil || fi.Mode().Perm() != 0o644 {
		t.Errorf("state file: %v, %v", fi, err)
	}
	// Credentials print without their key.
	c := Credentials{Source: CredFile, AccessKeyID: "AKIAEXAMPLE", SecretAccessKey: "topsecret"}
	for _, s := range []string{c.String(), c.GoString()} {
		if strings.Contains(s, "topsecret") || strings.Contains(s, "AKIAEXAMPLE") {
			t.Errorf("a credentials value prints its key: %s", s)
		}
	}
}

func TestBytesString(t *testing.T) {
	for n, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1024: "1.0 KiB", 300_000: "293.0 KiB", 80 << 20: "80.0 MiB", 3 << 30: "3.0 GiB"} {
		if got := bytesString(n); got != want {
			t.Errorf("bytesString(%d) = %q, want %q", n, got, want)
		}
	}
}
