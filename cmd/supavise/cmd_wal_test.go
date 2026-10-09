package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/backup"
)

// A standby that loses the daemon's relay and the daemon's forwarders together (the daemon restarts) must not end its
// startup process with a fetch that fails at once: the fetch waits for the relay to come back, and fails the way it
// always did if the daemon stays away.
func TestRelayFetchWaitsForARelayThatIsComingBack(t *testing.T) {
	old := walRelayWait
	defer func() { walRelayWait = old }()
	dir, err := os.MkdirTemp("/tmp", "walrelay")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "r.sock")
	dest := filepath.Join(dir, "out")
	const name = "000000010000000000000007"

	walRelayWait = 10 * time.Second
	served := make(chan struct{})
	go func() {
		time.Sleep(1500 * time.Millisecond) // the daemon is back
		ln, err := net.Listen("unix", sock)
		if err != nil {
			return
		}
		srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("name") != name {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write([]byte("wal bytes"))
		})}
		close(served)
		_ = srv.Serve(ln)
	}()
	if err := relayFetchPatiently(context.Background(), sock, "system", name, dest); err != nil {
		t.Fatalf("a relay that came back after 1.5 s: %v", err)
	}
	<-served
	if b, _ := os.ReadFile(dest); string(b) != "wal bytes" {
		t.Fatalf("fetched %q", b)
	}
	// A file the archive does not hold is the answer at once, not a reason to wait.
	began := time.Now()
	err = relayFetchPatiently(context.Background(), sock, "system", "000000010000000000000009", filepath.Join(dir, "none"))
	if !errors.Is(err, backup.ErrNoWAL) || time.Since(began) > 2*time.Second {
		t.Fatalf("a file that is not archived: %v after %s", err, time.Since(began))
	}
	// A daemon that stays away is reported once the wait is over.
	walRelayWait = 1500 * time.Millisecond
	began = time.Now()
	err = relayFetchPatiently(context.Background(), filepath.Join(dir, "nobody.sock"), "system", name, dest)
	if !errors.Is(err, backup.ErrRelayDown) || time.Since(began) < time.Second {
		t.Fatalf("no relay: %v after %s", err, time.Since(began))
	}
}
