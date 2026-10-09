package api

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/supavise/supavise/internal/oauth"
	"github.com/supavise/supavise/internal/proxy"
)

// mcpStudio stands in for Studio's /api/mcp: it records what it receives and answers like the
// stateless JSON transport. While hold is set, it keeps the request until hold is closed.
type mcpStudio struct {
	srv  *httptest.Server
	mu   sync.Mutex
	seen []mcpSeen
	hold chan struct{}
	here chan struct{} // one send for each request that arrived
}

type mcpSeen struct {
	method, path, query, body string
	header                    http.Header
}

func newMCPStudio(t *testing.T) *mcpStudio {
	t.Helper()
	s := &mcpStudio{here: make(chan struct{}, 64)}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.seen = append(s.seen, mcpSeen{r.Method, r.URL.Path, r.URL.RawQuery, string(b), r.Header.Clone()})
		hold := s.hold
		s.mu.Unlock()
		s.here <- struct{}{}
		if hold != nil {
			<-hold
		}
		// Next's answers carry CORS headers of their own; the edge must not pass them on.
		w.Header().Set("Access-Control-Allow-Origin", "https://studio.example.test")
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *mcpStudio) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.seen)
}

func (s *mcpStudio) last(t *testing.T) mcpSeen {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.seen) == 0 {
		t.Fatal("Studio received no request")
	}
	return s.seen[len(s.seen)-1]
}

// mcpEdge is the proxy in front of the Management API's gate and a stand-in Studio: the whole path of
// a request to api.<domain>/mcp.
type mcpEdge struct {
	*mcpFixture
	studio *mcpStudio
	ts     *httptest.Server
}

func newMCPEdge(t *testing.T) *mcpEdge {
	t.Helper()
	f := newMCPFixture(t)
	st := newMCPStudio(t)
	_, port, err := net.SplitHostPort(strings.TrimPrefix(st.srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	if f.cfg.Ports.Studio, err = strconv.Atoi(port); err != nil {
		t.Fatal(err)
	}
	edge, err := proxy.New(proxy.Options{
		Config: f.cfg, Registry: f.reg, Keys: proxy.RegistryKeys{Registry: f.reg, Secrets: f.srv.sec},
		APIHandler: f.srv, MCPGate: f.srv.MCPGate, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(edge.Handler())
	t.Cleanup(ts.Close)
	return &mcpEdge{mcpFixture: f, studio: st, ts: ts}
}

// call sends a request to the edge with the given Host.
func (e *mcpEdge) call(method, host, target string, body io.Reader, headers ...string) (*http.Response, string) {
	e.t.Helper()
	req, err := http.NewRequest(method, e.ts.URL+target, body)
	if err != nil {
		e.t.Fatal(err)
	}
	req.Host = host
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := e.ts.Client().Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, strings.TrimSpace(string(b))
}

// M1 on the whole path: Studio is never contacted without a token that is good right now.
func TestMCPThroughTheEdge(t *testing.T) {
	e := newMCPEdge(t)
	const api = "api.example.test"
	const rpc = `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`

	t.Run("refused", func(t *testing.T) {
		const bad = `{"message":"The access token is invalid, expired or revoked"}`
		const none = `{"message":"No access token provided"}`
		for _, tc := range []struct {
			name, target, want string
			headers            []string
		}{
			{"no token", "/mcp", none, nil},
			{"a token in the URL only", "/mcp?access_token=" + e.oauth, none, nil},
			{"dashboard JWT", "/mcp", bad, mcpAuthz(e.jwt)},
			{"unknown oauth token", "/mcp", bad, mcpAuthz("sbp_oauth_" + strings.Repeat("b2", 20))},
			{"unknown personal access token", "/mcp", bad, mcpAuthz("sbp_" + strings.Repeat("c3", 20))},
		} {
			resp, body := e.call("POST", api, tc.target, strings.NewReader(rpc),
				append(tc.headers, "Content-Type", "application/json")...)
			if resp.StatusCode != 401 || body != tc.want || resp.Header.Get("WWW-Authenticate") == "" {
				t.Errorf("%s: %d %q %v", tc.name, resp.StatusCode, body, resp.Header)
			}
		}
		if n := e.studio.count(); n != 0 {
			t.Fatalf("Studio received %d requests without a good token", n)
		}
	})

	t.Run("served", func(t *testing.T) {
		for name, tok := range map[string]string{"oauth": e.oauth, "personal access token": e.pat} {
			before := e.studio.count()
			resp, body := e.call("POST", api, "/mcp?access_token="+tok+"&foo=bar&read_only=true&project_ref=abcdefghijklmnopqrst&features=database,docs",
				strings.NewReader(rpc), "Authorization", "Bearer "+tok, "Cookie", "sb-access-token=session", "Content-Type", "application/json",
				"Origin", "https://claude.ai", "X-Forwarded-For", "6.6.6.6")
			if resp.StatusCode != 200 || body != `{"jsonrpc":"2.0","id":1,"result":{}}` || e.studio.count() != before+1 {
				t.Fatalf("%s: %d %q", name, resp.StatusCode, body)
			}
			got := e.studio.last(t)
			if got.method != "POST" || got.path != "/api/mcp" || got.body != rpc ||
				got.query != "features=database%2Cdocs&project_ref=abcdefghijklmnopqrst&read_only=true" {
				t.Errorf("%s: Studio saw %+v", name, got)
			}
			if got.header.Get("Authorization") != "Bearer "+tok || got.header.Get("Cookie") != "" || got.header.Get("X-Forwarded-For") == "6.6.6.6" ||
				got.header.Get("X-Forwarded-Host") != api {
				t.Errorf("%s: Studio's headers: %v", name, got.header)
			}
			// The client reads the answer with the gate's CORS policy and none of Studio's.
			if v := resp.Header.Values("Access-Control-Allow-Origin"); len(v) != 1 || v[0] != "*" || resp.Header.Get("Access-Control-Allow-Credentials") != "" {
				t.Errorf("%s: CORS headers %v", name, resp.Header)
			}
		}
	})

	t.Run("methods Studio's route does not serve still need a token", func(t *testing.T) {
		before := e.studio.count()
		for _, m := range []string{"GET", "DELETE"} {
			if resp, _ := e.call(m, api, "/mcp", nil); resp.StatusCode != 401 {
				t.Errorf("%s without a token: %d", m, resp.StatusCode)
			}
		}
		if resp, _ := e.call("PUT", api, "/mcp", nil, mcpAuthz(e.oauth)...); resp.StatusCode != 405 {
			t.Errorf("PUT: %d", resp.StatusCode)
		}
		if e.studio.count() != before {
			t.Error("Studio was contacted")
		}
	})

	t.Run("preflight", func(t *testing.T) {
		before := e.studio.count()
		resp, _ := e.call("OPTIONS", api, "/mcp", nil, "Origin", "https://claude.ai", "Access-Control-Request-Method", "POST",
			"Access-Control-Request-Headers", "authorization,content-type,mcp-protocol-version")
		if resp.StatusCode != 204 || resp.Header.Get("Access-Control-Allow-Origin") != "*" ||
			!strings.Contains(resp.Header.Get("Access-Control-Allow-Headers"), "mcp-protocol-version") || e.studio.count() != before {
			t.Errorf("%d %v", resp.StatusCode, resp.Header)
		}
	})

	t.Run("revoked", func(t *testing.T) {
		before := e.studio.count()
		if resp, _ := e.call("POST", api, "/mcp", strings.NewReader(rpc), mcpAuthz(e.oauth)...); resp.StatusCode != 200 {
			t.Fatalf("control: %d", resp.StatusCode)
		}
		if err := e.auth.RevokeGrant(context.Background(), 7, oauth.ReasonAdmin, "admin"); err != nil {
			t.Fatal(err)
		}
		resp, body := e.call("POST", api, "/mcp", strings.NewReader(rpc), mcpAuthz(e.oauth)...)
		if resp.StatusCode != 401 || !strings.Contains(resp.Header.Get("WWW-Authenticate"), `error="invalid_token"`) || !strings.Contains(body, "revoked") {
			t.Errorf("%d %q %v", resp.StatusCode, body, resp.Header)
		}
		if e.studio.count() != before+1 {
			t.Errorf("Studio received %d requests, want only the one before the revocation", e.studio.count()-before)
		}
	})

	t.Run("studio host has no MCP route", func(t *testing.T) {
		before := e.studio.count()
		for _, tok := range []string{e.pat, e.jwt} {
			if resp, _ := e.call("POST", "studio.example.test", "/api/mcp", strings.NewReader(rpc), mcpAuthz(tok)...); resp.StatusCode != 404 {
				t.Errorf("studio.<domain>/api/mcp: %d", resp.StatusCode)
			}
		}
		if e.studio.count() != before {
			t.Error("Studio was contacted")
		}
	})

	t.Run("body", func(t *testing.T) {
		before := e.studio.count()
		// Declared too long: refused up front.
		big := strings.NewReader(strings.Repeat("x", mcpMaxBody+1))
		if resp, body := e.call("POST", api, "/mcp", big, mcpAuthz(e.pat)...); resp.StatusCode != 413 || !strings.Contains(body, "too large") {
			t.Errorf("declared: %d %q", resp.StatusCode, body)
		}
		if e.studio.count() != before {
			t.Error("Studio was sent a body that was declared too long")
		}
		// Sent in chunks with no length: cut off, and the client is told why.
		pr, pw := io.Pipe()
		go func() {
			chunk := []byte(strings.Repeat("x", 1<<20))
			for i := 0; i <= mcpMaxBody>>20; i++ {
				if _, err := pw.Write(chunk); err != nil {
					break
				}
			}
			pw.Close()
		}()
		if resp, body := e.call("POST", api, "/mcp", pr, mcpAuthz(e.pat)...); resp.StatusCode != 413 {
			t.Errorf("chunked: %d %q", resp.StatusCode, body)
		}
		pr.Close()
	})
}

// M6 on the whole path: the slot of a request is held until Studio has answered, and given back after.
func TestMCPInFlightThroughTheEdge(t *testing.T) {
	e := newMCPEdge(t)
	hold := make(chan struct{})
	e.studio.mu.Lock()
	e.studio.hold = hold
	e.studio.mu.Unlock()

	var wg sync.WaitGroup
	codes := make(chan int, mcpMaxInFlight)
	for i := 0; i < mcpMaxInFlight; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, _ := e.call("POST", "api.example.test", "/mcp", strings.NewReader(`{}`), mcpAuthz(e.pat)...)
			codes <- resp.StatusCode
		}()
	}
	for i := 0; i < mcpMaxInFlight; i++ {
		<-e.studio.here
	}
	resp, body := e.call("POST", "api.example.test", "/mcp", strings.NewReader(`{}`), mcpAuthz(e.pat)...)
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") != "1" || !strings.Contains(body, "Too many requests") {
		t.Errorf("the 9th request: %d %q %v", resp.StatusCode, body, resp.Header)
	}
	if e.studio.count() != mcpMaxInFlight {
		t.Errorf("Studio received %d requests, want %d", e.studio.count(), mcpMaxInFlight)
	}
	// Another principal is not held up.
	e.studio.mu.Lock()
	e.studio.hold = nil
	e.studio.mu.Unlock()
	if resp, _ := e.call("POST", "api.example.test", "/mcp", strings.NewReader(`{}`), mcpAuthz(e.oauth)...); resp.StatusCode != 200 {
		t.Errorf("another principal: %d", resp.StatusCode)
	}

	close(hold)
	wg.Wait()
	close(codes)
	for c := range codes {
		if c != 200 {
			t.Errorf("a held request ended with %d", c)
		}
	}
	// The server cancels each request's context once it is served, and that gives the slots back.
	waitFor(t, "the slots to be given back", func() bool {
		resp, _ := e.call("POST", "api.example.test", "/mcp", strings.NewReader(`{}`), mcpAuthz(e.pat)...)
		return resp.StatusCode == 200
	})
	for i := 0; i < 3*mcpMaxInFlight; i++ {
		if resp, _ := e.call("POST", "api.example.test", "/mcp", strings.NewReader(`{}`), mcpAuthz(e.pat)...); resp.StatusCode != 200 {
			t.Fatalf("request %d one after the other: %d (a slot was not given back)", i+1, resp.StatusCode)
		}
	}
}
