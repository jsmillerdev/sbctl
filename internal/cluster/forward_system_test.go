package cluster

import (
	"context"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/mesh"
)

type noDialer struct{}

func (noDialer) Dial(context.Context, string, mesh.Header) (net.Conn, error) {
	return nil, net.ErrClosed
}

// A port that the old system cluster still holds is waited for, so that a rejoin run again at once does not fail on
// how long that cluster takes to stop; a port that stays held is an error that names the port.
func TestForwardSystemWaitsForTheOldClusterToLetGoOfThePort(t *testing.T) {
	old := portWait
	defer func() { portWait = old }()
	port := freeWindow(t, 20000, 30000, 1)
	held, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	o := &JoinOptions{Log: quiet()}

	portWait = 3 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { time.Sleep(700 * time.Millisecond); held.Close() }()
	if err := o.forwardSystem(ctx, noDialer{}, port, "n2"); err != nil {
		t.Fatalf("the port was released after 0.7 s: %v", err)
	}
	cancel() // the forwarder lets go of the port

	held2, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(freeWindow(t, 30000, 40000, 1))))
	if err != nil {
		t.Fatal(err)
	}
	defer held2.Close()
	portWait = 600 * time.Millisecond
	err = o.forwardSystem(context.Background(), noDialer{}, held2.Addr().(*net.TCPAddr).Port, "n2")
	if err == nil || !strings.Contains(err.Error(), "cannot forward the system cluster's port") {
		t.Fatalf("a port that stays held: %v", err)
	}
}
