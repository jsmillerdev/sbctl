package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/supavise/supavise/internal/config"
)

// stubBinary writes a shell script that stands for the supavise binary: it records its arguments in
// the file it returns, and exits with status 1 when they contain failOn.
func stubBinary(t *testing.T, failOn string) (path, record string) {
	t.Helper()
	dir := t.TempDir()
	record = filepath.Join(dir, "calls")
	path = filepath.Join(dir, "supavise")
	script := "#!/bin/sh\necho \"$@\" >>" + record + "\n"
	if failOn != "" {
		script += "case \"$*\" in *'" + failOn + "'*) exit 1 ;; esac\n"
	}
	script += "exit 0\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path, record
}

// stubHost is a nodeHost on the stub binary whose daemon restart is counted instead of done.
func stubHost(bin string) (h *nodeHost, out *bytes.Buffer, restarts *int) {
	out, restarts = &bytes.Buffer{}, new(int)
	h = &nodeHost{cfg: config.Default(), out: out, errw: out, binPath: bin,
		restart: func(context.Context) error { *restarts++; return nil }}
	return h, out, restarts
}

func calls(t *testing.T, record string) string {
	t.Helper()
	b, _ := os.ReadFile(record)
	return strings.TrimSpace(string(b))
}

// A binary of a release that has a host layer is converged after the swap.
func TestActivateConvergesTheHost(t *testing.T) {
	bin, record := stubBinary(t, "")
	h, _, restarts := stubHost(bin)
	if err := h.activate(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if got := calls(t, record); got != "system converge" || *restarts != 1 {
		t.Errorf("calls %q, restarts %d", got, *restarts)
	}
}

// A failure of the host layer is the upgrade's failure, and the daemon is not restarted onto a host
// that was not brought forward.
func TestActivateFailsWhenConvergeFails(t *testing.T) {
	bin, record := stubBinary(t, "system converge")
	h, _, restarts := stubHost(bin)
	err := h.activate(context.Background(), true)
	if err == nil || !strings.Contains(err.Error(), "supavise system converge") {
		t.Fatalf("err = %v", err)
	}
	if *restarts != 0 {
		t.Error("the daemon was restarted after a failed converge")
	}
	if got := calls(t, record); got != "system converge" {
		t.Errorf("calls %q", got)
	}
}

// A binary without a host layer (a rollback to an older release) renders its units the way it
// always did, and a failure of that is a warning.
func TestActivateWithoutAHostLayerWarns(t *testing.T) {
	bin, record := stubBinary(t, "system install-units")
	h, out, restarts := stubHost(bin)
	if err := h.activate(context.Background(), false); err != nil {
		t.Fatalf("a failing install-units failed the activation: %v", err)
	}
	if got := calls(t, record); got != "system install-units" || *restarts != 1 {
		t.Errorf("calls %q, restarts %d", got, *restarts)
	}
	if !strings.Contains(out.String(), "warning: supavise system install-units failed") {
		t.Errorf("output %q", out.String())
	}
}

// Without a swap the installed binary converges the host and nothing restarts.
func TestConvergeWithoutASwap(t *testing.T) {
	bin, record := stubBinary(t, "")
	h, _, restarts := stubHost(bin)
	if err := h.Converge(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := calls(t, record); got != "system converge" || *restarts != 0 {
		t.Errorf("calls %q, restarts %d", got, *restarts)
	}
	bin, _ = stubBinary(t, "system converge")
	h, _, _ = stubHost(bin)
	if err := h.Converge(context.Background()); err == nil || !strings.Contains(err.Error(), "supavise system converge") {
		t.Errorf("err = %v", err)
	}
}
