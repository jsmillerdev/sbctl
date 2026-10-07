package app

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"
)

// TestEdgeKeepsServingWhileLifecycleOperationsDrain pins the stop order: after SIGTERM
// the proxy answers until the drain returns, then it stops, then the admin listener.
func TestEdgeKeepsServingWhileLifecycleOperationsDrain(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	edgeSrv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok") })}
	edgeRun := func(ctx context.Context) error {
		go edgeSrv.Serve(ln)
		<-ctx.Done()
		return edgeSrv.Shutdown(context.Background())
	}
	release := make(chan struct{})
	draining := make(chan struct{})
	var adminDown atomic.Bool
	drain := func(context.Context) error {
		close(draining)
		<-release
		return nil
	}
	shutdownAdmin := func(context.Context) error { adminDown.Store(true); return nil }

	ctx, stop := context.WithCancel(context.Background())
	g, gctx := errgroup.WithContext(ctx)
	superviseStop(g, gctx, time.Minute, edgeRun, drain, shutdownAdmin, slog.New(slog.NewTextHandler(io.Discard, nil)))

	get := func() error {
		resp, err := http.Get("http://" + ln.Addr().String())
		if err != nil {
			return err
		}
		resp.Body.Close()
		return nil
	}
	// The edge is up before the stop signal (retry while its goroutine starts).
	deadline := time.Now().Add(5 * time.Second)
	for get() != nil {
		if time.Now().After(deadline) {
			t.Fatal("edge never came up")
		}
		time.Sleep(10 * time.Millisecond)
	}

	stop() // SIGTERM
	select {
	case <-draining:
	case <-time.After(5 * time.Second):
		t.Fatal("drain did not start after the stop signal")
	}
	// A detached operation is still running: the edge must still answer.
	for range 5 {
		if err := get(); err != nil {
			t.Fatalf("edge stopped answering while the drain was running: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if adminDown.Load() {
		t.Fatal("admin listener shut down before the drain finished")
	}

	close(release)
	done := make(chan error, 1)
	go func() { done <- g.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor did not finish after the drain returned")
	}
	if !adminDown.Load() {
		t.Fatal("admin listener not shut down")
	}
	if get() == nil {
		t.Fatal("edge still answering after the drain")
	}
}
