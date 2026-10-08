package hold

import (
	"os"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/config"
)

func TestHeldUntilItExpires(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	p := cfg.Paths()
	now := time.Now()
	if Held(p, now) || ClearStale(p, now) || Remove(p) != nil {
		t.Fatal("a marker that is not there")
	}
	if err := Write(p, Marker{PID: 7, Since: now, Until: now.Add(time.Minute), Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(Path(p)); err != nil || fi.Mode().Perm() != 0o644 {
		t.Errorf("marker: %v, %v", fi, err)
	}
	if !Held(p, now) || Held(p, now.Add(2*time.Minute)) {
		t.Error("Held follows the expiry")
	}
	if ClearStale(p, now) || !Held(p, now) {
		t.Error("a live marker was cleared")
	}
	if !ClearStale(p, now.Add(2*time.Minute)) || Held(p, now) {
		t.Error("an expired marker stayed")
	}
	if err := Write(p, Marker{Until: now.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if err := Remove(p); err != nil || Held(p, now) {
		t.Errorf("Remove: %v", err)
	}
	// Anything that is not a marker holds nothing.
	if err := os.WriteFile(Path(p), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if Held(p, now) {
		t.Error("garbage holds writes")
	}
}
