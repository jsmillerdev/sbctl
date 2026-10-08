package mesh

import (
	"bytes"
	"crypto/rand"
	"io"
	"net"
	"testing"
	"time"

	"github.com/xtaci/smux"
)

// The windows and timers are the ones spike S6 needs for replication through the mesh.
func TestMuxConfigKeepsTheSpikeWindows(t *testing.T) {
	c := muxConfig()
	if err := smux.VerifyConfig(c); err != nil {
		t.Fatal(err)
	}
	if c.Version != 2 {
		t.Errorf("smux version %d: protocol 1 has no per-stream window", c.Version)
	}
	if c.MaxStreamBuffer < 4<<20 || c.MaxReceiveBuffer < 16<<20 {
		t.Errorf("windows %d / %d are below the 4 MiB / 16 MiB that S6 needs", c.MaxStreamBuffer, c.MaxReceiveBuffer)
	}
	if c.KeepAliveTimeout >= 60*time.Second || c.KeepAliveDisabled {
		t.Errorf("keepalive timeout %s must be below wal_receiver_timeout (60 s) and enabled", c.KeepAliveTimeout)
	}
}

// A stream moves several times its window in one write and the transfer completes: the
// window updates flow while the writer is still writing.
func TestSessionMovesMoreThanTheWindow(t *testing.T) {
	a, b := net.Pipe()
	client, err := NewSession(a, true)
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewSession(b, false)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	defer server.Close()
	payload := make([]byte, 3*StreamWindow+12345)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	got := make(chan []byte, 1)
	go func() {
		st, err := server.AcceptStream()
		if err != nil {
			got <- nil
			return
		}
		defer st.Close()
		data, _ := io.ReadAll(st)
		got <- data
	}()
	st, err := client.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Write(payload); err != nil {
		t.Fatal(err)
	}
	st.Close()
	select {
	case data := <-got:
		if !bytes.Equal(data, payload) {
			t.Fatalf("received %d bytes, want %d", len(data), len(payload))
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the transfer stalled")
	}
}
