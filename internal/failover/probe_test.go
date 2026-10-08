package failover

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPublicProbeAsksTheServiceAddressForTheAPIHost(t *testing.T) {
	var gotHost, gotPath string
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost, gotPath = r.Host, r.URL.Path
		w.WriteHeader(status)
	}))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	probe := probeAt("api.example.test", func(context.Context) string { return "127.0.0.1" }, port, "http")

	if err := probe(context.Background()); err != nil {
		t.Fatalf("a healthy leader: %v", err)
	}
	// The connection went to the service address; the request names the API host.
	if gotHost != "api.example.test" || gotPath != "/healthz" {
		t.Fatalf("asked %q %q", gotHost, gotPath)
	}
	status = http.StatusServiceUnavailable // the node says it is down
	if err := probe(context.Background()); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("a leader that says it is down: %v", err)
	}
	srv.Close()
	if err := probe(context.Background()); err == nil {
		t.Fatal("a leader nobody reaches answered")
	}
	empty := PublicProbe("api.example.test", func(context.Context) string { return "" }, false)
	if err := empty(context.Background()); err == nil || !strings.Contains(err.Error(), "no service address") {
		t.Fatalf("no address: %v", err)
	}
}
