package mesh

import (
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/config"
)

const ref = "abcdefghijklmnopqrst"

func TestHeaderRoundTripLeavesThePayload(t *testing.T) {
	var buf bytes.Buffer
	for _, h := range []Header{
		{T: StreamForward, Kind: KindPostgres, Ref: ref},
		{T: StreamForward, Kind: KindReplicaPostgREST, Ref: "system"},
		{T: StreamForward, Kind: KindStorage},
		{T: StreamRPC},
	} {
		buf.Reset()
		if err := WriteHeader(&buf, h); err != nil {
			t.Fatal(err)
		}
		buf.WriteString("payload after the header\n")
		got, err := ReadHeader(&buf)
		if err != nil || got != h {
			t.Fatalf("round trip of %+v: %+v, %v", h, got, err)
		}
		if rest, _ := io.ReadAll(&buf); string(rest) != "payload after the header\n" {
			t.Fatalf("the header reader took more than the header: %q", rest)
		}
	}
	if line := `{"t":"fwd","kind":"postgres","ref":"` + ref + `"}` + "\n"; !strings.HasSuffix(line, "\n") {
		t.Fatal("the documented header is one line")
	}
}

func TestHeaderValidation(t *testing.T) {
	for name, h := range map[string]Header{
		"no type":               {},
		"unknown type":          {T: "udp"},
		"rpc with a kind":       {T: StreamRPC, Kind: KindPostgres},
		"rpc with a ref":        {T: StreamRPC, Ref: ref},
		"forward, no kind":      {T: StreamForward, Ref: ref},
		"a port is not a kind":  {T: StreamForward, Kind: "20003", Ref: ref},
		"unknown service":       {T: StreamForward, Kind: "svc:shell"},
		"project kind, no ref":  {T: StreamForward, Kind: KindPostgres},
		"project kind, bad ref": {T: StreamForward, Kind: KindPostgres, Ref: "../etc"},
		"service kind with ref": {T: StreamForward, Kind: KindStudio, Ref: ref},
		"uppercase ref":         {T: StreamForward, Kind: KindGoTrue, Ref: strings.ToUpper(ref)},
		"short ref":             {T: StreamForward, Kind: KindPostgREST, Ref: "abc"},
	} {
		if err := h.Validate(); err == nil {
			t.Errorf("%s: %+v was accepted", name, h)
		}
	}
	for _, line := range []string{
		"not json\n",
		`{"t":"fwd","kind":"postgres","ref":"` + ref + `","port":22}` + "\n", // unknown field
		`{"t":"rpc"` + "\n",
		strings.Repeat("x", 2000) + "\n",
	} {
		if _, err := ReadHeader(strings.NewReader(line)); err == nil {
			t.Errorf("header line %.40q was accepted", line)
		}
	}
	if _, err := ReadHeader(strings.NewReader(`{"t":"rpc"}`)); err == nil {
		t.Error("a header with no newline was accepted")
	}
}

func TestLocalPort(t *testing.T) {
	cfg := config.Default()
	for _, tc := range []struct {
		kind Kind
		ref  string
		seq  int
		want int
	}{
		{KindPostgres, ref, 2, 20006}, {KindGoTrue, ref, 2, 20007}, {KindPostgREST, ref, 2, 20008},
		{KindReplicaPostgres, ref, 2, 10006}, {KindReplicaPostgREST, ref, 2, 10008},
		{KindPostgres, "system", 0, 5433}, {KindGoTrue, "system", 0, 9999}, {KindReplicaPostgres, "system", 0, 10000},
		{KindAdmin, "", 0, 7000}, {KindStudio, "", 0, 3000}, {KindPGMeta, "", 0, 8080}, {KindRealtime, "", 0, 4000},
		{KindStorage, "", 0, 5000}, {KindImgproxy, "", 0, 5002}, {KindEdgeRuntime, "", 0, 9000},
	} {
		got, err := LocalPort(cfg, tc.kind, tc.ref, tc.seq)
		if err != nil || got != tc.want {
			t.Errorf("LocalPort(%s, %s, %d) = %d, %v, want %d", tc.kind, tc.ref, tc.seq, got, err, tc.want)
		}
	}
	for _, tc := range []struct {
		kind Kind
		ref  string
	}{{KindPostgREST, "system"}, {KindReplicaPostgREST, "system"}, {"nope", ref}} {
		if _, err := LocalPort(cfg, tc.kind, tc.ref, 0); err == nil {
			t.Errorf("LocalPort(%s, %s) found a port", tc.kind, tc.ref)
		}
	}
	if _, err := LocalPort(cfg, KindPostgREST, "system", 0); !errors.Is(err, ErrNoPort) {
		t.Errorf("system has no PostgREST: %v", err)
	}
	// Every valid kind has a port for an ordinary project, so the reconciler can bind them all.
	for _, k := range append(append([]Kind{}, ProjectKinds...), ServiceKinds...) {
		if !k.Valid() {
			t.Errorf("%s is listed but not valid", k)
		}
		if p, err := LocalPort(cfg, k, ref, 1); err != nil || p <= 0 {
			t.Errorf("%s: %d, %v", k, p, err)
		}
	}
}

func TestMuxRegistersAndRoutes(t *testing.T) {
	m := NewMux()
	m.Handle("GET /peer/v1/ping", func(w http.ResponseWriter, r *http.Request) {
		p, ok := PeerFrom(r.Context())
		if !ok || p.Node != "n2" {
			http.Error(w, "no peer", http.StatusForbidden)
			return
		}
		_, _ = io.WriteString(w, "pong "+p.Node)
	})
	m.Handle("POST /peer/v1/instances/{identifier}/{action}", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, r.PathValue("identifier")+" "+r.PathValue("action"))
	})
	req := httptest.NewRequest("GET", "/peer/v1/ping", nil)
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, req.WithContext(WithPeer(req.Context(), Peer{Node: "n2"})))
	if rec.Code != 200 || rec.Body.String() != "pong n2" {
		t.Fatalf("ping: %d %q", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	m.ServeHTTP(rec, httptest.NewRequest("GET", "/peer/v1/ping", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a request with no peer: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	m.ServeHTTP(rec, httptest.NewRequest("POST", "/peer/v1/instances/abc-rr-x-123456/promote", nil))
	if rec.Body.String() != "abc-rr-x-123456 promote" {
		t.Fatalf("wildcards: %q", rec.Body)
	}
	rec = httptest.NewRecorder()
	m.ServeHTTP(rec, httptest.NewRequest("GET", "/peer/v1/unknown", nil))
	if rec.Code != 404 {
		t.Fatalf("unknown path: %d", rec.Code)
	}
	if got := strings.Join(m.Patterns(), "|"); got != "GET /peer/v1/ping|POST /peer/v1/instances/{identifier}/{action}" {
		t.Fatalf("patterns: %s", got)
	}
	for name, fn := range map[string]func(){
		"duplicate": func() { m.Handle("GET /peer/v1/ping", func(http.ResponseWriter, *http.Request) {}) },
		"empty":     func() { m.Handle("", func(http.ResponseWriter, *http.Request) {}) },
		"nil":       func() { m.Handle("GET /x", nil) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s registration did not panic", name)
				}
			}()
			fn()
		}()
	}
}

// Both sides of a session open streams at the same time, each stream starts with a header, and
// bytes go both ways: the property the mesh relies on when only one side can dial.
func TestSessionEitherSideOpensStreams(t *testing.T) {
	a, b := net.Pipe()
	dialer, err := NewSession(a, true)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := NewSession(b, false)
	if err != nil {
		t.Fatal(err)
	}
	defer dialer.Close()
	defer listener.Close()

	// serve accepts streams on s and echoes each after reading its header.
	var wg sync.WaitGroup
	serve := func(s Session, name string) {
		defer wg.Done()
		st, err := s.AcceptStream()
		if err != nil {
			t.Errorf("%s accept: %v", name, err)
			return
		}
		defer st.Close()
		h, err := ReadHeader(st)
		if err != nil || h.Kind != KindPostgres || h.Ref != ref {
			t.Errorf("%s header: %+v, %v", name, h, err)
			return
		}
		buf := make([]byte, 5)
		if _, err := io.ReadFull(st, buf); err != nil {
			t.Errorf("%s read: %v", name, err)
			return
		}
		_, _ = st.Write(append([]byte(name+":"), buf...))
	}
	wg.Add(2)
	go serve(listener, "listener")
	go serve(dialer, "dialer")

	open := func(s Session, payload string) string {
		st, err := s.OpenStream()
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		if err := WriteHeader(st, Header{T: StreamForward, Kind: KindPostgres, Ref: ref}); err != nil {
			t.Fatal(err)
		}
		_, _ = st.Write([]byte(payload))
		_ = st.SetReadDeadline(time.Now().Add(5 * time.Second))
		out, err := io.ReadAll(io.LimitReader(st, int64(len("listener:")+len(payload))))
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}
	got := make(chan string, 2)
	go func() { got <- open(dialer, "hello") }()
	go func() { got <- open(listener, "world") }()
	seen := map[string]bool{<-got: true, <-got: true}
	if !seen["listener:hello"] || !seen["dialer:world"] {
		t.Fatalf("answers: %v", seen)
	}
	wg.Wait()
	if err := dialer.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-dialer.CloseChan():
	case <-time.After(2 * time.Second):
		t.Fatal("CloseChan did not close")
	}
	if !dialer.IsClosed() {
		t.Fatal("IsClosed after Close")
	}
	if _, err := dialer.OpenStream(); err == nil {
		t.Fatal("a closed session opened a stream")
	}
}
