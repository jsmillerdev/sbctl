package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/rand"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/OWNER/sbctl/internal/registry"
	"github.com/OWNER/sbctl/internal/secrets"
)

const b64url = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"

// reencodings returns spellings of a JWT's signature that decode to the same bytes: the last
// character of a 32-byte signature (43 characters) carries 4 significant bits, so the 3 other
// characters with the same top 4 bits are equivalent, and the "=" padded form is the standard
// encoding of the same bytes. Upstream verifiers decode the signature leniently.
func reencodings(t *testing.T, key string) map[string]string {
	t.Helper()
	i := strings.LastIndexByte(key, '.')
	sig := key[i+1:]
	want, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || len(sig) != 43 {
		t.Fatalf("signature %q: %v", sig, err)
	}
	last := strings.IndexByte(b64url, sig[len(sig)-1])
	out := map[string]string{}
	for low := 0; low < 4; low++ {
		c := b64url[last&^3|low]
		if c == sig[len(sig)-1] {
			continue
		}
		alt := key[:len(key)-1] + string(c)
		got, err := base64.RawURLEncoding.DecodeString(alt[i+1:])
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("variant %q is not the same signature: %v", c, err)
		}
		out["last character "+string(c)] = alt
	}
	if len(out) != 3 {
		t.Fatalf("got %d low-bit variants, want 3", len(out))
	}
	out["= padding"] = key + "="
	return out
}

// legacyVariants are the keys the disabled switch must turn away, by name: both legacy keys in
// their canonical spelling and every re-encoding of their signatures.
func legacyVariants(t *testing.T, k *secrets.ProjectKeys) map[string]string {
	t.Helper()
	all := map[string]string{"anon": k.AnonKey, "service_role": k.ServiceRoleKey}
	for n, key := range map[string]string{"anon": k.AnonKey, "service_role": k.ServiceRoleKey} {
		for v, alt := range reencodings(t, key) {
			all[n+" "+v] = alt
		}
	}
	return all
}

func TestLegacyKeyTokenMatchesSigningInput(t *testing.T) {
	k := testKeys(t, testRef)
	other := testKeys(t, "zyxwvutsrqponmlkjihg")
	for name, tok := range legacyVariants(t, k) {
		if !isLegacyKeyToken(k, tok) {
			t.Errorf("%s: not recognized", name)
		}
	}
	// A different payload (another project's key, a user session, a changed claim) or header is
	// another token, whatever its signature looks like.
	for name, tok := range map[string]string{
		"empty":                  "",
		"publishable":            k.PublishableKey,
		"another project's anon": other.AnonKey,
		"one segment":            "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9",
		"payload one char off":   k.AnonKey[:strings.LastIndexByte(k.AnonKey, '.')-1] + "x" + k.AnonKey[strings.LastIndexByte(k.AnonKey, '.'):],
	} {
		if isLegacyKeyToken(k, tok) {
			t.Errorf("%s: recognized as a legacy key", name)
		}
	}
	if isLegacyKeyToken(&secrets.ProjectKeys{}, k.AnonKey) || isLegacyKeyToken(nil, k.AnonKey) {
		t.Error("a project without legacy keys recognized one")
	}
}

// Every route that recognizes a legacy key must do it by its signing input, so that no
// spelling of the signature gets around the switch.
func TestLegacyDisabledRefusesReencodedKeys(t *testing.T) {
	k := testKeys(t, testRef)
	k.LegacyDisabled = true
	variants := legacyVariants(t, k)
	mustRoute := func(p string) *route {
		rt := matchRoute(p)
		if rt == nil {
			t.Fatalf("no route for %s", p)
		}
		return rt
	}
	rest, open, fn, s3 := mustRoute("/rest/v1/t"), mustRoute("/storage/v1/object/public/b/o"), mustRoute("/functions/v1/f"), mustRoute("/storage/v1/s3/b/o")
	if open.access != accessOpen || s3.keys != keyS3 || fn.keys != keyFunctions {
		t.Fatalf("unexpected routes: %+v %+v %+v", open, s3, fn)
	}
	for name, tok := range variants {
		// Authorization bearer, next to a valid opaque key, on a protected and an open route.
		for rname, rt := range map[string]*route{"rest": rest, "storage": open, "functions": fn} {
			h := hdr("Authorization", "Bearer "+tok, "apikey", k.PublishableKey)
			if res := authorize(rt, k, testRef, h, ""); res.status != http.StatusUnauthorized {
				t.Errorf("%s as bearer on %s: status %d", name, rname, res.status)
			}
		}
		// apikey header and query.
		for rname, rt := range map[string]*route{"rest": rest, "storage": open, "functions": fn} {
			if res := authorize(rt, k, testRef, hdr("apikey", tok), ""); res.status != http.StatusUnauthorized {
				t.Errorf("%s as apikey on %s: status %d", name, rname, res.status)
			}
		}
		if res := authorize(rest, k, testRef, hdr(), "apikey="+tok); res.status != http.StatusUnauthorized {
			t.Errorf("%s as apikey query on rest: status %d", name, res.status)
		}
		if res := authorize(open, k, testRef, hdr(), "apikey="+tok); res.status != http.StatusUnauthorized {
			t.Errorf("%s as apikey query on storage: status %d", name, res.status)
		}
		// S3: bearer, session token header and presigned query.
		sigv4 := "AWS4-HMAC-SHA256 Credential=x/20260101/local/s3/aws4_request, SignedHeaders=host, Signature=abc"
		for cname, c := range map[string]struct {
			h http.Header
			q string
		}{
			"bearer":                         {h: hdr("Authorization", "Bearer "+tok)},
			"session header":                 {h: hdr("Authorization", sigv4, "X-Amz-Security-Token", tok)},
			"session query":                  {h: hdr(), q: "X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Security-Token=" + tok},
			"session query, padding escaped": {h: hdr(), q: "X-Amz-Security-Token=" + strings.ReplaceAll(tok, "=", "%3D")},
		} {
			if res := authorize(s3, k, testRef, c.h, c.q); res.status != http.StatusForbidden || res.ctype != "application/xml" {
				t.Errorf("%s as S3 %s: status %d %q", name, cname, res.status, res.ctype)
			}
		}
	}
	// Enabled again, nothing is refused.
	k.LegacyDisabled = false
	for name, tok := range variants {
		if res := authorize(open, k, testRef, hdr("apikey", tok), ""); res.status != 0 {
			t.Errorf("%s with the legacy keys enabled: status %d", name, res.status)
		}
	}
}

func TestLegacyNeedlesAreSigningInputs(t *testing.T) {
	k := testKeys(t, testRef)
	k.LegacyDisabled = true
	needles := legacyNeedles(k)
	if len(needles) != 2 {
		t.Fatalf("needles: %d", len(needles))
	}
	for _, n := range needles {
		if n[len(n)-1] != '.' || bytes.Count(n, []byte(".")) != 2 {
			t.Errorf("needle %q is not <header>.<payload>.", n)
		}
	}
	join := func(tok string) []byte {
		return wsFrame(true, 1, []byte(`["1","1","realtime:x","phx_join",{"access_token":"`+tok+`"}]`), true, 0)
	}
	for name, tok := range legacyVariants(t, k) {
		for seed := int64(1); seed <= 4; seed++ {
			if found, _ := feedChunks(newWSInspector(needles), join(tok), rand.New(rand.NewSource(seed))); !found {
				t.Errorf("%s: not found in a socket frame (chunks seed %d)", name, seed)
			}
		}
		if found, _ := newWSInspector(needles).feed(join(tok)); !found {
			t.Errorf("%s: not found in a socket frame (one read)", name)
		}
	}
}

// ---- through the proxy ----

func TestRealtimeSocketRefusesReencodedKeys(t *testing.T) {
	h := disabledHarness(t)
	up := serveEchoRealtime(h)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for name, tok := range legacyVariants(t, h.k) {
		b, _ := json.Marshal([]any{"1", "1", "realtime:room", "phx_join", map[string]any{"access_token": tok}})
		c := h.dialRealtime(t, ctx, nil)
		before := len(up.received())
		if err := c.Write(ctx, websocket.MessageText, b); err != nil {
			t.Fatalf("%s: write: %v", name, err)
		}
		_, _, err := c.Read(ctx)
		if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
			t.Errorf("%s: read error %v, want a 1008 close", name, err)
		}
		c.CloseNow()
		<-up.done
		if got := up.received(); len(got) != before {
			t.Errorf("%s: Realtime received the frame", name)
		}
	}
}

func TestRealtimeLongPollRefusesReencodedKeys(t *testing.T) {
	h := disabledHarness(t)
	up := serveEchoRealtime(h)
	for name, tok := range legacyVariants(t, h.k) {
		resp, text := h.reqBody("POST", h.host(h.ref), "/realtime/v1/longpoll?apikey="+h.k.PublishableKey,
			`{"payload":{"access_token":"`+tok+`"}}`, "Content-Type", "application/json")
		if resp.StatusCode != 401 || text != msgInvalidKey {
			t.Errorf("%s: %d %q", name, resp.StatusCode, text)
		}
	}
	if got := up.received(); len(got) != 0 {
		t.Errorf("Realtime received %d bodies", len(got))
	}
}

// The body check does not depend on what the request claims to be: an Upgrade header without
// Connection, or Upgrade plus Connection on a POST (or a GET with a body), is not a WebSocket
// handshake, and never becomes a raw tunnel either.
func TestRealtimeBodyCheckedWhateverTheUpgradeHeaders(t *testing.T) {
	h := disabledHarness(t)
	up := serveEchoRealtime(h)
	target := "/realtime/v1/longpoll?apikey=" + h.k.PublishableKey
	bad := `{"payload":{"access_token":"` + h.k.ServiceRoleKey + `"}}`
	good := `{"payload":{"access_token":"` + h.k.PublishableKey + `"}}`
	reencoded := `{"payload":{"access_token":"` + reencodings(t, h.k.ServiceRoleKey)["= padding"] + `"}}`
	for name, hd := range map[string][]string{
		"Upgrade without Connection":      {"Upgrade", "websocket"},
		"Upgrade and Connection":          {"Upgrade", "websocket", "Connection", "Upgrade"},
		"Connection list":                 {"Upgrade", "websocket", "Connection", "keep-alive, upgrade"},
		"another protocol":                {"Upgrade", "h2c", "Connection", "Upgrade"},
		"Connection upgrade, no Upgrade":  {"Connection", "Upgrade"},
		"Upgrade websocket, odd casing":   {"Upgrade", "WebSocket", "Connection", "UPGRADE"},
		"Upgrade h2c, Connection missing": {"Upgrade", "h2c"},
	} {
		for _, method := range []string{"POST", "PUT", "GET"} {
			for body, want := range map[string]int{bad: 401, reencoded: 401, good: 200} {
				if method == "GET" && want == 200 {
					continue // a GET that is a real handshake is the WebSocket path, tested elsewhere
				}
				before := len(up.received())
				hd := append([]string{"Content-Type", "application/json"}, hd...)
				resp, text := h.reqBody(method, h.host(h.ref), target, body, hd...)
				if resp.StatusCode != want {
					t.Errorf("%s %s, body %.40q: %d %q, want %d", method, name, body, resp.StatusCode, text, want)
				}
				if want == 401 && len(up.received()) != before {
					t.Errorf("%s %s: Realtime saw a refused body", method, name)
				}
				if want == 200 {
					if got := h.ups[svcRealtime].last(t).Header.Get("Upgrade"); got != "" {
						t.Errorf("%s %s: Upgrade %q reached Realtime on a request that is not a handshake", method, name, got)
					}
				}
			}
		}
	}
}

func TestRealtimeBodyCap(t *testing.T) {
	h := disabledHarness(t)
	up := serveEchoRealtime(h)
	if maxGuardedBody != 256<<10 {
		t.Fatalf("cap is %d", maxGuardedBody)
	}
	target := "/realtime/v1/longpoll?apikey=" + h.k.PublishableKey
	if resp, _ := h.reqBody("POST", h.host(h.ref), target, strings.Repeat("p", maxGuardedBody)); resp.StatusCode != 200 {
		t.Errorf("a body at the cap: %d", resp.StatusCode)
	}
	if resp, _ := h.reqBody("POST", h.host(h.ref), target, strings.Repeat("p", maxGuardedBody+1)); resp.StatusCode != 413 {
		t.Errorf("a body over the cap: %d", resp.StatusCode)
	}
	// No declared length: the cap still holds.
	r, _ := http.NewRequest("POST", h.ts.URL+target, io.MultiReader(strings.NewReader(strings.Repeat("p", maxGuardedBody)), strings.NewReader("pp")))
	r.Host = h.host(h.ref)
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 413 {
		t.Errorf("a chunked body over the cap: %d", resp.StatusCode)
	}
	if got := len(up.received()); got != 1 {
		t.Errorf("Realtime received %d bodies, want 1", got)
	}
}

// ---- sockets opened before the switch ----

func (h *harness) flipLegacy(t *testing.T, disabled bool) {
	t.Helper()
	k := *h.k
	k.LegacyDisabled = disabled
	h.keys.set(h.ref, &k)
	// What the registry's project_secrets notification does.
	h.srv.table.apply(context.Background(), registry.Change{Table: "project_secrets", Op: "update", Key: h.ref})
}

func TestRealtimeSocketOpenedBeforeSwitchIsClosed(t *testing.T) {
	h := newHarness(t) // legacy keys enabled
	up := serveEchoRealtime(h)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	c := h.dialRealtime(t, ctx, &websocket.DialOptions{CompressionMode: websocket.CompressionContextTakeover})
	defer c.CloseNow()
	other := h.dialRealtime(t, ctx, nil)
	defer other.CloseNow()
	echoOK(t, ctx, c, `["1","1","realtime:x","phx_join",{"access_token":"`+h.k.PublishableKey+`"}]`)
	eventually(t, "both sockets tracked", func() bool {
		h.srv.sockets.mu.Lock()
		defer h.srv.sockets.mu.Unlock()
		return len(h.srv.sockets.m[h.ref]) == 2
	})

	h.flipLegacy(t, true)
	eventually(t, "tracked sockets closed", func() bool { return len(h.srv.sockets.refs(h.ref)) == 0 })

	// The socket is gone; a legacy access_token sent on it never reaches Realtime.
	before := len(up.received())
	_ = c.Write(ctx, websocket.MessageText, []byte(`["1","2","realtime:x","access_token",{"access_token":"`+h.k.ServiceRoleKey+`"}]`))
	if _, _, err := c.Read(ctx); err == nil {
		t.Fatal("the socket opened before the switch is still open")
	}
	if _, _, err := other.Read(ctx); err == nil {
		t.Fatal("the second socket is still open")
	}
	<-up.done
	<-up.done
	for _, m := range up.received()[before:] {
		if strings.Contains(m, h.k.ServiceRoleKey) {
			t.Fatalf("Realtime received a legacy key on a socket opened before the switch: %.80s", m)
		}
	}

	// Reconnecting goes through the guarded path.
	c2 := h.dialRealtime(t, ctx, nil)
	defer c2.CloseNow()
	echoOK(t, ctx, c2, `["1","3","phoenix","heartbeat",{}]`)
	_ = c2.Write(ctx, websocket.MessageText, []byte(`["1","4","realtime:x","access_token",{"access_token":"`+h.k.AnonKey+`"}]`))
	if _, _, err := c2.Read(ctx); websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Errorf("legacy key on the reconnected socket: %v", err)
	}
}

// A socket that is open while the keys change in another way (a rotation with the legacy keys
// still on, or a switch to disabled and back) is closed only when they are disabled.
func TestRealtimeSocketStaysOpenWhileLegacyKeysAreEnabled(t *testing.T) {
	h := newHarness(t)
	serveEchoRealtime(h)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := h.dialRealtime(t, ctx, nil)
	defer c.CloseNow()
	echoOK(t, ctx, c, "one")
	calls := h.keys.callCount(h.ref)
	h.flipLegacy(t, false)
	eventually(t, "keys looked up again", func() bool { return h.keys.callCount(h.ref) > calls })
	time.Sleep(50 * time.Millisecond)
	echoOK(t, ctx, c, "two")
}

// A socket whose handshake raced the switch (keys read before it, hijack after it) is caught
// by the check that follows its registration.
func TestRealtimeSocketRegisteredAfterSwitchIsClosed(t *testing.T) {
	h := newHarness(t)
	serveEchoRealtime(h)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// The proxy's cached keys are still the enabled ones when the switch lands, as when a
	// notification arrives between the authorization and the hijack of one request.
	k := *h.k
	k.LegacyDisabled = true
	h.keys.set(h.ref, &k)
	h.srv.table.mu.Lock()
	h.srv.table.keyCache[h.ref] = keyEntry{keys: h.k, expires: time.Now().Add(time.Hour)}
	h.srv.table.mu.Unlock()
	c := h.dialRealtime(t, ctx, nil)
	defer c.CloseNow()
	h.srv.table.invalidateKeys(h.ref) // the late notification
	eventually(t, "socket closed", func() bool { return len(h.srv.sockets.refs(h.ref)) == 0 })
	if _, _, err := c.Read(ctx); err == nil {
		t.Fatal("socket still open")
	}
}

// ---- frame boundaries ----

func TestWSFrameTracker(t *testing.T) {
	frame := func(n int) []byte {
		p := bytes.Repeat([]byte("x"), n)
		return wsFrame(true, 1, p, false, 0)
	}
	stream := bytes.Join([][]byte{frame(0), frame(5), frame(125), frame(126), frame(70000), frame(3)}, nil)
	ends := map[int]bool{0: true}
	off := 0
	for _, f := range [][]byte{frame(0), frame(5), frame(125), frame(126), frame(70000), frame(3)} {
		off += len(f)
		ends[off] = true
	}
	var tr wsFrameTracker
	for i := 0; i < len(stream); i++ {
		if got := tr.boundary(); got != ends[i] {
			t.Fatalf("byte %d: boundary=%v, want %v", i, got, ends[i])
		}
		tr.feed(stream[i : i+1])
	}
	if !tr.boundary() {
		t.Fatal("not at a boundary after the last frame")
	}
	var tr2 wsFrameTracker
	for rest := stream; len(rest) > 0; {
		n := 1 + rand.Intn(300)
		if n > len(rest) {
			n = len(rest)
		}
		tr2.feed(rest[:n])
		rest = rest[n:]
	}
	if !tr2.boundary() {
		t.Fatal("chunked: not at a boundary after the last frame")
	}
}

// The close frame is injected only between frames the client is reading; in the middle of one
// the connection is just torn down.
func TestWSGuardConnCloseFrameOnlyOnBoundary(t *testing.T) {
	for name, c := range map[string]struct {
		written   []byte
		wantFrame bool
	}{
		"between frames":     {written: wsFrame(true, 1, []byte("hello"), false, 0), wantFrame: true},
		"nothing written":    {wantFrame: true},
		"inside a payload":   {written: wsFrame(true, 1, []byte("hello"), false, 0)[:4]},
		"inside a header":    {written: wsFrame(true, 1, bytes.Repeat([]byte("x"), 300), false, 0)[:3]},
		"after a long frame": {written: wsFrame(true, 2, bytes.Repeat([]byte("x"), 70000), false, 0), wantFrame: true},
	} {
		client, proxySide := net.Pipe()
		var got bytes.Buffer
		var wg sync.WaitGroup
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = io.Copy(&got, client) }()
		gc := &wsGuardConn{Conn: proxySide, r: proxySide}
		if len(c.written) > 0 {
			if _, err := gc.Write(c.written); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		}
		gc.sever("because")
		wg.Wait()
		tail := got.Bytes()[len(c.written):]
		if c.wantFrame {
			if len(tail) < 4 || tail[0] != 0x88 || string(tail[4:]) != "because" {
				t.Errorf("%s: no close frame after %d bytes: %q", name, len(c.written), tail)
			}
		} else if len(tail) != 0 {
			t.Errorf("%s: %d bytes written after a partial frame", name, len(tail))
		}
		if _, err := gc.Write([]byte("more")); err == nil {
			t.Errorf("%s: write after the close succeeded", name)
		}
		client.Close()
	}
}
